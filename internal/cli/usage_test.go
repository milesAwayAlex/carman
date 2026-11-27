package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
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

// TestR7ARunsCountIsTheWholeRuns: what a run says it used is every call
// its process made (ADR 0050: it is set against the proxy's record of the
// pod). A harness that went on after its result, when a subagent or a
// command it left running ended, printed a result for each part: the count
// is the last one's count by model, not the last part's own. With no count
// by model the parts are added up; with no result, or a part cut short
// after one, the stream's answers are, once each, and the count says so.
func TestR7ARunsCountIsTheWholeRuns(t *testing.T) {
	count := func(out string) (map[string]int64, string, bool) {
		r, _, ok := harnessResult([]byte(out))
		got := map[string]int64{}
		_ = json.Unmarshal(r.Usage, &got)
		return got, r.Basis, ok
	}
	// As the harness printed it for a run that started a subagent (its
	// answer has a parent) and went on after its result.
	stream := `{"type":"system","subtype":"init","model":"m"}
{"type":"assistant","message":{"id":"a","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":7}}}
{"type":"assistant","message":{"id":"a","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":7}}}
{"type":"assistant","parent_tool_use_id":"t","message":{"id":"b","usage":{"input_tokens":60,"output_tokens":1}}}
{"type":"assistant","message":{"id":"c","usage":{"input_tokens":120,"output_tokens":1}}}
{"type":"result","result":"first","total_cost_usd":0.1,"usage":{"input_tokens":220,"output_tokens":10,"cache_read_input_tokens":7},"modelUsage":{"m":{"inputTokens":220,"outputTokens":10,"cacheReadInputTokens":7},"small":{"inputTokens":60,"outputTokens":5,"cacheCreationInputTokens":3}}}
{"type":"assistant","message":{"id":"d","usage":{"input_tokens":130,"output_tokens":1}}}
{"type":"result","result":"last","total_cost_usd":0.2,"usage":{"input_tokens":130,"output_tokens":5},"modelUsage":{"m":{"inputTokens":350,"outputTokens":15,"cacheReadInputTokens":7},"small":{"inputTokens":60,"outputTokens":5,"cacheCreationInputTokens":3}}}
`
	got, basis, ok := count(stream)
	if !ok || basis != "whole" || got["input_tokens"] != 410 || got["output_tokens"] != 20 || got["cache_read_input_tokens"] != 7 || got["cache_creation_input_tokens"] != 3 {
		t.Errorf("the whole run: %v, %q, %v", got, basis, ok)
	}
	// A harness with no count by model: its parts, added up.
	parts := strings.ReplaceAll(stream, `"modelUsage"`, `"other"`)
	if got, basis, _ := count(parts); basis != "results" || got["input_tokens"] != 350 || got["output_tokens"] != 15 {
		t.Errorf("the parts added up: %v, %q", got, basis)
	}
	// Cut short before any result: the answers the stream showed, once each.
	lines := strings.SplitAfter(stream, "\n")
	if got, basis, ok := count(strings.Join(lines[:5], "")); ok || basis != "stream" || got["input_tokens"] != 280 || got["output_tokens"] != 3 {
		t.Errorf("cut short before a result: %v, %q, %v", got, basis, ok)
	}
	// Cut short in a later part: the result before it, and the answers since.
	if got, basis, ok := count(strings.Join(lines[:7], "")); !ok || basis != "stream" || got["input_tokens"] != 410 || got["output_tokens"] != 16 {
		t.Errorf("cut short after a result: %v, %q, %v", got, basis, ok)
	}
	// One result object, as a harness asked for no stream prints it.
	if got, basis, _ := count(`{"result":"done","usage":{"input_tokens":5,"output_tokens":2}}`); basis != "results" || got["input_tokens"] != 5 {
		t.Errorf("one object: %v, %q", got, basis)
	}
	// Nothing was counted: nothing is said, and not zeros.
	if r, _, _ := harnessResult([]byte(`{"type":"system","subtype":"init"}` + "\n")); r.Usage != nil || r.Basis != "" {
		t.Errorf("nothing counted: %s, %q", r.Usage, r.Basis)
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

// TestR7MessageRunUsageEmitted: a run the dispatcher started for a message
// (agent-resume --message) records its usage through the same code as a
// routine's: the agent, the message, the model the stream says ran, the
// cost, the tokens and the last reading of the plan's limits. Recording
// never fails the command, even with nothing to read.
func TestR7MessageRunUsageEmitted(t *testing.T) {
	var got []api.EmitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e api.EmitRequest
		json.NewDecoder(r.Body).Decode(&e)
		got = append(got, e)
		io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	stream := dir + "/out"
	os.WriteFile(stream, []byte(`{"type":"system","subtype":"init","model":"claude-haiku-5-5"}
{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.4}}}}
{"type":"result","result":"answered","total_cost_usd":0.07,"is_error":false,"usage":{"input_tokens":50,"output_tokens":9}}
`), 0o600)
	env := map[string]string{"DISPATCHER_URL": srv.URL, "DISPATCHER_TOKEN": "agent-token", "AGENT_NAME": "review",
		"TASK_ID": "message/m-1", "CMEM_ROUTINE_MODELS": "claude-sonnet-5-5,claude-opus-5-5"}
	getenv := func(k string) string { return env[k] }
	var out, errs strings.Builder
	since := "2026-11-25T08:00:00Z"
	if code := Run(context.Background(), []string{"routine", "--usage-of", stream, "--message", "m-1", "--since", since}, getenv, nil, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if code := Run(context.Background(), []string{"routine", "--usage-of", dir + "/none", "--message", "m-2", "--failed"}, getenv, nil, &out, &errs); code != 0 {
		t.Fatalf("a run with no output to read: exit %d: %s", code, errs.String())
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d events, want 2", len(got))
	}
	var first, second runUsage
	json.Unmarshal(got[0].Data, &first)
	json.Unmarshal(got[1].Data, &second)
	if got[0].Type != "run.usage" || got[0].Subject != "review" || first.Routine != "message" || first.Message != "m-1" ||
		first.Task != "message/m-1" || !slices.Equal(first.Models, []string{"claude-haiku-5-5"}) || first.Cost != 0.07 ||
		first.Tokens["output_tokens"] != 9 || !strings.Contains(string(first.RateLimit), "0.4") || first.Outcome != "done" ||
		!first.Started.Equal(time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("the run's usage: %+v (%+v)", got[0], first)
	}
	if second.Message != "m-2" || second.Outcome != "not finished" || !slices.Equal(second.Models, []string{"claude-sonnet-5-5"}) {
		t.Errorf("a run that left nothing to read: %+v", second)
	}
	// No dispatcher: one line, and still no failure.
	delete(env, "DISPATCHER_URL")
	errs.Reset()
	if code := Run(context.Background(), []string{"routine", "--usage-of", stream, "--message", "m-3"}, getenv, nil, &out, &errs); code != 0 ||
		!strings.Contains(errs.String(), "usage is not recorded") {
		t.Errorf("no dispatcher: exit %d, %q", code, errs.String())
	}
}

// TestR7ARoutineRecordsWhatItHadNoToolFor: the part of a routine's report
// the improvement loop reads (what it had no tool for, its questions for
// the owner) is recorded as routine.report, bounded, marked as an agent's
// words; a report without either records nothing.
func TestR7ARoutineRecordsWhatItHadNoToolFor(t *testing.T) {
	report := `Run 20261127-0300.

- Retired two entries about the old proxy.
- I checked the chart with the read-only kubectl prefix: it agrees.
- I would have checked the tracker for the ticket an entry names, and had no tool for it.

Questions for the owner
- Is the backup's store meant to be kept a year?

Left alone: three scoped entries.

To undo: cmem consolidate revert 20261127-0300 'REASON'`
	part := reportPart(report)
	for _, want := range []string{"tracker", "Is the backup's store meant to be kept a year?"} {
		if !strings.Contains(part, want) {
			t.Errorf("the part lacks %q:\n%s", want, part)
		}
	}
	for _, not := range []string{"Retired", "chart", "Left alone", "revert"} {
		if strings.Contains(part, not) {
			t.Errorf("the part has %q:\n%s", not, part)
		}
	}
	if reportPart("Done: two entries merged.") != "" {
		t.Error("a report with nothing for the loop gave a part")
	}
	if long := reportPart("I had no tool for " + strings.Repeat("x", 5000)); len([]rune(long)) > reportMax+1 {
		t.Errorf("the part is %d long", len([]rune(long)))
	}

	var got api.EmitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()
	env := map[string]string{"DISPATCHER_URL": srv.URL, "DISPATCHER_TOKEN": "agent-token"}
	var out, errs bytes.Buffer
	c := &command{ctx: context.Background(), getenv: func(k string) string { return env[k] }, stdout: &out, stderr: &errs}
	c.emitReport(runUsage{Agent: "dreamer", Routine: "consolidate", Started: time.Now()}, report)
	var data map[string]string
	json.Unmarshal(got.Data, &data)
	if got.Type != TypeRoutineReport || got.Subject != "dreamer" || data["written_by"] != "agent" || !strings.Contains(data["report"], "tracker") {
		t.Errorf("emitted %+v (%v); stderr %q", got, data, errs.String())
	}
}
