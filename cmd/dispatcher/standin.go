package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A stand-in for the model's API (ADR 0068):
// it answers a message in the API's own shapes, streamed or whole, with the
// headers that carry the plan's standing (ADR 0064), so that a trial's
// proxy and meter can be tried end to end with no model and no credential.
// It knows nothing, and says so: every answer is the word "ok", unless the
// conversation's first message scripts it. A line there of the form
//
//	stand-in: use TOOL {"an": "input"}
//
// is a tool for it to ask for: the first such line in its first answer, the
// next once that tool's result is back, and "ok" when the lines are used
// up. It is how a check makes a real harness do what a run does (start a
// subagent, leave a command running behind it) with no model. With a line
//
//	stand-in: tell results
//
// its last answer is, in place of "ok", what each tool gave back, a line
// for each, so that a check reads what the harness let the tools do.
// And a line
//
//	stand-in: slow SECONDS
//
// makes a streamed answer take that long (at most 120), for a check of
// what happens to an answer under way.
//
//	dispatcher stand-in                     the service
//	dispatcher stand-in ask [--model M] [--stream] [--url URL] [--say TEXT]
//	                                        one message, as a harness would send it, through the
//	                                        proxy the environment names; prints the answer's status,
//	                                        text and usage
//
// From the environment: STAND_IN_LISTEN (:8080); STAND_IN_TOKEN_FILE, the
// credential it takes (the token alone; read at every request, so that a
// changed credential is seen at once); without it any credential passes.
//
// Beside the API's own paths it has two of its own, for whoever runs a
// check:
//
//	POST /stand-in/standing   {"status": "allowed|allowed_warning|rejected", "windows": {"5h": {"utilization": 0.4, "reset_in": 3600}, ...}}
//	GET  /stand-in/seen       what it has been asked: requests, by model, refused for the credential, the last caller's
type standIn struct {
	tokenFile string
	now       func() time.Time

	mu       sync.Mutex
	status   string
	windows  map[string]standWindow
	requests int
	byModel  map[string]int
	tokens   map[string]map[string]int // by model: input_tokens, output_tokens, as answered
	denied   int
	lastUA   string
}

type standWindow struct {
	Utilization float64 `json:"utilization"`
	ResetIn     int64   `json:"reset_in"` // seconds from now
	Status      string  `json:"status,omitempty"`
}

func newStandIn(tokenFile string) *standIn {
	return &standIn{tokenFile: tokenFile, now: time.Now, status: "allowed", byModel: map[string]int{}, tokens: map[string]map[string]int{},
		windows: map[string]standWindow{"5h": {Utilization: 0.1, ResetIn: 3 * 3600}, "7d": {Utilization: 0.2, ResetIn: 4 * 86400}}}
}

// headers are the plan's standing as the API sends it on every answer.
func (s *standIn) headers(h http.Header) {
	const p = "anthropic-ratelimit-unified-"
	now := s.now()
	h.Set(p+"status", s.status)
	claim, soonest := "", int64(0)
	for name, w := range s.windows {
		st := w.Status
		if st == "" {
			st = "allowed"
		}
		h.Set(p+name+"-utilization", strconv.FormatFloat(w.Utilization, 'f', -1, 64))
		h.Set(p+name+"-reset", strconv.FormatInt(now.Unix()+w.ResetIn, 10))
		h.Set(p+name+"-status", st)
		if claim == "" || w.Utilization > s.windows[claim].Utilization {
			claim, soonest = name, now.Unix()+w.ResetIn
		}
	}
	if claim != "" {
		h.Set(p+"representative-claim", map[string]string{"5h": "five_hour", "7d": "seven_day"}[claim])
		h.Set(p+"reset", strconv.FormatInt(soonest, 10))
	}
}

