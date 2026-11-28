package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// `dispatcher attend` (ADR 0053) runs beside a standing agent's sessions,
// in its pod, with the pod's own token. It tells the dispatcher when a
// session's harness has gone without a word, and opens again, with
// agent-open, a remembered session that a message waits for.

// A session's harness is known by the record its SessionStart hook writes:
// the harness's process id and start time, under harnessDir()/SESSION.

func harnessDir() string {
	if d := os.Getenv("DISPATCHER_HARNESS_DIR"); d != "" {
		return d
	}
	if os.Getenv("DISPATCHER_PLACE") != "" {
		return filepath.Join(stateDir(), "harness") // the owner's machine
	}
	return "/tmp/agent-harness"
}

var procRoot = "/proc"

// procStat is a process's parent and start time, from /proc/PID/stat.
func procStat(pid int) (ppid int, start string, err error) {
	if noProc() {
		ppid, start, _, err = psStat(pid)
		return ppid, start, err
	}
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", err
	}
	// The command's name, in parentheses, may hold spaces.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, "", errors.New("no stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, "", errors.New("short stat")
	}
	ppid, _ = strconv.Atoi(f[1])
	return ppid, f[19], nil
}

// isHarness says whether a command line is the harness's.
func isHarness(cmdline []byte) bool {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if filepath.Base(args[0]) == "claude" || strings.Contains(args[0], "/claude/versions/") {
		return true
	}
	if filepath.Base(args[0]) == "node" {
		for _, a := range args[1:] {
			if strings.Contains(a, "claude") {
				return true
			}
		}
	}
	return false
}

// findHarness is the harness that runs a hook: the nearest ancestor of
// pid whose command is the harness's.
func findHarness(pid int) (int, string, bool) {
	for range 8 {
		if pid <= 1 {
			break
		}
		var cmd []byte
		var ppid int
		var start string
		var err error
		if noProc() {
			var words string
			ppid, start, words, err = psStat(pid)
			cmd = []byte(strings.Join(strings.Fields(words), "\x00"))
		} else if cmd, err = os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline")); err == nil {
			ppid, start, err = procStat(pid)
		}
		if err != nil {
			break
		}
		if isHarness(cmd) {
			return pid, start, true
		}
		pid = ppid
	}
	return 0, "", false
}

// recordHarness writes which process is the session's harness, for
// attend to find out when it has gone. Best effort: no record, no verdict.
func recordHarness(session string) {
	pid, start, ok := findHarness(os.Getppid())
	if !ok || session == "" || strings.ContainsAny(session, "/.") {
		return
	}
	dir := harnessDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp := filepath.Join(dir, "."+session)
	if os.WriteFile(tmp, fmt.Appendf(nil, "%d %s\n", pid, start), 0o600) == nil {
		os.Rename(tmp, filepath.Join(dir, session))
	}
}

// harnessGoneFunc says, each time it is asked, whether the harness that is
// an ancestor of pid has gone; never, where none is found.
func harnessGoneFunc(pid int) func() bool {
	hp, start, ok := findHarness(pid)
	asked := 0
	return func() bool {
		if !ok {
			return false
		}
		if noProc() {
			// ps is a process each time: asked whether it is the same
			// process only now and then, and whether one is there each time.
			if asked++; asked%15 != 0 {
				return !psAlive(hp)
			}
		}
		_, now, err := procStat(hp)
		if err != nil && noProc() {
			return !psAlive(hp) // ps did not answer: gone only if no process is there
		}
		return err != nil || now != start
	}
}

// harnessAlive says whether a session's harness is known here, and if so
// whether its process is still there (the same process: same start time).
func harnessAlive(session string) (known, alive bool) {
	if strings.ContainsAny(session, "/") {
		return false, false
	}
	b, err := os.ReadFile(filepath.Join(harnessDir(), session))
	if err != nil {
		return false, false
	}
	var pid int
	var start string
	if _, err := fmt.Sscan(string(b), &pid, &start); err != nil || pid <= 1 {
		return false, false
	}
	_, now, err := procStat(pid)
	if err != nil && noProc() && psAlive(pid) {
		// ps did not answer, and a process of that id is there: no verdict.
		return false, false
	}
	return true, err == nil && now == start
}

