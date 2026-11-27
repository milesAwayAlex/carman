package meter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeUsageObserved is a reading of the account's standing against its
// plan's limits, the event the reading job writes (ADR 0043). The meter
// writes it too, from the headers of the answers that pass it.
const TypeUsageObserved = "usage.observed"

// Reading is a usage.observed event's data in the reading job's form, so
// that whatever reads the job's readings reads these unchanged.
type Reading struct {
	Model   string    `json:"model"`
	Info    LimitInfo `json:"rate_limit_info"`
	Refused *Refusal  `json:"refused,omitempty"`
	// The agent whose call it was read from: the proxy's word.
	Agent string `json:"agent,omitempty"`
	// Seen is when the answer passed.
	Seen time.Time `json:"seen"`
}

// LimitInfo is the harness's rate_limit_info, as the harness makes it
// from the same headers.
type LimitInfo struct {
	Status        string            `json:"status"`
	ResetsAt      int64             `json:"resetsAt,omitempty"`
	RateLimitType string            `json:"rateLimitType,omitempty"`
	Utilization   *float64          `json:"utilization,omitempty"`
	Windows       map[string]Window `json:"unifiedWindows,omitempty"`
}

type Window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
	Status      string  `json:"status,omitempty"`
}

// Refusal is a call refused for a limit.
type Refusal struct {
	Limit    string `json:"limit,omitempty"`
	ResetsAt int64  `json:"resets_at,omitempty"`
	Message  string `json:"message,omitempty"`
}

const unified = "anthropic-ratelimit-unified-"

// A window's part of a header's name: 5h, 7d, 7d_oi, 7d_opus. Other
// unified headers (grace-5h-utilization, overage-...) are not windows.
var windowPart = regexp.MustCompile(`^([0-9]+)([hd])(?:_([a-z_]+))?$`)

// windowNames are the harness's names for the headers' short ones.
var windowNames = map[string]string{"5h": "five_hour", "7d": "seven_day", "oi": "overage_included"}

func windowName(short string) (string, bool) {
	m := windowPart.FindStringSubmatch(short)
	if m == nil {
		return "", false
	}
	name, ok := windowNames[m[1]+m[2]]
	if !ok {
		name = m[1] + map[string]string{"h": "_hour", "d": "_day"}[m[2]]
	}
	if m[3] != "" {
		rest := m[3]
		if n, ok := windowNames[rest]; ok {
			rest = n
		}
		name += "_" + rest
	}
	return name, true
}

// ReadHeaders is the standing an answer's headers say, and whether they
// say any.
func ReadHeaders(h http.Header) (LimitInfo, bool) {
	var li LimitInfo
	for name, vs := range h {
		name = strings.ToLower(name)
		rest, ok := strings.CutPrefix(name, unified)
		if !ok || len(vs) == 0 {
			continue
		}
		v := strings.TrimSpace(vs[0])
		switch rest {
		case "status":
			li.Status = v
			continue
		case "reset":
			li.ResetsAt, _ = strconv.ParseInt(v, 10, 64)
			continue
		case "representative-claim":
			li.RateLimitType = v
			continue
		}
		i := strings.LastIndexByte(rest, '-')
		if i < 0 {
			continue
		}
		wn, ok := windowName(rest[:i])
		if !ok {
			continue
		}
		if li.Windows == nil {
			li.Windows = map[string]Window{}
		}
		w := li.Windows[wn]
		switch rest[i+1:] {
		case "utilization":
			w.Utilization, _ = strconv.ParseFloat(v, 64)
		case "reset":
			w.ResetsAt, _ = strconv.ParseInt(v, 10, 64)
		case "status":
			w.Status = v
		default:
			continue
		}
		li.Windows[wn] = w
	}
	if li.Status == "" && len(li.Windows) == 0 {
		return LimitInfo{}, false
	}
	if w, ok := li.Windows[li.RateLimitType]; ok {
		u := w.Utilization
		li.Utilization = &u
	}
	return li, true
}

// signature is what, changed, is worth a record at once: each status.
func (li LimitInfo) signature(refused bool) string {
	parts := []string{li.Status, li.RateLimitType, strconv.FormatBool(refused)}
	for n, w := range li.Windows {
		parts = append(parts, n+"="+w.Status)
	}
	sort.Strings(parts[3:])
	return strings.Join(parts, ",")
}

// Observer turns the headers of answers into readings: at most one a
// model each Every, and one at once when a status changes or a call is
// refused for a limit.
type Observer struct {
	Every time.Duration
	Now   func() time.Time

	mu      sync.Mutex
	last    map[string]observed
	pending []Reading
	wake    chan struct{}
}

type observed struct {
	at  time.Time
	sig string
}

func (o *Observer) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Wake is told when a reading should go at once.
func (o *Observer) Wake() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.wake == nil {
		o.wake = make(chan struct{}, 1)
	}
	return o.wake
}

// Observe takes one answer's headers. It says whether a reading was kept.
func (o *Observer) Observe(model, agent string, code int, h http.Header) bool {
	if o == nil || model == "" {
		return false
	}
	li, ok := ReadHeaders(h)
	if !ok {
		return false
	}
	refused := code == http.StatusTooManyRequests && li.Status == "rejected"
	now := o.now()
	sig := li.signature(refused)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil {
		o.last = map[string]observed{}
	}
	last, seen := o.last[model]
	changed := !seen || last.sig != sig
	if !changed && now.Sub(last.at) < o.Every {
		return false
	}
	o.last[model] = observed{at: now, sig: sig}
	rd := Reading{Model: model, Info: li, Agent: agent, Seen: now}
	if refused {
		rd.Refused = &Refusal{Limit: li.RateLimitType, ResetsAt: li.ResetsAt, Message: "refused by the model's API (429), as seen at the egress proxy"}
		if rd.Refused.Limit == "" {
			rd.Refused.Limit = "a usage limit"
		}
	}
	o.pending = append(o.pending, rd)
	if changed && o.wake != nil {
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
	return true
}

// Take returns the readings kept since the last Take.
func (o *Observer) Take() []Reading {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.pending
	o.pending = nil
	return out
}

// mostSniffed is how much of a request is looked at for its model.
const mostSniffed = 64 << 10

// sniffer passes a request's body on and keeps its first bytes, to find
// the model asked for.
type sniffer struct {
	body interface {
		Read([]byte) (int, error)
		Close() error
	}
	mu  sync.Mutex
	buf []byte
}

func (s *sniffer) Read(b []byte) (int, error) {
	n, err := s.body.Read(b)
	if n > 0 {
		s.mu.Lock()
		if room := mostSniffed - len(s.buf); room > 0 {
			s.buf = append(s.buf, b[:min(n, room)]...)
		}
		s.mu.Unlock()
	}
	return n, err
}

func (s *sniffer) Close() error { return s.body.Close() }

// model is the top-level "model" of the JSON seen so far, if it is in it.
func (s *sniffer) model() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	b := bytes.Clone(s.buf)
	s.mu.Unlock()
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return ""
		}
		if k == "model" {
			var m string
			if dec.Decode(&m) != nil {
				return ""
			}
			return m
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}
