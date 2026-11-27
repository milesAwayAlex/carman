package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// series builds readings of one model, oldest first, each with the given
// windows: name to utilization and reset.
type win struct {
	u     float64
	reset time.Time
}

func rec(seq int64, at time.Time, model string, ws map[string]win) dispatch.Record {
	uw := map[string]any{}
	for n, w := range ws {
		uw[n] = map[string]any{"utilization": w.u, "resetsAt": w.reset.Unix()}
	}
	b, _ := json.Marshal(map[string]any{"model": model, "every_hours": 1,
		"rate_limit_info": map[string]any{"status": "allowed", "unifiedWindows": uw}})
	return dispatch.Record{Seq: seq, Event: dispatch.Event{Type: TypeUsageObserved, Subject: model, Time: at, Data: b}}
}

func curveRule(now *time.Time, rs *[]dispatch.Record) budgetRule {
	return budgetRule{Below: 50, Mode: "curve", Target: 0.95, Soon: time.Hour, MaxAge: 2 * time.Hour,
		Now:      func() time.Time { return *now },
		Readings: func(context.Context) ([]dispatch.Record, error) { return *rs, nil },
		Models: func(_ context.Context, class string) ([]string, error) {
			return map[string][]string{"deep": {"strong", "cheap"}, "light": {"cheap"}}[class], nil
		}}
}

var (
	backlog = dispatch.TaskSpec{ID: "b", Class: "light", Priority: 10}
	urgent  = dispatch.TaskSpec{ID: "u", Class: "light", Priority: 50}
)

func find(t *testing.T, ps []pace, window, model string) pace {
	t.Helper()
	for _, p := range ps {
		if p.Window == window && p.Model == model {
			return p
		}
	}
	t.Fatalf("no pace for %s %q in %+v", window, model, ps)
	return pace{}
}

// TestR7PaceQuietWeek: a seven-day window used little and steadily is
// under its line, admits backlog work, and says how much would be lost at
// its reset, as status and as a usage.room event.
func TestR7PaceQuietWeek(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * 24 * time.Hour) // four days in
	var rs []dispatch.Record
	for h := 48; h >= 0; h-- {
		at := now.Add(-time.Duration(h) * time.Hour)
		// 0.4% an hour since the window began, 96 hours ago.
		u := 0.004 * at.Sub(reset.Add(-7*24*time.Hour)).Hours()
		rs = append(rs, rec(int64(100-h), at, "cheap", map[string]win{"seven_day": {u, reset}}))
	}
	p := find(t, paces(rs, now, 0.95), "seven_day", "")
	if p.Rate < 0.0039 || p.Rate > 0.0041 {
		t.Errorf("rate %v, want 0.004/h", p.Rate)
	}
	if p.Ahead() >= 0 || p.Unused() < 0.27 || p.Unused() > 0.29 {
		t.Errorf("used %v line %v unused %v: want under its line, about 28 points unused", p.Used, p.Line, p.Unused())
	}
	if !p.RateSince.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("rate measured since %v, want a day back", p.RateSince)
	}
	rule := curveRule(&now, &rs)
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("held in a quiet week: %s", why)
	}
	line := paceLine(p, now)
	if !strings.Contains(line, "under by 16 points") || !strings.Contains(line, "28 points UNUSED") {
		t.Errorf("status line: %s", line)
	}
	es := roomEvents(rs, now, 0.95, 0.1)
	if len(es) != 1 || es[0].Subject != "seven_day" || es[0].ID != "usage.room/100/seven_day" {
		t.Fatalf("room events: %+v", es)
	}
	var d roomData
	_ = json.Unmarshal(es[0].Data, &d)
	if d.Unused < 0.27 || d.Target != 0.95 || !d.ResetsAt.Equal(reset) {
		t.Errorf("room data: %+v", d)
	}
	if es := roomEvents(rs, now, 0.95, 0.5); len(es) != 0 {
		t.Errorf("room under the minimum recorded: %+v", es)
	}
}

