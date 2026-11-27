package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/meter"
)

type lastStore struct {
	fakeStore
	byType map[string][]dispatch.Record
}

func (l *lastStore) Last(_ context.Context, typ string, _ int) ([]dispatch.Record, error) {
	return l.byType[typ], nil
}

func ev(seq int64, typ, source, subject string, at time.Time, data any) dispatch.Record {
	b, _ := json.Marshal(data)
	return dispatch.Record{Seq: seq, Event: dispatch.Event{Type: typ, Source: source, Subject: subject, Time: at, Data: b}}
}

// TestR7Standing: an agent's limit is its definition's unless the owner
// said otherwise; what it used today is the log's, less what the asking
// meter counted itself since it started; and the reading job learns when
// each model was last read, since the account was changed.
func TestR7Standing(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	use := func(agent, m string, from time.Time, out int64) meter.Record {
		return meter.Record{Agent: agent, Pod: "p", Meter: m, From: from, To: from, Tokens: map[string]int64{"output_tokens": out}}
	}
	store := &lastStore{byType: map[string][]dispatch.Record{
		meter.TypeModelLimit: {
			ev(1, meter.TypeModelLimit, "owner", "b", now, meter.Limit{Day: 50}),
			ev(2, meter.TypeModelLimit, "meter/proxy", "a", now, meter.Limit{}), // not the owner's: ignored
			ev(3, meter.TypeModelLimit, "owner", "c", now, meter.Limit{}),       // lifted
			ev(4, meter.TypeModelLimit, "owner", "d", now, meter.Limit{Day: 1}),
			ev(5, meter.TypeModelLimit, "owner", "d", now, map[string]bool{"definition": true}),
		},
		meter.TypeModelUsage: {
			ev(6, meter.TypeModelUsage, "meter/proxy", "a", now, use("a", "old", now.Add(-time.Hour), 10)),
			ev(7, meter.TypeModelUsage, "meter/proxy", "a", now, use("a", "me", now.Add(-time.Minute), 1000)),
			ev(8, meter.TypeModelUsage, "meter/proxy", "b", now, use("b", "old", now.Add(-time.Hour), 20)),
		},
		meter.TypeUsageObserved: {
			ev(9, meter.TypeUsageObserved, "agent/usage", "m1", now.Add(-3*time.Hour), map[string]string{"model": "m1"}),
			ev(11, meter.TypeUsageObserved, "meter/proxy", "m2", now.Add(-time.Minute), map[string]string{"model": "m2"}),
			ev(12, meter.TypeUsageObserved, "meter/proxy", "m1", now.Add(-2*time.Minute), map[string]string{"model": "m1"}),
		},
		"model.account": {ev(10, "model.account", "owner", "second", now.Add(-2*time.Hour), nil)},
	}}
	s := &Server{Store: store, Tokens: map[string]Role{"o": RoleOwner, "a": RoleAgent}, Now: func() time.Time { return now },
		ProxyLimits: map[string]meter.Limit{"a": {Day: 100}, "c": {Day: 100}, "d": {Pod: 7}}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path string, q url.Values, v any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path+"?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer o")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, resp.Status)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(v)
	}
	var st meter.Standing
	get("/v1/meter/standing", url.Values{"meter": {"me"}, "since": {now.Add(-10 * time.Minute).Format(time.RFC3339Nano)}}, &st)
	if a := st.Agents["a"]; a.Limit.Day != 100 || a.By != "definition" || a.Used != 50 {
		t.Errorf("a: %+v", a)
	}
	if b := st.Agents["b"]; b.Limit.Day != 50 || b.By != "owner" || b.Used != 100 {
		t.Errorf("b: %+v", b)
	}
	if _, ok := st.Agents["c"]; ok {
		t.Errorf("c's limit was lifted: %+v", st.Agents["c"])
	}
	if d := st.Agents["d"]; d.Limit.Pod != 7 || d.By != "definition" {
		t.Errorf("d: %+v", d)
	}
	get("/v1/meter/standing", nil, &st)
	if st.Agents["a"].Used != 5050 {
		t.Errorf("the owner's view counts every record: %+v", st.Agents["a"])
	}
	var latest UsageLatest
	get("/v1/usage/latest", nil, &latest)
	if len(latest.Models) != 2 || !latest.Models["m1"].Equal(now.Add(-2*time.Minute)) {
		t.Errorf("latest: %+v", latest)
	}
}
