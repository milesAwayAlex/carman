package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// Messaging (ADR 0045): the commands a session runs through its shell, the
// owner runs from the laptop (scripts/deploy.sh CONTEXT dispatch ...), and
// the harness's hooks run to carry messages into a running session.

// identityFile is where an agent's pod has the token the cluster made for it
// to show the dispatcher.
func identityFile() string {
	if f := os.Getenv("DISPATCHER_IDENTITY"); f != "" {
		return f
	}
	return "/var/run/secrets/dispatcher/token"
}

// client is the dispatcher's client as whoever this is: the pod's own
// token if it has one, else DISPATCHER_TOKEN.
func client() (*api.Client, error) {
	c := &api.Client{URL: os.Getenv("DISPATCHER_URL"), Token: os.Getenv("DISPATCHER_TOKEN")}
	if b, err := os.ReadFile(identityFile()); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		c.Token = strings.TrimSpace(string(b))
	}
	if c.URL == "" || c.Token == "" {
		return nil, errors.New("set DISPATCHER_URL, and DISPATCHER_TOKEN or a token for the dispatcher at DISPATCHER_IDENTITY")
	}
	return c, nil
}

// session is the session this command runs in, as its SessionStart hook
// told the shell, or as given.
func session(flags map[string][]string) string {
	// The hook puts the session's id in the shell; where the shell did not
	// take it (seen in a session resumed on the owner's machine, 2.1.326),
	// the harness's own variable says it.
	return one(flags, "session", or(os.Getenv("DISPATCHER_SESSION"), os.Getenv("CLAUDE_CODE_SESSION_ID")))
}

// text is the words after the command, or what is on stdin when they are "-".
func text(words []string) (string, error) {
	if len(words) == 1 && words[0] == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		return string(b), err
	}
	return strings.Join(words, " "), nil
}

