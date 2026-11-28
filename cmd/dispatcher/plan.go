package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/budget"
)

// The plan of the model's budget (ADR new-the-plan-of-the-models-budget):
// with several accounts, each with a five-hour window, a seven-day one and
// a model's own weekly one, which account the proxy should hold now and
// over the days ahead so that no week ends with room unused. No model
// decides: it is arithmetic on the readings in the log. This part says and
// records; it switches nothing.

// TypeBudgetPlan is recorded when the plan's advice changes: the account
// to hold, the rule that says so, and the standings it was computed from.
const TypeBudgetPlan = "budget.plan"

const week = 7 * 24 * time.Hour

// planConfig is what the owner may change.
type planConfig struct {
	Reserve  float64       // of each weekly window, kept for the owner's own sessions
	Ceiling  float64       // of a five-hour window: the rest is the owner's
	Release  time.Duration // before a weekly reset the reserve is nothing
	Margin   float64       // how much more urgent the other account must be for a switch
	Hold     time.Duration // the least time between switches by urgency
	Soon     time.Duration // before a weekly reset with room, everything is admitted
	Band     float64       // how far ahead of its line the held account is worked
	Cost     float64       // of the seven-day window for one full five-hour window, when not measured
	CostOwn  float64       // of a model's own weekly window, when not measured
	Days     int           // how far the plan looks
	MinCost5 float64       // how much of five-hour windows a cost must be measured over
}

func defaultPlan() planConfig {
	return planConfig{Reserve: 0.25, Ceiling: 0.85, Release: 3 * time.Hour, Margin: 0.2, Hold: 30 * time.Minute,
		Soon: 24 * time.Hour, Band: 0.05, Cost: 0.14, CostOwn: 0.23, Days: 7, MinCost5: 0.5}
}

// planSettings reads DISPATCHER_PLAN_*.
func planSettings() (planConfig, error) {
	c := defaultPlan()
	for _, f := range []struct {
		env string
		v   *float64
	}{{"DISPATCHER_PLAN_RESERVE", &c.Reserve}, {"DISPATCHER_PLAN_CEILING", &c.Ceiling}, {"DISPATCHER_PLAN_MARGIN", &c.Margin}} {
		if s := os.Getenv(f.env); s != "" {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v < 0 || v > 1 {
				return c, fmt.Errorf("%s is a share between 0 and 1", f.env)
			}
			*f.v = v
		}
	}
	if c.Ceiling == 0 {
		return c, fmt.Errorf("DISPATCHER_PLAN_CEILING is above 0")
	}
	return c, nil
}

// winAt is one window of one account as last read, brought to now: a
// window whose reset has passed is empty.
type winAt struct {
	Used   float64   `json:"utilization"`
	Reset  time.Time `json:"resets_at,omitempty"` // zero: a five-hour window that starts at its next use
	ReadAt time.Time `json:"read_at"`
	Rolled bool      `json:"reset_since_read,omitempty"`
}

// accountPlan is one account's standing and what the rules make of it.
type accountPlan struct {
	Name     string           `json:"account"`
	Held     bool             `json:"held"`
	Known    bool             `json:"known"` // its weekly window was read
	Five     winAt            `json:"five_hour"`
	Week     winAt            `json:"seven_day"`
	Own      map[string]winAt `json:"own,omitempty"` // a model's own weekly window, by model
	Cost     float64          `json:"cost"`          // of the week, for one full five-hour window
	CostOwn  float64          `json:"cost_own"`      // of a model's own week, the same
	Measured float64          `json:"measured_over"` // five-hour windows the cost was measured over; 0: the default
	Room     float64          `json:"room"`          // of the week, for the agents
	RoomOwn  float64          `json:"room_own"`      // of the tightest model's own week
	OwnModel string           `json:"own_model,omitempty"`
	Windows  float64          `json:"windows"`     // full five-hour windows the room is
	WindowsO float64          `json:"windows_own"` // of them, on the model whose own week is tightest
	Urgency  float64          `json:"urgency"`     // windows per hour to the weekly reset
	Admits   bool             `json:"admits"`      // rule 5: work that can wait may run on it now
	// Goes: the same for the account held, which is worked until it is a
	// band ahead of its line, so that two accounts on their lines are not
	// switched between at every look.
	Goes bool `json:"goes"`
}

