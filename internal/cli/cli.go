// Package cli is the cmem command: the memory service itself (cmem serve) and
// the client an agent or a person uses from a shell.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"carman/internal/memory"
	"carman/internal/server"
)

const usage = `cmem is the carman memory.

Reading:
  cmem recall [--scope TAG]... [--kind KIND]... [--as-of TIME] [--limit N] WORDS...
        Find entries. Describe what you are doing or looking for in plain words.
  cmem get ID
        Show one entry or episode in full.

Writing:
  cmem note --kind KIND [--here | --global | --scope TAG]...
            (--human NAME | --evidence "what you observed")
            [--replaces ID | --separate] TEXT...
        Remember one thing. KIND is fact, procedure, pitfall or preference.
        --here keeps it to the repository you are in; --global, or no scope,
        shows it to every session everywhere.
        Say where it came from: --human NAME when a person told you or
        corrected you (laptop only; "user" will do for a name), or --evidence
        when you found it out yourself. What a person told you is trusted;
        your own observation is stored as unverified.
        --replaces ID puts the note in place of an entry it corrects or
        updates. The old entry stays in history and stops being current. An
        unverified note cannot replace a trusted entry; flag that instead.
        The replacement keeps the old entry's scope unless you give --scope,
        or --global to make it apply everywhere.
        When memory already holds something close, the note is not stored
        until you say which it is: --replaces ID, or --separate.
  cmem flag ID REASON...
        Report that an entry is wrong or out of date.
  cmem record --outcome passed|failed|unverified [--task-id ID] [--session-id ID]
              [--scope TAG]... [--cost USD]
        Store the record of a finished task, read from standard input.
  cmem import [--scope TAG]... [--global FILE]... [--globals-only]
              [--human NAME] [--dry-run] claude-code DIR
        Copy the memories in a Claude Code memory directory
        (~/.claude/projects/PROJECT/memory) into cmem, as told by NAME.
        Files named with --global apply everywhere and get no scope; the rest
        get --scope, or are left out with --globals-only. Laptop only. Safe
        to run twice: what is already here is skipped.

Consolidation role only. Every change belongs to a run, which is logged,
capped, and can be reverted as a unit:
  cmem consolidate start PURPOSE...
        Open a run. Prints its ID, which the commands below take as --run.
  cmem consolidate entries
        Every current entry, with its sources and the flags against it.
  cmem add --run RUN --kind KIND [--scope TAG]... --source EPISODE_ID... TEXT...
        A new entry resting on episodes.
  cmem supersede ID --run RUN --kind KIND [--scope TAG]... [--global]
                 [--absorb ID]... [--from ID]... [--source EPISODE_ID]... TEXT...
        A new entry in place of entry ID. --absorb merges further entries
        into it. --from ID gives it the sources of entry ID; name every entry
        whose meaning the new one carries, or it loses what backed them.
        The new entry keeps the scope of the entries it replaces. When they
        differ you must say: --scope for each tag it should have, or --global
        for an entry that applies everywhere.
  cmem retire ID --run RUN REASON...
        Withdraw an entry with nothing in its place.
  cmem consolidate log [RUN]
        What runs changed.
  cmem consolidate revert RUN REASON...
        Undo everything a run did.

For harnesses (what puts memory in front of an agent without it asking):
  cmem index [--session ID] [--scope TAG]... [--budget CHARS]
        The compact list for the start of a session.
  cmem flashback [--session ID] [--event NAME] [--scope TAG]... [--name WORD]... TEXT...
        Entries that closely match the text of a moment, such as a prompt or
        an error. Prints nothing when nothing is close enough.
  cmem scope [DIR]
        The scope tag of the repository DIR (or this directory) belongs to.
        It comes from the repository's origin remote, so it is the same in
        every checkout: repo:github.com/owner/name.
  cmem hook [--part N] claude-code
        A Claude Code hook: reads the hook's JSON on standard input and answers
        with the index or a flashback, whichever fits the event. Claude Code
        keeps 10,000 characters per hook, so the index is cut to that; to give
        a session more, list the hook N times at session start as --part 1 to
        --part N and set CMEM_INDEX_PARTS=N.

Service:
  cmem serve [--listen ADDR] [--library DIR]
        Needs CMEM_TOKENS (role=token,role=token). The library is a directory
        of markdown notes in git; set it with --library or CMEM_LIBRARY.
        CMEM_RUN_CAP is how many entries one consolidation run may change
        (20 unless set).
  cmem check [--library DIR]
        Report notes that were changed or removed after they were written.

Client commands need CMEM_URL and CMEM_TOKEN. Add --json for machine output.
A scope tag names what an entry is about, for example repo:carman.
`

