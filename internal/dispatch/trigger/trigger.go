// Package trigger starts work when something happens (ADR 0063).
//
// A rule joins the dispatcher's two halves: when an event of this kind is
// in the log, submit a task of that class. Rules come from the
// installation's values and are applied mechanically; no model decides.
// Each event starts at most one task for a rule (the task's id is the
// rule's name and the event's id), a rule starts at most so many in a
// period, and an event that is about a run a rule started starts nothing,
// so that a rule cannot feed itself through its own run's end.
//
// Events from outside come two ways. A watch: a job asks a source on a
// schedule and tells the dispatcher a digest of what it saw; the dispatcher
// records watch.changed when the digest differs from the last. A hook: a
// source calls in, and the dispatcher records hook.received when the call
// is signed with the hook's secret. Neither puts the source's text in the
// log: a watch's event has a digest and a count, a hook's has the fields
// the installation names, each a token (see Token). A run is handed
// references, and reads the source itself, knowing it is the outside's.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
)

const (
	TypeFired = "rule.fired" // subject the rule; data a Fired
	TypeHeld  = "rule.held"  // subject the rule; data a Fired with why

	TypeWatchChanged = "watch.changed" // subject the watch; data a Changed
	TypeWatchFirst   = "watch.first"   // the first observation of a watch
	TypeHookReceived = "hook.received" // subject the hook; data the fields picked

	Source = "triggers"
	// MarkName is where the reader of rules keeps its place in the log.
	MarkName = "rules"
	// TaskPrefix starts the id of every task a rule starts.
	TaskPrefix = "rule/"
)

// Reserved are the prefixes of the event types only the dispatcher (and
// the owner) writes here: an agent that could write watch.changed could
// start whatever a rule starts on it.
var Reserved = []string{"rule.", "watch.", "hook."}

// Rule is one rule of the installation's.
type Rule struct {
	Name string `json:"name"`
	On   Match  `json:"when"`
	// Start is the class of the task: the CronJob whose job template runs.
	Start    string `json:"start"`
	Priority int    `json:"priority"`
	// Timeout limits an attempt (1h when empty); StartWithin, when set, is
	// how late the start may be before it is told as not started.
	Timeout     string `json:"timeout"`
	StartWithin string `json:"startWithin"`
	// AtMost limits how often the rule starts work: Count tasks in Per.
	// One in an hour when not said. It is also what bounds a loop that
	// goes through the outside, which nothing here can see.
	AtMost Rate `json:"atMost"`
	// Carry names the fields of the event's data handed to the run (dotted
	// paths), each as TASK_TRIGGER_NAME and only if it is a token.
	Carry []string `json:"carry"`

	timeout, startWithin, per time.Duration
}

// Match says which events a rule is for.
type Match struct {
	Type    string `json:"type"`    // exact; required
	Subject string `json:"subject"` // exact, when set
	// Where: fields of the event's data (dotted paths) and the value each
	// must have, compared as text.
	Where map[string]string `json:"where"`
}

// Rate is a count in a period.
type Rate struct {
	Count int    `json:"count"`
	Per   string `json:"per"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// ParseRules reads the installation's rules (a JSON list); empty is none.
func ParseRules(raw string) ([]Rule, error) {
	var rules []Rule
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf("the rules: %w", err)
	}
	seen := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		bad := func(format string, a ...any) error {
			return fmt.Errorf("rule %q: %s", r.Name, fmt.Sprintf(format, a...))
		}
		if !nameRe.MatchString(r.Name) || seen[r.Name] {
			return nil, bad("a rule's name is lowercase letters, digits and -, and its own")
		}
		seen[r.Name] = true
		if r.On.Type == "" || r.Start == "" {
			return nil, bad("needs when.type, the event's type, and start, the class to start")
		}
		if strings.HasPrefix(r.On.Type, "rule.") {
			return nil, bad("a rule is not for what rules record (%s)", r.On.Type)
		}
		var err error
		if r.timeout, err = duration(r.Timeout, time.Hour); err != nil || r.timeout <= 0 {
			return nil, bad("timeout is a duration, such as 1h")
		}
		if r.startWithin, err = duration(r.StartWithin, 0); err != nil {
			return nil, bad("startWithin is a duration, such as 1h")
		}
		if r.AtMost.Count == 0 && r.AtMost.Per == "" {
			r.AtMost = Rate{Count: 1, Per: "1h"}
		}
		if r.per, err = duration(r.AtMost.Per, time.Hour); err != nil || r.per <= 0 || r.AtMost.Count < 1 {
			return nil, bad("atMost is a count of at least 1 and a period, such as {count: 1, per: 24h}")
		}
		if len(r.Carry) > 16 {
			return nil, bad("carries at most 16 fields")
		}
		for _, f := range r.Carry {
			if envName(f) == "" {
				return nil, bad("carry: %q is not a field's name (letters, digits, _ and ., as in issue.number)", f)
			}
		}
	}
	return rules, nil
}

func duration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

var fieldRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)*$`)

