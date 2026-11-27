package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The stream's world: a delivery team's systems over five days. Its rules
// are written down nowhere an agent can read; each is learned by running
// into it, and a call it refuses is recorded under the rule's name, which
// is how the benchmark knows whether a lesson was known.
//
// The rules, by name:
//
//	change     a deploy names an open change for the same service (--change ID)
//	cr         from day 3 the option is --cr; --change is refused (cr-old)
//	image      a deploy needs the version pushed to the registry first
//	registry   from day 3 a push names the new registry (--registry r2)
//	staged     a deploy to prod needs the same version on staging
//	approval   a deploy to prod is approved by the service's owner, who for
//	           ledger changes on day 4 (approval-old: the owner before;
//	           approval-wrong: the owner a wrong note names)
//	reason     a config change gives a reason (--reason)
//	cap        a rate is at most 200
//	name       a service is named TEAM-SOMETHING
//	force      there is no --force (a wrong note says there is)
//
// Usage errors (a missing word, an unknown stage) are answered plainly and
// are no rule's.

var streamStages = []string{"test", "staging", "prod"}

const streamHelp = `ops: the delivery systems.
  ops stages                               the stages there are
  ops status SERVICE                       what is deployed of a service
  ops owner SERVICE                        who owns a service
  ops service list                         the services there are
  ops service create NAME [options]        make a service
  ops change open SERVICE                  open a change for a service
  ops change list                          the changes that are open
  ops registry tags SERVICE                the versions the registry has
  ops registry push SERVICE VERSION [options]   put a version in the registry
  ops deploy SERVICE VERSION [options]     put a version of a service out
  ops config get KEY                       a setting
  ops config set KEY VALUE [options]       change a setting
  ops attach NAME TEXT                     hand a text in with the ticket, under a name
  ops help                                 this`

// streamInit is the world as the first day finds it.
func streamInit(w *World) {
	if w.fact("world") != "" {
		return
	}
	w.set("world", "stream")
	for name, team := range map[string]string{"ledger": "payments", "mailer": "mailer", "search": "search"} {
		w.set("service/"+name, team)
	}
	for key, value := range map[string]string{
		"ledger.rate": "100", "ledger.timeout": "10", "mailer.retries": "3", "mailer.timeout": "30",
		"mailer.rate": "80", "search.replicas": "2", "search.timeout": "15",
	} {
		w.set("config/"+key, value)
	}
}

// streamOwner is who owns a service on a day.
func streamOwner(service string, day int) string {
	switch service {
	case "ledger":
		if day >= 4 {
			return "dana"
		}
		return "alice"
	case "mailer":
		return "carol"
	case "search":
		return "erin"
	}
	return "frank"
}

