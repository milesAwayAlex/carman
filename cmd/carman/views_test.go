package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// TestR2ViewsAskTheClient: carman status, plan and queue are the
// dispatcher's client asked what a checkout asks it (status, plan, priority
// list), as the owner: the token from the owner's file, by the environment
// and never the command line, and the client told it is no session and no
// pod. status and plan are the owner's to ask and say so with no owner's
// token; queue then takes the machine's, with its place.
func TestR2ViewsAskTheClient(t *testing.T) {
	file := filepath.Join(t.TempDir(), "owner.token")
	os.WriteFile(file, []byte("owner-secret\n"), 0o600)
	environ := []string{"PATH=/bin", "DISPATCHER_SESSION=s1", "CLAUDE_CODE_SESSION_ID=s1", "DISPATCHER_PLACE=host", "DISPATCHER_TOKEN=stale", "DISPATCHER_URL=http://stale",
		"DISPATCHER_IDENTITY=/somewhere", "DISPATCHER_PLAN_RESERVE=0.2"}
	c := config{URL: "http://d", Token: "machine-secret", OwnerTokenFile: file, Dispatcher: "/bin/the-client"}
	for word, want := range map[string]string{"status": "/bin/the-client status", "plan": "/bin/the-client plan", "queue": "/bin/the-client priority list"} {
		argv, env, err := viewCommand(c, word, nil, environ)
		if err != nil || strings.Join(argv, " ") != want {
			t.Errorf("%s: %v, %v; want %s", word, argv, err, want)
		}
		slices.Sort(env)
		if got := strings.Join(env, " "); got != "DISPATCHER_IDENTITY=/dev/null DISPATCHER_PLAN_RESERVE=0.2 DISPATCHER_TOKEN=owner-secret DISPATCHER_URL=http://d PATH=/bin" {
			t.Errorf("%s: the client's environment: %s", word, got)
		}
		if strings.Contains(strings.Join(argv, " "), "secret") {
			t.Errorf("%s: a token on the command line: %v", word, argv)
		}
	}
	// What may follow: a task for status, --json for plan, nothing for queue.
	if argv, _, err := viewCommand(c, "status", []string{"task-1"}, environ); err != nil || strings.Join(argv, " ") != "/bin/the-client status task-1" {
		t.Errorf("status of a task: %v, %v", argv, err)
	}
	if argv, _, err := viewCommand(c, "plan", []string{"--json"}, environ); err != nil || strings.Join(argv, " ") != "/bin/the-client plan --json" {
		t.Errorf("the plan as JSON: %v, %v", argv, err)
	}
	for _, bad := range [][]string{{"queue", "list"}, {"plan", "x"}, {"status", "a", "b"}} {
		if _, _, err := viewCommand(c, bad[0], bad[1:], environ); err == nil {
			t.Errorf("%v is asked of the client", bad)
		}
	}
	// With no owner's token.
	c.OwnerTokenFile = ""
	for _, word := range []string{"status", "plan"} {
		if _, _, err := viewCommand(c, word, nil, environ); err == nil || !strings.Contains(err.Error(), "CARMAN_OWNER_TOKEN_FILE") {
			t.Errorf("%s with no owner's token: %v", word, err)
		}
	}
	argv, env, err := viewCommand(c, "queue", nil, environ)
	if err != nil || strings.Join(argv, " ") != "/bin/the-client priority list" || !slices.Contains(env, "DISPATCHER_TOKEN=machine-secret") || !slices.Contains(env, "DISPATCHER_PLACE=host") || slices.Contains(env, "DISPATCHER_SESSION=s1") {
		t.Errorf("queue as the machine: %v, %v, %v", argv, env, err)
	}
	c.OwnerTokenFile = filepath.Join(t.TempDir(), "none")
	if _, _, err := viewCommand(c, "plan", nil, environ); err == nil {
		t.Error("plan with an owner's token file that is not there")
	}
}

