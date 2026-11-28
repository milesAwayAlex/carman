package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// The watcher on the owner's own machine (ADR new-the-owners-machine-
// attends-to-its-sessions): no /proc, no pod, no window to open.

// aHarness is a real process whose command is the harness's, and how to
// end it.
func aHarness(t *testing.T) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath(psBin); err != nil {
		t.Skip("no ps here")
	}
	// This test binary under the harness's name, sleeping.
	bin := filepath.Join(t.TempDir(), "claude")
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestSleepsAsAHarness$")
	cmd.Env = append(os.Environ(), "SLEEP_AS_A_HARNESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// TestSleepsAsAHarness is the process aHarness starts.
func TestSleepsAsAHarness(t *testing.T) {
	if os.Getenv("SLEEP_AS_A_HARNESS") != "" {
		time.Sleep(5 * time.Minute)
	}
}

// TestR2HostWatcherKnowsAProcessWithoutProc: where there is no /proc, the
// harness's process is found, recorded and judged through ps: there while
// it runs, gone when it has ended.
func TestR2HostWatcherKnowsAProcessWithoutProc(t *testing.T) {
	dir := t.TempDir()
	procRoot = filepath.Join(dir, "no-proc-here")
	t.Cleanup(func() { procRoot = "/proc" })
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	h := aHarness(t)

	pid, start, ok := findHarness(h.Process.Pid)
	if !ok || pid != h.Process.Pid || start == "" || strings.ContainsAny(start, " \n") {
		t.Fatalf("the harness is not found through ps: %d %q %v", pid, start, ok)
	}
	os.MkdirAll(harnessDir(), 0o700)
	os.WriteFile(filepath.Join(harnessDir(), "s-1"), []byte(itoa(pid)+" "+start+"\n"), 0o600)
	os.WriteFile(filepath.Join(harnessDir(), "s-other"), []byte(itoa(pid)+" Mon_Jan_1_00:00:00_2001\n"), 0o600)
	if known, alive := harnessAlive("s-1"); !known || !alive {
		t.Errorf("a harness that runs: known %v, alive %v", known, alive)
	}
	if known, alive := harnessAlive("s-other"); !known || alive {
		t.Errorf("another process with the recorded id: known %v, alive %v", known, alive)
	}
	gone := harnessGoneFunc(h.Process.Pid)
	if gone() {
		t.Error("the listener takes a harness that runs for gone")
	}
	h.Process.Kill()
	h.Wait()
	if _, alive := harnessAlive("s-1"); alive {
		t.Error("a harness that has ended is taken for there")
	}
	if !gone() {
		t.Error("the listener takes a harness that has ended for there")
	}
}

// TestR2HostWatcherSaysGoneAndShowsANotice: on the owner's machine the
// watcher says a session ended whose harness has gone, opens nothing, and
// for a name that messages wait for runs the machine's command once, saying
// so on the messages; again only when another message comes.
func TestR2HostWatcherSaysGoneAndShowsANotice(t *testing.T) {
	dir := t.TempDir()
	procRoot = filepath.Join(dir, "no-proc-here")
	t.Cleanup(func() { procRoot = "/proc" })
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	h := aHarness(t)
	_, start, _ := findHarness(h.Process.Pid)
	os.MkdirAll(harnessDir(), 0o700)
	os.WriteFile(filepath.Join(harnessDir(), "h-live"), []byte(itoa(h.Process.Pid)+" "+start+"\n"), 0o600)
	os.WriteFile(filepath.Join(harnessDir(), "h-dead"), []byte("2147483000 Mon_Jan_1_00:00:00_2001\n"), 0o600)

	shown := filepath.Join(dir, "shown")
	notify := filepath.Join(dir, "notify")
	os.WriteFile(notify, []byte("#!/bin/sh\necho \"$1|$2|$#|$DISPATCHER_NOTIFY_CWD\" >> "+shown+"\n"), 0o755)
	opened := filepath.Join(dir, "opened")
	now := time.Now()
	f := &fakeAttend{attend: message.Attend{
		Running: []message.Session{{ID: "h-live", Name: "proj"}, {ID: "h-dead", Name: "old"}, {ID: "h-unknown", Name: "none"}},
		Waiting: []message.Awaited{{Name: "notes", Session: "h-9", Cwd: "/work/notes", Messages: []string{"m-1"}}},
	}}
	a := newAttender(t, f, []string{"sh", "-c", "touch " + opened}, filepath.Join(dir, "attend.json"), &now)
	a.host, a.pod, a.notify = true, "host", []string{notify}
	for range 3 {
		if err := a.pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.gone, ",") != "h-dead" {
		t.Errorf("said gone: %v, want only h-dead", f.gone)
	}
	if _, err := os.Stat(opened); err == nil {
		t.Error("the watcher opened a session on the owner's machine")
	}
	b, _ := os.ReadFile(shown)
	if string(b) != "notes|1|2|/work/notes\n" {
		t.Errorf("the machine's command was run as: %q", b)
	}
	if len(f.holds) != 1 || f.holds[0].Message != "m-1" || !strings.Contains(f.holds[0].Reason, "a notice was shown on the owner's machine") {
		t.Errorf("what the message says: %+v", f.holds)
	}
	// Another message for the name: shown again, once.
	f.mu.Lock()
	f.attend.Waiting[0].Messages = []string{"m-1", "m-2"}
	f.mu.Unlock()
	for range 2 {
		a.pass(context.Background())
	}
	if b, _ := os.ReadFile(shown); string(b) != "notes|1|2|/work/notes\nnotes|2|2|/work/notes\n" {
		t.Errorf("after another message: %q", b)
	}
	// With no command set nothing is run and nothing is claimed.
	f.holds = nil
	a.notify = nil
	f.mu.Lock()
	f.attend.Waiting[0].Messages = []string{"m-3"}
	f.mu.Unlock()
	a.pass(context.Background())
	if len(f.holds) != 0 {
		t.Errorf("with no command, the message says a notice was shown: %+v", f.holds)
	}
}

// TestR2HostWatcherGivesNoVerdictWhenPsFails: a ps that does not run, or
// answers in another shape, says nothing of a process that is there: the
// session is not ended, and its listener stays.
func TestR2HostWatcherGivesNoVerdictWhenPsFails(t *testing.T) {
	dir := t.TempDir()
	procRoot = filepath.Join(dir, "no-proc-here")
	t.Cleanup(func() { procRoot = "/proc" })
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	h := aHarness(t)
	_, start, _ := findHarness(h.Process.Pid)
	gone := harnessGoneFunc(h.Process.Pid)
	os.MkdirAll(harnessDir(), 0o700)
	os.WriteFile(filepath.Join(harnessDir(), "h-live"), []byte(itoa(h.Process.Pid)+" "+start+"\n"), 0o600)
	os.WriteFile(filepath.Join(harnessDir(), "h-dead"), []byte("2147483000 Mon_Jan_1_00:00:00_2001\n"), 0o600)

	broken := filepath.Join(dir, "ps")
	real := psBin
	t.Cleanup(func() { psBin = real })
	for what, script := range map[string]string{"fails": "#!/bin/sh\nexit 1\n", "another shape": "#!/bin/sh\necho 12 unexpected\n"} {
		os.WriteFile(broken, []byte(script), 0o755)
		psBin = broken
		if known, alive := harnessAlive("h-live"); known || alive {
			t.Errorf("ps %s, the process is there: known %v, alive %v; want no verdict", what, known, alive)
		}
		if known, alive := harnessAlive("h-dead"); !known || alive {
			t.Errorf("ps %s, no process of that id: known %v, alive %v; want gone", what, known, alive)
		}
		for range 20 {
			if gone() {
				t.Fatalf("ps %s: the listener takes a harness that runs for gone", what)
			}
		}
		now := time.Now()
		f := &fakeAttend{attend: message.Attend{Running: []message.Session{{ID: "h-live", Name: "proj"}}}}
		a := newAttender(t, f, nil, filepath.Join(dir, "attend.json"), &now)
		a.host, a.pod = true, "host"
		for range 4 {
			a.pass(context.Background())
		}
		if len(f.gone) != 0 {
			t.Errorf("ps %s: a session that runs was said to have ended: %v", what, f.gone)
		}
	}
}

// TestR2HostWatcherTriesAFailedNoticeAgain: a notice command that fails
// says nothing on the message and is run again after a while.
func TestR2HostWatcherTriesAFailedNoticeAgain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISPATCHER_HARNESS_DIR", filepath.Join(dir, "harness"))
	ran := filepath.Join(dir, "ran")
	ok := filepath.Join(dir, "ok")
	notify := filepath.Join(dir, "notify")
	os.WriteFile(notify, []byte("#!/bin/sh\necho x >> "+ran+"\ntest -e "+ok+"\n"), 0o755)
	now := time.Now()
	f := &fakeAttend{attend: message.Attend{Waiting: []message.Awaited{{Name: "notes", Session: "h-9", Messages: []string{"m-1"}}}}}
	a := newAttender(t, f, nil, filepath.Join(dir, "attend.json"), &now)
	a.host, a.pod, a.notify = true, "host", []string{notify}
	runs := func() int { b, _ := os.ReadFile(ran); return strings.Count(string(b), "x") }
	for range 3 {
		a.pass(context.Background())
	}
	if runs() != 1 || len(f.holds) != 0 {
		t.Fatalf("a failing command: run %d times, the message says %+v", runs(), f.holds)
	}
	now = now.Add(firstBackoff + time.Second)
	os.WriteFile(ok, nil, 0o644)
	for range 3 {
		a.pass(context.Background())
	}
	if runs() != 2 || len(f.holds) != 1 {
		t.Errorf("after a while: run %d times, the message says %+v", runs(), f.holds)
	}
	now = now.Add(time.Hour)
	a.pass(context.Background())
	if runs() != 2 {
		t.Errorf("shown, and run again: %d", runs())
	}
}
