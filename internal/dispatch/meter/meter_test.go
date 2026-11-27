package meter

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

const stream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard"}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"

const streamRest = "event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok \"usage\" message_delta"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":42,"server_tool_use":{"web_search_requests":2}}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

type seen struct {
	auth, apiKey, caller, encoding, path string
}

// model is a fake of the model's API: it streams in two parts, the second
// only when told, so that a test can see the first arrive on its own.
func model(t *testing.T, got chan<- seen, more <-chan struct{}) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Egress-Caller"), r.Header.Get("Accept-Encoding"), r.URL.Path}
		switch {
		case r.URL.Path == "/v1/messages" && r.URL.Query().Get("whole") == "1":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"m2","model":"claude-sonnet-5-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0}}`)
		case r.URL.Path == "/v1/messages" && r.URL.Query().Get("garbled") == "1":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":[1,2\n\ndata: {nonsense\n\n")
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, stream)
			w.(http.Flusher).Flush()
			<-more
			io.WriteString(w, streamRest)
		case r.URL.Path == "/api/claude_code/policy_limits":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func meterFor(t *testing.T, srv *httptest.Server, ledger *Ledger, cred ...func() (string, error)) *httptest.Server {
	up, _ := url.Parse(srv.URL)
	// As the meter's own: no compression asked for (cmd/dispatcher).
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	p := &Proxy{
		Upstream:   up,
		Transport:  tr,
		Credential: func() (string, error) { return "Bearer the-real-one", nil },
		Identify: func(_ context.Context, ip string) (message.Caller, bool) {
			switch ip {
			case "10.0.0.1":
				return message.Caller{Agent: "dreamer", Pod: "dreamer-1", PodUID: "u1"}, true
			case "10.0.0.5":
				return message.Caller{Pod: "not-an-agent"}, true
			}
			return message.Caller{}, false
		},
		CallerHeader: "X-Egress-Caller",
		Ledger:       ledger,
	}
	if len(cred) > 0 {
		p.Credential = cred[0]
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front
}

func post(t *testing.T, front *httptest.Server, path, from, auth string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{"model":"claude-haiku-5-5","stream":true}`))
	req.Header.Set("X-Egress-Caller", from)
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-Api-Key", "whatever-the-agent-had")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Claude-Code-Session-Id", "s-1")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestR10MeterStreamed: an agent's pod gets the model's answer as it comes,
// each part as soon as it is sent and unchanged; the API sees the
// installation's credential and nothing the agent sent in its place; the
// ledger has the answer's tokens by kind, under the pod's agent, pod and
// the model the answer named.
func TestR10MeterStreamed(t *testing.T) {
	got, more := make(chan seen, 4), make(chan struct{})
	ledger := &Ledger{Idle: time.Minute, Longest: time.Hour}
	front := meterFor(t, model(t, got, more), ledger)

	resp := post(t, front, "/v1/messages?beta=true", "10.0.0.1", "Bearer sk-ant-oat01-placeholder")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := <-got
	if s.auth != "Bearer the-real-one" || s.apiKey != "" || s.caller != "" || s.encoding != "" {
		t.Errorf("the API saw %+v", s)
	}
	// The first part arrives while the API still holds the rest.
	first := make(chan string, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		var b strings.Builder
		for !strings.Contains(b.String(), "content_block_start") {
			l, err := br.ReadString('\n')
			b.WriteString(l)
			if err != nil {
				break
			}
		}
		first <- b.String()
	}()
	select {
	case f := <-first:
		if !strings.HasPrefix(stream, f) {
			t.Errorf("the first part changed: %q", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first part of the stream was held back")
	}
	close(more)
	rest, _ := io.ReadAll(br)
	if !strings.HasSuffix(string(rest), streamRest) {
		t.Errorf("the rest changed: %q", rest)
	}
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	recs := ledger.Due(true)
	if len(recs) != 1 {
		t.Fatalf("records: %+v", recs)
	}
	r := recs[0]
	if r.Agent != "dreamer" || r.Pod != "dreamer-1" || r.PodUID != "u1" || r.Model != "claude-haiku-5-5" || r.Requests != 1 || r.Errors != 0 || r.Unread != 0 {
		t.Errorf("record: %+v", r)
	}
	want := map[string]int64{"input_tokens": 12, "output_tokens": 42, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 5000,
		"cache_creation.ephemeral_5m_input_tokens": 100, "cache_creation.ephemeral_1h_input_tokens": 0}
	for k, n := range want {
		if r.Tokens[k] != n {
			t.Errorf("%s: %d, want %d (%v)", k, r.Tokens[k], n, r.Tokens)
		}
	}
	if len(r.Sessions) != 1 || r.Sessions[0] != "s-1" {
		t.Errorf("sessions: %v", r.Sessions)
	}
	if r.ServerTools["web_search_requests"] != 2 {
		t.Errorf("server tools: %v", r.ServerTools)
	}
}

// TestR10MeterWholeAndOther: an answer that is not streamed is read whole;
// a request whose answer says nothing of usage is counted, with no model,
// and a 304 is not an error.
// Two requests of one pod and model are one record.
func TestR10MeterWholeAndOther(t *testing.T) {
	got := make(chan seen, 8)
	ledger := &Ledger{Idle: time.Minute, Longest: time.Hour}
	front := meterFor(t, model(t, got, nil), ledger)
	for range 2 {
		resp := post(t, front, "/v1/messages?whole=1", "10.0.0.1", "")
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	for _, path := range []string{"/api/oauth/usage", "/api/claude_code/policy_limits"} {
		resp := post(t, front, path, "10.0.0.1", "")
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	time.Sleep(50 * time.Millisecond)
	recs := ledger.Due(true)
	byModel := map[string]Record{}
	for _, r := range recs {
		byModel[r.Model] = r
	}
	if w := byModel["claude-sonnet-5-5"]; w.Requests != 2 || w.Tokens["input_tokens"] != 14 || w.Tokens["output_tokens"] != 6 {
		t.Errorf("whole answers: %+v", w)
	}
	if o := byModel[""]; o.Requests != 2 || o.Errors != 0 || o.Agent != "dreamer" || len(o.Tokens) != 0 {
		t.Errorf("another request: %+v", o)
	}
}

// TestR10MeterRefuses: a request from an address that is no agent's pod,
// or from nowhere said, never reaches the API, and is counted as refused.
func TestR10MeterRefuses(t *testing.T) {
	got := make(chan seen, 4)
	ledger := &Ledger{Idle: time.Minute, Longest: time.Hour}
	front := meterFor(t, model(t, got, nil), ledger)
	for _, from := range []string{"10.9.9.9", "10.0.0.5", ""} {
		resp := post(t, front, "/v1/messages", from, "Bearer sk-ant-oat01-placeholder")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "permission_error") {
			t.Errorf("from %q: %d %s", from, resp.StatusCode, body)
		}
	}
	select {
	case s := <-got:
		t.Errorf("the API was reached: %+v", s)
	default:
	}
	var refused int64
	for _, r := range ledger.Due(true) {
		if r.Agent != "" {
			t.Errorf("a refusal under an agent: %+v", r)
		}
		refused += r.Refused
	}
	if refused != 3 {
		t.Errorf("refused: %d", refused)
	}
}

// TestR10MeterNeverFailsForItself: an answer the meter cannot read passes
// unchanged and is counted as unread; a record the dispatcher does not take
// is kept and sent at the next try.
func TestR10MeterNeverFailsForItself(t *testing.T) {
	got := make(chan seen, 4)
	ledger := &Ledger{Idle: time.Minute, Longest: time.Hour}
	front := meterFor(t, model(t, got, nil), ledger)
	resp := post(t, front, "/v1/messages?garbled=1", "10.0.0.1", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "nonsense") {
		t.Fatalf("garbled: %d %q", resp.StatusCode, body)
	}
	time.Sleep(50 * time.Millisecond)

	var sent []Record
	fail := true
	s := &Sender{Ledger: ledger, Every: time.Hour, Meter: "egress-1", Keep: 10, Emit: func(_ context.Context, r Record) error {
		if fail {
			return errors.New("the dispatcher is away")
		}
		sent = append(sent, r)
		return nil
	}}
	s.Flush(context.Background(), true)
	if len(sent) != 0 || len(s.pending) != 1 {
		t.Fatalf("while the dispatcher is away: sent %d, kept %d", len(sent), len(s.pending))
	}
	fail = false
	s.Flush(context.Background(), false)
	if len(sent) != 1 || sent[0].Unread != 1 || sent[0].Requests != 1 || sent[0].Meter != "egress-1" {
		t.Errorf("sent: %+v", sent)
	}
	if id := EventID(sent[0]); !strings.HasPrefix(id, "model.usage/egress-1/dreamer/dreamer-1/unknown/") {
		t.Errorf("event id: %s", id)
	}
}

// TestR10LedgerClosesIdleAndOld: a tally is sent when its pod has been
// quiet for a while, or has been open long enough, and not before.
func TestR10LedgerClosesIdleAndOld(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	l := &Ledger{Idle: 2 * time.Minute, Longest: 15 * time.Minute, Now: func() time.Time { return now }}
	a := message.Caller{Agent: "a", Pod: "pa"}
	b := message.Caller{Agent: "b", Pod: "pb"}
	l.add(a, "m", func(r *Record) { r.Requests++ })
	l.add(b, "m", func(r *Record) { r.Requests++ })
	for i := 0; i < 14; i++ {
		now = now.Add(time.Minute)
		l.add(b, "m", func(r *Record) { r.Requests++ })
		if got := l.Due(false); i == 1 && (len(got) != 1 || got[0].Agent != "a") {
			t.Fatalf("a, idle two minutes: %+v", got)
		} else if i != 1 && len(got) != 0 {
			t.Fatalf("minute %d: %+v", i, got)
		}
	}
	now = now.Add(time.Minute)
	if got := l.Due(false); len(got) != 1 || got[0].Agent != "b" || got[0].Requests != 15 {
		t.Errorf("b, open fifteen minutes: %+v", got)
	}
}