// accountsOf sorts the readings by account: a reading is of the account
// the latest model.account before it names. The readings between a switch
// and its record are the new account's: they are known by a weekly reset
// that is the next account's and not this one's.
func accountsOf(readings, changes []dispatch.Record) (map[string][]dispatch.Record, string) {
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Seq < changes[j].Seq })
	nameAt := func(seq int64) (string, int) {
		name, at := "", -1
		for i, c := range changes {
			if c.Seq < seq {
				name, at = c.Subject, i
			}
		}
		return name, at
	}
	weekly := func(r dispatch.Record) (time.Time, bool) {
		var rd reading
		if json.Unmarshal(r.Data, &rd) != nil {
			return time.Time{}, false
		}
		w, ok := rd.Info.Windows["seven_day"]
		return w.resets(), ok && w.ResetsAt != 0
	}
	// The first weekly reset read after each change.
	first := make([]time.Time, len(changes))
	for _, r := range readings {
		if _, i := nameAt(r.Seq); i >= 0 && first[i].IsZero() {
			if t, ok := weekly(r); ok {
				first[i] = t
			}
		}
	}
	out := map[string][]dispatch.Record{}
	var own time.Time // the weekly reset of the present stretch, before any doubt
	stretch := -2
	for _, r := range readings {
		name, i := nameAt(r.Seq)
		if i != stretch {
			stretch, own = i, time.Time{}
		}
		if t, ok := weekly(r); ok {
			next := i + 1
			if next < len(changes) && !first[next].IsZero() && sameReset(t, first[next]) && !own.IsZero() && !sameWeekly(t, own) {
				name = changes[next].Subject
			} else if own.IsZero() {
				own = t
			}
		}
		out[name] = append(out[name], r)
	}
	held := ""
	if len(changes) > 0 {
		held = changes[len(changes)-1].Subject
	}
	return out, held
}

// sameWeekly says two weekly resets are one account's: the same, or whole
// weeks apart.
func sameWeekly(a, b time.Time) bool {
	d := a.Sub(b).Abs() % week
	return d < 30*time.Minute || week-d < 30*time.Minute
}

