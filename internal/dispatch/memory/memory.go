// Package memory is a dispatcher that keeps everything in memory. It is not
// durable on purpose: it exists to prove the acceptance suite, by passing
// every test without a restart and failing every test with one.
//
// Its state is a Store. An instance opened on a fresh Store loses everything
// when killed. Instances opened one after another on one Store behave as if
// the Store were a perfect database; the suite's own test uses that to show
// the restart tests can be passed.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"carman/internal/dispatch"
)

// ErrKilled is returned by every call on an instance after Kill.
var ErrKilled = errors.New("memory: instance killed")

const tick = 50 * time.Millisecond

type task struct {
	spec       dispatch.TaskSpec
	order      int64
	state      dispatch.State
	attempt    int
	startingBy *Dispatcher // the instance whose start call is in progress
	startedAt  time.Time
	wait       *dispatch.Wait
	waitAfter  int64 // seq of the task.finished that asked for the wait
	wake       *dispatch.Wake
	reason     dispatch.Reason
	notStarted bool
	submitted  time.Time
	lapsed     bool // task.after-lapsed was emitted
}

type schedule struct {
	spec     dispatch.Schedule
	lastTick time.Time
}

// inFlight says whether a start call for t is in progress in a live
// instance. Caller holds the store's mu.
func (t *task) inFlight() bool { return t.startingBy != nil && !t.startingBy.killed }

// Store is the state instances share. Every change to it is made under mu,
// so it behaves like a database with serializable transactions.
type Store struct {
	mu        sync.Mutex
	log       []dispatch.Record
	ids       map[string]bool
	tasks     map[string]*task
	order     int64
	schedules map[string]*schedule
	cursor    int // index into log of the next event to act on
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{ids: map[string]bool{}, tasks: map[string]*task{}, schedules: map[string]*schedule{}}
}

// Emit appends an event without acting on it.
func (s *Store) Emit(ctx context.Context, e dispatch.Event) error {
	if err := validEvent(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.append(e)
	return nil
}

// Dispatcher is one in-memory instance.
type Dispatcher struct {
	*Store
	cfg    dispatch.Config
	killed bool // under Store.mu

	quit chan struct{}
	ctx  context.Context
	stop context.CancelFunc
}

// New starts an instance with empty state.
func New(cfg dispatch.Config) *Dispatcher { return Open(NewStore(), cfg) }

// Open starts an instance on s.
func Open(s *Store, cfg dispatch.Config) *Dispatcher {
	ctx, stop := context.WithCancel(context.Background())
	d := &Dispatcher{Store: s, cfg: cfg, quit: make(chan struct{}), ctx: ctx, stop: stop}
	go d.loop()
	return d
}

func validEvent(e dispatch.Event) error {
	if e.ID == "" || e.Type == "" || e.Source == "" {
		return fmt.Errorf("%w: event needs id, type and source", dispatch.ErrInvalid)
	}
	if e.Data != nil && !json.Valid(e.Data) {
		return fmt.Errorf("%w: event data is not JSON", dispatch.ErrInvalid)
	}
	return nil
}

// Kill stops the instance at once; its state is lost.
func (d *Dispatcher) Kill() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.killed {
		d.killed = true
		d.stop()
		close(d.quit)
	}
}

func (d *Dispatcher) Emit(ctx context.Context, e dispatch.Event) error {
	if err := validEvent(e); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return ErrKilled
	}
	d.append(e)
	return nil
}

