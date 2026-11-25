package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/api"
)

// TestR7HarnessResultFromAStream: the result and the last reading of the
// plan's limits are read from the harness's stream, and a single result
// object is read as before.
func TestR7HarnessResultFromAStream(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.1}}}}
{"type":"assistant","message":{}}
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.2,"resetsAt":1764000000}}}}
{"type":"result","result":"ok","total_cost_usd":0.25,"is_error":false,"usage":{"input_tokens":10,"output_tokens":3}}
`
	r, rl, ok := harnessResult([]byte(stream))
	if !ok || r.Result != "ok" || r.Cost != 0.25 || !strings.Contains(string(rl), `"utilization":0.2`) || !strings.Contains(string(r.Usage), "input_tokens") {
		t.Errorf("read %+v, %s, %v", r, rl, ok)
	}
	r, rl, ok = harnessResult([]byte(`{"result":"done","total_cost_usd":1}`))
	if !ok || r.Result != "done" || rl != nil {
		t.Errorf("read %+v, %s, %v", r, rl, ok)
	}
	if _, _, ok := harnessResult([]byte("plain words")); ok {
		t.Error("plain words read as a result")
	}
}

// TestR7RunUsageEmitted: a run's usage goes to the dispatcher as the agent
// role; a dispatcher that is absent or refuses is one line, not a failure.
func TestR7RunUsageEmitted(t *testing.T) {
	var got api.EmitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			http.Error(w, `{"error":"no"}`, http.StatusForbidden)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()
	env := map[string]string{"DISPATCHER_URL": srv.URL, "DISPATCHER_TOKEN": "agent-token"}
	var out, errs bytes.Buffer
	c := &command{ctx: context.Background(), getenv: func(k string) string { return env[k] }, stdout: &out, stderr: &errs}
	u := runUsage{Agent: "price-review", Routine: "review", Started: time.Now(), Tokens: map[string]int64{}}
	u.add("claude-sonnet-5-5", attempt{cost: 0.5, usage: json.RawMessage(`{"input_tokens":100,"output_tokens":20,"service_tier":"standard"}`),
		rateLimit: json.RawMessage(`{"unifiedWindows":{}}`)})
	u.Cost, u.Outcome = 0.5, "done"
	c.emitUsage(u)
	var data runUsage
	json.Unmarshal(got.Data, &data)
	if got.Type != "run.usage" || got.Subject != "price-review" || data.Cost != 0.5 || data.Tokens["input_tokens"] != 100 ||
		len(data.Models) != 1 || data.RateLimit == nil || errs.Len() != 0 {
		t.Errorf("emitted %+v (%+v); stderr %q", got, data, errs.String())
	}
	env["DISPATCHER_TOKEN"] = "wrong"
	c.emitUsage(u)
	delete(env, "DISPATCHER_URL")
	c.emitUsage(u)
	if n := strings.Count(errs.String(), "usage is not recorded"); n != 2 {
		t.Errorf("a refusal and an absent dispatcher gave %d lines: %q", n, errs.String())
	}
}
