// Package notice tells the owner of what went wrong, from the dispatcher's
// log: a Job that failed, a CronJob's run that did not start, a task that
// did not start in time or ran too long, a schedule that skipped ticks.
//
// A notice is a task like any other, of a class whose template sends it
// (the installation's notify-owner, in the agent image): the dispatcher's
// own image has no way to reach a person. Each event gives at most one
// notice, whose task ID is derived from the event's, so that reading the
// log again after a restart sends nothing twice. Switched off, each is
// recorded as notice.withheld, with the words it would have sent.
package notice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/kube"
)

const (
	TypeWithheld = "notice.withheld"
	Source       = "notices"
	// MarkName is where the reader keeps its place in the log.
	MarkName = "notices"
)

// Watched are the event types that may give a notice.
var Watched = []string{kube.TypeJobEnded, kube.TypeRunMissed, dispatch.TypeNotStarted, dispatch.TypeOverran, dispatch.TypeMissed}

// Notice is what would be sent: the payload's variables for the template.
type Notice struct {
	Title      string `json:"title"`
	Text       string `json:"text"`
	Urgency    string `json:"urgency"`    // now, today, whenever
	Importance string `json:"importance"` // high, normal, low
	Event      string `json:"event"`      // the event it is about
}

// Log is what the reader needs of the dispatcher.
type Log interface {
	EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error)
	Explain(ctx context.Context, taskID string) (dispatch.TaskStatus, error)
	Submit(ctx context.Context, t dispatch.TaskSpec) error
	Emit(ctx context.Context, e dispatch.Event) error
	Mark(ctx context.Context, name string, v any) (bool, error)
	SetMark(ctx context.Context, name string, v any) error
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
}

// Notifier reads the log and gives the notices.
type Notifier struct {
	Log Log
	// Enabled sends notices; off, they are withheld.
	Enabled bool
	// Class is the notice tasks' class: the CronJob whose template sends one.
	Class string
	// Context and Namespace are named in the commands a notice suggests.
	Context, Namespace string
	Logger             *slog.Logger
	// Every is how often it reads; five seconds when zero.
	Every time.Duration
}

type place struct {
	Seq int64 `json:"seq"`
}

// TaskID is the ID of the notice task for an event.
func TaskID(eventID string) string { return "notice/" + eventID }

