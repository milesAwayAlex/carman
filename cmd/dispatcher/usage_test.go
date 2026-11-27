package main

import (
	"context"
	"encoding/json"
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
		b, _ := json.Marshal(map[string]any{"rate_limit_info": map[string]any{"unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": u, "resetsAt": reset.Unix()},
			"seven_day": map[string]any{"utilization": 0.4, "resetsAt": now.Add(72 * time.Hour).Unix()},
		}}})
		return b
	}
	var rec dispatch.Record
	have := true
	rule := budgetRule{Below: 50, Above: 0.9, MaxAge: 2 * time.Hour, Now: func() time.Time { return now },
		Latest: func(context.Context) (dispatch.Record, bool, error) { return rec, have, nil }}
	backlog, urgent := dispatch.TaskSpec{ID: "b", Priority: 10}, dispatch.TaskSpec{ID: "u", Priority: 50}
	ctx := context.Background()

	rec = dispatch.Record{Event: dispatch.Event{Time: now.Add(-10 * time.Minute), Data: data(0.93)}}
	ok, why := rule.admit(ctx, backlog, nil)
	if ok || why != "the five-hour window is at 93% and resets at 14:30 UTC; this task can wait" {
		t.Errorf("backlog at 93%%: %v %q", ok, why)
	}
	if ok, _ := rule.admit(ctx, urgent, nil); !ok {
		t.Error("urgent work was held")
	}
	rec.Data = data(0.5)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("held under the threshold")
	}
	rec.Data = data(0.95)
	now = reset.Add(time.Minute)
	rec.Time = now.Add(-30 * time.Minute)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("held after the window reset")
	}
	now = reset.Add(-time.Minute)
	rec.Time = now.Add(-3 * time.Hour)
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("an old reading held work")
	}
	have = false
	if ok, _ := rule.admit(ctx, backlog, nil); !ok {
		t.Error("no reading held work")
	}
}

func TestR7ReadStream(t *testing.T) {
	stream := `{"type":"system"}
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.1,"resetsAt":1764000000}}}}
not json
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1764000000},"seven_day":{"utilization":0.5,"resetsAt":1764500000}}}}
{"type":"result","total_cost_usd":0.0031}`
	info, cost, err := readStream(strings.NewReader(stream))
	var rd reading
	json.Unmarshal([]byte(`{"rate_limit_info":`+string(info)+`}`), &rd)
	if err != nil || cost != 0.0031 || rd.Info.Windows["five_hour"].Utilization != 0.25 || rd.Info.Windows["seven_day"].resets().Unix() != 1764500000 {
		t.Errorf("read %s, %v, %v", info, cost, err)
	}
	if _, _, err := readStream(strings.NewReader(`{"type":"result"}`)); err == nil {
		t.Error("a stream without a reading was taken")
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
		"runs against the proxy's record, last seven days: 1 agree",
		"b-1 (builder: message) DISAGREES:\n      the run says  in 10, out 20,",
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