// Run executes one cmem command and returns its exit code.
func Run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	c := &command{
		ctx: ctx, getenv: getenv, stdin: stdin, stdout: stdout, stderr: stderr,
		flags: flag.NewFlagSet("cmem "+args[0], flag.ContinueOnError),
	}
	c.flags.SetOutput(stderr)

	commands := map[string]func([]string) error{
		"serve": c.serve, "recall": c.recall, "get": c.get, "note": c.note,
		"flag": c.flag, "record": c.record, "add": c.add, "supersede": c.supersede,
		"index": c.index, "flashback": c.flashback, "hook": c.hook, "check": c.check,
		"retire": c.retire, "consolidate": c.consolidate, "import": c.importMemory, "scope": c.scope,
	}
	run, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "cmem: unknown command %q. Run cmem help.\n", args[0])
		return 2
	}
	if err := run(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "cmem %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type command struct {
	ctx            context.Context
	getenv         func(string) string
	stdin          io.Reader
	stdout, stderr io.Writer
	flags          *flag.FlagSet
	json           bool
	timeout        time.Duration // for calls to the service; zero means 30 seconds
	indexPart      int           // hook only: which part of the session index to print
}

// list is a flag that may be given several times.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// parse reads the flags and returns the remaining words joined as text.
func (c *command) parse(args []string) (string, error) {
	c.flags.BoolVar(&c.json, "json", false, "print JSON")
	if err := c.flags.Parse(args); err != nil {
		return "", err
	}
	return strings.Join(c.flags.Args(), " "), nil
}

