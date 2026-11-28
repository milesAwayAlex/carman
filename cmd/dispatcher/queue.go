package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
)

// The queue (ADR 0079): the owner's list of what is
// to be done (ADR 0077's items, under his priorities) worked through by
// itself. While fewer than a set number of its items run, the dispatcher
// takes the next that waits and sends it, as the owner, to an agent that
// takes messages; the item is marked started, and done with the first
// line of the run's answer when the run ends. A run that failed, stopped
// at the plan's limit or ran out of time marks its item failed, which
// nothing takes again by itself, and the queue goes on.
//
// An item runs on the model it names, the strongest when it is marked
// hard, and otherwise the queue's own, which is not the strongest. It is
// started only while usage is at or under its line to each window's reset
// (ADR 0060's rule, asked here whether or not admission is on) and while
// its model is not refused; an item held for its model does not hold the
// ones behind it. The owner's own messages are not the queue's and are
// never held by this.

// TypeQueueStarted records that the queue started an item, and with which
// message; its subject is the item's id.
const TypeQueueStarted = "queue.started"

type queueStarted struct {
	Item    string `json:"item"`
	Message string `json:"message"`
	Model   string `json:"model"`
	Agent   string `json:"agent"`
}

// queueConfig is DISPATCHER_QUEUE, a JSON object.
type queueConfig struct {
	// Agent takes the items: one that takes messages.
	Agent string `json:"agent"`
	// Running is how many items may run at once (1).
	Running int `json:"running"`
	// Model is what an item runs on unless it says; Strongest, what a
	// hard item runs on. Empty: the agent's own first model.
	Model     string `json:"model"`
	Strongest string `json:"strongest"`
}

func parseQueue(raw string) (queueConfig, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return queueConfig{}, false, nil
	}
	var c queueConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, false, fmt.Errorf("DISPATCHER_QUEUE: %w", err)
	}
	if c.Agent == "" {
		return c, false, errors.New("DISPATCHER_QUEUE: agent names the agent that takes the queue's items")
	}
	if c.Running <= 0 {
		c.Running = 1
	}
	if c.Model != "" && c.Model == c.Strongest {
		return c, false, errors.New("DISPATCHER_QUEUE: model is what an item runs on unless it is hard, and is not the strongest")
	}
	return c, true, nil
}

type queueStore interface {
	Claim(ctx context.Context, e dispatch.Event) (bool, error)
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
	Explain(ctx context.Context, taskID string) (dispatch.TaskStatus, error)
}

type queueRunner struct {
	cfg   queueConfig
	list  *budget.Service
	store queueStore
	// send sends an item's words to the agent under the id given; status
	// is what became of a message.
	send   func(ctx context.Context, id, to, body, model string) error
	status func(ctx context.Context, id string) (message.Status, error)
	// hold says whether work on a model waits for usage, and why.
	hold func(ctx context.Context, model string) (bool, string)
	now  func() time.Time
	log  *slog.Logger
}

var queueOwner = message.Caller{Owner: true}

// model is what an item runs on.
func (q *queueRunner) model(it budget.Item) string {
	switch {
	case it.Model != "":
		return it.Model
	case it.Hard:
		return q.cfg.Strongest
	}
	return q.cfg.Model
}

// started are the queue's own starts, the latest for each item.
func (q *queueRunner) started(ctx context.Context) (map[string]queueStarted, error) {
	recs, err := q.store.Last(ctx, TypeQueueStarted, 500)
	if err != nil {
		return nil, err
	}
	out := map[string]queueStarted{}
	var seq = map[string]int64{}
	for _, r := range recs {
		var s queueStarted
		if json.Unmarshal(r.Data, &s) == nil && s.Item != "" && r.Seq > seq[s.Item] {
			out[s.Item], seq[s.Item] = s, r.Seq
		}
	}
	return out, nil
}

// firstLine is the first line of what a run said, cut to fit a list.
func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			return l
		}
	}
	return "(it said nothing)"
}