// standingOf is one account's windows by its readings (oldest first), at
// now, and what a full five-hour window has cost of its weekly ones.
func standingOf(name string, rs []dispatch.Record, now time.Time, c planConfig) accountPlan {
	a := accountPlan{Name: name, Own: map[string]winAt{}, Cost: c.Cost, CostOwn: c.CostOwn}
	type at struct {
		five, week, own    float64
		fiveR, weekR, ownR time.Time
		ok                 bool
	}
	var prev at
	prevOwn := map[string]at{}
	var sum5, sum7, own5, ownSum float64
	for _, r := range rs {
		var rd reading
		if json.Unmarshal(r.Data, &rd) != nil {
			continue
		}
		f, okF := rd.Info.Windows["five_hour"]
		w, okW := rd.Info.Windows["seven_day"]
		if okF && f.ResetsAt != 0 {
			a.Five = winAt{Used: f.Utilization, Reset: f.resets(), ReadAt: r.Time}
		}
		if okW && w.ResetsAt != 0 {
			a.Week = winAt{Used: w.Utilization, Reset: w.resets(), ReadAt: r.Time}
			a.Known = true
		}
		if !(okF && okW && f.ResetsAt != 0 && w.ResetsAt != 0) {
			continue
		}
		cur := at{five: f.Utilization, week: w.Utilization, fiveR: f.resets(), weekR: w.resets(), ok: true}
		if prev.ok && sameReset(prev.fiveR, cur.fiveR) && sameReset(prev.weekR, cur.weekR) && cur.five > prev.five {
			sum5 += cur.five - prev.five
			sum7 += math.Max(0, cur.week-prev.week)
		}
		prev = cur
		for n, o := range rd.Info.Windows {
			if sharedWindows[n] || windowLength(n) != week || o.ResetsAt == 0 || rd.Model == "" {
				continue
			}
			a.Own[rd.Model] = winAt{Used: o.Utilization, Reset: o.resets(), ReadAt: r.Time}
			cur.own, cur.ownR = o.Utilization, o.resets()
			if p := prevOwn[rd.Model]; p.ok && sameReset(p.fiveR, cur.fiveR) && sameReset(p.ownR, cur.ownR) && cur.five > p.five {
				own5 += cur.five - p.five
				ownSum += math.Max(0, cur.own-p.own)
			}
			prevOwn[rd.Model] = cur
		}
	}
	if sum5 >= c.MinCost5 && sum7 > 0 {
		a.Cost, a.Measured = sum7/sum5, sum5
	}
	if own5 >= c.MinCost5 && ownSum > 0 {
		a.CostOwn = ownSum / own5
	}
	// Brought to now.
	if !a.Five.Reset.After(now) {
		a.Five = winAt{ReadAt: a.Five.ReadAt, Rolled: !a.Five.ReadAt.IsZero()}
	}
	roll := func(s winAt) winAt {
		if s.Reset.IsZero() || s.Reset.After(now) {
			return s
		}
		for !s.Reset.After(now) {
			s.Reset = s.Reset.Add(week)
		}
		s.Used, s.Rolled = 0, true
		return s
	}
	a.Week = roll(a.Week)
	for m, s := range a.Own {
		s = roll(s)
		if !a.Week.Reset.IsZero() && !sameReset(s.Reset, a.Week.Reset) && s.Reset.Before(a.Week.Reset.Add(-week+time.Hour)) {
			s = roll(winAt{Reset: a.Week.Reset.Add(-week)})
		}
		a.Own[m] = s
	}
	a.reckon(now, c)
	return a
}

// reserveAt is the owner's share of a weekly window at t: in proportion
// to the time left, nothing in the last hours.
func (c planConfig) reserveAt(reset, t time.Time) float64 {
	left := reset.Sub(t)
	if left <= c.Release {
		return 0
	}
	return c.Reserve * math.Min(1, float64(left)/float64(week))
}

