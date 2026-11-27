package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"carman/internal/dispatch"
)

// scheduleValue is one schedule as the installation's values give it
// (DISPATCHER_SCHEDULES, a JSON list).
type scheduleValue struct {
	Name     string `json:"name"`
	Class    string `json:"class"` // the CronJob whose job template is run
	Cron     string `json:"cron"`
	TimeZone string `json:"timeZone"`
	// How late a start may be before it counts as not started, and how
	// long an attempt may run: Go durations (90s, 1h).
	StartWithin string `json:"startWithin"`
	Timeout     string `json:"timeout"`
	Priority    int    `json:"priority"`
	// After names a schedule (or, when none has the name, a class) whose
	// task, submitted before this one's, this schedule's task starts after
	// (ADR 0062); AfterWithin how long it waits at most, by default that
	// schedule's timeout, or an hour when there is none.
	After       string `json:"after"`
	AfterWithin string `json:"afterWithin"`
}

// parseSchedules reads the schedules the installation lists.
func parseSchedules(raw string) ([]dispatch.Schedule, error) {
	if raw == "" {
		return nil, nil
	}
	var vs []scheduleValue
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %w", err)
	}
	seen := map[string]bool{}
	byName := map[string]scheduleValue{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	var out []dispatch.Schedule
	for _, v := range vs {
		if seen[v.Name] {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %q is listed twice", v.Name)
		}
		seen[v.Name] = true
		s := dispatch.Schedule{Name: v.Name, Cron: v.Cron, TimeZone: v.TimeZone,
			Template: dispatch.TaskSpec{Class: v.Class, Priority: v.Priority}}
		var err error
		if s.Template.AttemptTimeout, err = parseDuration(v.Timeout, time.Hour); err != nil {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s: timeout: %w", v.Name, err)
		}
		if s.StartWithin, err = parseDuration(v.StartWithin, 0); err != nil {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s: startWithin: %w", v.Name, err)
		}
		if v.After != "" {
			s.Template.After = v.After
			other, ok := byName[v.After]
			if ok {
				s.Template.After = other.Class
			}
			def, err := parseDuration(other.Timeout, time.Hour)
			if err != nil {
				def = time.Hour // the other schedule's own error is told
			}
			if s.Template.AfterWithin, err = parseDuration(v.AfterWithin, def); err != nil {
				return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s: afterWithin: %w", v.Name, err)
			}
		} else if v.AfterWithin != "" {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s: afterWithin without after", v.Name)
		}
		if s.Cron == "" {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s has no cron expression", v.Name)
		}
		if err := s.Check(); err != nil {
			return nil, fmt.Errorf("DISPATCHER_SCHEDULES: %s: %w", v.Name, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

type scheduleStore interface {
	Declare(ctx context.Context, s dispatch.Schedule) error
	Undeclare(ctx context.Context, name string) error
	Schedules(ctx context.Context) ([]dispatch.Schedule, error)
}

// declareSchedules makes the declared schedules the ones listed: it
// declares each, and removes any other. A schedule whose class check
// refuses (its CronJob is missing, or not suspended, so that the cluster
// would start it too) is not declared, and removed if it was.
func declareSchedules(ctx context.Context, d scheduleStore, want []dispatch.Schedule, check func(context.Context, string) error, log *slog.Logger) error {
	keep := map[string]bool{}
	var errs []error
	for _, s := range want {
		if check != nil {
			if err := check(ctx, s.Template.Class); err != nil {
				log.Error("not scheduling", "schedule", s.Name, "reason", err)
				errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
				continue
			}
		}
		if err := d.Declare(ctx, s); err != nil {
			return err
		}
		keep[s.Name] = true
		log.Info("scheduled", "schedule", s.Name, "class", s.Template.Class, "cron", s.Cron, "time_zone", s.TimeZone, "after", s.Template.After)
	}
	have, err := d.Schedules(ctx)
	if err != nil {
		return err
	}
	for _, s := range have {
		if !keep[s.Name] {
			if err := d.Undeclare(ctx, s.Name); err != nil {
				return err
			}
			log.Info("schedule removed", "schedule", s.Name)
		}
	}
	return errors.Join(errs...)
}
