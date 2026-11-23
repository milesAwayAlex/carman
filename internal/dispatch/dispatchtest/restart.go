package dispatchtest

import (
	"fmt"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// testRestart kills an instance with work in every state and checks that a
// new one carries on from where it stopped.
func testRestart(t *testing.T, f Factory) {
	t.Run("PendingNotLost", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.setHold("t1", "wait")
		e.submit(a, spec("t1", 0))
		e.kill(a)
		e.setHold("t1", "")
		b := e.open("b1")
		e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
		e.check(b)
	})
	t.Run("RunningFinishesAfter", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.submit(a, spec("t1", 0))
		e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
		e.kill(a)
		b := e.open("b1")
		if st := e.explain(b, "t1"); st.State != dispatch.Running {
			t.Errorf("after a restart a running task is %s", st.State)
		}
		e.finish(b, attempt("t1", 1), dispatch.OutcomeDone, nil)
		e.waitState(b, "t1", dispatch.Done)
		e.expectNoCall(attempt("t1", 1), 1)
		e.check(b)
	})
	t.Run("FinishedWhileDown", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.submit(a, spec("t1", 0))
		e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
		e.kill(a)
		e.finish(e.db, attempt("t1", 1), dispatch.OutcomeDone, nil)
		b := e.open("b1")
		e.waitState(b, "t1", dispatch.Done)
		e.expectNoCall(attempt("t1", 1), 1)
		e.check(b)
	})
	t.Run("DeadlinePassedWhileDown", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.startAndWait(a, "t1", dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Second)})
		e.waitState(a, "t1", dispatch.Waiting)
		e.kill(a)
		time.Sleep(2 * time.Second)
		b := e.open("b1")
		c := e.waitCall(attempt("t1", 2))
		if c.attempt.Wake == nil || c.attempt.Wake.Event != nil {
			t.Errorf("woken by %+v, want the deadline", c.attempt.Wake)
		}
		e.check(b)
	})
	t.Run("WaitingWokenAfter", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.startAndWait(a, "t1", dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)})
		e.waitState(a, "t1", dispatch.Waiting)
		e.kill(a)
		b := e.open("b1")
		if st := e.explain(b, "t1"); st.State != dispatch.Waiting || st.Reason.Wait == nil {
			t.Errorf("after a restart a waiting task is %+v", st)
		}
		time.Sleep(time.Second)
		e.expectNoCall(attempt("t1", 2), 0)
		e.emit(b, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
		c := e.waitCall(attempt("t1", 2))
		if c.attempt.Wake == nil || c.attempt.Wake.Event == nil || c.attempt.Wake.Event.ID != "wake" {
			t.Errorf("woken by %+v, want the event", c.attempt.Wake)
		}
		e.check(b)
	})
	t.Run("WakeEventWhileDown", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 4)
		a := e.open("b1")
		e.startAndWait(a, "t1", dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)})
		e.waitState(a, "t1", dispatch.Waiting)
		e.kill(a)
		e.emit(e.db, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
		b := e.open("b1")
		c := e.waitCall(attempt("t1", 2))
		if c.attempt.Wake == nil || c.attempt.Wake.Event == nil || c.attempt.Wake.Event.ID != "wake" {
			t.Errorf("woken by %+v, want the event", c.attempt.Wake)
		}
		e.check(b)
	})
	t.Run("ScheduleCatchesUp", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 8)
		a := e.open("b1")
		c, cancel := ctx()
		defer cancel()
		if err := a.Declare(c, everySecond("s")); err != nil {
			t.Fatal(err)
		}
		e.eventually(react, "the schedule to fire", func() bool { return len(e.ticks(a, "s")) > 0 })
		e.kill(a)
		time.Sleep(3500 * time.Millisecond)
		b := e.open("b1")
		time.Sleep(2500 * time.Millisecond)
		ticks := e.ticks(b, "s")
		var gaps []int
		for i := 1; i < len(ticks); i++ {
			if n := int(ticks[i].Sub(ticks[i-1]) / time.Second); n > 1 {
				gaps = append(gaps, n)
			}
		}
		m := e.missed(b, "s")
		if len(gaps) != 1 || gaps[0] < 3 || len(m) != 1 || m[0].Skipped != gaps[0]-1 {
			t.Errorf("down for 3.5s: ticks %v, missed %+v; want one gap of 3 or more ticks and one schedule.missed skipping all but the last", ticks, m)
		}
		e.check(b)
	})
	t.Run("PriorityAndHoldsAsBefore", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, f, 1)
		a := e.open("b1")
		e.submit(a, spec("block", 0))
		e.waitEvent(a, tid(dispatch.TypeStarted, "block", attempt("block", 1)), react)
		e.setHold("held", "over budget")
		e.submit(a, spec("low", 1))
		e.submit(a, spec("high", 5))
		e.submit(a, spec("held", 9))
		e.kill(a)
		b := e.open("b1")
		e.finish(b, attempt("block", 1), dispatch.OutcomeDone, nil)
		e.waitEvent(b, tid(dispatch.TypeStarted, "high", attempt("high", 1)), react)
		e.eventually(react, "held to show admission's reason", func() bool {
			return e.explain(b, "held").Reason.Kind == dispatch.ReasonAdmission
		})
		e.finish(b, attempt("high", 1), dispatch.OutcomeDone, nil)
		e.waitEvent(b, tid(dispatch.TypeStarted, "low", attempt("low", 1)), react)
		e.expectNoCall(attempt("held", 1), 0)
		e.check(b)
	})
}

