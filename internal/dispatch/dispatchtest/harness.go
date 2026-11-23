// Package dispatchtest is the acceptance suite every dispatcher
// implementation must pass unchanged. A candidate runs it with one line:
//
//	func TestAcceptance(t *testing.T) { dispatchtest.Run(t, factory) }
//
// See ../README.md.
package dispatchtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// DatabaseEnv names the environment variable with the test database's
// address.
const DatabaseEnv = "DISPATCH_TEST_DATABASE_URL"

// DatabaseURL returns the test database's address, or skips the test when
// it is not set. A factory that needs a database calls it.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv(DatabaseEnv)
	if u == "" {
		t.Skipf("%s is not set", DatabaseEnv)
	}
	return u
}

// Instance is a running dispatcher that the suite can kill.
type Instance interface {
	dispatch.Dispatcher
	// Kill stops the instance as if its process had died: no graceful
	// shutdown path runs, work in flight is abandoned (an open transaction
	// is rolled back, not committed), and once Kill returns the instance
	// makes no further call to the database, the starter or admission.
	// Kill may wait for the instance's goroutines; a Start call in
	// progress returns by itself once Kill is called.
	Kill()
}

// Database is one test's database: empty when the factory returns it.
type Database interface {
	// Open starts a dispatcher instance on this database. build identifies
	// the binary; two opens with different builds are two versions of the
	// program on one database.
	Open(cfg dispatch.Config, build string) (Instance, error)
	// Emit appends an event to the log while no instance may be running,
	// as a watcher does whose event lands while the dispatcher is down.
	// It must not act on the event.
	Emit(ctx context.Context, e dispatch.Event) error
}

// Factory gives a test its own database. It is called once per test, from
// tests that run in parallel, and cleans up with t.Cleanup.
type Factory func(t *testing.T) Database

// react is how long the suite waits for something the dispatcher should do
// within about a second.
const react = 5 * time.Second

const longTimeout = time.Minute

// call is one call the fake starter received.
type call struct {
	attempt  dispatch.Attempt
	instance int
	at       int64 // the env's clock when the call began
}

// env is one test's world: the database, the instances opened on it, the
// fake starter and admission every instance shares.
type env struct {
	t     *testing.T
	db    Database
	max   int
	clock atomic.Int64

	mu        sync.Mutex
	calls     []call
	finished  map[string]int64  // attempt name -> clock when its first task.finished was emitted
	hold      map[string]string // task ID -> admission's reason for holding it
	blockNext bool              // the next Start blocks until its instance is killed
	blocked   chan string       // attempt names of blocked Starts
	started   chan string       // attempt names of Starts that returned nil
	insts     []*inst
}

type inst struct {
	Instance
	n     int
	dying chan struct{}
	dead  atomic.Bool
}

func newEnv(t *testing.T, f Factory, max int) *env {
	e := &env{
		t:        t,
		db:       f(t),
		max:      max,
		finished: map[string]int64{},
		hold:     map[string]string{},
		blocked:  make(chan string, 16),
		started:  make(chan string, 256),
	}
	t.Cleanup(func() {
		for _, i := range e.insts {
			e.kill(i)
		}
	})
	return e
}

// starter is the fake given to one instance.
type starter struct {
	e *env
	i *inst
}

func (s starter) Start(ctx context.Context, a dispatch.Attempt) error {
	e := s.e
	if s.i.dead.Load() {
		e.t.Errorf("instance %d called Start(%s) after it was killed", s.i.n, a.Name)
		return fmt.Errorf("killed")
	}
	e.mu.Lock()
	c := call{attempt: a, instance: s.i.n, at: e.clock.Add(1)}
	e.calls = append(e.calls, c)
	if at, ok := e.finished[a.Name]; ok {
		e.t.Errorf("Start(%s) was called after a task.finished for it was emitted (clock %d > %d)", a.Name, c.at, at)
	}
	block := e.blockNext
	e.blockNext = false
	e.mu.Unlock()
	if block {
		e.blocked <- a.Name
		select {
		case <-s.i.dying:
		case <-time.After(longTimeout):
		}
		return fmt.Errorf("the process died during the start")
	}
	select {
	case e.started <- a.Name:
	default:
	}
	return nil
}