func messages(ctx context.Context, c *api.Client, cmd string, pos []string, flags map[string][]string) error {
	me := session(flags)
	switch cmd {
	case "whoami":
		var out map[string]any
		if err := c.Do(ctx, http.MethodGet, "/v1/whoami", nil, nil, &out); err != nil {
			return err
		}
		fmt.Printf("role %v, agent %v, pod %v, session %s\n", out["role"], out["agent"], out["pod"], or(me, "(none)"))
		return nil
	case "peers":
		var ps []message.Peer
		q := url.Values{}
		if v := one(flags, "since", ""); v != "" {
			q.Set("since", v)
		}
		if _, ok := flags["all"]; ok {
			q.Set("all", "yes")
		}
		if err := c.Do(ctx, http.MethodGet, "/v1/peers", q, nil, &ps); err != nil {
			return err
		}
		for _, p := range ps {
			fmt.Println(peerLine(p, me))
		}
		return nil
	case "send", "reply":
		if len(pos) < 2 {
			return fmt.Errorf("say: %s %s 'TEXT' (or - to read it from stdin)", cmd, map[string]string{"send": "TO", "reply": "MESSAGE-ID"}[cmd])
		}
		body, err := text(pos[1:])
		if err != nil {
			return err
		}
		req := api.SendRequest{To: pos[0], Body: body, Answers: one(flags, "answers", ""), Session: me}
		if cmd == "reply" {
			req.To, req.Answers = "", pos[0]
		}
		var res message.Result
		if err := c.Do(ctx, http.MethodPost, "/v1/messages", nil, req, &res); err != nil {
			return err
		}
		if res.State == "waiting" {
			res = settle(ctx, c, res, one(flags, "wait", "4s"))
		}
		fmt.Printf("%s %s: %s\n", res.ID, res.State, res.Detail)
		if res.State == "refused" {
			os.Exit(4)
		}
		return nil
	case "message":
		if len(pos) != 1 {
			return errors.New("say: message ID")
		}
		var st message.Status
		if err := c.Do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(pos[0]), nil, nil, &st); err != nil {
			return err
		}
		m := st.Message
		// For a program: the body as it was sent, or who sent it.
		if _, ok := flags["body"]; ok {
			fmt.Print(m.Body)
			return nil
		}
		if _, ok := flags["from"]; ok {
			fmt.Printf("%s %s %s\n", m.From, or(m.Agent, "-"), or(m.Name, "-"))
			return nil
		}
		fmt.Printf("%s, from %s to %s (%s), sent %s: %s\n", m.ID, m.From, or(m.To, m.Given), st.State, m.Sent.UTC().Format(time.RFC3339), m.Body)
		for _, e := range st.Events {
			fmt.Printf("  %d  %s  %-18s %s\n", e.Seq, e.Time.UTC().Format(time.RFC3339), e.Type, e.Data)
		}
		if st.TaskNow != "" {
			fmt.Printf("  task now: %s\n", st.TaskNow)
		}
		for _, a := range st.Answers {
			fmt.Printf("  answered by %s (%s), %s: %s\n", a.From, a.ID, a.Sent.UTC().Format(time.RFC3339), a.Body)
		}
		return nil
	case "inbox":
		var items []message.InboxItem
		if err := c.Do(ctx, http.MethodGet, "/v1/inbox", url.Values{"session": {me}}, nil, &items); err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("nothing unread")
		}
		var ids []string
		for _, it := range items {
			how := "not delivered yet"
			if it.Delivered != nil {
				how = "delivered to " + it.Delivered.Session + " (" + it.Delivered.How + ")"
			}
			fmt.Printf("%s  from %s%s  %s  [%s]\n  %s\n", it.ID, it.From, prefixed(" re ", it.Answers), it.Sent.UTC().Format(time.RFC3339), how, strings.ReplaceAll(it.Body, "\n", "\n  "))
			ids = append(ids, it.ID)
		}
		if _, ok := flags["read"]; ok && len(ids) > 0 {
			return markRead(ctx, c, me, ids)
		}
		return nil
	case "read":
		if len(pos) == 0 {
			return errors.New("say: read MESSAGE-ID...")
		}
		return markRead(ctx, c, me, pos)
	case "watch":
		if len(pos) != 1 {
			return errors.New("say: watch SESSION (you are told once, by a message, when it next goes idle)")
		}
		var out map[string]string
		if err := c.Do(ctx, http.MethodPost, "/v1/watch", nil, api.WatchRequest{Session: me, Target: pos[0]}, &out); err != nil {
			return err
		}
		fmt.Printf("watching %s (%s): a message tells you when it next goes idle\n", pos[0], out["watch"])
		return nil
	case "take":
		// What a run started for a message hands its harness.
		var t message.Taken
		req := api.TakeRequest{Session: me, How: one(flags, "how", "start"), IDs: flags["message"]}
		if err := c.Do(ctx, http.MethodPost, "/v1/inbox/take", nil, req, &t); err != nil {
			return err
		}
		fmt.Print(t.Text)
		return nil
	case "answer":
		if len(pos) < 1 {
			return errors.New("say: answer MESSAGE-ID [TEXT | -]")
		}
		body, err := text(pos[1:])
		if err != nil {
			return err
		}
		var res message.Result
		if err := c.Do(ctx, http.MethodPost, "/v1/messages/"+url.PathEscape(pos[0])+"/answer", nil, api.SendRequest{Body: body, Session: me}, &res); err != nil {
			return err
		}
		fmt.Printf("%s %s: %s\n", res.ID, res.State, res.Detail)
		return nil
	case "session":
		if len(pos) != 1 {
			return errors.New("say: session started|busy|idle|ended [--name N] [--kind K]")
		}
		return c.Do(ctx, http.MethodPost, "/v1/sessions", nil, message.Presence{Session: me, State: pos[0], Name: one(flags, "name", ""), Kind: one(flags, "kind", "")}, nil)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func markRead(ctx context.Context, c *api.Client, me string, ids []string) error {
	var out struct{ Read []string }
	err := c.Do(ctx, http.MethodPost, "/v1/inbox/read", nil, api.ReadRequest{Session: me, IDs: ids}, &out)
	if len(out.Read) > 0 {
		fmt.Println("read:", strings.Join(out.Read, " "))
	}
	return err
}

