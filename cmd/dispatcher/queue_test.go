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
// (ADR new-the-queue-runs-itself). The next item that waits is sent to the
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
		hold: queueHold(readings, paceConfig{target: 1, soon: 3 * time.Hour}, 2*time.Hour, now),
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
	// Ahead of the line to the week's reset, nothing of the queue starts.
	week := l.now.Add(3 * 24 * time.Hour)
	q.hold = queueHold(func(context.Context) ([]dispatch.Record, error) {
		return []dispatch.Record{{Seq: 1, Event: dispatch.Event{Type: TypeUsageObserved, Time: l.now, Data: json.RawMessage(fmt.Sprintf(
			`{"model":"plain","rate_limit_info":{"status":"allowed","unifiedWindows":{"seven_day":{"utilization":0.9,"resetsAt":%d}}}}`, week.Unix()))}}}, nil
	}, paceConfig{target: 1, soon: 3 * time.Hour}, 2*time.Hour, now)
	if held, why := q.hold(ctx, "plain"); !held || !strings.Contains(why, "ahead of its line") {
		t.Errorf("ahead of the week's line: held %v, %q", held, why)
	}
}