func (e *env) admission(ctx context.Context, task dispatch.TaskSpec, running []dispatch.TaskSpec) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if why, ok := e.hold[task.ID]; ok {
		return false, why
	}
	return true, ""
}

func (e *env) setHold(taskID, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if why == "" {
		delete(e.hold, taskID)
	} else {
		e.hold[taskID] = why
	}
}

func (e *env) blockNextStart() {
	e.mu.Lock()
	e.blockNext = true
	e.mu.Unlock()
}

// open starts an instance with the fake starter and admission.
func (e *env) open(build string) *inst {
	e.t.Helper()
	e.mu.Lock()
	i := &inst{n: len(e.insts) + 1, dying: make(chan struct{})}
	e.insts = append(e.insts, i)
	e.mu.Unlock()
	d, err := e.db.Open(dispatch.Config{
		Starter:    starter{e: e, i: i},
		Admission:  e.admission,
		MaxRunning: e.max,
	}, build)
	if err != nil {
		e.t.Fatalf("open instance %d (build %s): %v", i.n, build, err)
	}
	i.Instance = d
	return i
}

func (e *env) kill(i *inst) {
	if i.dead.Load() || i.Instance == nil {
		return
	}
	close(i.dying)
	i.Kill()
	i.dead.Store(true)
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func spec(id string, priority int) dispatch.TaskSpec {
	return dispatch.TaskSpec{
		ID:             id,
		Class:          "test",
		Priority:       priority,
		Payload:        json.RawMessage(`{"n":1}`),
		AttemptTimeout: longTimeout,
	}
}

func (e *env) submit(d dispatch.Dispatcher, s dispatch.TaskSpec) {
	e.t.Helper()
	c, cancel := ctx()
	defer cancel()
	if err := d.Submit(c, s); err != nil {
		e.t.Fatalf("submit %s: %v", s.ID, err)
	}
}

func (e *env) emit(d interface {
	Emit(context.Context, dispatch.Event) error
}, ev dispatch.Event) {
	e.t.Helper()
	if ev.Source == "" {
		ev.Source = "test"
	}
	c, cancel := ctx()
	defer cancel()
	if err := d.Emit(c, ev); err != nil {
		e.t.Fatalf("emit %s: %v", ev.ID, err)
	}
	if ev.Type == dispatch.TypeFinished {
		var f dispatch.Finished
		json.Unmarshal(ev.Data, &f)
		e.mu.Lock()
		if _, ok := e.finished[f.Attempt]; !ok {
			e.finished[f.Attempt] = e.clock.Add(1)
		}
		e.mu.Unlock()
	}
}

// finishedEvent is the task.finished a watcher emits for an attempt.
func finishedEvent(attempt string, outcome dispatch.Outcome, wait *dispatch.Wait) dispatch.Event {
	data, _ := json.Marshal(dispatch.Finished{Attempt: attempt, Outcome: outcome, Wait: wait})
	taskID := attempt[:strings.LastIndex(attempt, "#")]
	return dispatch.Event{
		ID:      "finished/" + attempt,
		Type:    dispatch.TypeFinished,
		Source:  "watcher",
		Subject: taskID,
		Data:    data,
	}
}

func (e *env) finish(d interface {
	Emit(context.Context, dispatch.Event) error
}, attempt string, outcome dispatch.Outcome, wait *dispatch.Wait) {
	e.t.Helper()
	e.emit(d, finishedEvent(attempt, outcome, wait))
}

func (e *env) callsFor(attempt string) []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []call
	for _, c := range e.calls {
		if c.attempt.Name == attempt {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) startOrder() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		out = append(out, c.attempt.Name)
	}
	return out
}

