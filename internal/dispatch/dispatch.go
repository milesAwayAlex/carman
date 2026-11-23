// Package dispatch is the dispatcher's contract: the types and interfaces
// every implementation provides, and the behavior they must have. It holds
// no implementation. The acceptance suite is in dispatchtest.
//
// The dispatcher keeps a durable, append-only log of events, runs tasks
// through their life (pending, running, waiting, done, failed), fires
// schedules, and records in the log every transition it makes, so that what
// did not happen is visible too. All of it must survive the dispatcher's
// process dying at any moment (R6).
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Event is one entry in the log. The field names follow the CloudEvents
// attributes of the same names (id, type, source, subject, time, data).
type Event struct {
	// ID is unique in the log. Emitting an event whose ID the log already
	// has changes nothing and is not an error, even if the other fields
	// differ: the first one stays.
	ID string `json:"id"`
	// Type, for example "task.finished". Required.
	Type string `json:"type"`
	// Source says who emitted it. Required.
	Source string `json:"source"`
	// Subject is what the event is about, for example a task's ID.
	Subject string `json:"subject,omitempty"`
	// Time is when it happened. When zero, Emit sets it to the time of
	// appending.
	Time time.Time `json:"time"`
	// Data is the body: JSON, or empty.
	Data json.RawMessage `json:"data,omitempty"`
}

// Record is an event as the log holds it.
type Record struct {
	// Seq is the event's position in the log. It increases along the log
	// but need not be dense.
	Seq int64
	Event
}

// TaskSpec is what a caller submits.
type TaskSpec struct {
	// ID is chosen by the caller. Submitting an ID that exists changes
	// nothing and is not an error: the first spec stays.
	ID string `json:"id"`
	// Class names the kind of work; the starter and admission read it.
	Class string `json:"class"`
	// Priority: higher runs first.
	Priority int `json:"priority"`
	// Payload is passed through to the starter.
	Payload json.RawMessage `json:"payload,omitempty"`
	// StartBy, when not zero, is the time by which the task's first attempt
	// must have started. If it has not, the dispatcher emits one
	// task.not-started event and the task stays pending.
	StartBy time.Time `json:"start_by,omitzero"`
	// AttemptTimeout limits how long one attempt may run, counted from its
	// task.started event. Required, positive.
	AttemptTimeout time.Duration `json:"attempt_timeout"`
}

// State is where a task is in its life.
type State string

const (
	// Pending: submitted or woken, not yet started. A start that has been
	// called but has not yet returned success is still pending.
	Pending State = "pending"
	// Running: the starter returned success for the current attempt, and
	// no task.finished for it has been acted on.
	Running State = "running"
	// Waiting: the current attempt finished asking to wait for an event.
	Waiting State = "waiting"
	// Done and Failed are final.
	Done   State = "done"
	Failed State = "failed"
)

// Attempt is what the starter is given. Attempts are numbered from 1; the
// number goes up only when a waiting task is woken.
type Attempt struct {
	Task   TaskSpec
	Number int
	// Name is AttemptName(Task.ID, Number): the same every time the same
	// attempt is started again, so that a starter can be idempotent.
	Name string
	// Wake says why this attempt was started; nil for attempt 1.
	Wake *Wake
}

// AttemptName is the name of a task's attempt.
func AttemptName(taskID string, number int) string {
	return taskID + "#" + strconv.Itoa(number)
}

// Wait is what a task waits for: an event with this type and subject that
// comes after, in the log, the task.finished event that asked for the wait,
// or else the deadline.
type Wait struct {
	Type     string    `json:"type"`
	Subject  string    `json:"subject"`
	Deadline time.Time `json:"deadline"`
}

// Wake is why a waiting task became pending again.
type Wake struct {
	Wait Wait `json:"wait"`
	// Event is the event that matched; nil when the deadline passed.
	Event *Event `json:"event,omitempty"`
}

// Starter starts the work for an attempt (in production, a Kubernetes Job).
//
// Start may be called more than once for one attempt: after a crash, or after
// it returned an error. It is never called for an attempt once a
// task.finished event for that attempt is in the log, nor for an attempt
// whose task has moved on (overran, waiting, woken, final).
//
// An error means the attempt did not start. The task stays pending and the
// same attempt is started again later, subject to the running limit and
// admission as before.
type Starter interface {
	Start(ctx context.Context, a Attempt) error
}

// Admission says whether task may start now, given the tasks that are
// running (including any whose start is in progress). When it says no, the
// task stays pending, reason is shown by Explain, and it is asked again
// later. A task held back does not hold back tasks after it in order.
type Admission func(ctx context.Context, task TaskSpec, running []TaskSpec) (ok bool, reason string)

// Config is what a dispatcher instance is given when it starts.
type Config struct {
	Starter Starter
	// Admission is nil to admit everything.
	Admission Admission
	// MaxRunning limits how many tasks are running (or being started) at
	// once. Must be positive.
	MaxRunning int
}

// Schedule submits a task every Interval.
//
// Ticks are the multiples of Interval since the Unix epoch, so they are the
// same in every instance. The first tick a schedule fires is the first one
// after it was declared. For tick T the task is Template with ID
// ScheduledTaskID(Name, T), and StartBy T+StartWithin when StartWithin is
// positive.
//
// When ticks have passed without firing (the dispatcher was down), it fires
// once, for the latest tick that has passed, and emits one schedule.missed
// event whose data says how many ticks were skipped.
type Schedule struct {
	// Name identifies the schedule. Declaring a name again replaces its
	// interval and template and keeps what it has fired.
	Name string `json:"name"`
	// Interval is a whole number of seconds, at least one.
	Interval time.Duration `json:"interval"`
	// Template is the task to submit; its ID and StartBy are ignored.
	Template    TaskSpec      `json:"template"`
	StartWithin time.Duration `json:"start_within,omitempty"`
}

