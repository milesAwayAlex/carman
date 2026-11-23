package dispatchtest

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
)

type test struct {
	name    string
	restart bool // kills an instance or opens more than one
	run     func(t *testing.T, f Factory)
}

var tests = []test{
	{"R2_Lifecycle", false, testLifecycle},
	{"R2_Duplicates", false, testDuplicates},
	{"R7_PriorityAndLimit", false, testPriorityAndLimit},
	{"R7_AdmissionHoldsThenReleases", false, testAdmission},
	{"Overrun", false, testOverrun},
	{"NotStartedInTime", false, testNotStarted},
	{"R2_WaitWokenByEvent", false, testWakeByEvent},
	{"R2_WaitWokenByDeadline", false, testWakeByDeadline},
	{"R2_WaitEventBeforeRegistered", false, testEventBeforeRegistered},
	{"R2_WaitSameEventTwice", false, testSameWakeTwice},
	{"R2_Schedule", false, testSchedule},
	{"R6_Restart", true, testRestart},
	{"R6_RestartAtEachStep", true, testRestartAtEachStep},
	{"R6_RestartAsDifferentBuild", true, testDifferentBuild},
	{"R6_TwoInstances", true, testTwoInstances},
}

// Run runs the whole suite against the implementation f makes.
func Run(t *testing.T, f Factory) { run(t, f, true) }

// RunWithoutRestart runs only the tests that never kill an instance or open
// a second one. It exists for the in-memory implementation, which is not
// durable; a candidate uses Run.
func RunWithoutRestart(t *testing.T, f Factory) { run(t, f, false) }

func run(t *testing.T, f Factory, restart bool) {
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.restart && !restart {
				t.Skip("a restart test, and this implementation is not durable")
			}
			t.Parallel()
			tc.run(t, f)
		})
	}
}

func testLifecycle(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	e.submit(d, spec("t1", 0))
	c := e.waitCall(attempt("t1", 1))
	if c.attempt.Number != 1 || c.attempt.Wake != nil || c.attempt.Task.Class != "test" ||
		string(c.attempt.Task.Payload) != `{"n":1}` {
		t.Errorf("Start got %+v", c.attempt)
	}
	e.waitEvent(d, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
	if st := e.explain(d, "t1"); st.State != dispatch.Running || st.Attempt != 1 || st.Reason.Kind != dispatch.ReasonNone {
		t.Errorf("while running, explain says %+v", st)
	}
	e.finish(d, attempt("t1", 1), dispatch.OutcomeDone, nil)
	e.waitState(d, "t1", dispatch.Done)
	e.wantTransitions(d, "t1",
		dispatch.TypeSubmitted, dispatch.TypeStarted+" t1#1", dispatch.TypeDone+" t1#1")

	// The order of the whole log: submitted, started, finished, done.
	var types []string
	for _, r := range e.log(d) {
		if r.Subject == "t1" {
			types = append(types, r.Type)
		}
	}
	want := []string{dispatch.TypeSubmitted, dispatch.TypeStarted, dispatch.TypeFinished, dispatch.TypeDone}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Errorf("log about t1: %v, want %v", types, want)
	}
	if _, err := explainErr(d, "nope"); err != dispatch.ErrNotFound {
		t.Errorf("explain of an unknown task: %v, want ErrNotFound", err)
	}
	e.expectNoCall(attempt("t1", 1), 1)
	e.check(d)
}

func explainErr(d dispatch.Dispatcher, id string) (dispatch.TaskStatus, error) {
	c, cancel := ctx()
	defer cancel()
	return d.Explain(c, id)
}

