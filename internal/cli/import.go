package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"carman/internal/memory"
	"carman/internal/server"
)

// importMemory copies another memory's lessons into cmem. The only source so
// far is a Claude Code memory directory: one markdown file per memory, with a
// YAML header, beside a MEMORY.md index.
//
// A directory mixes memories that apply everywhere (who the person is, how
// they like to work) with ones about the project it belongs to. --global
// names the files of the first kind: those are stored with no scope, and the
// rest with --scope. Which files are global is a judgement, made by a person
// or by a session following routines/import-claude-code.md; this command only
// carries it out.
//
// Running it twice is safe. A memory that is already here comes back from the
// service as close to an existing entry, and is skipped.
func (c *command) importMemory(args []string) error {
	var scope, global list
	c.flags.Var(&global, "global", "a file in DIR whose memory applies everywhere; it is stored with no scope (repeatable)")
	globalsOnly := c.flags.Bool("globals-only", false, "import only the files named with --global and leave the rest")
	human := c.flags.String("human", "user", "the person whose memory this is; the entries are recorded as told by them")
	dryRun := c.flags.Bool("dry-run", false, "show what would be imported and store nothing")
	c.flags.Var(&scope, "scope", "what these memories are about, for example repo:carman (repeatable)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	if c.flags.Arg(0) != "claude-code" || c.flags.Arg(1) == "" {
		return errors.New("say what to import: claude-code DIR, where DIR is a Claude Code memory directory " +
			"(~/.claude/projects/PROJECT/memory)")
	}
	dir := c.flags.Arg(1)
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	// A name given to --global that matches no file is a slip worth stopping
	// for: the memory would otherwise be imported with the wrong scope.
	isGlobal := map[string]bool{}
	for _, name := range global {
		name = filepath.Base(name)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil || name == "MEMORY.md" {
			return fmt.Errorf("--global %s: no such memory file in %s", name, dir)
		}
		isGlobal[name] = true
	}
	if *globalsOnly && len(isGlobal) == 0 {
		return errors.New("--globals-only needs at least one --global FILE")
	}
	var imported, skipped, refused, left int
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "MEMORY.md" {
			continue // the index; each line points at a file read here
		}
		entryScope, where := []string(scope), strings.Join(scope, ", ")
		switch {
		case isGlobal[name]:
			entryScope, where = nil, "global"
		case *globalsOnly:
			left++
			fmt.Fprintf(c.stdout, "left out  %s  not named as global\n", name)
			continue
		case where == "":
			where = "no scope"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		kind, text := claudeCodeMemory(data)
		if text == "" {
			fmt.Fprintf(c.stdout, "empty     %s\n", name)
			continue
		}
		if *dryRun {
			fmt.Fprintf(c.stdout, "would add %s  [%s, %s] %s\n", name, kind, where, summaryLine(text))
			continue
		}
		req := server.NoteRequest{
			Kind: kind, Text: text, Scope: entryScope, Human: *human,
			Evidence: "Imported from Claude Code's built-in memory, file " + name + ".",
		}
		var entry memory.Entry
		err = c.call(http.MethodPost, "/v1/notes", req, &entry)
		var failure *statusError
		switch {
		case err == nil:
			imported++
			fmt.Fprintf(c.stdout, "imported  %s  as %s (%s)\n", name, entry.ID, where)
		case errors.As(err, &failure) && len(failure.similar) > 0:
			skipped++
			fmt.Fprintf(c.stdout, "skipped   %s  close to %s, which memory already holds\n", name, failure.similar[0].ID)
		case isStatus(err, http.StatusUnprocessableEntity):
			refused++
			fmt.Fprintf(c.stdout, "refused   %s  %v\n", name, err)
		default:
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if !*dryRun {
		fmt.Fprintf(c.stdout, "%d imported, %d already there, %d refused", imported, skipped, refused)
		if left > 0 {
			fmt.Fprintf(c.stdout, ", %d left out", left)
		}
		fmt.Fprintln(c.stdout, ".")
	}
	return nil
}

// claudeCodeMemory reads one Claude Code memory file: the kind its type maps
// to, and its text. A file with no header is taken as a fact.
//
// The header's description is what Claude Code showed in its index for this
// memory, and is put first in the text: the opening of an entry is what cmem
// shows in its own index and what it matches moments against.
func claudeCodeMemory(data []byte) (memory.Kind, string) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var head struct {
		Type        string `yaml:"type"`
		Description string `yaml:"description"`
		Metadata    struct {
			Type string `yaml:"type"`
		} `yaml:"metadata"`
	}
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if header, body, ok := strings.Cut(rest, "\n---\n"); ok {
			yaml.Unmarshal([]byte(header), &head)
			text = body
		}
	}
	kind := memory.KindFact
	switch head.Metadata.Type + head.Type {
	case "feedback", "user":
		// How the person wants things done, or who they are.
		kind = memory.KindPreference
	}
	text = strings.TrimSpace(text)
	if summary := strings.TrimSpace(head.Description); summary != "" && text != "" && !strings.HasPrefix(text, summary) {
		text = summary + "\n\n" + text
	}
	return kind, text
}

func summaryLine(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if runes := []rune(line); len(runes) > 100 {
		line = string(runes[:100]) + "…"
	}
	return line
}
