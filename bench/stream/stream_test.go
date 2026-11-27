package main

import (
	"regexp"
	"strings"
	"testing"
)

// knowing works a task as an agent that knows every rule would.
type knowing struct {
	w *World
	t *testing.T
}

func (k knowing) do(words ...string) string {
	k.t.Helper()
	code, out := k.w.Do(words)
	if code != 0 {
		k.t.Fatalf("task %s: %v: %s", k.w.state.Task, words, out)
	}
	return out
}

func (k knowing) deploy(service, version, stage string) {
	day := k.w.state.Day
	if k.w.fact("image/"+service+"/"+version) == "" {
		push := []string{"registry", "push", service, version}
		if day >= 3 {
			push = append(push, "--registry", "r2")
		}
		k.do(push...)
	}
	if stage == "prod" && k.w.fact("deployed/"+service+"/staging") != version {
		k.deploy(service, version, "staging")
	}
	id := strings.Fields(k.do("change", "open", service))[1]
	option := "--change"
	if day >= 3 {
		option = "--cr"
	}
	call := []string{"deploy", service, version, "--stage", stage, option, id}
	if stage == "prod" {
		call = append(call, "--approved-by", k.do("owner", service))
	}
	k.do(call...)
}

var (
	deployRe  = regexp.MustCompile(`version ([0-9.]+) of the service (\w+) out to (?:the )?(\w+)`)
	settingRe = regexp.MustCompile(`[Ss]et the setting (\S+) to (\w+)\.`)
	raiseRe   = regexp.MustCompile(`setting (\S+\.rate)`)
	serviceRe = regexp.MustCompile(`Team (\w+) needs a new service (?:called (\S+)|for (?:its |)(\w+))\.`)
)

// work does what a ticket asks, knowing the rules and what the owner said.
func (k knowing) work(task Task) {
	ticket := task.Ticket
	changed := false
	switch {
	case deployRe.MatchString(ticket):
		m := deployRe.FindStringSubmatch(ticket)
		k.deploy(m[2], m[1], m[3])
		changed = true
	case settingRe.MatchString(ticket):
		m := settingRe.FindStringSubmatch(ticket)
		k.do("config", "set", m[1], m[2], "--reason", "asked for in a ticket")
		changed = true
	case strings.Contains(ticket, "as near to it") || strings.Contains(ticket, "The sale needs"):
		key := raiseRe.FindStringSubmatch(ticket)[1]
		if k.w.fact("config/"+key) != "200" {
			k.do("config", "set", key, "200", "--reason", "as high as a rate goes")
			changed = true
		}
		k.do("attach", "plan", "the rate is at its cap of 200; 400 is not allowed")
	case serviceRe.MatchString(ticket):
		m := serviceRe.FindStringSubmatch(ticket)
		name := m[2]
		if name == "" {
			name = map[string]string{"refunds": "refunds", "indexer": "indexer", "bounces": "bounces"}[m[3]]
		}
		k.do("service", "create", m[1]+"-"+name, "--team", m[1])
		changed = true
	case strings.Contains(ticket, "name stages"):
		k.do("attach", "stages", k.do("stages"))
	case strings.Contains(ticket, "name value"):
		key := regexp.MustCompile(`setting (\S+) now`).FindStringSubmatch(ticket)[1]
		k.do("attach", "value", k.do("config", "get", key))
	case strings.Contains(ticket, "name status"):
		k.do("attach", "status", "ledger ok")
	case strings.Contains(ticket, "name services"):
		k.do("attach", "services", k.do("service", "list"))
	case strings.Contains(ticket, "name deployed"):
		k.do("attach", "deployed", k.do("status", "mailer"))
	case strings.Contains(ticket, "name change"):
		k.do("attach", "change", strings.Fields(k.do("change", "open", "search"))[1])
	case strings.Contains(ticket, "name count"):
		k.do("attach", "count", "3")
	case strings.Contains(ticket, `name "about"`):
		k.do("attach", "about", "The ledger keeps the accounts.\nPayments write to it.")
	case strings.Contains(ticket, "release note"):
		k.do("attach", "release-note", "A release.\nIt has changes.\nReviewed-by: platform")
	default:
		k.t.Fatalf("task %s: no way to work %q", task.ID, ticket)
	}
	// The standing instruction of task s07: a summary of what was changed.
	if changed && task.ID >= "s07" {
		k.do("attach", "summary", "did what the ticket asked")
	}
}