// What agent-open --reopen's exit says.
const (
	reopenNotRemembered = 3
	reopenWindowBusy    = 4
)

// attempt is what was done for a name that messages wait for.
type attempt struct {
	Tries    int       `json:"tries"`
	Last     time.Time `json:"last"`
	Outcome  string    `json:"outcome"` // opened, not-remembered, busy, failed
	Messages []string  `json:"messages"`
}

// attender is the watcher's state between its passes.
type attender struct {
	c     *api.Client
	open  []string // the command that opens a remembered session: agent-open --reopen
	state string   // where attempts are kept, to outlive the watcher
	pod   string
	out   io.Writer
	now   func() time.Time
	gone  map[string]int // passes a session's harness was found gone in a row
	tries map[string]*attempt
	// limited is, by session, the stop at the plan's limit last told.
	limited map[string]string
	// host: this is the owner's machine, not a pod; notify is its command
	// for a message that waits (attend_host.go).
	host   bool
	notify []string
	down   bool // the dispatcher did not answer at the last pass
}

const (
	// goneAfter is how many passes in a row a harness must be gone: a
	// harness and its hooks starting race a pass.
	goneAfter = 2
	// maxTries opens of a session that does not take its messages, and
	// the watcher stops until another message comes.
	maxTries     = 4
	firstBackoff = 2 * time.Minute
	maxBackoff   = time.Hour
)