// envName is the variable's name for a carried field: issue.number is
// TASK_TRIGGER_ISSUE_NUMBER. Empty when the field's name will not do.
func envName(field string) string {
	if !fieldRe.MatchString(field) || len(field) > 40 {
		return ""
	}
	return "TASK_TRIGGER_" + strings.ToUpper(strings.ReplaceAll(field, ".", "_"))
}

// tokenRe is what a value from an event may look like to be handed to a
// run, or kept from a hook's call: an identifier, a number, a time, a
// digest, a URL. No spaces, so no sentence: text from outside is where
// instructions hide, and a run that needs the text reads it at its source.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._:/@#%+=,?&~-]{1,200}$`)

// Token is v as a token, if it is one: a string that looks like one, a
// number or a boolean.
func Token(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		s = strconv.FormatBool(x)
	case json.Number:
		s = x.String()
	default:
		return "", false
	}
	return s, tokenRe.MatchString(s)
}

// field reads a dotted path in decoded JSON.
func field(data any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		m, ok := data.(map[string]any)
		if !ok {
			return nil, false
		}
		if data, ok = m[part]; !ok {
			return nil, false
		}
	}
	return data, true
}

// text is a scalar as text, for comparing.
func text(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// matches says whether a rule is for an event.
func (r Rule) matches(e dispatch.Record, data any) bool {
	if e.Type != r.On.Type || (r.On.Subject != "" && e.Subject != r.On.Subject) {
		return false
	}
	for path, want := range r.On.Where {
		v, ok := field(data, path)
		if !ok {
			return false
		}
		if got, ok := text(v); !ok || got != want {
			return false
		}
	}
	return true
}

// Fired is what a rule records of an event it was for.
type Fired struct {
	Rule  string `json:"rule"`
	Event string `json:"event"` // the event's id
	Seq   int64  `json:"seq"`
	Task  string `json:"task,omitempty"`
	// Carried are the fields handed to the run; Dropped those named by
	// the rule that the event did not have as tokens.
	Carried map[string]string `json:"carried,omitempty"`
	Dropped []string          `json:"dropped,omitempty"`
	Why     string            `json:"why,omitempty"` // for one held
}

// Log is what the reader of rules needs of the dispatcher.
type Log interface {
	EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error)
	Submit(ctx context.Context, t dispatch.TaskSpec) error
	Emit(ctx context.Context, e dispatch.Event) error
	Mark(ctx context.Context, name string, v any) (bool, error)
	SetMark(ctx context.Context, name string, v any) error
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
}

// Rules reads the log and starts what the rules say.
type Rules struct {
	Log   Log
	Rules []Rule
	// Check says whether a task can be started at all (its class names a
	// template); nil accepts every task.
	Check  func(context.Context, dispatch.TaskSpec) error
	Logger *slog.Logger
	Every  time.Duration
	Now    func() time.Time
}

type place struct {
	Seq int64 `json:"seq"`
	// Starts are, for each rule, when it last started work, newest last:
	// what the rate is counted from.
	Starts map[string][]time.Time `json:"starts,omitempty"`
}

