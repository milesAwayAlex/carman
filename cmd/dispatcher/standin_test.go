package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
)

// R10: the stand-in answers in shapes the meter reads as it reads the
// model's own: a whole answer and a streamed one are both counted, by the
// model asked for, with the counts the stand-in says it gives; it takes
// only the credential it was given, which the meter puts on; and a
// refusal for the plan's limit is a 429 with the standing in its headers.
func TestR10_TheMeterReadsTheStandIn(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("the-trials-own\n"), 0o600)
	stand := newStandIn(tokenFile)
	model := httptest.NewServer(stand)
	t.Cleanup(model.Close)
	up, _ := url.Parse(model.URL)
	ledger := &meter.Ledger{Idle: time.Minute, Longest: time.Hour}
	front := httptest.NewServer(&meter.Proxy{Upstream: up, Transport: &http.Transport{DisableCompression: true},
		Credential: func() (string, error) { return "Bearer the-trials-own", nil }, Direct: true, Ledger: ledger,
		Identify: func(context.Context, string) (message.Caller, bool) {
			return message.Caller{Agent: "caller", Pod: "caller-1"}, true
		}})
	t.Cleanup(front.Close)
	ask := func(base, body, auth string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	whole, streamed := `{"model":"claude-haiku-5-5","messages":[]}`, `{"model":"claude-haiku-5-5","stream":true,"messages":[]}`
	if resp, body := ask(front.URL, whole, "Bearer a-placeholder"); resp.StatusCode != 200 || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("a whole answer: %d %s", resp.StatusCode, body)
	}
	resp, body := ask(front.URL, streamed, "Bearer a-placeholder")
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "event: message_start\ndata: {\"type\":\"message_start\"") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("a streamed answer: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("anthropic-ratelimit-unified-status") != "allowed" || resp.Header.Get("anthropic-ratelimit-unified-5h-utilization") == "" {
		t.Errorf("the standing's headers: %v", resp.Header)
	}
	// Asked directly with the pod's placeholder, it knows no such credential.
	if resp, _ := ask(model.URL, whole, "Bearer a-placeholder"); resp.StatusCode != 401 {
		t.Errorf("the placeholder passed: %d", resp.StatusCode)
	}
	var mine *meter.Record
	for _, r := range ledger.Due(true) {
		if r.Agent == "caller" && r.Model == "claude-haiku-5-5" {
			mine = &r
		}
	}
	want := int64(len(whole)/4 + len(streamed)/4)
	if mine == nil || mine.Requests != 2 || mine.Unread != 0 || mine.Tokens["output_tokens"] != 10 || mine.Tokens["input_tokens"] != want {
		t.Fatalf("the meter's record of the stand-in's answers: %+v (want input %d)", mine, want)
	}
	stand.mu.Lock()
	stand.status = "rejected"
	stand.mu.Unlock()
	if resp, body := ask(front.URL, whole, "x"); resp.StatusCode != 429 || resp.Header.Get("anthropic-ratelimit-unified-status") != "rejected" ||
		resp.Header.Get("x-should-retry") != "false" || !strings.Contains(body, "rate_limit_error") {
		t.Errorf("a refusal for the plan's limit: %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

// TestR10_TheStandInFollowsAScript: a conversation whose first message
// scripts the stand-in is asked for those tools, one for each answer, whole
// and streamed, and told "ok" when they are used up; so a check can make a
// real harness start a subagent or leave a command running, with no model.
func TestR10_TheStandInFollowsAScript(t *testing.T) {
	model := httptest.NewServer(newStandIn(""))
	t.Cleanup(model.Close)
	ask := func(stream bool, messages string) string {
		body := fmt.Sprintf(`{"model":"m","stream":%v,"messages":[%s]}`, stream, messages)
		resp, err := http.Post(model.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return string(out)
	}
	first := `{"role":"user","content":[{"type":"text","text":"Do it.\nstand-in: use Bash {\"command\":\"echo one\"}\nstand-in: use Agent {\"prompt\":\"say ok\"}"}]}`
	used := `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"one"}]}`
	if got := ask(false, first); !strings.Contains(got, `"type":"tool_use"`) || !strings.Contains(got, `"name":"Bash","input":{"command":"echo one"}`) || !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Errorf("the first answer is not the script's first tool: %s", got)
	}
	got := ask(true, first+","+used)
	if !strings.Contains(got, `"content_block":{"type":"tool_use"`) || !strings.Contains(got, `"name":"Agent"`) ||
		!strings.Contains(got, `"delta":{"type":"input_json_delta","partial_json":"{\"prompt\":\"say ok\"}"}`) || !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Errorf("the second answer, streamed, is not the script's second tool: %s", got)
	}
	both := used + `,{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Agent","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`
	if got := ask(false, first+","+both); !strings.Contains(got, `"text":"ok"`) || !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Errorf("the script used up, the answer is not ok: %s", got)
	}
	// Asked to tell the results, its last answer is what the tools gave back.
	telling := strings.Replace(first, `"Do it.\n`, `"Do it.\nstand-in: tell results\n`, 1)
	denied := strings.Replace(both, `"content":"ok"`, `"is_error":true,"content":[{"type":"text","text":"not\nallowed"}]`, 1)
	if got := ask(false, telling+","+denied); !strings.Contains(got, `result 1: one\nresult 2: ERROR not allowed`) {
		t.Errorf("the results are not told: %s", got)
	}
	// No script, as a string or as blocks: ok, as before.
	if got := ask(false, `{"role":"user","content":"say ok"}`); !strings.Contains(got, `"text":"ok"`) {
		t.Errorf("a conversation with no script: %s", got)
	}
	// What it answered is kept by model, for a check to set beside a count.
	resp, _ := http.Get(model.URL + "/stand-in/seen")
	seen, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(seen), `"tokens":{"m":{"input_tokens":`) || !strings.Contains(string(seen), `"output_tokens":25`) {
		t.Errorf("what the stand-in answered is not kept: %s", seen)
	}
}