// reckon is rules 1 to 3: room, windows, urgency.
func (a *accountPlan) reckon(now time.Time, c planConfig) {
	a.Room, a.RoomOwn, a.OwnModel, a.Windows, a.WindowsO, a.Urgency = 0, 0, "", 0, 0, 0
	if !a.Known {
		return
	}
	a.Room = math.Max(0, 1-a.Week.Used-c.reserveAt(a.Week.Reset, now))
	a.Windows = a.Room / a.Cost
	a.WindowsO = a.Windows
	for _, m := range sortedKeys(a.Own) {
		o := a.Own[m]
		room := math.Max(0, 1-o.Used-c.reserveAt(o.Reset, now))
		if w := room / a.CostOwn; a.OwnModel == "" || w < a.WindowsO {
			a.OwnModel, a.RoomOwn, a.WindowsO = m, room, math.Min(a.Windows, w)
		}
	}
	if h := a.Week.Reset.Sub(now).Hours(); h > 0 {
		a.Urgency = a.Windows / h
	}
	a.Admits = c.admits(*a, now, 0)
	a.Goes = c.admits(*a, now, c.Band)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// line is rule 5: where a weekly window may stand at t for work that can
// wait, a straight line to what the reserve leaves at its reset.
func (c planConfig) line(reset, t time.Time) float64 {
	f := 1 - float64(reset.Sub(t))/float64(week)
	return (1 - c.Reserve) * math.Min(1, math.Max(0, f))
}

// admits is rule 5 for an account at t: work that can wait runs while its
// week is at or under its line, or resets soon with room left.
func (c planConfig) admits(a accountPlan, t time.Time, band float64) bool {
	if !a.Known || a.Room <= 0 {
		return false
	}
	return a.Week.Used <= c.line(a.Week.Reset, t)+band+1e-9 || a.Week.Reset.Sub(t) <= c.Soon
}

// advice is which account to hold, and by which rule.
type advice struct {
	Account string `json:"account"`
	Rule    int    `json:"rule"`
	Why     string `json:"why"`
	Switch  bool   `json:"switch"` // it is not the one held
}

// choose is rules 3 and 4 among the accounts. Those whose five-hour
// window has room come first, of them those rule 5 admits work on, of them
// the one with the most room for the time it has left. sinceSwitch is how
// long the held one has been held.
func choose(as []accountPlan, held string, sinceSwitch time.Duration, c planConfig) advice {
	var cur, best *accountPlan
	admits := func(a *accountPlan) bool {
		if a.Name == held {
			return a.Goes
		}
		return a.Admits
	}
	better := func(a, b *accountPlan) bool {
		if admits(a) != admits(b) {
			return admits(a)
		}
		return a.Urgency > b.Urgency
	}
	for i := range as {
		a := &as[i]
		if a.Name == held {
			cur = a
		}
		if !a.Known || a.Five.Used >= c.Ceiling {
			continue
		}
		if best == nil || better(a, best) {
			best = a
		}
	}
	rate := func(a *accountPlan) string {
		s := fmt.Sprintf("%s has %.3f windows an hour to its weekly reset", label(a.Name), a.Urgency)
		if !admits(a) {
			s += " and is ahead of its line"
		}
		return s
	}
	switch {
	case best == nil && cur != nil:
		return advice{Account: held, Rule: 4, Why: fmt.Sprintf("every account's five-hour window is at %s or more: only work that cannot wait runs until one resets", pct(c.Ceiling))}
	case best == nil:
		return advice{Account: held, Rule: 3, Why: "no account's standing has been read"}
	case cur == nil || !cur.Known:
		return advice{Account: best.Name, Rule: 3, Switch: best.Name != held,
			Why: rate(best) + ", the most; the standing of the one held is not known"}
	case cur.Five.Used >= c.Ceiling:
		return advice{Account: best.Name, Rule: 4, Switch: true,
			Why: fmt.Sprintf("the five-hour window of %s is at %s, of %s at %s", label(cur.Name), pct(cur.Five.Used), label(best.Name), pct(best.Five.Used))}
	case best.Name == cur.Name:
		return advice{Account: held, Rule: 3, Why: rate(cur) + ", the most"}
	case sinceSwitch < c.Hold:
		return advice{Account: held, Rule: 3, Why: fmt.Sprintf("%s; %s; the last switch was under %s ago", rate(best), rate(cur), c.Hold)}
	case admits(best) == admits(cur) && best.Urgency < cur.Urgency*(1+c.Margin):
		return advice{Account: held, Rule: 3, Why: fmt.Sprintf("%s; %s: not %s more", rate(best), rate(cur), pct(c.Margin))}
	}
	return advice{Account: best.Name, Rule: 3, Switch: true, Why: rate(best) + "; " + rate(cur)}
}

func label(name string) string {
	if name == "" {
		return "the account"
	}
	return "account " + name
}

func hours(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	}
	return fmt.Sprintf("%.1f hours", d.Hours())
}

// block is a stretch of the days ahead and what the rules do in it.
type block struct {
	From     time.Time          `json:"from"`
	To       time.Time          `json:"to"`
	Held     map[string]float64 `json:"held_hours"`  // by account
	Windows  map[string]float64 `json:"windows"`     // full five-hour windows worked, by account
	WindowsO map[string]float64 `json:"windows_own"` // of them on the model whose own week is tightest
	Switches int                `json:"switches"`
	Why      []string           `json:"why,omitempty"` // each switch, once: when, to which, by which rule
}

// lost is room a weekly reset leaves unused.
type lost struct {
	Account string    `json:"account"`
	At      time.Time `json:"at"`
	Week    float64   `json:"seven_day"` // of the week, beyond the owner's share
	Own     float64   `json:"own,omitempty"`
	Model   string    `json:"model,omitempty"`
}

