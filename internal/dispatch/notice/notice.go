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
//
// Before a notice is handed on, its words are checked (ADR 0048: the channel
// carries no credentials, personal data or internal identifiers): one that
// looks like a credential, or matches a shape the installation names, is
// replaced by one that says a notice was held back and where to read it.
// This catches only what has a shape: a name, a description of a person or
// a system in plain words, or an identifier the installation did not list,
// goes through.
package notice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/kube"
	"carman/internal/secret"
)

const (
	TypeWithheld = "notice.withheld"
	Source       = "notices"
	// MarkName is where the reader keeps its place in the log.
	MarkName = "notices"
)

// Watched are the event types that may give a notice.
var Watched = []string{kube.TypeJobEnded, kube.TypeRunMissed, dispatch.TypeNotStarted, dispatch.TypeOverran, dispatch.TypeMissed, TypeMessage}

// TypeMessage is a message (ADR 0045): one for the owner is a notice.
const TypeMessage = "message.sent"

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
	// Place is what the owner's own machine is called (ADR 0049): a session
	// there is in a terminal of the owner's, not somewhere to be taken to.
	Place  string
	Logger *slog.Logger
	// Every is how often it reads; five seconds when zero.
	Every time.Duration
	// Withhold are the shapes the installation names that no notice may
	// carry (its account ids, hostnames, e-mail addresses), beside the
	// credentials secret.Find knows.
	Withhold []*regexp.Regexp
}

// Shapes reads the installation's shapes, one regular expression a line,
// matched without regard to case. A line that is not one is matched as
// plain text, and said in the error.
func Shapes(spec string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	var bad []string
	for _, l := range strings.Split(spec, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + l)
		if err != nil {
			bad = append(bad, fmt.Sprintf("line %d", len(out)+1))
			re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(l))
		}
		out = append(out, re)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("not regular expressions, matched as plain text: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// unsafe says what in a notice's words may not go over the channel, if
// anything: a kind of credential, or the number of a shape of the
// installation's (not the shape itself, which may be an identifier).
func (n *Notifier) unsafe(no Notice) (string, bool) {
	words := no.Title + "\n" + no.Text
	if kind, found := secret.Find(words); found {
		return "what looks like " + kind, true
	}
	for i, re := range n.Withhold {
		if re.MatchString(words) {
			return fmt.Sprintf("a shape the installation names (number %d of dispatcher.notices.withhold)", i+1), true
		}
	}
	return "", false
}

// TypeReplaced records a notice that was replaced for what its words held.
const TypeReplaced = "notice.replaced"

// checked is the notice, or the one that stands for it when its words may
// not go over the channel; the event it is about keeps what it said.
func (n *Notifier) checked(ctx context.Context, r dispatch.Record, no Notice) (Notice, error) {
	why, found := n.unsafe(no)
	if !found {
		return no, nil
	}
	where := n.deploy("dispatch log --type " + TypeReplaced)
	var m struct{ ID string }
	if r.Type == TypeMessage && json.Unmarshal(r.Data, &m) == nil && m.ID != "" {
		where = n.deploy("dispatch message " + m.ID)
	}
	data, _ := json.Marshal(map[string]string{"event": r.ID, "type": r.Type, "why": why})
	if err := n.Log.Emit(ctx, dispatch.Event{ID: TypeReplaced + "/" + r.ID, Type: TypeReplaced, Source: Source, Subject: r.Subject, Data: data}); err != nil {
		return no, err
	}
	out := Notice{Urgency: no.Urgency, Importance: no.Importance, Event: r.ID, Title: "A notice was withheld",
		Text: fmt.Sprintf("A notice was not sent as written: its words held %s, which this channel may not carry. Read it where it is kept: %s.", why, where)}
	if _, still := n.unsafe(out); still {
		out.Text = "A notice was not sent as written: its words held what this channel may not carry. Read it in the dispatcher's log, as " + TypeReplaced + "."
	}
	return out, nil
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
	no, err := n.checked(ctx, r, no)
	if err != nil {
		return err
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

// kubeWord is the shape of the cluster's reasons (OOMKilled, Evicted).
var kubeWord = regexp.MustCompile(`^[A-Z][A-Za-z]{1,40}$`)

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
		// Only the cluster's own words for why: its messages and the log's
		// lines stay in the dispatcher's log (ADR 0048).
		more := ""
		if w := j.Why; w != nil {
			for _, r := range []string{w.PodReason, w.Termination} {
				if r != "" && r != j.Reason && kubeWord.MatchString(r) {
					how += ", " + r
				}
			}
			if w.Killed != "" {
				how += ", killed for its " + w.Killed
			}
			more = fmt.Sprintf(" Why, with its log's last lines: %s.", n.deploy("dispatch log --type job.ended"))
		}
		no.Title = fmt.Sprintf("%s failed", j.Template)
		no.Text = fmt.Sprintf("The Job %s (%s, started by %s) failed at %s: %s.%s Its log, while the cluster keeps the Job: %s. What ran lately: %s.",
			j.Job, j.Template, j.By, at(j.Ended), strings.TrimPrefix(how, ", "), more, n.kubectl("logs job/"+j.Job), n.deploy("dispatch status"))
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
	case TypeMessage:
		var m struct{ ID, From, Agent, Name, To, Body string }
		if json.Unmarshal(r.Data, &m) != nil || m.To != "owner" {
			return no, false
		}
		who := m.From
		if m.Agent != "" {
			who = "agent " + m.Agent
			if m.Name != "" {
				who += ", session " + m.Name
			}
		}
		// The notice goes over a chat channel that is mostly trusted and not
		// private (ADR 0048): it carries the message's first line, which the
		// sender writes to say roughly what this is about, and how to read
		// the rest. The body stays in the log.
		about := strings.TrimSpace(m.Body)
		if k := strings.IndexByte(about, '\n'); k >= 0 {
			about = strings.TrimSpace(about[:k])
		}
		if r := []rune(about); len(r) > 240 {
			about = string(r[:240]) + "…"
		}
		no.Title = "A message from " + who
		no.Text = fmt.Sprintf("%s\n\nAll of it: %s. To answer: %s.", about,
			n.deploy("dispatch message "+m.ID), n.deploy("dispatch reply "+m.ID+" 'YOUR ANSWER'"))
		// The session it came from, by the owner's one command that goes to
		// any session (carman go, ADR 0057), which takes the start of an
		// id. Not by name: a job's runs share one, and the name reaches only
		// the latest. A window of a standing agent is gone to as it is; a
		// job's kept conversation still goes through a checkout.
		if id, ok := strings.CutPrefix(m.From, "session/"); ok && (n.Place == "" || m.Agent != n.Place) {
			if len(id) > 8 {
				id = id[:8]
			}
			no.Text += fmt.Sprintf(" To go to its session: carman go %s (a job's kept conversation: with CARMAN_CHECKOUT set to a checkout, or as it then says).", id)
		}
		no.Text += fmt.Sprintf(" (%s, message %s.)", m.From, m.ID)
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