// Run reads until ctx ends. On its first start it begins at the end of the
// log: what happened before notices existed is not told.
func (n *Notifier) Run(ctx context.Context) {
	every := n.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	for {
		if err := n.Pass(ctx); err != nil && ctx.Err() == nil {
			n.Logger.Warn("reading the log for notices", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Pass reads what the log has gained since the last pass.
func (n *Notifier) Pass(ctx context.Context) error {
	var p place
	found, err := n.Log.Mark(ctx, MarkName, &p)
	if err != nil {
		return err
	}
	if !found {
		last, err := n.Log.Last(ctx, "", 1)
		if err != nil {
			return err
		}
		if len(last) > 0 {
			p.Seq = last[0].Seq
		}
		return n.Log.SetMark(ctx, MarkName, p)
	}
	for {
		recs, err := n.Log.EventsOfTypes(ctx, p.Seq, Watched, 200)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := n.handle(ctx, r); err != nil {
				return err
			}
			p.Seq = r.Seq
		}
		if len(recs) > 0 {
			if err := n.Log.SetMark(ctx, MarkName, p); err != nil {
				return err
			}
		}
		if len(recs) < 200 {
			return nil
		}
	}
}

func (n *Notifier) handle(ctx context.Context, r dispatch.Record) error {
	no, ok := n.Compose(ctx, r)
	if !ok {
		return nil
	}
	if !n.Enabled {
		data, _ := json.Marshal(no)
		return n.Log.Emit(ctx, dispatch.Event{ID: TypeWithheld + "/" + r.ID, Type: TypeWithheld, Source: Source, Subject: r.Subject, Data: data})
	}
	payload, _ := json.Marshal(kube.Payload{Env: map[string]string{
		"TASK_TITLE": no.Title, "TASK_TEXT": no.Text, "TASK_URGENCY": no.Urgency, "TASK_IMPORTANCE": no.Importance,
	}})
	return n.Log.Submit(ctx, dispatch.TaskSpec{
		ID: TaskID(r.ID), Class: n.Class, Priority: 1000, Payload: payload,
		AttemptTimeout: 10 * time.Minute,
	})
}

func (n *Notifier) isNotice(taskID string) bool { return strings.HasPrefix(taskID, "notice/") }

func at(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func (n *Notifier) kubectl(args string) string {
	return fmt.Sprintf("kubectl --context %s -n %s %s", n.Context, n.Namespace, args)
}

func (n *Notifier) deploy(args string) string {
	return fmt.Sprintf("scripts/deploy.sh %s %s", n.Context, args)
}

// Compose gives the notice an event calls for, if any.
func (n *Notifier) Compose(ctx context.Context, r dispatch.Record) (Notice, bool) {
	no := Notice{Urgency: "today", Importance: "normal", Event: r.ID}
	switch r.Type {
	case kube.TypeJobEnded:
		var j kube.JobEnded
		if json.Unmarshal(r.Data, &j) != nil || j.Outcome != "failed" {
			return no, false
		}
		if j.Template == n.Class || n.isNotice(j.Task) {
			n.Logger.Error("a notice could not be sent", "job", j.Job, "task", j.Task, "reason", j.Reason)
			return no, false
		}
		// A program that reports its own failure did, if it ended on its
		// own (it has an exit code); killed, it could not.
		if j.NotifiesOwner && j.ExitCode != nil && j.Reason != "DeadlineExceeded" {
			return no, false
		}
		how := j.Reason
		if j.ExitCode != nil {
			how += fmt.Sprintf(", exit code %d", *j.ExitCode)
		}
		no.Title = fmt.Sprintf("%s failed", j.Template)
		no.Text = fmt.Sprintf("The Job %s (%s, started by %s) failed at %s: %s. Its log, while the cluster keeps the Job: %s. What ran lately: %s.",
			j.Job, j.Template, j.By, at(j.Ended), strings.TrimPrefix(how, ", "), n.kubectl("logs job/"+j.Job), n.deploy("dispatch status"))
	case kube.TypeRunMissed:
		var m kube.RunMissed
		if json.Unmarshal(r.Data, &m) != nil {
			return no, false
		}
		no.Title = fmt.Sprintf("%s did not run", m.CronJob)
		no.Text = fmt.Sprintf("The CronJob %s (%q, %s) was due to run at %s, and no Job of it started within %s. It may be suspended, or the cluster was down past its catch-up. Look: %s, and %s.",
			m.CronJob, m.Schedule, zoneOr(m.TimeZone), at(m.Tick), m.Grace, n.kubectl("get cronjob "+m.CronJob), n.kubectl("get events --field-selector involvedObject.name="+m.CronJob))
	case dispatch.TypeNotStarted, dispatch.TypeOverran:
		if n.isNotice(r.Subject) {
			n.Logger.Error("a notice did not go out", "task", r.Subject, "event", r.Type)
			return no, false
		}
		var tr dispatch.Transition
		_ = json.Unmarshal(r.Data, &tr)
		class, why := "", ""
		if st, err := n.Log.Explain(ctx, r.Subject); err == nil {
			class = " (" + st.Spec.Class + ")"
			if st.Reason.Kind != dispatch.ReasonNone {
				why = fmt.Sprintf(" It is held by %s", st.Reason.Kind)
				if st.Reason.Detail != "" {
					why += ": " + st.Reason.Detail
				}
				why += "."
			}
		}
		if r.Type == dispatch.TypeNotStarted {
			by := ""
			if tr.StartBy != nil {
				by = " by " + at(*tr.StartBy)
			}
			no.Title = fmt.Sprintf("%s has not started", r.Subject)
			no.Text = fmt.Sprintf("The task %s%s was to start%s and has not; it is still waiting to.%s See: %s.",
				r.Subject, class, by, why, n.deploy("dispatch status "+r.Subject))
		} else {
			job := kube.JobName(r.Subject, attemptNumber(tr.Attempt))
			no.Title = fmt.Sprintf("%s ran too long", r.Subject)
			no.Text = fmt.Sprintf("The task %s%s, attempt %s, ran past its time limit and is counted failed; the dispatcher never heard that its Job ended. The Job may still be running: %s, and its log: %s.",
				r.Subject, class, tr.Attempt, n.kubectl("get job "+job), n.kubectl("logs job/"+job))
		}
	case dispatch.TypeMissed:
		var tr dispatch.Transition
		if json.Unmarshal(r.Data, &tr) != nil || tr.Tick == nil {
			return no, false
		}
		no.Urgency, no.Importance = "whenever", "low"
		no.Title = fmt.Sprintf("The schedule %s skipped %d run(s)", r.Subject, tr.Skipped)
		no.Text = fmt.Sprintf("The dispatcher was down across %d tick(s) of the schedule %s; on its return it ran only the latest, %s. Its log: %s.",
			tr.Skipped, r.Subject, at(*tr.Tick), n.deploy("dispatch log --type schedule.missed"))
	default:
		return no, false
	}
	return no, true
}

func zoneOr(z string) string {
	if z == "" {
		return "UTC"
	}
	return z
}

func attemptNumber(name string) int {
	i := strings.LastIndex(name, "#")
	n := 0
	if i >= 0 {
		fmt.Sscanf(name[i+1:], "%d", &n)
	}
	if n == 0 {
		n = 1
	}
	return n
}
