package pgxdispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// TestR2RetentionUnowned: task history goes after Retention, and an event
// that belongs to no task (a record of a job that ran) stays for the
// longer RetentionUnowned.
func TestR2RetentionUnowned(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	s := newFake()
	inst, err := OpenWith(ctx, d.url, s.config(4), "b1", Options{Retention: time.Second, RetentionUnowned: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	tr := dispatch.TransitionID
	submit(t, inst, "done", 0)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "done", "done#1"))
	finish(t, inst, "done#1", dispatch.OutcomeDone, nil)
	emit(t, inst, dispatch.Event{ID: "ran", Type: "job.ended", Subject: "nightly", Source: "test"})
	eventually(t, "the finished task to go", func() bool {
		_, err := inst.Explain(ctx, "done")
		return errors.Is(err, dispatch.ErrNotFound)
	})
	time.Sleep(1500 * time.Millisecond)
	if !hasEvent(t, inst, "ran") {
		t.Error("the event that belongs to no task was removed with task history")
	}
}

// TestR7Reads: the service's reads list tasks by state with their reasons,
// and the log by type and from its end.
func TestR7Reads(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	s := newFake()
	inst, err := Open(ctx, d.url, s.config(1), "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	tr := dispatch.TransitionID
	submit(t, inst, "first", 1)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "first", "first#1"))
	submit(t, inst, "low", 0)
	submit(t, inst, "high", 5)
	eventually(t, "both to show the limit", func() bool {
		return reason(t, inst, "low") == dispatch.ReasonLimit && reason(t, inst, "high") == dispatch.ReasonLimit
	})
	pending, err := inst.Tasks(ctx, dispatch.Pending, 10)
	if err != nil || len(pending) != 2 || pending[0].Spec.ID != "high" || pending[0].Reason.Kind != dispatch.ReasonLimit {
		t.Fatalf("pending: %+v, %v", pending, err)
	}
	running, err := inst.Tasks(ctx, dispatch.Running, 10)
	if err != nil || len(running) != 1 || running[0].Spec.ID != "first" {
		t.Fatalf("running: %+v, %v", running, err)
	}
	if _, err := inst.Tasks(ctx, "sideways", 10); !errors.Is(err, dispatch.ErrInvalid) {
		t.Errorf("an unknown state: %v", err)
	}
	subs, err := inst.EventsOf(ctx, 0, dispatch.TypeSubmitted, 10)
	if err != nil || len(subs) != 3 || subs[0].Subject != "first" || subs[2].Subject != "high" {
		t.Fatalf("submitted: %+v, %v", subs, err)
	}
	if later, _ := inst.EventsOf(ctx, subs[1].Seq, dispatch.TypeSubmitted, 10); len(later) != 1 || later[0].Subject != "high" {
		t.Errorf("after %d: %+v", subs[1].Seq, later)
	}
	last, err := inst.Last(ctx, dispatch.TypeSubmitted, 2)
	if err != nil || len(last) != 2 || last[0].Subject != "low" || last[1].Subject != "high" {
		t.Errorf("last two: %+v, %v", last, err)
	}
	if all, _ := inst.Last(ctx, "", 1); len(all) != 1 || all[0].Seq < subs[2].Seq {
		t.Errorf("last of the log: %+v", all)
	}
}
