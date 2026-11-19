package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"carman/internal/memory"
	"carman/internal/server"
)

// shell runs cmem commands against a test service, as one role.
type shell struct {
	t   *testing.T
	env map[string]string
}

func newService(t *testing.T) (laptop, worker, dreamer shell) {
	t.Helper()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tokens, err := server.ParseTokens("laptop=l,worker=w,dreamer=d")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ServiceHandler(server.New(store, tokens, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()))
	t.Cleanup(srv.Close)
	as := func(token string) shell {
		return shell{t, map[string]string{"CMEM_URL": srv.URL, "CMEM_TOKEN": token}}
	}
	return as("l"), as("w"), as("d")
}

// run returns what the command printed and its exit code.
func (s shell) run(stdin string, args ...string) (stdout, stderr string, code int) {
	s.t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(key string) string { return s.env[key] }
	var environ []string
	for key, value := range s.env {
		environ = append(environ, key+"="+value)
	}
	// As the installed binary does: the command is sent to the service to be
	// run there, unless it is one that only this machine can do.
	code = RunWith(context.Background(), args, getenv, environ, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

// ok runs a command that must succeed and returns what it printed.
func (s shell) ok(stdin string, args ...string) string {
	s.t.Helper()
	stdout, stderr, code := s.run(stdin, args...)
	if code != 0 {
		s.t.Fatalf("cmem %s: exit %d: %s", strings.Join(args, " "), code, stderr)
	}
	return stdout
}

func expectContains(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
}

// M8 asks that a harness need only a shell and the CLI. This walks every
// memory operation through the CLI alone; the benchmark is the full check.
func TestM8_EveryMemoryOperationIsReachableFromTheCLI(t *testing.T) {
	laptop, worker, dreamer := newService(t)

	// A worker notes something mid-task.
	var noted memory.Entry
	out := worker.ok("", "note", "--json", "--kind", "pitfall", "--scope", "repo:api",
		"--evidence", "The deploy failed until I ran the migration job.",
		"Run", "the", "migration", "job", "before", "deploying", "the", "api.")
	if err := json.Unmarshal([]byte(out), &noted); err != nil {
		t.Fatalf("note --json: %v\n%s", err, out)
	}
	if noted.Text != "Run the migration job before deploying the api." || noted.Status != memory.StatusUnverified {
		t.Errorf("noted = %+v", noted)
	}

	// The laptop recalls it: what it is, that nothing has checked it, and
	// where it applies. Reading the entry says where it came from.
	out = laptop.ok("", "recall", "--scope", "repo:api", "what", "should", "I", "do", "before", "deploying?")
	expectContains(t, out, noted.ID, "pitfall, unverified", "[repo:api]", "Run the migration job before deploying the api.", "1 shown")
	expectContains(t, laptop.ok("", "get", noted.ID), "what an agent saw (episode "+noted.Sources[0].EpisodeID+")")
	// A session searches what applies to it. This one is not in repo:api, so
	// it finds the note only by asking for everywhere.
	if got := laptop.ok("", "recall", "before", "deploying", "the", "api"); strings.Contains(got, noted.ID) {
		t.Errorf("a session in another repository found repo:api's note without asking:\n%s", got)
	}
	expectContains(t, laptop.ok("", "recall", "--everywhere", "before", "deploying", "the", "api"), noted.ID)
	// Flags may follow the words they go with.
	expectContains(t, laptop.ok("", "recall", "before", "deploying", "the", "api", "--scope", "repo:api"), noted.ID)

	// get shows an entry, and the episode behind it.
	expectContains(t, laptop.ok("", "get", noted.ID), noted.ID, "Run the migration job")
	expectContains(t, laptop.ok("", "get", noted.Sources[0].EpisodeID), "The deploy failed until I ran the migration job.", "worker")

	// The end of a task is recorded from standard input.
	out = worker.ok("Deployed api v41. Migration job first, then rollout. Tests green.\n",
		"record", "--json", "--outcome", "passed", "--task-id", "task-41", "--scope", "repo:api", "--cost", "0.42")
	var episode memory.Episode
	if err := json.Unmarshal([]byte(out), &episode); err != nil {
		t.Fatalf("record --json: %v\n%s", err, out)
	}
	if episode.Outcome != memory.OutcomePassed || episode.TaskID != "task-41" || episode.CostUSD == nil || *episode.CostUSD != 0.42 {
		t.Errorf("recorded = %+v", episode)
	}

	// The consolidation role opens a run and replaces the note with a verified entry.
	var run memory.Run
	if err := json.Unmarshal([]byte(dreamer.ok("", "consolidate", "--json", "start", "Promote", "what", "task-41", "confirmed.")), &run); err != nil {
		t.Fatal(err)
	}
	expectContains(t, dreamer.ok("", "consolidate", "entries"), "1 current entries", noted.ID)
	out = dreamer.ok("", "supersede", noted.ID, "--run", run.ID, "--kind", "procedure", "--scope", "repo:api", "--source", episode.ID,
		"Deploy the api by running the migration job, then the rollout.")
	expectContains(t, out, "procedure  [repo:api]", "episode "+episode.ID+" (passed)")
	if strings.Contains(out, "unverified") {
		t.Errorf("an entry resting on a passed task is shown as unverified:\n%s", out)
	}
	out = laptop.ok("", "recall", "--everywhere", "deploying", "the", "api")
	expectContains(t, out, "Deploy the api by running the migration job, then the rollout.")
	if strings.Contains(out, noted.Text) {
		t.Errorf("recall still returns the superseded note:\n%s", out)
	}
	expectContains(t, laptop.ok("", "get", noted.ID), "no longer current")

	// A person tells the laptop something, and an agent later disputes it.
	out = laptop.ok("", "note", "--json", "--kind", "preference", "--human", "alex", "Deploys", "are", "frozen", "on", "Fridays.")
	var told memory.Entry
	if err := json.Unmarshal([]byte(out), &told); err != nil {
		t.Fatalf("note --json: %v\n%s", err, out)
	}
	expectContains(t, worker.ok("", "flag", told.ID, "The", "freeze", "moved", "to", "Thursdays."), "Flagged "+told.ID, "shown as disputed")

	// What the person said changes. The corrected note is close to the old
	// one, so it is held until the agent says which it is, and then replaces it.
	stdout, _, code := laptop.run("", "note", "--kind", "preference", "--human", "alex", "Deploys", "are", "frozen", "on", "Thursdays,", "no", "longer", "on", "Fridays.")
	if code == 0 {
		t.Errorf("a note close to an existing entry was stored without asking:\n%s", stdout)
	}
	expectContains(t, stdout, "Not recorded yet", told.ID, "--replaces ID", "--separate")
	out = laptop.ok("", "note", "--kind", "preference", "--human", "alex", "--replaces", told.ID, "Deploys", "are", "frozen", "on", "Thursdays,", "no", "longer", "on", "Fridays.")
	expectContains(t, out, "Remembered", "frozen on Thursdays", "replaces "+told.ID)
	out = laptop.ok("", "recall", "when", "are", "deploys", "frozen")
	if expectContains(t, out, "frozen on Thursdays"); strings.Contains(out, "Deploys are frozen on Fridays.") {
		t.Errorf("recall still returns the replaced note:\n%s", out)
	}

	// A worker's own observation cannot displace what a person said.
	_, stderr, code := worker.run("", "note", "--kind", "preference", "--evidence", "Saw a deploy on a Thursday.",
		"--replaces", strings.Fields(out)[0], "There", "is", "no", "deploy", "freeze.")
	if code == 0 || !strings.Contains(stderr, "cmem flag") {
		t.Errorf("an unverified note replacing a trusted entry: exit %d, stderr %q", code, stderr)
	}

	// The consolidation role retires an entry, the log shows the run, and the
	// run can be undone.
	var cache memory.Entry
	json.Unmarshal([]byte(worker.ok("", "note", "--json", "--kind", "fact", "--evidence", "Saw it once.", "The", "cache", "needs", "warming.")), &cache)
	expectContains(t, dreamer.ok("", "retire", cache.ID, "--run", run.ID, "Nobody", "has", "seen", "this", "since."), "Retired", "no longer current")
	expectContains(t, laptop.ok("", "consolidate", "log"), run.ID, "Promote what task-41 confirmed.",
		"replaced "+noted.ID, "retired "+cache.ID+": Nobody has seen this since.")
	expectContains(t, dreamer.ok("", "consolidate", "revert", run.ID, "Trying", "the", "undo."), "Reverted", "REVERTED")
	expectContains(t, laptop.ok("", "recall", "cache", "warming"), cache.ID)
	second := dreamer.ok("", "consolidate", "start", "Do", "it", "again.")
	out = dreamer.ok("", "supersede", noted.ID, "--run", strings.Fields(second)[1], "--kind", "procedure", "--scope", "repo:api", "--from", noted.ID, "--source", episode.ID,
		"Deploy the api by running the migration job, then the rollout.")
	expectContains(t, out, "procedure  [repo:api]", "replaces "+noted.ID)

	// A question that shares no words with any note says how much memory
	// holds, so that a miss in wording is not taken for an empty memory.
	expectContains(t, laptop.ok("", "recall", "--everywhere", "kubernetes", "ingress"),
		"None of the 3 entries anywhere shares a word with that", "cmem index")
}

// hookContext runs the Claude Code hook with the given input and returns the
// context it asks Claude Code to add, or "" when it adds nothing.
func hookContext(t *testing.T, s shell, input map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(input)
	stdout, stderr, code := s.run(string(raw), "hook", "claude-code")
	if code != 0 || stderr != "" {
		t.Fatalf("a hook must always succeed quietly: exit %d, stderr %q", code, stderr)
	}
	if stdout == "" {
		return ""
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string
			AdditionalContext string
		}
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, stdout)
	}
	if out.HookSpecificOutput.HookEventName != input["hook_event_name"] {
		t.Errorf("hook answered for event %q, was asked about %q", out.HookSpecificOutput.HookEventName, input["hook_event_name"])
	}
	return out.HookSpecificOutput.AdditionalContext
}

