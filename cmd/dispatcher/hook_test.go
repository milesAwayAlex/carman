package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestR2HookOff: with CMEM_HOOK=off (or DISPATCHER_HOOK=off) the
// dispatcher's hook does not call the dispatcher, so a harness an agent
// starts for a trial registers no session and takes no message; with
// DISPATCHER_HOOK=on it does, whatever CMEM_HOOK says.
func TestR2HookOff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("t"), 0o600)
	t.Setenv("DISPATCHER_URL", srv.URL)
	t.Setenv("DISPATCHER_IDENTITY", token)
	run := func() {
		r, w, _ := os.Pipe()
		w.WriteString(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","cwd":"/"}`)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old }()
		if err := hook(context.Background(), map[string][]string{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, env := range []string{"CMEM_HOOK", "DISPATCHER_HOOK"} {
		t.Setenv(env, "off")
		run()
		t.Setenv(env, "")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the hook called the dispatcher %d times while off", n)
	}
	run()
	if calls.Load() == 0 {
		t.Error("the hook, on, did not call the dispatcher: the test proves nothing")
	}
	// The dreamer: memory's hook off, the dispatcher's on.
	calls.Store(0)
	t.Setenv("CMEM_HOOK", "off")
	t.Setenv("DISPATCHER_HOOK", "on")
	run()
	if calls.Load() == 0 {
		t.Error("with DISPATCHER_HOOK=on beside CMEM_HOOK=off the hook did not call the dispatcher")
	}
}

// TestR2HookWithoutPod: on the owner's machine (ADR 0049) there is no pod
// token: with DISPATCHER_PLACE the hook uses DISPATCHER_TOKEN, names the
// session for its directory, registers it as interactive, puts its id in
// the shell, and says where it is; without it, the hook does nothing.
func TestR2HookWithoutPod(t *testing.T) {
	var auth, presence, takes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/sessions" {
			b, _ := io.ReadAll(r.Body)
			presence = append(presence, string(b))
		}
		if r.URL.Path == "/v1/inbox/take" {
			b, _ := io.ReadAll(r.Body)
			takes = append(takes, string(b))
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	t.Setenv("DISPATCHER_URL", srv.URL)
	t.Setenv("DISPATCHER_TOKEN", "host-token")
	t.Setenv("DISPATCHER_IDENTITY", filepath.Join(dir, "no-such-token"))
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv("AGENT_NAME", "")
	t.Setenv("DISPATCHER_SESSION_NAME", "")
	t.Setenv("DISPATCHER_SESSION_KIND", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "1")
	run := func(event string) string {
		r, w, _ := os.Pipe()
		w.WriteString(`{"session_id":"s1","hook_event_name":"` + event + `","cwd":"/work/src/my proj"}`)
		w.Close()
		out, ow, _ := os.Pipe()
		oldIn, oldOut := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = r, ow
		err := hook(context.Background(), map[string][]string{})
		os.Stdin, os.Stdout = oldIn, oldOut
		ow.Close()
		b, _ := io.ReadAll(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Setenv("DISPATCHER_PLACE", "")
	run("SessionStart")
	if len(auth) != 0 {
		t.Fatalf("no pod token and no place: the hook called the dispatcher %d times", len(auth))
	}
	t.Setenv("DISPATCHER_PLACE", "host")
	out := run("SessionStart")
	if len(auth) == 0 || auth[0] != "Bearer host-token" {
		t.Fatalf("the hook's token: %v", auth)
	}
	if len(presence) != 1 || !strings.Contains(presence[0], `"name":"my-proj"`) || !strings.Contains(presence[0], `"kind":"interactive"`) || !strings.Contains(presence[0], `"state":"idle"`) {
		t.Errorf("what the session says of itself: %v", presence)
	}
	if !strings.Contains(out, "on the owner's machine (host)") || !strings.Contains(out, "host/my-proj") {
		t.Errorf("what the session is told: %s", out)
	}
	if b, _ := os.ReadFile(envFile); !strings.Contains(string(b), "DISPATCHER_SESSION=s1") {
		t.Errorf("the shell is not told its session: %q", b)
	}
	// An interactive session's start is no turn: what waits is left for its
	// listener, whose wake starts one (ADR 0053).
	if len(takes) != 0 {
		t.Errorf("an interactive session took its messages at its start: %v", takes)
	}
	// A headless run there (claude -p) is headless: an idle listener would
	// hold the run open, since the harness waits for it.
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	presence = nil
	run("SessionStart")
	if len(presence) != 1 || !strings.Contains(presence[0], `"kind":"headless"`) {
		t.Errorf("a headless run on the owner's machine says of itself: %v", presence)
	}
	if len(takes) != 1 || !strings.Contains(takes[0], `"start"`) {
		t.Errorf("a headless run, whose start is a turn, takes what waits at its start: %v", takes)
	}
	calls := len(auth)
	r, w, _ := os.Pipe()
	w.WriteString(`{"session_id":"s1","hook_event_name":"Stop","cwd":"/work/src/my proj"}`)
	w.Close()
	oldIn := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldIn }()
	done := make(chan error, 1)
	go func() { done <- hook(context.Background(), map[string][]string{"wait": {"335h"}}) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the idle listener of a headless run did not return at once")
	}
	if len(auth) != calls {
		t.Errorf("the idle listener of a headless run called the dispatcher")
	}
}

// TestR2SignalledListenerEnds: an idle listener that is told to end (a
// signal cancels its context) ends. One that went on asked nothing, each
// call failing at once, while its harness took it for a listener: its
// session heard no message until its next turn.
func TestR2SignalledListenerEnds(t *testing.T) {
	asked := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/inbox/take" {
			io.Copy(io.Discard, r.Body) // the server sees a caller leave only once the body is read
			asked <- struct{}{}
			<-r.Context().Done() // a long poll: held until the listener leaves
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	t.Setenv("DISPATCHER_URL", srv.URL)
	t.Setenv("DISPATCHER_TOKEN", "host-token")
	t.Setenv("DISPATCHER_PLACE", "host")
	t.Setenv("DISPATCHER_IDENTITY", filepath.Join(t.TempDir(), "none"))
	t.Setenv("DISPATCHER_HOOK", "on")
	t.Setenv("DISPATCHER_SESSION_KIND", "interactive")
	r, w, _ := os.Pipe()
	w.WriteString(`{"session_id":"s1","hook_event_name":"Stop","cwd":"/work/src/proj"}`)
	w.Close()
	oldIn := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldIn }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- hook(ctx, map[string][]string{"wait": {"335h"}}) }()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not ask the dispatcher")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a listener told to end went on")
	}
}

// TestR2ClientKnowsItsSession: a command in a session's shell sends as that
// session: by the id the hook put in the shell, or, where the shell did not
// take it, by the harness's own variable; a session named on the command
// line comes first.
func TestR2ClientKnowsItsSession(t *testing.T) {
	t.Setenv("DISPATCHER_SESSION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "from-harness")
	if got := session(map[string][]string{}); got != "from-harness" {
		t.Errorf("with only the harness's variable: %q", got)
	}
	t.Setenv("DISPATCHER_SESSION", "from-hook")
	if got := session(map[string][]string{}); got != "from-hook" {
		t.Errorf("with the hook's variable: %q", got)
	}
	if got := session(map[string][]string{"session": {"given"}}); got != "given" {
		t.Errorf("named on the command line: %q", got)
	}
}
