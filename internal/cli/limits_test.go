package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// ADR 0044: a note that adds detail to an entry stands beside it, and the
// entry shows where more is without being marked corrected or disputed.
func TestANoteCanAddToAnEntryWithoutCorrectingIt(t *testing.T) {
	laptop, worker, dreamer := newService(t)
	restore := strings.Fields(laptop.ok("", "note", "--kind", "fact", "--human", "user", "--global",
		"The staging database is restored from the nightly snapshot every Sunday at 03:00 UTC."))[1]
	out := worker.ok("", "note", "--kind", "fact", "--evidence", "read the restore job's script", "--adds-to", restore,
		"After the Sunday staging restore, scripts/anonymise.sh runs on the restored data.")
	added := strings.Fields(out)[1]
	expectContains(t, out, restore+" is now marked as added to", "It is not marked as corrected")

	got := laptop.ok("", "get", restore)
	expectContains(t, got, "fact, added to", "Added to by "+added, "03:00 UTC")
	if strings.Contains(got, "corrected in part") || strings.Contains(got, "disputed") || strings.Contains(got, "Flagged as wrong") {
		t.Errorf("an entry that was only added to reads as corrected:\n%s", got)
	}
	expectContains(t, laptop.ok("", "recall", "staging", "restore", "Sunday"), "fact, added to", "Added to by "+added)
	var entry struct{ Contradictions int }
	json.Unmarshal([]byte(laptop.ok("", "get", "--json", restore)), &entry)
	if entry.Contradictions != 0 {
		t.Errorf("an addition counts as a contradiction: %d", entry.Contradictions)
	}

	// Only one of the two, and never with --replaces.
	for _, both := range [][]string{{"--amends", restore, "--adds-to", restore}, {"--replaces", restore, "--adds-to", restore}} {
		if _, stderr, code := worker.run("", append(append([]string{"note", "--kind", "fact", "--evidence", "e"}, both...), "x")...); code == 0 || !strings.Contains(stderr, "Choose one") {
			t.Errorf("cmem note %v: exit %d, %s", both, code, stderr)
		}
	}

	// A note held as close to an entry is offered the addition as well.
	stdout, _, _ := laptop.run("", "note", "--kind", "fact", "--human", "user", "--global",
		"The staging database is restored from the nightly snapshot every Sunday at 03:00 UTC, by the restore job.")
	expectContains(t, stdout, "--adds-to ID")

	// The consolidation role can read a correction as an addition, in a run,
	// on the owner's word; reverting the run undoes it.
	fixed := worker.ok("", "note", "--kind", "fact", "--evidence", "the job's log", "--amends", restore,
		"The Sunday staging restore takes about forty minutes.")
	fixedID := strings.Fields(fixed)[1]
	expectContains(t, laptop.ok("", "get", restore), "corrected in part", "Corrected in part by "+fixedID)
	if _, stderr, code := worker.run("", "relabel", restore, "--run", "x", "nothing was corrected"); code == 0 || !strings.Contains(stderr, "may not") {
		t.Errorf("a working agent relabelled: exit %d, %s", code, stderr)
	}
	run := strings.Fields(dreamer.ok("", "consolidate", "start", "The owner: a note added detail and corrected nothing"))[1]
	out = dreamer.ok("", "relabel", restore, "--run", run, "--note", fixedID, "The owner says the note adds the duration and corrects nothing.")
	expectContains(t, out, "Relabelled:", "fact, added to", "Added to by "+fixedID, "consolidation run "+run+" found that it corrects nothing")
	expectContains(t, dreamer.ok("", "consolidate", "log", run), "marked "+restore+" as added to, not corrected: The owner says")
	dreamer.ok("", "consolidate", "revert", run, "the owner changed their mind")
	expectContains(t, laptop.ok("", "get", restore), "corrected in part", "Corrected in part by "+fixedID)
}