func (c *command) serve(args []string) error {
	listen := c.flags.String("listen", "127.0.0.1:8377", "address to listen on")
	library := c.flags.String("library", c.getenv("CMEM_LIBRARY"), "directory holding the library")
	if _, err := c.parse(args); err != nil {
		return err
	}
	if *library == "" {
		return errors.New("say where the library is with --library or CMEM_LIBRARY")
	}
	tokens, err := server.ParseTokens(c.getenv("CMEM_TOKENS"))
	if err != nil {
		return fmt.Errorf("CMEM_TOKENS: %w", err)
	}
	store, err := memory.Open(*library)
	if err != nil {
		return err
	}
	if raw := c.getenv("CMEM_RUN_CAP"); raw != "" {
		if store.RunCap, err = strconv.Atoi(raw); err != nil || store.RunCap < 1 {
			return fmt.Errorf("CMEM_RUN_CAP must be a positive number, got %q", raw)
		}
	}

	log := slog.New(slog.NewTextHandler(c.stderr, nil))
	srv := &http.Server{Addr: *listen, Handler: server.New(store, tokens, log).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-c.ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("cmem serving", "listen", *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (c *command) recall(args []string) error {
	var scope, kinds list
	c.flags.Var(&scope, "scope", "keep entries with this scope tag (repeatable)")
	c.flags.Var(&kinds, "kind", "keep entries of this kind (repeatable)")
	asOf := c.flags.String("as-of", "", "what was current at this RFC 3339 time")
	limit := c.flags.Int("limit", 0, "how many entries to return")
	text, err := c.parse(args)
	if err != nil {
		return err
	}
	params := url.Values{"q": {text}, "scope": scope, "kind": kinds}
	if *asOf != "" {
		params.Set("as_of", *asOf)
	}
	if *limit > 0 {
		params.Set("limit", strconv.Itoa(*limit))
	}
	var entries []memory.Entry
	if err := c.call(http.MethodGet, "/v1/recall?"+params.Encode(), nil, &entries); err != nil {
		return err
	}
	// Search is by shared words, so a note phrased differently from the
	// question is missed. When nothing matches, show what memory does hold,
	// so that a miss in wording is not mistaken for an empty memory.
	var held server.IndexResponse
	if len(entries) == 0 && !c.json {
		if err := c.call(http.MethodPost, "/v1/index", server.IndexRequest{Scope: scope, Budget: missBudget}, &held); err != nil {
			return err
		}
	}
	return c.print(entries, func() {
		switch {
		case len(entries) > 0:
			for _, e := range entries {
				printEntry(c.stdout, e)
			}
		case held.Total == 0:
			fmt.Fprintln(c.stdout, "Nothing in memory matches, and memory holds no entries here.")
		default:
			fmt.Fprintf(c.stdout, "Nothing in memory shares words with that. Memory holds %d entries here; these are the most trusted and newest:\n", held.Total)
			for _, e := range held.Entries {
				fmt.Fprint(c.stdout, entryLine(e))
			}
		}
	})
}

// missBudget is how much of memory a recall that matched nothing lists.
const missBudget = 2000

func (c *command) get(args []string) error {
	id, err := c.parse(args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("give the ID of an entry or episode")
	}
	var entry memory.Entry
	err = c.call(http.MethodGet, "/v1/entries/"+url.PathEscape(id), nil, &entry)
	if err == nil {
		return c.print(entry, func() { printEntry(c.stdout, entry) })
	}
	if !isStatus(err, http.StatusNotFound) {
		return err
	}
	var episode memory.Episode
	if err := c.call(http.MethodGet, "/v1/episodes/"+url.PathEscape(id), nil, &episode); err != nil {
		return err
	}
	return c.print(episode, func() { printEpisode(c.stdout, episode) })
}

func (c *command) note(args []string) error {
	var req server.NoteRequest
	var scope list
	c.flags.StringVar((*string)(&req.Kind), "kind", "", "fact, procedure, pitfall or preference")
	c.flags.Var(&scope, "scope", "what this is about, for example repo:carman (repeatable)")
	c.flags.StringVar(&req.Evidence, "evidence", "", "what you observed that makes you believe this")
	c.flags.StringVar(&req.Human, "human", "", "the person who told you (laptop only)")
	c.flags.StringVar(&req.Replaces, "replaces", "", "ID of the entry this note corrects or updates")
	c.flags.BoolVar(&req.Separate, "separate", false, "store it even though memory holds something close")
	c.flags.BoolVar(&req.Global, "global", false, "it applies everywhere; with --replaces, whatever scope the old entry had")
	here := c.flags.Bool("here", false, "it is about the repository you are in, and is shown only there")
	c.flags.StringVar(&req.TaskID, "task-id", c.getenv("CMEM_TASK_ID"), "the task this belongs to")
	c.flags.StringVar(&req.SessionID, "session-id", c.getenv("CMEM_SESSION_ID"), "the session this belongs to")
	text, err := c.parse(args)
	if err != nil {
		return err
	}
	req.Text, req.Scope = text, scope
	if *here {
		dir, err := c.workingDir()
		if err != nil {
			return err
		}
		req.Scope = append(req.Scope, c.sessionScope(dir)...)
	}
	// A harness can require the agent to say who a new note is for, so that
	// "everywhere" is a choice made each time and not what happens when the
	// question is skipped. A replacement keeps the old entry's scope.
	if c.getenv("CMEM_NOTE_SCOPE") == "ask" && req.Replaces == "" && !req.Global && len(req.Scope) == 0 {
		return errors.New("say who this note is for: --here if it is true of this repository only (its code, its " +
			"commands, its own conventions, something you were allowed to do in it), or --global if it would also " +
			"hold in another repository (what the user prefers, how their machine is set up, how their team or " +
			"organisation does things everywhere)")
	}
	var entry memory.Entry
	err = c.call(http.MethodPost, "/v1/notes", req, &entry)
	var refused *statusError
	switch {
	case errors.As(err, &refused) && len(refused.similar) > 0 && !c.json:
		// Said on standard output, where an agent reads results: this is a
		// question to answer, not a failure to work around.
		fmt.Fprintln(c.stdout, "Not recorded yet. Memory already holds something close:")
		for _, e := range refused.similar {
			fmt.Fprint(c.stdout, entryLine(e))
		}
		fmt.Fprintln(c.stdout, "If your note corrects or updates one of these, run the same command again with "+
			"--replaces ID. The old entry stays in history and yours becomes the current one.\n"+
			"If it is a different thing, run the same command again with --separate.")
		return errors.New("not recorded: say --replaces ID or --separate")
	case isStatus(err, http.StatusConflict) && strings.Contains(err.Error(), "cannot replace a trusted entry"):
		return fmt.Errorf("%w. Entry %s rests on what a person said or a verified task showed, and this note on "+
			"your own observation. If you believe it is wrong, run: cmem flag %s REASON", err, req.Replaces, req.Replaces)
	case err != nil:
		return err
	}
	return c.print(entry, func() {
		fmt.Fprintln(c.stdout, "Remembered:")
		printEntry(c.stdout, entry)
	})
}

func (c *command) flag(args []string) error {
	if _, err := c.parse(args); err != nil {
		return err
	}
	words := c.flags.Args()
	if len(words) < 2 {
		return errors.New("give the entry ID and the reason it is wrong")
	}
	var entry memory.Entry
	req := server.FlagRequest{Reason: strings.Join(words[1:], " ")}
	if err := c.call(http.MethodPost, "/v1/entries/"+url.PathEscape(words[0])+"/flag", req, &entry); err != nil {
		return err
	}
	return c.print(entry, func() {
		fmt.Fprintln(c.stdout, "Flagged:")
		printEntry(c.stdout, entry)
	})
}

func (c *command) record(args []string) error {
	var req server.EpisodeRequest
	var scope list
	c.flags.StringVar((*string)(&req.Outcome), "outcome", "", "passed, failed or unverified")
	c.flags.StringVar(&req.TaskID, "task-id", c.getenv("CMEM_TASK_ID"), "the task this belongs to")
	c.flags.StringVar(&req.SessionID, "session-id", c.getenv("CMEM_SESSION_ID"), "the session this belongs to")
	c.flags.Var(&scope, "scope", "what this is about (repeatable)")
	cost := c.flags.Float64("cost", -1, "what the task cost, in US dollars")
	if _, err := c.parse(args); err != nil {
		return err
	}
	body, err := io.ReadAll(c.stdin)
	if err != nil {
		return err
	}
	req.Body, req.Scope = strings.TrimSpace(string(body)), scope
	if *cost >= 0 {
		req.CostUSD = cost
	}
	var episode memory.Episode
	if err := c.call(http.MethodPost, "/v1/episodes", req, &episode); err != nil {
		return err
	}
	return c.print(episode, func() { fmt.Fprintf(c.stdout, "Recorded episode %s\n", episode.ID) })
}

func (c *command) add(args []string) error {
	return c.curate(args, func(string) string { return "/v1/entries" }, 0)
}

func (c *command) supersede(args []string) error {
	return c.curate(args, func(id string) string { return "/v1/entries/" + url.PathEscape(id) + "/supersede" }, 1)
}

// curate implements add and supersede. Supersede takes the ID of the entry to
// replace as its first argument, before the flags.
func (c *command) curate(args []string, path func(id string) string, leading int) error {
	if len(args) < leading {
		return errors.New("give the ID of the entry to supersede")
	}
	id := ""
	if leading == 1 {
		id, args = args[0], args[1:]
	}
	var req server.EntryRequest
	var scope, sources, from, absorbs list
	c.flags.StringVar(&req.Run, "run", c.getenv("CMEM_RUN"), "the consolidation run this belongs to")
	c.flags.StringVar((*string)(&req.Kind), "kind", "", "fact, procedure, pitfall or preference")
	c.flags.Var(&scope, "scope", "what this is about (repeatable)")
	c.flags.Var(&sources, "source", "ID of an episode this rests on (repeatable)")
	c.flags.Var(&from, "from", "ID of an entry whose sources this inherits (repeatable)")
	if leading == 1 {
		c.flags.Var(&absorbs, "absorb", "ID of a further entry merged into this one (repeatable)")
		c.flags.BoolVar(&req.Global, "global", false, "the new entry applies everywhere, whatever scope the old ones had")
	}
	text, err := c.parse(args)
	if err != nil {
		return err
	}
	req.Text, req.Scope, req.From, req.Absorbs = text, scope, from, absorbs
	for _, episodeID := range sources {
		req.Sources = append(req.Sources, memory.Source{EpisodeID: episodeID})
	}
	var entry memory.Entry
	if err := c.call(http.MethodPost, path(id), req, &entry); err != nil {
		return err
	}
	return c.print(entry, func() { printEntry(c.stdout, entry) })
}

func (c *command) retire(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("give the ID of the entry to retire, then --run RUN and the reason")
	}
	id := args[0]
	req := server.RetireRequest{}
	c.flags.StringVar(&req.Run, "run", c.getenv("CMEM_RUN"), "the consolidation run this belongs to")
	reason, err := c.parse(args[1:])
	if err != nil {
		return err
	}
	req.Reason = reason
	var entry memory.Entry
	if err := c.call(http.MethodPost, "/v1/entries/"+url.PathEscape(id)+"/retire", req, &entry); err != nil {
		return err
	}
	return c.print(entry, func() {
		fmt.Fprintln(c.stdout, "Retired:")
		printEntry(c.stdout, entry)
	})
}

// consolidate holds what the consolidation role does around its changes:
// opening a run, reading what is there, and reading or undoing what runs did.
func (c *command) consolidate(args []string) error {
	text, err := c.parse(args)
	if err != nil {
		return err
	}
	words := c.flags.Args()
	if len(words) == 0 {
		return errors.New("say which: start PURPOSE, entries, log [RUN], or revert RUN REASON")
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, words[0]))
	switch words[0] {
	case "start":
		var run memory.Run
		if err := c.call(http.MethodPost, "/v1/runs", server.RunRequest{Purpose: rest}, &run); err != nil {
			return err
		}
		return c.print(run, func() {
			fmt.Fprintf(c.stdout, "Run %s is open. It may add, replace or retire at most %d entries.\n"+
				"Pass --run %s to cmem add, cmem supersede and cmem retire.\n", run.ID, run.Cap, run.ID)
		})
	case "entries":
		var entries []memory.Entry
		if err := c.call(http.MethodGet, "/v1/entries", nil, &entries); err != nil {
			return err
		}
		return c.print(entries, func() {
			fmt.Fprintf(c.stdout, "%d current entries, oldest first.\n", len(entries))
			for _, e := range entries {
				printEntry(c.stdout, e)
			}
		})
	case "log":
		var runs []memory.Run
		if len(words) > 1 {
			var run memory.Run
			if err := c.call(http.MethodGet, "/v1/runs/"+url.PathEscape(words[1]), nil, &run); err != nil {
				return err
			}
			runs = []memory.Run{run}
		} else if err := c.call(http.MethodGet, "/v1/runs", nil, &runs); err != nil {
			return err
		}
		return c.print(runs, func() {
			if len(runs) == 0 {
				fmt.Fprintln(c.stdout, "No consolidation run has been made.")
			}
			for _, run := range runs {
				printRun(c.stdout, run)
			}
		})
	case "revert":
		if len(words) < 3 {
			return errors.New("give the run ID and the reason for undoing it")
		}
		var run memory.Run
		req := server.RevertRequest{Reason: strings.Join(words[2:], " ")}
		if err := c.call(http.MethodPost, "/v1/runs/"+url.PathEscape(words[1])+"/revert", req, &run); err != nil {
			return err
		}
		return c.print(run, func() {
			fmt.Fprintln(c.stdout, "Reverted. What the run added is no longer current, and what it replaced or retired is current again.")
			printRun(c.stdout, run)
		})
	}
	return fmt.Errorf("unknown: %s. Say start, entries, log or revert", words[0])
}

func printRun(w io.Writer, run memory.Run) {
	fmt.Fprintf(w, "run %s  %s  %d of at most %d entries changed", run.ID, run.CreatedAt.Format(time.RFC3339), run.Size, run.Cap)
	if run.RevertedAt != nil {
		fmt.Fprintf(w, "  REVERTED %s: %s", run.RevertedAt.Format(time.RFC3339), run.Reverted)
	}
	fmt.Fprintf(w, "\n    %s\n", run.Purpose)
	for _, change := range run.Changes {
		text := strings.Join(strings.Fields(change.Text), " ")
		switch change.Action {
		case "supersede":
			fmt.Fprintf(w, "    - %s replaced %s: %s\n", change.Entry, strings.Join(change.Replaced, ", "), text)
		case "retire":
			fmt.Fprintf(w, "    - retired %s: %s\n", change.Entry, text)
		default:
			fmt.Fprintf(w, "    - added %s: %s\n", change.Entry, text)
		}
	}
}

func (c *command) check(args []string) error {
	library := c.flags.String("library", c.getenv("CMEM_LIBRARY"), "directory holding the library")
	if _, err := c.parse(args); err != nil {
		return err
	}
	if *library == "" {
		return errors.New("say where the library is with --library or CMEM_LIBRARY")
	}
	store, err := memory.Open(*library)
	if err != nil {
		return err
	}
	problems, err := store.Check()
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		fmt.Fprintln(c.stdout, "The library is intact: every note is as it was written.")
		return nil
	}
	for _, problem := range problems {
		fmt.Fprintln(c.stdout, problem)
	}
	return fmt.Errorf("%d problem(s) found", len(problems))
}

