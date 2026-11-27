package main

import (
	"fmt"
	"regexp"
	"strings"
)

// The pilot's world: enough to show that the harness works (a task is
// handed over, the world is acted on, the score is the world's), with one
// lesson met by doing, one told, and a neutral task. Its tasks are not
// among the stream's.

var pilotStages = []string{"test", "staging", "prod"}
var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

const pilotHelp = `ops: the delivery systems.
  ops stages                              the stages there are
  ops status SERVICE                      what is deployed of a service
  ops change open SERVICE                 open a change for a service
  ops deploy SERVICE VERSION [options]    put a version of a service out
  ops attach NAME TEXT                    hand a text in with the ticket, under a name
  ops help                                this`

func pilotRules(w *World, argv []string) (int, string) {
	if len(argv) == 0 {
		return 2, pilotHelp
	}
	pos, named := flags(argv[1:])
	switch argv[0] {
	case "help":
		return 0, pilotHelp
	case "stages":
		return 0, strings.Join(pilotStages, "\n")
	case "status":
		if len(pos) != 1 {
			return 2, "status: say which service"
		}
		var lines []string
		for _, st := range pilotStages {
			if v := w.fact("deployed/" + pos[0] + "/" + st); v != "" {
				lines = append(lines, st+" "+v)
			}
		}
		if len(lines) == 0 {
			return 0, pos[0] + ": nothing deployed"
		}
		return 0, strings.Join(lines, "\n")
	case "change":
		if len(pos) != 2 || pos[0] != "open" {
			return 2, "change: say: ops change open SERVICE"
		}
		id := w.next("CH")
		w.set("change/"+id, "open "+pos[1])
		return 0, "opened " + id + " for " + pos[1]
	case "deploy":
		if len(pos) != 2 {
			return 2, "deploy: say: ops deploy SERVICE VERSION"
		}
		service, version := pos[0], pos[1]
		if !versionRe.MatchString(version) {
			return 1, "deploy: refused: a version is three numbers, like 1.2.3"
		}
		stage, ok := named["stage"]
		if !ok || stage == "" {
			return 1, "deploy: refused"
		}
		if !contains(pilotStages, stage) {
			return 1, "deploy: refused: no stage " + stage
		}
		// The rule nobody wrote down: a deploy names an open change for
		// the same service, and uses it up.
		change, ok := named["change"]
		if !ok || change == "" {
			return 1, "deploy: refused: no change"
		}
		switch w.fact("change/" + change) {
		case "":
			return 1, "deploy: refused: no change " + change
		case "open " + service:
		default:
			return 1, fmt.Sprintf("deploy: refused: %s is not an open change for %s", change, service)
		}
		w.set("change/"+change, "used "+service)
		w.set("deployed/"+service+"/"+stage, version)
		return 0, fmt.Sprintf("deployed %s %s to %s", service, version, stage)
	case "attach":
		if len(pos) < 2 {
			return 2, "attach: say: ops attach NAME TEXT"
		}
		w.state.Attached[w.state.Task+"/"+pos[0]] = strings.Join(pos[1:], " ")
		return 0, "attached " + pos[0]
	}
	return 2, "ops: no such command: " + argv[0] + "\n" + pilotHelp
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