func testDuplicates(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	e.submit(d, spec("t1", 3))
	e.submit(d, spec("t1", 7)) // same ID: the first spec stays
	ev := dispatch.Event{ID: "e1", Type: "note", Subject: "x", Data: json.RawMessage(`{"a":1}`)}
	e.emit(d, ev)
	ev.Data = json.RawMessage(`{"a":2}`)
	e.emit(d, ev)
	n := 0
	for _, r := range e.log(d) {
		if r.ID == "e1" {
			n++
			if string(r.Data) != `{"a":1}` {
				t.Errorf("the second emit of e1 replaced its data: %s", r.Data)
			}
		}
	}
	if n != 1 {
		t.Errorf("e1 is in the log %d times", n)
	}
	e.waitEvent(d, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
	if st := e.explain(d, "t1"); st.Spec.Priority != 3 {
		t.Errorf("the second submission changed the task: priority %d", st.Spec.Priority)
	}
	// The same finished event twice, then another for the same attempt.
	e.finish(d, attempt("t1", 1), dispatch.OutcomeWaiting, &dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)})
	e.finish(d, attempt("t1", 1), dispatch.OutcomeWaiting, &dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Minute)})
	e.waitState(d, "t1", dispatch.Waiting)
	other := finishedEvent(attempt("t1", 1), dispatch.OutcomeDone, nil)
	other.ID = "finished-again"
	e.emit(d, other)
	time.Sleep(1500 * time.Millisecond)
	if st := e.explain(d, "t1"); st.State != dispatch.Waiting {
		t.Errorf("a second finished event for an attempt that had finished moved the task to %s", st.State)
	}
	e.wantTransitions(d, "t1",
		dispatch.TypeSubmitted, dispatch.TypeStarted+" t1#1", dispatch.TypeWaiting+" t1#1")
	e.expectNoCall(attempt("t1", 1), 1)
	e.check(d)
}

func testPriorityAndLimit(t *testing.T, f Factory) {
	e := newEnv(t, f, 1)
	d := e.open("b1")
	e.submit(d, spec("block", 0))
	e.waitEvent(d, tid(dispatch.TypeStarted, "block", attempt("block", 1)), react)
	e.submit(d, spec("low1", 1))
	e.submit(d, spec("high", 5))
	e.submit(d, spec("low2", 1))
	e.eventually(react, "high to be held by the limit", func() bool {
		return e.explain(d, "high").Reason.Kind == dispatch.ReasonLimit
	})
	for _, id := range []string{"block", "high", "low1", "low2"} {
		e.waitEvent(d, tid(dispatch.TypeStarted, id, attempt(id, 1)), react)
		time.Sleep(300 * time.Millisecond) // nothing else starts while one runs
		if order := e.startOrder(); len(order) != map[string]int{"block": 1, "high": 2, "low1": 3, "low2": 4}[id] {
			t.Fatalf("with a limit of one, started %v", order)
		}
		e.finish(d, attempt(id, 1), dispatch.OutcomeDone, nil)
	}
	want := "block#1 high#1 low1#1 low2#1"
	if got := strings.Join(e.startOrder(), " "); got != want {
		t.Errorf("start order %s, want %s", got, want)
	}
	e.waitState(d, "low2", dispatch.Done)
	e.check(d)
}

func testAdmission(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	e.setHold("held", "over budget")
	e.submit(d, spec("held", 9))
	e.submit(d, spec("free", 1))
	// A held task does not hold back the ones after it.
	e.waitEvent(d, tid(dispatch.TypeStarted, "free", attempt("free", 1)), react)
	e.eventually(react, "held to show admission's reason", func() bool {
		st := e.explain(d, "held")
		return st.State == dispatch.Pending && st.Reason.Kind == dispatch.ReasonAdmission && st.Reason.Detail == "over budget"
	})
	time.Sleep(time.Second)
	e.expectNoCall(attempt("held", 1), 0)
	e.setHold("held", "")
	e.waitEvent(d, tid(dispatch.TypeStarted, "held", attempt("held", 1)), react)
	e.check(d)
}

func testOverrun(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	s := spec("slow", 0)
	s.AttemptTimeout = time.Second
	e.submit(d, s)
	start := e.waitEvent(d, tid(dispatch.TypeStarted, "slow", attempt("slow", 1)), react)
	over := e.waitEvent(d, tid(dispatch.TypeOverran, "slow", attempt("slow", 1)), react)
	if since := over.Time.Sub(start.Time); since < time.Second {
		t.Errorf("overran %s after starting, before its limit of 1s", since)
	}
	e.waitState(d, "slow", dispatch.Failed)
	// A finished event after the overrun changes nothing.
	e.finish(d, attempt("slow", 1), dispatch.OutcomeDone, nil)
	time.Sleep(1500 * time.Millisecond)
	if st := e.explain(d, "slow"); st.State != dispatch.Failed {
		t.Errorf("after the overrun and a late finished event the task is %s", st.State)
	}
	e.wantTransitions(d, "slow", dispatch.TypeSubmitted, dispatch.TypeStarted+" slow#1",
		dispatch.TypeOverran+" slow#1", dispatch.TypeFailed+" slow#1")
	e.check(d)
}