func (c *command) index(args []string) error {
	var req server.IndexRequest
	var scope list
	c.flags.StringVar(&req.SessionID, "session", c.getenv("CMEM_SESSION_ID"), "the session this is for")
	c.flags.Var(&scope, "scope", "the session's scope tag (repeatable)")
	c.flags.IntVar(&req.Budget, "budget", 0, "roughly how many characters of entry text to return")
	if _, err := c.parse(args); err != nil {
		return err
	}
	req.Scope = scope
	var resp server.IndexResponse
	if err := c.call(http.MethodPost, "/v1/index", req, &resp); err != nil {
		return err
	}
	return c.print(resp, func() { fmt.Fprint(c.stdout, indexText(resp, req.Scope)) })
}

func (c *command) flashback(args []string) error {
	var req server.FlashbackRequest
	var scope, names list
	c.flags.StringVar(&req.SessionID, "session", c.getenv("CMEM_SESSION_ID"), "the session this is for")
	c.flags.StringVar(&req.Event, "event", "manual", "what prompted this, for the log")
	c.flags.Var(&scope, "scope", "the session's scope tag (repeatable)")
	c.flags.Var(&names, "name", "a word that alone makes an entry relevant, such as a command about to run (repeatable)")
	c.flags.IntVar(&req.Limit, "limit", 0, "at most this many entries")
	text, err := c.parse(args)
	if err != nil {
		return err
	}
	if text == "" {
		piped, err := io.ReadAll(c.stdin)
		if err != nil {
			return err
		}
		text = string(piped)
	}
	req.Text, req.Scope, req.Names = text, scope, names
	var entries []memory.Entry
	if err := c.call(http.MethodPost, "/v1/flashback", req, &entries); err != nil {
		return err
	}
	return c.print(entries, func() { fmt.Fprint(c.stdout, flashbackText("From memory (cmem):", entries)) })
}

