package kube

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
)

type memMarks struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *memMarks) Mark(_ context.Context, name string, v any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[name]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (m *memMarks) SetMark(_ context.Context, name string, v any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := json.Marshal(v)
	m.m[name] = b
	return err
}

// TestR10ExpectedRuns: a CronJob's tick with no Job is recorded once as
// run.missed, through restarts; a tick it ran for, or caught up on, is not;
// nor is one while it was suspended, nor one before it was first seen.
func TestR10ExpectedRuns(t *testing.T) {
	var mu sync.Mutex
	suspended := false
	jobs := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/apis/batch/v1/namespaces/ns/cronjobs":
			io.WriteString(w, `{"items":[{"metadata":{"name":"cj","uid":"u1"},"spec":{"schedule":"* * * * *","suspend":`+strconv.FormatBool(suspended)+`,"startingDeadlineSeconds":30}}]}`)
		case "/apis/batch/v1/namespaces/ns/jobs":
			var items []map[string]any
			for _, j := range jobs {
				items = append(items, map[string]any{"metadata": map[string]any{"name": j}})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	client := &Client{Base: srv.URL, Namespace: "ns", HTTP: srv.Client()}
	base := time.Date(2026, 11, 24, 10, 0, 30, 0, time.UTC)
	now := base
	var emitted []dispatch.Event
	marks := &memMarks{m: map[string][]byte{}}
	ended := map[string]bool{}
	open := func() *Expect {
		return &Expect{Client: client, Marks: marks, Grace: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:   func() time.Time { return now },
			Emit:  func(_ context.Context, e dispatch.Event) error { emitted = append(emitted, e); return nil },
			Ended: func(context.Context, string, time.Time) (map[string]bool, error) { return ended, nil }}
	}
	jobFor := func(hhmm string) string {
		tk, _ := time.Parse(time.RFC3339, "2026-11-24T"+hhmm+":00Z")
		return "cj-" + strconv.FormatInt(tk.Unix()/60, 10)
	}
	x := open()
	pass := func(at string) {
		t.Helper()
		now, _ = time.Parse(time.RFC3339, "2026-11-24T"+at+"Z")
		if err := x.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	pass("09:58:30") // first seen: 09:58 and before are not judged
	jobs = append(jobs, jobFor("09:59"))
	ended[jobFor("10:00")] = true // ended, and gone from the cluster
	pass("10:00:30")
	pass("10:02:00") // 10:01 and 10:02 owed: no Job for either yet
	mu.Lock()
	jobs = append(jobs, jobFor("10:02")) // the catch-up covers 10:01 too
	mu.Unlock()
	pass("10:04:20") // 10:03 and 10:04 owed; judged up to 10:02:50
	if len(emitted) != 0 {
		t.Fatalf("emitted %+v before any tick was missed", emitted)
	}
	pass("10:04:50") // 10:03 is judged: missed
	if len(emitted) != 1 || emitted[0].ID != "run.missed/cj/2026-11-24T10:03:00Z" {
		t.Fatalf("emitted %+v, want one run.missed for 10:03", emitted)
	}
	mu.Lock()
	suspended = true // between looks: the next tick, 10:05, still counts
	mu.Unlock()
	x = open() // a restart
	pass("10:05:10")
	pass("10:09:00")
	if len(emitted) != 3 || emitted[1].ID != "run.missed/cj/2026-11-24T10:04:00Z" || emitted[2].ID != "run.missed/cj/2026-11-24T10:05:00Z" {
		t.Fatalf("emitted %v, want 10:04 and 10:05 missed, and nothing while suspended", ids(emitted))
	}
	x = open()
	pass("10:20:00")
	if len(emitted) != 3 {
		t.Errorf("after a restart, emitted %v", ids(emitted))
	}
}

func ids(es []dispatch.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func TestR10RanFor(t *testing.T) {
	tick := time.Date(2026, 11, 24, 10, 17, 0, 0, time.UTC)
	m := tick.Unix() / 60
	n := func(k int64) map[string]bool {
		return map[string]bool{"agent-dreamer-" + strconv.FormatInt(k, 10): true}
	}
	if !ranFor("agent-dreamer", tick, time.Hour, n(m)) || !ranFor("agent-dreamer", tick, time.Hour, n(m+60)) {
		t.Error("a Job for the tick, or for one within the grace, did not count")
	}
	if ranFor("agent-dreamer", tick, time.Hour, n(m-1)) || ranFor("agent-dreamer", tick, time.Hour, n(m+61)) ||
		ranFor("agent", tick, time.Hour, n(m)) {
		t.Error("a Job outside the grace, or of another CronJob, counted")
	}
}
