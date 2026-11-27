package meter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// limitHeaders are the shapes the model's API sends, for a subscription's
// credential, on every answer (looked at when this was built).
func limitHeaders(h http.Header, status, fiveStatus string, five float64) {
	h.Set("anthropic-ratelimit-unified-status", status)
	h.Set("anthropic-ratelimit-unified-reset", "1795794000")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-fallback-percentage", "0.5")
	h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
	h.Set("anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled")
	h.Set("anthropic-ratelimit-unified-5h-utilization", strconv.FormatFloat(five, 'f', -1, 64))
	h.Set("anthropic-ratelimit-unified-5h-reset", "1795794000")
	h.Set("anthropic-ratelimit-unified-5h-status", fiveStatus)
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.16")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1795975200")
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
}

func TestR7ReadHeaders(t *testing.T) {
	h := http.Header{}
	limitHeaders(h, "allowed", "allowed", 0.06)
	h.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.26")
	h.Set("anthropic-ratelimit-unified-7d_oi-reset", "1795975200")
	h.Set("anthropic-ratelimit-unified-7d_oi-status", "allowed")
	h.Set("anthropic-ratelimit-unified-grace-5h-utilization", "0.9")
	li, ok := ReadHeaders(h)
	if !ok || li.Status != "allowed" || li.ResetsAt != 1795794000 || li.RateLimitType != "five_hour" || li.Utilization == nil || *li.Utilization != 0.06 {
		t.Fatalf("top level: %+v", li)
	}
	want := map[string]Window{
		"five_hour":                  {0.06, 1795794000, "allowed"},
		"seven_day":                  {0.16, 1795975200, "allowed"},
		"seven_day_overage_included": {0.26, 1795975200, "allowed"},
	}
	if len(li.Windows) != len(want) {
		t.Fatalf("windows: %+v", li.Windows)
	}
	for n, w := range want {
		if li.Windows[n] != w {
			t.Errorf("%s: %+v, want %+v", n, li.Windows[n], w)
		}
	}
	if n, _ := windowName("7d_opus"); n != "seven_day_opus" {
		t.Errorf("7d_opus is %q", n)
	}
	if _, ok := ReadHeaders(http.Header{"Content-Type": {"application/json"}}); ok {
		t.Error("an answer with no limits' headers read as a standing")
	}
	// The harness's reading of the same answer, as the job records it, is
	// read by the same fields.
	b, _ := json.Marshal(li)
	if !strings.Contains(string(b), `"unifiedWindows":{"five_hour":{"utilization":0.06,"resetsAt":1795794000,"status":"allowed"}`) {
		t.Errorf("not the harness's form: %s", b)
	}
}

func TestR7ObserverThrottles(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	o := &Observer{Every: 5 * time.Minute, Now: func() time.Time { return now }}
	wake := o.Wake()
	h := http.Header{}
	limitHeaders(h, "allowed", "allowed", 0.1)
	if !o.Observe("m", "a", 200, h) {
		t.Fatal("the first answer gave no reading")
	}
	<-wake
	now = now.Add(time.Minute)
	if o.Observe("m", "a", 200, h) {
		t.Error("a reading a minute after the last, nothing changed")
	}
	if !o.Observe("other", "a", 200, h) {
		t.Error("another model's first answer gave no reading")
	}
	now = now.Add(5 * time.Minute)
	if !o.Observe("m", "a", 200, h) {
		t.Error("no reading after Every")
	}
	now = now.Add(time.Second)
	limitHeaders(h, "allowed_warning", "allowed_warning", 0.8)
	if !o.Observe("m", "a", 200, h) {
		t.Error("no reading at once when a window's status changed")
	}
	select {
	case <-wake:
	default:
		t.Error("a change did not wake the sender")
	}
	now = now.Add(time.Second)
	limitHeaders(h, "rejected", "rejected", 1)
	if !o.Observe("m", "a", 429, h) {
		t.Error("no reading at once for a refusal")
	}
	rs := o.Take()
	if len(rs) != 5 || o.Take() != nil {
		t.Fatalf("readings: %d", len(rs))
	}
	last := rs[4]
	if last.Refused == nil || last.Refused.Limit != "five_hour" || last.Refused.ResetsAt != 1795794000 || last.Model != "m" || last.Agent != "a" {
		t.Errorf("the refusal: %+v %+v", last, last.Refused)
	}
	// A 429 that is no limit's (no unified status) is no refusal.
	if o.Observe("x", "a", 429, http.Header{}) {
		t.Error("an answer without the headers gave a reading")
	}
}