// ADR 0044: a routine that may run only the memory command can be given
// directories to read and MCP tools to call, and is told its limits. These
// are the flags the harness gets, in `cmem routine` and in agent-resume.
func TestARestrictedRoutineGetsExactlyWhatItIsAllowedAndIsToldSo(t *testing.T) {
	flagsFor := func(env map[string]string) []string {
		t.Helper()
		var out, errOut strings.Builder
		getenv := func(key string) string { return env[key] }
		if code := Run(context.Background(), []string{"routine", "--flags"}, getenv, nil, &out, &errOut); code != 0 {
			t.Fatalf("cmem routine --flags: exit %d: %s", code, errOut.String())
		}
		// What sh reads back from the printed line.
		words, err := exec.Command("sh", "-c", `eval "set -- $0"; printf '%s\0' "$@"`, out.String()).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(words), "\x00"), "\x00")
	}
	after := func(flags []string, name string) []string {
		i := slices.Index(flags, name)
		if i < 0 {
			return nil
		}
		var values []string
		for _, f := range flags[i+1:] {
			if strings.HasPrefix(f, "--") {
				break
			}
			values = append(values, f)
		}
		return values
	}

	flags := flagsFor(map[string]string{
		"CMEM_ROUTINE_READ":       "/etc/agent/definitions:/srv/more/",
		"CMEM_ROUTINE_MCP_TOOLS":  "mcp__tracker__get_issue, mcp__tracker__list_comments,Bash(rm *),Write",
		"CMEM_ROUTINE_MCP_CONFIG": "/etc/agent/mcp/mcp.json",
	})
	want := map[string][]string{
		"--permission-mode": {"dontAsk"},
		"--tools":           {"Bash,Read,Glob,Grep"},
		"--allowedTools": {"Bash(cmem *)", "Read(//etc/agent/definitions/**)", "Read(//srv/more/**)",
			"mcp__tracker__get_issue", "mcp__tracker__list_comments"},
		"--add-dir":    {"/etc/agent/definitions", "/srv/more"},
		"--mcp-config": {"/etc/agent/mcp/mcp.json"},
	}
	for name, values := range want {
		if got := after(flags, name); !slices.Equal(got, values) {
			t.Errorf("%s %v, want %v\nall: %q", name, got, values, flags)
		}
	}
	if !slices.Contains(flags, "--strict-mcp-config") {
		t.Errorf("other MCP servers are not shut out: %q", flags)
	}
	told := after(flags, "--append-system-prompt")
	if len(told) != 1 {
		t.Fatalf("the agent is not told its limits: %q", flags)
	}
	expectContains(t, told[0], "`cmem` command", "/etc/agent/definitions and /srv/more", "mcp__tracker__get_issue, mcp__tracker__list_comments",
		"Anything else is refused", "joined with `;`, `&&` or new lines", "refused whole, and none of it runs", "does not take a tool away")
	if strings.Contains(told[0], "rm *") || strings.Contains(told[0], "Write") {
		t.Errorf("a name that is not an MCP tool's widened what the agent is told it may do:\n%s", told[0])
	}

	// With nothing to read, Read stays for the harness's own long outputs,
	// and the agent is told only of the memory command.
	flags = flagsFor(map[string]string{})
	if after(flags, "--add-dir") != nil || slices.Contains(flags, "--mcp-config") || !slices.Equal(after(flags, "--allowedTools"), []string{"Bash(cmem *)"}) {
		t.Errorf("a routine given nothing more got more: %q", flags)
	}
	if told := after(flags, "--append-system-prompt"); len(told) != 1 || strings.Contains(told[0], "read the files") {
		t.Errorf("told %q", told)
	}

	// An agent with every tool is not told of limits it does not have.
	flags = flagsFor(map[string]string{"CMEM_ROUTINE_TOOLS": "all", "CMEM_ROUTINE_MCP_CONFIG": "/etc/agent/mcp/mcp.json"})
	if !slices.Equal(after(flags, "--permission-mode"), []string{"bypassPermissions"}) || slices.Contains(flags, "--append-system-prompt") ||
		!slices.Equal(after(flags, "--mcp-config"), []string{"/etc/agent/mcp/mcp.json"}) || slices.Contains(flags, "--strict-mcp-config") {
		t.Errorf("every tool: %q", flags)
	}
}