// testRestartAtEachStep kills the instance at each point of a task's life;
// whatever the point, the task ends as it would have without the crash.
func testRestartAtEachStep(t *testing.T, f Factory) {
	wait := func() *dispatch.Wait {
		return &dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)}
	}
	plain := []string{dispatch.TypeSubmitted, dispatch.TypeStarted + " t1#1", dispatch.TypeDone + " t1#1"}
	woken := []string{dispatch.TypeSubmitted, dispatch.TypeStarted + " t1#1", dispatch.TypeWaiting + " t1#1",
		dispatch.TypeWoken + " t1#1", dispatch.TypeStarted + " t1#2", dispatch.TypeDone + " t1#2"}

	steps := []struct {
		name string
		want []string
		// run drives the task from submission to done, killing a and
		// opening another instance at its step; it returns the live one.
		run func(e *env, a *inst) *inst
	}{
		{"AfterSubmit", plain, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			e.kill(a)
			b := e.open("b1")
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(b, attempt("t1", 1), dispatch.OutcomeDone, nil)
			return b
		}},
		{"DuringStart", plain, func(e *env, a *inst) *inst {
			e.blockNextStart()
			e.submit(a, spec("t1", 0))
			<-e.blocked
			e.kill(a)
			b := e.open("b1")
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(b, attempt("t1", 1), dispatch.OutcomeDone, nil)
			return b
		}},
		{"DuringStartFinishedWhileDown", plain, func(e *env, a *inst) *inst {
			e.blockNextStart()
			e.submit(a, spec("t1", 0))
			<-e.blocked
			e.kill(a)
			e.finish(e.db, attempt("t1", 1), dispatch.OutcomeDone, nil)
			return e.open("b1")
		}},
		{"AfterStart", plain, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			<-e.started
			e.kill(a)
			b := e.open("b1")
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(b, attempt("t1", 1), dispatch.OutcomeDone, nil)
			return b
		}},
		{"AfterFinishedDone", plain, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(a, attempt("t1", 1), dispatch.OutcomeDone, nil)
			e.kill(a)
			return e.open("b1")
		}},
		{"AfterFinishedWaiting", woken, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(a, attempt("t1", 1), dispatch.OutcomeWaiting, wait())
			e.kill(a)
			b := e.open("b1")
			e.waitState(b, "t1", dispatch.Waiting)
			e.emit(b, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 2)), react)
			e.finish(b, attempt("t1", 2), dispatch.OutcomeDone, nil)
			return b
		}},
		{"AfterWakeEvent", woken, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(a, attempt("t1", 1), dispatch.OutcomeWaiting, wait())
			e.waitState(a, "t1", dispatch.Waiting)
			e.emit(a, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
			e.kill(a)
			b := e.open("b1")
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 2)), react)
			e.finish(b, attempt("t1", 2), dispatch.OutcomeDone, nil)
			return b
		}},
		{"AfterSecondStart", woken, func(e *env, a *inst) *inst {
			e.submit(a, spec("t1", 0))
			e.waitEvent(a, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
			e.finish(a, attempt("t1", 1), dispatch.OutcomeWaiting, wait())
			e.waitState(a, "t1", dispatch.Waiting)
			<-e.started // attempt 1's
			e.emit(a, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
			<-e.started
			e.kill(a)
			b := e.open("b1")
			e.waitEvent(b, tid(dispatch.TypeStarted, "t1", attempt("t1", 2)), react)
			e.finish(b, attempt("t1", 2), dispatch.OutcomeDone, nil)
			return b
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, f, 4)
			b := s.run(e, e.open("b1"))
			e.waitState(b, "t1", dispatch.Done)
			time.Sleep(500 * time.Millisecond)
			e.wantTransitions(b, "t1", s.want...)
			e.check(b)
		})
	}
}

