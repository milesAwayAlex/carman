package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The library is a directory of markdown notes in a git repository:
//
//	entries/2026/11/20261118-112425-k3f9.md
//	episodes/2026/11/20261118-112420-a1b2.md
//	flags/2026/11/20261118-113000-zz9q.md
//	retirements/, runs/, reverts/    what consolidation did, and its undoing
//
// Every note is written once and never changed. What is current is worked out
// from the notes: an entry stops being current when a later entry names it in
// `supersedes` or `absorbs`, or a retirement names it. A consolidation run is
// undone by a revert note that names the run; the run's notes stay where they
// are and stop counting. Each write is one git commit.

const (
	typeEntry      = "entry"
	typeEpisode    = "episode"
	typeFlag       = "flag"
	typeRetirement = "retirement"
	typeRun        = "run"
	typeRevert     = "revert"

	// linksMarker starts a generated footer of [[links]] for people reading
	// the note in an editor. The header above it is what the service reads.
	linksMarker = "<!-- cmem:links -->"
)

var noteDirs = map[string]string{
	typeEntry: "entries", typeEpisode: "episodes", typeFlag: "flags",
	typeRetirement: "retirements", typeRun: "runs", typeRevert: "reverts",
}

// header is the YAML block at the top of a note. One shape serves every note
// type; fields that do not apply are left out.
type header struct {
	ID      string    `yaml:"id"`
	Type    string    `yaml:"type"`
	Created time.Time `yaml:"created"`
	Scope   []string  `yaml:"scope,omitempty"`

	// entry
	Kind       Kind           `yaml:"kind,omitempty"`
	CreatedBy  Author         `yaml:"created_by,omitempty"`
	Supersedes string         `yaml:"supersedes,omitempty"`
	Absorbs    []string       `yaml:"absorbs,omitempty"` // further entries merged into this one
	Sources    []sourceHeader `yaml:"sources,omitempty"`

	// entry, retirement, revert: the consolidation run it belongs to or undoes
	Run string `yaml:"run,omitempty"`

	// episode
	Origin    Origin   `yaml:"origin,omitempty"`
	Outcome   Outcome  `yaml:"outcome,omitempty"`
	TaskID    string   `yaml:"task_id,omitempty"`
	SessionID string   `yaml:"session_id,omitempty"`
	CostUSD   *float64 `yaml:"cost_usd,omitempty"`

	// flag, retirement
	Entry string `yaml:"entry,omitempty"`
}

type sourceHeader struct {
	Episode string `yaml:"episode,omitempty"`
	Human   string `yaml:"human,omitempty"`
}

// note is one file of the library.
type note struct {
	header
	body string
}

func (n note) path() string {
	return filepath.Join(noteDirs[n.Type], n.Created.Format("2006"), n.Created.Format("01"), n.ID+".md")
}

// links are the other notes this one refers to.
func (n note) links() []string {
	var ids []string
	if n.Supersedes != "" {
		ids = append(ids, n.Supersedes)
	}
	ids = append(ids, n.Absorbs...)
	for _, src := range n.Sources {
		if src.Episode != "" {
			ids = append(ids, src.Episode)
		}
	}
	if n.Entry != "" {
		ids = append(ids, n.Entry)
	}
	if n.Run != "" {
		ids = append(ids, n.Run)
	}
	return ids
}

func (n note) render() ([]byte, error) {
	head, err := yaml.Marshal(n.header)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "---\n%s---\n\n%s\n", head, n.body)
	if links := n.links(); len(links) > 0 {
		fmt.Fprintf(&b, "\n%s\n", linksMarker)
		for _, id := range links {
			fmt.Fprintf(&b, "[[%s]] ", id)
		}
		b.WriteString("\n")
	}
	return b.Bytes(), nil
}

func parseNote(data []byte) (note, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return note{}, errors.New("no header block")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return note{}, errors.New("header block is not closed")
	}
	var n note
	if err := yaml.Unmarshal([]byte(head), &n.header); err != nil {
		return note{}, fmt.Errorf("header: %w", err)
	}
	body, _, _ = strings.Cut(body, linksMarker)
	n.body = strings.TrimSpace(body)
	if n.ID == "" || noteDirs[n.Type] == "" || n.Created.IsZero() {
		return note{}, errors.New("header needs id, type and created")
	}
	return n, nil
}

type library struct {
	dir string
}

func openLibrary(dir string) (library, error) {
	l := library{dir}
	for _, sub := range noteDirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return l, err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, fs.ErrNotExist) {
		if _, err := l.git("init", "--quiet", "--initial-branch=main"); err != nil {
			return l, err
		}
		// .cmem holds logs that can be thrown away; they are not part of the library.
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".cmem/\n"), 0o644); err != nil {
			return l, err
		}
		if err := l.commit("Start the library", ".gitignore"); err != nil {
			return l, err
		}
	}
	return l, os.MkdirAll(filepath.Join(dir, ".cmem"), 0o755)
}

