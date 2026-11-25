package cli

import (
	"context"
	"encoding/json"
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