// The case behind this test: an agent recommended a hard topology spread, and
// only its user remembered that the practice had already failed here. Memory
// has to speak up at that moment without being asked.
func TestM5_TheClaudeCodeHookVolunteersMemoryAtTheMomentsThatMatter(t *testing.T) {
	laptop, worker, _ := newService(t)
	laptop.env["CMEM_SCOPE"] = "repo:infra"
	worker.env["CMEM_SCOPE"] = "repo:infra"

	lesson := "Hard topology spread constraints leave pods pending because the cluster autoscaler ignores pod topology; keep spread constraints soft."
	laptop.ok("", "note", "--kind", "pitfall", "--scope", "repo:infra", "--human", "alex", lesson)
	worker.ok("", "note", "--kind", "pitfall", "--scope", "repo:infra",
		"--evidence", "helm upgrade failed with: another operation (install/upgrade/rollback) is in progress",
		"When helm reports another operation in progress, roll back the pending release before retrying the upgrade.")
	laptop.ok("", "note", "--kind", "fact", "--scope", "repo:billing", "--human", "alex", "Invoices are generated on the first of the month.")

	// Session start: the index, limited to this repository, with how to use cmem.
	index := hookContext(t, laptop, map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "source": "startup"})
	expectContains(t, index, "2 entries for repo:infra", lesson, "roll back the pending release", "unverified", "cmem recall", "cmem note")
	if strings.Contains(index, "Invoices") {
		t.Errorf("the index for repo:infra includes an entry from another repository:\n%s", index)
	}
	// A session resumed without ever having had the index (it began before
	// the hook was installed) gets it then, once.
	late := map[string]any{"hook_event_name": "SessionStart", "session_id": "s-late", "source": "resume"}
	expectContains(t, hookContext(t, laptop, late), "2 entries for repo:infra", lesson)
	if again := hookContext(t, laptop, late); again != "" {
		t.Errorf("the index was added a second time to a resumed session:\n%s", again)
	}
	// A resumed session already has the index; it is not added a second time.
	if again := hookContext(t, laptop, map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "source": "resume"}); again != "" {
		t.Errorf("the index was added again on resume:\n%s", again)
	}

	// The agent is about to recommend the practice that failed. The question
	// that led there shares almost no words with the lesson; the proposal does.
	question := "how do we stop both replicas of a service landing on one spot node?"
	if got := hookContext(t, laptop, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s2", "prompt": question}); got != "" {
		t.Errorf("the question alone brought up:\n%s", got)
	}
	proposal := "I recommend a hard topology spread (DoNotSchedule) per node, so pods of one service cannot share a node. " +
		"The cluster autoscaler will add a node whenever a pod is pending."
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "s2", "last_assistant_message": proposal}
	expectContains(t, hookContext(t, laptop, stop), lesson, "[pitfall]", "revise")

	// One round only: the continued turn is not interrupted again, and the
	// entry is not shown twice in a session.
	stop["stop_hook_active"] = true
	if got := hookContext(t, laptop, stop); got != "" {
		t.Errorf("the hook interrupted a turn it had already continued:\n%s", got)
	}
	stop["stop_hook_active"] = false
	if got := hookContext(t, laptop, stop); got != "" {
		t.Errorf("the hook repeated an entry within one session:\n%s", got)
	}

	// A tool failure brings up what was learned from the same failure before.
	failure := map[string]any{
		"hook_event_name": "PostToolUseFailure", "session_id": "s3", "tool_name": "Bash",
		"tool_input": map[string]string{"command": "helm upgrade api ./chart"},
		"error":      "Exit code 1\nError: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress",
	}
	expectContains(t, hookContext(t, worker, failure), "roll back the pending release", "unverified")
	failure["session_id"], failure["is_interrupt"] = "s4", true
	if got := hookContext(t, worker, failure); got != "" {
		t.Errorf("the hook reacted to an interrupted tool call:\n%s", got)
	}

	// Before a command runs: what memory holds about the program it invokes.
	// The case behind this: an agent ran kubectl without knowing that the
	// machine's default context was not the local cluster. The command shares
	// one word with the lesson, so it is matched by the program's name.
	context := "The default kubectl context on this machine is a shared production cluster; for local work name the local context with --context devvm."
	laptop.ok("", "note", "--kind", "pitfall", "--human", "alex", context)
	before := func(session, tool, command string) string {
		return hookContext(t, worker, map[string]any{
			"hook_event_name": "PreToolUse", "session_id": session, "tool_name": tool,
			"tool_input": map[string]string{"command": command},
		})
	}
	expectContains(t, before("s7", "Bash", "kubectl get nodes"), context, "before this command runs")
	expectContains(t, before("s8", "Bash", "cd deploy && KUBECONFIG=x /usr/local/bin/kubectl apply -f app.yaml | tee out.log"), context)
	if got := before("s7", "Bash", "kubectl get pods"); got != "" {
		t.Errorf("the lesson was repeated on the next kubectl command of the same session:\n%s", got)
	}
	for _, command := range []string{"ls -la", "go build ./...", "echo kubectl"} {
		if got := before("s9", "Bash", command); got != "" {
			t.Errorf("before %q the hook brought up:\n%s", command, got)
		}
	}
	if got := before("s10", "Read", "kubectl get nodes"); got != "" {
		t.Errorf("the hook reacted to a tool that is not the shell:\n%s", got)
	}

	// Nothing close enough: nothing is added to the context.
	unrelated := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s5", "prompt": "rename the variable in main.go"}
	if got := hookContext(t, laptop, unrelated); got != "" {
		t.Errorf("an unrelated prompt brought up:\n%s", got)
	}

	// With more in memory than the session was shown, and a command from the
	// harness that asks a small model, the model's choice is what is added:
	// the entries it names, as written, and never one it invented.
	small := shell{t, map[string]string{"CMEM_URL": laptop.env["CMEM_URL"], "CMEM_TOKEN": laptop.env["CMEM_TOKEN"], "CMEM_SCOPE": "repo:infra", "CMEM_INDEX_BUDGET": "1"}}
	// The index has room for one entry, and the newest trusted one takes it.
	laptop.ok("", "note", "--kind", "preference", "--human", "alex", "Prefer small commits with one logical change each.")
	expectContains(t, hookContext(t, small, map[string]any{"hook_event_name": "SessionStart", "session_id": "b1", "source": "startup"}), "1 of 4 entries", "small commits")
	ask := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "b1", "prompt": "List the pods on the local cluster."}
	// Stands in for the model: picks the note that mentions kubectl, and adds an ID that was never offered.
	small.env["CMEM_BRIEF_CMD"] = `grep "kubectl" | grep -v "^List" | cut -d" " -f1; echo 20200101-000000-aaaa`
	got := hookContext(t, small, ask)
	expectContains(t, got, "chosen for this request", context)
	if strings.Contains(got, "20200101") || strings.Contains(got, "helm") {
		t.Errorf("the brief added something the model did not choose from what was offered:\n%s", got)
	}
	// What the brief showed is not offered or shown again.
	if again := hookContext(t, small, ask); again != "" {
		t.Errorf("the brief repeated an entry:\n%s", again)
	}
	// A model that chooses nothing adds nothing, and word matching is not used behind its back.
	small.env["CMEM_BRIEF_CMD"] = "cat >/dev/null; echo NONE"
	ask["session_id"], ask["prompt"] = "b2", "kubectl context production cluster local work"
	hookContext(t, small, map[string]any{"hook_event_name": "SessionStart", "session_id": "b2", "source": "startup"})
	if got := hookContext(t, small, ask); got != "" {
		t.Errorf("the model chose nothing, yet the hook added:\n%s", got)
	}
	// A command that fails falls back to matching by words.
	small.env["CMEM_BRIEF_CMD"] = "exit 3"
	ask["session_id"] = "b3"
	hookContext(t, small, map[string]any{"hook_event_name": "SessionStart", "session_id": "b3", "source": "startup"})
	expectContains(t, hookContext(t, small, ask), "possibly relevant to this request", context)

	// Claude Code keeps 10,000 characters per hook and hides the rest, so no
	// part of the index may be longer, however much memory holds. Split over
	// several hooks, the parts cover more, never repeat an entry, and only
	// what was delivered counts as shown.
	for i := 0; i < 120; i++ {
		laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--separate", "--scope", "repo:big",
			fmt.Sprintf("Service number %d listens on port %d and is owned by the team that looks after area %d of the platform.", i, 9000+i, i%7))
	}
	big := shell{t, map[string]string{"CMEM_URL": laptop.env["CMEM_URL"], "CMEM_TOKEN": laptop.env["CMEM_TOKEN"], "CMEM_SCOPE": "repo:big"}}
	one := hookContext(t, big, map[string]any{"hook_event_name": "SessionStart", "session_id": "p1", "source": "startup"})
	if len(one) > 10000 || !strings.Contains(one, "of 122 entries") || !strings.Contains(one, "cmem note") {
		t.Errorf("the index as one hook: %d characters; it must fit in 10,000, say how many entries there are, and say how to use cmem:\n%.300s", len(one), one)
	}
	big.env["CMEM_INDEX_PARTS"] = "3"
	seen := map[string]int{}
	for part := 1; part <= 3; part++ {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "p2", "source": "startup"})
		stdout, _, _ := big.run(string(raw), "hook", "--part", fmt.Sprint(part), "claude-code")
		var out struct {
			HookSpecificOutput struct{ AdditionalContext string }
		}
		json.Unmarshal([]byte(stdout), &out)
		text := out.HookSpecificOutput.AdditionalContext
		// The third part has nothing left to carry and prints nothing.
		if len(stdout) > 10000 || (text == "") != (part == 3) {
			t.Errorf("part %d of the index: %d characters of output", part, len(stdout))
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "- [") {
				seen[line]++
			}
		}
	}
	if len(seen) != 122 {
		t.Errorf("three parts delivered %d of 122 entries", len(seen))
	}
	for line, times := range seen {
		if times != 1 {
			t.Errorf("an entry was delivered %d times across the parts: %s", times, line)
		}
	}
	// With one part, an entry that did not fit was not shown, so flashback can still bring it up.
	left := "Service number 3 listens on port 9003"
	if strings.Contains(one, left) {
		t.Fatalf("the test expects the oldest entries to fall outside a one-part index")
	}
	expectContains(t, hookContext(t, big, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "p1",
		"prompt": "which team owns service number 3 on port 9003 of the platform?"}), left)

	// A line of the index is an entry's first paragraph, not as much of the
	// entry as fits: the summary of a long entry, and no more.
	laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--scope", "repo:docs",
		"Release checklist for the docs site.\n\nStep one is to build. Step two is to check links. Step three is to publish.")
	docs := shell{t, map[string]string{"CMEM_URL": laptop.env["CMEM_URL"], "CMEM_TOKEN": laptop.env["CMEM_TOKEN"], "CMEM_SCOPE": "repo:docs"}}
	line := hookContext(t, docs, map[string]any{"hook_event_name": "SessionStart", "session_id": "l1", "source": "startup"})
	if expectContains(t, line, "- [fact] Release checklist for the docs site. ("); strings.Contains(line, "Step one") {
		t.Errorf("the index line ran on past the entry's first paragraph:\n%s", line)
	}

	// A session can switch the hook off for itself.
	laptop.env["CMEM_HOOK"] = "off"
	if got := hookContext(t, laptop, map[string]any{"hook_event_name": "SessionStart", "session_id": "s11", "source": "startup"}); got != "" {
		t.Errorf("with CMEM_HOOK=off the hook printed:\n%s", got)
	}
	delete(laptop.env, "CMEM_HOOK")

	// A hook never breaks the session. With the service unreachable it still
	// succeeds; it says so once, when the session starts, and is silent after.
	offline := shell{t, map[string]string{"CMEM_URL": "http://127.0.0.1:1", "CMEM_TOKEN": "x"}}
	expectContains(t, hookContext(t, offline, map[string]any{"hook_event_name": "SessionStart", "session_id": "s6"}),
		"could not be reached", "Tell the user")
	unrelated["session_id"] = "s6"
	if got := hookContext(t, offline, unrelated); got != "" {
		t.Errorf("with the service unreachable the hook printed at a later moment:\n%s", got)
	}
	if stdout, stderr, code := laptop.run("not json", "hook", "claude-code"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("on unreadable input: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestImportCopiesAClaudeCodeMemoryDirectoryOnce(t *testing.T) {
	laptop, worker, _ := newService(t)
	dir := t.TempDir()
	files := map[string]string{
		"MEMORY.md": "- [Local context](local-context.md) — kubectl must name the local context\n",
		"local-context.md": "---\nname: local-context\ndescription: kubectl must name the local context\nmetadata:\n  type: feedback\n---\n\n" +
			"On this machine the local cluster's kubectl context is devvm; always pass --context devvm.\n\n**Why:** The default context is a shared cluster.\n",
		"registry.md": "---\nname: registry\ntype: reference\n---\n\nImage pulls go through the internal registry mirror.\n",
		"token.md":    "The deploy token is ghp_abcdefghijklmnopqrstuvwxyz0123456789.\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := laptop.ok("", "import", "--dry-run", "claude-code", dir)
	expectContains(t, out, "would add local-context.md  [preference, no scope]", "would add registry.md  [fact, no scope]")
	if strings.Contains(laptop.ok("", "recall", "kubectl", "context"), "devvm") {
		t.Errorf("a dry run stored something")
	}

	out = laptop.ok("", "import", "--scope", "repo:infra", "--human", "alex", "claude-code", dir)
	expectContains(t, out, "imported  local-context.md", "imported  registry.md", "refused   token.md", "2 imported, 0 already there, 1 refused.")
	if strings.Contains(out, "ghp_") {
		t.Errorf("the refusal repeats the secret:\n%s", out)
	}
	// The header is left out, the text is whole, and it is recorded as told by the person.
	out = laptop.ok("", "recall", "--scope", "repo:infra", "kubectl", "context")
	expectContains(t, out, "preference  [repo:infra]", "kubectl must name the local context")
	imported := strings.Fields(out)[0]
	out = laptop.ok("", "get", imported)
	expectContains(t, out, "always pass --context devvm", "**Why:** The default context", "told by alex")
	if strings.Contains(out, "name: local-context") {
		t.Errorf("the header was imported as text:\n%s", out)
	}

	// A second run adds nothing.
	expectContains(t, laptop.ok("", "import", "--scope", "repo:infra", "--human", "alex", "claude-code", dir), "0 imported, 2 already there, 1 refused.")

	// Only the laptop has a person to speak for.
	if _, stderr, code := worker.run("", "import", "claude-code", dir); code == 0 || !strings.Contains(stderr, "only the laptop") {
		t.Errorf("a worker importing: exit %d, stderr %q", code, stderr)
	}
}

// A memory directory mixes what applies everywhere with what is about one
// project. Which is which is decided outside cmem; import carries it out.
func TestImportStoresTheFilesNamedGlobalWithNoScope(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"MEMORY.md":      "- [Profile](profile.md)\n- [Deploys](deploys.md)\n- [Queue](queue.md)\n",
		"profile.md":     "---\nmetadata:\n  type: user\n---\n\nPrefers answers that start with the conclusion and give the evidence after.\n",
		"deploys.md":     "---\nmetadata:\n  type: feedback\n---\n\nIn this repository the agent may deploy to staging without asking.\n",
		"queue.md":       "---\nmetadata:\n  type: project\n---\n\nThe ingest queue is drained by the worker pool in cmd/ingest.\n",
		"ignored.txt":    "not a memory file",
		"sub-ignored.md": "",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recallIn := func(s shell, scope string, words ...string) string {
		return s.ok("", append([]string{"recall", "--scope", scope}, words...)...)
	}

	// For an active repository: the global file everywhere, the rest in the repository.
	laptop, _, _ := newService(t)
	out := laptop.ok("", "import", "--dry-run", "--scope", "repo:ingest", "--global", "profile.md", "claude-code", dir)
	expectContains(t, out, "would add profile.md  [preference, global]", "would add deploys.md  [preference, repo:ingest]", "would add queue.md  [fact, repo:ingest]")
	out = laptop.ok("", "import", "--human", "alex", "--scope", "repo:ingest", "--global", "profile.md", "claude-code", dir)
	expectContains(t, out, "profile.md  as", "(global)", "deploys.md  as", "(repo:ingest)", "3 imported, 0 already there, 0 refused.")
	index := func(scope string) string {
		s := shell{t, map[string]string{"CMEM_URL": laptop.env["CMEM_URL"], "CMEM_TOKEN": laptop.env["CMEM_TOKEN"], "CMEM_SCOPE": scope}}
		return hookContext(t, s, map[string]any{"hook_event_name": "SessionStart", "session_id": "s-" + scope, "source": "startup"})
	}
	elsewhere := index("repo:other")
	if expectContains(t, elsewhere, "start with the conclusion"); strings.Contains(elsewhere, "deploy to staging") || strings.Contains(elsewhere, "ingest queue") {
		t.Errorf("a session in another repository was shown this repository's memories:\n%s", elsewhere)
	}
	expectContains(t, index("repo:ingest"), "start with the conclusion", "deploy to staging without asking", "ingest queue")
	expectContains(t, recallIn(laptop, "repo:ingest", "deploy", "staging"), "[repo:ingest]")

	// A correction made later in the repository keeps the entry there; saying
	// --global is what makes it apply everywhere.
	var deploys []memory.Entry
	json.Unmarshal([]byte(laptop.ok("", "recall", "--json", "--scope", "repo:ingest", "deploy", "staging")), &deploys)
	out = laptop.ok("", "note", "--kind", "preference", "--human", "alex", "--replaces", deploys[0].ID,
		"In this repository the agent may deploy to staging and preview without asking.")
	expectContains(t, out, "[repo:ingest]", "staging and preview")
	if strings.Contains(index("repo:third"), "staging and preview") {
		t.Errorf("a replacement with no scope given became visible in another repository")
	}
	var queue []memory.Entry
	json.Unmarshal([]byte(laptop.ok("", "recall", "--json", "--scope", "repo:ingest", "ingest", "queue")), &queue)
	laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--replaces", queue[0].ID, "--global",
		"Every service here drains its queue with a worker pool under cmd.")
	expectContains(t, index("repo:fourth"), "drains its queue with a worker pool")
	if _, stderr, code := laptop.run("", "note", "--kind", "fact", "--human", "alex", "--global", "--scope", "repo:x", "Both at once."); code == 0 || !strings.Contains(stderr, "not both") {
		t.Errorf("a note both global and scoped: exit %d, stderr %q", code, stderr)
	}

	// For a directory whose project is no longer worked on: only the global files.
	fresh, _, _ := newService(t)
	out = fresh.ok("", "import", "--globals-only", "--global", "profile.md", "claude-code", dir)
	expectContains(t, out, "imported  profile.md", "left out  deploys.md", "left out  queue.md", "1 imported, 0 already there, 0 refused, 3 left out.")
	if got := fresh.ok("", "recall", "deploy", "staging", "queue"); !strings.Contains(got, "None of the 1 entries here shares a word") {
		t.Errorf("--globals-only imported a file that was not named:\n%s", got)
	}

	// A slip in a name stops the import before anything is stored with the wrong scope.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"import", "--global", "profil.md", "claude-code", dir}, "no such memory file"},
		{[]string{"import", "--global", "MEMORY.md", "claude-code", dir}, "no such memory file"},
		{[]string{"import", "--globals-only", "claude-code", dir}, "needs at least one --global"},
	} {
		empty, _, _ := newService(t)
		_, stderr, code := empty.run("", c.args...)
		if code == 0 || !strings.Contains(stderr, c.want) {
			t.Errorf("cmem %s: exit %d, stderr %q; want it to fail mentioning %q", strings.Join(c.args[:3], " "), code, stderr, c.want)
		}
		if got := empty.ok("", "recall", "conclusion", "deploy", "queue"); !strings.Contains(got, "holds no entries") {
			t.Errorf("a refused import stored something:\n%s", got)
		}
	}
}

