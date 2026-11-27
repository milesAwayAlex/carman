package main

import (
	"context"
	"strings"
	"testing"

	"carman/internal/dispatch"
)

// TestR2ReservePlaces: with three places and one kept from priority 60,
// work below it takes two, and a scheduled job still finds the third.
func TestR2ReservePlaces(t *testing.T) {
	r := reserveRule{Places: 1, From: 60, MaxRunning: 3}
	if err := r.check(); err != nil {
		t.Fatal(err)
	}
	build := dispatch.TaskSpec{Class: "agent-build", Priority: 50}
	night := dispatch.TaskSpec{Class: "dreamer", Priority: 60}
	one := []dispatch.TaskSpec{build}
	two := []dispatch.TaskSpec{build, build}
	if ok, _ := r.admit(context.Background(), build, one); !ok {
		t.Error("a second build is held with one running")
	}
	if ok, why := r.admit(context.Background(), build, two); ok || !strings.Contains(why, "priority 60") {
		t.Errorf("a third build is not held for the reserved place: %v %q", ok, why)
	}
	if ok, _ := r.admit(context.Background(), night, two); !ok {
		t.Error("the nightly job is held with a place kept for it")
	}
	if ok, _ := r.admit(context.Background(), build, []dispatch.TaskSpec{night, night}); !ok {
		t.Error("two nightly jobs running hold a build that would take the last place")
	}
	if ok, _ := (reserveRule{From: 60, MaxRunning: 3}).admit(context.Background(), build, two); !ok {
		t.Error("no places kept, and a build is held")
	}
	for _, bad := range []reserveRule{{Places: 1, From: 60, MaxRunning: 1}, {Places: -1, MaxRunning: 3}} {
		if bad.check() == nil {
			t.Errorf("%+v leaves no place for other work, and checks", bad)
		}
	}
	held := func(context.Context, dispatch.TaskSpec, []dispatch.TaskSpec) (bool, string) { return false, "second" }
	if ok, why := admitAll(r.admit, held)(context.Background(), build, two); ok || strings.Contains(why, "second") {
		t.Errorf("the first refusal is not the one given: %v %q", ok, why)
	}
	if admitAll(nil, nil) != nil {
		t.Error("no rules is not no admission")
	}
}
