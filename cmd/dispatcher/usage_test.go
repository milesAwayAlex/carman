package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
)

// TestR7BudgetRule: work below the priority is held while a window is above
// the threshold, with a reason a person can read; urgent work, a window
// that has reset, a reading under it, and an old or absent reading admit.
func TestR7BudgetRule(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	reset := now.Add(150 * time.Minute)
	data := func(u float64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"model": "cheap", "rate_limit_info": map[string]any{"unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": u, "resetsAt": reset.Unix()},
			"seven_day": map[string]any{"utilization": 0.4, "resetsAt": now.Add(72 * time.Hour).Unix()},
		}}})
		return b
	}
	var recs []dispatch.Record
	rule := budgetRule{Below: 50, Above: 0.9, MaxAge: 2 * time.Hour, Now: func() time.Time { return now },
		Readings: func(context.Context) ([]dispatch.Record, error) { return recs, nil }}
	backlog, urgent := dispatch.TaskSpec{ID: "b", Priority: 10}, dispatch.TaskSpec{ID: "u", Priority: 50}
	ctx := context.Background()

	recs = []dispatch.Record{{Event: dispatch.Event{Time: now.Add(-10 * time.Minute), Data: data(0.93)}}}
	ok, why := rule.admit(ctx, backlog, nil)
	if ok || why != "the five-hour window is at 93% and resets at 14:30 UTC; this task can wait" {
		t.Errorf("backlog at 93%%: %v %q", ok, why)
	}
	if ok, _ := rule.admit(ctx, urgent, nil); !ok {
		t.Error("urgent work was held")
	}
	// A later reading of a refused model, which has no windows, does not
	// hide the shared windows of the one before it.
	recs = append(recs, dispatch.Record{Event: dispatch.Event{Time: now.Add(-5 * time.Minute),
		Data: json.RawMessage(`{"model":"strong","rate_limit_info":null,"refused":{"limit":"seven_day_opus"}}`)}})
	if ok, _ := rule.admit(ctx, backlog, nil); ok {
		t.Error("a refused model's reading hid the shared windows")
	}
	recs = recs[:1]
	recs[0].Data = data(0.5)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("held under the threshold")
	}
	recs[0].Data = data(0.95)
	now = reset.Add(time.Minute)
	recs[0].Time = now.Add(-30 * time.Minute)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("held after the window reset")
	}
	now = reset.Add(-time.Minute)
	recs[0].Time = now.Add(-3 * time.Hour)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("an old reading held work")
	}
	recs = nil
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("no reading held work")
	}
}

// TestR7BudgetRuleRefusedModel: a task whose ladder starts with a model
// refused until a time to come waits for it, whatever its priority, and
// says so; a refusal that has passed, a refused model later in the
// ladder, and a class with no ladder admit.
func TestR7BudgetRuleRefusedModel(t *testing.T) {
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	recs := []dispatch.Record{
		{Event: dispatch.Event{Time: now.Add(-3 * time.Hour), Data: json.RawMessage(fmt.Sprintf(
			`{"model":"strong","rate_limit_info":null,"refused":{"limit":"seven_day_overage_included","resets_at":%d,"message":"You've hit your limit"}}`, reset.Unix()))}},
		{Event: dispatch.Event{Time: now.Add(-10 * time.Minute), Data: json.RawMessage(
			`{"model":"cheap","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1795766400}}}}`)}},
	}
	ladders := map[string][]string{"night": {"strong", "cheap"}, "day": {"cheap", "strong"}}
	rule := budgetRule{Below: 50, Above: 0.9, MaxAge: 2 * time.Hour, Now: func() time.Time { return now },
		Readings: func(context.Context) ([]dispatch.Record, error) { return recs, nil },
		Models:   func(_ context.Context, task dispatch.TaskSpec) ([]string, error) { return ladders[task.Class], nil }}
	ctx := context.Background()
	ok, why := rule.admit(ctx, dispatch.TaskSpec{ID: "n", Class: "night", Priority: 90}, nil)
	if ok || why != "strong, the first model of this task's ladder, is refused (seven_day_overage_included) until 2026-11-27 18:00 UTC; this task waits for it" {
		t.Errorf("first model refused: %v %q", ok, why)
	}
	if ok, why := rule.admit(ctx, dispatch.TaskSpec{ID: "d", Class: "day", Priority: 10}, nil); !ok {
		t.Errorf("a refused model later in the ladder held work: %q", why)
	}
	if ok, _ := rule.admit(ctx, dispatch.TaskSpec{ID: "x", Class: "other", Priority: 10}, nil); !ok {
		t.Error("a class with no ladder was held")
	}
	now = reset.Add(time.Minute)
	if ok, _ := rule.admit(ctx, dispatch.TaskSpec{ID: "n", Class: "night", Priority: 10}, nil); !ok {
		t.Error("held after the refusal's reset")
	}
}