// A repository is checked out under different directory names in different
// places: a worktree, a second clone, a worker's fresh checkout. Its scope
// must be the same in all of them, so it comes from the origin remote. The
// git files are read directly; nothing here needs git installed.
func TestARepositoryHasOneScopeWhereverItIsCheckedOut(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origin := func(url string) string {
		return "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	// The laptop's clone over ssh, with a linked worktree and a subdirectory.
	laptop := filepath.Join(root, "laptop", "billing")
	write(filepath.Join(laptop, ".git", "config"), origin("git@github.com:Acme/Billing.git"))
	os.MkdirAll(filepath.Join(laptop, "pkg", "deep"), 0o755)
	tree := filepath.Join(root, "laptop", "billing-fix-rounding")
	write(filepath.Join(laptop, ".git", "worktrees", "fix", "commondir"), "../..\n")
	write(filepath.Join(tree, ".git"), "gitdir: "+filepath.Join(laptop, ".git", "worktrees", "fix")+"\n")
	// A worker's clone of the same repository over https, under another name.
	worker := filepath.Join(root, "worker", "workspace-7f3a")
	write(filepath.Join(worker, ".git", "config"), origin("https://x-token:not-a-real-secret@github.com/acme/billing/"))
	// Other repositories.
	other := filepath.Join(root, "laptop", "ledger")
	write(filepath.Join(other, ".git", "config"), origin("ssh://git@git.example.org:2222/platform/ledger.git"))
	local := filepath.Join(root, "laptop", "scratch")
	write(filepath.Join(local, ".git", "config"), "[core]\n\tbare = false\n")
	localTree := filepath.Join(root, "laptop", "scratch-try")
	write(filepath.Join(local, ".git", "worktrees", "try", "commondir"), "../..\n")
	write(filepath.Join(localTree, ".git"), "gitdir: ../scratch/.git/worktrees/try\n")
	pathRemote := filepath.Join(root, "laptop", "mirror")
	write(filepath.Join(pathRemote, ".git", "config"), origin("/srv/git/mirror.git"))
	upstreamOnly := filepath.Join(root, "laptop", "fork")
	write(filepath.Join(upstreamOnly, ".git", "config"), "[remote \"upstream\"]\n\turl = https://gitlab.com/Group/Sub/Tool.git\n")
	plain := filepath.Join(root, "notes")
	os.MkdirAll(plain, 0o755)

	c := &command{getenv: func(string) string { return "" }}
	for dir, want := range map[string]string{
		laptop:                               "repo:github.com/acme/billing",
		filepath.Join(laptop, "pkg", "deep"): "repo:github.com/acme/billing",
		tree:                                 "repo:github.com/acme/billing",
		worker:                               "repo:github.com/acme/billing",
		other:                                "repo:git.example.org/platform/ledger",
		upstreamOnly:                         "repo:gitlab.com/group/sub/tool",
		// With no remote, or one that is a path on this machine, there is no
		// shared name; the main checkout's directory name serves, and is at
		// least the same for every worktree.
		local:      "repo:scratch",
		localTree:  "repo:scratch",
		pathRemote: "repo:mirror",
		plain:      "repo:notes",
	} {
		if got := c.sessionScope(dir); len(got) != 1 || got[0] != want {
			t.Errorf("scope of %s = %q, want %q", strings.TrimPrefix(dir, root), got, want)
		}
	}
	// A scope set by the harness still wins.
	set := &command{getenv: func(key string) string { return map[string]string{"CMEM_SCOPE": "repo:x,team:y"}[key] }}
	if got := set.sessionScope(tree); strings.Join(got, " ") != "repo:x team:y" {
		t.Errorf("with CMEM_SCOPE set, scope = %q", got)
	}

	// The same name is what `cmem scope` prints and what `cmem note --here` uses,
	// so a lesson noted in one checkout is seen in every other.
	svc, _, _ := newService(t)
	svc.env["CMEM_DIR"] = tree
	if got := strings.TrimSpace(svc.ok("", "scope")); got != "repo:github.com/acme/billing" {
		t.Errorf("cmem scope in a worktree printed %q", got)
	}
	if got := strings.TrimSpace(svc.ok("", "scope", other)); got != "repo:git.example.org/platform/ledger" {
		t.Errorf("cmem scope DIR printed %q", got)
	}
	expectContains(t, svc.ok("", "note", "--kind", "fact", "--human", "alex", "--here", "Invoices are rounded half-even."),
		"[repo:github.com/acme/billing]")
	index := func(dir string) string {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "s-" + dir, "source": "startup", "cwd": dir})
		stdout, _, _ := svc.run(string(raw), "hook", "claude-code")
		return stdout
	}
	if !strings.Contains(index(worker), "rounded half-even") {
		t.Errorf("a note made --here in a worktree is not shown in another clone of the repository")
	}
	if strings.Contains(index(other), "rounded half-even") {
		t.Errorf("a note made --here is shown in a different repository")
	}
	// `cmem index`, typed by hand, lists what applies where it is typed too.
	svc.env["CMEM_DIR"] = other
	// A search there does not look in this repository's entries, and says
	// so when one of them is the best match there is.
	elsewhere := svc.ok("", "recall", "how", "are", "invoices", "rounded")
	expectContains(t, elsewhere, "entries about other repositories", "repo:github.com/acme/billing", "cmem recall --everywhere")
	if lines := strings.Count(elsewhere, "\n"); lines > 4 {
		t.Errorf("a search whose answer is elsewhere printed %d lines:\n%s", lines, elsewhere)
	}
	if strings.Contains(elsewhere, "rounded half-even") {
		t.Errorf("a search shows another repository's entry without being asked to:\n%s", elsewhere)
	}
	expectContains(t, svc.ok("", "recall", "--everywhere", "how", "are", "invoices", "rounded"), "rounded half-even")
	if out := svc.ok("", "index"); strings.Contains(out, "rounded half-even") {
		t.Errorf("cmem index in a different repository lists the note:\n%s", out)
	}
	expectContains(t, svc.ok("", "index", "--everywhere"), "rounded half-even")
	svc.env["CMEM_DIR"] = tree
	expectContains(t, svc.ok("", "index"), "rounded half-even")

	// A harness can require every new note to say who it is for.
	svc.env["CMEM_NOTE_SCOPE"] = "ask"
	if _, stderr, code := svc.run("", "note", "--kind", "fact", "--human", "alex", "Prefers short answers."); code == 0 || !strings.Contains(stderr, "--here") || !strings.Contains(stderr, "--global") {
		t.Errorf("a note that does not say who it is for: exit %d, stderr %q", code, stderr)
	}
	out := svc.ok("", "note", "--kind", "preference", "--human", "alex", "--global", "Prefers short answers.")
	if strings.Contains(out, "[repo:") {
		t.Errorf("a note made --global has a scope:\n%s", out)
	}
	expectContains(t, index(other), "Prefers short answers", "`--here` when it is true of this repository only", "`--global` when it would also hold in another repository")
	// A replacement keeps the old entry's scope and is not asked again.
	var held []memory.Entry
	json.Unmarshal([]byte(svc.ok("", "recall", "--json", "invoices", "rounded")), &held)
	expectContains(t, svc.ok("", "note", "--kind", "fact", "--human", "alex", "--replaces", held[0].ID, "Invoices are rounded half-up since March."),
		"[repo:github.com/acme/billing]")
}