// entryLine is one entry as it is put into an agent's context: as short as it
// can be while still saying what kind it is, whether it is confirmed, and how
// to look it up.
func entryLine(e memory.Entry) string {
	text := strings.Join(strings.Fields(e.Text), " ")
	if runes := []rune(text); len(runes) > memory.IndexLineLimit {
		text = string(runes[:memory.IndexLineLimit]) + "…"
	}
	mark := ""
	if e.Status == memory.StatusUnverified {
		mark = ", unverified"
	}
	if e.Contradictions > 0 {
		mark += ", disputed"
	}
	return fmt.Sprintf("- [%s%s] %s (%s)\n", e.Kind, mark, text, e.ID)
}

// indexHelpAsk replaces a sentence of the help when the harness requires a
// note to say who it is for (CMEM_NOTE_SCOPE=ask).
//
// The first wording said global was for what is "about the user or this
// machine". Agents took that at its word and kept an organisation-wide rule
// ("every JavaScript project here uses pnpm") to one repository, so it did
// not travel. The test is now whether it would hold in another repository.
const indexHelpAsk = "then who it is for: `--here` when it is true of this repository only (its code, its commands, " +
	"its own conventions, something you were allowed to do in it), or `--global` when it would also hold in " +
	"another repository (what the user prefers, how their machine is set up, how their team or organisation " +
	"does things everywhere); then where it came from, then the text: `--human NAME` when the user told you or " +
	"corrected you "