// outcome is how an item's run ended: the state to give the item and the
// words to keep, or running.
func (q *queueRunner) outcome(ctx context.Context, it budget.Item, s queueStarted) (state, note string, running bool) {
	st, err := q.status(ctx, s.Message)
	if err != nil {
		// Recorded as started and never sent (the dispatcher stopped in
		// between): after a while it is a failure, not a run.
		if q.now().Sub(it.Changed) > 2*time.Minute {
			return budget.Failed, "its message was not sent (the dispatcher stopped as it started it)", false
		}
		return "", "", true
	}
	for _, e := range st.Events {
		if e.Type == message.TypeLimited {
			return budget.Failed, "its run stopped at the plan's usage limit", false
		}
		if e.Type == message.TypeRefused {
			return budget.Failed, "its message was refused: " + firstLine(st.Message.Reason), false
		}
	}
	task, err := q.store.Explain(ctx, message.TaskPrefix+s.Message)
	ended := err == nil && (task.State == dispatch.Done || task.State == dispatch.Failed)
	if len(st.Answers) > 0 {
		words := firstLine(st.Answers[len(st.Answers)-1].Body)
		if err == nil && task.State == dispatch.Failed {
			return budget.Failed, "its run failed, saying: " + words, false
		}
		// The answer is sent as the run ends: wait for the task to say so,
		// a moment at most, so that a failed run is not taken for done.
		if ended || q.now().Sub(st.Answers[len(st.Answers)-1].Sent) > time.Minute {
			return budget.Done, words, false
		}
		return "", "", true
	}
	if ended {
		why := "its run ended with no answer"
		if task.Reason.Kind != "" {
			why += " (" + string(task.Reason.Kind) + ")"
		}
		return budget.Failed, why, false
	}
	return "", "", true
}

// Pass looks once: ends what has ended, and starts what there is room for.
func (q *queueRunner) Pass(ctx context.Context) error {
	view, err := q.list.View(ctx, queueOwner)
	if err != nil {
		return err
	}
	mine, err := q.started(ctx)
	if err != nil {
		return err
	}
	running := 0
	for _, stream := range view.Streams {
		for _, it := range stream.Items {
			s, ok := mine[it.ID]
			if it.State != budget.Started || !ok {
				continue // not begun, or begun by somebody's own hand
			}
			state, note, still := q.outcome(ctx, it, s)
			if still {
				running++
				continue
			}
			if _, err := q.list.ChangeAs(ctx, "the queue", budget.Change{ID: it.ID, State: state, Note: note}); err != nil {
				return err
			}
			q.log.Info("a queued item ended", "item", it.ID, "state", state, "message", s.Message)
		}
	}
	// Priorities in the list's order (the largest share first), and the
	// items of each as the list has them.
	for _, stream := range view.Streams {
		for _, it := range stream.Items {
			if running >= q.cfg.Running {
				return nil
			}
			if it.State != budget.Waiting {
				continue
			}
			model := q.model(it)
			if held, _ := q.hold(ctx, model); held {
				continue // the next may not need what this one waits for
			}
			if err := q.start(ctx, it, model); err != nil {
				return err
			}
			running++
		}
	}
	return nil
}

func (q *queueRunner) start(ctx context.Context, it budget.Item, model string) error {
	id := message.NewID()
	data, _ := json.Marshal(queueStarted{Item: it.ID, Message: id, Model: model, Agent: q.cfg.Agent})
	// The item first: two dispatchers at once (a rolling restart) make one
	// change of the list's same reading, and the other is refused.
	if _, err := q.list.ChangeAs(ctx, "the queue", budget.Change{ID: it.ID, State: budget.Started, Note: "on " + or(model, "the agent's own model")}); err != nil {
		return err
	}
	if _, err := q.store.Claim(ctx, dispatch.Event{ID: TypeQueueStarted + "/" + it.ID + "/" + id, Type: TypeQueueStarted, Source: "dispatcher", Subject: it.ID, Time: q.now(), Data: data}); err != nil {
		return err
	}
	body := fmt.Sprintf("From the owner's queue, item %s (priority %s). Do it, unattended: nobody is at hand to ask. What you say last is the result he is told, and its first line is what his list shows: start with one plain sentence that says what came of it.\n\n%s", it.ID, it.Priority, it.Title)
	if err := q.send(ctx, id, q.cfg.Agent, body, model); err != nil {
		_, _ = q.list.ChangeAs(ctx, "the queue", budget.Change{ID: it.ID, State: budget.Failed, Note: "it could not be sent to " + q.cfg.Agent + ": " + firstLine(err.Error())})
		return nil
	}
	q.log.Info("the queue started an item", "item", it.ID, "agent", q.cfg.Agent, "model", model, "message", id)
	return nil
}