func TestALongEntryIsReadAPageAtATime(t *testing.T) {
	laptop, _, dreamer := newService(t)
	body := strings.TrimSpace(strings.Repeat("Line of a long working document about the cache migration and its waves. ", 700))
	var long memory.Entry
	if err := json.Unmarshal([]byte(laptop.ok("", "note", "--json", "--kind", "fact", "--human", "alex", "Cache migration plan.", body)), &long); err != nil {
		t.Fatal(err)
	}
	full := "Cache migration plan. " + body

	// A search result, the list of every entry, and the session index each carry only part of it.
	for name, out := range map[string]string{
		"recall":              laptop.ok("", "recall", "cache", "migration"),
		"consolidate entries": dreamer.ok("", "consolidate", "entries"),
		"index":               laptop.ok("", "index"),
	} {
		if len(out) > 4000 || !strings.Contains(out, "Cache migration plan.") {
			t.Errorf("%s printed %d characters for an entry of %d", name, len(out), len(full))
		}
	}
	// A search result says how long the entry is, and how to read it.
	expectContains(t, laptop.ok("", "recall", "cache", "migration"), long.ID, "its opening; the entry is 51121 characters", "cmem get --find WORDS ID")
	// Looking for words inside it shows where they are, not the whole text.
	deep := laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--separate",
		"Rollout notes.\n\n"+strings.Repeat("Nothing of interest happened on this day of the rollout. ", 300)+
			"The quota ceiling for the eu region is 4,200 vCPU and was raised by ticket. "+
			strings.Repeat("Nothing of interest happened on this day either. ", 300))
	deepID := strings.Fields(deep)[1]
	found := laptop.ok("", "get", "--find", "quota ceiling eu region", deepID)
	if expectContains(t, found, "at character", "4,200 vCPU", "cmem get --from CHARACTER "+deepID); len(found) > 4000 {
		t.Errorf("find printed %d characters", len(found))
	}
	expectContains(t, laptop.ok("", "recall", "quota", "ceiling", "eu", "region"), deepID, "at character", "4,200 vCPU")
	expectContains(t, laptop.ok("", "get", deepID, "--find", "unicorns"), "No part of this entry has those words", "cmem get --outline "+deepID)
	// The words need no quoting, and the ID may come before or after them.
	for _, args := range [][]string{
		{"get", "--find", "quota", "ceiling", "eu", "region", deepID},
		{"get", deepID, "--find", "quota", "ceiling", "eu", "region"},
	} {
		expectContains(t, laptop.ok("", args...), "at character", "4,200 vCPU")
	}
	if _, stderr, code := laptop.run("", "get", "quota", deepID); code == 0 || !strings.Contains(stderr, "--find") {
		t.Errorf("stray words beside an ID: exit %d, %q", code, stderr)
	}
	// One part of it can be read, from one place to another.
	if part := laptop.ok("", "get", "--from", "16", "--to", "216", deepID); len(part) > 700 || !strings.Contains(part, "Nothing of interest") || strings.Contains(part, "Rollout notes") {
		t.Errorf("get --from 16 --to 216 printed %d characters:\n%.300s", len(part), part)
	}
	// Asking a command for help gets the help, not the flag package's list.
	if out := laptop.ok("", "get", "--help"); !strings.Contains(out, "cmem get --find WORDS ID") || strings.Contains(out, "-find string") {
		t.Errorf("cmem get --help:\n%.400s", out)
	}
	// And its map says where each part starts.
	expectContains(t, laptop.ok("", "get", "--outline", deepID), "in parts starting at", "       0  Rollout notes.", "cmem get --from START --to NEXT-START "+deepID)

	// get reads it a page at a time, each page naming the next, and the pages make the whole.
	var read strings.Builder
	next := regexp.MustCompile("read on from character ([0-9]+) with")
	for from, pages := 0, 0; from < len(full); pages++ {
		out := laptop.ok("", "get", "--from", fmt.Sprint(from), long.ID)
		text := out[strings.Index(out, "\n    ")+5:]
		text = text[:strings.Index(text, "\n    sources:")]
		if i := strings.Index(text, "\n… [characters "); i >= 0 {
			from, _ = strconv.Atoi(next.FindStringSubmatch(text[i:])[1])
			text = text[:i]
		} else if from+8000 < len(full) {
			t.Fatalf("page from %d does not say how to continue", from)
		} else {
			from = len(full)
		}
		read.WriteString(text)
		if pages > 20 {
			t.Fatal("get never reached the end")
		}
	}
	if read.String() != full {
		t.Errorf("the pages of a long entry make %d characters, the entry has %d", read.Len(), len(full))
	}
	// --json gives all of it at once.
	var whole memory.Entry
	json.Unmarshal([]byte(laptop.ok("", "get", "--json", long.ID)), &whole)
	if whole.Text != full || whole.TextLength != 0 {
		t.Errorf("get --json returned %d characters", len(whole.Text))
	}
}

