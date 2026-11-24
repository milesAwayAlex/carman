package pgxdispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// TestR2Retention: with a short period, finished tasks and their events go,
// and so do old events that belong to no task; what belongs to an unfinished
// task stays however old, and a task that waited longer than the period is
// still woken.
func TestR2Retention(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	s := newFake()
	inst, err := OpenWith(ctx, d.url, s.config(4), "b1", Options{
		Retention:       time.Second,
		RetentionByType: map[string]time.Duration{"usage.reading": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	tr := dispatch.TransitionID
	submit(t, inst, "done", 0)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "done", "done#1"))
	finish(t, inst, "done#1", dispatch.OutcomeDone, nil)
	waitEvent(t, inst, tr(dispatch.TypeDone, "done", "done#1"))
	submit(t, inst, "waiter", 0)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "waiter", "waiter#1"))
	finish(t, inst, "waiter#1", dispatch.OutcomeWaiting,
		&dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Hour)})
	waitEvent(t, inst, tr(dispatch.TypeWaiting, "waiter", "waiter#1"))
	s.hold("held")
	submit(t, inst, "held", 0)
	emit(t, inst, dispatch.Event{ID: "old", Type: "reading", Source: "test"})
	emit(t, inst, dispatch.Event{ID: "kept", Type: "usage.reading", Source: "test"})

	eventually(t, "the finished task and the old event to go", func() bool {
		_, err := inst.Explain(ctx, "done")
		return errors.Is(err, dispatch.ErrNotFound) && !hasEvent(t, inst, "old") &&
			!hasEvent(t, inst, tr(dispatch.TypeSubmitted, "done", "")) && !hasEvent(t, inst, "finished-done#1")
	})
	time.Sleep(1500 * time.Millisecond) // another round of pruning
	emit(t, inst, dispatch.Event{ID: "fresh", Type: "reading", Source: "test"})
	for _, id := range []string{
		tr(dispatch.TypeSubmitted, "waiter", ""), tr(dispatch.TypeStarted, "waiter", "waiter#1"),
		"finished-waiter#1", tr(dispatch.TypeWaiting, "waiter", "waiter#1"),
		tr(dispatch.TypeSubmitted, "held", ""), "kept", "fresh",
	} {
		if !hasEvent(t, inst, id) {
			t.Errorf("%s was removed", id)
		}
	}
	if k := reason(t, inst, "held"); k != dispatch.ReasonAdmission {
		t.Errorf("held shows %q", k)
	}
	emit(t, inst, dispatch.Event{ID: "wake", Type: "x", Subject: "y", Source: "test"})
	waitEvent(t, inst, tr(dispatch.TypeStarted, "waiter", "waiter#2"))
}
