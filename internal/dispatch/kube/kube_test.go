package kube

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// fakeAPI is the part of the Kubernetes API the starter and the watcher
// use, for one namespace.
type fakeAPI struct {
	mu       sync.Mutex
	jobs     map[string]map[string]any
	cronjobs map[string]map[string]any
	pods     []any
	creates  int
	watch    chan string // lines a watch sends, then it ends
}

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	f := &fakeAPI{jobs: map[string]map[string]any{}, cronjobs: map[string]map[string]any{}, watch: make(chan string, 16)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &Client{Base: srv.URL, Namespace: "ns", HTTP: srv.Client(), Token: func() (string, error) { return "t", nil }}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer t" {
		http.Error(w, `{"reason":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"reason":"NotFound","message":"not found"}`)
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/apis/batch/v1/namespaces/ns/cronjobs/"):
		cj, ok := f.cronjobs[strings.TrimPrefix(path, "/apis/batch/v1/namespaces/ns/cronjobs/")]
		if !ok {
			notFound()
			return
		}
		json.NewEncoder(w).Encode(cj)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/apis/batch/v1/namespaces/ns/jobs/"):
		job, ok := f.jobs[strings.TrimPrefix(path, "/apis/batch/v1/namespaces/ns/jobs/")]
		if !ok {
			notFound()
			return
		}
		json.NewEncoder(w).Encode(job)
	case r.Method == http.MethodPost && path == "/apis/batch/v1/namespaces/ns/jobs":
		var job map[string]any
		json.NewDecoder(r.Body).Decode(&job)
		name := dig(job, "metadata", "name").(string)
		if _, ok := f.jobs[name]; ok {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"reason":"AlreadyExists"}`)
			return
		}
		f.creates++
		job["metadata"].(map[string]any)["uid"] = "uid-" + name
		f.jobs[name] = job
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(job)
	case r.Method == http.MethodGet && path == "/apis/batch/v1/namespaces/ns/jobs" && r.URL.Query().Get("watch") == "1":
		f.mu.Unlock()
		for {
			select {
			case line := <-f.watch:
				io.WriteString(w, line+"\n")
				w.(http.Flusher).Flush()
			case <-time.After(200 * time.Millisecond):
				f.mu.Lock()
				return
			}
		}
	case r.Method == http.MethodGet && path == "/apis/batch/v1/namespaces/ns/jobs":
		items := []any{}
		sel := &Client{Selector: r.URL.Query().Get("labelSelector")}
		for _, j := range f.jobs {
			if l, _ := dig(j, "metadata", "labels").(map[string]any); sel.Selects(stringMap(l)) {
				items = append(items, j)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"resourceVersion": "7"}, "items": items})
	case r.Method == http.MethodGet && path == "/api/v1/namespaces/ns/pods":
		items := []any{}
		for _, p := range f.pods {
			if r.URL.Query().Get("labelSelector") == "job-name="+dig(p.(map[string]any), "metadata", "labels", "job-name").(string) {
				items = append(items, p)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
	}
}

func cronJob(name string) map[string]any {
	var cj map[string]any
	json.Unmarshal([]byte(`{"metadata":{"name":"`+name+`"},"spec":{"jobTemplate":{
		"metadata":{"labels":{"app":"`+name+`"}},
		"spec":{"backoffLimit":0,"template":{"metadata":{"labels":{"app":"`+name+`"}},"spec":{"restartPolicy":"Never",
			"containers":[{"name":"main","image":"busybox","args":["run"],"env":[{"name":"SECRET_THING","value":"x"}]}]}}}}}}`), &cj)
	return cj
}

func attempt(id string, n int, payload string) dispatch.Attempt {
	spec := dispatch.TaskSpec{ID: id, Class: "sleeper", AttemptTimeout: time.Minute}
	if payload != "" {
		spec.Payload = json.RawMessage(payload)
	}
	return dispatch.Attempt{Task: spec, Number: n, Name: dispatch.AttemptName(id, n)}
}

// TestR6StartTwiceMakesOneJob: a start of the same attempt again (after a
// crash, or a retry) finds the Job and is a success; it makes no second.
func TestR6StartTwiceMakesOneJob(t *testing.T) {
	f, c := newFakeAPI(t)
	f.cronjobs["sleeper"] = cronJob("sleeper")
	s := &Starter{Client: c}
	ctx := context.Background()
	a := attempt("trial-1", 1, `{"env":{"TASK_NOTE":"hello"},"args":["--quick"]}`)
	for range 2 {
		if err := s.Start(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 || len(f.jobs) != 1 {
		t.Fatalf("%d creates, %d jobs", f.creates, len(f.jobs))
	}
	job := f.jobs["trial-1-1"]
	if job == nil {
		t.Fatalf("no job named trial-1-1: %v", f.jobs)
	}
	if got := dig(job, "metadata", "annotations", AnnotationAttempt); got != "trial-1#1" {
		t.Errorf("attempt annotation %v", got)
	}
	if got := dig(job, "metadata", "labels", LabelTask); got != "trial-1" {
		t.Errorf("task label %v", got)
	}
	if got := dig(job, "spec", "template", "metadata", "labels", LabelAttempt); got != "1" {
		t.Errorf("pod's attempt label %v", got)
	}
	c0 := dig(job, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	env, _ := json.Marshal(c0["env"])
	for _, want := range []string{`"SECRET_THING"`, `"TASK_ID","value":"trial-1"`, `"TASK_ATTEMPT","value":"trial-1#1"`, `"TASK_NOTE","value":"hello"`} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env %s lacks %s", env, want)
		}
	}
	if args, _ := json.Marshal(c0["args"]); string(args) != `["run","--quick"]` {
		t.Errorf("args %s", args)
	}

	// Made by an earlier start that never heard back: a success too.
	f.jobs["trial-2-1"] = map[string]any{"metadata": map[string]any{"name": "trial-2-1", "annotations": map[string]any{AnnotationAttempt: "trial-2#1"}}}
	if err := s.Start(ctx, attempt("trial-2", 1, "")); err != nil || f.creates != 1 {
		t.Errorf("an existing job: %v, %d creates", err, f.creates)
	}
	// A Job of that name that is someone else's is not.
	f.jobs["trial-3-1"] = map[string]any{"metadata": map[string]any{"name": "trial-3-1"}}
	if err := s.Start(ctx, attempt("trial-3", 1, "")); err == nil {
		t.Error("a job that is not the attempt's counted as started")
	}
}

// TestR6StartRefuses: a class with no template, and a payload that would
// replace what the template sets, start nothing.
func TestR6StartRefuses(t *testing.T) {
	f, c := newFakeAPI(t)
	f.cronjobs["sleeper"] = cronJob("sleeper")
	s := &Starter{Client: c}
	ctx := context.Background()
	bad := []dispatch.Attempt{
		{Task: dispatch.TaskSpec{ID: "x", Class: "nothing"}, Number: 1, Name: "x#1"},
		attempt("x", 1, `{"env":{"PATH":"/tmp"}}`),
		attempt("x", 1, `{"env":{"TASK_ID":"other"}}`),
		attempt("x", 1, `{"image":"evil"}`),
	}
	for _, a := range bad {
		if err := s.Start(ctx, a); err == nil {
			t.Errorf("started %s with %s", a.Task.Class, a.Task.Payload)
		}
		if err := s.Check(ctx, a.Task); err == nil {
			t.Errorf("checked %s with %s as fine", a.Task.Class, a.Task.Payload)
		}
	}
	if f.creates != 0 {
		t.Errorf("%d jobs made", f.creates)
	}
}

func TestJobName(t *testing.T) {
	seen := map[string]string{}
	for _, c := range []struct {
		id string
		n  int
	}{{"a", 1}, {"a", 11}, {"a-1", 1}, {"A", 1}, {"nightly@2026-11-24T10:00:00Z", 1}, {"nightly@2026-11-24T10:00:01Z", 1}, {strings.Repeat("x", 80), 2}, {"", 1}} {
		name := JobName(c.id, c.n)
		if len(name) > 63 || notName.MatchString(name) || strings.HasPrefix(name, "-") {
			t.Errorf("%q#%d gives an invalid name %q", c.id, c.n, name)
		}
		key := dispatch.AttemptName(c.id, c.n)
		if other, ok := seen[name]; ok {
			t.Errorf("%s and %s share %s", key, other, name)
		}
		seen[name] = key
	}
	if got := JobName("trial-1", 1); got != "trial-1-1" {
		t.Errorf("a valid name is kept as it is: %s", got)
	}
}

type log struct {
	mu     sync.Mutex
	ids    map[string]bool
	events []dispatch.Event
}

// emit appends like the dispatcher's log: an ID it has is refused quietly.
func (l *log) emit(_ context.Context, e dispatch.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = map[string]bool{}
	}
	if !l.ids[e.ID] {
		l.ids[e.ID] = true
		l.events = append(l.events, e)
	}
	return nil
}

func (l *log) all() []dispatch.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dispatch.Event(nil), l.events...)
}

func endedJob(name, uid, outcome string, annotations map[string]any) map[string]any {
	cond := "Complete"
	if outcome == "failed" {
		cond = "Failed"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "uid": uid, "annotations": annotations,
			"ownerReferences": []any{map[string]any{"kind": "CronJob", "name": "nightly"}}},
		"status": map[string]any{"startTime": "2026-11-24T10:00:00Z", "conditions": []any{map[string]any{
			"type": cond, "status": "True", "reason": "BackoffLimitExceeded", "lastTransitionTime": "2026-11-24T10:03:00Z"}}},
	}
}

// TestR6JobEndedOnce: a Job that ended while nothing watched is reported
// when the watcher starts; seen again, by this process or the next, it
// adds nothing; a Job the dispatcher started also finishes its attempt.
func TestR6JobEndedOnce(t *testing.T) {
	f, c := newFakeAPI(t)
	f.jobs["nightly-1"] = endedJob("nightly-1", "u1", "succeeded", nil)
	f.jobs["trial-1-1"] = endedJob("trial-1-1", "u2", "failed", map[string]any{
		AnnotationTask: "trial-1", AnnotationAttempt: "trial-1#1", AnnotationClass: "sleeper"})
	f.jobs["running"] = map[string]any{"metadata": map[string]any{"name": "running", "uid": "u3"}}
	f.pods = []any{map[string]any{"metadata": map[string]any{"name": "trial-1-1-abc", "labels": map[string]any{"job-name": "trial-1-1"},
		"creationTimestamp": "2026-11-24T10:00:01Z"},
		"status": map[string]any{"containerStatuses": []any{map[string]any{"state": map[string]any{"terminated": map[string]any{"exitCode": 3}}}}}}}
	var l log
	ctx := context.Background()
	newWatcher := func() *Watcher {
		return &Watcher{Client: c, Emit: l.emit, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	w := newWatcher()
	for range 2 {
		if _, err := w.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newWatcher().Sync(ctx); err != nil { // the service restarted
		t.Fatal(err)
	}
	events := l.all()
	if len(events) != 3 {
		t.Fatalf("%d events: %+v", len(events), events)
	}
	byID := map[string]dispatch.Event{}
	for _, e := range events {
		byID[e.ID] = e
	}
	var cron, mine JobEnded
	json.Unmarshal(byID["job.ended/u1"].Data, &cron)
	json.Unmarshal(byID["job.ended/u2"].Data, &mine)
	if cron.Outcome != "succeeded" || cron.Template != "nightly" || cron.By != "cronjob" || byID["job.ended/u1"].Subject != "nightly" {
		t.Errorf("the CronJob's run: %+v", cron)
	}
	if mine.Outcome != "failed" || mine.Template != "sleeper" || mine.By != "dispatcher" || mine.ExitCode == nil || *mine.ExitCode != 3 ||
		mine.Started == nil || !mine.Ended.Equal(time.Date(2026, 11, 24, 10, 3, 0, 0, time.UTC)) {
		t.Errorf("the dispatcher's run: %+v", mine)
	}
	fin, ok := byID["task.finished/u2"]
	var data dispatch.Finished
	json.Unmarshal(fin.Data, &data)
	if !ok || fin.Subject != "trial-1" || data.Attempt != "trial-1#1" || data.Outcome != dispatch.OutcomeFailed {
		t.Errorf("task.finished: %+v %+v", fin, data)
	}
}

// TestR6WatchSeesAnEnd: a Job that ends while the watch is open is
// recorded, once, though the watch reports it twice.
func TestR6WatchSeesAnEnd(t *testing.T) {
	f, c := newFakeAPI(t)
	var l log
	w := &Watcher{Client: c, Emit: l.emit, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Retry: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	b, _ := json.Marshal(map[string]any{"type": "MODIFIED", "object": endedJob("nightly-2", "u9", "succeeded", nil)})
	f.watch <- string(b)
	f.watch <- string(b)
	deadline := time.Now().Add(3 * time.Second)
	for len(l.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if events := l.all(); len(events) != 1 || events[0].ID != "job.ended/u9" {
		t.Fatalf("%+v", events)
	}
}

// TestR7ClassModels: a task's ladder is that of the first routine its Job
// runs (CMEM_ROUTINE_MODELS_ROUTINE), else its CronJob's
// CMEM_ROUTINE_MODELS, first model first; none when it sets none.
func TestR7ClassModels(t *testing.T) {
	f, c := newFakeAPI(t)
	f.cronjobs["sleeper"] = cronJob("sleeper")
	cj := cronJob("ladder")
	ct := dig(cj, "spec", "jobTemplate", "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	ct["env"] = append(ct["env"].([]any), map[string]any{"name": "CMEM_ROUTINE_MODELS", "value": "strong, cheap,cheap"})
	f.cronjobs["ladder"] = cj
	own := cronJob("own")
	ct = dig(own, "spec", "jobTemplate", "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	ct["args"] = []any{"tidy-up", "distill"}
	ct["env"] = append(ct["env"].([]any), map[string]any{"name": "CMEM_ROUTINE_MODELS", "value": "cheap"},
		map[string]any{"name": "CMEM_ROUTINE_MODELS_DISTILL", "value": "middle"},
		map[string]any{"name": "CMEM_ROUTINE_MODELS_TIDY_UP", "value": "strong,cheap"})
	f.cronjobs["own"] = own
	s := &Starter{Client: c}
	ctx := context.Background()
	task := func(class, payload string) dispatch.TaskSpec {
		return dispatch.TaskSpec{Class: class, Payload: json.RawMessage(payload)}
	}
	for _, tc := range []struct{ class, payload, want string }{
		{"ladder", "", "strong,cheap,cheap"},
		{"own", "", "strong,cheap"},
		{"own", `{"env":{"TASK_MESSAGE":"m1"}}`, "cheap"},
		{"sleeper", "", ""},
	} {
		got, err := s.Models(ctx, task(tc.class, tc.payload))
		if err != nil || strings.Join(got, ",") != tc.want {
			t.Errorf("%s %s: %v, %v; want %s", tc.class, tc.payload, got, err, tc.want)
		}
	}
	if _, err := s.Models(ctx, task("nothing", "")); err == nil {
		t.Error("a class with no CronJob has a ladder")
	}
}

// TestR6SelectorKeepsTrialsApart: with a selector, a dispatcher records the
// ends of its own Jobs only, and starts nothing from another's CronJob.
func TestR6SelectorKeepsTrialsApart(t *testing.T) {
	f, c := newFakeAPI(t)
	c.Selector = "trial=a"
	mine, theirs := endedJob("a-1", "u1", "succeeded", nil), endedJob("b-1", "u2", "succeeded", nil)
	mine["metadata"].(map[string]any)["labels"] = map[string]any{"trial": "a"}
	theirs["metadata"].(map[string]any)["labels"] = map[string]any{"trial": "b"}
	f.jobs["a-1"], f.jobs["b-1"] = mine, theirs
	var l log
	w := &Watcher{Client: c, Emit: l.emit, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := w.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events := l.all(); len(events) != 1 || events[0].ID != "job.ended/u1" {
		t.Errorf("events: %+v", events)
	}
	f.cronjobs["sleeper"] = cronJob("sleeper")
	if err := (&Starter{Client: c}).Start(context.Background(), attempt("x", 1, "")); err == nil || f.creates != 0 {
		t.Errorf("started from a CronJob outside the selector: %v", err)
	}
	f.cronjobs["sleeper"]["metadata"].(map[string]any)["labels"] = map[string]any{"trial": "a"}
	if err := (&Starter{Client: c}).Start(context.Background(), attempt("x", 1, "")); err != nil {
		t.Errorf("its own CronJob: %v", err)
	}
}
