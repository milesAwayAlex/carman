package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
)

// queueLog is a log in memory, enough for the list and the queue.
type queueLog struct {
	recs  []dispatch.Record
	tasks map[string]dispatch.TaskStatus
	now   time.Time
}

func (l *queueLog) Claim(_ context.Context, e dispatch.Event) (bool, error) {
	for _, r := range l.recs {
		if r.ID == e.ID {
			return false, nil
		}
	}
	if e.Time.IsZero() {
		e.Time = l.now
	}
	l.recs = append(l.recs, dispatch.Record{Seq: int64(len(l.recs) + 1), Event: e})
	return true, nil
}

func (l *queueLog) EventsOfTypes(_ context.Context, after int64, types []string, limit int) ([]dispatch.Record, error) {
	var out []dispatch.Record
	for _, r := range l.recs {
		for _, t := range types {
			if r.Type == t && r.Seq > after && len(out) < limit {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (l *queueLog) Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error) {
	return l.EventsOfTypes(ctx, 0, []string{typ}, n)
}

func (l *queueLog) Explain(_ context.Context, id string) (dispatch.TaskStatus, error) {
	if t, ok := l.tasks[id]; ok {
		return t, nil
	}
	return dispatch.TaskStatus{}, dispatch.ErrNotFound
}

// TestR2TheQueueRunsItself: the owner's list is worked through by itself
// (ADR 0079). The next item that waits is sent to the
// agent, as many at once as the installation says; an item runs on its own
// model, the strongest only when hard; one whose model is refused is held
// and does not hold the ones behind it; an item ends done with the first
// line of its run's answer, or failed, saying how, and a failed one is not
// taken again; one begun by somebody's own hand is left alone.
func TestR2TheQueueRunsItself(t *testing.T) {
	ctx := context.Background()
	l := &queueLog{tasks: map[string]dispatch.TaskStatus{}, now: time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)}
	now := func() time.Time { return l.now }
	list := &budget.Service{Store: l, Now: now}
	owner := message.Caller{Owner: true}
	if _, err := list.Set(ctx, owner, budget.Priority{Name: "work", Share: 1, Default: true}); err != nil {
		t.Fatal(err)
	}
	hard := true
	for _, ch := range []budget.Change{{Priority: "work", Title: "one\nmore words"}, {Priority: "work", Title: "two", Hard: &hard}, {Priority: "work", Title: "three"}, {Priority: "work", Title: "four"}} {
		if _, err := list.Change(ctx, owner, ch); err != nil {
			t.Fatal(err)
		}
	}
	// The strongest model is refused until tonight; nothing else is.
	reset := l.now.Add(6 * time.Hour)
	readings := func(context.Context) ([]dispatch.Record, error) {
		return []dispatch.Record{{Seq: 1, Event: dispatch.Event{Type: TypeUsageObserved, Time: l.now, Data: json.RawMessage(fmt.Sprintf(
			`{"model":"strong","rate_limit_info":null,"refused":{"limit":"seven_day","resets_at":%d,"message":"limit"}}`, reset.Unix()))}}}, nil
	}
	type sent struct{ id, to, model, body string }
	var sends []sent
	statuses := map[string]message.Status{}
	q := &queueRunner{cfg: queueConfig{Agent: "builder", Running: 2, Model: "plain", Strongest: "strong"}, list: list, store: l, now: now,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		hold: queueHold(readings, func(context.Context) (meter.Logged, error) { return meter.Logged{}, nil }, paceConfig{target: 1, soon: 3 * time.Hour}, 0.85, now),
		send: func(_ context.Context, id, to, body, model string) error {
			sends = append(sends, sent{id, to, model, body})
			statuses[id] = message.Status{Message: message.Message{ID: id}}
			return nil
		},
		status: func(_ context.Context, id string) (message.Status, error) {
			st, ok := statuses[id]
			if !ok {
				return st, message.ErrUnknown
			}
			return st, nil
		}}
	list.Held = q.Held
	pass := func() {
		t.Helper()
		l.now = l.now.Add(10 * time.Second)
		if err := q.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		v, _ := list.View(ctx, owner)
		var out []string
		for _, it := range v.Streams[0].Items {
			out = append(out, it.ID+"="+it.State)
		}
		for _, it := range v.Streams[0].Recent {
			out = append(out, it.ID+"=done:"+it.Note)
		}
		return strings.Join(out, " ")
	}
	// Two at once: the first, and the third, since the second needs the
	// strongest model, which is refused.
	pass()
	if len(sends) != 2 || sends[0].to != "builder" || sends[0].model != "plain" || !strings.Contains(sends[0].body, "work-1") || !strings.Contains(sends[0].body, "more words") || !strings.Contains(sends[1].body, "work-3") {
		t.Fatalf("the first pass sent %+v", sends)
	}
	if got := state(); got != "work-1=started work-2=waiting work-3=started work-4=waiting" {
		t.Fatalf("after the first pass: %s", got)
	}
	v, _ := list.View(ctx, owner)
	if h := v.Streams[0].Items[1].Held; !strings.Contains(h, "strong is refused (seven_day) until 2026-11-28 12:00 UTC") || v.Streams[0].Items[3].Held != "" {
		t.Errorf("why the hard item is held: %q; the fourth: %q", h, v.Streams[0].Items[3].Held)
	}
	pass() // nothing ended, no room
	if len(sends) != 2 {
		t.Fatalf("a pass with no room sent %d", len(sends))
	}
	// The first run answers and ends; the third's fails with no answer.
	st := statuses[sends[0].id]
	st.Answers = []message.Message{{Body: "\nIt is done: the page is up.\nMore.", Sent: l.now}}
	statuses[sends[0].id] = st
	l.tasks[message.TaskPrefix+sends[0].id] = dispatch.TaskStatus{State: dispatch.Done}
	l.tasks[message.TaskPrefix+sends[1].id] = dispatch.TaskStatus{State: dispatch.Failed}
	pass()
	if got := state(); got != "work-2=waiting work-3=failed work-4=started work-1=done:It is done: the page is up." {
		t.Fatalf("after two ended: %s", got)
	}
	// The fourth stops at the plan's limit; a failed item is not taken
	// again, and the hard one still waits for its model.
	st = statuses[sends[2].id]
	st.Events = []dispatch.Record{{Event: dispatch.Event{Type: message.TypeLimited}}}
	statuses[sends[2].id] = st
	pass()
	pass()
	if got := state(); len(sends) != 3 || got != "work-2=waiting work-3=failed work-4=failed work-1=done:It is done: the page is up." {
		t.Fatalf("after a limit: %d sent, %s", len(sends), got)
	}
	// The model's reset passes: the hard item runs, on the strongest.
	l.now = reset.Add(time.Minute)
	pass()
	if len(sends) != 4 || sends[3].model != "strong" || !strings.Contains(sends[3].body, "work-2") {
		t.Fatalf("after the reset: %+v", sends[len(sends)-1])
	}
	// Put back by the owner, a failed item waits and is taken; one he
	// began by hand is not the queue's to end.
	if _, err := list.Change(ctx, owner, budget.Change{ID: "work-3", State: budget.Waiting}); err != nil {
		t.Fatal(err)
	}
	it, _ := list.Change(ctx, owner, budget.Change{Priority: "work", Title: "his own"})
	if _, err := list.Change(ctx, owner, budget.Change{ID: it.ID, State: budget.Started}); err != nil {
		t.Fatal(err)
	}
	pass()
	pass()
	if got := state(); len(sends) != 5 || !strings.Contains(got, "work-3=started") || !strings.Contains(got, it.ID+"=started") {
		t.Fatalf("put back, and one begun by hand: %d sent, %s", len(sends), got)
	}
}

// TestR7TheQueuePacesByTheWeek: the queue holds an item by the line to
// the weekly reset (ADR 0079): the seven-day window, and its model's own
// week where it has one. The five-hour window is a rate limit: ahead of
// a line of its own it holds nothing, and at the ceiling the plan keeps
// for the owner it holds, saying so.
func TestR7TheQueuePacesByTheWeek(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 28, 7, 10, 0, 0, time.UTC)
	now := func() time.Time { return at }
	none := func(context.Context) ([]dispatch.Record, error) { return nil, nil }
	// Half way through the week; the five-hour window resets in three hours.
	five, weekly := at.Add(3*time.Hour).Unix(), at.Add(84*time.Hour).Unix()
	hold := func(fiveUsed, weekUsed float64, own map[string]meter.Window, model string) (bool, string) {
		l := meter.Logged{Held: "b", Accounts: map[string]meter.LoggedAccount{"b": {Own: own,
			Stood: meter.Stood{At: at, FiveHour: meter.Window{Utilization: fiveUsed, ResetsAt: five}, SevenDay: meter.Window{Utilization: weekUsed, ResetsAt: weekly}}}}}
		return queueHold(none, func(context.Context) (meter.Logged, error) { return l, nil }, paceConfig{target: 0.95, soon: 3 * time.Hour}, 0.85, now)(ctx, model)
	}
	// As seen on the installation: five-hour at 47%, two hours into it, the week under its line.
	if held, why := hold(0.47, 0.16, nil, "plain"); held {
		t.Errorf("the five-hour window at 47%% holds the queue: %s", why)
	}
	if held, why := hold(0.84, 0.16, nil, "plain"); held {
		t.Errorf("the five-hour window under its ceiling holds the queue: %s", why)
	}
	if held, why := hold(0.86, 0.16, nil, "plain"); !held || !strings.Contains(why, "the five-hour window is at 86%, at the ceiling of 85% above which only the owner's own work runs; it resets at 10:10 UTC") {
		t.Errorf("the five-hour window at its ceiling: %v, %q", held, why)
	}
	// The week: its line is 47.5% half way.
	if held, why := hold(0.2, 0.47, nil, "plain"); held {
		t.Errorf("a week under its line holds the queue: %s", why)
	}
	if held, why := hold(0.2, 0.6, nil, "plain"); !held || !strings.Contains(why, "the seven-day window is at 60%, ahead of its line to the weekly reset (48% now, 95% at its reset at 2026-12-01 19:10 UTC)") {
		t.Errorf("a week ahead of its line: %v, %q", held, why)
	}
	// A model's own week holds the items that need that model, and no others.
	own := map[string]meter.Window{"strong": {Utilization: 0.7, ResetsAt: weekly}}
	if held, why := hold(0.2, 0.2, own, "strong"); !held || !strings.Contains(why, "strong's own week is at 70%, ahead of its line") {
		t.Errorf("a model's own week ahead of its line: %v, %q", held, why)
	}
	if held, why := hold(0.2, 0.2, own, "plain"); held {
		t.Errorf("another model's week holds this item: %s", why)
	}
}

// TestR7TheQueueJudgesTheAccountHeld: after the proxy changes accounts
// the queue judges by the standing of the account it holds now, as the
// log has it, and not by the latest reading, which may be the other's.
func TestR7TheQueueJudgesTheAccountHeld(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 28, 6, 55, 0, 0, time.UTC)
	now := func() time.Time { return at }
	// The latest reading in the log is account a's: its week nearly spent.
	readings := func(context.Context) ([]dispatch.Record, error) {
		return []dispatch.Record{{Seq: 9, Event: dispatch.Event{Type: TypeUsageObserved, Time: at.Add(-10 * time.Minute), Data: json.RawMessage(fmt.Sprintf(
			`{"model":"plain","account":"a","rate_limit_info":{"status":"allowed","unifiedWindows":{"seven_day":{"utilization":0.99,"resetsAt":%d}}}}`, at.Add(6*24*time.Hour).Unix()))}}}, nil
	}
	weekA := meter.Window{Utilization: 0.99, ResetsAt: at.Add(6 * 24 * time.Hour).Unix()}
	weekB := meter.Window{Utilization: 0.1, ResetsAt: at.Add(3 * 24 * time.Hour).Unix()}
	logged := meter.Logged{Held: "b", Accounts: map[string]meter.LoggedAccount{
		"a": {Stood: meter.Stood{At: at.Add(-10 * time.Minute), SevenDay: weekA}},
		"b": {Stood: meter.Stood{At: at.Add(-time.Hour), SevenDay: weekB}}}}
	hold := queueHold(readings, func(context.Context) (meter.Logged, error) { return logged, nil }, paceConfig{target: 0.95, soon: 3 * time.Hour}, 0.85, now)
	if held, why := hold(ctx, "plain"); held {
		t.Errorf("holding account b, the queue is held by account a's week: %s", why)
	}
	logged.Held = "a"
	if held, why := hold(ctx, "plain"); !held || !strings.Contains(why, "the seven-day window of account a is at 99%") {
		t.Errorf("holding account a: %v, %q", held, why)
	}
	// Nothing known of the account held: nothing is held for it.
	logged.Held = "c"
	if held, why := hold(ctx, "plain"); held {
		t.Errorf("an account the log knows nothing of holds the queue: %s", why)
	}
}