func testNotStarted(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	e.setHold("late", "not now")
	s := spec("late", 0)
	s.StartBy = time.Now().Add(time.Second)
	e.submit(d, s)
	e.waitEvent(d, tid(dispatch.TypeNotStarted, "late", ""), react)
	time.Sleep(1500 * time.Millisecond)
	if st := e.explain(d, "late"); st.State != dispatch.Pending {
		t.Errorf("a task not started in time is %s, want pending", st.State)
	}
	e.setHold("late", "")
	e.waitEvent(d, tid(dispatch.TypeStarted, "late", attempt("late", 1)), react)
	e.wantTransitions(d, "late", dispatch.TypeSubmitted, dispatch.TypeNotStarted, dispatch.TypeStarted+" late#1")
	e.check(d)
}

// startAndWait runs task id to its first attempt and has it finish waiting.
func (e *env) startAndWait(d dispatch.Dispatcher, id string, w dispatch.Wait) {
	e.t.Helper()
	e.submit(d, spec(id, 0))
	e.waitEvent(d, tid(dispatch.TypeStarted, id, attempt(id, 1)), react)
	e.finish(d, attempt(id, 1), dispatch.OutcomeWaiting, &w)
}

func testWakeByEvent(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	w := dispatch.Wait{Type: "ci.done", Subject: "pr-1", Deadline: time.Now().Add(time.Minute)}
	e.startAndWait(d, "t1", w)
	st := e.waitState(d, "t1", dispatch.Waiting)
	if st.Reason.Kind != dispatch.ReasonWaiting || st.Reason.Wait == nil ||
		st.Reason.Wait.Type != "ci.done" || st.Reason.Wait.Subject != "pr-1" || !st.Reason.Wait.Deadline.Equal(w.Deadline) {
		t.Errorf("explain of a waiting task: %+v", st)
	}
	e.emit(d, dispatch.Event{ID: "other", Type: "ci.done", Subject: "pr-2"})
	time.Sleep(time.Second)
	e.expectNoCall(attempt("t1", 2), 0)
	e.emit(d, dispatch.Event{ID: "ci-1", Type: "ci.done", Subject: "pr-1", Data: json.RawMessage(`{"ok":true}`)})
	c := e.waitCall(attempt("t1", 2))
	if c.attempt.Number != 2 || c.attempt.Wake == nil || c.attempt.Wake.Event == nil || c.attempt.Wake.Event.ID != "ci-1" {
		t.Errorf("the woken attempt was told %+v", c.attempt.Wake)
	}
	e.waitEvent(d, tid(dispatch.TypeStarted, "t1", attempt("t1", 2)), react)
	e.finish(d, attempt("t1", 2), dispatch.OutcomeDone, nil)
	e.waitState(d, "t1", dispatch.Done)
	e.wantTransitions(d, "t1", dispatch.TypeSubmitted, dispatch.TypeStarted+" t1#1", dispatch.TypeWaiting+" t1#1",
		dispatch.TypeWoken+" t1#1", dispatch.TypeStarted+" t1#2", dispatch.TypeDone+" t1#2")
	e.check(d)
}

func testWakeByDeadline(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	w := dispatch.Wait{Type: "ci.done", Subject: "pr-1", Deadline: time.Now().Add(2 * time.Second)}
	e.startAndWait(d, "t1", w)
	c := e.waitCall(attempt("t1", 2))
	if early := w.Deadline.Sub(time.Now()); early > clockSkew {
		t.Errorf("woken %s before the deadline", early)
	}
	if c.attempt.Wake == nil || c.attempt.Wake.Event != nil || !c.attempt.Wake.Wait.Deadline.Equal(w.Deadline) {
		t.Errorf("the attempt woken by the deadline was told %+v", c.attempt.Wake)
	}
	e.waitEvent(d, tid(dispatch.TypeWoken, "t1", attempt("t1", 1)), react)
	e.check(d)
}