const indexHelpWhere = "then where it came from, then the text: `--human NAME` when the user told you or corrected you "

const indexHelp = "Search the rest with `cmem recall WORDS`. `cmem get ID` shows an entry's sources. " +
	"When you learn something worth keeping, record it with `cmem note --kind fact|procedure|pitfall|preference`, " +
	indexHelpWhere +
	"(use `user` if you do not know their name), or `--evidence \"what you saw\"` when you found it out yourself. " +
	"If an entry is out of date or wrong and you know what is right, record that in its place: " +
	"add `--replaces ID` to the note. If you only know it is wrong, run `cmem flag ID REASON`. An entry marked unverified is an agent's own observation " +
	"that nothing has checked yet: use it, and check it first when being wrong would be costly.\n"

func indexText(resp server.IndexResponse, scope []string) string {
	var b strings.Builder
	about := ""
	if len(scope) > 0 {
		about = " for " + strings.Join(scope, ", ")
	}
	switch {
	case resp.Total == 0:
		fmt.Fprintf(&b, "Long-term memory (cmem) has no entries%s yet.\n", about)
	case len(resp.Entries) < resp.Total:
		fmt.Fprintf(&b, "Long-term memory (cmem): %d of %d entries%s.\n", len(resp.Entries), resp.Total, about)
	case resp.Total == 1:
		fmt.Fprintf(&b, "Long-term memory (cmem): 1 entry%s.\n", about)
	default:
		fmt.Fprintf(&b, "Long-term memory (cmem): %d entries%s.\n", resp.Total, about)
	}
	for _, e := range resp.Entries {
		b.WriteString(entryLine(e))
	}
	b.WriteString(indexHelp)
	return b.String()
}