// settle waits a moment for a message to a running session to be delivered,
// so that the sender is told so.
func settle(ctx context.Context, c *api.Client, res message.Result, wait string) message.Result {
	d, err := time.ParseDuration(wait)
	if err != nil || d <= 0 {
		return res
	}
	until := time.Now().Add(d)
	for time.Now().Before(until) {
		var st message.Status
		if err := c.Do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(res.ID), nil, nil, &st); err != nil {
			return res
		}
		for _, e := range st.Events {
			if e.Type == message.TypeDelivered {
				var d message.Delivery
				_ = json.Unmarshal(e.Data, &d)
				res.State, res.Detail = "delivered", fmt.Sprintf("to %s (%s), %s", d.Session, d.How, e.Time.UTC().Format(time.RFC3339))
				return res
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return res
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

// hookInput is what the harness gives a hook on stdin.
type hookInput struct {
	SessionID string `json:"session_id"`
	Event     string `json:"hook_event_name"`
	Cwd       string `json:"cwd"`
}

const intro = "You can be written to, and can write to other sessions, to agents by name and to the owner, through the dispatcher's client in your shell: " +
	"`dispatcher peers` (who, and whether each is busy, idle or not running), `dispatcher send TO 'TEXT'`, `dispatcher reply MESSAGE-ID 'TEXT'`, " +
	"`dispatcher inbox`, `dispatcher message ID` (what became of one you sent), `dispatcher watch SESSION` (be told when it next goes idle). " +
	"A message reaches its addressee whether it is running or not. Messages for you arrive in this conversation as <message> blocks, " +
	"between your tool calls or when you are idle; each says who it is from. You are %s."

// hooksOff says whether this harness runs without the image's hooks.
func hooksOff() bool {
	return os.Getenv("CMEM_HOOK") == "off" || os.Getenv("DISPATCHER_HOOK") == "off"
}

// hook carries messages into a running session (ADR 0045), as a hook of
// the harness: Claude Code's events SessionStart, UserPromptSubmit,
// PostToolBatch, Stop and SessionEnd. With --wait it is the idle waiter, run
// as an asyncRewake Stop hook: it waits for a message and wakes the
// session with it (exit 2, the message on stderr). It never fails the
// harness: without a dispatcher, it says nothing.
func hook(ctx context.Context, flags map[string][]string) error {
	// A harness started for a trial or a side job (CMEM_HOOK=off switches
	// off every hook the agent image installs) is not a session to message.
	if hooksOff() {
		return nil
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&in); err != nil || in.SessionID == "" {
		return nil
	}
	// Only a pod with its own token for the dispatcher has messages (the
	// shared agent token would be refused at every call), or a session on
	// the owner's machine, with the host role's token (ADR 0049).
	place := os.Getenv("DISPATCHER_PLACE")
	if _, err := os.Stat(identityFile()); err != nil && place == "" {
		return nil
	}
	c, err := client()
	if err != nil {
		return nil
	}
	// A dispatcher that does not answer costs a tool call no more than this.
	c.HTTP = &http.Client{Timeout: 3 * time.Second}
	kind := or(os.Getenv("DISPATCHER_SESSION_KIND"), "headless")
	if (os.Getenv("AGENT_INTERACTIVE") != "" || place != "") && os.Getenv("DISPATCHER_SESSION_KIND") == "" && !unattended() {
		kind = "interactive"
	}
	name := sessionName(place, in.Cwd)
	present := func(state string) error {
		return c.Do(ctx, http.MethodPost, "/v1/sessions", nil, message.Presence{Session: in.SessionID, State: state,
			Name: name, Kind: kind, Cwd: in.Cwd}, nil)
	}
	take := func(how string, wait time.Duration) (message.Taken, error) {
		var t message.Taken
		req := api.TakeRequest{Session: in.SessionID, How: how}
		if wait > 0 {
			req.Wait = wait.String()
		}
		err := c.Do(ctx, http.MethodPost, "/v1/inbox/take", nil, req, &t)
		return t, err
	}
	say := func(event, text string) {
		if text == "" {
			return
		}
		out := map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": text}}
		json.NewEncoder(os.Stdout).Encode(out)
	}
	if w := one(flags, "wait", ""); w != "" {
		if kind == "headless" {
			// A headless run is not woken: it ends, and a message that comes
			// after starts it again. A listener left running would hold the
			// run open: the harness waits for it (seen on 2.1.326).
			return nil
		}
		total, err := time.ParseDuration(w)
		if err != nil {
			return nil
		}
		c.HTTP = &http.Client{}
		// A listener outlives a harness that is killed: it would take the
		// next message for a session nobody runs (ADR 0053). It listens
		// only while the harness that started it is there.
		gone := harnessGoneFunc(os.Getppid())
		until := time.Now().Add(total)
		for left := time.Until(until); left > time.Second; left = time.Until(until) {
			if gone() {
				return nil
			}
			chunk := min(left, 30*time.Minute)
			cctx, cancel := context.WithTimeout(ctx, chunk+30*time.Second)
			stop := context.AfterFunc(cctx, func() {})
			go func() {
				for cctx.Err() == nil {
					if gone() {
						cancel()
						return
					}
					time.Sleep(2 * time.Second)
				}
			}()
			t, err := func() (message.Taken, error) {
				defer cancel()
				defer stop()
				var t message.Taken
				err := c.Do(cctx, http.MethodPost, "/v1/inbox/take", nil, api.TakeRequest{Session: in.SessionID, How: "wake", Wait: chunk.String()}, &t)
				return t, err
			}()
			switch {
			case err != nil && gone():
				return nil
			case err != nil:
				time.Sleep(5 * time.Second) // the dispatcher restarting: wait for it
			case t.Busy:
				return nil
			case len(t.Messages) > 0:
				fmt.Fprintln(os.Stderr, t.Text)
				os.Exit(2)
			}
		}
		return nil
	}
	switch in.Event {
	case "SessionStart":
		// In a standing agent's pod, which process is this session's
		// harness, for attend to know when it has gone (ADR 0053).
		if place == "" && os.Getenv("AGENT_INTERACTIVE") != "" {
			recordHarness(in.SessionID)
		}
		// An interactive session waits at its prompt once started, and its
		// waiter (the same hook with --wait) is listening already.
		state := message.Busy
		if kind == "interactive" {
			state = message.Idle
		}
		if err := present(state); err != nil {
			fmt.Fprintf(os.Stderr, "dispatcher: this session could not be registered for messages: %v\n", err)
			return nil
		}
		if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" {
			if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintf(fh, "export DISPATCHER_SESSION=%s\n", in.SessionID)
				fh.Close()
			}
		}
		agent := or(os.Getenv("AGENT_NAME"), place)
		me := fmt.Sprintf("session/%s, of agent %s", in.SessionID, or(agent, "?"))
		if place != "" && os.Getenv("AGENT_NAME") == "" {
			me = fmt.Sprintf("session/%s, on the owner's machine (%s); you work for the owner but are not the owner, and what you send says so", in.SessionID, place)
		}
		if name != "" && agent != "" {
			me += fmt.Sprintf(", called %s (%s/%s)", name, agent, name)
		}
		// What waits is taken here only by a session whose start is a turn
		// (headless). An interactive one would hold it at an idle prompt
		// (seen 2026-11-27 on a session opened again for a message): its
		// listener takes it instead, which starts a turn.
		var t message.Taken
		if kind != "interactive" {
			t, _ = take("start", 0)
		}
		say("SessionStart", strings.TrimSpace(fmt.Sprintf(intro, me)+"\n\n"+t.Text))
	case "UserPromptSubmit":
		_ = present(message.Busy)
		t, _ := take("prompt", 0)
		say("UserPromptSubmit", t.Text)
	case "PostToolBatch", "PostToolUse":
		t, _ := take("tool", 0)
		say(in.Event, t.Text)
	case "Stop":
		t, err := take("stop", 0)
		if err == nil && len(t.Messages) > 0 {
			// The turn goes on with them.
			say("Stop", t.Text)
			return nil
		}
		_ = present(message.Idle)
	case "SessionEnd":
		_ = present(message.Ended)
	}
	return nil
}

// sessionName is what a session calls itself: DISPATCHER_SESSION_NAME, or
// on the owner's machine, the directory it runs in, so that a session
// started again in the same project has the same name (ADR 0049).
func sessionName(place, cwd string) string {
	if n := os.Getenv("DISPATCHER_SESSION_NAME"); n != "" || place == "" {
		return n
	}
	if cwd == "" {
		return ""
	}
	return safeName(filepath.Base(cwd))
}

// safeName keeps what an address may hold: letters, digits, '.', '_' and '-'.
func safeName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	return strings.TrimLeft(string(b), ".-_")
}