// TestR7PaceBurst: a burst that would use the five-hour window up before
// its reset holds backlog work with the reason and when it is expected to
// be admitted, and does not hold urgent work.
func TestR7PaceBurst(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour) // two hours in
	seven := win{0.2, now.Add(5 * 24 * time.Hour)}
	rs := []dispatch.Record{
		rec(1, now.Add(-90*time.Minute), "cheap", map[string]win{"five_hour": {0.05, reset}, "seven_day": seven}),
		rec(2, now.Add(-60*time.Minute), "cheap", map[string]win{"five_hour": {0.2, reset}, "seven_day": seven}),
		rec(3, now, "cheap", map[string]win{"five_hour": {0.6, reset}, "seven_day": seven}),
	}
	p := find(t, paces(rs, now, 0.95), "five_hour", "")
	if p.Rate < 0.36 || p.Rate > 0.37 {
		t.Errorf("rate %v, want (0.6-0.05)/1.5h", p.Rate)
	}
	if got := p.ReachesTarget(); got.Sub(now) < 56*time.Minute || got.Sub(now) > 58*time.Minute {
		t.Errorf("reaches the target at %v", got)
	}
	rule := curveRule(&now, &rs)
	ok, why := rule.admit(context.Background(), backlog, nil)
	// The line reaches 60% at 3h9m into the window: 13:09.
	if ok || !strings.Contains(why, "the five-hour window is at 60%, ahead of its line (38% now, 95% at its reset at 2026-11-27 15:00 UTC)") ||
		!strings.Contains(why, "expected to be admitted about 2026-11-27 13:09 UTC") {
		t.Errorf("burst: %v %q", ok, why)
	}
	if ok, _ := rule.admit(context.Background(), urgent, nil); !ok {
		t.Error("urgent work held")
	}
	if line := paceLine(p, now); !strings.Contains(line, "AHEAD by 22 points") || !strings.Contains(line, "reaches 95% at 12:57 UTC, 2h03m before its reset") {
		t.Errorf("status line: %s", line)
	}
	if es := roomEvents(rs, now, 0.95, 0.1); len(es) != 1 || es[0].Subject != "seven_day" {
		t.Errorf("room events in a burst: %+v", es)
	}
	// An old reading is unknown, and unknown admits.
	later := now.Add(3*time.Hour - time.Minute)
	rule.Now = func() time.Time { return later }
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("an old reading held: %s", why)
	}
}

// TestR7PaceSoon: a window that resets soon with room left admits
// everything; one at its target holds backlog work until it resets.
func TestR7PaceSoon(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reset := now.Add(40 * time.Minute)
	rs := []dispatch.Record{rec(1, now.Add(-10*time.Minute), "cheap", map[string]win{"five_hour": {0.92, reset}})}
	rule := curveRule(&now, &rs)
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("held before a reset with room: %s", why)
	}
	rs = []dispatch.Record{rec(1, now.Add(-10*time.Minute), "cheap", map[string]win{"five_hour": {0.97, reset}})}
	if ok, why := rule.admit(context.Background(), backlog, nil); ok || !strings.Contains(why, "admitted about 2026-11-27 12:40 UTC") {
		t.Errorf("at the target: %v %q", ok, why)
	}
	// Ahead with room, two hours out: admitted at the line, or an hour
	// before the reset, whichever is first.
	reset = now.Add(2 * time.Hour)
	rs = []dispatch.Record{rec(1, now, "cheap", map[string]win{"five_hour": {0.9, reset}})}
	if ok, why := rule.admit(context.Background(), backlog, nil); ok || !strings.Contains(why, "admitted about 2026-11-27 13:00 UTC") {
		t.Errorf("ahead, two hours out: %v %q", ok, why)
	}
}

// TestR7PaceReset: readings of the window before its reset are not
// counted in the new one's rate; with no reading since the reset the
// window is new and admits.
func TestR7PaceReset(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	old, next := now.Add(-30*time.Minute), now.Add(270*time.Minute)
	rs := []dispatch.Record{
		rec(1, now.Add(-2*time.Hour), "cheap", map[string]win{"five_hour": {0.7, old}}),
		rec(2, now.Add(-time.Hour), "cheap", map[string]win{"five_hour": {0.99, old}}),
	}
	p := find(t, paces(rs, now, 0.95), "five_hour", "")
	if !p.Reset0 || !p.Reset.Equal(old.Add(5*time.Hour)) || p.Used != 0 {
		t.Errorf("after the reset: %+v", p)
	}
	rule := curveRule(&now, &rs)
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("held by a window that has reset: %s", why)
	}
	if line := paceLine(p, now); !strings.Contains(line, "reset since the last reading") {
		t.Errorf("status line: %s", line)
	}
	if es := roomEvents(rs, now, 0.95, 0.1); len(es) != 0 {
		t.Errorf("room recorded for a window not read since its reset: %+v", es)
	}
	// One reading in the new window: its rate is its average since the
	// window began, not the slope from the old window's 99%.
	rs = append(rs, rec(3, now, "cheap", map[string]win{"five_hour": {0.1, next}}))
	p = find(t, paces(rs, now, 0.95), "five_hour", "")
	if p.Reset0 || p.Rate < 0.199 || p.Rate > 0.201 || !p.RateSince.Equal(old) {
		t.Errorf("first reading of the new window: rate %v since %v", p.Rate, p.RateSince)
	}
}

