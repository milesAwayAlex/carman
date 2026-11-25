package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/memory"
)

func TestR2SchedulesFromValues(t *testing.T) {
	ss, err := parseSchedules(`[{"name":"usage","class":"agent-usage","cron":"7 * * * *","startWithin":"30m","timeout":"15m","priority":50},
		{"name":"weekly","class":"agent-x","cron":"0 15 * * 1","timeZone":"Europe/London"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].Template.AttemptTimeout != 15*time.Minute || ss[0].StartWithin != 30*time.Minute ||
		ss[0].Template.Priority != 50 || ss[1].TimeZone != "Europe/London" || ss[1].Template.AttemptTimeout != time.Hour {
		t.Errorf("read %+v", ss)
	}
	for _, bad := range []string{`[{"name":"a","class":"c"}]`, `[{"name":"a","class":"c","cron":"bad"}]`,
		`[{"name":"a","class":"c","cron":"* * * * *"},{"name":"a","class":"c","cron":"* * * * *"}]`, `{`} {
		if _, err := parseSchedules(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// The service declares what is listed, removes what is not, and does not
// schedule a class its check refuses.
func TestR2DeclareSchedulesReplacesTheList(t *testing.T) {
	ctx := context.Background()
	d := memory.New(dispatch.Config{Starter: nopStarter{}, MaxRunning: 1})
	defer d.Kill()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmpl := dispatch.TaskSpec{Class: "c", AttemptTimeout: time.Minute}
	old := dispatch.Schedule{Name: "old", Cron: "0 0 * * *", Template: tmpl}
	if err := d.Declare(ctx, old); err != nil {
		t.Fatal(err)
	}
	want := []dispatch.Schedule{{Name: "a", Cron: "0 1 * * *", Template: tmpl}, {Name: "b", Cron: "0 2 * * *", Template: dispatch.TaskSpec{Class: "unsuspended", AttemptTimeout: time.Minute}}}
	check := func(_ context.Context, class string) error {
		if class == "unsuspended" {
			return errors.New("not suspended")
		}
		return nil
	}
	if err := declareSchedules(ctx, d, want, check, log); err == nil {
		t.Error("a refused class was not reported")
	}
	have, _ := d.Schedules(ctx)
	if len(have) != 1 || have[0].Name != "a" {
		t.Errorf("declared %+v, want only a", have)
	}
}

type nopStarter struct{}

func (nopStarter) Start(context.Context, dispatch.Attempt) error { return nil }