func apiError(w http.ResponseWriter, code int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": msg}})
}

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusNoContent)
		return
	case r.URL.Path == "/stand-in/standing" && r.Method == http.MethodPost:
		var set struct {
			Status  string                 `json:"status"`
			Windows map[string]standWindow `json:"windows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.mu.Lock()
		if set.Status != "" {
			s.status = set.Status
		}
		for k, v := range set.Windows {
			s.windows[k] = v
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	case r.URL.Path == "/stand-in/seen":
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": s.requests, "by_model": s.byModel, "tokens": s.tokens, "refused_for_the_credential": s.denied, "last_user_agent": s.lastUA})
		return
	}
	// The API's own paths take the credential.
	if s.tokenFile != "" {
		want, err := os.ReadFile(s.tokenFile)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || got == "" || got != strings.TrimSpace(string(want)) {
			s.mu.Lock()
			s.denied++
			s.mu.Unlock()
			apiError(w, http.StatusUnauthorized, "authentication_error", "the stand-in does not know this credential")
			return
		}
	}
	if r.Method != http.MethodPost || strings.TrimSuffix(r.URL.Path, "/") != "/v1/messages" {
		// What a harness asks beside messages (HEAD /api/hello, its
		// settings): an empty answer does.
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodHead {
			io.WriteString(w, "{}")
		}
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "a message names its model")
		return
	}
	s.mu.Lock()
	s.requests++
	s.byModel[req.Model]++
	s.lastUA = r.Header.Get("User-Agent")
	s.headers(w.Header())
	rejected := s.status == "rejected"
	s.mu.Unlock()
	if rejected {
		// The plan's limit, as the API refuses for it: no retry asked for.
		w.Header().Set("x-should-retry", "false")
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "This request would exceed your account's rate limit. Please try again later.")
		return
	}
	// Counts that follow from the request, so that a check can know them:
	// a token for every four bytes asked, five answered. The fields are in
	// the API's own order, "type" first: what reads a stream as it passes
	// (the meter) tells an event by its first bytes.
	in := len(body) / 4
	s.mu.Lock()
	if s.tokens[req.Model] == nil {
		s.tokens[req.Model] = map[string]int{}
	}
	s.tokens[req.Model]["input_tokens"] += in
	s.tokens[req.Model]["output_tokens"] += 5
	s.mu.Unlock()
	id := "msg_standin_" + strconv.FormatInt(s.now().UnixNano(), 36)
	// The script, if the conversation has one: the tool to ask for now.
	var first string
	asked := 0
	var results []string
	for i, m := range req.Messages {
		text, uses := standInContent(m.Content)
		if i == 0 {
			first = text
		}
		if m.Role == "assistant" {
			asked += uses
		}
		for _, r := range standInResults(m.Content) {
			results = append(results, fmt.Sprintf("result %d: %s", len(results)+1, r))
		}
	}
	said := "ok"
	if strings.Contains(first, "\nstand-in: tell results") && len(results) > 0 {
		said = strings.Join(results, "\n")
	}
	quoted, _ := json.Marshal(said)
	block, blockStart, blockDelta, stop := `{"type":"text","text":`+string(quoted)+`}`, `{"type":"text","text":""}`, `{"type":"text_delta","text":`+string(quoted)+`}`, "end_turn"
	if script := standInScript(first); asked < len(script) {
		use := script[asked]
		toolID := "toolu_standin_" + strconv.FormatInt(s.now().UnixNano(), 36)
		block = fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, toolID, use.tool, use.input)
		blockStart = fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, toolID, use.tool)
		partial, _ := json.Marshal(string(use.input))
		blockDelta = fmt.Sprintf(`{"type":"input_json_delta","partial_json":%s}`, partial)
		stop = "tool_use"
	}
	const usage = `{"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":%d,"service_tier":"standard"}`
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"type":"message","role":"assistant","model":%q,"content":[%s],"stop_reason":%q,"stop_sequence":null,"usage":`+usage+"}\n", id, req.Model, block, stop, in, 5)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	event := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":`+usage+`}}`, id, req.Model, in, 1))
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+blockStart+`}`)
	event("ping", `{"type":"ping"}`)
	time.Sleep(50 * time.Millisecond) // a stream comes in pieces
	if m := standInSlow.FindStringSubmatch(first); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 && n <= 120 {
			for i := 0; i < n; i++ { // a ping a second, as a long answer has
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Second):
				}
				event("ping", `{"type":"ping"}`)
			}
		}
	}
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":`+blockDelta+`}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}`, stop, in))
	event("message_stop", `{"type":"message_stop"}`)
}

