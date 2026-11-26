// Package meter is what the egress proxy puts between an agent and the
// model's API (ADR 0050). For each request it says which agent's pod is
// asking, from the address the request came from, and refuses one that is
// no agent's; puts the installation's credential on it, so that no agent
// holds it; and adds up what the request used from the API's own answer,
// by agent, pod and model, as the answer passes. What it adds up reaches
// the dispatcher's log as model.usage events, a few per run, never one per
// request.
//
// Nothing here may slow an answer or fail a request for its own sake: the
// answer is passed on as it comes, each piece as soon as it arrives, and
// reading usage from it is a side look that gives up quietly.
package meter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"carman/internal/dispatch/message"
)

// TypeModelUsage is the event a meter emits: what one agent's pod used of
// one model over a stretch of time, as the API answered it.
const TypeModelUsage = "model.usage"

// Proxy is the meter in front of the model's API.
type Proxy struct {
	// Upstream is the model's API, for example https://api.anthropic.com.
	Upstream  *url.URL
	Transport http.RoundTripper
	// Credential is the value of the Authorization header to send, read
	// each time so that a renewed credential is used at once.
	Credential func() (string, error)
	// Identify says whose pod has an address now.
	Identify func(ctx context.Context, ip string) (message.Caller, bool)
	// CallerHeader is the header in which the proxy in front of the meter
	// says where the request came from. Whatever the agent sent in it is
	// replaced there, before the request reaches the meter.
	CallerHeader string
	Ledger       *Ledger
	Log          *slog.Logger
	// Shapes logs the names of what passes (headers, events, fields of
	// usage), never their values: how the harness and the API were looked
	// at when this was built.
	Shapes bool

	once sync.Once
	rp   *httputil.ReverseProxy
}

type ctxKey struct{}

type request struct {
	who        message.Caller
	credential string
	metered    bool
	started    time.Time
}

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// metered says whether a request's answer says what it used: a message,
// streamed or not. Counting tokens, batches and the rest do not.
func metered(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.TrimSuffix(r.URL.Path, "/") == "/v1/messages"
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.once.Do(p.build)
	ip := strings.TrimSpace(r.Header.Get(p.CallerHeader))
	who, ok := message.Caller{}, false
	if ip != "" {
		who, ok = p.Identify(r.Context(), ip)
	}
	if !ok || who.Agent == "" {
		p.Ledger.refuse(ip, who.Pod)
		p.log().Warn("refused: not an agent's pod", "from", ip, "pod", who.Pod, "path", r.URL.Path)
		refuse(w, http.StatusForbidden, "permission_error",
			"the egress proxy does not know the pod at "+orUnknown(ip)+" as an agent's, so it does not put the model's credential on this request")
		return
	}
	cred, err := p.Credential()
	if err != nil || cred == "" {
		p.log().Error("the model's credential cannot be read", "error", err)
		refuse(w, http.StatusBadGateway, "api_error", "the egress proxy has no credential for the model")
		return
	}
	if p.Shapes {
		p.log().Info("shape: request", "agent", who.Agent, "method", r.Method, "path", r.URL.Path,
			"query", names(r.URL.Query()), "headers", names(r.Header), "content_type", r.Header.Get("Content-Type"))
	}
	rq := &request{who: who, credential: cred, metered: metered(r), started: time.Now()}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rq)))
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown address"
	}
	return s
}

// refuse answers as the model's API answers an error, so that the harness
// shows it as it would show the API's own.
func refuse(w http.ResponseWriter, code int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": msg}})
}

func (p *Proxy) build() {
	p.rp = &httputil.ReverseProxy{
		Transport: p.Transport,
		// Each piece of an answer goes on as soon as it arrives.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			rq := pr.In.Context().Value(ctxKey{}).(*request)
			pr.SetURL(p.Upstream)
			pr.Out.Host = p.Upstream.Host
			h := pr.Out.Header
			for name := range h {
				if strings.HasPrefix(strings.ToLower(name), "x-envoy-") {
					h.Del(name)
				}
			}
			h.Del(p.CallerHeader)
			// Whatever the agent sent in place of a credential is not sent on.
			h.Del("X-Api-Key")
			h.Del("Proxy-Authorization")
			h.Set("Authorization", rq.credential)
			// An answer that is not compressed is one the meter can read as
			// it passes; it is compressed again by nobody, inside the cluster.
			h.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			rq := resp.Request.Context().Value(ctxKey{}).(*request)
			if p.Shapes {
				p.log().Info("shape: response", "agent", rq.who.Agent, "path", resp.Request.URL.Path, "status", resp.StatusCode,
					"content_type", resp.Header.Get("Content-Type"), "headers", names(resp.Header))
			}
			if !rq.metered || resp.StatusCode/100 != 2 {
				p.Ledger.add(rq.who, "", func(t *Record) {
					t.Requests++
					if resp.StatusCode/100 != 2 {
						t.Errors++
					}
				})
				return nil
			}
			sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			resp.Body = &reading{body: resp.Body, sse: sse, shapes: p.Shapes, log: p.log(), done: func(u answer) {
				p.Ledger.add(rq.who, u.model, func(t *Record) {
					t.Requests++
					if u.failed {
						t.Errors++
					}
					if !u.read {
						t.Unread++
					}
					t.addTokens(u.tokens, u.tools)
				})
			}}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rq := r.Context().Value(ctxKey{}).(*request)
			p.Ledger.add(rq.who, "", func(t *Record) { t.Requests++; t.Errors++ })
			if r.Context().Err() == nil {
				p.log().Warn("the model's API could not be reached", "agent", rq.who.Agent, "error", err)
			}
			refuse(w, http.StatusBadGateway, "api_error", "the egress proxy could not reach the model's API")
		},
	}
}