func (r *Rules) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run reads until ctx ends. On its first start it begins at the end of the
// log: what happened before there were rules starts nothing.
func (r *Rules) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warn("reading the log for rules", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// about is the task an event is about, if it says: a task's own events
// have it as their subject, a Job's end and a run's usage name it.
func about(e dispatch.Record, data any) string {
	if strings.HasPrefix(e.Type, "task.") {
		return e.Subject
	}
	if v, ok := field(data, "task"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// TaskID is the id of the task a rule starts for an event.
func TaskID(rule, eventID string) string { return TaskPrefix + rule + "/" + eventID }

// Pass reads what the log has gained since the last pass.
func (r *Rules) Pass(ctx context.Context) error {
	var p place
	found, err := r.Log.Mark(ctx, MarkName, &p)
	if err != nil {
		return err
	}
	if !found {
		last, err := r.Log.Last(ctx, "", 1)
		if err != nil {
			return err
		}
		if len(last) > 0 {
			p.Seq = last[0].Seq
		}
		return r.Log.SetMark(ctx, MarkName, p)
	}
	if len(r.Rules) == 0 {
		return nil
	}
	types := map[string]bool{}
	for _, rule := range r.Rules {
		types[rule.On.Type] = true
	}
	var list []string
	for t := range types {
		list = append(list, t)
	}
	sort.Strings(list)
	if p.Starts == nil {
		p.Starts = map[string][]time.Time{}
	}
	for {
		recs, err := r.Log.EventsOfTypes(ctx, p.Seq, list, 200)
		if err != nil {
			return err
		}
		for _, e := range recs {
			var data any
			_ = json.Unmarshal(e.Data, &data)
			for _, rule := range r.Rules {
				if rule.matches(e, data) {
					if err := r.fire(ctx, rule, e, data, &p); err != nil {
						return err
					}
				}
			}
			p.Seq = e.Seq
			// The place is kept after each event: an event acted on
			// twice submits the same task and records the same event.
			if err := r.Log.SetMark(ctx, MarkName, p); err != nil {
				return err
			}
		}
		if len(recs) < 200 {
			return nil
		}
	}
}

// fire starts a rule's work for an event, or records why it does not.
func (r *Rules) fire(ctx context.Context, rule Rule, e dispatch.Record, data any, p *place) error {
	f := Fired{Rule: rule.Name, Event: e.ID, Seq: e.Seq}
	held := func(why string) error {
		f.Why = why
		b, _ := json.Marshal(f)
		return r.Log.Emit(ctx, dispatch.Event{ID: TypeHeld + "/" + rule.Name + "/" + e.ID, Type: TypeHeld, Source: Source, Subject: rule.Name, Data: b})
	}
	// A run a rule started ends, fails, costs: those events start nothing,
	// or a rule for "a run failed" would start a run for its own failure.
	if task := about(e, data); strings.HasPrefix(task, TaskPrefix) {
		return held("the event is about " + task + ", a run a rule started; a rule's run starts no further run")
	}
	now := r.now()
	var recent []time.Time
	for _, t := range p.Starts[rule.Name] {
		if now.Sub(t) < rule.per {
			recent = append(recent, t)
		}
	}
	p.Starts[rule.Name] = recent
	if len(recent) >= rule.AtMost.Count {
		return held(fmt.Sprintf("the rule starts at most %d in %s, and started %d, the last at %s; this event starts nothing, and is not kept for later",
			rule.AtMost.Count, rule.per, len(recent), recent[len(recent)-1].UTC().Format("15:04:05 UTC")))
	}
	env := map[string]string{
		"TASK_TRIGGER_RULE": rule.Name, "TASK_TRIGGER_EVENT": e.ID, "TASK_TRIGGER_TYPE": e.Type,
		"TASK_TRIGGER_TIME": e.Time.UTC().Format(time.RFC3339),
	}
	if s, ok := Token(e.Subject); ok {
		env["TASK_TRIGGER_SUBJECT"] = s
	}
	for _, name := range rule.Carry {
		v, ok := field(data, name)
		s, token := Token(v)
		if !ok || !token {
			f.Dropped = append(f.Dropped, name)
			continue
		}
		if f.Carried == nil {
			f.Carried = map[string]string{}
		}
		f.Carried[name] = s
		env[envName(name)] = s
	}
	// The run's own list of what it was handed, so that it need not guess
	// which variables there are.
	var names []string
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	env["TASK_TRIGGER_FIELDS"] = strings.Join(names, ",")
	payload, _ := json.Marshal(struct {
		Env map[string]string `json:"env"`
	}{env})
	spec := dispatch.TaskSpec{ID: TaskID(rule.Name, e.ID), Class: rule.Start, Priority: rule.Priority, Payload: payload, AttemptTimeout: rule.timeout}
	if rule.startWithin > 0 {
		spec.StartBy = now.Add(rule.startWithin)
	}
	if r.Check != nil {
		if err := r.Check(ctx, spec); err != nil {
			return held("the task cannot be started: " + err.Error())
		}
	}
	if err := r.Log.Submit(ctx, spec); err != nil {
		if errors.Is(err, dispatch.ErrInvalid) {
			return held("the task was refused: " + err.Error())
		}
		return err
	}
	p.Starts[rule.Name] = append(recent, now)
	f.Task = spec.ID
	b, _ := json.Marshal(f)
	return r.Log.Emit(ctx, dispatch.Event{ID: TypeFired + "/" + rule.Name + "/" + e.ID, Type: TypeFired, Source: Source, Subject: rule.Name, Data: b})
}
