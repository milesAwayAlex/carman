package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
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