// TestR7ReadStream: a reading is the last rate_limit_event and the cost.
func TestR7ReadStream(t *testing.T) {
	stream := `{"type":"system"}
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.1,"resetsAt":1764000000}}}}
not json
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1764000000},"seven_day":{"utilization":0.5,"resetsAt":1764500000}}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
{"type":"result","total_cost_usd":0.0031}`
	o, err := readStream(strings.NewReader(stream), time.Now())
	var rd reading
	json.Unmarshal([]byte(`{"rate_limit_info":`+string(o.info)+`}`), &rd)
	if err != nil || o.cost != 0.0031 || o.refused != nil || rd.Info.Windows["five_hour"].Utilization != 0.25 || rd.Info.Windows["seven_day"].resets().Unix() != 1764500000 {
		t.Errorf("read %s, %v, %+v, %v", o.info, o.cost, o.refused, err)
	}
	if _, err := readStream(strings.NewReader(`{"type":"result"}`), time.Now()); err == nil {
		t.Error("a stream without a reading was taken")
	}
	if _, err := readStream(strings.NewReader(`{"type":"result","is_error":true,"result":"API Error: 500 overloaded"}`), time.Now()); err == nil {
		t.Error("a failure that is no limit was taken as a reading")
	}
}

// TestR7ReadStreamRefused: a call refused for a limit is a reading of its
// model, with the limit, its reset and the harness's words, whichever of
// the shapes it comes in. The first two cases give the one reading recorded
// from a real refusal (its info has no resetsAt, so its reset came from the
// words); the stream it came from was not kept, so either may be its shape.
// The other cases are made up after the harness's own schema, unconfirmed.
func TestR7ReadStreamRefused(t *testing.T) {
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	msg := "You've hit your limit · your model limit resets 6pm (UTC)"
	const realMsg = "You've hit your individual spend limit · ask your admin to raise it at … · your Fable limit resets 6pm (UTC)"
	const realInfo = `{"status":"rejected","rateLimitType":"seven_day_overage_included","unifiedWindows":{"five_hour":{"utilization":0.08,"status":"allowed"},"seven_day":{"utilization":0.71,"status":"allowed"},"seven_day_overage_included":{"utilization":1,"status":"rejected"}}}`
	for _, c := range []struct {
		name, stream string
		limit        string
		resets       int64
	}{
		{"seen: a rejected rate_limit_event", `{"type":"rate_limit_event","rate_limit_info":` + realInfo + `}
{"type":"result","is_error":true,"result":"` + realMsg + `"}`,
			"seven_day_overage_included", 1795802400},
		{"seen: the error message's record alone", `{"type":"assistant","error":"rate_limit","is_api_error_message":true,"api_error":"usage_limit_reached","api_error_params":{"rate_limit_info":` + realInfo + `},"message":{"content":[{"type":"text","text":"` + realMsg + `"}]}}
{"type":"result","is_error":true,"result":"` + realMsg + `"}`,
			"seven_day_overage_included", 1795802400},
		{"the error message's own record", `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1795766400}}}}
{"type":"assistant","error":"rate_limit","is_api_error_message":true,"api_error":"usage_limit_reached","api_error_params":{"rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_overage_included","resetsAt":1795802400,"isUsingOverage":false}},"message":{"content":[{"type":"text","text":"` + msg + `"}]}}
{"type":"result","subtype":"success","is_error":true,"result":"` + msg + `","total_cost_usd":0}`,
			"seven_day_overage_included", 1795802400},
		{"a rejected rate_limit_event", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_opus","resetsAt":1795800000}}
{"type":"result","is_error":true,"result":"` + msg + `"}`,
			"seven_day_opus", 1795800000},
		{"the words alone", `{"type":"result","is_error":true,"result":"` + msg + `"}`,
			"", time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC).Unix()},
		{"an error message with only its kind", `{"type":"assistant","error":"rate_limit","api_error":"usage_limit_reached","message":{"content":[{"type":"text","text":"Usage limit reached."}]}}`,
			"usage_limit_reached", 0},
	} {
		o, err := readStream(strings.NewReader(c.stream), now)
		if err != nil || o.refused == nil {
			t.Errorf("%s: %+v, %v", c.name, o, err)
			continue
		}
		if o.refused.Limit != c.limit || int64(o.refused.ResetsAt) != c.resets || o.refused.Message == "" {
			t.Errorf("%s: refused %+v", c.name, *o.refused)
		}
	}

	// The reading as recorded: refused until the words' reset, and its
	// windows, which name no reset, are not taken for the shared standing.
	rec := []byte(`{"model":"claude-fable-5-2","every_hours":1,"refused":{"limit":"seven_day_overage_included","resets_at":1795802400,"message":"` + realMsg + `"},"rate_limit_info":` + realInfo + `}`)
	var rd reading
	if err := json.Unmarshal(rec, &rd); err != nil {
		t.Fatal(err)
	}
	if until, refused := rd.refusedUntil(); !refused || until.Unix() != 1795802400 || rd.refusedLimit() != "seven_day_overage_included" {
		t.Errorf("the recorded refusal reads as %v %v %q", until, refused, rd.refusedLimit())
	}
	if _, _, ok := latestShared([]dispatch.Record{{Seq: 1, Event: dispatch.Event{Data: rec}}}); ok {
		t.Error("a refusal's windows without resets were taken as the shared standing")
	}
}

// TestR7DueModels: the reading's own model every hour; one read every six
// hours four times a day, not all in the same hour.
func TestR7DueModels(t *testing.T) {
	spec := "cheap=1,a=6,b=6,c=6"
	count := map[string]int{}
	hours := map[int]bool{}
	for h := range 24 {
		due, err := dueModels(spec, h)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			count[d]++
			if d != "cheap=1" {
				hours[h] = true
			}
		}
	}
	if count["cheap=1"] != 24 || count["a=6"] != 4 || count["b=6"] != 4 || count["c=6"] != 4 {
		t.Errorf("readings a day: %v", count)
	}
	if len(hours) < 8 {
		t.Logf("the six-hourly readings fall in %d hours of the day", len(hours))
	}
	if _, err := dueModels("cheap", 0); err == nil {
		t.Error("a model with no hours was taken")
	}
}

// TestR7UsageLines: dispatch status shows the shared windows once, then
// each model, refused first with its limit, reset and words, then a model
// with a limit of its own, then the rest; an old reading says so.
func TestR7UsageLines(t *testing.T) {
	now := time.Date(2026, 11, 27, 7, 12, 0, 0, time.UTC)
	rec := func(ago time.Duration, data string) dispatch.Record {
		return dispatch.Record{Event: dispatch.Event{Time: now.Add(-ago), Data: json.RawMessage(data)}}
	}
	five, seven := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC).Unix()
	shared := fmt.Sprintf(`"five_hour":{"utilization":0,"resetsAt":%d},"seven_day":{"utilization":0.7,"resetsAt":%d}`, five, seven)
	lines := usageLines([]dispatch.Record{
		rec(30*time.Hour, `{"model":"model-old","every_hours":6,"rate_limit_info":{"status":"allowed","unifiedWindows":{`+shared+`}}}`),
		rec(3*time.Hour, fmt.Sprintf(`{"model":"model-own","every_hours":6,"rate_limit_info":{"status":"allowed","unifiedWindows":{%s,"seven_day_overage_included":{"utilization":0.45,"resetsAt":%d}}}}`, shared, seven)),
		rec(2*time.Hour, fmt.Sprintf(`{"model":"model-refused","every_hours":6,"rate_limit_info":null,"refused":{"limit":"seven_day_overage_included","resets_at":%d,"message":"You've hit your limit"}}`,
			time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC).Unix())),
		rec(12*time.Minute, `{"model":"model-cheap","every_hours":1,"rate_limit_info":{"status":"allowed","unifiedWindows":{`+shared+`}}}`),
	}, now)
	want := []string{
		"usage, as read 12m0s ago (2026-11-27 07:00 UTC):",
		"  the five-hour window     0%, resets 2026-11-27 10:00 UTC (in 2h48m0s)",
		"  the seven-day window    70%, resets 2026-11-27 20:00 UTC (in 12h48m0s)",
		"  models, most constrained first:",
		`    model-refused  REFUSED: seven_day_overage_included, until 2026-11-27 18:00 UTC (in 10h48m0s); "You've hit your limit" (read 2h0m0s ago)`,
		"    model-own      allowed; its own limit: seven-day-overage-included 45%, resets 2026-11-27 20:00 UTC (read 3h0m0s ago)",
		"    model-cheap    allowed (read 12m0s ago)",
		"    model-old      allowed (read 30h0m0s ago, OLD: it is read every 6h0m0s)",
	}
	if got := strings.Join(lines, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// TestR7WindowLine: a window whose reset has passed since the reading says
// it has reset, and does not show its old percentage as current.
func TestR7WindowLine(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	past := window{Utilization: 0.83, ResetsAt: float64(now.Add(-42 * time.Minute).Unix())}
	got := windowLine("five_hour", past, now)
	if strings.Contains(got, "83%") || strings.Contains(got, "in -") || !strings.Contains(got, "reset at") || !strings.Contains(got, "42m0s ago") {
		t.Errorf("a window that has reset: %q", got)
	}
	future := window{Utilization: 0.83, ResetsAt: float64(now.Add(90 * time.Minute).Unix())}
	if got := windowLine("five_hour", future, now); !strings.Contains(got, "83%") || !strings.Contains(got, "in 1h30m0s") {
		t.Errorf("a window that has not reset: %q", got)
	}
}

// TestR10ModelStanding: dispatch status adds up the proxy's record of model
// use by agent, says what was refused, and sets each run against the
// proxy's record of the pod it ran in, showing both where they disagree.
func TestR10ModelStanding(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	rec := func(typ string, at time.Duration, data string) api.Record {
		return api.Record{Event: dispatch.Event{Type: typ, Time: now.Add(-at), Data: json.RawMessage(data)}}
	}
	runs := []api.Record{
		rec("run.usage", time.Hour, `{"agent":"dreamer","pod":"d-1","routine":"consolidate","tokens":{"input_tokens":10,"output_tokens":200,"cache_read_input_tokens":5000},"ended":"2026-11-26T11:00:00Z"}`),
		rec("run.usage", time.Hour, `{"agent":"dreamer","pod":"d-1","routine":"distill","tokens":{"input_tokens":5,"output_tokens":100},"ended":"2026-11-26T11:00:00Z"}`),
		rec("run.usage", 2*time.Hour, `{"agent":"builder","pod":"b-1","routine":"message","tokens":{"input_tokens":10,"output_tokens":20}}`),
		rec("run.usage", 3*time.Hour, `{"agent":"old","routine":"x","tokens":{"input_tokens":1}}`),
	}
	seen := []api.Record{
		rec("model.usage", time.Hour, `{"agent":"dreamer","pod":"d-1","model":"m1","requests":3,"tokens":{"input_tokens":15,"output_tokens":250,"cache_read_input_tokens":5000}}`),
		rec("model.usage", time.Hour, `{"agent":"dreamer","pod":"d-1","model":"m2","requests":1,"tokens":{"output_tokens":50}}`),
		rec("model.usage", 2*time.Hour, `{"agent":"builder","pod":"b-1","model":"m1","requests":2,"tokens":{"input_tokens":10,"output_tokens":90}}`),
		rec("model.usage", 6*time.Hour, `{"agent":"usage","pod":"u-1","model":"m3","requests":1,"tokens":{"input_tokens":2}}`),
		rec("model.usage", time.Hour, `{"agent":"","address":"10.0.0.9","requests":2,"refused":2}`),
	}
	got := strings.Join(modelStanding(now, runs, seen), "\n")
	for _, want := range []string{
		"model use at the proxy, last five hours: builder (in 10, out 90, cache write 0, cache read 0; 2 requests); dreamer (in 15, out 300, cache write 0, cache read 5000; 4 requests)\n",
		"last seven days: builder", "usage (in 2,",
		"refused at the proxy, last seven days: 2 requests, from 10.0.0.9",
		"runs against the proxy's record, last seven days: 1 agree, 1 differ in a way that is known\n",
		"b-1 (builder: message) differs: counted the old way, from the last result alone: the harness went on after an earlier result, and this is the last part only\n      the run says  in 10, out 20,",
		"the proxy saw in 10, out 90,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "d-1 (") {
		t.Errorf("d-1 agrees, summed over its runs and models:\n%s", got)
	}
}

// TestR10ADifferenceSaysItsKind: where a run's count and the proxy's record
// differ, status says which kind of difference it is, from how the run
// says it counted, and keeps DISAGREES for one that no way of counting
// explains. The first five are differences an installation showed.
func TestR10ADifferenceSaysItsKind(t *testing.T) {
	tok := func(in, out, cw, cr int64) tokens {
		return tokens{"input_tokens": in, "output_tokens": out, "cache_creation_input_tokens": cw, "cache_read_input_tokens": cr}
	}
	for _, c := range []struct {
		run, seen tokens
		basis     string
		finished  bool
		want      string
		known     bool
	}{
		{tok(110, 40_000, 125_000, 5_200_000), tok(166, 44_000, 143_000, 5_500_000), "", true, "leaves out a subagent's calls and the harness's small ones: the proxy saw 6% more", true},
		{tok(174, 52_000, 138_000, 8_700_000), tok(176, 52_000, 142_000, 8_700_000), "", true, "leaves out a subagent's calls and the harness's small ones: the proxy saw under 1% more", true},
		{tok(0, 0, 0, 0), tok(22, 2295, 41_000, 407_000), "", false, "cut short and counted nothing", true},
		{tok(188, 76_000, 180_000, 11_000_000), tok(324, 76_000, 180_000, 11_000_000), "", true, "the proxy saw under 1% more", true},
		{tok(2, 220, 1410, 202_000), tok(204, 64_000, 192_000, 13_000_000), "", true, "this is the last part only", true},
		// Counted the new ways.
		{tok(10, 3, 0, 0), tok(30, 9, 0, 0), "stream", false, "cut short: it counted what its stream had shown, and the proxy saw 200% more", true},
		{tok(100, 50, 0, 0), tok(120, 50, 0, 0), "whole", true, "the harness counted every call it made, and the proxy saw 13% more", false},
		{tok(100, 50, 0, 0), tok(90, 50, 0, 0), "whole", true, "the proxy's record of this pod is not whole", false},
		{tok(100, 50, 0, 0), tok(90, 50, 0, 0), "", true, "the proxy's record of this pod is not whole", false},
	} {
		kind, known := differs(c.run, c.seen, c.basis, c.finished)
		if !strings.Contains(kind, c.want) || known != c.known {
			t.Errorf("%v against %v (%q): %q, known %v; want %q, %v", c.run, c.seen, c.basis, kind, known, c.want, c.known)
		}
	}
	// In status: a pod whose run says it counted the whole and still differs.
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	rec := func(typ, data string) api.Record {
		return api.Record{Event: dispatch.Event{Type: typ, Time: now.Add(-time.Hour), Data: json.RawMessage(data)}}
	}
	got := strings.Join(modelStanding(now,
		[]api.Record{
			rec("run.usage", `{"agent":"a","pod":"p-1","routine":"message","basis":"whole","outcome":"done","tokens":{"input_tokens":100}}`),
			rec("run.usage", `{"agent":"a","pod":"p-2","routine":"message","basis":"stream","outcome":"not finished","tokens":{"input_tokens":100}}`),
		},
		[]api.Record{
			rec("model.usage", `{"agent":"a","pod":"p-1","model":"m","requests":2,"tokens":{"input_tokens":150}}`),
			rec("model.usage", `{"agent":"a","pod":"p-2","model":"m","requests":2,"tokens":{"input_tokens":150}}`),
		}), "\n")
	for _, want := range []string{
		"last seven days: 0 agree, 1 differ in a way that is known, 1 DISAGREE\n",
		"p-1 (a: message) DISAGREES: the harness counted every call it made, and the proxy saw 50% more",
		"p-2 (a: message) differs: the run was cut short: it counted what its stream had shown",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestR7ReadingsOfAnotherAccount: when the account at the proxy is changed,
// readings from before the change are of another account: they are not used
// for the standing or for admission.
func TestR7ReadingsOfAnotherAccount(t *testing.T) {
	rd := func(seq int64) dispatch.Record {
		return dispatch.Record{Seq: seq, Event: dispatch.Event{Type: TypeUsageObserved}}
	}
	rs := []dispatch.Record{rd(3), rd(7), rd(12)}
	if got := sinceAccount(rs, nil); len(got) != 3 {
		t.Errorf("no change recorded: %d readings, want all 3", len(got))
	}
	changed := []dispatch.Record{{Seq: 9, Event: dispatch.Event{Type: TypeModelAccount, Subject: "b"}}}
	got := sinceAccount(rs, changed)
	if len(got) != 1 || got[0].Seq != 12 {
		t.Errorf("after a change at 9: %+v, want only the reading at 12", got)
	}
	if lines := usageLines(sinceAccount(rs[:2], changed), time.Now()); len(lines) != 1 || !strings.Contains(lines[0], "no reading yet") {
		t.Errorf("with every reading from before the change: %v", lines)
	}
}