// testDifferentBuild restarts as another build with work in every state.
func testDifferentBuild(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	a := e.open("build-1")
	c, cancel := ctx()
	defer cancel()
	if err := a.Declare(c, everySecond("s")); err != nil {
		t.Fatal(err)
	}
	e.submit(a, spec("running", 0))
	e.startAndWait(a, "waiting", dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)})
	e.setHold("pending", "later")
	e.submit(a, spec("pending", 0))
	e.waitEvent(a, tid(dispatch.TypeStarted, "running", attempt("running", 1)), react)
	e.waitState(a, "waiting", dispatch.Waiting)
	e.eventually(react, "the schedule to fire", func() bool { return len(e.ticks(a, "s")) > 0 })
	before := len(e.ticks(a, "s"))
	e.kill(a)

	b := e.open("build-2")
	e.finish(b, attempt("running", 1), dispatch.OutcomeDone, nil)
	e.emit(b, dispatch.Event{ID: "wake", Type: "x", Subject: "y"})
	e.setHold("pending", "")
	e.waitState(b, "running", dispatch.Done)
	e.waitEvent(b, tid(dispatch.TypeStarted, "waiting", attempt("waiting", 2)), react)
	e.waitEvent(b, tid(dispatch.TypeStarted, "pending", attempt("pending", 1)), react)
	e.eventually(react, "the schedule to fire again", func() bool { return len(e.ticks(b, "s")) > before })
	e.check(b)
}

// testTwoInstances runs two instances on one database at once, as during
// a rolling restart: each attempt is started once, and work submitted to
// one is seen and finished through the other.
func testTwoInstances(t *testing.T, f Factory) {
	e := newEnv(t, f, 20)
	ins := []*inst{e.open("b1"), e.open("b2")}
	const n = 10
	for i := range n {
		e.submit(ins[i%2], spec(fmt.Sprintf("t%d", i), 0))
	}
	for i := range n {
		id := fmt.Sprintf("t%d", i)
		e.waitEvent(ins[(i+1)%2], tid(dispatch.TypeStarted, id, attempt(id, 1)), react)
		e.finish(ins[(i+1)%2], attempt(id, 1), dispatch.OutcomeDone, nil)
	}
	for i := range n {
		id := fmt.Sprintf("t%d", i)
		e.waitState(ins[i%2], id, dispatch.Done)
		e.expectNoCall(attempt(id, 1), 1)
	}
	time.Sleep(500 * time.Millisecond)
	for i := range n {
		e.expectNoCall(attempt(fmt.Sprintf("t%d", i), 1), 1)
	}
	e.check(ins[0])
}
