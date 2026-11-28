package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/budget"
)

// Two accounts as they were read over a day: "two" worked through a
// five-hour window late in its week, "one" through one at the start of
// its week, then the proxy back on "two".
type planLog struct {
	seq      int64
	readings []dispatch.Record
	changes  []dispatch.Record
}

func (l *planLog) change(at time.Time, name string) {
	l.seq++
	l.changes = append(l.changes, dispatch.Record{Seq: l.seq, Event: dispatch.Event{Type: TypeModelAccount, Subject: name, Time: at}})
}

func (l *planLog) read(at time.Time, model string, five float64, fiveR time.Time, wk float64, weekR time.Time, own float64) {
	l.seq++
	ws := map[string]win{"five_hour": {five, fiveR}, "seven_day": {wk, weekR}}
	if own >= 0 {
		ws["seven_day_strong"] = win{own, weekR}
	}
	l.readings = append(l.readings, rec(l.seq, at, model, ws))
}

var (
	planDay  = time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC) // a Saturday
	oneWeek  = planDay.Add(6*24*time.Hour + 18*time.Hour)  // Friday 18:00
	twoWeek  = planDay.Add(24*time.Hour + 18*time.Hour)    // Sunday 18:00
	planTime = planDay.Add(5*time.Hour + 30*time.Minute)
)