// TestM7StreamCanBeWorked: an agent that knows every rule of the stream's
// world and what the owner said passes all forty tasks and is refused
// nothing; every rule the tasks say they turn on is one the world has; and
// the tasks are forty over five days of eight.
func TestM7StreamCanBeWorked(t *testing.T) {
	var tasks []Task
	if err := readJSON("stream.json", &tasks); err != nil {
		t.Fatal(err)
	}
	w := newWorld(streamRules)
	k := knowing{w, t}
	perDay := map[int]int{}
	for _, task := range tasks {
		perDay[task.Day]++
		w.Begin(task.ID, task.Day)
		k.work(task)
	}
	if len(tasks) != 40 || len(perDay) != 5 || perDay[1] != 8 || perDay[5] != 8 {
		t.Errorf("the stream is %d tasks over %v", len(tasks), perDay)
	}
	results, err := Score(tasks, w.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Pass || r.WrongCalls != 0 || len(r.Refused) != 0 {
			t.Errorf("knowing every rule, task %s: %+v", r.Task, r)
		}
	}
}

// TestM7StreamRulesAreMet: an agent that knows nothing runs into each rule
// under its own name, is told enough to go on, and the old ways are told
// apart from the ones never known.
func TestM7StreamRulesAreMet(t *testing.T) {
	w := newWorld(streamRules)
	refused := func(day int, want string, words ...string) {
		t.Helper()
		w.state.Day = day
		w.Do(words)
		last := w.state.Calls[len(w.state.Calls)-1]
		if last.Code == 0 || last.Rule != want {
			t.Errorf("day %d, %v: ended %d under rule %q, want a refusal under %q: %s", day, words, last.Code, last.Rule, want, last.Out)
		}
	}
	ok := func(day int, words ...string) string {
		t.Helper()
		w.state.Day = day
		code, out := w.Do(words)
		if code != 0 {
			t.Fatalf("day %d, %v: %s", day, words, out)
		}
		return out
	}
	refused(1, "", "deploy", "ledger", "1.0.0")
	refused(1, "change", "deploy", "ledger", "1.0.0", "--stage", "test")
	id := strings.Fields(ok(1, "change", "open", "ledger"))[1]
	refused(1, "image", "deploy", "ledger", "1.0.0", "--stage", "test", "--change", id)
	ok(1, "registry", "push", "ledger", "1.0.0")
	refused(1, "staged", "deploy", "ledger", "1.0.0", "--stage", "prod", "--change", id)
	ok(1, "deploy", "ledger", "1.0.0", "--stage", "staging", "--change", id)
	id = strings.Fields(ok(1, "change", "open", "ledger"))[1]
	refused(1, "approval", "deploy", "ledger", "1.0.0", "--stage", "prod", "--change", id)
	refused(1, "force", "deploy", "ledger", "1.0.0", "--stage", "prod", "--change", id, "--force")
	ok(1, "deploy", "ledger", "1.0.0", "--stage", "prod", "--change", id, "--approved-by", "alice")
	refused(1, "reason", "config", "set", "ledger.timeout", "30")
	refused(1, "cap", "config", "set", "ledger.rate", "500", "--reason", "x")
	refused(1, "name", "service", "create", "refunds", "--team", "payments")
	// Day 3: the option is renamed and the registry has moved.
	id = strings.Fields(ok(3, "change", "open", "ledger"))[1]
	refused(3, "cr-old", "deploy", "ledger", "1.0.0", "--stage", "test", "--change", id)
	refused(3, "cr", "deploy", "ledger", "1.0.0", "--stage", "test")
	refused(3, "registry", "registry", "push", "ledger", "1.1.0")
	ok(3, "registry", "push", "ledger", "1.1.0", "--registry", "r2")
	ok(3, "deploy", "ledger", "1.1.0", "--stage", "staging", "--cr", id)
	// Day 4: the ledger's owner is another; a wrong note names bob for the mailer.
	id = strings.Fields(ok(4, "change", "open", "ledger"))[1]
	refused(4, "approval-old", "deploy", "ledger", "1.1.0", "--stage", "prod", "--cr", id, "--approved-by", "alice")
	ok(4, "deploy", "ledger", "1.1.0", "--stage", "prod", "--cr", id, "--approved-by", "dana")
	ok(4, "registry", "push", "mailer", "3.2.0", "--registry", "r2")
	id = strings.Fields(ok(4, "change", "open", "mailer"))[1]
	ok(4, "deploy", "mailer", "3.2.0", "--stage", "staging", "--cr", id)
	id = strings.Fields(ok(4, "change", "open", "mailer"))[1]
	refused(4, "approval-wrong", "deploy", "mailer", "3.2.0", "--stage", "prod", "--cr", id, "--approved-by", "bob")
	for _, c := range w.state.Calls {
		if c.Rule != "" && !strings.Contains(c.Out, "refused") {
			t.Errorf("a rule's refusal does not say it is one: %v: %s", c.Argv, c.Out)
		}
	}
}
