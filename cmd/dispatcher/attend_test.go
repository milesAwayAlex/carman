package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// The watcher of a standing agent's pod (ADR 0053), against a fake
// dispatcher, a fake /proc and a fake agent-open.

type fakeAttend struct {
	mu     sync.Mutex
	attend message.Attend
	down   bool
	gone   []string
	holds  []api.HoldRequest
}

func (f *fakeAttend) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"restarting"}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/attend":
			json.NewEncoder(w).Encode(f.attend)
			return
		case "/v1/attend/gone":
			var g api.GoneRequest
			json.Unmarshal(b, &g)
			f.gone = append(f.gone, g.Session)
		case "/v1/attend/hold":
			var h api.HoldRequest
			json.Unmarshal(b, &h)
			f.holds = append(f.holds, h)
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAttend) holdKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, h := range f.holds {
		keys = append(keys, h.Message+":"+h.Key)
	}
	return keys
}

// fakeProc writes a process into a fake /proc.
func fakeProc(t *testing.T, root string, pid, ppid int, start, cmdline string) {
	t.Helper()
	dir := filepath.Join(root, itoa(pid))
	os.MkdirAll(dir, 0o755)
	// pid (comm) state ppid ... with the start time as field 22.
	f := []string{"S", itoa(ppid)}
	for len(f) < 19 {
		f = append(f, "0")
	}
	f = append(f, start, "0")
	os.WriteFile(filepath.Join(dir, "stat"), []byte(itoa(pid)+" (a b) "+strings.Join(f, " ")+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.ReplaceAll(cmdline, " ", "\x00")+"\x00"), 0o644)
}

func itoa(i int) string { return strconv.Itoa(i) }

// fakeOpen is an agent-open that records how it was called and exits with
// what its file says.
func fakeOpen(t *testing.T, dir string) (cmd []string, calls func() []string, exit func(int)) {
	t.Helper()
	script := filepath.Join(dir, "agent-open")
	log := filepath.Join(dir, "calls")
	code := filepath.Join(dir, "exit")
	os.WriteFile(code, []byte("0"), 0o644)
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\necho opened\nexit $(cat "+code+")\n"), 0o755)
	return []string{script, "--reopen"}, func() []string {
			b, _ := os.ReadFile(log)
			return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "--reopen ", ""))
		}, func(n int) {
			os.WriteFile(code, []byte(itoa(n)), 0o644)
		}
}

func newAttender(t *testing.T, f *fakeAttend, open []string, state string, now *time.Time) *attender {
	srv := f.server(t)
	a := &attender{c: &api.Client{URL: srv.URL, Token: "pod"}, open: open, state: state, pod: "pod-d",
		out: io.Discard, now: func() time.Time { return *now }}
	a.load()
	return a
}

// TestR2AttendHarnessGone: a session believed running whose recorded
// harness process is gone is said to have ended, after two looks; one
// whose process is there, or that has no record, is not.
func TestR2AttendHarnessGone(t *testing.T) {
	dir := t.TempDir()
	procRoot = filepath.Join(dir, "proc")
	t.Cleanup(func() { procRoot = "/proc" })
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	os.MkdirAll(harnessDir(), 0o700)
	fakeProc(t, procRoot, 200, 1, "777", "/home/a/.local/bin/claude --name vpn")
	os.WriteFile(filepath.Join(harnessDir(), "s-alive"), []byte("200 777\n"), 0o600)
	os.WriteFile(filepath.Join(harnessDir(), "s-dead"), []byte("300 555\n"), 0o600)
	// The same pid, a later process: not the harness.
	fakeProc(t, procRoot, 400, 1, "900", "bash")
	os.WriteFile(filepath.Join(harnessDir(), "s-reused"), []byte("400 111\n"), 0o600)

	f := &fakeAttend{attend: message.Attend{Running: []message.Session{{ID: "s-alive"}, {ID: "s-dead"}, {ID: "s-reused"}, {ID: "s-unknown"}}}}
	now := time.Now()
	a := newAttender(t, f, []string{"true"}, "", &now)
	if err := a.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.gone) != 0 {
		t.Errorf("ended at the first look: %v", f.gone)
	}
	if err := a.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.gone, ",") != "s-dead,s-reused" {
		t.Errorf("ended: %v", f.gone)
	}
	// The record stays: a session the dispatcher comes to believe runs
	// again (a listener its harness left took a message, seen 2026-11-27)
	// is judged again, and said again to have ended.
	if _, err := os.Stat(filepath.Join(harnessDir(), "s-dead")); err != nil {
		t.Error("the record of a harness that has gone is removed, so a session believed running again is never judged")
	}
	f.attend.Running = []message.Session{{ID: "s-dead"}}
	f.gone = nil
	for range 2 {
		if err := a.pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.gone, ",") != "s-dead" {
		t.Errorf("believed running again, then ended: %v", f.gone)
	}
}