// eventually polls cond until it holds or the timeout passes.
func (e *env) eventually(timeout time.Duration, what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitCall waits for a Start call for attempt and returns the latest one.
func (e *env) waitCall(attempt string) call {
	e.t.Helper()
	var c call
	e.eventually(react, "Start("+attempt+")", func() bool {
		cs := e.callsFor(attempt)
		if len(cs) > 0 {
			c = cs[len(cs)-1]
		}
		return len(cs) > 0
	})
	return c
}

func (e *env) log(d dispatch.Dispatcher) []dispatch.Record {
	e.t.Helper()
	c, cancel := ctx()
	defer cancel()
	var all []dispatch.Record
	var after int64
	for {
		rs, err := d.Events(c, after, 500)
		if err != nil {
			e.t.Fatalf("events: %v", err)
		}
		if len(rs) == 0 {
			return all
		}
		for _, r := range rs {
			if r.Seq <= after {
				e.t.Fatalf("events out of order: %d after %d", r.Seq, after)
			}
			after = r.Seq
		}
		all = append(all, rs...)
	}
}

func (e *env) hasEvent(d dispatch.Dispatcher, id string) (dispatch.Record, bool) {
	for _, r := range e.log(d) {
		if r.ID == id {
			return r, true
		}
	}
	return dispatch.Record{}, false
}

// waitEvent waits for the event with this ID.
func (e *env) waitEvent(d dispatch.Dispatcher, id string, timeout time.Duration) dispatch.Record {
	e.t.Helper()
	var r dispatch.Record
	e.eventually(timeout, "event "+id, func() bool {
		var ok bool
		r, ok = e.hasEvent(d, id)
		return ok
	})
	return r
}

func tid(typ, subject, key string) string { return dispatch.TransitionID(typ, subject, key) }

func (e *env) explain(d dispatch.Dispatcher, id string) dispatch.TaskStatus {
	e.t.Helper()
	c, cancel := ctx()
	defer cancel()
	st, err := d.Explain(c, id)
	if err != nil {
		e.t.Fatalf("explain %s: %v", id, err)
	}
	return st
}

func (e *env) waitState(d dispatch.Dispatcher, id string, want dispatch.State) dispatch.TaskStatus {
	e.t.Helper()
	var st dispatch.TaskStatus
	e.eventually(react, fmt.Sprintf("%s to be %s", id, want), func() bool {
		st = e.explain(d, id)
		return st.State == want
	})
	return st
}

// transitions lists the dispatcher's events about subject, in log order,
// as "type attempt".
func (e *env) transitions(d dispatch.Dispatcher, subject string) []string {
	var out []string
	for _, r := range e.log(d) {
		if r.Source != dispatch.Source || r.Subject != subject {
			continue
		}
		var tr dispatch.Transition
		json.Unmarshal(r.Data, &tr)
		s := r.Type
		if tr.Attempt != "" {
			s += " " + tr.Attempt
		}
		out = append(out, s)
	}
	return out
}

func (e *env) wantTransitions(d dispatch.Dispatcher, subject string, want ...string) {
	e.t.Helper()
	got := e.transitions(d, subject)
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		e.t.Errorf("transitions of %s:\n got  %s\n want %s", subject, strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

// check is run at the end of every test, on a live instance: no
// transition is in the log twice, under any ID, and every dispatcher event
// carries the ID the contract gives it.
func (e *env) check(d dispatch.Dispatcher) {
	e.t.Helper()
	seen := map[string]bool{}
	for _, r := range e.log(d) {
		if r.Source != dispatch.Source {
			continue
		}
		var tr dispatch.Transition
		if err := json.Unmarshal(r.Data, &tr); err != nil {
			e.t.Errorf("event %s: data is not a Transition: %v", r.ID, err)
		}
		key := tr.Attempt
		if r.Type == dispatch.TypeMissed && tr.Tick != nil {
			key = tr.Tick.UTC().Format(time.RFC3339)
		}
		k := tid(r.Type, r.Subject, key)
		if seen[k] {
			e.t.Errorf("transition %s is in the log twice", k)
		}
		seen[k] = true
		if r.ID != k {
			e.t.Errorf("event %s should have the ID %s", r.ID, k)
		}
	}
}

func (e *env) expectNoCall(attempt string, after int) {
	e.t.Helper()
	if n := len(e.callsFor(attempt)); n != after {
		e.t.Errorf("Start(%s) was called %d times, want %d", attempt, n, after)
	}
}

func attempt(id string, n int) string { return dispatch.AttemptName(id, n) }