func twoAccounts() *planLog {
	l := &planLog{}
	at := func(h, m int) time.Time {
		return planDay.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	l.change(at(-17, 0), "two")
	r2 := at(2, 0)
	l.read(at(-3, 0), "strong", 0, r2, 0.25, twoWeek, 0.42)
	l.read(at(-2, 0), "strong", 0.30, r2, 0.288, twoWeek, 0.483)
	l.read(at(-1, 0), "cheap", 0.60, r2, 0.326, twoWeek, -1)
	l.read(at(0, 40), "strong", 0.95, r2, 0.37, twoWeek, 0.62)
	// The proxy is switched; its first answers are read before the change
	// is recorded.
	r1 := at(5, 50)
	l.read(at(0, 51), "cheap", 0.01, r1, 0, oneWeek, -1)
	l.change(at(0, 52), "one")
	l.read(at(0, 53), "strong", 0.01, r1, 0, oneWeek, 0)
	l.read(at(2, 0), "strong", 0.46, r1, 0.07, oneWeek, 0.11)
	l.read(at(4, 20), "strong", 0.77, r1, 0.12, oneWeek, 0.19)
	l.read(at(5, 20), "strong", 0.90, r1, 0.14, oneWeek, 0.22)
	l.change(at(5, 25), "two")
	l.read(at(5, 26), "strong", 0, at(10, 20), 0.38, twoWeek, 0.63)
	return l
}

func near(t *testing.T, what string, got, want, by float64) {
	t.Helper()
	if math.Abs(got-want) > by {
		t.Errorf("%s: %.4f, want %.4f (within %.4f)", what, got, want, by)
	}
}

func account(t *testing.T, p budgetPlan, name string) accountPlan {
	t.Helper()
	for _, a := range p.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no account %s in the plan: %+v", name, p.Accounts)
	return accountPlan{}
}

// The plan knows each account's standing, the one not held too, and what
// a full five-hour window costs of each week, from the readings.
func TestPlanStandingOfEachAccount(t *testing.T) {
	l := twoAccounts()
	p := planOf(l.readings, l.changes, planTime, defaultPlan())
	if p.Held != "two" || len(p.Accounts) != 2 {
		t.Fatalf("held %q, %d accounts", p.Held, len(p.Accounts))
	}
	one, two := account(t, p, "one"), account(t, p, "two")
	near(t, "one's week", one.Week.Used, 0.14, 0.001)
	near(t, "one's own week", one.Own["strong"].Used, 0.22, 0.001)
	if !one.Week.Reset.Equal(oneWeek) || one.Held || !two.Held {
		t.Errorf("one: %+v", one)
	}
	// Its five-hour window was at 90% and has not reset yet at 05:30.
	near(t, "one's five-hour", one.Five.Used, 0.90, 0.001)
	near(t, "two's week", two.Week.Used, 0.38, 0.001)
	// The reading taken between the switch and its record is one's: two's
	// week did not fall to nothing.
	near(t, "one's cost", one.Cost, 0.14/0.89, 0.005)
	near(t, "one's cost of the model's own", one.CostOwn, 0.22/0.89, 0.005)
	near(t, "two's cost", two.Cost, 0.12/0.95, 0.005)
	near(t, "two's cost of the model's own", two.CostOwn, 0.20/0.95, 0.01)
	// Rules 1 to 3.
	near(t, "two's room", two.Room, 1-0.38-0.25*36.5/168, 0.002)
	near(t, "two's windows", two.Windows, two.Room/two.Cost, 0.001)
	near(t, "two's urgency", two.Urgency, two.Windows/36.5, 0.001)
	near(t, "one's urgency", one.Urgency, (1-0.14-0.25*156.5/168)/one.Cost/156.5, 0.001)
	if two.OwnModel != "strong" || two.WindowsO >= two.Windows {
		t.Errorf("two's own model: %q, %.2f of %.2f windows", two.OwnModel, two.WindowsO, two.Windows)
	}
	// After 05:50 one's five-hour window is empty, though nothing read it.
	p = planOf(l.readings, l.changes, planTime.Add(time.Hour), defaultPlan())
	if one = account(t, p, "one"); one.Five.Used != 0 || !one.Five.Rolled {
		t.Errorf("one's five-hour window after its reset: %+v", one.Five)
	}
}

// Rule 3: the account whose week ends first with the most room is held,
// and what leaving it would lose is said.
func TestPlanHoldsTheAccountWhoseRoomExpiresFirst(t *testing.T) {
	l := twoAccounts()
	p := planOf(l.readings, l.changes, planTime, defaultPlan())
	if p.Advice.Account != "two" || p.Advice.Switch || p.Advice.Rule != 3 {
		t.Fatalf("advice: %+v", p.Advice)
	}
	var lostTwo lost
	for _, x := range p.IfElsewhere {
		if x.Account == "two" {
			lostTwo = x
		}
	}
	near(t, "what two loses if not held", lostTwo.Week, 0.62-0.25*36.5/168, 0.002)
	// Had the proxy stayed on one, the plan says to switch.
	l.changes = l.changes[:2]
	l.readings = l.readings[:len(l.readings)-1]
	p = planOf(l.readings, l.changes, planTime.Add(time.Hour), defaultPlan())
	if p.Held != "one" || p.Advice.Account != "two" || !p.Advice.Switch {
		t.Fatalf("held one, advice: %+v", p.Advice)
	}
	out := strings.Join(planLines(p), "\n")
	for _, want := range []string{"switch to account two, by rule 3", "if the proxy does not hold account two until Sun 03-08 18:00", "account one (held by the proxy"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
}

// Rule 4, and rule 3's margin and least time between switches.
func TestPlanSwitchesAtAFullFiveHourWindowAndNotForLittle(t *testing.T) {
	c := defaultPlan()
	now := planTime
	mk := func(name string, five, urgency float64) accountPlan {
		return accountPlan{Name: name, Known: true, Five: winAt{Used: five}, Week: winAt{Reset: now.Add(48 * time.Hour)}, Urgency: urgency, Windows: 1, Admits: true, Goes: true}
	}
	if a := choose([]accountPlan{mk("x", 0.86, 0.1), mk("y", 0.2, 0.01)}, "x", time.Hour, c); a.Account != "y" || a.Rule != 4 || !a.Switch {
		t.Errorf("a full five-hour window: %+v", a)
	}
	if a := choose([]accountPlan{mk("x", 0.86, 0.1), mk("y", 0.9, 0.01)}, "x", time.Hour, c); a.Account != "x" || a.Rule != 4 || a.Switch {
		t.Errorf("both full: %+v", a)
	}
	if a := choose([]accountPlan{mk("x", 0.1, 0.10), mk("y", 0.1, 0.11)}, "x", time.Hour, c); a.Account != "x" || a.Switch {
		t.Errorf("a tenth more urgent: %+v", a)
	}
	if a := choose([]accountPlan{mk("x", 0.1, 0.10), mk("y", 0.1, 0.13)}, "x", time.Hour, c); a.Account != "y" || !a.Switch {
		t.Errorf("a third more urgent: %+v", a)
	}
	if a := choose([]accountPlan{mk("x", 0.1, 0.10), mk("y", 0.1, 0.13)}, "x", 10*time.Minute, c); a.Account != "x" || a.Switch {
		t.Errorf("switched ten minutes ago: %+v", a)
	}
}

// The days ahead by the rules: the week that ends first is spent before
// it ends, nothing is left at a reset, and no week is spent ahead of its
// line.
func TestPlanSpendsEachWeekBeforeItEnds(t *testing.T) {
	l := twoAccounts()
	c := defaultPlan()
	p := planOf(l.readings, l.changes, planTime, c)
	if len(p.Blocks) < 7 {
		t.Fatalf("%d blocks", len(p.Blocks))
	}
	for _, x := range p.Lost {
		if x.Week > 0.03 {
			t.Errorf("the plan leaves %s of %s's week at %s", pct(x.Week), x.Account, x.At)
		}
	}
	// Until two's reset the proxy is mostly on two, and two's room is worked.
	var held, worked float64
	for _, b := range p.Blocks {
		if !b.From.Before(twoWeek) {
			break
		}
		held += b.Held["two"]
		worked += b.Windows["two"]
	}
	two := account(t, p, "two")
	if held < 28 {
		t.Errorf("two is held %.0f of the 36 hours to its reset", held)
	}
	near(t, "windows worked on two before its reset", worked, (1-0.38)/two.Cost, 0.4)
	// One's week, a week long, is not spent in its first days.
	var early float64
	for _, b := range p.Blocks[:3] {
		early += b.Windows["one"]
	}
	if one := account(t, p, "one"); early > one.Windows*0.6 {
		t.Errorf("one: %.1f of its %.1f windows in the first three days", early, one.Windows)
	}
	out := strings.Join(planLines(p), "\n")
	if !strings.Contains(out, "the next 7 days by the rules") {
		t.Errorf("the plan's lines:\n%s", out)
	}
	t.Log("\n" + out)
}

// The dispatcher records the advice once, and again when it changes.
func TestPlanIsRecordedWhenItsAdviceChanges(t *testing.T) {
	l := twoAccounts()
	l.changes = l.changes[:2]
	l.readings = l.readings[:len(l.readings)-1]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan dispatch.Event, 8)
	held := make(chan struct{})
	readings := func(context.Context) ([]dispatch.Record, error) { return l.readings, nil }
	changes := func(context.Context) ([]dispatch.Record, error) {
		select {
		case <-held:
			return twoAccounts().changes, nil
		default:
			return l.changes, nil
		}
	}
	go watchPlan(ctx, readings, changes, func(_ context.Context, e dispatch.Event) error { got <- e; return nil },
		defaultPlan(), func() time.Time { return planTime.Add(time.Hour) }, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := <-got
	if e.Type != TypeBudgetPlan || e.Subject != "two" || !strings.Contains(string(e.Data), `"switch":true`) {
		t.Fatalf("recorded: %s %s %s", e.Type, e.Subject, e.Data)
	}
	select {
	case e := <-got:
		t.Fatalf("the same advice was recorded twice: %s", e.Data)
	case <-time.After(50 * time.Millisecond):
	}
	close(held) // the account is changed to two
	e = <-got
	if e.Subject != "two" || !strings.Contains(string(e.Data), `"switch":false`) {
		t.Fatalf("after the switch: %s %s", e.Subject, e.Data)
	}
}

// The room is divided between the owner's priorities by their parts, on
// each account and on the model whose own week is tightest.
func TestPlanDividesTheRoomBetweenPriorities(t *testing.T) {
	l := twoAccounts()
	p := planOf(l.readings, l.changes, planTime, defaultPlan())
	out := strings.Join(planLines(p), "\n")
	if !strings.Contains(out, "no priorities are set") {
		t.Errorf("with no priorities:\n%s", out)
	}
	hard := budget.Item{ID: "platform-1", Title: "the planner", Hard: true, State: budget.Started}
	p.divide(budget.View{Streams: []budget.Stream{
		{Priority: budget.Priority{Name: "platform", Share: 3}, Part: 0.75, Items: []budget.Item{hard, {ID: "platform-2", Title: "next", Model: "cheap"}}},
		{Priority: budget.Priority{Name: "errands", Share: 1}, Part: 0.25},
	}})
	one, two := account(t, p, "one"), account(t, p, "two")
	if len(p.Shares) != 2 {
		t.Fatalf("shares: %+v", p.Shares)
	}
	near(t, "platform on two", p.Shares[0].Windows["two"], 0.75*two.Windows, 0.001)
	near(t, "platform on two's own model", p.Shares[0].WindowsO["two"], 0.75*two.WindowsO, 0.001)
	near(t, "errands on one", p.Shares[1].Windows["one"], 0.25*one.Windows, 0.001)
	out = strings.Join(planLines(p), "\n")
	for _, want := range []string{"platform, 75%:", "errands, 25%:", "next: platform-1: the planner (hard: the strongest model) [started]", "next: platform-2: next (on cheap)", "2 waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
}

// A reading that does not say its account (the reading job's) is of the
// account whose week it shows, by the proxy's own readings, though the
// log believes another was held: a switch that was not recorded does not
// put one account's figures under the other's name.
func TestPlanReadingsOfTheJobAreOfTheAccountTheirWeekShows(t *testing.T) {
	l := &planLog{}
	at := func(h, m int) time.Time {
		return planDay.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	labelled := func(when time.Time, account string, five, wk float64, weekR time.Time) {
		l.read(when, "cheap", five, when.Add(3*time.Hour), wk, weekR, -1)
		r := &l.readings[len(l.readings)-1]
		var m map[string]json.RawMessage
		_ = json.Unmarshal(r.Data, &m)
		m["account"], _ = json.Marshal(account)
		r.Data, _ = json.Marshal(m)
	}
	l.change(at(6, 31), "one") // recorded
	labelled(at(6, 35), "one", 0.05, 0.14, oneWeek)
	labelled(at(6, 50), "two", 0.40, 0.42, twoWeek) // the proxy went to two; that was not recorded
	// The job's reading at 07:07, on two, under the log's belief of one.
	l.read(at(7, 7), "cheap", 0.47, at(10, 0), 0.44, twoWeek, -1)
	p := planOf(l.readings, l.changes, at(7, 10), defaultPlan())
	one, two := account(t, p, "one"), account(t, p, "two")
	near(t, "one's week", one.Week.Used, 0.14, 0.001)
	near(t, "two's week", two.Week.Used, 0.44, 0.001)
	if !one.Week.Reset.Equal(oneWeek) || !two.Week.Reset.Equal(twoWeek) {
		t.Errorf("weeks: one %s, two %s", one.Week.Reset, two.Week.Reset)
	}
	// At the door the same reading is given its account.
	job := l.readings[len(l.readings)-1].Data
	if got := readingAccount(stampAccount(l.readings[:len(l.readings)-1], job)); got != "two" {
		t.Errorf("the job's reading is stamped %q", got)
	}
	// One that shows a week no account's readings show is left as it is,
	// and one that says its account is not touched.
	odd := rec(99, at(7, 8), "cheap", map[string]win{"seven_day": {0.3, twoWeek.Add(50 * time.Hour)}}).Data
	if got := readingAccount(stampAccount(l.readings, odd)); got != "" {
		t.Errorf("a reading of an unknown week is stamped %q", got)
	}
	if got := readingAccount(stampAccount(l.readings, l.readings[0].Data)); got != "one" {
		t.Errorf("a reading that says one is stamped %q", got)
	}
}
