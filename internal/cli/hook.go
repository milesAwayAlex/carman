package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"carman/internal/memory"
	"carman/internal/server"
)

// hook adapts a harness's hook mechanism to cmem. It is how memory reaches an
// agent without the agent asking: the index when a session starts, and a
// flashback at the moments where a memory is most likely to matter.
//
// A hook must never break the session it runs in. Whatever goes wrong, this
// reports success and the session carries on without memory. It says so once,
// at the start of the session, because a memory that is silently absent is
// worse than none: the agent would learn things and have nowhere to put them.
func (c *command) hook(args []string) error {
	part := c.flags.Int("part", 1, "which part of the session index this invocation prints, when the harness splits it")
	if _, err := c.parse(args); err != nil {
		return err
	}
	c.indexPart = *part
	if c.flags.Arg(0) != "claude-code" {
		return errors.New("name the harness; the only one so far is claude-code")
	}
	// A session that is not doing ordinary work, such as a consolidation
	// pass, switches the hook off for itself.
	if c.getenv("CMEM_HOOK") == "off" {
		return nil
	}
	c.timeout = 3 * time.Second
	input, err := io.ReadAll(c.stdin)
	if err == nil {
		c.claudeCodeHook(input)
	}
	return nil
}

// claudeCodeInput is the part of Claude Code's hook input that cmem uses.
// https://code.claude.com/docs/en/hooks
type claudeCodeInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`

	Source string `json:"source"` // SessionStart: startup, resume, clear, compact or fork
	Prompt string `json:"prompt"` // UserPromptSubmit

	ToolName    string          `json:"tool_name"`    // PreToolUse, PostToolUseFailure
	ToolInput   json.RawMessage `json:"tool_input"`   // PreToolUse, PostToolUseFailure
	Error       string          `json:"error"`        // PostToolUseFailure
	IsInterrupt bool            `json:"is_interrupt"` // PostToolUseFailure

	StopHookActive       bool   `json:"stop_hook_active"`       // Stop
	LastAssistantMessage string `json:"last_assistant_message"` // Stop
}

func (c *command) claudeCodeHook(raw []byte) {
	var in claudeCodeInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	scope := c.sessionScope(in.Cwd)
	flashback := func(event, heading, text string, names ...string) string {
		var entries []memory.Entry
		req := server.FlashbackRequest{SessionID: in.SessionID, Event: event, Text: text, Scope: scope, Names: names}
		if c.call(http.MethodPost, "/v1/flashback", req, &entries) != nil {
			return ""
		}
		return flashbackText(heading, entries)
	}

	var context string
	switch in.Event {
	case "SessionStart":
		// A resumed session still has the index in its transcript. Adding it
		// again on every resume would bloat the context, most of all under a
		// harness that resumes the session for each turn. The exception is a
		// session that never had one: begun before the hook was installed, or
		// while the service was down. It gets the index when it is resumed.
		if in.Source == "resume" && c.sessionHasSeenMemory(in.SessionID, scope) {
			return
		}
		// Claude Code keeps at most 10,000 characters of what one hook adds;
		// anything longer is replaced by a short preview, which would hide
		// most of the index. So the index is cut to fit. A harness that wants
		// more of it lists this hook several times, as --part 1, --part 2 and
		// so on, with CMEM_INDEX_PARTS saying how many: each part is one
		// hook's worth.
		parts, _ := strconv.Atoi(c.getenv("CMEM_INDEX_PARTS"))
		parts = max(parts, 1)
		var resp server.IndexResponse
		// No session is named, so the service does not yet record anything as
		// shown: only what fits is shown, and that is recorded below.
		req := server.IndexRequest{Scope: scope, Budget: parts * claudeCodeHookLimit}
		// CMEM_INDEX_BUDGET asks for less than would fit.
		if budget, _ := strconv.Atoi(c.getenv("CMEM_INDEX_BUDGET")); budget > 0 {
			req.Budget = min(budget, req.Budget)
		}
		if err := c.call(http.MethodPost, "/v1/index", req, &resp); err != nil {
			if c.indexPart == 1 {
				context = unreachableText(err)
			}
			break
		}
		help := indexHelp
		if c.getenv("CMEM_NOTE_SCOPE") == "ask" {
			help = strings.Replace(indexHelp, indexHelpWhere, indexHelpAsk, 1)
		}
		texts, shown := indexParts(resp, scope, parts, claudeCodeHookLimit, help)
		if c.indexPart < 1 || c.indexPart > len(texts) {
			return
		}
		context = texts[c.indexPart-1]
		var ids []string
		for _, e := range shown[c.indexPart-1] {
			ids = append(ids, e.ID)
		}
		if len(ids) > 0 {
			var marked []memory.Entry
			c.call(http.MethodPost, "/v1/brief", server.BriefRequest{SessionID: in.SessionID, Scope: scope, Pick: ids, Event: "index"}, &marked)
		}

	case "UserPromptSubmit":
		// When memory holds more than the session was shown and the harness
		// has supplied a way to ask a small model, let that model read the
		// rest and pick. Otherwise, or if that fails, match by words.
		picked, asked := c.brief(in, scope)
		if asked {
			context = picked
		} else {
			context = flashback("prompt", "From memory (cmem), possibly relevant to this request:", in.Prompt)
		}

	case "PreToolUse":
		// Before a shell command runs: what does memory hold about the
		// programs it is about to invoke? This is the moment for "on this
		// machine that command points somewhere you do not expect".
		var input struct {
			Command string `json:"command"`
		}
		if in.ToolName != "Bash" || json.Unmarshal(in.ToolInput, &input) != nil || input.Command == "" {
			return
		}
		context = flashback("pre-tool", "From memory (cmem), before this command runs:",
			input.Command, programNames(input.Command)...)

	case "PostToolUseFailure":
		if in.IsInterrupt {
			return
		}
		// The command or arguments say what was being attempted, and the
		// error says how it failed; both are good search keys.
		context = flashback("tool-failure", "From memory (cmem), possibly relevant to this failure:",
			string(in.ToolInput)+"\n"+in.Error)

	case "Stop":
		// The agent has just said what it concluded or recommends. That text
		// is the best key there is for "we have been here before". When
		// memory has something close, the turn continues so the agent can
		// weigh it before the user reads the answer. One round only.
		if in.StopHookActive {
			return
		}
		context = flashback("proposal", "Before this reply stands, memory (cmem) has entries that bear on it. "+
			"If they change your answer, revise it and say why; if they do not, say so in a line.", in.LastAssistantMessage)
	}
	if context == "" {
		return
	}
	json.NewEncoder(c.stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{"hookEventName": in.Event, "additionalContext": context},
	})
}

// claudeCodeHookLimit is how much text one hook may add to the context and
// have all of it kept. Claude Code's cap is 10,000 characters; this leaves
// room for the JSON around the text.
const claudeCodeHookLimit = 9500

// indexParts lays the index out in at most `parts` texts of at most `limit`
// characters each, and reports which entries went into which. Entries that do
// not fit are left out and the first text says how many there are in all.
func indexParts(resp server.IndexResponse, scope []string, parts, limit int, help string) (texts []string, shown [][]memory.Entry) {
	about := ""
	if len(scope) > 0 {
		about = " for " + strings.Join(scope, ", ")
	}
	const continued = "Long-term memory (cmem), continued:\n"
	// The first part also carries a heading of unknown length (it names the
	// counts) and how to use cmem; allow for both.
	room := limit - 120 - len(about) - len(help)
	var lines []string
	var group []memory.Entry
	flush := func() {
		texts = append(texts, strings.Join(lines, ""))
		shown = append(shown, group)
		lines, group = nil, nil
	}
	for _, e := range resp.Entries {
		line := entryLine(e)
		if len(line) > room {
			if len(texts) == parts-1 {
				break
			}
			flush()
			room = limit - len(continued)
		}
		lines = append(lines, line)
		group = append(group, e)
		room -= len(line)
	}
	flush()
	count := 0
	for _, g := range shown {
		count += len(g)
	}
	resp.Entries = resp.Entries[:count]
	var head string
	switch {
	case resp.Total == 0:
		head = fmt.Sprintf("Long-term memory (cmem) has no entries%s yet.\n", about)
	case count < resp.Total:
		head = fmt.Sprintf("Long-term memory (cmem): %d of %d entries%s. How to work here and what to avoid come first; "+
			"documents and older entries are not shown, and `cmem recall WORDS` finds them.\n", count, resp.Total, about)
	case resp.Total == 1:
		head = fmt.Sprintf("Long-term memory (cmem): 1 entry%s.\n", about)
	default:
		head = fmt.Sprintf("Long-term memory (cmem): %d entries%s.\n", resp.Total, about)
	}
	for i := range texts {
		if i == 0 {
			texts[i] = head + texts[i] + help
		} else {
			texts[i] = continued + texts[i]
		}
	}
	return texts, shown
}

// sessionHasSeenMemory reports whether this session has been shown any entry
// that applies to it. When the service cannot say, the answer is yes: better
// to add nothing than to add the index twice.
func (c *command) sessionHasSeenMemory(session string, scope []string) bool {
	var all server.IndexResponse
	var unseen []memory.Entry
	if c.call(http.MethodPost, "/v1/index", server.IndexRequest{Scope: scope, Budget: 1}, &all) != nil ||
		c.call(http.MethodPost, "/v1/brief", server.BriefRequest{SessionID: session, Scope: scope}, &unseen) != nil {
		return true
	}
	return len(unseen) < all.Total
}

const briefLimit = 5

// brief asks the harness's own small model which of the entries this session
// has not seen bear on the prompt, and returns those entries as written. The
// model is reached by running CMEM_BRIEF_CMD in a shell: the question goes to
// its standard input and it answers with entry IDs. cmem calls no model
// itself; the command is the harness's, and is given CMEM_HOOK=off so that a
// session it starts does not run this hook in turn.
//
// asked is false when no model was consulted (no command, nothing unseen, or
// the command failed), and the caller should fall back to matching by words.
func (c *command) brief(in claudeCodeInput, scope []string) (text string, asked bool) {
	run := c.getenv("CMEM_BRIEF_CMD")
	if run == "" || strings.TrimSpace(in.Prompt) == "" {
		return "", false
	}
	var unseen []memory.Entry
	if c.call(http.MethodPost, "/v1/brief", server.BriefRequest{SessionID: in.SessionID, Scope: scope}, &unseen) != nil || len(unseen) == 0 {
		return "", false
	}
	var question strings.Builder
	question.WriteString("An agent is about to start a task. Below are notes from its long-term memory that it has not seen.\n" +
		"Choose the notes that bear on the task: ones that would change what the agent does, or that it would\n" +
		"regret not having read first. A note about a tool, system or practice the task involves counts even if\n" +
		"the task does not mention the problem the note describes. Most tasks need none, one or two notes; never\n" +
		"choose more than five.\n\nThe task:\n")
	question.WriteString(in.Prompt)
	question.WriteString("\n\nThe notes, one per line, as ID then text:\n")
	for _, e := range unseen {
		fmt.Fprintf(&question, "%s %s\n", e.ID, strings.TrimSuffix(strings.TrimPrefix(entryLine(e), "- "), "("+e.ID+")\n"))
	}
	question.WriteString("\nReply with the IDs of the notes you choose, one per line, and nothing else. If none bears on the task, reply NONE.\n")

	timeout := 45 * time.Second
	if seconds, err := strconv.Atoi(c.getenv("CMEM_BRIEF_TIMEOUT")); err == nil && seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", run)
	cmd.Stdin = strings.NewReader(question.String())
	cmd.Env = append(os.Environ(), "CMEM_HOOK=off")
	answer, err := cmd.Output()
	if err != nil {
		return "", false
	}
	// Only IDs that were offered count, in the order the model gave them.
	offered := map[string]bool{}
	for _, e := range unseen {
		offered[e.ID] = true
	}
	var pick []string
	for _, word := range strings.FieldsFunc(string(answer), func(r rune) bool {
		return !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z')
	}) {
		if offered[word] && !slices.Contains(pick, word) && len(pick) < briefLimit {
			pick = append(pick, word)
		}
	}
	if len(pick) == 0 {
		return "", true
	}
	var entries []memory.Entry
	if c.call(http.MethodPost, "/v1/brief", server.BriefRequest{SessionID: in.SessionID, Scope: scope, Pick: pick}, &entries) != nil {
		return "", false
	}
	return flashbackText("From memory (cmem), chosen for this request:", entries), true
}

// everydayPrograms are too common for a mention of one to mean an entry is
// about it. Names shorter than four letters (go, git, npm, rm) are left out
// for the same reason: as words they turn up in unrelated entries.
var everydayPrograms = map[string]bool{
	"bash": true, "chmod": true, "command": true, "echo": true, "export": true, "false": true, "find": true,
	"grep": true, "head": true, "mkdir": true, "printf": true, "sleep": true, "sort": true, "source": true,
	"sudo": true, "tail": true, "test": true, "time": true, "touch": true, "true": true, "which": true, "xargs": true,
}

// programNames returns the distinctive programs a shell command invokes: the
// first word of each part of a pipeline or command list.
func programNames(command string) []string {
	var names []string
	parts := strings.FieldsFunc(command, func(r rune) bool { return r == '|' || r == '&' || r == ';' || r == '\n' })
	for _, part := range parts {
		for _, word := range strings.Fields(part) {
			if strings.Contains(word, "=") { // VAR=value before the program
				continue
			}
			name := strings.ToLower(filepath.Base(word))
			if len(name) >= 4 && !everydayPrograms[name] && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_.-") == "" {
				names = append(names, name)
			}
			break
		}
	}
	return names
}

// sessionScope works out what a session is about: CMEM_SCOPE when set, or
// else the repository it runs in (see scope.go).
func (c *command) sessionScope(cwd string) []string {
	if set := c.getenv("CMEM_SCOPE"); set != "" {
		return strings.FieldsFunc(set, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if cwd == "" {
		return nil
	}
	return []string{repositoryScope(cwd)}
}