func TestR7Tally(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	rec := func(agent, pod, meter string, from time.Time, out int64) Record {
		return Record{Agent: agent, Pod: pod, PodUID: pod + "-uid", Meter: meter, From: from, To: from.Add(time.Minute),
			Tokens: map[string]int64{"output_tokens": out, "cache_read_input_tokens": 1000}}
	}
	limits := map[string]AgentStanding{"a": {Limit: Limit{Day: 1000}, By: "definition"}, "off": {}}
	rs := []Record{
		rec("a", "p1", "m1", now.Add(-20*time.Hour), 100), // yesterday: the pod's, not the day's
		rec("a", "p1", "m1", now.Add(-2*time.Hour), 10),   // another meter's, or before this one started
		rec("a", "p2", "me", now.Add(-30*time.Minute), 7), // this meter's own since it started: left out
		rec("b", "p3", "m1", now.Add(-time.Hour), 99),     // no limit
		{Agent: "a", OverBudget: 3, From: now.Add(-time.Minute), To: now},
	}
	s := Tally(limits, rs, now, func(r Record) bool { return r.Meter == "me" && !r.From.Before(started) })
	a, ok := s.Agents["a"]
	if !ok || len(s.Agents) != 1 || s.Day != "2026-11-27" {
		t.Fatalf("standing: %+v", s)
	}
	if a.Used != 150 || a.Pods["p1-uid"] != 750 || a.Pods["p2-uid"] != 0 || a.Refused != 3 {
		t.Errorf("a: %+v", a)
	}
}

func TestR7BudgetHolds(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 0, 0, 0, time.UTC)
	b := &Budget{Now: func() time.Time { return now }}
	who := message.Caller{Agent: "a", Pod: "p", PodUID: "u"}
	if _, over := b.Over(who); over {
		t.Fatal("held before any standing came")
	}
	b.Set(Standing{Day: "2026-11-27", Agents: map[string]AgentStanding{"a": {Limit: Limit{Day: 1000, Pod: 600}, Used: 500, By: "owner"}}})
	if _, over := b.Over(who); over {
		t.Fatal("held under its limit")
	}
	b.Count(who, map[string]int64{"output_tokens": 100}) // 500 weighted: the pod's 500, the day's 1000
	why, over := b.Over(who)
	if !over || !strings.Contains(why, "1000 weighted tokens for today") || !strings.Contains(why, "set by the owner") {
		t.Fatalf("not held at its day's limit: %q", why)
	}
	// The next day the standing's count is yesterday's and this meter's
	// own starts again; the pod's count does not.
	now = now.Add(2 * time.Hour)
	if _, over := b.Over(who); over {
		t.Fatal("held the next day")
	}
	b.Count(who, map[string]int64{"input_tokens": 100})
	why, over = b.Over(who)
	if !over || !strings.Contains(why, "for one pod") {
		t.Fatalf("not held at its pod's limit: %q", why)
	}
	if _, over := b.Over(message.Caller{Agent: "a", Pod: "q", PodUID: "v"}); over {
		t.Error("another pod held for one pod's use")
	}
	if _, over := b.Over(message.Caller{Agent: "b"}); over {
		t.Error("an agent with no limit held")
	}
}

// The proxy refuses an agent over its budget as the API refuses, with a
// status the harness shows once as what it is; reads the standing from
// the answer's headers, by the model the request asked for; and counts
// what the answer used against the agent.
func TestR7ProxyBudgetAndReadings(t *testing.T) {
	var calls int
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.Copy(io.Discard, r.Body)
		limitHeaders(w.Header(), "allowed", "allowed", 0.3)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","model":"claude-sonnet-5-5","usage":{"input_tokens":400,"output_tokens":100}}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	tr := up.Client().Transport.(*http.Transport).Clone()
	ledger := &Ledger{Idle: time.Minute, Longest: time.Hour}
	obs := &Observer{Every: 5 * time.Minute}
	budget := &Budget{}
	budget.Set(Standing{Day: Day(time.Now()), Agents: map[string]AgentStanding{"dreamer": {Limit: Limit{Day: 1500}, Used: 600, By: "definition"}}})
	p := &Proxy{Upstream: u, Transport: tr, CallerHeader: "X-Egress-Caller", Ledger: ledger, Observer: obs, Budget: budget,
		Credential: func() (string, error) { return "Bearer x", nil },
		Identify: func(context.Context, string) (message.Caller, bool) {
			return message.Caller{Agent: "dreamer", Pod: "d-1", PodUID: "u1"}, true
		}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	ask := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"a \"model\": no"}]}`))
		req.Header.Set("X-Egress-Caller", "10.0.0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := ask(); resp.StatusCode != 200 {
		t.Fatalf("under its budget: %d", resp.StatusCode)
	}
	// 600 + 400 + 5*100 = 1500: spent.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5"}`))
	req.Header.Set("X-Egress-Caller", "10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct{ Type, Message string }
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || body.Error.Type != "billing_error" || !strings.Contains(body.Error.Message, "agent dreamer has used 1500") || calls != 1 {
		t.Fatalf("over its budget: %d %+v, upstream asked %d times", resp.StatusCode, body, calls)
	}
	rs := obs.Take()
	if len(rs) != 1 || rs[0].Model != "claude-sonnet-5-5" || rs[0].Agent != "dreamer" || rs[0].Info.Windows["five_hour"].Utilization != 0.3 {
		t.Fatalf("readings: %+v", rs)
	}
	var refused, requests int64
	for _, r := range ledger.Due(true) {
		refused += r.OverBudget
		requests += r.Requests
	}
	if refused != 1 || requests != 2 {
		t.Errorf("ledger: refused %d of %d", refused, requests)
	}
}