// TestR7PaceAccountChange: readings from before the account changed are
// another account's; with one reading of the new one, the rate is its
// average since the window began.
func TestR7PaceAccountChange(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	rs := []dispatch.Record{
		rec(1, now.Add(-2*time.Hour), "cheap", map[string]win{"five_hour": {0.1, reset}}),
		rec(2, now.Add(-time.Hour), "cheap", map[string]win{"five_hour": {0.8, reset}}),
		rec(4, now, "cheap", map[string]win{"five_hour": {0.2, reset}}),
	}
	changed := []dispatch.Record{{Seq: 3, Event: dispatch.Event{Type: TypeModelAccount, Subject: "b"}}}
	p := find(t, paces(sinceAccount(rs, changed), now, 0.95), "five_hour", "")
	if p.Rate < 0.099 || p.Rate > 0.101 {
		t.Errorf("rate after the account changed %v, want 0.2 over two hours", p.Rate)
	}
	now2 := now
	all := sinceAccount(rs, changed)
	rule := curveRule(&now2, &all)
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("held by the other account's readings: %s", why)
	}
}

// TestR7PaceModelWindow: a model's own window holds the work whose ladder
// starts with it, and not the work of another model.
func TestR7PaceModelWindow(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	seven := now.Add(4 * 24 * time.Hour) // three days in: line 41%
	rs := []dispatch.Record{
		rec(1, now.Add(-time.Hour), "cheap", map[string]win{"seven_day": {0.2, seven}}),
		rec(2, now, "strong", map[string]win{"seven_day": {0.2, seven}, "seven_day_opus": {0.7, seven}}),
	}
	rule := curveRule(&now, &rs)
	deep := dispatch.TaskSpec{ID: "d", Class: "deep", Priority: 10}
	if ok, why := rule.admit(context.Background(), deep, nil); ok || !strings.Contains(why, "the seven-day-opus window is at 70%") {
		t.Errorf("own window: %v %q", ok, why)
	}
	if ok, why := rule.admit(context.Background(), backlog, nil); !ok {
		t.Errorf("another model's window held: %s", why)
	}
	if lines := paceLines(rs, now, 0.95); len(lines) != 3 || !strings.Contains(lines[2], "seven-day-opus (strong)") {
		t.Errorf("status: %q", lines)
	}
}

// TestR7PaceRealShape: readings in the shape the hourly reading records
// (several models, the binding limit at the top, the windows beside it,
// a refusal with no windows) are paced without error.
func TestR7PaceRealShape(t *testing.T) {
	now := time.Unix(1795766400, 0).UTC().Add(-3 * time.Hour)
	raw := []string{
		`{"model":"cheap","every_hours":1,"rate_limit_info":{"status":"allowed","resetsAt":1795766400,"rateLimitType":"five_hour","utilization":0.31,"unifiedWindows":{"five_hour":{"utilization":0.31,"resetsAt":1795766400,"status":"allowed"},"seven_day":{"utilization":0.52,"resetsAt":1796140800,"status":"allowed"}}}}`,
		`{"model":"strong","every_hours":6,"rate_limit_info":{"status":"allowed","resetsAt":1795766400,"rateLimitType":"five_hour","utilization":0.33,"unifiedWindows":{"five_hour":{"utilization":0.33,"resetsAt":1795766400},"seven_day":{"utilization":0.52,"resetsAt":1796140800},"seven_day_opus":{"utilization":0.12,"resetsAt":1796140800}}}}`,
		`{"model":"mid","every_hours":6,"rate_limit_info":null,"refused":{"limit":"seven_day_sonnet","resets_at":1796140800,"message":"You've hit your limit"}}`,
		`{"model":"cheap","every_hours":1,"rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.41,"resetsAt":1795766400},"seven_day":{"utilization":0.53,"resetsAt":1796140800}}}}`,
	}
	var rs []dispatch.Record
	for i, d := range raw {
		rs = append(rs, dispatch.Record{Seq: int64(i + 1), Event: dispatch.Event{Type: TypeUsageObserved, Time: now.Add(time.Duration(i-3) * 20 * time.Minute), Data: json.RawMessage(d)}})
	}
	ps := paces(rs, now, 0.95)
	if len(ps) != 3 {
		t.Fatalf("paces: %+v", ps)
	}
	five := find(t, ps, "five_hour", "")
	if five.Used != 0.41 || five.Rate < 0.099 || five.Rate > 0.101 {
		t.Errorf("five-hour: used %v rate %v, want 41%% at 10 points an hour", five.Used, five.Rate)
	}
	find(t, ps, "seven_day_opus", "strong")
	for _, l := range paceLines(rs, now, 0.95) {
		t.Log(l)
	}
}