func TestTheCLISaysWhatWentWrong(t *testing.T) {
	laptop, worker, _ := newService(t)

	cases := []struct {
		name     string
		shell    shell
		args     []string
		wantCode int
		wantErr  string
	}{
		{"an unknown command", laptop, []string{"forget"}, 2, "unknown command"},
		{"a note without evidence", worker, []string{"note", "--kind", "fact", "x"}, 1, "what you observed"},
		{"a note of an unknown kind", worker, []string{"note", "--kind", "rumour", "--evidence", "e", "x"}, 1, "unknown kind"},
		{"a worker citing a human", worker, []string{"note", "--kind", "fact", "--human", "alex", "x"}, 1, "only the laptop"},
		{"a worker superseding", worker, []string{"supersede", "some-id", "--kind", "fact", "x"}, 1, "may not"},
		{"a worker opening a run", worker, []string{"consolidate", "start", "x"}, 1, "may not"},
		{"a worker undoing a run", worker, []string{"consolidate", "revert", "some-run", "x"}, 1, "may not"},
		{"an ID that does not exist", laptop, []string{"get", "20200101-000000-aaaa"}, 1, "not found"},
		{"a note with evidence and no text", worker, []string{"note", "--kind", "fact", "--evidence", "the thing itself"}, 1, "the text to remember"},
		{"a note that both replaces and amends", worker, []string{"note", "--kind", "fact", "--evidence", "e", "--replaces", "a", "--amends", "b", "x"}, 1, "Choose one"},
		{"a note holding a secret", worker, []string{"note", "--kind", "fact", "--evidence", "e", "key", "AKIAIOSFODNN7EXAMPLE"}, 1, "AWS access key"},
		{"no service configured", shell{t, map[string]string{}}, []string{"recall", "x"}, 1, "CMEM_URL and CMEM_TOKEN"},
	}
	for _, c := range cases {
		_, stderr, code := c.shell.run("", c.args...)
		if code != c.wantCode || !strings.Contains(stderr, c.wantErr) {
			t.Errorf("%s: exit %d, stderr %q; want exit %d mentioning %q", c.name, code, stderr, c.wantCode, c.wantErr)
		}
	}

	if out := laptop.ok("", "help"); !strings.Contains(out, "cmem recall") {
		t.Errorf("help does not describe recall:\n%s", out)
	}
}