// TestR10_TheStandInIsSetBesideTheAPI: the comparison finds what the
// stand-in says that an API does not (an event, a field, a header, fields
// in another order), lists what the API has more without failing, and
// passes the stand-in set beside itself.
func TestR10_TheStandInIsSetBesideTheAPI(t *testing.T) {
	model := httptest.NewServer(newStandIn(""))
	t.Cleanup(model.Close)
	get := func(stream bool) shape {
		resp, err := http.Post(model.URL+"/v1/messages", "application/json", strings.NewReader(fmt.Sprintf(`{"model":"m","stream":%v,"messages":[{"role":"user","content":"x"}]}`, stream)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return shapeOf(resp, raw)
	}
	whole, stream := get(false), get(true)
	if w, b := compareShapes(whole, whole); len(w)+len(b) != 0 {
		t.Errorf("a whole answer beside itself: %v, %v", w, b)
	}
	if w, b := compareShapes(stream, stream); len(w)+len(b) != 0 {
		t.Errorf("a stream beside itself: %v, %v", w, b)
	}
	if got := strings.Join(stream.fields["message_start"][:4], " "); got != "type message message.model message.id" {
		t.Errorf("a stream's fields are not read in the order sent: %s", got)
	}
	if len(stream.headers) < 5 || stream.events[0] != "message_start" || stream.events[len(stream.events)-1] != "message_stop" {
		t.Errorf("the shape of a stream: %+v", stream)
	}
	// An API that has more, has one field elsewhere, and lacks an event and a header.
	api := shape{status: 200, headers: append([]string{"anthropic-ratelimit-unified-new"}, stream.headers[1:]...), fields: map[string][]string{}}
	for e, f := range stream.fields {
		api.fields[e] = append([]string{}, f...)
	}
	for _, e := range stream.events {
		if e != "content_block_stop" {
			api.events = append(api.events, e)
		}
	}
	delete(api.fields, "content_block_stop")
	f := api.fields["message_start"]
	f[0], f[1] = f[1], f[0]
	api.fields["message_delta"] = append(api.fields["message_delta"], "usage.server_tool_use")
	wrong, behind := compareShapes(api, stream)
	for _, want := range []string{"a stream's events", "content_block_stop: the API sent no such event", "message_start: fields in another order", "the stand-in sends the header " + stream.headers[0]} {
		if !slices.ContainsFunc(wrong, func(w string) bool { return strings.Contains(w, want) }) {
			t.Errorf("not found wrong: %q in %v", want, wrong)
		}
	}
	if len(wrong) != 4 || len(behind) != 2 {
		t.Errorf("wrong %v; behind %v", wrong, behind)
	}
}