// unattended says whether the harness that runs this hook is a headless run
// (`claude -p`), by what the harness puts in a hook's environment: such a
// run must not be given an idle listener, wherever it was started.
func unattended() bool {
	return os.Getenv("CLAUDE_CODE_ENTRYPOINT") == "sdk-cli" || os.Getenv("CLAUDE_CODE_SESSION_ATTENDED") == "0"
}

// peerLine is one peer: the address to write to (a session's by name when
// one reaches it, else its id shortened, which Resolve takes), then who it
// is and what a message to it does.
func peerLine(p message.Peer, me string) string {
	addr, who := p.Address, or(p.Agent, p.Kind)
	if p.Kind == "session" {
		_, id := message.Split(p.Address)
		short := message.SessionAddress(id[:min(len(id), 8)])
		if p.Named != "" {
			addr, who = p.Named, short
		} else {
			addr = short
			if p.Name != "" {
				who += " " + p.Name
			}
		}
	}
	what := p.State + prefixed(", ", p.Where)
	if p.NotWritable != "" {
		what = p.NotWritable
	}
	mine := ""
	if me != "" && p.Address == message.SessionAddress(me) {
		mine = "  (you)"
	}
	return fmt.Sprintf("%-28s %s: %s%s", addr, who, what, mine)
}