// TestR2ViewsPrintWhatTheClientPrints: each of the three runs the client
// and gives its words and its status as they are.
func TestR2ViewsPrintWhatTheClientPrints(t *testing.T) {
	dir := t.TempDir()
	asked := filepath.Join(dir, "asked")
	client := filepath.Join(dir, "client")
	os.WriteFile(client, []byte("#!/bin/sh\necho \"$* as ${DISPATCHER_TOKEN} at ${DISPATCHER_URL}\" >> "+asked+"\n[ \"$1\" != plan ] || exit 7\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "token"), []byte("owner-secret"), 0o600)
	t.Setenv("CARMAN_DISPATCHER", client)
	t.Setenv("CARMAN_DISPATCHER_URL", "http://d")
	t.Setenv("CARMAN_OWNER_TOKEN_FILE", filepath.Join(dir, "token"))
	for word, want := range map[string]int{"status": 0, "queue": 0, "plan": 7} {
		if got := viewCmd(word, nil); got != want {
			t.Errorf("carman %s ends with %d, the client with %d", word, got, want)
		}
	}
	b, _ := os.ReadFile(asked)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	slices.Sort(lines)
	if got := strings.Join(lines, "; "); got != "plan as owner-secret at http://d; priority list as owner-secret at http://d; status as owner-secret at http://d" {
		t.Errorf("what the client was asked: %s", got)
	}
	t.Setenv("CARMAN_DISPATCHER", filepath.Join(dir, "no-such-client"))
	if got := viewCmd("status", nil); got != exitError {
		t.Errorf("with no client: status %d", got)
	}
}

// ownerInbox is a dispatcher that keeps an inbox for the owner: it gives
// what is unread to the owner's token only, and marks what it is told.
func ownerInbox(t *testing.T, now time.Time) (c config, marked func() string) {
	t.Helper()
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	items := []message.InboxItem{
		{Message: message.Message{ID: "m3", From: "session/cccc3333-0000", Agent: "platform", Name: "build", To: "owner", Sent: at(5), Body: "The build is done.\n\nSecond line.\nThird line.\nFourth line, not shown."}},
		{Message: message.Message{ID: "m2", From: "session/cccc3333-0000", Agent: "platform", Name: "build", To: "owner", Sent: at(50), Answers: "m0", Body: "An answer."}},
		{Message: message.Message{ID: "m1", From: "session/dddd4444-0000", Agent: "platform", Name: "vpn", To: "owner", Sent: at(90), Body: "From another session."}},
		{Message: message.Message{ID: "m0", From: "session/0000dead-0000", Agent: "review", To: "owner", Sent: at(60 * 24 * 9), Body: "From a run long gone."}},
	}
	var mu sync.Mutex
	read := map[string]bool{}
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		owner := r.Header.Get("Authorization") == "Bearer owner-secret"
		switch {
		case r.URL.Path == "/v1/peers":
			json.NewEncoder(w).Encode(somePeers(now))
		case !owner:
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "the owner's"})
		case r.URL.Path == "/v1/inbox":
			var out []message.InboxItem
			for _, it := range items {
				if !read[it.ID] {
					out = append(out, it)
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/v1/inbox/read":
			var req api.ReadRequest
			json.NewDecoder(r.Body).Decode(&req)
			for _, id := range req.IDs {
				read[id] = true
				order = append(order, id)
			}
			json.NewEncoder(w).Encode(map[string]any{"read": req.IDs})
		}
	}))
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "owner.token")
	os.WriteFile(file, []byte("owner-secret\n"), 0o600)
	return config{Context: "local", Namespace: "agents", URL: srv.URL, Token: "machine-secret", OwnerTokenFile: file}, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(order, " ")
	}
}

// TestR2InboxShowsAndMarks: carman inbox shows what is unread for the
// owner, each with who sent it (by what carman go takes), when, and its
// first lines; a message of a session that is gone is shown too, and said
// to be; --read marks all of it, carman read the ones named.
func TestR2InboxShowsAndMarks(t *testing.T) {
	c, marked := ownerInbox(t, time.Now())
	t.Setenv("CARMAN_DISPATCHER_URL", c.URL)
	t.Setenv("CARMAN_DISPATCHER_TOKEN", c.Token)
	t.Setenv("CARMAN_OWNER_TOKEN_FILE", c.OwnerTokenFile)
	var out bytes.Buffer
	if got := inboxCmd("inbox", nil, &out); got != exitLeft || marked() != "" {
		t.Fatalf("carman inbox: status %d, marked %q", got, marked())
	}
	for _, want := range []string{
		"m3  from platform/build (cccc3333), 5m ago\n  The build is done.\n  Second line.\n  Third line.\n  …\n",
		"m2  from platform/build (cccc3333), 50m ago, answering m0\n  An answer.\n",
		"m1  from platform/vpn (dddd4444), 1h ago\n",
		"m0  from review (0000dead), a session that cannot be gone to any more, 9d ago\n  From a run long gone.\n",
		"4 unread.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the inbox lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Fourth line") {
		t.Errorf("the inbox shows more than a message's first lines:\n%s", out.String())
	}
	out.Reset()
	if got := inboxCmd("read", []string{"m1"}, &out); got != exitLeft || marked() != "m1" {
		t.Errorf("carman read m1: status %d, marked %q", got, marked())
	}
	if got := inboxCmd("inbox", []string{"--read"}, &out); got != exitLeft || marked() != "m1 m3 m2 m0" {
		t.Errorf("carman inbox --read: status %d, marked %q", got, marked())
	}
	out.Reset()
	if got := inboxCmd("inbox", nil, &out); got != exitLeft || strings.TrimSpace(out.String()) != "nothing unread" {
		t.Errorf("then: status %d, %q", got, out.String())
	}
	for _, bad := range [][]string{{"read"}, {"inbox", "m1"}, {"read", "--all"}} {
		if got := inboxCmd(bad[0], bad[1:], &out); got != exitNoMatch {
			t.Errorf("carman %v: status %d", bad, got)
		}
	}
	// The inbox is the owner's: with no owner's token it is not read.
	t.Setenv("CARMAN_OWNER_TOKEN_FILE", "")
	if got := inboxCmd("inbox", nil, &out); got != exitError {
		t.Errorf("with no owner's token: status %d", got)
	}
}