// A long entry that is wrong in one place is not replaced by the agent that
// notices: the correction is stored beside it, and the entry points at it.
func TestANoteCanCorrectPartOfAnEntry(t *testing.T) {
	laptop, worker, _ := newService(t)
	plan := laptop.ok("", "note", "--kind", "fact", "--human", "alex",
		"Region capacity plan.\n\n"+strings.Repeat("Background on how the regions were sized. ", 200)+
			"\n\nThe quota ceiling for the eu region is 4,200 vCPU.")
	planID := strings.Fields(plan)[1]

	out := worker.ok("", "note", "--kind", "fact", "--evidence", "the quota page showed 6,000 today", "--amends", planID,
		"The quota ceiling for the eu region is 6,000 vCPU, raised from 4,200.")
	correction := strings.Fields(out)[1]
	expectContains(t, out, "Remembered:", planID+" is now marked as corrected in part")

	// The entry is still there, in full, and says where its correction is.
	expectContains(t, laptop.ok("", "get", "--from", "0", planID), "Region capacity plan.", "Corrected in part by "+correction, "6,000 vCPU")
	// So does a search that finds it.
	expectContains(t, laptop.ok("", "recall", "quota", "ceiling", "eu", "region"), planID, "fact, corrected in part", "Corrected in part by "+correction)

	// The pointer comes before the text, wherever the entry is read.
	for _, view := range [][]string{{"get", planID}, {"get", "--outline", planID}, {"get", "--find", "background", planID}} {
		out := laptop.ok("", view...)
		if pointer, body := strings.Index(out, "Corrected in part by "+correction), strings.Index(out, "Background"); pointer < 0 || (body >= 0 && pointer > body) {
			t.Errorf("cmem %s does not say the entry is corrected before showing it:\n%.600s", strings.Join(view, " "), out)
		}
	}
	// The correction is for whoever the entry was for, without being told.
	laptop.env["CMEM_NOTE_SCOPE"] = "ask"
	scoped := strings.Fields(laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--scope", "repo:fleet", "Fleet plan.\n\n"+strings.Repeat("History of the fleet. ", 150)))[1]
	expectContains(t, laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--amends", scoped, "The fleet now has nine clusters."), "[repo:fleet]")
	delete(laptop.env, "CMEM_NOTE_SCOPE")

	// A short note in place of a long entry is questioned: it is usually one
	// fact corrected, and --replaces would drop everything else.
	stdout, stderr, code := laptop.run("", "note", "--kind", "fact", "--human", "alex", "--replaces", planID, "The eu ceiling is 6,500 vCPU.")
	if code == 0 || !strings.Contains(stdout, "--amends "+planID) || !strings.Contains(stdout, "--whole") || !strings.Contains(stderr, "not recorded") {
		t.Errorf("a short note replacing a long entry: exit %d\n%s\n%s", code, stdout, stderr)
	}
	expectContains(t, laptop.ok("", "note", "--kind", "fact", "--human", "alex", "--replaces", scoped, "--whole", "The fleet plan is withdrawn; there is no fleet."),
		"It takes the place of: Fleet plan.")

	// Flagging answers in a line or two, not with the entry's text again.
	flagged := worker.ok("", "flag", planID, "the us figure is out of date too")
	if expectContains(t, flagged, "Flagged "+planID, "cmem note --amends "+planID); len(flagged) > 400 {
		t.Errorf("flag printed %d characters:\n%s", len(flagged), flagged)
	}
}

// The installed binary is a relay: it sends its command line to the service,
// which runs the command and says what to print. A change to what a command
// does then reaches every machine when the service is deployed.
func TestTheBinaryRelaysItsCommandToTheService(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens, _ := server.ParseTokens("laptop=l,worker=w,dreamer=d")
	api := server.New(store, tokens, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()

	// What reaches the service, and with what.
	var seen RemoteRequest
	var path, auth string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli" {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &seen)
			path, auth = r.URL.Path, r.Header.Get("Authorization")
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		ServiceHandler(api).ServeHTTP(w, r)
	}))
	defer relay.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@git.example.com:acme/alpha.git\n"), 0o644)
	sh := shell{t, map[string]string{"CMEM_URL": relay.URL, "CMEM_TOKEN": "l", "CMEM_DIR": dir, "CMEM_NOTE_SCOPE": "ask", "CMEM_BRIEF_TIMEOUT": "7"}}

	out := sh.ok("", "note", "--kind", "fact", "--human", "alex", "--here", "The alpha service listens on port 9100.")
	expectContains(t, out, "Remembered", "[repo:git.example.com/acme/alpha]")
	if path != "/v1/cli" || auth != "Bearer l" || strings.Join(seen.Args[:2], " ") != "note --kind" {
		t.Errorf("the command was not relayed: path %q, args %q", path, seen.Args)
	}
	// The repository is worked out on the caller's machine; settings travel,
	// credentials and what only means something locally do not.
	if strings.Join(seen.Scope, ",") != "repo:git.example.com/acme/alpha" || seen.Env["CMEM_NOTE_SCOPE"] != "ask" || seen.Env["CMEM_BRIEF_TIMEOUT"] != "7" {
		t.Errorf("relayed scope %q and settings %v", seen.Scope, seen.Env)
	}
	for _, key := range []string{"CMEM_TOKEN", "CMEM_URL", "CMEM_DIR"} {
		if _, sent := seen.Env[key]; sent {
			t.Errorf("%s was sent as a setting", key)
		}
	}
	// The service applies the caller's role, as on any other route.
	if _, stderr, code := (shell{t, map[string]string{"CMEM_URL": relay.URL, "CMEM_TOKEN": "w"}}).run("", "consolidate", "start", "x"); code == 0 || !strings.Contains(stderr, "may not") {
		t.Errorf("a worker opening a run through the relay: exit %d, %q", code, stderr)
	}
	if _, stderr, code := (shell{t, map[string]string{"CMEM_URL": relay.URL, "CMEM_TOKEN": "nobody"}}).run("", "recall", "alpha"); code == 0 || !strings.Contains(stderr, "token") {
		t.Errorf("an unknown token through the relay: exit %d, %q", code, stderr)
	}

	// What only the caller's machine can do is never asked of the service,
	// and the service refuses it if asked directly.
	path = ""
	if got := strings.TrimSpace(sh.ok("", "scope", dir)); got != "repo:git.example.com/acme/alpha" || path != "" {
		t.Errorf("cmem scope printed %q and went to the service: %q", got, path)
	}
	for _, args := range [][]string{{"serve", "--library", "/tmp/x"}, {"check", "--library", "/tmp/x"}, {"import", "claude-code", "/tmp"}, {"forget"}} {
		body, _ := json.Marshal(RemoteRequest{Args: args})
		req, _ := http.NewRequest("POST", relay.URL+"/v1/cli", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer l")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var answer RemoteResponse
		json.NewDecoder(resp.Body).Decode(&answer)
		resp.Body.Close()
		if answer.Code != 2 || answer.Stdout != "" {
			t.Errorf("the service ran %q for a caller: %+v", args, answer)
		}
	}

	// A service from before the relay existed: the binary runs the command
	// with its own copy of the logic, and nothing is lost on the way.
	old := httptest.NewServer(api)
	defer old.Close()
	legacy := shell{t, map[string]string{"CMEM_URL": old.URL, "CMEM_TOKEN": "l", "CMEM_DIR": dir}}
	expectContains(t, legacy.ok("", "recall", "alpha", "port"), "listens on port 9100")
	var episode memory.Episode
	if err := json.Unmarshal([]byte(legacy.ok("Finished the alpha task.\n", "record", "--json", "--outcome", "passed")), &episode); err != nil || episode.Body != "Finished the alpha task." {
		t.Errorf("standard input was lost on the way to the local run: %+v, %v", episode, err)
	}
	// And CMEM_LOCAL=1 asks for that on purpose.
	path = ""
	sh.env["CMEM_LOCAL"] = "1"
	if expectContains(t, sh.ok("", "recall", "alpha", "port"), "listens on port 9100"); path != "" {
		t.Errorf("with CMEM_LOCAL=1 the command still went to the service")
	}
}