// Held is why an item that waits is not started now; empty when it is next.
func (q *queueRunner) Held(ctx context.Context, it budget.Item) string {
	if held, why := q.hold(ctx, q.model(it)); held {
		return why
	}
	return ""
}

// Run looks every ten seconds until the context ends.
func (q *queueRunner) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		if err := q.Pass(ctx); err != nil && ctx.Err() == nil {
			q.log.Warn("the queue's pass", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// queueHold is the queue's rule for usage (ADR 0079, as the owner agreed
// it: paced against the line to the weekly reset). It judges by the
// standing of the account the proxy holds now, as the log has it, so that
// after a change of account the other's last reading holds nothing:
//
//   - a model that is refused holds the items that need it until its reset;
//   - a weekly window (the seven-day one, and for an item that needs a
//     model with a week of its own, that week) ahead of its line to its
//     reset holds work that can wait, which all of the queue's is, unless
//     it resets within soon with room under the target left;
//   - the five-hour window is a rate limit, and holds only at the ceiling
//     the plan keeps for the owner (ADR 0077): under it a week is spent,
//     not saved.
func queueHold(readings func(context.Context) ([]dispatch.Record, error), accounts func(context.Context) (meter.Logged, error), pacing paceConfig, ceiling float64, now func() time.Time) func(context.Context, string) (bool, string) {
	const week = 7 * 24 * time.Hour
	return func(ctx context.Context, model string) (bool, string) {
		at := now()
		if model != "" {
			// Only the refusal: no priority is below nothing.
			rule := budgetRule{Readings: readings, Now: now, Models: func(context.Context, dispatch.TaskSpec) ([]string, error) { return []string{model}, nil }}
			if ok, why := rule.admit(ctx, dispatch.TaskSpec{}, nil); !ok {
				return true, strings.NewReplacer(", the first model of this task's ladder,", "", "; this task waits for it", "").Replace(why)
			}
		}
		l, err := accounts(ctx)
		a, known := l.Accounts[l.Held]
		if err != nil || !known || a.At.IsZero() {
			return false, "" // nothing is known of the account held: nothing is held for it
		}
		of := ""
		if len(l.Accounts) > 1 {
			of = " of account " + l.Held
		}
		if w := a.FiveHour; ceiling > 0 && w.Utilization >= ceiling && time.Unix(w.ResetsAt, 0).After(at) {
			return true, fmt.Sprintf("the five-hour window%s is at %s, at the ceiling of %s above which only the owner's own work runs; it resets at %s",
				of, pct(w.Utilization), pct(ceiling), time.Unix(w.ResetsAt, 0).UTC().Format("15:04 UTC"))
		}
		weeks := []struct {
			name string
			w    meter.Window
		}{{"the seven-day window", a.SevenDay}}
		if own, ok := a.Own[model]; ok && model != "" {
			weeks = append(weeks, struct {
				name string
				w    meter.Window
			}{model + "'s own week", own})
		}
		for _, x := range weeks {
			reset := time.Unix(x.w.ResetsAt, 0)
			if x.w.ResetsAt == 0 || !reset.After(at) {
				continue // a window whose reset has passed is new
			}
			start := reset.Add(-week)
			line := pacing.target * float64(at.Sub(start)) / float64(week)
			if x.w.Utilization <= line || (reset.Sub(at) <= pacing.soon && x.w.Utilization < pacing.target) {
				continue
			}
			until := reset
			if x.w.Utilization < pacing.target {
				until = start.Add(time.Duration(x.w.Utilization / pacing.target * float64(week)))
			}
			return true, fmt.Sprintf("%s%s is at %s, ahead of its line to the weekly reset (%s now, %s at its reset at %s); it is expected to start about %s",
				x.name, of, pct(x.w.Utilization), pct(line), pct(pacing.target), reset.UTC().Format("2006-01-02 15:04 UTC"), until.UTC().Format("2006-01-02 15:04 UTC"))
		}
		return false, ""
	}
}