// budgetPlan is the whole plan.
type budgetPlan struct {
	At       time.Time     `json:"at"`
	Held     string        `json:"held"`
	Accounts []accountPlan `json:"accounts"`
	Advice   advice        `json:"advice"`
	// IfElsewhere is what each account loses at its next weekly reset if
	// the proxy does not hold it until then; Lost is what the plan itself
	// leaves at the resets of the days ahead.
	IfElsewhere []lost `json:"if_elsewhere"`
	// Shares is the room divided between the owner's priorities.
	Shares []share `json:"shares,omitempty"`
	Lost   []lost  `json:"lost,omitempty"`
	Blocks []block `json:"blocks"`
	Config planConfig
}

// planOf computes the plan from the readings and the changes of account.
func planOf(readings, changes []dispatch.Record, now time.Time, c planConfig) budgetPlan {
	by, held := accountsOf(readings, changes)
	p := budgetPlan{At: now, Held: held, Config: c}
	for _, n := range sortedKeys(by) {
		if n == "" && len(by) > 1 {
			continue // readings from before any account was named
		}
		a := standingOf(n, by[n], now, c)
		a.Held = n == held
		p.Accounts = append(p.Accounts, a)
	}
	var since time.Duration = week
	if len(changes) > 0 {
		since = now.Sub(changes[len(changes)-1].Time)
	}
	p.Advice = choose(p.Accounts, held, since, c)
	for _, a := range p.Accounts {
		if !a.Known || a.Room <= 0 {
			continue
		}
		// Unheld, only the release of the reserve changes its room.
		l := lost{Account: a.Name, At: a.Week.Reset, Week: math.Max(0, 1-a.Week.Used-c.Reserve*a.Week.Reset.Sub(now).Hours()/week.Hours())}
		if a.OwnModel != "" {
			l.Model, l.Own = a.OwnModel, a.RoomOwn
		}
		p.IfElsewhere = append(p.IfElsewhere, l)
	}
	p.Blocks, p.Lost = simulate(p.Accounts, p.Advice.Account, now, c)
	return p
}

// simulate walks the days ahead by the rules, with work always waiting:
// the held account is worked at one five-hour window in five hours while
// rule 5 admits it, on the model whose own week is tightest while that has
// room and is under its line (rule 6), else on another model.
func simulate(start []accountPlan, held string, now time.Time, c planConfig) ([]block, []lost) {
	const step = 10 * time.Minute
	as := make([]accountPlan, len(start))
	for i, a := range start {
		as[i] = a
		as[i].Own = map[string]winAt{}
		for m, s := range a.Own {
			as[i].Own[m] = s
		}
	}
	end := now.Add(time.Duration(c.Days) * 24 * time.Hour)
	var blocks []block
	var losses []lost
	newBlock := func(t time.Time) *block {
		blocks = append(blocks, block{From: t, Held: map[string]float64{}, Windows: map[string]float64{}, WindowsO: map[string]float64{}})
		return &blocks[len(blocks)-1]
	}
	b := newBlock(now)
	day := now.Add(24 * time.Hour)
	sinceSwitch := c.Hold
	for t := now; t.Before(end); t = t.Add(step) {
		cut := false
		for i := range as {
			a := &as[i]
			if !a.Five.Reset.IsZero() && !a.Five.Reset.After(t) {
				a.Five = winAt{}
			}
			if a.Known && !a.Week.Reset.After(t) {
				l := lost{Account: a.Name, At: a.Week.Reset, Week: math.Max(0, 1-a.Week.Used)}
				if a.OwnModel != "" {
					l.Model, l.Own = a.OwnModel, math.Max(0, 1-a.Own[a.OwnModel].Used)
				}
				if l.Week > 0.005 {
					losses = append(losses, l)
				}
				a.Week = winAt{Reset: a.Week.Reset.Add(week)}
				for m, s := range a.Own {
					a.Own[m] = winAt{Reset: s.Reset.Add(week)}
				}
				cut = true
			}
			a.reckon(t, c)
		}
		if !t.Before(day) {
			cut, day = true, day.Add(24*time.Hour)
		}
		if cut && t.After(b.From) {
			b.To = t
			b = newBlock(t)
		}
		adv := choose(as, held, sinceSwitch, c)
		if adv.Switch {
			b.Switches++
			if len(b.Why) < 4 {
				b.Why = append(b.Why, fmt.Sprintf("%s to %s (rule %d)", t.UTC().Format("Mon 15:04"), label(adv.Account), adv.Rule))
			}
			held, sinceSwitch = adv.Account, 0
		}
		sinceSwitch += step
		for i := range as {
			a := &as[i]
			if a.Name != held {
				continue
			}
			b.Held[a.Name] += step.Hours()
			if a.Five.Used >= c.Ceiling || !a.Goes {
				continue
			}
			d := math.Min(step.Hours()/5, c.Ceiling-a.Five.Used)
			if a.Room < d*a.Cost {
				d = a.Room / a.Cost
			}
			if d <= 0 {
				continue
			}
			if a.Five.Reset.IsZero() {
				a.Five.Reset = t.Add(5 * time.Hour)
			}
			a.Five.Used += d
			a.Week.Used += d * a.Cost
			b.Windows[a.Name] += d
			if m := a.OwnModel; m != "" {
				o := a.Own[m]
				if a.RoomOwn >= d*a.CostOwn && (o.Used <= c.line(o.Reset, t)+1e-9 || o.Reset.Sub(t) <= c.Soon) {
					o.Used += d * a.CostOwn
					a.Own[m] = o
					b.WindowsO[a.Name] += d
				}
			}
		}
	}
	b.To = end
	return blocks, losses
}

