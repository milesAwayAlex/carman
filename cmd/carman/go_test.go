package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// The peers a dispatcher gives the owner's machine, as its JSON says them.
func somePeers(now time.Time) []message.Peer {
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	return []message.Peer{
		{Address: "owner", Kind: "owner"},
		{Address: "agent/desk", Kind: "agent", Agent: "desk"},
		{Address: "session/aaaa1111-0000", Kind: "session", Agent: "desk", Name: "vpn", Named: "desk/vpn", Running: true, State: "busy", Pod: "agent-desk-0", Mode: "interactive", Standing: true, Seen: at(1)},
		{Address: "session/bbbb2222-0000", Kind: "session", Agent: "desk", Name: "prices", Named: "desk/prices", Running: true, State: "idle, not listening: it gets messages at its next turn", Pod: "agent-desk-0", Mode: "interactive", Standing: true, Seen: at(30)},
		{Address: "session/cccc3333-0000", Kind: "session", Agent: "platform", Name: "build", Named: "platform/build", Running: true, State: "idle", Pod: "agent-platform-0", Mode: "interactive", Standing: true, ForOwner: 2, Seen: at(90)},
		{Address: "session/dddd4444-0000", Kind: "session", Agent: "platform", Name: "vpn", Named: "platform/vpn", Running: true, State: "busy", Pod: "agent-platform-0", Mode: "interactive", Standing: true, ForOwner: 1, Seen: at(2)},
		{Address: "session/eeee5555-0000", Kind: "session", Agent: "review", Running: true, State: "busy", Pod: "review-1-abcde", Mode: "headless", Seen: at(3)},
		{Address: "session/ffff6666-0000", Kind: "session", Agent: "host", Name: "code", Named: "host/code", Running: true, State: "idle", Where: "interactive on the owner's machine", Seen: at(0)},
		{Address: "session/aaaa7777-0000", Kind: "session", Agent: "review", State: "not running (kept)", Pod: "review-0-zzzzz", Mode: "headless", Seen: at(600)},
		{Address: "session/9999aaaa-0000", Kind: "session", Agent: "desk", Name: "notes", Named: "desk/notes", State: "not running (kept)", Pod: "agent-desk-0", Mode: "interactive", Standing: true, Seen: at(500)},
		{Address: "session/8888bbbb-0000", Kind: "session", Agent: "desk", Name: "notes", State: "not running (kept)", Pod: "agent-desk-0", Mode: "interactive", Standing: true, Seen: at(900)},
	}
}