func testEventBeforeRegistered(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	e.submit(d, spec("t1", 0))
	e.waitEvent(d, tid(dispatch.TypeStarted, "t1", attempt("t1", 1)), react)
	// A matching event before the finished event does not count; one right
	// after it does, though the dispatcher has not yet acted on either.
	e.emit(d, dispatch.Event{ID: "early", Type: "ci.done", Subject: "pr-1"})
	e.finish(d, attempt("t1", 1), dispatch.OutcomeWaiting,
		&dispatch.Wait{Type: "ci.done", Subject: "pr-1", Deadline: time.Now().Add(time.Minute)})
	e.emit(d, dispatch.Event{ID: "right-after", Type: "ci.done", Subject: "pr-1"})
	c := e.waitCall(attempt("t1", 2))
	if c.attempt.Wake == nil || c.attempt.Wake.Event == nil || c.attempt.Wake.Event.ID != "right-after" {
		t.Errorf("woken by %+v, want the event right after the finished one", c.attempt.Wake)
	}
	e.check(d)
}

func testSameWakeTwice(t *testing.T, f Factory) {
	e := newEnv(t, f, 4)
	d := e.open("b1")
	w := dispatch.Wait{Type: "ci.done", Subject: "pr-1", Deadline: time.Now().Add(time.Minute)}
	e.startAndWait(d, "t1", w)
	e.waitState(d, "t1", dispatch.Waiting)
	wake := dispatch.Event{ID: "ci-1", Type: "ci.done", Subject: "pr-1"}
	e.emit(d, wake)
	e.emit(d, wake)
	e.waitEvent(d, tid(dispatch.TypeStarted, "t1", attempt("t1", 2)), react)
	// Attempt 2 waits for the same thing; the same event again is the
	// same event, so only the deadline wakes it.
	w.Deadline = time.Now().Add(2 * time.Second)
	e.finish(d, attempt("t1", 2), dispatch.OutcomeWaiting, &w)
	e.waitState(d, "t1", dispatch.Waiting)
	e.emit(d, wake)
	c := e.waitCall(attempt("t1", 3))
	if c.attempt.Wake == nil || c.attempt.Wake.Event != nil {
		t.Errorf("attempt 3 was woken by %+v, want the deadline", c.attempt.Wake)
	}
	e.expectNoCall(attempt("t1", 2), 1)
	e.check(d)
}

func everySecond(name string) dispatch.Schedule {
	s := spec("", 0)
	s.Class = "scheduled"
	return dispatch.Schedule{Name: name, Interval: time.Second, Template: s}
}

// ticks lists the ticks of the tasks schedule name submitted, in order.
func (e *env) ticks(d dispatch.Dispatcher, name string) []time.Time {
	var out []time.Time
	for _, r := range e.log(d) {
		if r.Type != dispatch.TypeSubmitted || !strings.HasPrefix(r.Subject, name+"@") {
			continue
		}
		tk, err := time.Parse(time.RFC3339, strings.TrimPrefix(r.Subject, name+"@"))
		if err != nil {
			e.t.Errorf("scheduled task %s: %v", r.Subject, err)
		}
		out = append(out, tk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func (e *env) missed(d dispatch.Dispatcher, name string) []dispatch.Transition {
	var out []dispatch.Transition
	for _, r := range e.log(d) {
		if r.Type == dispatch.TypeMissed && r.Subject == name {
			var tr dispatch.Transition
			json.Unmarshal(r.Data, &tr)
			out = append(out, tr)
		}
	}
	return out
}

func testSchedule(t *testing.T, f Factory) {
	e := newEnv(t, f, 8)
	d := e.open("b1")
	c, cancel := ctx()
	defer cancel()
	for range 2 {
		if err := d.Declare(c, everySecond("s")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3500 * time.Millisecond)
	ticks := e.ticks(d, "s")
	if len(ticks) < 2 || len(ticks) > 4 {
		t.Fatalf("in 3.5s a schedule every second fired %d times: %v", len(ticks), ticks)
	}
	for i, tk := range ticks {
		if i > 0 && tk.Sub(ticks[i-1]) != time.Second {
			t.Errorf("ticks %v are not one second apart", ticks)
		}
	}
	if m := e.missed(d, "s"); len(m) != 0 {
		t.Errorf("a schedule that never stopped missed ticks: %+v", m)
	}
	st := e.explain(d, dispatch.ScheduledTaskID("s", ticks[0]))
	if st.Spec.Class != "scheduled" {
		t.Errorf("the scheduled task is %+v", st.Spec)
	}
	e.check(d)
}
