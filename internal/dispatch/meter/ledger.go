package meter

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"carman/internal/dispatch/message"
)

// Record is what one agent's pod used of one model over a stretch of time:
// the data of a model.usage event. Model is empty for requests whose answer
// says nothing of usage (counting tokens, the account's own questions) and
// for refusals.
type Record struct {
	Agent    string `json:"agent"`
	Pod      string `json:"pod,omitempty"`
	PodUID   string `json:"pod_uid,omitempty"`
	Model    string `json:"model,omitempty"`
	Requests int64  `json:"requests"`
	// Answers that were an error (4xx or 5xx), or a stream that ended in one.
	Errors int64 `json:"errors,omitempty"`
	// Requests refused because the address was no agent's pod; Agent is
	// then empty and Address is where they came from.
	Refused int64  `json:"refused,omitempty"`
	Address string `json:"address,omitempty"`
	// Calls the meter refused because the agent was over its budget at
	// the proxy (ADR 0064).
	OverBudget int64 `json:"over_budget,omitempty"`
	// Answers whose usage could not be read.
	Unread int64 `json:"unread,omitempty"`
	// Tokens by kind, as the API names them (input_tokens, output_tokens,
	// cache_creation_input_tokens, cache_read_input_tokens, and the cache's
	// breakdown as cache_creation.NAME), summed over the answers.
	Tokens      map[string]int64 `json:"tokens,omitempty"`
	ServerTools map[string]int64 `json:"server_tool_use,omitempty"`
	// The sessions the requests named (the harness's own word, not the
	// proxy's), at most a few.
	Sessions []string  `json:"sessions,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	// The proxy's pod that saw it.
	Meter string `json:"meter,omitempty"`
}

func (t *Record) addTokens(tokens, tools map[string]int64) {
	for k, n := range tokens {
		if t.Tokens == nil {
			t.Tokens = map[string]int64{}
		}
		t.Tokens[k] += n
	}
	for k, n := range tools {
		if t.ServerTools == nil {
			t.ServerTools = map[string]int64{}
		}
		t.ServerTools[k] += n
	}
}

func (t *Record) addSession(id string) {
	if id == "" || len(t.Sessions) >= 8 || slices.Contains(t.Sessions, id) {
		return
	}
	t.Sessions = append(t.Sessions, id)
}

// String is the record as one line of the meter's own log.
func (t Record) String() string {
	var parts []string
	for _, k := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		parts = append(parts, fmt.Sprintf("%s=%d", strings.TrimSuffix(k, "_tokens"), t.Tokens[k]))
	}
	who := t.Agent
	if who == "" {
		who = "(refused " + t.Address + ")"
	}
	return fmt.Sprintf("%s pod=%s model=%s requests=%d errors=%d refused=%d over_budget=%d unread=%d %s from=%s to=%s",
		who, t.Pod, t.Model, t.Requests, t.Errors, t.Refused, t.OverBudget, t.Unread, strings.Join(parts, " "),
		t.From.UTC().Format(time.RFC3339), t.To.UTC().Format(time.RFC3339))
}

type key struct{ agent, pod, uid, model, address string }

// Ledger adds up requests by agent, pod and model. A tally is closed, to be
// sent as one record, when its pod has asked nothing for Idle, or when it
// has been open for Longest: a run of a few minutes is one record per
// model, an hour's session a few.
type Ledger struct {
	Idle    time.Duration
	Longest time.Duration
	Now     func() time.Time

	mu   sync.Mutex
	open map[key]*Record
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Ledger) add(who message.Caller, model string, f func(*Record)) {
	l.tally(key{agent: who.Agent, pod: who.Pod, uid: who.PodUID, model: model}, f)
}

func (l *Ledger) refuse(address, pod string) {
	l.tally(key{pod: pod, address: address}, func(t *Record) { t.Requests++; t.Refused++ })
}

func (l *Ledger) tally(k key, f func(*Record)) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open == nil {
		l.open = map[key]*Record{}
	}
	t := l.open[k]
	if t == nil {
		t = &Record{Agent: k.agent, Pod: k.pod, PodUID: k.uid, Model: k.model, Address: k.address, From: now}
		l.open[k] = t
	}
	t.To = now
	f(t)
}

// Due closes the tallies that are idle or old enough, or all of them, and
// returns them oldest first.
func (l *Ledger) Due(all bool) []Record {
	now := l.now()
	l.mu.Lock()
	var out []Record
	for k, t := range l.open {
		if all || now.Sub(t.To) >= l.Idle || now.Sub(t.From) >= l.Longest {
			out = append(out, *t)
			delete(l.open, k)
		}
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out
}

// Sender sends the ledger's closed records, every so often and once more
// when it stops. A record that cannot be sent is kept and tried again, up
// to a limit; each is also a line of the meter's own log, so that what the
// proxy saw is on record even where the dispatcher is not.
type Sender struct {
	Ledger *Ledger
	Every  time.Duration
	// Emit sends one record; nil keeps the records in the log only.
	Emit  func(ctx context.Context, r Record) error
	Meter string
	Log   *slog.Logger
	// Keep is how many unsent records are kept for another try.
	Keep int

	pending []Record
}

func (s *Sender) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run sends until ctx ends, then sends what is left, with a few seconds to
// do it.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			s.Flush(last, true)
			cancel()
			return
		case <-t.C:
			s.Flush(ctx, false)
		}
	}
}

// Flush sends what is due (everything, with all).
func (s *Sender) Flush(ctx context.Context, all bool) {
	for _, r := range s.Ledger.Due(all) {
		r.Meter = s.Meter
		s.log().Info("model use: " + r.String())
		s.pending = append(s.pending, r)
	}
	if s.Emit == nil {
		s.pending = nil
		return
	}
	var kept []Record
	for i, r := range s.pending {
		if err := s.Emit(ctx, r); err != nil {
			s.log().Warn("model use not recorded in the dispatcher's log yet", "agent", r.Agent, "pod", r.Pod, "error", err)
			kept = append(kept, s.pending[i:]...)
			break
		}
	}
	if keep := s.Keep; keep > 0 && len(kept) > keep {
		s.log().Error("model use dropped: the dispatcher has not taken it for too long", "records", len(kept)-keep)
		kept = kept[len(kept)-keep:]
	}
	s.pending = kept
}

// EventID is the ID of a record's event: the same record sent twice is
// one event.
func EventID(r Record) string {
	who := r.Agent
	if who == "" {
		who = "refused/" + r.Address
	}
	model := r.Model
	if model == "" {
		model = "-"
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s", TypeModelUsage, r.Meter, who, r.Pod, model) + "/" + r.From.UTC().Format(time.RFC3339Nano)
}