// append adds e unless its ID is in the log. Caller holds mu.
func (d *Store) append(e dispatch.Event) {
	if d.ids[e.ID] {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	d.ids[e.ID] = true
	d.log = append(d.log, dispatch.Record{Seq: int64(len(d.log) + 1), Event: e})
}

func (d *Store) transition(typ, subject, key string, data dispatch.Transition) {
	body, _ := json.Marshal(data)
	d.append(dispatch.Event{
		ID:      dispatch.TransitionID(typ, subject, key),
		Type:    typ,
		Source:  dispatch.Source,
		Subject: subject,
		Data:    body,
	})
}

func (d *Dispatcher) Events(ctx context.Context, after int64, limit int) ([]dispatch.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return nil, ErrKilled
	}
	var out []dispatch.Record
	for _, r := range d.log {
		if r.Seq > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *Dispatcher) Submit(ctx context.Context, t dispatch.TaskSpec) error {
	if err := t.Check(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return ErrKilled
	}
	d.submit(t)
	return nil
}

func (d *Store) submit(t dispatch.TaskSpec) {
	if _, ok := d.tasks[t.ID]; ok {
		return
	}
	d.order++
	d.tasks[t.ID] = &task{spec: t, order: d.order, state: dispatch.Pending, submitted: time.Now()}
	d.transition(dispatch.TypeSubmitted, t.ID, "", dispatch.Transition{Spec: &t})
}

func (d *Dispatcher) Declare(ctx context.Context, s dispatch.Schedule) error {
	if err := s.Check(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return ErrKilled
	}
	now := time.Now()
	if old, ok := d.schedules[s.Name]; ok {
		// New timing starts from now: ticks of the new timing that passed
		// before it was declared were never due.
		if !sameTiming(old.spec, s) {
			if l := s.Latest(now); l.After(old.lastTick) {
				old.lastTick = l
			}
		}
		old.spec = s
		return nil
	}
	d.schedules[s.Name] = &schedule{spec: s, lastTick: s.Latest(now)}
	return nil
}

func sameTiming(a, b dispatch.Schedule) bool {
	return a.Interval == b.Interval && a.Cron == b.Cron && a.TimeZone == b.TimeZone
}

func (d *Dispatcher) Undeclare(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return ErrKilled
	}
	delete(d.schedules, name)
	return nil
}

func (d *Dispatcher) Schedules(ctx context.Context) ([]dispatch.Schedule, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return nil, ErrKilled
	}
	out := make([]dispatch.Schedule, 0, len(d.schedules))
	for _, s := range d.schedules {
		out = append(out, s.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Dispatcher) Explain(ctx context.Context, id string) (dispatch.TaskStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return dispatch.TaskStatus{}, ErrKilled
	}
	t, ok := d.tasks[id]
	if !ok {
		return dispatch.TaskStatus{}, dispatch.ErrNotFound
	}
	st := dispatch.TaskStatus{Spec: t.spec, State: t.state, Attempt: t.attempt, Reason: t.reason}
	if t.state == dispatch.Waiting {
		w := *t.wait
		st.Reason = dispatch.Reason{Kind: dispatch.ReasonWaiting, Wait: &w}
	} else if t.state != dispatch.Pending || t.inFlight() {
		st.Reason = dispatch.Reason{}
	}
	return st, nil
}

func (d *Dispatcher) loop() {
	timer := time.NewTicker(tick)
	defer timer.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-timer.C:
		}
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		d.actOnEvents()
		d.actOnTime(time.Now())
		d.mu.Unlock()
		d.startWhatCan()
	}
}

// actOnEvents acts on the log from the cursor on. Caller holds mu.
func (d *Dispatcher) actOnEvents() {
	for d.cursor < len(d.log) {
		r := d.log[d.cursor]
		d.cursor++
		if r.Type == dispatch.TypeFinished {
			d.finished(r)
		}
		for _, t := range d.tasks {
			if t.state == dispatch.Waiting && r.Seq > t.waitAfter &&
				r.Type == t.wait.Type && r.Subject == t.wait.Subject {
				ev := r.Event
				d.wakeUp(t, &dispatch.Wake{Wait: *t.wait, Event: &ev})
			}
		}
	}
}