// planLines is the plan as a person reads it.
func planLines(p budgetPlan) []string {
	now := p.At
	var out []string
	if len(p.Accounts) == 0 {
		return []string{"the plan of the model's budget: no usage has been read yet"}
	}
	c := p.Config
	out = append(out, fmt.Sprintf("the plan of the model's budget, %s (kept for the owner: %s of each week, the last %s of each five-hour window)",
		now.UTC().Format("2006-01-02 15:04 UTC"), pct(c.Reserve), pct(1-c.Ceiling)))
	for _, a := range p.Accounts {
		state := "not held"
		if a.Held {
			state = "held by the proxy"
		}
		if !a.Known {
			out = append(out, fmt.Sprintf("  %s (%s): its weekly window has not been read", label(a.Name), state))
			continue
		}
		five := "five-hour " + pct(a.Five.Used)
		switch {
		case a.Five.Rolled:
			five = "five-hour empty (it has reset since it was read)"
		case !a.Five.Reset.IsZero():
			five += ", resets " + a.Five.Reset.UTC().Format("15:04")
		}
		l := fmt.Sprintf("  %s (%s, read %s): %s; seven-day %s, resets %s", label(a.Name), state, a.Week.ReadAt.UTC().Format("01-02 15:04"),
			five, pct(a.Week.Used), a.Week.Reset.UTC().Format("Mon 01-02 15:04"))
		for _, m := range sortedKeys(a.Own) {
			l += fmt.Sprintf("; %s's own week %s", m, pct(a.Own[m].Used))
		}
		out = append(out, l)
		cost := fmt.Sprintf("a full five-hour window costs %s of its week and %s of a model's own (measured over %.1f windows)", pct(a.Cost), pct(a.CostOwn), a.Measured)
		if a.Measured == 0 {
			cost = fmt.Sprintf("a full five-hour window is taken to cost %s of its week and %s of a model's own (too little read to measure)", pct(a.Cost), pct(a.CostOwn))
		}
		own := ""
		if a.OwnModel != "" {
			own = fmt.Sprintf(", %.1f of them on %s", a.WindowsO, a.OwnModel)
		}
		out = append(out, fmt.Sprintf("    %s; room for the agents: %s of the week, %.1f windows%s; %.3f windows an hour to its reset", cost, points(a.Room), a.Windows, own, a.Urgency))
	}
	verb := "hold"
	if p.Advice.Switch {
		verb = "switch to"
	} else if p.Advice.Account == p.Held {
		verb = "stay on"
	}
	out = append(out, fmt.Sprintf("  now: %s %s, by rule %d: %s", verb, label(p.Advice.Account), p.Advice.Rule, p.Advice.Why))
	for _, l := range p.IfElsewhere {
		s := fmt.Sprintf("  if the proxy does not hold %s until %s: %s of its week are lost", label(l.Account), l.At.UTC().Format("Mon 01-02 15:04"), points(l.Week))
		if l.Model != "" {
			s += fmt.Sprintf(", and %s of %s's own", points(l.Own), l.Model)
		}
		out = append(out, s)
	}
	out = append(out, shareLines(p)...)
	out = append(out, fmt.Sprintf("  the next %d days by the rules, work always waiting (UTC):", c.Days))
	for _, b := range p.Blocks {
		var parts []string
		for _, n := range sortedKeys(b.Held) {
			s := fmt.Sprintf("%s %.0fh, %.1f windows", label(n), b.Held[n], b.Windows[n])
			if b.WindowsO[n] > 0.05 {
				s += fmt.Sprintf(" (%.1f on the model whose own week is tightest)", b.WindowsO[n])
			}
			parts = append(parts, s)
		}
		s := fmt.Sprintf("    %s to %s: %s", b.From.UTC().Format("Mon 01-02 15:04"), b.To.UTC().Format("Mon 01-02 15:04"), strings.Join(parts, "; "))
		if b.Switches > 0 {
			s += fmt.Sprintf("; %d switch(es): %s", b.Switches, strings.Join(b.Why, ", "))
			if b.Switches > len(b.Why) {
				s += ", ..."
			}
		}
		out = append(out, s)
	}
	for _, l := range p.Lost {
		s := fmt.Sprintf("  left at the reset of %s, %s: %s of its week", label(l.Account), l.At.UTC().Format("Mon 01-02 15:04"), points(l.Week))
		if l.Model != "" && l.Own > 0.005 {
			s += fmt.Sprintf(", %s of %s's own", points(l.Own), l.Model)
		}
		out = append(out, s)
	}
	return out
}

