package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// TestR2GoNewWhichAgent: a new session is opened in the standing agent
// named; in the only one when there is one; with several and none named,
// in the one picked on a terminal, and with no terminal it is refused with
// their names. The dispatcher says which agents are standing, with their
// sessions (and their pod) or, one that has no session yet, by itself.
func TestR2GoNewWhichAgent(t *testing.T) {
	peers := somePeers(time.Now())
	var out bytes.Buffer
	if a, err := choose(peers, "platform", strings.NewReader(""), &out, false); err != nil || a.Name != "platform" || a.Pod != "agent-platform-0" {
		t.Errorf("the one named: %+v, %v", a, err)
	}
	if _, err := choose(peers, "", strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "--in AGENT") || !strings.Contains(err.Error(), "desk, platform") {
		t.Errorf("several, none named, no terminal: %v", err)
	}
	// An agent of jobs, and the owner's machine, hold no sessions to open.
	for _, no := range []string{"review", "host", "nobody"} {
		if _, err := choose(peers, no, strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "desk, platform") {
			t.Errorf("%s, which is no standing agent: %v", no, err)
		}
	}
	out.Reset()
	if a, err := choose(peers, "", strings.NewReader("2\n"), &out, true); err != nil || a.Name != "platform" || !strings.Contains(out.String(), " 1  desk") {
		t.Errorf("picked on a terminal: %+v, %v\n%s", a, err, out.String())
	}
	if _, err := choose(peers, "", strings.NewReader("\n"), &out, true); err != errCancelled {
		t.Errorf("nothing picked: %v", err)
	}
	// One standing agent: that one. Here one with no session yet, which the
	// dispatcher says is standing; its pod is its StatefulSet's.
	one := []message.Peer{{Address: "agent/review", Kind: "agent", Agent: "review"}, {Address: "agent/fresh", Kind: "agent", Agent: "fresh", Standing: true}}
	if a, err := choose(one, "", strings.NewReader(""), &out, false); err != nil || a.Name != "fresh" || a.Pod != "agent-fresh-0" {
		t.Errorf("the only one: %+v, %v", a, err)
	}
	if _, err := choose(one[:1], "", strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "no standing agent") {
		t.Errorf("none: %v", err)
	}
}

// fakeKubectl is a kubectl that writes each call's arguments, one to a
// line after a line "call", and ends as the next line of codes says.
func fakeKubectl(t *testing.T) (script string, calls func() string, codes func(string)) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "kubectl")
	body := "#!/bin/sh\ncase \"$*\" in *--beat) cat >/dev/null; exit 0 ;; esac\n{ echo call; printf '%s\\n' \"$@\"; } >> " + dir + "/calls\n" +
		"code=$(head -1 " + dir + "/codes)\nsed -i.bak 1d " + dir + "/codes\nexit ${code:-0}\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	calls = func() string { b, _ := os.ReadFile(filepath.Join(dir, "calls")); return string(b) }
	codes = func(s string) {
		os.WriteFile(filepath.Join(dir, "codes"), []byte(s), 0o644)
		os.Remove(filepath.Join(dir, "calls"))
	}
	return script, calls, codes
}