var standInSlow = regexp.MustCompile(`(?m)^stand-in: slow ([0-9]+)$`)

// standInUse is one line of a conversation's script.
type standInUse struct {
	tool  string
	input json.RawMessage
}

// standInScript is the script in a conversation's first message.
func standInScript(text string) []standInUse {
	var script []standInUse
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "stand-in: use ")
		if !ok {
			continue
		}
		tool, input, _ := strings.Cut(rest, " ")
		if input = strings.TrimSpace(input); !json.Valid([]byte(input)) || !strings.HasPrefix(input, "{") {
			input = "{}"
		}
		script = append(script, standInUse{tool: tool, input: json.RawMessage(input)})
	}
	return script
}

// standInContent is a message's text and how many tools it asks for; its
// content is a string or a list of blocks.
func standInContent(raw json.RawMessage) (text string, uses int) {
	if json.Unmarshal(raw, &text) == nil {
		return text, 0
	}
	var blocks []struct{ Type, Text string }
	_ = json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text + "\n"
		case "tool_use":
			uses++
		}
	}
	return text, uses
}

// standInResults are what the tools of a message gave back, each on one
// line of at most 300 characters, an error marked as one.
func standInResults(raw json.RawMessage) []string {
	var blocks []struct {
		Type    string          `json:"type"`
		IsError bool            `json:"is_error"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text, _ := standInContent(b.Content)
		text = strings.Join(strings.Fields(text), " ")
		if len(text) > 300 {
			text = text[:300]
		}
		if b.IsError {
			text = "ERROR " + text
		}
		out = append(out, text)
	}
	return out
}

func standInCmd(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "ask" {
		_, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		return standInAsk(ctx, flags)
	}
	listen := envOr("STAND_IN_LISTEN", ":8080")
	srv := &http.Server{Addr: listen, Handler: newStandIn(os.Getenv("STAND_IN_TOKEN_FILE")), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "a stand-in for the model's API listens on %s; it answers \"ok\" to everything\n", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// standInAsk sends one message as a harness would: to the model's API (or
// --url), through the proxy the environment names, with whatever stands in
// the environment for a credential. It prints what came back.
func standInAsk(ctx context.Context, flags map[string][]string) error {
	base := one(flags, "url", envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com"))
	model := one(flags, "model", "claude-haiku-5-5")
	_, stream := flags["stream"]
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16, "stream": stream,
		"messages": []map[string]string{{"role": "user", "content": one(flags, "say", "Reply with the single word ok.")}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+envOr("CLAUDE_CODE_OAUTH_TOKEN", "a-placeholder"))
	req.Header.Set("User-Agent", "stand-in-ask")
	resp, err := sourceClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text, usage := "", ""
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Delta struct{ Text string } `json:"delta"`
				Usage json.RawMessage       `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				text += ev.Delta.Text
				if len(ev.Usage) > 0 {
					usage = string(ev.Usage)
				}
			}
		}
	} else {
		var m struct {
			Content []struct{ Text string }  `json:"content"`
			Usage   json.RawMessage          `json:"usage"`
			Error   struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &m)
		for _, c := range m.Content {
			text += c.Text
		}
		usage = string(m.Usage)
		if m.Error.Message != "" {
			text = m.Error.Message
		}
	}
	fmt.Printf("status %d\ntext %s\nusage %s\nstanding %s, 5h at %s\n", resp.StatusCode, text, usage,
		resp.Header.Get("anthropic-ratelimit-unified-status"), resp.Header.Get("anthropic-ratelimit-unified-5h-utilization"))
	if resp.StatusCode/100 != 2 {
		os.Exit(3)
	}
	return nil
}