// TestR2GoingToASessionReadsItsMessages: going to a session marks read
// what that session sent the owner, and nothing of another's; with no
// owner's token nothing is marked, and it says so when something is unread.
func TestR2GoingToASessionReadsItsMessages(t *testing.T) {
	c, marked := ownerInbox(t, time.Now())
	script, calls, codes := fakeKubectl(t)
	c.Kubectl = script
	all, err := fetch(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := resolve(all, "platform/build")
	codes("0\n")
	if got := stay(context.Background(), c, s, false); got != exitLeft || marked() != "m3 m2" || !strings.Contains(calls(), "agent-view") {
		t.Errorf("gone to platform/build: status %d, marked %q", got, marked())
	}
	// A headless run cannot be gone to: nothing of it is read.
	h, _ := resolve(all, "eeee")
	if got := stay(context.Background(), c, h, false); got != exitError || marked() != "m3 m2" {
		t.Errorf("a session that cannot be gone to: status %d, marked %q", got, marked())
	}
	var out bytes.Buffer
	c.OwnerTokenFile = ""
	other, _ := resolve(all, "platform/vpn")
	looked(context.Background(), c, other, &out)
	if marked() != "m3 m2" || !strings.Contains(out.String(), "1 unread from platform/vpn stay unread") || !strings.Contains(out.String(), "CARMAN_OWNER_TOKEN_FILE") {
		t.Errorf("with no owner's token: marked %q, said %q", marked(), out.String())
	}
	out.Reset()
	quiet, _ := resolve(all, "desk/prices")
	looked(context.Background(), c, quiet, &out)
	if out.String() != "" {
		t.Errorf("with nothing unread and no owner's token it says %q", out.String())
	}
}

// TestR2QueueIsAddedTo: carman queue add puts an item on the owner's list
// (dispatcher priority add), under the priority --in names or, with none,
// the default one, which the dispatcher is asked for; drop, first and again
// change an item by its id. The words are one argument, after the flags.
// Setting or removing a priority is not this command's.
func TestR2QueueIsAddedTo(t *testing.T) {
	asked := 0
	def := func() (string, error) { asked++; return "general", nil }
	for in, want := range map[string]string{
		"add|Write the report":                          "priority|add|general|Write the report",
		"add|Write|the|report":                          "priority|add|general|Write the report",
		"add|--hard|Fix it|--model|heavy-model|--first": "priority|add|general|--hard|--model|heavy-model|--first|Fix it",
		"add|Fix it|--in|platform":                      "priority|add|platform|Fix it",
		"drop|general-3":                                "priority|drop|general-3",
		"first|general-3":                               "priority|first|general-3",
		"again|general-3":                               "priority|again|general-3",
	} {
		got, err := queueAsk(strings.Split(in, "|"), def)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("%s: %q, %v; want %q", in, strings.Join(got, "|"), err, want)
		}
	}
	if asked != 3 {
		t.Errorf("the default priority was asked for %d times, want 3: only when none is named", asked)
	}
	for _, bad := range []string{"add", "add|--hard", "add|x|--share|2", "add|x|--model", "add|x|--in", "drop", "drop|a|b", "set|x", "remove|x", "list", "start|x", "done|x", "again|--all"} {
		if got, err := queueAsk(strings.Split(bad, "|"), def); err == nil {
			t.Errorf("%s is asked of the client: %v", bad, got)
		}
	}
	// The default priority is the dispatcher's word, asked as the machine
	// when there is no owner's token; and the change is then asked so too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/budget/priorities" || r.Header.Get("Authorization") != "Bearer machine-secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"priorities":[{"name":"platform","share":1},{"name":"general","share":2,"default":true}]}`))
	}))
	defer srv.Close()
	c := config{URL: srv.URL, Token: "machine-secret", Dispatcher: "/bin/the-client"}
	if name, err := defaultPriority(context.Background(), c); err != nil || name != "general" {
		t.Errorf("the default priority: %q, %v", name, err)
	}
	argv, env, err := clientCommand(c, "queue", []string{"priority", "add", "general", "x"}, false, []string{"DISPATCHER_PLACE=host"})
	if err != nil || strings.Join(argv, " ") != "/bin/the-client priority add general x" || !slices.Contains(env, "DISPATCHER_TOKEN=machine-secret") || !slices.Contains(env, "DISPATCHER_PLACE=host") {
		t.Errorf("a change as the machine: %v, %v, %v", argv, env, err)
	}
}