func names[T ~map[string][]string](m T) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// answer is what one answer said it used.
type answer struct {
	model  string
	tokens map[string]int64
	tools  map[string]int64
	read   bool // its usage was found
	failed bool // a stream that ended in an error event
}

// most is the most of an unstreamed answer that is kept to be read, and of
// one line of a stream.
const (
	mostBody = 32 << 20
	mostLine = 4 << 20
)

// reading passes an answer on unchanged and reads its usage on the side:
// in a stream, from the message_start and message_delta events (whose
// counts are totals so far, so the last one wins); in a whole answer, from
// its usage. Whatever goes wrong while reading only means the usage is
// not known.
type reading struct {
	body   io.ReadCloser
	sse    bool
	shapes bool
	log    *slog.Logger
	done   func(answer)

	buf     []byte
	skip    bool // the rest of a line too long to keep
	broken  bool
	ended   bool
	u       answer
	events  map[string]bool
	usageAt map[string]bool
}

func (r *reading) Read(b []byte) (int, error) {
	n, err := r.body.Read(b)
	if n > 0 && !r.broken {
		r.feed(b[:n])
	}
	if err == io.EOF {
		r.end()
	}
	return n, err
}

func (r *reading) Close() error {
	r.end()
	return r.body.Close()
}

func (r *reading) feed(b []byte) {
	defer func() {
		if recover() != nil {
			r.broken, r.buf = true, nil
		}
	}()
	if !r.sse {
		if len(r.buf)+len(b) > mostBody {
			r.broken, r.buf = true, nil
			return
		}
		r.buf = append(r.buf, b...)
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if !r.skip {
				r.buf = append(r.buf, b...)
				if len(r.buf) > mostLine {
					r.buf, r.skip = r.buf[:0], true
				}
			}
			return
		}
		if !r.skip {
			r.buf = append(r.buf, b[:i]...)
			r.line(r.buf)
		}
		r.buf, r.skip = r.buf[:0], false
		b = b[i+1:]
	}
}

// line is one line of a stream. Only the few events that carry usage are
// decoded; the many that carry text are passed over by their first bytes.
func (r *reading) line(l []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(l, "\r"), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	head := data
	if len(head) > 48 {
		head = head[:48]
	}
	if r.shapes {
		var t struct{ Type string }
		if json.Unmarshal(data, &t) == nil && t.Type != "" {
			if r.events == nil {
				r.events = map[string]bool{}
			}
			r.events[t.Type] = true
		}
	}
	switch {
	case bytes.Contains(head, []byte(`"message_start"`)):
		var ev struct {
			Message struct {
				Model string          `json:"model"`
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &ev) == nil {
			r.u.model = ev.Message.Model
			r.usage(ev.Message.Usage)
		}
	case bytes.Contains(head, []byte(`"message_delta"`)):
		var ev struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(data, &ev) == nil {
			r.usage(ev.Usage)
		}
	case bytes.Contains(head, []byte(`"type":"error"`)) || bytes.Contains(head, []byte(`"type": "error"`)):
		r.u.failed = true
	}
}

// usage takes the counts of a usage object: every number whose name ends
// in _tokens, at the top or one level down (the cache's breakdown), and
// the counts of the server's own tools.
func (r *reading) usage(raw json.RawMessage) {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return
	}
	if r.u.tokens == nil {
		r.u.tokens, r.u.tools = map[string]int64{}, map[string]int64{}
	}
	r.u.read = true
	for k, v := range m {
		if r.shapes {
			if r.usageAt == nil {
				r.usageAt = map[string]bool{}
			}
			r.usageAt[k] = true
		}
		switch x := v.(type) {
		case float64:
			if strings.HasSuffix(k, "_tokens") {
				r.u.tokens[k] = int64(x)
			}
		case map[string]any:
			for k2, v2 := range x {
				n, ok := v2.(float64)
				if !ok {
					continue
				}
				if k == "server_tool_use" {
					r.u.tools[k2] = int64(n)
				} else if strings.HasSuffix(k2, "_tokens") {
					r.u.tokens[k+"."+k2] = int64(n)
				}
			}
		}
	}
}

func (r *reading) end() {
	if r.ended {
		return
	}
	r.ended = true
	func() {
		defer func() {
			if recover() != nil {
				r.broken = true
			}
		}()
		if !r.sse && !r.broken {
			var whole struct {
				Model string          `json:"model"`
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(r.buf, &whole) == nil {
				r.u.model = whole.Model
				r.usage(whole.Usage)
			}
		}
	}()
	r.buf = nil
	if r.shapes {
		r.log.Info("shape: answer", "stream", r.sse, "model_found", r.u.model != "", "events", keys(r.events), "usage_fields", keys(r.usageAt))
	}
	if r.u.model == "" {
		r.u.model = "unknown"
	}
	r.done(r.u)
}

func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
