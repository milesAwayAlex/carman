package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/cron"
)

// What the expectations record.
const (
	// TypeRunMissed is a CronJob's tick for which no Job started. Subject
	// is the CronJob; data a RunMissed. Its ID is TypeRunMissed/CRONJOB/TICK,
	// so that it is recorded once however often the service restarts.
	TypeRunMissed = "run.missed"
	SourceExpect  = "cronjob-expectations"
	// AnnotationGrace on a CronJob replaces the time its Jobs are given
	// (a Go duration), for one whose runs are known to start late or soon.
	AnnotationGrace = "dispatcher/grace"
)

// RunMissed is the data of a run.missed event.
type RunMissed struct {
	CronJob  string    `json:"cronjob"`
	Schedule string    `json:"schedule"`
	TimeZone string    `json:"time_zone,omitempty"`
	Tick     time.Time `json:"tick"`
	Grace    string    `json:"grace"`
}

// Marks is where the expectations keep what they have judged.
type Marks interface {
	Mark(ctx context.Context, name string, v any) (bool, error)
	SetMark(ctx context.Context, name string, v any) error
}

// Expect watches the CronJobs the dispatcher does not start. For each tick
// of a CronJob that was not suspended when last seen before the tick, it
// expects a Job of that CronJob, for that tick or a later one, within the
// grace: the CronJob's own catch-up (startingDeadlineSeconds) and Grace
// beyond it. A tick without one is recorded once, as run.missed. Ticks from
// before the service first saw the CronJob are not judged.
//
// A CronJob suspended between the service's last look and its next tick
// counts that tick as missed: the service cannot tell it from one the
// cluster did not start.
type Expect struct {
	Client *Client
	Emit   func(context.Context, dispatch.Event) error
	Marks  Marks
	// Ended lists the Jobs of a CronJob whose end the log has, from since on.
	Ended func(ctx context.Context, cronjob string, since time.Time) (map[string]bool, error)
	Log   *slog.Logger
	// Grace is added to each CronJob's own catch-up; an hour when zero.
	Grace time.Duration
	// Every is how often it looks; thirty seconds when zero.
	Every time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	warned map[string]string
}