// TestR2ListenerLeavesWithItsHarness: the idle listener asks whether the
// harness above it is there, so that one left behind by a killed harness
// takes no message (ADR 0053).
func TestR2ListenerLeavesWithItsHarness(t *testing.T) {
	dir := t.TempDir()
	procRoot = filepath.Join(dir, "proc")
	t.Cleanup(func() { procRoot = "/proc" })
	fakeProc(t, procRoot, 200, 1, "777", "/home/a/.local/bin/claude --name vpn")
	fakeProc(t, procRoot, 210, 200, "780", "/bin/sh -c dispatcher hook claude-code --wait 335h")
	gone := harnessGoneFunc(210)
	if gone() {
		t.Error("a harness that is there is taken for gone")
	}
	os.RemoveAll(filepath.Join(procRoot, "200"))
	if !gone() {
		t.Error("a harness that has gone is taken for there")
	}
	fakeProc(t, procRoot, 200, 1, "999", "bash")
	if !gone() {
		t.Error("another process with the harness's id is taken for the harness")
	}
	if harnessGoneFunc(12345)() {
		t.Error("with no harness found, the listener would never listen")
	}
}

// TestR2AttendFindsItsHarness: the hook's record is the nearest ancestor
// that is the harness, with its start time.
func TestR2AttendFindsItsHarness(t *testing.T) {
	procRoot = filepath.Join(t.TempDir(), "proc")
	t.Cleanup(func() { procRoot = "/proc" })
	fakeProc(t, procRoot, 200, 1, "777", "/usr/bin/node /opt/claude-code/cli.js --name vpn")
	fakeProc(t, procRoot, 300, 200, "800", "/bin/sh -c dispatcher hook claude-code")
	fakeProc(t, procRoot, 400, 300, "801", "dispatcher hook claude-code")
	if pid, start, ok := findHarness(400); !ok || pid != 200 || start != "777" {
		t.Errorf("found %d %s %v", pid, start, ok)
	}
	fakeProc(t, procRoot, 500, 1, "1", "bash -l")
	if _, _, ok := findHarness(500); ok {
		t.Error("a shell is taken for the harness")
	}
}

// TestR2AttendOpensOnce: a name messages wait for is opened once; while it
// has not taken them it is opened again only after a while, a few times,
// and the sender is told; another message asks again; the watcher's
// attempts outlive it, and a dispatcher that does not answer is waited for.
func TestR2AttendOpensOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	open, calls, _ := fakeOpen(t, dir)
	f := &fakeAttend{attend: message.Attend{Waiting: []message.Awaited{{Name: "vpn", Session: "s-old", Messages: []string{"m1"}}}}}
	now := time.Now()
	state := filepath.Join(dir, "attend.json")
	a := newAttender(t, f, open, state, &now)
	ctx := context.Background()
	a.pass(ctx)
	a.pass(ctx)
	if got := calls(); strings.Join(got, ",") != "vpn" {
		t.Fatalf("opened: %v", got)
	}
	// The watcher starting again does not open it again at once.
	a = newAttender(t, f, open, state, &now)
	a.pass(ctx)
	if got := calls(); len(got) != 1 {
		t.Errorf("opened again by a watcher started again: %v", got)
	}
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	if err := a.pass(ctx); err == nil {
		t.Error("a dispatcher that does not answer is not an error of the pass")
	}
	f.mu.Lock()
	f.down = false
	f.mu.Unlock()
	for range 12 {
		now = now.Add(maxBackoff)
		a.pass(ctx)
	}
	if got := calls(); len(got) != maxTries {
		t.Errorf("opened %d times; at most %d", len(got), maxTries)
	}
	keys := strings.Join(f.holdKeys(), " ")
	if !strings.Contains(keys, "m1:failed-1") || !strings.Contains(keys, "m1:gave-up") {
		t.Errorf("the sender is not told: %s", keys)
	}
	f.attend.Waiting[0].Messages = []string{"m1", "m2"}
	a.pass(ctx)
	if got := calls(); len(got) != maxTries+1 {
		t.Errorf("another message does not ask again: %v", got)
	}
	f.attend.Waiting = nil
	a.pass(ctx)
	if len(a.tries) != 0 {
		t.Errorf("attempts kept for a name nothing waits for: %v", a.tries)
	}
}

// TestR2AttendNotRemembered: a name the pod does not remember is not
// opened, and the sender is told why; nor is it tried at every look.
func TestR2AttendNotRemembered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	open, calls, exit := fakeOpen(t, dir)
	exit(reopenNotRemembered)
	f := &fakeAttend{attend: message.Attend{Waiting: []message.Awaited{{Name: "old", Messages: []string{"m1", "m2"}}}}}
	now := time.Now()
	a := newAttender(t, f, open, "", &now)
	for range 5 {
		a.pass(context.Background())
		now = now.Add(time.Minute)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("asked agent-open %d times", len(got))
	}
	if keys := strings.Join(f.holdKeys(), " "); keys != "m1:not-remembered m2:not-remembered" {
		t.Errorf("the senders are told: %s", keys)
	}
}