func (d *Dispatcher) finished(r dispatch.Record) {
	t, ok := d.tasks[r.Subject]
	if !ok {
		return
	}
	var f dispatch.Finished
	if json.Unmarshal(r.Data, &f) != nil {
		return
	}
	name := dispatch.AttemptName(t.spec.ID, max(t.attempt, 1))
	if f.Attempt != name || !(t.state == dispatch.Running || t.state == dispatch.Pending) {
		return
	}
	t.startingBy = nil
	if t.state == dispatch.Pending {
		t.attempt = max(t.attempt, 1)
		t.wake = nil
		d.transition(dispatch.TypeStarted, t.spec.ID, name, dispatch.Transition{Attempt: name})
	}
	switch f.Outcome {
	case dispatch.OutcomeDone:
		t.state = dispatch.Done
		d.transition(dispatch.TypeDone, t.spec.ID, name, dispatch.Transition{Attempt: name, Event: r.ID})
	case dispatch.OutcomeWaiting:
		if f.Wait == nil {
			t.state = dispatch.Failed
			d.transition(dispatch.TypeFailed, t.spec.ID, name, dispatch.Transition{Attempt: name, Event: r.ID})
			return
		}
		w := *f.Wait
		t.state = dispatch.Waiting
		t.wait = &w
		t.waitAfter = r.Seq
		d.transition(dispatch.TypeWaiting, t.spec.ID, name, dispatch.Transition{Attempt: name, Event: r.ID, Wait: &w})
	default:
		t.state = dispatch.Failed
		d.transition(dispatch.TypeFailed, t.spec.ID, name, dispatch.Transition{Attempt: name, Event: r.ID})
	}
}

func (d *Dispatcher) wakeUp(t *task, w *dispatch.Wake) {
	name := dispatch.AttemptName(t.spec.ID, t.attempt)
	t.state = dispatch.Pending
	t.attempt++
	t.wait = nil
	t.wake = w
	t.reason = dispatch.Reason{}
	d.transition(dispatch.TypeWoken, t.spec.ID, name, dispatch.Transition{Attempt: name, Wake: w})
}

// blocker is the ID of the task t is held after, or "". Caller holds mu.
func (d *Store) blocker(t *task, now time.Time) string {
	if t.spec.After == "" || t.attempt > 1 || t.lapsed {
		return ""
	}
	var b *task
	for _, o := range d.tasks {
		if o.spec.Class == t.spec.After && o.order < t.order &&
			(o.state == dispatch.Pending || o.state == dispatch.Running || o.state == dispatch.Waiting) &&
			(b == nil || o.order > b.order) {
			b = o
		}
	}
	if b == nil {
		return ""
	}
	if now.Sub(t.submitted) >= t.spec.AfterWithin {
		t.lapsed = true
		d.transition(dispatch.TypeAfterLapsed, t.spec.ID, "", dispatch.Transition{After: b.spec.ID})
		return ""
	}
	return b.spec.ID
}

// actOnTime handles overruns, deadlines, late starts and schedules.
func (d *Dispatcher) actOnTime(now time.Time) {
	for _, t := range d.tasks {
		switch {
		case t.state == dispatch.Running && now.Sub(t.startedAt) > t.spec.AttemptTimeout:
			name := dispatch.AttemptName(t.spec.ID, t.attempt)
			t.state = dispatch.Failed
			d.transition(dispatch.TypeOverran, t.spec.ID, name, dispatch.Transition{Attempt: name})
			d.transition(dispatch.TypeFailed, t.spec.ID, name, dispatch.Transition{Attempt: name, Overran: true})
		case t.state == dispatch.Waiting && !now.Before(t.wait.Deadline):
			d.wakeUp(t, &dispatch.Wake{Wait: *t.wait})
		case t.state == dispatch.Pending && t.attempt == 0 && !t.inFlight() && !t.notStarted &&
			!t.spec.StartBy.IsZero() && now.After(t.spec.StartBy):
			t.notStarted = true
			sb := t.spec.StartBy
			d.transition(dispatch.TypeNotStarted, t.spec.ID, "", dispatch.Transition{StartBy: &sb})
		}
	}
	type due struct {
		s       *schedule
		latest  time.Time
		skipped int
	}
	var dues []due
	for _, s := range d.schedules {
		if latest, skipped := s.spec.Due(s.lastTick, now); !latest.IsZero() {
			dues = append(dues, due{s, latest, skipped})
		}
	}
	sort.Slice(dues, func(i, j int) bool {
		if !dues[i].latest.Equal(dues[j].latest) {
			return dues[i].latest.Before(dues[j].latest)
		}
		return dues[i].s.spec.Name < dues[j].s.spec.Name
	})
	for _, dd := range dues {
		s, latest, skipped := dd.s, dd.latest, dd.skipped
		s.lastTick = latest
		spec := s.spec.Template
		spec.ID = dispatch.ScheduledTaskID(s.spec.Name, latest)
		spec.StartBy = time.Time{}
		if s.spec.StartWithin > 0 {
			spec.StartBy = latest.Add(s.spec.StartWithin)
		}
		d.submit(spec)
		if skipped > 0 {
			tk := latest
			d.transition(dispatch.TypeMissed, s.spec.Name, latest.UTC().Format(time.RFC3339),
				dispatch.Transition{Tick: &tk, Skipped: skipped})
		}
	}
}

