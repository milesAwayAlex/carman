package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/message"
)

// dispatcherBin is the dispatcher's client, which prints the views:
// CARMAN_DISPATCHER, or this, which a package may set to its own build of
// the client (-X main.dispatcherBin=...), or the one on PATH.
var dispatcherBin = "dispatcher"

// views are the dispatcher's client's own, by the owner's words for them:
// what the client is asked, and whether only the owner may ask it.
var views = map[string]struct {
	Ask   []string
	Owner bool
	// Takes says what may follow the word.
	Takes string
}{
	"status": {Ask: []string{"status"}, Owner: true, Takes: "[TASK-ID]"},
	"plan":   {Ask: []string{"plan"}, Owner: true, Takes: "[--json]"},
	// With nothing after it; what changes the queue is queueAsk's.
	"queue": {Ask: []string{"priority", "list"}},
}

// ownerToken is the owner's token for the dispatcher, from the file the
// configuration names. It is read when a command needs it and is in no
// shell's environment.
func ownerToken(c config) (string, error) {
	if c.OwnerTokenFile == "" {
		return "", errors.New("CARMAN_OWNER_TOKEN_FILE is not set: the file that holds the owner's token for the dispatcher (a checkout's .local/dispatcher-owner.token)")
	}
	b, err := os.ReadFile(c.OwnerTokenFile)
	if err != nil {
		return "", fmt.Errorf("the owner's token: %w", err)
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("the owner's token: %s is empty", c.OwnerTokenFile)
}

// viewCommand is what prints a view: the dispatcher's client, asked what
// it is asked from a checkout, and its environment.
func viewCommand(c config, word string, args, environ []string) (argv, env []string, err error) {
	v := views[word]
	switch {
	case len(args) > 1, len(args) == 1 && v.Takes == "", word == "plan" && len(args) == 1 && args[0] != "--json":
		return nil, nil, fmt.Errorf("say: carman %s", strings.TrimSpace(word+" "+v.Takes))
	}
	return clientCommand(c, word, append(slices.Clone(v.Ask), args...), v.Owner, environ)
}

// clientCommand is the dispatcher's client asked something, and its
// environment. The token is the owner's where there is one, and has to be
// for what only the owner may ask; else the machine's. It goes by the
// environment, never the command line, and the client is told it is no
// session and no pod.
func clientCommand(c config, word string, ask []string, owner bool, environ []string) (argv, env []string, err error) {
	if c.URL == "" {
		return nil, nil, errors.New("the dispatcher's address is not set: CARMAN_DISPATCHER_URL, or DISPATCHER_URL")
	}
	token, terr := ownerToken(c)
	if terr != nil {
		if owner {
			return nil, nil, fmt.Errorf("carman %s is the owner's to ask: %w", word, terr)
		}
		if token = c.Token; token == "" {
			return nil, nil, errors.New("no token for the dispatcher: CARMAN_OWNER_TOKEN_FILE, or CARMAN_DISPATCHER_TOKEN")
		}
	}
	for _, e := range environ {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "DISPATCHER_URL", "DISPATCHER_TOKEN", "DISPATCHER_IDENTITY", "DISPATCHER_SESSION", "CLAUDE_CODE_SESSION_ID":
			continue
		case "DISPATCHER_PLACE":
			// The machine's token says its place; the owner's has none.
			if terr == nil {
				continue
			}
		}
		env = append(env, e)
	}
	env = append(env, "DISPATCHER_URL="+c.URL, "DISPATCHER_TOKEN="+token, "DISPATCHER_IDENTITY=/dev/null")
	return append([]string{c.Dispatcher}, ask...), env, nil
}

// queueUsage is what may follow carman queue.
const queueUsage = "carman queue | carman queue add 'WHAT' [--in PRIORITY] [--hard] [--model MODEL] [--first] | carman queue drop|first|again ID"