// ScheduledTaskID is the ID of the task a schedule submits for a tick.
func ScheduledTaskID(name string, tick time.Time) string {
	return name + "@" + tick.UTC().Format(time.RFC3339)
}

// ReasonKind says why a task is not running.
type ReasonKind string

const (
	// ReasonNone: nothing holds the task back (it is about to start, or is
	// running, or is final).
	ReasonNone ReasonKind = ""
	// ReasonLimit: MaxRunning tasks are running.
	ReasonLimit ReasonKind = "limit"
	// ReasonAdmission: admission said no; Detail is its reason.
	ReasonAdmission ReasonKind = "admission"
	// ReasonWaiting: Wait says for what and until when.
	ReasonWaiting ReasonKind = "waiting"
	// ReasonStartFailed: the last start call returned an error; Detail is it.
	ReasonStartFailed ReasonKind = "start-failed"
)

// Reason is the explanation Explain gives.
type Reason struct {
	Kind   ReasonKind
	Detail string
	Wait   *Wait
}

// TaskStatus is a task's state and, when it is not running, why.
type TaskStatus struct {
	Spec    TaskSpec
	State   State
	Attempt int // the current or next attempt's number, 0 before the first start
	Reason  Reason
}

// Dispatcher is one running instance. It works from the moment it is
// started until its process stops; it reacts to a submission, an event or a
// passing deadline within about a second. More than one instance may run on
// one database for a short time (a rolling restart); together they still
// start each attempt once and record each transition once.
type Dispatcher interface {
	// Emit appends an event to the log. When it returns nil, the event is
	// durable. Its effects (finishing or waking a task) follow
	// asynchronously.
	Emit(ctx context.Context, e Event) error
	// Events returns up to limit records with Seq greater than after, in
	// log order. A reader that has seen Seq n never later sees a new
	// record with Seq at or below n.
	Events(ctx context.Context, after int64, limit int) ([]Record, error)
	// Submit records a task as pending. When it returns nil, the task is
	// durable and its task.submitted event is in the log.
	Submit(ctx context.Context, t TaskSpec) error
	// Declare records a schedule. Durable when it returns nil.
	Declare(ctx context.Context, s Schedule) error
	// Explain returns a task's status, or ErrNotFound.
	Explain(ctx context.Context, taskID string) (TaskStatus, error)
}

var (
	ErrNotFound = errors.New("dispatch: no such task")
	ErrInvalid  = errors.New("dispatch: invalid")
)

// Event types. Those other than TypeFinished are emitted by the dispatcher
// itself, with Source, exactly once per transition however often it crashed,
// and with the ID TransitionID(type, subject, key). Their data is a
// Transition.
const (
	// TypeFinished is emitted from outside, by whatever watches the work.
	// Subject is the task ID; data is a Finished. It is acted on only when
	// its attempt is the task's current one and the task is running or
	// pending; otherwise it is ignored. A pending task's current attempt is
	// the one it would start next: after a crash the dispatcher may not know
	// whether that attempt's start happened, and the watcher's word is taken.
	// The attempt is then not started again, and its task.started is
	// recorded before the outcome, so that the log has every attempt's
	// start before its end.
	TypeFinished = "task.finished"

	TypeSubmitted  = "task.submitted"   // key "": data has Spec
	TypeStarted    = "task.started"     // key attempt name
	TypeDone       = "task.done"        // key attempt name; data has Event
	TypeFailed     = "task.failed"      // key attempt name; data has Event, or Overran
	TypeOverran    = "task.overran"     // key attempt name; precedes its task.failed
	TypeWaiting    = "task.waiting"     // key attempt name; data has Wait
	TypeWoken      = "task.woken"       // key the woken attempt's name; data has Wake
	TypeNotStarted = "task.not-started" // key ""; data has StartBy
	TypeMissed     = "schedule.missed"  // subject schedule name; key the tick fired; data has Tick, Skipped
)

// Source is the source of the dispatcher's own events.
const Source = "dispatcher"

// TransitionID is the ID of a dispatcher event.
func TransitionID(typ, subject, key string) string {
	return typ + "/" + subject + "/" + key
}

// Outcome is how an attempt ended.
type Outcome string

const (
	OutcomeDone    Outcome = "done"
	OutcomeFailed  Outcome = "failed"
	OutcomeWaiting Outcome = "waiting"
)

// Finished is the data of a task.finished event.
type Finished struct {
	Attempt string  `json:"attempt"`
	Outcome Outcome `json:"outcome"`
	// Wait is required when Outcome is waiting.
	Wait    *Wait  `json:"wait,omitempty"`
	Message string `json:"message,omitempty"`
}

// Transition is the data of a dispatcher event; only the fields its type
// names are set.
type Transition struct {
	Attempt string     `json:"attempt,omitempty"`
	Spec    *TaskSpec  `json:"spec,omitempty"`
	Event   string     `json:"event,omitempty"` // the task.finished event's ID
	Overran bool       `json:"overran,omitempty"`
	Wait    *Wait      `json:"wait,omitempty"`
	Wake    *Wake      `json:"wake,omitempty"`
	StartBy *time.Time `json:"start_by,omitempty"`
	Tick    *time.Time `json:"tick,omitempty"`
	Skipped int        `json:"skipped,omitempty"`
}