// share is one priority's part of each account's room: until that
// account's weekly reset, so many full five-hour windows, so many of them
// on the model whose own week is tightest.
type share struct {
	Priority string             `json:"priority"`
	Part     float64            `json:"part"`
	Windows  map[string]float64 `json:"windows"`
	WindowsO map[string]float64 `json:"windows_own"`
	Next     []string           `json:"next,omitempty"` // what waits under it, first first
	Waiting  int                `json:"waiting"`
}

// divide splits each account's room by the priorities' parts.
func (p *budgetPlan) divide(v budget.View) {
	p.Shares = nil
	for _, st := range v.Streams {
		sh := share{Priority: st.Name, Part: st.Part, Windows: map[string]float64{}, WindowsO: map[string]float64{}, Waiting: len(st.Items)}
		for _, a := range p.Accounts {
			if a.Known {
				sh.Windows[a.Name], sh.WindowsO[a.Name] = st.Part*a.Windows, st.Part*a.WindowsO
			}
		}
		for i, it := range st.Items {
			if i == 3 {
				break
			}
			t := it.ID + ": " + it.Title
			if len(t) > 90 {
				t = t[:87] + "..."
			}
			switch {
			case it.Model != "":
				t += " (on " + it.Model + ")"
			case it.Hard:
				t += " (hard: the strongest model)"
			}
			if it.State == budget.Started {
				t += " [started]"
			}
			sh.Next = append(sh.Next, t)
		}
		p.Shares = append(p.Shares, sh)
	}
}