// TestR2GoOrder: the sessions are listed by how much each needs the owner:
// idle with something unread for him, then working with something unread,
// then idle, then busy, then those not running that can be gone to. The
// owner's own machine's sessions, and a standing agent's session that no
// name reaches any more, are not listed.
func TestR2GoOrder(t *testing.T) {
	now := time.Now()
	var got []string
	for _, s := range sessions(somePeers(now)) {
		got = append(got, s.By())
	}
	want := "platform/build platform/vpn desk/prices desk/vpn session/eeee5555-0000 desk/notes session/aaaa7777-0000"
	if strings.Join(got, " ") != want {
		t.Errorf("order:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	first := sessions(somePeers(now))[0].line(now)
	if !strings.HasPrefix(first, "platform/build ") || !strings.Contains(first, "2 unread for you") || !strings.Contains(first, "cccc3333") {
		t.Errorf("a line: %q", first)
	}
}

// TestR2GoOlderDispatcher: a dispatcher that does not yet say a session's
// pod and how it runs apart says them in words, which are read.
func TestR2GoOlderDispatcher(t *testing.T) {
	got := sessions([]message.Peer{
		{Address: "session/aaaa1111", Kind: "session", Agent: "desk", Name: "vpn", Named: "desk/vpn", Running: true, State: "idle", Where: "interactive in agent-desk-0"},
		{Address: "session/bbbb2222", Kind: "session", Agent: "review", Running: true, State: "busy", Where: "headless in review-1-abc"},
	})
	if len(got) != 2 || got[0].Pod != "agent-desk-0" || got[0].Mode != "interactive" || got[1].Mode != "headless" || got[1].Pod != "review-1-abc" {
		t.Errorf("read from words: %+v", got)
	}
}

// TestR2GoResolve: a session is named by AGENT/NAME, by its name alone when
// one session has it, by session/ID, or by the start of its id; a name two
// have says which, and one nobody has is an error.
func TestR2GoResolve(t *testing.T) {
	all := sessions(somePeers(time.Now()))
	for word, want := range map[string]string{
		"desk/vpn": "desk/vpn", "prices": "desk/prices", "cccc": "platform/build", "session/dddd4444-0000": "platform/vpn",
		"aaaa7": "session/aaaa7777-0000", "notes": "desk/notes", "eeee5555-0000": "session/eeee5555-0000",
		// What a notice names (the first eight characters of the id): a
		// session that runs, a standing agent's that does not, a job's
		// kept conversation.
		"cccc3333": "platform/build", "9999aaaa": "desk/notes", "aaaa7777": "session/aaaa7777-0000",
	} {
		s, err := resolve(all, word)
		if err != nil || s.By() != want {
			t.Errorf("%q: %q, %v; want %q", word, s.By(), err, want)
		}
	}
	if _, err := resolve(all, "vpn"); err == nil || !strings.Contains(err.Error(), "desk/vpn") || !strings.Contains(err.Error(), "platform/vpn") {
		t.Errorf("a name two sessions have: %v", err)
	}
	if _, err := resolve(all, "aaaa"); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("the start of two ids: %v", err)
	}
	for _, none := range []string{"nosuch", "aa", "host/code"} {
		if _, err := resolve(all, none); err == nil {
			t.Errorf("%q names a session", none)
		}
	}
}

// TestR2GoWhatIsRun: a session in a standing agent's pod is gone to with
// kubectl exec into the pod, at a view of its own; a job's kept
// conversation with the checkout's script, and says so when no checkout is
// named; a headless run has nothing to go to; nothing is run with no context.
func TestR2GoWhatIsRun(t *testing.T) {
	all := sessions(somePeers(time.Now()))
	by := func(word string) session {
		s, err := resolve(all, word)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	c := config{Context: "local", Namespace: "agents", Kubectl: "kubectl", Term: "xterm-256color", Host: "laptop"}
	argv, _, err := attach(c, by("platform/build"))
	want := "kubectl --context local --namespace agents exec -it agent-platform-0 -- env AGENT_TERM=xterm-256color AGENT_ATTACH_ID=laptop/platform/build AGENT_VIEW_BEAT=1 agent-view cccc3333-0000 build"
	if err != nil || strings.Join(argv, " ") != want {
		t.Errorf("a standing session:\n got %s, %v\nwant %s", strings.Join(argv, " "), err, want)
	}
	// Beside it, with no terminal, the word that this end is there.
	if got := strings.Join(beat(argv), " "); got != "kubectl --context local --namespace agents exec -i agent-platform-0 -- env AGENT_TERM=xterm-256color AGENT_ATTACH_ID=laptop/platform/build agent-view --beat" {
		t.Errorf("the beat: %s", got)
	}
	if beat([]string{"/src/scripts/deploy.sh", "local", "agent", "resume", "x"}) != nil {
		t.Error("a beat beside what is not a view")
	}
	// Not running, remembered by its pod: the same, and the pod opens it again.
	if argv, _, err = attach(c, by("desk/notes")); err != nil || !strings.HasSuffix(strings.Join(argv, " "), "agent-view 9999aaaa-0000 notes") || argv[7] != "agent-desk-0" {
		t.Errorf("a standing session that is not running: %v, %v", argv, err)
	}
	if _, _, err = attach(c, by("aaaa7")); err == nil || !strings.Contains(err.Error(), "CARMAN_CHECKOUT") || !strings.Contains(err.Error(), "agent resume aaaa7777") {
		t.Errorf("a job's conversation with no checkout named: %v", err)
	}
	c.Checkout = "/src/checkout"
	argv, dir, err := attach(c, by("aaaa7"))
	if err != nil || strings.Join(argv, " ") != "/src/checkout/scripts/deploy.sh local agent resume aaaa7777-0000" || dir != "/src/checkout" {
		t.Errorf("a job's conversation: %v in %s, %v", argv, dir, err)
	}
	if _, _, err = attach(c, by("eeee")); err == nil || !strings.Contains(err.Error(), "headless") {
		t.Errorf("a headless run: %v", err)
	}
	c.Context = ""
	if _, _, err = attach(c, by("platform/build")); err == nil || !strings.Contains(err.Error(), "CARMAN_CONTEXT") {
		t.Errorf("with no context: %v", err)
	}
}

// TestR2GoConfiguration: the program's own variables, with the client's as
// the fallback for the dispatcher.
func TestR2GoConfiguration(t *testing.T) {
	env := map[string]string{"CARMAN_CONTEXT": "local", "DISPATCHER_URL": "http://d", "DISPATCHER_TOKEN": "t", "TERM": "xterm"}
	c := configFrom(func(k string) string { return env[k] })
	if c.Context != "local" || c.Namespace != "carman" || c.Kubectl != "kubectl" || c.URL != "http://d" || c.Token != "t" || c.Checkout != "" {
		t.Errorf("defaults: %+v", c)
	}
	env["CARMAN_DISPATCHER_URL"], env["CARMAN_DISPATCHER_TOKEN"], env["CARMAN_NAMESPACE"], env["CARMAN_KUBECTL"] = "http://other", "u", "agents", "/bin/k"
	c = configFrom(func(k string) string { return env[k] })
	if c.URL != "http://other" || c.Token != "u" || c.Namespace != "agents" || c.Kubectl != "/bin/k" {
		t.Errorf("its own variables: %+v", c)
	}
}

// TestR2GoNumberedList: with no fzf or no terminal, a numbered list; a
// number chooses, anything else is nothing chosen.
func TestR2GoNumberedList(t *testing.T) {
	all := sessions(somePeers(time.Now()))
	var out bytes.Buffer
	s, err := pick(all, strings.NewReader("3\n"), &out, false)
	if err != nil || s.By() != "desk/prices" || !strings.Contains(out.String(), " 1  platform/build") {
		t.Errorf("the third: %q, %v\n%s", s.By(), err, out.String())
	}
	for _, in := range []string{"\n", "0\n", "99\n", "x\n", ""} {
		if _, err := pick(all, strings.NewReader(in), &out, false); err != errCancelled {
			t.Errorf("%q: %v, want nothing chosen", in, err)
		}
	}
}

// TestR2GoKeep: how the attach ended says what to do. Status 0 is a leave;
// the pod's refusal is given back and not tried again; anything else is a
// drop, which without --keep ends in an error and with it is attached
// again, at the session its name reaches by then, until it is left.
func TestR2GoKeep(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(somePeers(now))
	}))
	defer srv.Close()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	// A kubectl that ends as the next line of a file says.
	script := filepath.Join(dir, "kubectl")
	os.WriteFile(script, []byte("#!/bin/sh\ncase \"$*\" in *--beat) cat >/dev/null; exit 0 ;; esac\necho \"$*\" >> "+calls+"\ncode=$(head -1 "+dir+"/codes)\nsed -i.bak 1d "+dir+"/codes\nexit ${code:-0}\n"), 0o755)
	codes := func(s string) { os.WriteFile(filepath.Join(dir, "codes"), []byte(s), 0o644); os.Remove(calls) }
	count := func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "agent-view")
	}
	firstWait, quickFailure = 10*time.Millisecond, 0
	defer func() { firstWait, quickFailure = 2*time.Second, 5*time.Second }()
	c := config{Context: "local", Namespace: "agents", Kubectl: script, URL: srv.URL, Token: "t"}
	all, err := fetch(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := resolve(all, "platform/build")

	codes("0\n")
	if got := stay(context.Background(), c, s, true); got != exitLeft || count() != 1 {
		t.Errorf("left: status %d after %d attaches", got, count())
	}
	codes("1\n0\n")
	if got := stay(context.Background(), c, s, false); got != exitError || count() != 1 {
		t.Errorf("a drop without --keep: status %d after %d attaches", got, count())
	}
	codes("1\n137\n255\n0\n")
	if got := stay(context.Background(), c, s, true); got != exitLeft || count() != 4 {
		t.Errorf("three drops, then left, with --keep: status %d after %d attaches", got, count())
	}
	// This end goes while attached (its window is closed): what it runs is
	// ended with it, and that is a leave, whatever kubectl would have done.
	said := filepath.Join(dir, "said")
	os.WriteFile(script, []byte("#!/bin/sh\ncase \"$*\" in *--beat) while read w; do [ \"$w\" = bye ] && { echo bye > "+said+"; exit 0; }; done; exit 0 ;; esac\necho \"$*\" >> "+calls+"\ntrap '' HUP\nexec sleep 30\n"), 0o755)
	os.Remove(calls)
	began := time.Now()
	go func() {
		time.Sleep(300 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGHUP)
	}()
	if got := stay(context.Background(), c, s, true); got != exitLeft || count() != 1 || time.Since(began) > 10*time.Second {
		t.Errorf("hung up while attached: status %d after %d attaches and %s", got, count(), time.Since(began))
	}
	// And the pod was told that this end left, so its view goes at once.
	if b, _ := os.ReadFile(said); strings.TrimSpace(string(b)) != "bye" {
		t.Errorf("the pod was not told that this end left: %q", b)
	}
	os.WriteFile(script, []byte("#!/bin/sh\ncase \"$*\" in *--beat) cat >/dev/null; exit 0 ;; esac\necho \"$*\" >> "+calls+"\ncode=$(head -1 "+dir+"/codes)\nsed -i.bak 1d "+dir+"/codes\nexit ${code:-0}\n"), 0o755)
	codes("1\n3\n0\n")
	if got := stay(context.Background(), c, s, true); got != viewRefused || count() != 2 {
		t.Errorf("a drop, then nothing to show: status %d after %d attaches", got, count())
	}
}
