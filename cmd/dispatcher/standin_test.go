package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