// flashbackText is empty when there is nothing to say, so that a hook adds
// nothing to the context.
func flashbackText(heading string, entries []memory.Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(heading + "\n")
	for _, e := range entries {
		b.WriteString(entryLine(e))
	}
	return b.String()
}

func (c *command) print(value any, text func()) error {
	if !c.json {
		text()
		return nil
	}
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

type statusError struct {
	status  int
	message string
	similar []memory.Entry // on a refused note: the entries it is close to
}

func (e *statusError) Error() string { return e.message }

func isStatus(err error, status int) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == status
}

func (c *command) call(method, path string, in, out any) error {
	base, token := c.getenv("CMEM_URL"), c.getenv("CMEM_TOKEN")
	if base == "" || token == "" {
		return errors.New("CMEM_URL and CMEM_TOKEN must be set")
	}
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	timeout := c.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the memory service at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var failure struct {
			Error   string         `json:"error"`
			Similar []memory.Entry `json:"similar"`
		}
		json.NewDecoder(resp.Body).Decode(&failure)
		if failure.Error == "" {
			failure.Error = resp.Status
		}
		return &statusError{resp.StatusCode, failure.Error, failure.Similar}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func printEntry(w io.Writer, e memory.Entry) {
	fmt.Fprintf(w, "%s  %s  %s", e.ID, e.Kind, e.Status)
	if len(e.Scope) > 0 {
		fmt.Fprintf(w, "  [%s]", strings.Join(e.Scope, ", "))
	}
	fmt.Fprintf(w, "\n    %s\n", e.Text)

	sources := make([]string, len(e.Sources))
	for i, src := range e.Sources {
		if src.Human != "" {
			sources[i] = "told by " + src.Human
		} else {
			sources[i] = fmt.Sprintf("episode %s (%s)", src.EpisodeID, src.Outcome)
		}
	}
	fmt.Fprintf(w, "    sources: %s\n", strings.Join(sources, "; "))
	if e.Supersedes != nil {
		fmt.Fprintf(w, "    replaces %s\n", strings.Join(append([]string{*e.Supersedes}, e.Absorbs...), ", "))
	}
	if e.Contradictions > 0 {
		fmt.Fprintf(w, "    flagged as wrong %d time(s)\n", e.Contradictions)
		for _, f := range e.Flags {
			fmt.Fprintf(w, "      - %s (%s, %s)\n", strings.Join(strings.Fields(f.Reason), " "), f.Origin, f.CreatedAt.Format("2006-01-02"))
		}
	}
	if e.ValidTo != nil {
		fmt.Fprintf(w, "    no longer current since %s\n", e.ValidTo.Format(time.RFC3339))
	}
}

func printEpisode(w io.Writer, e memory.Episode) {
	fmt.Fprintf(w, "episode %s  %s  %s  %s", e.ID, e.Origin, e.Outcome, e.CreatedAt.Format(time.RFC3339))
	if len(e.Scope) > 0 {
		fmt.Fprintf(w, "  [%s]", strings.Join(e.Scope, ", "))
	}
	fmt.Fprintf(w, "\n%s\n", e.Body)
}