// queueAsk is what the client is asked for a change to the queue (the
// list of ADR 0077): dispatcher priority add|drop|first|again. An item is
// added under the priority --in names; with none, under the default one,
// which defaultPriority gives when it is needed.
func queueAsk(args []string, defaultPriority func() (string, error)) ([]string, error) {
	bad := func() ([]string, error) { return nil, errors.New("say: " + queueUsage) }
	switch args[0] {
	case "drop", "first", "again":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return bad()
		}
		return []string{"priority", args[0], args[1]}, nil
	case "add":
		var what, flags []string
		in := ""
		for i := 1; i < len(args); i++ {
			switch a := args[i]; {
			case a == "--hard" || a == "--first":
				flags = append(flags, a)
			case (a == "--model" || a == "--in") && i+1 < len(args) && args[i+1] != "" && !strings.HasPrefix(args[i+1], "-"):
				i++
				if a == "--in" {
					in = args[i]
				} else {
					flags = append(flags, a, args[i])
				}
			case strings.HasPrefix(a, "-"):
				return bad()
			default:
				what = append(what, a)
			}
		}
		if strings.TrimSpace(strings.Join(what, " ")) == "" {
			return bad()
		}
		if in == "" {
			var err error
			if in, err = defaultPriority(); err != nil {
				return nil, err
			}
		}
		// The words as one argument, after the flags: the client reads
		// what follows a flag as its value.
		return append(append([]string{"priority", "add", in}, flags...), strings.Join(what, " ")), nil
	}
	return bad()
}

// defaultPriority asks the dispatcher which priority is the default one,
// as the owner where there is his token, else as the machine.
func defaultPriority(ctx context.Context, c config) (string, error) {
	token, err := ownerToken(c)
	if err != nil {
		token = c.Token
	}
	if c.URL == "" || token == "" {
		return "", errors.New("the dispatcher's address and a token are not set; or name the priority with --in")
	}
	cl := &api.Client{URL: c.URL, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
	var v budget.View
	if err := cl.Do(ctx, http.MethodGet, "/v1/budget/priorities", nil, nil, &v); err != nil {
		return "", fmt.Errorf("the dispatcher at %s: %w", c.URL, err)
	}
	if v.Default() == "" {
		return "", errors.New("no priorities are set, so there is none to add under: scripts/deploy.sh CONTEXT dispatch priority set NAME --share N")
	}
	return v.Default(), nil
}

// viewCmd prints a view: the client's own words, on this terminal.
func viewCmd(word string, args []string) int {
	c := configFrom(os.Getenv)
	var argv, env []string
	var err error
	if word == "queue" && len(args) > 0 {
		var ask []string
		if ask, err = queueAsk(args, func() (string, error) { return defaultPriority(context.Background(), c) }); err == nil {
			argv, env, err = clientCommand(c, word, ask, false, os.Environ())
		}
	} else {
		argv, env, err = viewCommand(c, word, args, os.Environ())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "carman:", err)
		return exitNoMatch
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return exitLeft
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "carman: the dispatcher's client (%s; CARMAN_DISPATCHER names another): %v\n", c.Dispatcher, err)
	return exitError
}