// TestR2GoNewOpensAndGoes: the session is opened with agent-open in the
// agent's pod, the name, directory and model given as arguments and not in
// the script's text, and is then gone to as any session is: a view of its
// window, by its name, its id not being known yet. A name that is taken is
// refused, by what the dispatcher knows before anything is run, and by the
// pod; nothing is gone to then.
func TestR2GoNewOpensAndGoes(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(somePeers(now))
	}))
	defer srv.Close()
	script, calls, codes := fakeKubectl(t)
	c := config{Context: "local", Namespace: "agents", Kubectl: script, URL: srv.URL, Token: "t", Term: "xterm", Host: "laptop"}
	peers, err := fetchPeers(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	codes("0\n0\n")
	s, err := open(c, peers, opening{Name: "fresh", In: "platform", Dir: "src/project", Model: "some-model"}, strings.NewReader(""), &out, false)
	if err != nil || s.By() != "platform/fresh" || s.Pod != "agent-platform-0" {
		t.Fatalf("opened: %+v, %v", s, err)
	}
	want := strings.Join([]string{"call", "--context", "local", "--namespace", "agents", "exec", "agent-platform-0", "--", "sh", "-c"}, "\n") + "\n" +
		openInPod + "\nagent-open\nfresh\nsrc/project\nsome-model\n"
	if calls() != want {
		t.Errorf("what opens it:\n got %q\nwant %q", calls(), want)
	}
	for _, in := range []string{"fresh", "src/project", "some-model"} {
		if strings.Contains(openInPod, in) {
			t.Errorf("the pod's script holds %q", in)
		}
	}
	// Gone to as a session is, by its window's name.
	if got := stay(context.Background(), c, s, false); got != exitLeft {
		t.Errorf("going to it: status %d", got)
	}
	if !strings.HasSuffix(calls(), "\nexec\n-it\nagent-platform-0\n--\nenv\nAGENT_TERM=xterm\nAGENT_ATTACH_ID=laptop/platform/fresh\nAGENT_VIEW_BEAT=1\nagent-view\n\nfresh\n") {
		t.Errorf("what goes to it: %q", calls())
	}

	// The pod says the name is taken: refused, with how to go to that one.
	codes("5\n")
	if _, err := open(c, peers, opening{Name: "old", In: "desk"}, strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "taken") || !strings.Contains(err.Error(), "carman go desk/old") {
		t.Errorf("a name the pod has: %v", err)
	}
	// The dispatcher knows a running session of the name: nothing is run.
	codes("0\n")
	if _, err := open(c, peers, opening{Name: "vpn", In: "desk"}, strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "a session already") || calls() != "" {
		t.Errorf("a name that runs: %v; ran %q", err, calls())
	}
	// A name whose last session there no longer runs is the pod's to say.
	if _, err := open(c, peers, opening{Name: "notes", In: "desk"}, strings.NewReader(""), &out, false); err != nil || !strings.Contains(calls(), "agent-desk-0") {
		t.Errorf("a name that ran once: %v; ran %q", err, calls())
	}
	// The pod failing otherwise (no such directory) is an error, not a session.
	codes("1\n")
	if _, err := open(c, peers, opening{Name: "fresh", In: "desk", Dir: "nowhere"}, strings.NewReader(""), &out, false); err == nil || strings.Contains(err.Error(), "taken") {
		t.Errorf("the pod fails: %v", err)
	}
	// What is no name or no model never reaches the pod; nor anything
	// with no context.
	codes("0\n")
	for _, o := range []opening{{Name: "a b", In: "desk"}, {Name: "-x", In: "desk"}, {Name: "ok", In: "desk", Model: "m; rm"}} {
		if _, err := open(c, peers, o, strings.NewReader(""), &out, false); err == nil || calls() != "" {
			t.Errorf("%+v: %v; ran %q", o, err, calls())
		}
	}
	c.Context = ""
	if _, err := open(c, peers, opening{Name: "ok", In: "desk"}, strings.NewReader(""), &out, false); err == nil || !strings.Contains(err.Error(), "CARMAN_CONTEXT") || calls() != "" {
		t.Errorf("with no context: %v", err)
	}
}

// TestR2GoNewInThePod: what runs in the pod, with an agent-open and a tmux
// that only say what they were asked. It starts in the home's work
// directory; a name the pod remembers or has a window of ends with 5 and
// changes nothing; a directory that is not there is an error; otherwise
// agent-open is given the name, the directory and the model.
func TestR2GoNewInThePod(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(filepath.Join(home, "work", "project"), 0o755)
	os.MkdirAll(bin, 0o755)
	asked := filepath.Join(home, "asked")
	// agent-open: with nothing, the remembered sessions as it lists them.
	os.WriteFile(filepath.Join(bin, "agent-open"), []byte("#!/bin/sh\n[ $# -gt 0 ] || { printf '%-24s %s\\n' kept /home/agent/work/kept; exit 0; }\n{ pwd -P; printf '%s\\n' \"$@\"; } > "+asked+"\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho bash; echo windowed\n"), 0o755)
	run := func(name, dir, model string) (int, string) {
		os.Remove(asked)
		cmd := exec.Command("sh", "-c", openInPod, "agent-open", name, dir, model)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "AGENT_TMUX=" + filepath.Join(bin, "tmux")}
		code := 0
		if err := cmd.Run(); err != nil {
			code = cmd.ProcessState.ExitCode()
		}
		b, _ := os.ReadFile(asked)
		return code, string(b)
	}
	work := filepath.Join(home, "work")
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	if code, got := run("fresh", "", ""); code != 0 || got != work+"\nfresh\n.\n" {
		t.Errorf("with no directory and no model: status %d, agent-open %q", code, got)
	}
	if code, got := run("fresh", "project", "some-model"); code != 0 || got != work+"\n--model\nsome-model\nfresh\nproject\n" {
		t.Errorf("with a directory and a model: status %d, agent-open %q", code, got)
	}
	for _, taken := range []string{"kept", "windowed"} {
		if code, got := run(taken, "", ""); code != nameTaken || got != "" {
			t.Errorf("%s, which the pod has: status %d, agent-open %q", taken, code, got)
		}
	}
	if code, got := run("fresh", "nowhere", ""); code != 1 || got != "" {
		t.Errorf("a directory that is not there: status %d, agent-open %q", code, got)
	}
}