// ADR 0044: every session is told what memory is not for, and so is the help.
func TestSessionsAreToldWhatMemoryIsNotFor(t *testing.T) {
	laptop, _, _ := newService(t)
	laptop.ok("", "note", "--kind", "preference", "--human", "user", "--global", "Answers open with the conclusion.")
	for _, text := range []string{
		hookContext(t, laptop, map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "cwd": t.TempDir()}),
		laptop.ok("", "help"),
	} {
		expectContains(t, strings.Join(strings.Fields(text), " "), "what would help another agent on another task", "instruction for one job or agent",
			"change that job's definition", "present state of a system", "ask the system", "the project's decision records")
	}
	expectContains(t, laptop.ok("", "help"), "--adds-to ID")
	expectContains(t, laptop.ok("", "help", "all"), "cmem relabel ID --run RUN")
}

// ADR 0044, measured 2026-11-25: a restricted routine can be given command
// prefixes that only read, set by the installation. Each is an allow rule
// beside the others and is named in what the agent is told; one that is not
// plainly a read is left out, and said.
func TestARestrictedRoutineCanBeGivenReadOnlyCommands(t *testing.T) {
	env := map[string]string{"CMEM_ROUTINE_COMMANDS": "gh pr view, gh  issue view,gh pr list, gh, gh pr, gh pr edit, gh api, " +
		"gh pr view --web, sh -c view, env gh pr view, gh pr view *, gh pr view; rm, GH PR VIEW, git log, gh pr view"}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"routine", "--flags"}, func(k string) string { return env[k] }, nil, &out, &errOut); code != 0 {
		t.Fatalf("cmem routine --flags: exit %d: %s", code, errOut.String())
	}
	words, err := exec.Command("sh", "-c", `eval "set -- $0"; printf '%s\0' "$@"`, out.String()).Output()
	if err != nil {
		t.Fatal(err)
	}
	flags := strings.Split(strings.TrimSuffix(string(words), "\x00"), "\x00")
	i := slices.Index(flags, "--allowedTools")
	j := slices.Index(flags, "--append-system-prompt")
	if i < 0 || j < 0 {
		t.Fatalf("flags: %q", flags)
	}
	want := []string{"Bash(cmem *)", "Bash(gh pr view *)", "Bash(gh issue view *)", "Bash(gh pr list *)", "Bash(git log *)"}
	if got := flags[i+1 : i+1+len(want)]; !slices.Equal(got, want) || strings.HasPrefix(flags[i+1+len(want)], "Bash(") {
		t.Errorf("allowed %q, want %q", flags[i+1:j], want)
	}
	expectContains(t, flags[j+1], "which only read", "`gh pr view`, `gh issue view`, `gh pr list`")
	for _, bad := range []string{`"gh"`, `"gh pr"`, `"gh pr edit"`, `"gh api"`, `"gh pr view --web"`, `"sh -c view"`, `"env gh pr view"`,
		`"gh pr view *"`, `"gh pr view; rm"`, `"GH PR VIEW"`} {
		if !strings.Contains(errOut.String(), bad+" is refused") {
			t.Errorf("%s was not said to be refused:\n%s", bad, errOut.String())
		}
	}
	if strings.Count(errOut.String(), "is refused") != 10 {
		t.Errorf("refused other than the ten:\n%s", errOut.String())
	}

	// An agent with every tool needs none of it.
	env["CMEM_ROUTINE_TOOLS"] = "all"
	out.Reset()
	Run(context.Background(), []string{"routine", "--flags"}, func(k string) string { return env[k] }, nil, &out, &errOut)
	if strings.Contains(out.String(), "gh pr view") {
		t.Errorf("every tool: %s", out.String())
	}
}

