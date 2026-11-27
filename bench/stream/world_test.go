package main

import (
	"strings"
	"testing"
)

// TestM7PilotWorld: the pilot's world refuses a deploy until its rule is
// met, answers as a system would, and its record of the calls and its
// state are what a task is scored on.
func TestM7PilotWorld(t *testing.T) {
	var tasks []Task
	if err := readJSON("pilot.json", &tasks); err != nil {
		t.Fatal(err)
	}
	w := newWorld(pilotRules)
	do := func(line string) (int, string) { return w.Do(strings.Fields(line)) }
	w.state.Task, w.state.Day = "p1", 1
	for _, wrong := range []string{"deploy ledger 1.4.2", "deploy ledger v1.4.2 --stage test", "deploy ledger 1.4.2 --stage test",
		"deploy ledger 1.4.2 --stage test --change CH-9", "deploy ledger 1.4.2 --stage nowhere --change CH-1"} {
		if code, out := do(wrong); code == 0 {
			t.Errorf("%q was accepted: %s", wrong, out)
		}
	}
	if code, _ := do("help"); code != 0 {
		t.Error("help fails")
	}
	_, out := do("change open ledger")
	id := strings.Fields(out)[1]
	if code, out := do("deploy mailer 2.0.1 --stage test --change " + id); code == 0 || !strings.Contains(out, "not an open change for mailer") {
		t.Errorf("a change for another service: %d %s", code, out)
	}
	if code, out := do("deploy ledger 1.4.2 --stage test --change " + id); code != 0 {
		t.Fatalf("the right deploy: %s", out)
	}
	if code, _ := do("deploy ledger 1.4.3 --stage test --change " + id); code == 0 {
		t.Error("a change was used twice")
	}
	w.state.Task = "p2"
	w.Do([]string{"attach", "release-note", "ledger 1.4.2: fixes.\nReviewed-by: platform"})
	w.state.Task = "p6"
	w.Do([]string{"attach", "release-note", "search 0.9.0: new."})

	got, err := Score(tasks, w.state)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Result{}
	for _, r := range got {
		by[r.Task] = r
	}
	if r := by["p1"]; !r.Pass || r.WrongCalls != 7 || r.Calls != 10 {
		t.Errorf("p1: %+v", r)
	}
	if !by["p2"].Pass || by["p6"].Pass || by["p3"].Pass || by["p4"].Pass {
		t.Errorf("results: %+v", got)
	}
	if len(got) != 6 || got[0].Task != "p1" || got[5].Family != "notes-end-with-reviewed-by" {
		t.Errorf("order and families: %+v", got)
	}
}