type cronJobObject struct {
	Metadata struct {
		Name        string            `json:"name"`
		UID         string            `json:"uid"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Schedule                string  `json:"schedule"`
		TimeZone                *string `json:"timeZone"`
		Suspend                 *bool   `json:"suspend"`
		StartingDeadlineSeconds *int64  `json:"startingDeadlineSeconds"`
	} `json:"spec"`
}

// expectation is what is kept for one CronJob, under expect/UID.
type expectation struct {
	CronJob string `json:"cronjob"`
	// Since is when the service first saw it; Seen when it last looked,
	// and Suspended what it saw then.
	Since     time.Time `json:"since"`
	Seen      time.Time `json:"seen"`
	Suspended bool      `json:"suspended"`
	// Pending are the ticks expected and not yet judged.
	Pending []time.Time `json:"pending,omitempty"`
}

// maxPending bounds what is kept for one CronJob: a long outage of a
// CronJob that ticks every minute keeps only its latest ticks.
const maxPending = 2000

// Run looks every Every until ctx ends.
func (x *Expect) Run(ctx context.Context) {
	every := x.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	for {
		if err := x.Pass(ctx); err != nil && ctx.Err() == nil {
			x.Log.Warn("judging cronjobs' runs", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (x *Expect) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// Pass looks once: records the ticks each CronJob owes since the last
// look, and judges those whose grace has passed.
func (x *Expect) Pass(ctx context.Context) error {
	var cjs struct{ Items []cronJobObject }
	if err := x.Client.do(ctx, http.MethodGet, x.Client.list("cronjobs"), nil, &cjs); err != nil {
		return err
	}
	var jobs list
	if err := x.Client.do(ctx, http.MethodGet, x.Client.list("jobs"), nil, &jobs); err != nil {
		return err
	}
	running := map[string]bool{}
	for _, j := range jobs.Items {
		running[j.Metadata.Name] = true
	}
	now := x.now()
	var firstErr error
	for _, cj := range cjs.Items {
		if err := x.judge(ctx, cj, running, now); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", cj.Metadata.Name, err)
		}
	}
	return firstErr
}

func (x *Expect) grace(cj cronJobObject) time.Duration {
	if v := cj.Metadata.Annotations[AnnotationGrace]; v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	g := x.Grace
	if g <= 0 {
		g = time.Hour
	}
	if s := cj.Spec.StartingDeadlineSeconds; s != nil {
		g += time.Duration(*s) * time.Second
	}
	return g
}

func (x *Expect) judge(ctx context.Context, cj cronJobObject, jobs map[string]bool, now time.Time) error {
	name := cj.Metadata.Name
	zone := ""
	if cj.Spec.TimeZone != nil {
		zone = *cj.Spec.TimeZone
	}
	expr, err := cron.Parse(cj.Spec.Schedule, zone)
	if err != nil {
		// Said once per CronJob and schedule, not at every look.
		if x.warned == nil {
			x.warned = map[string]string{}
		}
		if x.warned[name] != cj.Spec.Schedule {
			x.warned[name] = cj.Spec.Schedule
			x.Log.Warn("not judging a cronjob whose schedule this service cannot read", "cronjob", name, "error", err)
		}
		return nil
	}
	suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend
	key := "expect/" + cj.Metadata.UID
	var e expectation
	found, err := x.Marks.Mark(ctx, key, &e)
	if err != nil {
		return err
	}
	if !found {
		e = expectation{CronJob: name, Since: now, Seen: now, Suspended: suspended}
		return x.Marks.SetMark(ctx, key, e)
	}
	// The ticks since the last look are owed if the CronJob was not
	// suspended then.
	if !e.Suspended {
		for t := expr.Next(e.Seen); !t.IsZero() && !t.After(now); t = expr.Next(t) {
			e.Pending = append(e.Pending, t.UTC())
		}
		if n := len(e.Pending); n > maxPending {
			x.Log.Warn("judging only a cronjob's latest ticks", "cronjob", name, "dropped", n-maxPending)
			e.Pending = e.Pending[n-maxPending:]
		}
	}
	e.CronJob, e.Seen, e.Suspended = name, now, suspended
	grace := x.grace(cj)
	var ended map[string]bool
	keep := e.Pending[:0]
	for _, tick := range e.Pending {
		if now.Before(tick.Add(grace)) {
			keep = append(keep, tick)
			continue
		}
		if ended == nil {
			if ended, err = x.Ended(ctx, name, e.Pending[0]); err != nil {
				return err
			}
		}
		if ranFor(name, tick, grace, jobs, ended) {
			continue
		}
		data, _ := json.Marshal(RunMissed{CronJob: name, Schedule: cj.Spec.Schedule, TimeZone: zone, Tick: tick, Grace: grace.String()})
		err := x.Emit(ctx, dispatch.Event{
			ID:   TypeRunMissed + "/" + name + "/" + tick.Format(time.RFC3339),
			Type: TypeRunMissed, Source: SourceExpect, Subject: name, Data: data,
		})
		if err != nil {
			return err
		}
		x.Log.Info("a cronjob's run did not start", "cronjob", name, "tick", tick)
	}
	e.Pending = keep
	return x.Marks.SetMark(ctx, key, e)
}

// ranFor says whether a Job of the CronJob started for tick, or for a later
// tick within grace (a cluster that catches up runs only the latest it
// missed). A CronJob's Job is named for its tick, in minutes since the epoch.
func ranFor(cronjob string, tick time.Time, grace time.Duration, sets ...map[string]bool) bool {
	from, to := tick.Unix()/60, tick.Add(grace).Unix()/60
	for _, set := range sets {
		for job := range set {
			rest, ok := strings.CutPrefix(job, cronjob+"-")
			if !ok {
				continue
			}
			m, err := strconv.ParseInt(rest, 10, 64)
			if err == nil && m >= from && m <= to {
				return true
			}
		}
	}
	return false
}