// ADR 0061: an agent held to named tools gets only what its entry names,
// with nothing given by default: not the memory command, and not the
// dispatcher's client beyond the commands named, even where the pod could
// message. Measured 2026-11-27 with the harness (2.1.326) and these flags
// for the surveyor: every form of `dispatcher improve` its routine uses ran,
// a heredoc into it among them; `dispatcher send`, `gh api`, `cmem`, `ls`,
// `dispatcher improvement` and a chain with `dispatcher send` in it were refused.
func TestAnAgentCanBeHeldToNamedToolsAndCommands(t *testing.T) {
	identity := t.TempDir() + "/token"
	if err := os.WriteFile(identity, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	flagsFor := func(env map[string]string) ([]string, string) {
		t.Helper()
		var out, errOut strings.Builder
		if code := Run(context.Background(), []string{"routine", "--flags"}, func(k string) string { return env[k] }, nil, &out, &errOut); code != 0 {
			t.Fatalf("cmem routine --flags: exit %d: %s", code, errOut.String())
		}
		words, err := exec.Command("sh", "-c", `eval "set -- $0"; printf '%s\0' "$@"`, out.String()).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(words), "\x00"), "\x00"), errOut.String()
	}
	between := func(flags []string, from, to string) []string {
		i, j := slices.Index(flags, from), slices.Index(flags, to)
		if i < 0 || j < i {
			t.Fatalf("no %s before %s: %q", from, to, flags)
		}
		return flags[i+1 : j]
	}

	// The surveyor, as the trial's values have it.
	surveyor := map[string]string{"CMEM_ROUTINE_TOOLS": "named", "CMEM_ROUTINE_MEMORY": "off", "CMEM_ROUTINE_RUN": "dispatcher improve",
		"DISPATCHER_URL": "http://dispatcher:8379", "DISPATCHER_IDENTITY": identity}
	flags, said := flagsFor(surveyor)
	if got, want := between(flags, "--allowedTools", "--append-system-prompt"), []string{"Bash(dispatcher improve)", "Bash(dispatcher improve *)"}; !slices.Equal(got, want) {
		t.Errorf("allowed %q, want %q", got, want)
	}
	if !slices.Equal(between(flags, "--permission-mode", "--tools"), []string{"dontAsk"}) || said != "" {
		t.Errorf("flags %q, said %q", flags, said)
	}
	told := flags[slices.Index(flags, "--append-system-prompt")+1]
	expectContains(t, told, "with any subcommands and arguments: `dispatcher improve`", "Anything else is refused", "refused whole")
	if strings.Contains(told, "`cmem`") || strings.Contains(told, "for messages") {
		t.Errorf("the surveyor is told of what it was not given:\n%s", told)
	}

	// Everything an entry may name, and what is not a command, said and left out.
	flags, said = flagsFor(map[string]string{"CMEM_ROUTINE_TOOLS": "named", "CMEM_ROUTINE_MEMORY": "on",
		"CMEM_ROUTINE_RUN":       "dispatcher improve, gh, sh -c x, dispatcher improve --all, dispatcher improve",
		"CMEM_ROUTINE_COMMANDS":  "gh pr view, gh pr edit",
		"CMEM_ROUTINE_MCP_TOOLS": "mcp__tracker__get_issue,Write", "CMEM_ROUTINE_MCP_CONFIG": "/etc/agent/mcp/mcp.json",
		"CMEM_ROUTINE_READ": "/etc/agent/routines"})
	want := []string{"Bash(cmem *)", "Bash(gh pr view *)", "Bash(dispatcher improve)", "Bash(dispatcher improve *)",
		"Read(//etc/agent/routines/**)", "mcp__tracker__get_issue"}
	if got := between(flags, "--allowedTools", "--add-dir"); !slices.Equal(got, want) {
		t.Errorf("allowed %q, want %q", got, want)
	}
	if !slices.Contains(flags, "--strict-mcp-config") {
		t.Errorf("other MCP servers are not shut out: %q", flags)
	}
	for _, bad := range []string{`CMEM_ROUTINE_RUN: "gh" is refused`, `CMEM_ROUTINE_RUN: "sh -c x" is refused`,
		`CMEM_ROUTINE_RUN: "dispatcher improve --all" is refused`, `CMEM_ROUTINE_COMMANDS: "gh pr edit" is refused`} {
		if !strings.Contains(said, bad) {
			t.Errorf("not said: %s\n%s", bad, said)
		}
	}

	// Nothing named: no shell at all, and Read for the harness's own outputs.
	flags, _ = flagsFor(map[string]string{"CMEM_ROUTINE_TOOLS": "named"})
	if !slices.Equal(between(flags, "--tools", "--allowedTools"), []string{"Read,Glob,Grep"}) {
		t.Errorf("nothing named: %q", flags)
	}

	// The dreamer's kind keeps the memory command and messages, as before.
	flags, _ = flagsFor(map[string]string{"DISPATCHER_URL": "http://dispatcher:8379", "DISPATCHER_IDENTITY": identity})
	if got := between(flags, "--allowedTools", "--append-system-prompt"); !slices.Equal(got, []string{"Bash(cmem *)", "Bash(dispatcher *)"}) {
		t.Errorf("memory: allowed %q", got)
	}
}
