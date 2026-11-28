package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"carman/internal/dispatch/message"
)

// The watcher on the owner's own machine (ADR new-the-owners-machine-attends-
// to-its-sessions): `dispatcher attend` with the place's token. It says when
// a session's harness has gone; it opens no terminal and types nothing. For
// a message that waits for a session that is not running there, it runs the
// machine's own command, if one is set, so that a person is shown.

// psBin is the ps that says a process's parent, start and command where
// there is no /proc.
var psBin = func() string {
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}()

// noProc says whether processes are asked of ps: no /proc here.
func noProc() bool {
	_, err := os.Stat(procRoot)
	return err != nil
}

// psStat is a process's parent, start time and command, from ps. The start
// time is ps's own words for it (five fields), compared as written.
func psStat(pid int) (ppid int, start, command string, err error) {
	cmd := exec.Command(psBin, "-o", "ppid=", "-o", "lstart=", "-o", "command=", "-p", strconv.Itoa(pid))
	// The start time is written in the caller's zone and language: one of
	// each, so that the hook's record and the watcher's reading agree.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return 0, "", "", err
	}
	f := strings.Fields(string(out))
	if len(f) < 7 {
		return 0, "", "", errors.New("short ps")
	}
	if ppid, err = strconv.Atoi(f[0]); err != nil {
		return 0, "", "", err
	}
	// As one word: the record is "PID START".
	return ppid, strings.Join(f[1:6], "_"), strings.Join(f[6:], " "), nil
}

// psAlive says, cheaply, whether a process is there at all.
func psAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// stateDir is where the watcher and the hooks keep what they share on the
// owner's machine.
func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "dispatcher")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "dispatcher")
}

// noticed is what was done for a name's waiting messages on the machine.
const noticeKey = "host-notice"

// notifyHost runs the machine's command for a name that messages wait for,
// once for a set of messages, and says on the messages that it did.
func (a *attender) notifyHost(ctx context.Context, w message.Awaited) {
	t := a.tries[w.Name]
	if t != nil && !subset(w.Messages, t.Messages) {
		t = nil // another message came: shown again
	}
	if t != nil && (t.Outcome == "noticed" || a.now().Sub(t.Last) < backoff(t.Tries)) {
		return
	}
	if t == nil {
		t = &attempt{}
		a.tries[w.Name] = t
	}
	t.Messages, t.Last, t.Outcome = w.Messages, a.now(), "noticed"
	t.Tries++
	if len(a.notify) == 0 {
		return
	}
	first := ""
	var st message.Status
	if err := a.c.Do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(w.Messages[0]), nil, nil, &st); err == nil && st.Message.From != "" {
		line, _, _ := strings.Cut(strings.TrimSpace(st.Message.Body), "\n")
		if r := []rune(line); len(r) > 200 {
			line = string(r[:200]) + "…"
		}
		first = st.Message.From + ": " + line
	}
	// A message's words go in the environment, not among the arguments,
	// which any process on the machine can read.
	cmd := exec.CommandContext(ctx, a.notify[0], append(append([]string{}, a.notify[1:]...), w.Name, strconv.Itoa(len(w.Messages)))...)
	cmd.Env = append(os.Environ(), "DISPATCHER_NOTIFY_FIRST="+first, "DISPATCHER_NOTIFY_CWD="+w.Cwd, "DISPATCHER_NOTIFY_SESSION="+w.Session)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Not shown: tried again after a while, as an open is in a pod.
		t.Outcome = "failed"
		a.logf("%s: the machine's notice command failed (try %d): %v: %s", w.Name, t.Tries, err, strings.TrimSpace(string(out)))
		return
	}
	a.logf("%s: %d message(s) wait; the machine's notice command was run", w.Name, len(w.Messages))
	a.hold(ctx, w.Messages, noticeKey, fmt.Sprintf("a notice was shown on the owner's machine (%s) at %s that it waits for a session called %s; "+
		"nothing opens a session there but a person", a.pod, a.now().UTC().Format(time.RFC3339), w.Name))
}

// pruneRecords removes the records of harnesses long gone: on the owner's
// machine nothing else clears them.
func pruneRecords(now time.Time) {
	entries, _ := os.ReadDir(harnessDir())
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || strings.HasPrefix(e.Name(), "attend") || now.Sub(info.ModTime()) < 30*24*time.Hour {
			continue
		}
		if _, alive := harnessAlive(e.Name()); !alive {
			os.Remove(filepath.Join(harnessDir(), e.Name()))
		}
	}
}