func backoff(tries int) time.Duration {
	d := firstBackoff
	for i := 1; i < tries && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (a *attender) logf(format string, args ...any) {
	fmt.Fprintf(a.out, "%s attend: %s\n", a.now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (a *attender) load() {
	a.gone, a.tries, a.limited = map[string]int{}, map[string]*attempt{}, map[string]string{}
	if b, err := os.ReadFile(a.state); err == nil {
		_ = json.Unmarshal(b, &a.tries)
	}
}

func (a *attender) save() {
	if a.state == "" {
		return
	}
	b, _ := json.Marshal(a.tries)
	tmp := a.state + ".new"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, a.state)
	}
}

func (a *attender) hold(ctx context.Context, ids []string, key, reason string) {
	for _, id := range ids {
		if err := a.c.Do(ctx, http.MethodPost, "/v1/attend/hold", nil, api.HoldRequest{Message: id, Key: key, Reason: reason}, nil); err != nil {
			a.logf("could not say why message %s waits: %v", id, err)
		}
	}
}

// pass is one look: harnesses that have gone, then names to open.
func (a *attender) pass(ctx context.Context) error {
	var at message.Attend
	if err := a.c.Do(ctx, http.MethodGet, "/v1/attend", nil, nil, &at); err != nil {
		return err
	}
	believed := map[string]bool{}
	for _, x := range at.Running {
		believed[x.ID] = true
		known, alive := harnessAlive(x.ID)
		if alive && !a.host {
			a.limit(ctx, x.ID, or(x.Name, "no name"))
		}
		if !known || alive {
			// No record (a harness started before this, or not by the
			// image's hooks): no verdict, as before.
			delete(a.gone, x.ID)
			continue
		}
		a.gone[x.ID]++
		if a.gone[x.ID] < goneAfter {
			continue
		}
		why := "its process is not there"
		if err := a.c.Do(ctx, http.MethodPost, "/v1/attend/gone", nil, api.GoneRequest{Session: x.ID, Why: why}, nil); err != nil {
			a.logf("could not say session %s (%s) has ended: %v", x.ID, x.Name, err)
			continue
		}
		a.logf("session %s (%s): its harness has gone; the dispatcher is told it ended", x.ID, or(x.Name, "no name"))
		delete(a.gone, x.ID)
		// The record stays: if the dispatcher comes to believe the session
		// runs again (a word from a process its harness left), it is judged
		// again. A session started again on the same id records anew.
	}
	for id := range a.gone {
		if !believed[id] {
			delete(a.gone, id)
		}
	}

	waiting := map[string]bool{}
	for _, w := range at.Waiting {
		waiting[w.Name] = true
		a.attend(ctx, w)
	}
	for n := range a.tries {
		if !waiting[n] {
			// Taken, or no longer waited for: start afresh next time.
			delete(a.tries, n)
		}
	}
	a.save()
	return nil
}

// attend opens a name that messages wait for, once, then again only after
// a while and a few times; and says why when it does not.
func (a *attender) attend(ctx context.Context, w message.Awaited) {
	if a.host {
		a.notifyHost(ctx, w)
		return
	}
	t := a.tries[w.Name]
	if t != nil && !subset(w.Messages, t.Messages) {
		// Another message came: it is asked for again.
		t.Tries, t.Outcome = 0, ""
		t.Messages = w.Messages
	}
	if t == nil {
		t = &attempt{Messages: w.Messages}
		a.tries[w.Name] = t
	}
	if t.Tries > 0 && a.now().Sub(t.Last) < backoff(t.Tries) {
		return
	}
	if t.Outcome == "opened" || t.Outcome == "failed" {
		// Opened before, and the messages still wait: the harness did not
		// start, or did not take them.
		t.Outcome = "failed"
		if t.Tries >= maxTries {
			if t.Tries == maxTries {
				a.hold(ctx, w.Messages, "gave-up", fmt.Sprintf("session %s was opened again %d times in pod %s and did not take its messages; "+
					"it is not opened again until another message comes. A person can open it: agent-open %s", w.Name, t.Tries, a.pod, w.Name))
				a.logf("%s: opened %d times without taking its messages; stopped", w.Name, t.Tries)
				t.Tries++
			}
			return
		}
		a.hold(ctx, w.Messages, fmt.Sprintf("failed-%d", t.Tries), fmt.Sprintf("session %s was opened again in pod %s at %s and has not taken its messages; "+
			"it is tried again now, and at most %d times", w.Name, a.pod, t.Last.UTC().Format(time.RFC3339), maxTries))
	}
	t.Tries++
	t.Last = a.now()
	args := append(append([]string{}, a.open[1:]...), w.Name)
	cmd := exec.CommandContext(ctx, a.open[0], args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	said := strings.TrimSpace(string(out))
	switch code {
	case 0:
		t.Outcome = "opened"
		a.logf("%s: opened again for %d message(s): %s", w.Name, len(w.Messages), said)
	case reopenNotRemembered:
		t.Outcome = "not-remembered"
		t.Tries = maxTries + 1 // looked at again after the longest wait, or when another message comes
		a.hold(ctx, w.Messages, "not-remembered", fmt.Sprintf("session %s is not remembered in pod %s (forgotten with agent-open --forget or by leaving its window, "+
			"or never opened there), so it is not opened for a message; the message waits until a session of that name is opened (agent-open %s DIR)", w.Name, a.pod, w.Name))
		a.logf("%s: not remembered; not opened", w.Name)
	case reopenWindowBusy:
		t.Outcome = "busy"
		a.hold(ctx, w.Messages, fmt.Sprintf("busy-%d", t.Tries), fmt.Sprintf("session %s's window in pod %s runs something other than its shell (%s), "+
			"so it is not opened over it; tried again later", w.Name, a.pod, said))
		a.logf("%s: its window is busy: %s", w.Name, said)
	default:
		t.Outcome = "failed"
		a.logf("%s: agent-open failed (%d): %s", w.Name, code, said)
	}
}

func subset(a, b []string) bool {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// attendCmd is the watcher's loop. A dispatcher that does not answer is
// waited for; the watcher does not end.
func attendCmd(ctx context.Context, flags map[string][]string) error {
	place := os.Getenv("DISPATCHER_PLACE")
	if _, err := os.Stat(identityFile()); err != nil && place == "" {
		return fmt.Errorf("attend needs the pod's own token for the dispatcher (%s), or on the owner's machine DISPATCHER_PLACE and the place's token", identityFile())
	}
	c, err := client()
	if err != nil {
		return err
	}
	c.HTTP = &http.Client{Timeout: 20 * time.Second}
	every, err := time.ParseDuration(one(flags, "every", "10s"))
	if err != nil || every < time.Second {
		return errors.New("--every is a duration of a second or more")
	}
	a := &attender{
		c:     c,
		open:  strings.Fields(one(flags, "open", "agent-open --reopen")),
		state: one(flags, "state", filepath.Join(harnessDir(), "attend.json")),
		pod:   or(os.Getenv("HOSTNAME"), "this pod"),
		out:   os.Stderr,
		now:   time.Now,
	}
	where := "this pod"
	if place != "" {
		a.host, a.pod = true, place
		a.notify = strings.Fields(one(flags, "notify", os.Getenv("DISPATCHER_ATTEND_NOTIFY")))
		where = "the owner's machine (" + place + ")"
		pruneRecords(a.now())
	}
	os.MkdirAll(filepath.Dir(a.state), 0o700)
	a.load()
	_, once := flags["once"]
	a.logf("attending to the sessions of %s, every %s", where, every)
	for {
		if err := a.pass(ctx); err != nil && ctx.Err() == nil {
			if !a.down || !a.host {
				// On the owner's machine the dispatcher is away for as
				// long as its cluster is: said once, and once when back.
				a.logf("the dispatcher: %v", err)
			}
			a.down = true
			if once {
				return err
			}
		} else if a.down {
			a.down = false
			a.logf("the dispatcher answers again")
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// projectsDir is where the harness keeps its sessions' transcripts.
func projectsDir() string {
	if d := os.Getenv("DISPATCHER_PROJECTS_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// limitStop is the stop at the plan's usage limit a transcript ends with,
// if it does: the harness's own entry for an error from the model (an
// assistant entry of model <synthetic>, isApiErrorMessage), its key and its
// words. Anything said after it means the session went on.
func limitStop(file string) (key, words string, ok bool) {
	f, err := os.Open(file)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 256<<10 {
		f.Seek(st.Size()-256<<10, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type     string `json:"type"`
			UUID     string `json:"uuid"`
			APIError bool   `json:"isApiErrorMessage"`
			Message  struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil || (e.Type != "assistant" && e.Type != "user") {
			continue
		}
		if e.Type == "user" || !(e.APIError || e.Message.Model == "<synthetic>") {
			return "", "", false
		}
		for _, c := range e.Message.Content {
			words += c.Text
		}
		if _, ok := message.LimitReset(words, time.Now()); !ok || e.UUID == "" {
			return "", "", false
		}
		return e.UUID, words, true
	}
	return "", "", false
}

// limit tells the dispatcher, once, that a running session stopped at the
// plan's usage limit, so that whoever waits on it hears (ADR 0053).
func (a *attender) limit(ctx context.Context, session, name string) {
	files, _ := filepath.Glob(filepath.Join(projectsDir(), "*", session+".jsonl"))
	if len(files) == 0 {
		return
	}
	key, words, ok := limitStop(files[0])
	if !ok || a.limited[session] == key {
		return
	}
	var res struct{ Told int }
	if err := a.c.Do(ctx, http.MethodPost, "/v1/attend/limited", nil, api.LimitedRequest{Session: session, Key: key, Words: words}, &res); err != nil {
		a.logf("could not say session %s (%s) stopped at the plan's limit: %v", session, name, err)
		return
	}
	a.limited[session] = key
	a.logf("session %s (%s) stopped at the plan's usage limit; %d told", session, name, res.Told)
}