// shareLines says the division as a person reads it.
func shareLines(p budgetPlan) []string {
	if len(p.Shares) == 0 {
		return []string{"  no priorities are set: the room is not divided (dispatcher priority set NAME --share N)"}
	}
	out := []string{"  the room divided between the priorities, in full five-hour windows until each account's weekly reset:"}
	for _, sh := range p.Shares {
		var parts []string
		for _, a := range p.Accounts {
			if !a.Known {
				continue
			}
			s := fmt.Sprintf("%.1f on %s", sh.Windows[a.Name], label(a.Name))
			if a.OwnModel != "" {
				s += fmt.Sprintf(" (%.1f on %s)", sh.WindowsO[a.Name], a.OwnModel)
			}
			parts = append(parts, s)
		}
		out = append(out, fmt.Sprintf("    %s, %s: %s; %d waiting", sh.Priority, pct(sh.Part), strings.Join(parts, ", "), sh.Waiting))
		for _, n := range sh.Next {
			out = append(out, "      next: "+n)
		}
	}
	return out
}

// planReadings is how many readings the plan is computed from: enough for
// two accounts' costs.
const planReadings = 3000

// planCmd prints the plan: `dispatcher plan [--json]`.
func planCmd(ctx context.Context, c *api.Client, flags map[string][]string) error {
	get := func(typ string, n int) ([]dispatch.Record, error) {
		var last []api.Record
		if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {typ}, "last": {strconv.Itoa(n)}}, nil, &last); err != nil {
			return nil, err
		}
		rs := make([]dispatch.Record, len(last))
		for i, r := range last {
			rs[i] = dispatch.Record{Seq: r.Seq, Event: r.Event}
		}
		return rs, nil
	}
	rs, err := get(TypeUsageObserved, planReadings)
	if err != nil {
		return err
	}
	changes, err := get(TypeModelAccount, 200)
	if err != nil {
		return err
	}
	cfg, err := planSettings()
	if err != nil {
		return err
	}
	p := planOf(rs, changes, time.Now(), cfg)
	var v budget.View
	if err := c.Do(ctx, http.MethodGet, "/v1/budget/priorities", nil, nil, &v); err == nil {
		p.divide(v)
	}
	if _, ok := flags["json"]; ok {
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	for _, l := range planLines(p) {
		fmt.Println(l)
	}
	return nil
}

// planEvent is the plan's advice as an event, its ID the advice's: the
// same advice again records nothing.
func planEvent(p budgetPlan, seq int64) dispatch.Event {
	b, _ := json.Marshal(struct {
		Held        string        `json:"held"`
		Advice      advice        `json:"advice"`
		Accounts    []accountPlan `json:"accounts"`
		IfElsewhere []lost        `json:"if_elsewhere,omitempty"`
		Reading     int64         `json:"reading"`
	}{p.Held, p.Advice, p.Accounts, p.IfElsewhere, seq})
	return dispatch.Event{ID: fmt.Sprintf("%s/%d", TypeBudgetPlan, seq), Type: TypeBudgetPlan, Source: "dispatcher",
		Subject: p.Advice.Account, Time: p.At, Data: b}
}

// watchPlan records budget.plan when the advice is another than the last
// recorded: which account, held or not, by which rule. It looks every
// minute and switches nothing.
func watchPlan(ctx context.Context, readings, changes func(context.Context) ([]dispatch.Record, error),
	emit func(context.Context, dispatch.Event) error, cfg planConfig, now func() time.Time, every time.Duration, log *slog.Logger) {
	last := ""
	for {
		rs, err := readings(ctx)
		var cs []dispatch.Record
		if err == nil {
			cs, err = changes(ctx)
		}
		switch {
		case err != nil:
			log.Warn("reading usage for the plan", "error", err)
		case len(rs) > 0:
			p := planOf(rs, cs, now(), cfg)
			if key := fmt.Sprintf("%s/%s/%d/%t", p.Held, p.Advice.Account, p.Advice.Rule, p.Advice.Switch); key != last {
				if err := emit(ctx, planEvent(p, rs[len(rs)-1].Seq)); err != nil {
					log.Warn("recording the plan", "error", err)
				} else {
					last = key
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