// clearStaleLocks removes the lock files git leaves behind when it is killed
// mid-write. With a lock in place every later write fails. Only the service
// calls this, as it starts: it is the library's only writer, so a lock found
// then cannot belong to anything still running. A second program looking at
// the library while the service runs must never do it.
func (l library) clearStaleLocks() {
	locks, _ := filepath.Glob(filepath.Join(l.dir, ".git", "*.lock"))
	more, _ := filepath.Glob(filepath.Join(l.dir, ".git", "refs", "heads", "*.lock"))
	for _, lock := range append(locks, more...) {
		os.Remove(lock)
	}
}

// recover finishes writes that an unclean stop left half-done: note files
// that are on disk, read as valid notes, and were never committed. They are
// committed now, so that the library and its git history agree again. It
// returns how many it committed. Files that do not read as notes are left
// where they are; load reports them.
func (l library) recover(valid map[string]bool) (int, error) {
	var dirs []string
	for _, dir := range noteDirs {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	status, err := l.git(append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, dirs...)...)
	if err != nil {
		return 0, err
	}
	var paths []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		state, path := line[:2], strings.Trim(strings.TrimSpace(line[3:]), `"`)
		if (state == "??" || state == "A " || state == "AM") && valid[path] {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return 0, nil
	}
	return len(paths), l.commit(fmt.Sprintf("Recover %d note(s) left uncommitted by an unclean stop", len(paths)), paths...)
}

// git runs one git command on the library.
//
// After a commit git may decide the repository needs repacking, and by default
// it does that in a background process. That process then runs alongside the
// service's next write, and it has cost a note: at about a hundred commits a
// repack ran, the blob the next `git add` had just written was gone when the
// commit looked for it, and the write failed. So housekeeping is kept in the
// foreground. It still happens, inside the same write, while the service
// holds its lock and no other git process is running.
func (l library) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{
		"-C", l.dir, "-c", "user.name=cmem", "-c", "user.email=cmem@localhost", "-c", "commit.gpgsign=false",
		"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
		// Have git flush what it writes to disk before a commit counts, so
		// that a machine that stops without warning loses at most the write
		// in flight and not the repository's consistency.
		"-c", "core.fsync=all",
	}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

func (l library) commit(message string, paths ...string) error {
	if _, err := l.git(append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	_, err := l.git(append([]string{"commit", "--quiet", "-m", message, "--"}, paths...)...)
	return err
}

// write adds the notes as new files and records them in one commit. It never
// overwrites: a note whose file already exists is refused. On any failure the
// files it created are removed again and taken back out of git's index, so a
// write is all or nothing and a failed one leaves nothing for the next to
// trip over.
func (l library) write(message string, notes ...note) (err error) {
	var written []string
	defer func() {
		if err != nil && len(written) > 0 {
			l.git(append([]string{"reset", "--quiet", "--"}, written...)...)
			for _, path := range written {
				os.Remove(filepath.Join(l.dir, path))
			}
		}
	}()
	for _, n := range notes {
		data, err := n.render()
		if err != nil {
			return err
		}
		path := n.path()
		full := filepath.Join(l.dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		written = append(written, path)
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return l.commit(message, written...)
}

// load reads every note. A file that cannot be read as a note is reported and
// skipped, so one bad hand edit does not take the whole memory down.
func (l library) load() (notes []note, problems []string, err error) {
	for _, sub := range noteDirs {
		root := filepath.Join(l.dir, sub)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			rel, _ := filepath.Rel(l.dir, path)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n, err := parseNote(data)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
			case n.path() != rel:
				problems = append(problems, fmt.Sprintf("%s: its header says it belongs at %s", rel, n.path()))
			default:
				notes = append(notes, n)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return notes, problems, nil
}

// check looks for notes that were changed or removed after they were written,
// which the service never does but a person with the files could.
func (l library) check() ([]string, error) {
	var dirs []string
	for _, dir := range noteDirs {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	var problems []string

	history, err := l.git(append([]string{"log", "--diff-filter=MDR", "--name-status", "--format=commit %h"}, append([]string{"--"}, dirs...)...)...)
	if err != nil {
		return nil, err
	}
	commit := ""
	for _, line := range strings.Split(history, "\n") {
		if rest, ok := strings.CutPrefix(line, "commit "); ok {
			commit = rest
		} else if line != "" {
			problems = append(problems, fmt.Sprintf("commit %s changed or removed a note: %s", commit, strings.Join(strings.Fields(line), " ")))
		}
	}

	status, err := l.git(append([]string{"status", "--porcelain", "--"}, dirs...)...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 && !strings.HasPrefix(line, "??") {
			problems = append(problems, fmt.Sprintf("uncommitted change to a note: %s", strings.TrimSpace(line)))
		}
	}
	return problems, nil
}