// startWhatCan starts pending tasks, one at a time, while the limit allows.
// Admission and the starter are called without holding mu.
func (d *Dispatcher) startWhatCan() {
	asked := map[string]bool{}
	for {
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		var pending []*task
		var running []dispatch.TaskSpec
		for _, t := range d.tasks {
			if t.state == dispatch.Running || t.inFlight() {
				running = append(running, t.spec)
			} else if t.state == dispatch.Pending && !asked[t.spec.ID] {
				if b := d.blocker(t, time.Now()); b != "" {
					t.reason = dispatch.Reason{Kind: dispatch.ReasonAfter, Detail: b}
					continue
				}
				pending = append(pending, t)
			}
		}
		if len(pending) == 0 {
			d.mu.Unlock()
			return
		}
		if len(running) >= d.cfg.MaxRunning {
			for _, t := range pending {
				// What admission said the last time it was asked stands.
				if t.reason.Kind != dispatch.ReasonAdmission {
					t.reason = dispatch.Reason{Kind: dispatch.ReasonLimit}
				}
			}
			d.mu.Unlock()
			return
		}
		sort.Slice(pending, func(i, j int) bool {
			a, b := pending[i], pending[j]
			if a.spec.Priority != b.spec.Priority {
				return a.spec.Priority > b.spec.Priority
			}
			return a.order < b.order
		})
		t := pending[0]
		asked[t.spec.ID] = true
		spec := t.spec
		d.mu.Unlock()

		if d.cfg.Admission != nil {
			ok, why := d.cfg.Admission(d.ctx, spec, running)
			if !ok {
				d.mu.Lock()
				t.reason = dispatch.Reason{Kind: dispatch.ReasonAdmission, Detail: why}
				d.mu.Unlock()
				continue
			}
		}

		d.mu.Lock()
		if d.killed || t.state != dispatch.Pending || t.inFlight() {
			d.mu.Unlock()
			continue
		}
		if t.attempt == 0 {
			t.attempt = 1
		}
		a := dispatch.Attempt{Task: t.spec, Number: t.attempt, Name: dispatch.AttemptName(t.spec.ID, t.attempt), Wake: t.wake}
		t.startingBy = d
		d.mu.Unlock()

		err := d.cfg.Starter.Start(d.ctx, a)

		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		if t.startingBy != d { // finished while the start was in progress
			d.mu.Unlock()
			continue
		}
		t.startingBy = nil
		if err != nil {
			t.reason = dispatch.Reason{Kind: dispatch.ReasonStartFailed, Detail: err.Error()}
		} else {
			t.state = dispatch.Running
			t.wake = nil
			t.startedAt = time.Now()
			t.reason = dispatch.Reason{}
			d.transition(dispatch.TypeStarted, t.spec.ID, a.Name, dispatch.Transition{Attempt: a.Name})
		}
		d.mu.Unlock()
	}
}