// asOwner is the dispatcher's client as the owner.
func asOwner(c config) (*api.Client, error) {
	if c.URL == "" {
		return nil, errors.New("the dispatcher's address is not set: CARMAN_DISPATCHER_URL, or DISPATCHER_URL")
	}
	token, err := ownerToken(c)
	if err != nil {
		return nil, err
	}
	return &api.Client{URL: c.URL, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

// unread is what was sent to the owner that he has not read, as the
// dispatcher keeps it: the latest first.
func unread(ctx context.Context, cl *api.Client) ([]message.InboxItem, error) {
	var items []message.InboxItem
	if err := cl.Do(ctx, http.MethodGet, "/v1/inbox", nil, nil, &items); err != nil {
		return nil, fmt.Errorf("the dispatcher at %s: %w", cl.URL, err)
	}
	return items, nil
}

func markRead(ctx context.Context, cl *api.Client, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return cl.Do(ctx, http.MethodPost, "/v1/inbox/read", nil, api.ReadRequest{IDs: ids}, nil)
}

// inboxLines is how many lines of a message the inbox shows.
const inboxLines = 3

// inboxEntry is how an unread message is shown: its id, who sent it (by
// what carman go takes, when it can be gone to; else as the message says
// it, marked as nowhere to go), when, and its first lines.
func inboxEntry(it message.InboxItem, all []session, now time.Time) string {
	who := it.From
	id, isSession := strings.CutPrefix(it.From, "session/")
	if it.Agent != "" {
		who = it.Agent
		if it.Name != "" {
			who += "/" + it.Name
		}
	}
	if isSession {
		gone := true
		for _, s := range all {
			if s.ID == id {
				who, gone = s.By(), false
			}
		}
		if who != it.From {
			who += " (" + short(id) + ")"
		}
		if gone {
			who += ", a session that cannot be gone to any more"
		}
	}
	head := fmt.Sprintf("%s  from %s, %s", it.ID, who, ago(now.Sub(it.Sent)))
	if it.Answers != "" {
		head += ", answering " + it.Answers
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(it.Body), "\n") {
		if l = strings.TrimRight(l, " \t\r"); l == "" {
			continue
		}
		if len(lines) == inboxLines {
			lines = append(lines, "…")
			break
		}
		if r := []rune(l); len(r) > 200 {
			l = string(r[:200]) + "…"
		}
		lines = append(lines, l)
	}
	return head + "\n  " + strings.Join(lines, "\n  ")
}

// inboxCmd is `carman inbox [--read]` and `carman read ID...`: what is
// unread for the owner, and marking it read. A message of a session that
// is gone is shown like any other and marked like any other: it is
// nobody's count in the list, and stays here until it is read.
func inboxCmd(word string, args []string, out io.Writer) int {
	fail := func(code int, err error) int {
		fmt.Fprintln(os.Stderr, "carman "+word+":", err)
		return code
	}
	c := configFrom(os.Getenv)
	read := false
	var ids []string
	for _, a := range args {
		switch {
		case word == "inbox" && a == "--read":
			read = true
		case word == "read" && !strings.HasPrefix(a, "-"):
			ids = append(ids, a)
		default:
			return fail(exitNoMatch, fmt.Errorf("%q is not understood: carman inbox [--read], carman read MESSAGE-ID...", a))
		}
	}
	if word == "read" && len(ids) == 0 {
		return fail(exitNoMatch, errors.New("say which: carman read MESSAGE-ID... (carman inbox lists them; carman inbox --read marks them all)"))
	}
	cl, err := asOwner(c)
	if err != nil {
		return fail(exitError, err)
	}
	ctx := context.Background()
	if word == "read" {
		if err := markRead(ctx, cl, ids); err != nil {
			return fail(exitError, err)
		}
		fmt.Fprintf(out, "marked read: %s\n", strings.Join(ids, " "))
		return exitLeft
	}
	items, err := unread(ctx, cl)
	if err != nil {
		return fail(exitError, err)
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "nothing unread")
		return exitLeft
	}
	// Who can be gone to, for the senders' names; the inbox is shown
	// without them if the list cannot be had.
	all, _ := fetch(ctx, c)
	now := time.Now()
	for _, it := range items {
		fmt.Fprintln(out, inboxEntry(it, all, now))
		ids = append(ids, it.ID)
	}
	if !read {
		fmt.Fprintf(out, "%d unread. carman read ID marks one read, carman inbox --read all of these; going to a session marks its own.\n", len(items))
		return exitLeft
	}
	if err := markRead(ctx, cl, ids); err != nil {
		return fail(exitError, err)
	}
	fmt.Fprintf(out, "marked read: %d\n", len(ids))
	return exitLeft
}

// looked marks read what the session gone to sent the owner: he went to
// look. Without the owner's token nothing is marked, and that is said when
// something of the session's is unread.
func looked(ctx context.Context, c config, s session, out io.Writer) {
	if s.ID == "" {
		return
	}
	cl, err := asOwner(c)
	if err != nil {
		if s.ForOwner > 0 {
			fmt.Fprintf(out, "carman go: %d unread from %s stay unread: %v\n", s.ForOwner, s.By(), err)
		}
		return
	}
	items, err := unread(ctx, cl)
	if err != nil {
		fmt.Fprintf(out, "carman go: what %s sent you is not marked read: %v\n", s.By(), err)
		return
	}
	var ids []string
	for _, it := range items {
		if it.From == "session/"+s.ID {
			ids = append(ids, it.ID)
		}
	}
	if err := markRead(ctx, cl, ids); err != nil {
		fmt.Fprintf(out, "carman go: what %s sent you is not marked read: %v\n", s.By(), err)
	}
}