func streamRules(w *World, argv []string) (int, string, string) {
	streamInit(w)
	if len(argv) == 0 {
		return 2, streamHelp, ""
	}
	day := w.state.Day
	pos, named := flags(argv[1:])
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch argv[0] {
	case "help":
		return 0, streamHelp, ""
	case "stages":
		return 0, strings.Join(streamStages, "\n"), ""
	case "status":
		if len(pos) != 1 {
			return 2, "status: say which service", ""
		}
		var lines []string
		for _, st := range streamStages {
			if v := w.fact("deployed/" + pos[0] + "/" + st); v != "" {
				lines = append(lines, st+" "+v)
			}
		}
		if len(lines) == 0 {
			return 0, pos[0] + ": nothing deployed", ""
		}
		return 0, strings.Join(lines, "\n"), ""
	case "owner":
		if len(pos) != 1 {
			return 2, "owner: say which service", ""
		}
		if w.fact("service/"+pos[0]) == "" {
			return 1, "owner: no service " + pos[0], ""
		}
		return 0, streamOwner(pos[0], day), ""
	case "service":
		switch sub {
		case "list":
			var lines []string
			for _, k := range w.keys("service/") {
				lines = append(lines, fmt.Sprintf("%s (team %s)", strings.TrimPrefix(k, "service/"), w.fact(k)))
			}
			return 0, strings.Join(lines, "\n"), ""
		case "create":
			if len(pos) != 2 {
				return 2, "service: say: ops service create NAME --team TEAM", ""
			}
			team := named["team"]
			if team == "" {
				return 2, "service: say which team: --team TEAM", ""
			}
			if !strings.HasPrefix(pos[1], team+"-") || len(pos[1]) <= len(team)+1 {
				return 1, "service: refused: a new service of team " + team + " is called " + team + "-SOMETHING", "name"
			}
			if w.fact("service/"+pos[1]) != "" {
				return 1, "service: there is a service " + pos[1] + " already", ""
			}
			w.set("service/"+pos[1], team)
			return 0, "made " + pos[1], ""
		}
		return 2, "service: say: ops service list, or ops service create NAME --team TEAM", ""
	case "change":
		switch sub {
		case "open":
			if len(pos) != 2 {
				return 2, "change: say: ops change open SERVICE", ""
			}
			id := w.next("CH")
			w.set("change/"+id, "open "+pos[1])
			return 0, "opened " + id + " for " + pos[1], ""
		case "list":
			var lines []string
			for _, k := range w.keys("change/") {
				if state, service, _ := strings.Cut(w.fact(k), " "); state == "open" {
					lines = append(lines, strings.TrimPrefix(k, "change/")+" "+service)
				}
			}
			if len(lines) == 0 {
				return 0, "no change is open", ""
			}
			return 0, strings.Join(lines, "\n"), ""
		}
		return 2, "change: say: ops change open SERVICE, or ops change list", ""
	case "registry":
		switch sub {
		case "tags":
			if len(pos) != 2 {
				return 2, "registry: say: ops registry tags SERVICE", ""
			}
			var lines []string
			for _, k := range w.keys("image/" + pos[1] + "/") {
				lines = append(lines, strings.TrimPrefix(k, "image/"+pos[1]+"/"))
			}
			if len(lines) == 0 {
				return 0, "no version of " + pos[1], ""
			}
			return 0, strings.Join(lines, "\n"), ""
		case "push":
			if len(pos) != 3 {
				return 2, "registry: say: ops registry push SERVICE VERSION", ""
			}
			if !versionRe.MatchString(pos[2]) {
				return 1, "registry: a version is three numbers, like 1.2.3", ""
			}
			reg := named["registry"]
			if day >= 3 {
				// The registry moved: the old one takes nothing more.
				if reg != "r2" {
					return 1, "registry: refused: registry r1 takes no more pushes; the registry is r2 (--registry r2)", "registry"
				}
			} else if reg != "" && reg != "r1" {
				return 1, "registry: no registry " + reg, ""
			}
			w.set("image/"+pos[1]+"/"+pos[2], "pushed")
			return 0, "pushed " + pos[1] + ":" + pos[2], ""
		}
		return 2, "registry: say: ops registry tags SERVICE, or ops registry push SERVICE VERSION", ""
	case "deploy":
		if len(pos) != 2 {
			return 2, "deploy: say: ops deploy SERVICE VERSION --stage STAGE", ""
		}
		service, version := pos[0], pos[1]
		if w.fact("service/"+service) == "" {
			return 1, "deploy: no service " + service, ""
		}
		if !versionRe.MatchString(version) {
			return 1, "deploy: a version is three numbers, like 1.2.3", ""
		}
		stage := named["stage"]
		if !contains(streamStages, stage) {
			return 2, "deploy: say which stage: --stage test, staging or prod", ""
		}
		if _, ok := named["force"]; ok {
			return 1, "deploy: refused: there is no option --force", "force"
		}
		// A deploy names an open change for the same service.
		change, rule := named["change"], "change"
		if day >= 3 {
			rule = "cr"
			if _, old := named["change"]; old {
				return 1, "deploy: refused: --change is no longer taken; a change is named with --cr", "cr-old"
			}
			change = named["cr"]
		}
		if change == "" {
			option := "--change"
			if day >= 3 {
				option = "--cr"
			}
			return 1, "deploy: refused: a deploy names an open change for the service (" + option + " ID)", rule
		}
		if w.fact("change/"+change) != "open "+service {
			return 1, fmt.Sprintf("deploy: refused: %s is not an open change for %s", change, service), rule
		}
		if w.fact("image/"+service+"/"+version) == "" {
			return 1, fmt.Sprintf("deploy: refused: the registry has no %s:%s", service, version), "image"
		}
		if stage == "prod" {
			if w.fact("deployed/"+service+"/staging") != version {
				return 1, fmt.Sprintf("deploy: refused: %s %s is not on staging", service, version), "staged"
			}
			approver, owner := named["approved-by"], streamOwner(service, day)
			if approver != owner {
				rule := "approval"
				switch {
				case service == "ledger" && approver == "alice" && day >= 4:
					rule = "approval-old"
				case service == "mailer" && approver == "bob":
					rule = "approval-wrong"
				}
				return 1, "deploy: refused: a deploy to prod is approved by the service's owner (--approved-by NAME)", rule
			}
		}
		w.set("change/"+change, "used "+service)
		w.set("deployed/"+service+"/"+stage, version)
		return 0, fmt.Sprintf("deployed %s %s to %s", service, version, stage), ""
	case "config":
		switch sub {
		case "get":
			if len(pos) != 2 {
				return 2, "config: say: ops config get KEY", ""
			}
			v := w.fact("config/" + pos[1])
			if v == "" {
				return 1, "config: no setting " + pos[1], ""
			}
			return 0, v, ""
		case "set":
			if len(pos) != 3 {
				return 2, "config: say: ops config set KEY VALUE", ""
			}
			if named["reason"] == "" {
				return 1, "config: refused: a change of a setting gives its reason (--reason TEXT)", "reason"
			}
			if strings.HasSuffix(pos[1], ".rate") {
				n, err := strconv.Atoi(pos[2])
				if err != nil {
					return 1, "config: a rate is a whole number", ""
				}
				if n > 200 {
					return 1, "config: refused: a rate is at most 200", "cap"
				}
			}
			w.set("config/"+pos[1], pos[2])
			return 0, "set " + pos[1] + " to " + pos[2], ""
		}
		return 2, "config: say: ops config get KEY, or ops config set KEY VALUE", ""
	case "attach":
		// The text is taken whole, whatever it holds.
		if len(argv) < 3 {
			return 2, "attach: say: ops attach NAME TEXT", ""
		}
		w.state.Attached[w.state.Task+"/"+argv[1]] = strings.Join(argv[2:], " ")
		return 0, "attached " + argv[1], ""
	}
	return 2, "ops: no such command: " + argv[0] + "\n" + streamHelp, ""
}

// keys are the facts whose names start with a prefix, in order.
func (w *World) keys(prefix string) []string {
	var out []string
	for k := range w.state.Facts {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
