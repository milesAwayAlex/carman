package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestR2HookOff: with CMEM_HOOK=off (or DISPATCHER_HOOK=off) the
// dispatcher's hook does not call the dispatcher, so a harness an agent
// starts for a trial registers no session and takes no message.
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
}
