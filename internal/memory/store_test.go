package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test names start with the ID of the requirement in REQUIREMENTS.md that they
// check, so `go test -run M2 -v` lists the evidence for M2.

// newStore returns a Store on a fresh, empty library.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func mustEpisode(t *testing.T, s *Store, outcome Outcome) Episode {
	t.Helper()
	e, err := s.AppendEpisode(NewEpisode{
		Origin:  OriginWorker,
		TaskID:  "task-1",
		Scope:   []string{"repo:carman"},
		Body:    "Rotated the staging database password; the migration job needed a restart.",
		Outcome: outcome,
	})
	if err != nil {
		t.Fatalf("append episode: %v", err)
	}
	return e
}

func newEntry(text string, episode Episode) NewEntry {
	return NewEntry{
		CreatedBy: AuthorAgent,
		Kind:      KindFact,
		Text:      text,
		Scope:     []string{"repo:carman"},
		Sources:   []Source{{EpisodeID: episode.ID}},
	}
}

func mustAdd(t *testing.T, s *Store, text string, episode Episode) Entry {
	t.Helper()
	e, err := s.AddEntry(newEntry(text, episode))
	if err != nil {
		t.Fatalf("add entry: %v", err)
	}
	return e
}

func mustSupersede(t *testing.T, s *Store, oldID, text string, episode Episode) Entry {
	t.Helper()
	e, err := s.Supersede(oldID, newEntry(text, episode))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	return e
}

func texts(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Text
	}
	return out
}

func equal(a, b []string) bool {
	return fmt.Sprint(a) == fmt.Sprint(b) && len(a) == len(b)
}

// noteFiles returns the library's note files, by path relative to its root.
func noteFiles(t *testing.T, s *Store) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, dir := range noteDirs {
		filepath.WalkDir(filepath.Join(s.lib.dir, dir), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				rel, _ := filepath.Rel(s.lib.dir, path)
				files[rel] = string(data)
			}
			return nil
		})
	}
	return files
}

func TestM2_NothingTheStoreDoesChangesOrRemovesANote(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	first := mustAdd(t, s, "The service runs in us-east1.", episode)
	before := noteFiles(t, s)

	// Every kind of write the store offers.
	second := mustSupersede(t, s, first.ID, "The service runs in us-central1.", episode)
	if err := s.Flag(second.ID, OriginWorker, "It moved again."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Note(NewEpisode{Origin: OriginWorker, Body: "Saw the region in the console."},
		NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Text: "The console shows the region."}); err != nil {
		t.Fatal(err)
	}
	mustEpisode(t, s, OutcomeFailed)

	after := noteFiles(t, s)
	for path, content := range before {
		if after[path] != content {
			t.Errorf("%s was changed or removed by a later write", path)
		}
	}
	if len(after) != len(before)+5 {
		t.Errorf("%d notes after, want %d: each write adds notes and nothing else", len(after), len(before)+5)
	}

	// A note's file is created once; writing the same note again is refused.
	var again note
	again.ID, again.Type, again.Created, again.body = episode.ID, typeEpisode, episode.CreatedAt, "rewritten"
	if err := s.lib.write("overwrite", again); err == nil {
		t.Error("the library overwrote an existing note")
	}
	if problems, err := s.Check(); err != nil || len(problems) != 0 {
		t.Errorf("check after ordinary use: %v %v", problems, err)
	}
}

func TestM2_ANoteChangedOrRemovedByHandIsDetected(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	entry := mustAdd(t, s, "Deploys go out on Tuesdays.", episode)
	var episodePath, entryPath string
	for path := range noteFiles(t, s) {
		if strings.HasPrefix(path, "episodes") {
			episodePath = path
		} else {
			entryPath = path
		}
	}

	// Someone edits the entry in an editor and has not committed it.
	full := filepath.Join(s.lib.dir, entryPath)
	data, _ := os.ReadFile(full)
	os.WriteFile(full, []byte(strings.Replace(string(data), "Tuesdays", "Fridays", 1)), 0o644)
	problems, err := s.Check()
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], entryPath) {
		t.Errorf("an uncommitted edit: check reported %q, %v", problems, err)
	}

	// They commit it, and delete the episode as well.
	if _, err := s.lib.git("commit", "--quiet", "-am", "tidy up"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lib.git("rm", "--quiet", episodePath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lib.git("commit", "--quiet", "-m", "remove"); err != nil {
		t.Fatal(err)
	}
	problems, err = s.Check()
	if err != nil {
		t.Fatal(err)
	}
	report := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(report, entryPath) || !strings.Contains(report, episodePath) {
		t.Errorf("a committed edit and a removal: check reported %q", problems)
	}

	// The running store still serves what was written through it.
	if got, err := s.Entry(entry.ID); err != nil || got.Text != "Deploys go out on Tuesdays." {
		t.Errorf("entry after a hand edit: %+v, %v", got, err)
	}
}

// The case behind this test: with a few hundred notes written one after
// another, git started repacking the library in the background after about a
// hundred commits, and the next write failed with a missing object.
func TestM2_ManyWritesInARowAllLandAndLeaveTheLibraryIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("writes several hundred notes")
	}
	s := newStore(t)
	const notes = 320
	for i := 0; i < notes; i++ {
		n := told(fmt.Sprintf("Lesson number %d: service %d must be restarted after its configuration changes.", i, i))
		n.Separate = true
		if _, err := s.Remember(n); err != nil {
			t.Fatalf("note %d of %d: %v", i, notes, err)
		}
	}
	if got := len(s.CurrentEntries(time.Time{})); got != notes {
		t.Errorf("%d notes written, %d current", notes, got)
	}
	if problems, err := s.Check(); err != nil || len(problems) != 0 {
		t.Errorf("check after %d writes: %v, %v", notes, problems, err)
	}
	// No housekeeping process is left running behind the store's back, and
	// git itself finds the repository whole.
	if out, err := s.lib.git("fsck", "--strict", "--no-dangling"); err != nil {
		t.Errorf("git fsck: %v\n%s", err, out)
	}
	if out, _ := s.lib.git("status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("the library has uncommitted changes:\n%s", out)
	}
}

// A write that fails part-way leaves no file and nothing staged, so the next
// write is not affected by it.
func TestM2_AFailedWriteLeavesNothingBehind(t *testing.T) {
	s := newStore(t)
	first := mustEpisode(t, s, OutcomePassed)
	// Make the commit fail after the file is written and staged.
	hook := filepath.Join(s.lib.dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := noteFiles(t, s)
	if _, err := s.AddEntry(newEntry("This write is refused by git.", first)); err == nil {
		t.Fatal("the write succeeded although the commit was made to fail")
	}
	if after := noteFiles(t, s); len(after) != len(before) {
		t.Errorf("a failed write left a file behind: %d notes before, %d after", len(before), len(after))
	}
	if out, _ := s.lib.git("status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("a failed write left something staged:\n%s", out)
	}
	if got := s.CurrentEntries(time.Time{}); len(got) != 0 {
		t.Errorf("a failed write is in memory: %q", texts(got))
	}
	// With the obstacle gone, the next write goes through.
	os.Remove(hook)
	mustAdd(t, s, "This one is accepted.", first)
	if problems, err := s.Check(); err != nil || len(problems) != 0 {
		t.Errorf("check after a failed and a good write: %v, %v", problems, err)
	}
}

// A machine can stop without warning in the middle of a write. git is then
// left with a lock that blocks every later write, and possibly with a note
// that is on disk but in no commit. Starting the service again has to put
// both right without anyone's help.
func TestM2_TheLibraryRecoversFromAStopInTheMiddleOfAWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	passed := mustEpisode(t, s, OutcomePassed)
	kept := mustAdd(t, s, "Written and committed before the stop.", passed)

	// What a kill between "file written" and "commit made" leaves behind.
	half := s.newNote(typeEntry)
	half.Kind, half.CreatedBy, half.body = KindFact, AuthorAgent, "Written to disk, never committed."
	half.Sources = []sourceHeader{{Episode: passed.ID}}
	data, _ := half.render()
	os.MkdirAll(filepath.Join(dir, filepath.Dir(half.path())), 0o755)
	if err := os.WriteFile(filepath.Join(dir, half.path()), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A file cut short by the same stop, and the locks git left.
	torn := filepath.Join(dir, "entries", "2026", "11", "20261119-000000-torn.md")
	os.MkdirAll(filepath.Dir(torn), 0o755)
	os.WriteFile(torn, []byte("---\nid: 20261119-000000-torn\ntype: ent"), 0o644)
	for _, lock := range []string{"index.lock", "HEAD.lock", filepath.Join("refs", "heads", "main.lock")} {
		os.WriteFile(filepath.Join(dir, ".git", lock), nil, 0o644)
	}

	// Looking at the library does not touch it: a second program may open it
	// while the service is in the middle of a write.
	if _, err := Open(dir); err != nil {
		t.Fatalf("open after an unclean stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); err != nil {
		t.Errorf("merely opening the library removed a lock")
	}
	// The service, as it starts, puts it right.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := again.Recover(); err != nil || n != 1 {
		t.Errorf("recovered %d notes (%v), want the one that was whole", n, err)
	}
	if got := texts(again.CurrentEntries(time.Time{})); !equal(got, []string{kept.Text, half.body}) {
		t.Errorf("after recovery, current = %q", got)
	}
	// Writes work again, and the library is consistent apart from the torn
	// file, which is reported and left for a person to look at.
	mustAdd(t, again, "Written after the restart.", passed)
	problems, err := again.Check()
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "torn") {
		t.Errorf("check after recovery: %q, %v; want only the torn file reported", problems, err)
	}
	out, _ := again.lib.git("status", "--porcelain", "--untracked-files=all")
	if left := strings.Fields(strings.TrimSpace(out)); len(left) != 2 || !strings.Contains(left[1], "torn") {
		t.Errorf("after recovery git should hold everything but the torn file:\n%s", out)
	}
	if out, err := again.lib.git("fsck", "--strict", "--no-dangling"); err != nil {
		t.Errorf("git fsck after recovery: %v\n%s", err, out)
	}
	// A third start finds nothing more to do.
	third, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := third.Recover(); err != nil || n != 0 {
		t.Errorf("a clean start recovered %d notes, err %v", n, err)
	}
}

// An entry can be a long working document, brought in from another memory.
// Such a document contains most common words. It must not look close to every
// prompt and every new note, and a list must not carry all of it.
func TestALongEntryIsJudgedByItsOpeningAndShownInPart(t *testing.T) {
	s := newStore(t)
	// What the entry is about is in its opening; a real document's first
	// paragraphs stay on its subject before the detail starts.
	opening := strings.Repeat("The session cache migration moves each cache to the managed service, wave by wave. ", 8)
	filler := strings.Repeat("The rollout plan lists every service, its owner, the command to restart it, the cluster, "+
		"the context, the tests to run, the notes taken and the decisions made along the way. ", 400)
	long, err := s.Remember(NewNote{Separate: true, Entry: NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Sources: []Source{{Human: "alex"}},
		Text: "Cache migration plan: the session cache moves to the managed service in three waves.\n\n" + opening + filler}})
	if err != nil {
		t.Fatal(err)
	}
	if len(long.Text) < 50000 {
		t.Fatalf("the test's long entry is only %d characters", len(long.Text))
	}

	// A moment that shares words only with the body does not bring it up...
	if got := s.Flashback(FlashbackQuery{SessionID: "a", Text: "which command restarts the service, and who is its owner?"}); len(got) != 0 {
		t.Errorf("a long entry was volunteered for a moment its opening has nothing to do with: %.80q", texts(got))
	}
	// ...one about what its opening says does...
	if got := s.Flashback(FlashbackQuery{SessionID: "b", Text: "the session cache migration to the managed service"}); len(got) != 1 {
		t.Errorf("a long entry was not volunteered for a moment about its subject")
	}
	// ...and a search of its whole text still finds it.
	if got := s.Recall(RecallQuery{Text: "who is the owner of each service in the rollout"}); len(got) != 1 {
		t.Errorf("recall does not search the whole of a long entry")
	}
	// A new note on something else is not held as close to it.
	if _, err := s.Remember(told("Restart a service with its own command; ask its owner first.")); err != nil {
		t.Errorf("a new note was held as close to a long document: %v", err)
	}

	// The other way round: a long moment contains the few words of a short
	// entry without being about it, and must not bring it up.
	short1, _ := s.Remember(told("Keep code comments short and use American spelling."))
	script := "python3 - <<'E'\n" + strings.Repeat("for path in files: text = read(path); check(text); report(path, text)\n", 30) +
		"# keep this short; the code below uses comments and spelling from the old file\n" + strings.Repeat("print(result)\n", 40)
	if got := s.Flashback(FlashbackQuery{SessionID: "c", Text: script}); len(got) != 0 {
		t.Errorf("a long script brought up %q", texts(got))
	}
	if got := s.Flashback(FlashbackQuery{SessionID: "d", Text: "should the code comments use British or American spelling?"}); len(got) != 1 || got[0].ID != short1.ID {
		t.Errorf("a short moment about the entry did not bring it up: %q", texts(got))
	}

	// In part: the opening, and a line saying how to go on.
	part := long.Part(0, 300)
	if part.TextLength != len([]rune(long.Text)) || !strings.HasPrefix(part.Text, "Cache migration plan") ||
		!regexp.MustCompile("read on from character (2[4-9][0-9]|300) with `cmem get --from (2[4-9][0-9]|300) "+long.ID+"`").MatchString(part.Text) || len(part.Text) > 600 {
		t.Errorf("part of a long entry = %d characters, length reported %d:\n%.400s", len(part.Text), part.TextLength, part.Text)
	}
	// Reading on from there reaches the end, and the pieces make the whole.
	// A page stops between words, and says where the next one starts.
	var whole strings.Builder
	next := regexp.MustCompile("read on from character ([0-9]+) with")
	for from := 0; from < len([]rune(long.Text)); {
		piece := long.Part(from, 20000).Text
		from = len([]rune(long.Text))
		if i := strings.Index(piece, "\n… [characters "); i >= 0 {
			from, _ = strconv.Atoi(next.FindStringSubmatch(piece[i:])[1])
			if piece = piece[:i]; !strings.HasSuffix(piece, " ") && !strings.HasSuffix(piece, "\n") {
				t.Errorf("a page stops inside a word: %q", piece[len(piece)-30:])
			}
		}
		whole.WriteString(piece)
	}
	if whole.String() != long.Text {
		t.Errorf("reading a long entry a page at a time does not give back its text")
	}
	// A second long document on another subject, with much the same words
	// in its body, is not close to the first: openings are compared.
	other := NewNote{Entry: NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Sources: []Source{{Human: "alex"}},
		Text: "Logging cost review: which log buckets are read, by whom, and what each costs a month.\n\n" +
			strings.Repeat("The log bucket review lists every bucket, its reader and its monthly cost in turn. ", 8) + filler}}
	if _, err := s.Remember(other); err != nil {
		t.Errorf("a long document on another subject was held as close to the first: %v", err)
	}
	// The same document offered again is.
	if _, err := s.Remember(other); !errors.Is(err, ErrSimilar) {
		t.Errorf("a long document offered twice: %v, want ErrSimilar", err)
	}

	// A short entry is never altered.
	short := mustAdd(t, s, "Short and whole.", mustEpisode(t, s, OutcomePassed))
	if p := short.Part(0, 300); p.Text != short.Text || p.TextLength != 0 {
		t.Errorf("a short entry was cut: %+v", p)
	}
	// Text copied from a cut listing is not accepted as an entry.
	cut := told(part.Text)
	cut.Separate = true
	if _, err := s.Remember(cut); !errors.Is(err, ErrInvalid) {
		t.Errorf("storing the cut text of an entry: %v, want ErrInvalid", err)
	}
}

func TestM2_TheLibrarySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	episode := mustEpisode(t, s, OutcomePassed)
	old := mustAdd(t, s, "v1", episode)
	replacement := mustSupersede(t, s, old.ID, "v2", episode)
	if err := s.Flag(replacement.ID, OriginLaptop, "v2 is doubtful."); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Entry(replacement.ID)
	if err != nil {
		t.Fatalf("entry after reopen: %v", err)
	}
	want, _ := s.Entry(replacement.ID)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("after reopen:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if kept, err := reopened.Entry(old.ID); err != nil || kept.ValidTo == nil || !kept.ValidTo.Equal(replacement.ValidFrom) {
		t.Errorf("superseded entry after reopen: %+v, %v", kept, err)
	}
	if got := texts(reopened.CurrentEntries(time.Time{})); !equal(got, []string{"v2"}) {
		t.Errorf("current entries after reopen: %q", got)
	}
	if e, err := reopened.Episode(episode.ID); err != nil || e.Body != episode.Body || e.Outcome != OutcomePassed {
		t.Errorf("episode after reopen: %+v, %v", e, err)
	}
}

func TestM2_SupersedingCreatesANewEntryAndKeepsTheOldOne(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	old := mustAdd(t, s, "The service runs in us-east1.", episode)

	replacement := mustSupersede(t, s, old.ID, "The service runs in us-central1.", episode)

	if replacement.ID == old.ID {
		t.Fatal("superseding reused the old entry's ID")
	}
	if replacement.Supersedes == nil || *replacement.Supersedes != old.ID {
		t.Errorf("new entry supersedes %v, want %s", replacement.Supersedes, old.ID)
	}
	kept, err := s.Entry(old.ID)
	if err != nil {
		t.Fatalf("the old entry is no longer retrievable: %v", err)
	}
	if kept.Text != "The service runs in us-east1." {
		t.Errorf("old entry text is now %q", kept.Text)
	}
	if kept.ValidTo == nil || !kept.ValidTo.Equal(replacement.ValidFrom) {
		t.Errorf("old entry valid_to = %v, want the new entry's valid_from %v", kept.ValidTo, replacement.ValidFrom)
	}
	if got := texts(s.CurrentEntries(time.Time{})); !equal(got, []string{"The service runs in us-central1."}) {
		t.Errorf("current entries = %q, want only the new one", got)
	}
}

func TestM2_RecallAsOfAPastDateReturnsWhatWasCurrentThen(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	first := mustAdd(t, s, "v1", episode)
	second := mustSupersede(t, s, first.ID, "v2", episode)
	third := mustSupersede(t, s, second.ID, "v3", episode)
	other := mustAdd(t, s, "unrelated", episode)

	cases := []struct {
		name string
		asOf time.Time
		want []string
	}{
		{"before anything was written", first.ValidFrom.Add(-time.Microsecond), []string{}},
		{"when the first version was written", first.ValidFrom, []string{"v1"}},
		{"just before the first supersession", second.ValidFrom.Add(-time.Microsecond), []string{"v1"}},
		{"at the moment of the first supersession", second.ValidFrom, []string{"v2"}},
		{"just before the second supersession", third.ValidFrom.Add(-time.Microsecond), []string{"v2"}},
		{"at the moment the last entry was added", other.ValidFrom, []string{"v3", "unrelated"}},
		{"now", time.Time{}, []string{"v3", "unrelated"}},
	}
	for _, c := range cases {
		if got := texts(s.CurrentEntries(c.asOf)); !equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestM2_AnEntryCanBeSupersededOnlyOnce(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	old := mustAdd(t, s, "v1", episode)
	mustSupersede(t, s, old.ID, "v2", episode)

	if _, err := s.Supersede(old.ID, newEntry("v2 again", episode)); !errors.Is(err, ErrNotCurrent) {
		t.Errorf("second supersession: got %v, want ErrNotCurrent", err)
	}

	// Many writers racing to supersede one entry: exactly one may win, so
	// history stays a single chain.
	contested := mustAdd(t, s, "contested", episode)
	const writers = 8
	var wg sync.WaitGroup
	var won, lost atomic.Int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Supersede(contested.ID, newEntry(fmt.Sprintf("winner %d", i), episode))
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrNotCurrent):
				lost.Add(1)
			default:
				t.Errorf("writer %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 || lost.Load() != writers-1 {
		t.Errorf("%d writers won and %d lost; want exactly 1 winner", won.Load(), lost.Load())
	}
	if _, err := s.Supersede("no-such-entry", newEntry("x", episode)); !errors.Is(err, ErrNotFound) {
		t.Errorf("superseding an unknown entry: got %v, want ErrNotFound", err)
	}
}

// told is a note a person dictated, as the laptop role offers it.
func told(text string) NewNote {
	return NewNote{Entry: NewEntry{CreatedBy: AuthorAgent, Kind: KindPitfall, Text: text, Sources: []Source{{Human: "alex"}}}}
}

// observed is a note resting on the agent's own observation.
func observed(text string) NewNote {
	return NewNote{
		Entry:    NewEntry{CreatedBy: AuthorAgent, Kind: KindPitfall, Text: text},
		Evidence: &NewEpisode{Origin: OriginWorker, Body: "Saw it happen."},
	}
}

// The lessons here are the ones agents wrote in the transfer benchmark when a
// lesson changed (bench/transfer/RESULTS.md).
const (
	contextOld = "On this machine the default kubectl context is a shared production cluster. The local development cluster is the context named devvm, so every kubectl command for local work must include --context devvm."
	contextNew = "On this machine the default kubectl context is a shared production cluster. The local development cluster's context is lima-dev (renamed from devvm after a rebuild), so every kubectl command for local work must include --context lima-dev."
	pnpmOld    = "Install JS dependencies with 'pnpm install --frozen-lockfile'; never npm (breaks lockfile with org registry)."
	bunNew     = "Org-wide JS tooling is bun: use 'bun install', 'bun add', 'bun remove'. pnpm is no longer installed; do not use pnpm or npm."
)

func TestM2_AnAgentReplacesAnEntryWithANoteAndTheOldOneStays(t *testing.T) {
	s := newStore(t)
	first, err := s.Remember(told(contextOld))
	if err != nil {
		t.Fatal(err)
	}
	before := noteFiles(t, s)

	replace := told(contextNew)
	replace.Replaces = first.ID
	second, err := s.Remember(replace)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if second.Supersedes == nil || *second.Supersedes != first.ID {
		t.Errorf("the replacement does not name what it replaces: %+v", second)
	}
	for path, content := range before {
		if noteFiles(t, s)[path] != content {
			t.Errorf("replacing changed the note %s", path)
		}
	}
	if got := texts(s.Recall(RecallQuery{Text: "kubectl context for local work"})); !equal(got, []string{contextNew}) {
		t.Errorf("recall after the replacement = %q", got)
	}
	if got := texts(s.Recall(RecallQuery{Text: "kubectl context for local work", AsOf: first.CreatedAt})); !equal(got, []string{contextOld}) {
		t.Errorf("recall as of before the replacement = %q", got)
	}
	old, err := s.Entry(first.ID)
	if err != nil || old.ValidTo == nil || old.Text != contextOld {
		t.Errorf("the replaced entry is no longer retrievable as it was: %+v, %v", old, err)
	}

	again := told("The context is now called something else again.")
	again.Replaces = first.ID
	if _, err := s.Remember(again); !errors.Is(err, ErrNotCurrent) {
		t.Errorf("replacing an entry that was already replaced: %v, want ErrNotCurrent", err)
	}
	again.Replaces = "20200101-000000-aaaa"
	if _, err := s.Remember(again); !errors.Is(err, ErrNotFound) {
		t.Errorf("replacing an entry that does not exist: %v, want ErrNotFound", err)
	}
}

func TestM3_AnUnverifiedNoteCannotReplaceATrustedEntry(t *testing.T) {
	s := newStore(t)
	trusted, err := s.Remember(told(contextOld))
	if err != nil {
		t.Fatal(err)
	}
	guess := observed("The local cluster seems to be called lima-dev now.")
	guess.Replaces = trusted.ID
	if _, err := s.Remember(guess); !errors.Is(err, ErrLessTrusted) {
		t.Fatalf("an agent's observation replacing what a person said: %v, want ErrLessTrusted", err)
	}
	if got := s.CurrentEntries(time.Time{}); len(got) != 1 || got[0].ID != trusted.ID {
		t.Errorf("the refused replacement changed what is current: %q", texts(got))
	}

	// An observation may replace another observation, and a person anything.
	first, err := s.Remember(observed(pnpmOld))
	if err != nil {
		t.Fatal(err)
	}
	better := observed(bunNew)
	better.Replaces = first.ID
	second, err := s.Remember(better)
	if err != nil {
		t.Fatalf("an observation replacing an observation: %v", err)
	}
	said := told("Every JavaScript project here uses bun.")
	said.Replaces = second.ID
	if _, err := s.Remember(said); err != nil {
		t.Fatalf("a person's word replacing an observation: %v", err)
	}
}

func TestANoteCloseToAnExistingEntryIsHeldUntilTheCallerSaysWhich(t *testing.T) {
	for _, c := range []struct{ name, old, new string }{
		{"a renamed context", contextOld, contextNew},
		{"a changed package manager", pnpmOld, bunNew},
	} {
		s := newStore(t)
		first, err := s.Remember(told(c.old))
		if err != nil {
			t.Fatal(err)
		}
		files := len(noteFiles(t, s))

		var similar *SimilarError
		if _, err := s.Remember(told(c.new)); !errors.As(err, &similar) || !errors.Is(err, ErrSimilar) {
			t.Fatalf("%s: storing the new version beside the old: %v, want a SimilarError", c.name, err)
		}
		if len(similar.Entries) != 1 || similar.Entries[0].ID != first.ID {
			t.Errorf("%s: the error names %q", c.name, texts(similar.Entries))
		}
		if len(noteFiles(t, s)) != files {
			t.Errorf("%s: a held note was written", c.name)
		}

		// Saying it is a separate thing stores it beside the old one.
		separate := told(c.new)
		separate.Separate = true
		if _, err := s.Remember(separate); err != nil {
			t.Fatalf("%s: separate: %v", c.name, err)
		}
		if got := s.CurrentEntries(time.Time{}); len(got) != 2 {
			t.Errorf("%s: after a separate note, current = %q", c.name, texts(got))
		}
	}

	// Close has to hold both ways. A one-line rule whose few words all occur
	// in the opening of a page about something else is not a version of it.
	page := newStore(t)
	if _, err := page.Remember(told("How pull requests merge in this repository: the bot labels a change, the format check runs before every commit is accepted, and tofu plans are attached to the pull request by the pipeline once the labels are in place and the reviewers have signed off on the change.")); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Remember(told("Run tofu fmt before every commit.")); err != nil {
		t.Errorf("a short rule was held as close to a page that happens to contain its words: %v", err)
	}

	// A lesson about something else is stored without a question, and so is a
	// close one when the entry it resembles has already been replaced.
	s := newStore(t)
	first, _ := s.Remember(told(contextOld))
	if _, err := s.Remember(told(pnpmOld)); err != nil {
		t.Errorf("an unrelated note was held: %v", err)
	}
	replace := told(contextNew)
	replace.Replaces = first.ID
	if _, err := s.Remember(replace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(told(contextOld)); !errors.Is(err, ErrSimilar) {
		t.Errorf("a note close to the current entry: %v, want ErrSimilar", err)
	}
}

func TestM4_ARunIsLoggedAndCanBeRevertedAsAUnit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	passed := mustEpisode(t, s, OutcomePassed)
	remember := func(n NewNote) Entry {
		t.Helper()
		n.Separate = true
		e, err := s.Remember(n)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	toldOnce := remember(told("Deploys are frozen on Fridays."))
	seenTwice := remember(observed("No deploys on Fridays; the pipeline refuses them."))
	stale := remember(observed("The old CI runner needs a manual restart every Monday."))
	untouched := remember(told("Prefer small commits."))
	currentTexts := func() []string { return texts(s.CurrentEntries(time.Time{})) }
	before := currentTexts()

	run, err := s.StartRun("Merge the two notes about Friday deploys; drop the note about the old runner.")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.Curate(Curation{
		Run: run.ID, Supersedes: toldOnce.ID, Absorbs: []string{seenTwice.ID}, From: []string{toldOnce.ID, seenTwice.ID},
		Entry: NewEntry{CreatedBy: AuthorDreamer, Kind: KindPreference, Text: "Deploys are frozen on Fridays, and the pipeline enforces it."},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The merged entry keeps what backed the originals: the person, and the observation.
	if merged.Status != StatusActive || len(merged.Sources) != 2 || merged.Sources[0].Human != "alex" || merged.Sources[1].EpisodeID == "" {
		t.Errorf("merged entry's sources = %+v, status %s", merged.Sources, merged.Status)
	}
	if _, err := s.Retire(run.ID, stale.ID, "The old runner was decommissioned."); err != nil {
		t.Fatalf("retire: %v", err)
	}
	added, err := s.Curate(Curation{Run: run.ID, Entry: newEntry("The migration job needs a restart after a password rotation.", passed)})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	afterRun := time.Now().UTC()

	want := []string{untouched.Text, merged.Text, added.Text}
	if got := currentTexts(); !equal(got, want) {
		t.Errorf("after the run, current = %q\nwant %q", got, want)
	}
	if got, _ := s.Entry(stale.ID); got.ValidTo == nil || got.Text != stale.Text {
		t.Errorf("a retired entry should stay retrievable and not be current: %+v", got)
	}

	// The log says what the run did.
	logged, err := s.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions := ""
	for _, c := range logged.Changes {
		actions += c.Action + " "
	}
	if actions != "supersede retire add " || logged.Size != 4 || logged.Cap != DefaultRunCap ||
		!equal(logged.Changes[0].Replaced, []string{toldOnce.ID, seenTwice.ID}) || logged.Changes[1].Entry != stale.ID {
		t.Errorf("run log = %+v", logged)
	}

	// Reverting undoes all of it at once and deletes nothing.
	files := noteFiles(t, s)
	reverted, err := s.Revert(run.ID, "The merge lost the detail about the pipeline.")
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.RevertedAt == nil {
		t.Errorf("the run does not say it was reverted: %+v", reverted)
	}
	if got := currentTexts(); !equal(got, before) {
		t.Errorf("after the revert, current = %q\nwant %q", got, before)
	}
	for path, content := range files {
		if noteFiles(t, s)[path] != content {
			t.Errorf("reverting changed or removed the note %s", path)
		}
	}
	if got, _ := s.Entry(toldOnce.ID); got.ValidTo != nil {
		t.Errorf("an entry the reverted run had replaced is not current again: %+v", got)
	}
	if got, _ := s.Entry(merged.ID); got.ValidTo == nil {
		t.Errorf("an entry the reverted run added is still current: %+v", got)
	}
	// What was current while the run stood can still be asked for (M2).
	if got := texts(s.Recall(RecallQuery{Text: "deploys frozen Fridays", AsOf: afterRun})); !equal(got, []string{merged.Text}) {
		t.Errorf("recall as of before the revert = %q", got)
	}

	// A reverted run is closed, and cannot be reverted twice.
	if _, err := s.Retire(run.ID, stale.ID, "Again."); !errors.Is(err, ErrInvalid) {
		t.Errorf("a change in a reverted run: %v, want ErrInvalid", err)
	}
	if _, err := s.Revert(run.ID, "Again."); !errors.Is(err, ErrInvalid) {
		t.Errorf("reverting twice: %v, want ErrInvalid", err)
	}

	// All of this is in the notes, so it survives a restart.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(reopened.CurrentEntries(time.Time{})); !equal(got, before) {
		t.Errorf("after a restart, current = %q\nwant %q", got, before)
	}
	if runs := reopened.Runs(); len(runs) != 1 || runs[0].RevertedAt == nil || len(runs[0].Changes) != 3 {
		t.Errorf("after a restart, runs = %+v", runs)
	}
	if problems, err := reopened.Check(); err != nil || len(problems) != 0 {
		t.Errorf("check after a run and a revert: %v, %v", problems, err)
	}
}

// Who sees an entry is its scope. Replacing or merging entries must not
// change that by accident: a repository's rule shown everywhere is followed
// everywhere.
func TestAReplacementKeepsItsScopeUnlessToldOtherwise(t *testing.T) {
	s := newStore(t)
	scoped := func(n NewNote, tags ...string) NewNote {
		n.Entry.Scope = tags
		n.Separate = true
		return n
	}
	must := func(e Entry, err error) Entry {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	tags := func(e Entry) string { return strings.Join(e.Scope, " ") }

	// A working agent's correction stays in the repository it was about.
	rule := must(s.Remember(scoped(told("In this repository the agent may deploy to staging without asking."), "repo:alpha")))
	fix := told("In this repository the agent may deploy to staging and to preview without asking.")
	fix.Replaces = rule.ID
	corrected := must(s.Remember(fix))
	if tags(corrected) != "repo:alpha" {
		t.Errorf("a replacement with no scope given has scope %q, want the old entry's", tags(corrected))
	}
	// It can be moved, or made to apply everywhere, by saying so.
	wide := told("The agent may deploy to staging without asking, in every repository.")
	wide.Replaces, wide.Global = corrected.ID, true
	if e := must(s.Remember(wide)); len(e.Scope) != 0 {
		t.Errorf("a replacement marked global has scope %q", tags(e))
	}

	// Consolidation: merging entries of one scope keeps it.
	run, _ := s.StartRun("Merge duplicates.")
	a := must(s.Remember(scoped(told("The integration tests need the seed script run first."), "repo:alpha")))
	b := must(s.Remember(scoped(told("Run the seed script before the integration tests."), "repo:alpha")))
	merged := must(s.Curate(Curation{Run: run.ID, Supersedes: a.ID, Absorbs: []string{b.ID}, From: []string{a.ID, b.ID},
		Entry: NewEntry{CreatedBy: AuthorDreamer, Kind: KindPitfall, Text: "Run the seed script before the integration tests; they need it."}}))
	if tags(merged) != "repo:alpha" {
		t.Errorf("merging two entries of repo:alpha gave scope %q", tags(merged))
	}

	// Merging across scopes is refused until the caller decides who sees the result.
	c := must(s.Remember(scoped(told("Prefers answers that start with the conclusion."), "repo:alpha")))
	d := must(s.Remember(scoped(told("Start answers with the conclusion, then the evidence."), "repo:beta")))
	across := Curation{Run: run.ID, Supersedes: c.ID, Absorbs: []string{d.ID}, From: []string{c.ID, d.ID},
		Entry: NewEntry{CreatedBy: AuthorDreamer, Kind: KindPreference, Text: "Prefers answers that start with the conclusion, then the evidence."}}
	if _, err := s.Curate(across); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "different scopes") {
		t.Fatalf("merging entries of two repositories with no scope given: %v, want a refusal that says why", err)
	}
	across.Global = true
	if e := must(s.Curate(across)); len(e.Scope) != 0 {
		t.Errorf("a merge marked global has scope %q", tags(e))
	}
	// Or it is given both scopes, and stays out of every other repository.
	e := must(s.Remember(scoped(told("Protobuf code is generated with buf generate."), "repo:alpha")))
	f := must(s.Remember(scoped(told("Run buf generate to regenerate the protobuf code."), "repo:beta")))
	both := must(s.Curate(Curation{Run: run.ID, Supersedes: e.ID, Absorbs: []string{f.ID}, From: []string{e.ID, f.ID},
		Entry: NewEntry{CreatedBy: AuthorDreamer, Kind: KindProcedure, Text: "Protobuf code is generated with buf generate.", Scope: []string{"repo:alpha", "repo:beta"}}}))
	if tags(both) != "repo:alpha repo:beta" {
		t.Errorf("a merge given two scopes has scope %q", tags(both))
	}
	if got, _ := s.Index(IndexQuery{Scope: []string{"repo:gamma"}}); strings.Contains(strings.Join(texts(got), "|"), "buf generate") {
		t.Errorf("an entry scoped to two repositories was shown in a third")
	}
}

// M4: a rewrite by the consolidation role keeps every number, command and
// identifier of the entries it replaces, or says which it drops and why.
func TestM4_ARewriteKeepsEverySpecificOrSaysWhichItDrops(t *testing.T) {
	s := newStore(t)
	passed := mustEpisode(t, s, OutcomePassed)
	deploy := mustAdd(t, s, "Staging is deployed with `make deploy ENV=staging`; the suite takes 14 minutes on the runner ci-large_2.", passed)
	vpn := mustAdd(t, s, "A staging deploy needs the VPN up, see ticket OPS-311. The runBook says so too.", passed)
	run, err := s.StartRun("Merge the two notes about staging deploys.")
	if err != nil {
		t.Fatal(err)
	}
	merge := func(text string, drops ...string) error {
		_, err := s.Curate(Curation{Run: run.ID, Supersedes: deploy.ID, Absorbs: []string{vpn.ID}, From: []string{deploy.ID, vpn.ID},
			Entry: NewEntry{CreatedBy: AuthorDreamer, Kind: KindProcedure, Text: text}, Drops: drops})
		return err
	}
	// A merge that reads well and has lost the command and the ticket.
	err = merge("To deploy staging, bring the VPN up first and run the deploy target; the suite takes 14 minutes on ci-large_2.")
	if !errors.Is(err, ErrLosesSpecifics) {
		t.Fatalf("a merge that lost a command: %v, want ErrLosesSpecifics", err)
	}
	for _, want := range []string{"`make deploy ENV=staging`", "`OPS-311`", "`runBook`", "--drops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "ci-large_2") || strings.Contains(err.Error(), "`14`") {
		t.Errorf("the refusal names something the text kept: %v", err)
	}
	// Nothing was written, and the run has room as before.
	if got, _ := s.Run(run.ID); len(got.Changes) != 0 {
		t.Errorf("a refused merge is in the run's log: %+v", got.Changes)
	}
	// Leaving one out on purpose is allowed when it is said, and is on record.
	text := "To deploy staging, bring the VPN up first (ticket OPS-311, and the runBook) and run `make deploy ENV=staging`; the suite takes 9 minutes on ci-large_2."
	if err := merge(text); !errors.Is(err, ErrLosesSpecifics) || !strings.Contains(err.Error(), "`14`") {
		t.Fatalf("a merge that changed a number without saying so: %v", err)
	}
	if err := merge(text, "14: the suite takes 9 minutes since the runner was upgraded"); err != nil {
		t.Fatalf("a merge that says what it drops: %v", err)
	}
	got, _ := s.Run(run.ID)
	if len(got.Changes) != 1 || len(got.Changes[0].Drops) != 1 || !strings.HasPrefix(got.Changes[0].Drops[0], "14: ") {
		t.Errorf("what was dropped is not in the run's log: %+v", got.Changes)
	}
	// And it is still there after the library is read again.
	reopened, err := Open(s.lib.dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := reopened.Run(run.ID); len(again.Changes) != 1 || len(again.Changes[0].Drops) != 1 {
		t.Errorf("what was dropped did not survive a restart: %+v", again.Changes)
	}
}

func TestM4_ARunIsCappedAndEveryChangeNeedsOne(t *testing.T) {
	s := newStore(t)
	s.RunCap = 3
	passed := mustEpisode(t, s, OutcomePassed)
	a, b, c := mustAdd(t, s, "First.", passed), mustAdd(t, s, "Second.", passed), mustAdd(t, s, "Third.", passed)
	d := mustAdd(t, s, "Fourth.", passed)

	if _, err := s.Curate(Curation{Entry: newEntry("No run.", passed)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a change outside any run: %v, want ErrNotFound", err)
	}
	if _, err := s.Retire("20200101-000000-aaaa", a.ID, "No such run."); !errors.Is(err, ErrNotFound) {
		t.Errorf("a change in a run that does not exist: %v, want ErrNotFound", err)
	}
	if _, err := s.StartRun("  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("a run with no purpose: %v, want ErrInvalid", err)
	}

	run, err := s.StartRun("Tidy up.")
	if err != nil {
		t.Fatal(err)
	}
	// A merge of three entries counts as three changes, which fills the run.
	if _, err := s.Curate(Curation{Run: run.ID, Supersedes: a.ID, Absorbs: []string{b.ID, c.ID}, Entry: newEntry("One to three.", passed)}); err != nil {
		t.Fatalf("merge within the cap: %v", err)
	}
	if _, err := s.Retire(run.ID, d.ID, "One too many."); !errors.Is(err, ErrRunFull) {
		t.Errorf("a change past the cap: %v, want ErrRunFull", err)
	}
	if got, _ := s.Entry(d.ID); got.ValidTo != nil {
		t.Errorf("the refused change took effect: %+v", got)
	}

	// An entry that is no longer current cannot be replaced or retired again.
	other, _ := s.StartRun("More tidying.")
	if _, err := s.Retire(other.ID, a.ID, "Already merged."); !errors.Is(err, ErrNotCurrent) {
		t.Errorf("retiring a replaced entry: %v, want ErrNotCurrent", err)
	}
	if _, err := s.Curate(Curation{Run: other.ID, Supersedes: d.ID, Absorbs: []string{d.ID}, Entry: newEntry("Twice.", passed)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("naming an entry twice in a merge: %v, want ErrInvalid", err)
	}
}

func TestM3_EveryEntryCitesAnEpisodeOrANamedHuman(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)

	rejected := []struct {
		name    string
		sources []Source
		want    error
	}{
		{"no sources", nil, ErrNoSource},
		{"an empty source", []Source{{}}, ErrNoSource},
		{"both episode and human", []Source{{EpisodeID: episode.ID, Human: "alex"}}, ErrNoSource},
		{"an episode that is not there", []Source{{EpisodeID: "20200101-000000-aaaa"}}, ErrNotFound},
	}
	for _, c := range rejected {
		n := newEntry("x", episode)
		n.Sources = c.sources
		if _, err := s.AddEntry(n); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if current := s.CurrentEntries(time.Time{}); len(current) != 0 {
		t.Errorf("refused entries were stored: %q", texts(current))
	}
}

func TestM3_StatusFollowsTheSources(t *testing.T) {
	s := newStore(t)
	passed := Source{EpisodeID: mustEpisode(t, s, OutcomePassed).ID}
	failed := Source{EpisodeID: mustEpisode(t, s, OutcomeFailed).ID}
	unverified := Source{EpisodeID: mustEpisode(t, s, OutcomeUnverified).ID}
	human := Source{Human: "alex"}

	cases := []struct {
		name    string
		sources []Source
		want    Status
	}{
		{"only a failed episode", []Source{failed}, StatusUnverified},
		{"only an unverified episode", []Source{unverified}, StatusUnverified},
		{"failed and unverified episodes", []Source{failed, unverified}, StatusUnverified},
		{"a passed episode", []Source{passed}, StatusActive},
		{"a passed episode among failed ones", []Source{failed, passed}, StatusActive},
		{"a named human", []Source{human}, StatusActive},
		{"a named human and a failed episode", []Source{failed, human}, StatusActive},
	}
	for _, c := range cases {
		n := NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Text: c.name, Sources: c.sources}
		added, err := s.AddEntry(n)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if added.Status != c.want {
			t.Errorf("%s: added with status %q, want %q", c.name, added.Status, c.want)
		}
		// The same rule applies to the entry that replaces it.
		replaced, err := s.Supersede(added.ID, n)
		if err != nil {
			t.Fatalf("%s: supersede: %v", c.name, err)
		}
		if replaced.Status != c.want {
			t.Errorf("%s: superseded with status %q, want %q", c.name, replaced.Status, c.want)
		}
	}
}

func TestM3_RecallShowsEachSourceAndItsOutcome(t *testing.T) {
	s := newStore(t)
	passed := mustEpisode(t, s, OutcomePassed)
	failed := mustEpisode(t, s, OutcomeFailed)
	_, err := s.AddEntry(NewEntry{
		CreatedBy: AuthorAgent, Kind: KindPitfall, Text: "The linter config is generated; edit the template.",
		Sources: []Source{{EpisodeID: passed.ID}, {EpisodeID: failed.ID}, {Human: "alex"}},
	})
	if err != nil {
		t.Fatalf("add entry: %v", err)
	}

	found := s.Recall(RecallQuery{Text: "linter config"})
	if len(found) != 1 {
		t.Fatalf("recall returned %d entries, want 1", len(found))
	}
	got := map[Source]bool{}
	for _, src := range found[0].Sources {
		got[src] = true
	}
	for _, want := range []Source{
		{EpisodeID: passed.ID, Outcome: OutcomePassed},
		{EpisodeID: failed.ID, Outcome: OutcomeFailed},
		{Human: "alex"},
	} {
		if !got[want] {
			t.Errorf("recall result lacks source %+v; it has %+v", want, found[0].Sources)
		}
	}
}

func TestM3_ANoteKeepsItsEvidenceAndStaysUnverified(t *testing.T) {
	s := newStore(t)
	evidence := NewEpisode{Origin: OriginWorker, TaskID: "task-7", Body: "kubectl rollout hung until the PDB was relaxed."}
	claim := NewEntry{CreatedBy: AuthorAgent, Kind: KindPitfall, Text: "Rollouts in prod hang on the PodDisruptionBudget."}

	note, err := s.Note(evidence, claim)
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if note.Status != StatusUnverified {
		t.Errorf("a note backed only by its own evidence has status %q, want unverified", note.Status)
	}
	if len(note.Sources) != 1 || note.Sources[0].Outcome != OutcomeUnverified {
		t.Fatalf("note sources = %+v, want one unverified episode", note.Sources)
	}
	episode, err := s.Episode(note.Sources[0].EpisodeID)
	if err != nil {
		t.Fatalf("the evidence episode is not retrievable: %v", err)
	}
	if episode.Body != evidence.Body || episode.TaskID != "task-7" {
		t.Errorf("evidence stored as %+v", episode)
	}

	// A note with no text is refused, and its evidence is not left behind.
	before := len(noteFiles(t, s))
	if _, err := s.Note(evidence, NewEntry{CreatedBy: AuthorAgent, Kind: KindFact}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty note: got %v, want ErrInvalid", err)
	}
	if after := len(noteFiles(t, s)); after != before {
		t.Errorf("a refused note left %d file(s) behind", after-before)
	}
}

func TestM5_RecallRanksEntriesByTheWordsTheyShareWithTheQuery(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	add := func(kind Kind, scope, text string) Entry {
		t.Helper()
		e, err := s.AddEntry(NewEntry{
			CreatedBy: AuthorAgent, Kind: kind, Text: text, Scope: []string{scope},
			Sources: []Source{{EpisodeID: episode.ID}},
		})
		if err != nil {
			t.Fatalf("add entry: %v", err)
		}
		return e
	}
	deploy := add(KindProcedure, "repo:api", "Deploying the api service requires running the database migration job first.")
	add(KindFact, "repo:api", "The api service listens on port 8443 behind the internal load balancer.")
	add(KindPitfall, "repo:web", "The web build fails when node modules are cached across major versions.")
	oldRegion := add(KindFact, "repo:api", "The database for the api service is hosted in us-east1.")
	newRegion := mustSupersede(t, s, oldRegion.ID, "The database for the api service is hosted in us-central1.", episode)

	recall := func(q RecallQuery) []string { return texts(s.Recall(q)) }

	// A long, natural-language question still finds the entry, and the entry
	// sharing the most words comes first.
	got := recall(RecallQuery{Text: "what do I need to do before deploying the api service to production?"})
	if len(got) == 0 || got[0] != deploy.Text {
		t.Errorf("deploy question: got %q, want the deploy procedure first", got)
	}

	// A superseded entry is not recalled now, but is as of an earlier time.
	now := recall(RecallQuery{Text: "where is the database hosted"})
	if len(now) == 0 || now[0] != newRegion.Text {
		t.Errorf("region now: got %q", now)
	}
	for _, text := range now {
		if text == oldRegion.Text {
			t.Errorf("recall returned the superseded entry %q", text)
		}
	}
	then := recall(RecallQuery{Text: "where is the database hosted", AsOf: newRegion.ValidFrom.Add(-time.Microsecond)})
	if len(then) == 0 || then[0] != oldRegion.Text {
		t.Errorf("region as of before the move: got %q, want the old entry first", then)
	}

	if got := recall(RecallQuery{Text: "build cache", Scope: []string{"repo:api"}}); len(got) != 0 {
		t.Errorf("scope filter: got %q, want nothing from repo:api about build caches", got)
	}
	if got := recall(RecallQuery{Text: "api service", Kinds: []Kind{KindProcedure}}); !equal(got, []string{deploy.Text}) {
		t.Errorf("kind filter: got %q, want only the procedure", got)
	}
	if got := recall(RecallQuery{Text: "api service", Limit: 2}); len(got) != 2 {
		t.Errorf("limit 2: got %d entries", len(got))
	}
	// An identifier written as one word finds a note written in plain words.
	if got := recall(RecallQuery{Text: "migrationJob databaseMigration"}); len(got) == 0 || got[0] != deploy.Text {
		t.Errorf("compound-word query: got %q, want the deploy procedure first", got)
	}
	if got := recall(RecallQuery{Text: "kubernetes ingress certificates"}); len(got) != 0 {
		t.Errorf("unrelated query: got %q, want nothing", got)
	}
	// With no words to search for, recall lists the newest current entries.
	if got := recall(RecallQuery{}); len(got) != 4 {
		t.Errorf("empty query: got %d entries, want the 4 current ones", len(got))
	}
}

func TestM5_FlashbackVolunteersOnlyCloseMatchesAndNeverRepeats(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	add := func(scope, text string) Entry {
		t.Helper()
		e, err := s.AddEntry(NewEntry{
			CreatedBy: AuthorAgent, Kind: KindPitfall, Text: text, Sources: []Source{{EpisodeID: episode.ID}},
			Scope: strings.Fields(scope),
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	pdb := add("repo:api", "Rollouts hang on the PodDisruptionBudget; relax it before a rollout.")
	add("repo:api", "The api service listens on port 8443.")
	add("repo:web", "Rollouts of the web app hang when the CDN purge is slow.")
	global := add("", "Run the linter before every commit.")

	// A long error message brings up the short entry it contains.
	failure := `error: timed out waiting for the condition: deployment "api" rollout hang:
		cannot evict pod as it would violate the pod's disruption budget (PodDisruptionBudget api-pdb); retry or relax it`
	got := s.Flashback(FlashbackQuery{SessionID: "s1", Event: "tool-failure", Text: failure, Scope: []string{"repo:api"}})
	if len(got) != 1 || got[0].ID != pdb.ID {
		t.Fatalf("flashback on the failure: got %q, want only the PodDisruptionBudget entry", texts(got))
	}
	// The same moment again in the same session adds nothing.
	if again := s.Flashback(FlashbackQuery{SessionID: "s1", Text: failure, Scope: []string{"repo:api"}}); len(again) != 0 {
		t.Errorf("flashback repeated %q in one session", texts(again))
	}
	// Another session gets it.
	if other := s.Flashback(FlashbackQuery{SessionID: "s2", Text: failure, Scope: []string{"repo:api"}}); len(other) != 1 {
		t.Errorf("a second session: got %q", texts(other))
	}

	// A short prompt brings up an entry that contains all of it, including
	// one with no scope of its own.
	got = s.Flashback(FlashbackQuery{SessionID: "s3", Text: "run the linter", Scope: []string{"repo:api"}})
	if len(got) != 1 || got[0].ID != global.ID {
		t.Errorf("flashback on a short prompt: got %q", texts(got))
	}
	// A loosely related prompt brings up nothing.
	for _, prompt := range []string{"add a retry to the api client", "why is the service slow today", "hang the picture"} {
		if got := s.Flashback(FlashbackQuery{SessionID: "s4", Text: prompt, Scope: []string{"repo:api"}}); len(got) != 0 {
			t.Errorf("flashback on %q volunteered %q", prompt, texts(got))
		}
	}
	// What was shown survives a restart, so a resumed session gets no repeats.
	reopened, err := Open(s.lib.dir)
	if err != nil {
		t.Fatal(err)
	}
	if again := reopened.Flashback(FlashbackQuery{SessionID: "s1", Text: failure, Scope: []string{"repo:api"}}); len(again) != 0 {
		t.Errorf("after a restart flashback repeated %q", texts(again))
	}
}

// What a moment may bring up depends on the moment. Seen in use: in one long
// session 19 entries were brought up before shell commands, matched on the
// words of scripts and the parts of file names, and almost none bore on the
// command being run.
// What agents asked to use the memory ran into, searching a library of real
// documents: an unrelated long entry ranked first, ten results came back for
// every question whether or not anything matched, and the entries of another
// repository were among them.
func TestM5_SearchReturnsWhatAppliesAndSaysHowWellItMatched(t *testing.T) {
	s := newStore(t)
	add := func(scope, text string) Entry {
		t.Helper()
		e, err := s.Remember(NewNote{Separate: true, Entry: NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Text: text, Scope: strings.Fields(scope), Sources: []Source{{Human: "alex"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	body := strings.Repeat("The weekly notes mention the spot plan, a commitment, a constraint or two, node pools and prices in passing. ", 60)
	plan := add("repo:infra", "Spot plan for the quarter: the baseline, the commitment constraint, and the waves.\n\nThe commitment is fully used every day, so on-demand should sit on that line.")
	add("repo:infra", "Tenant isolation review, with weekly notes.\n\n"+body)
	add("", "Prefer small commits with one logical change each.")
	elsewhere := add("repo:billing", "Spot plan notes for billing: the commitment constraint does not apply to invoices.")
	for i := 0; i < 12; i++ {
		add("repo:infra", fmt.Sprintf("Service %d restarts cleanly after a node pool upgrade when its readiness probe is patient.", i))
	}

	// The entry whose opening is about the question comes before a long one
	// that mentions the same words somewhere in its body.
	here := []string{"repo:infra"}
	found, total, _, weak := s.Search(RecallQuery{Text: "spot plan commitment constraint", Scope: here})
	if len(found) == 0 || found[0].ID != plan.ID || weak {
		t.Fatalf("searching for the spot plan: first is %q, weak %v", texts(found)[:1], weak)
	}
	// Results that match far less well than the best are not padded in.
	if len(found) > 3 || total < len(found) {
		t.Errorf("%d results for a question with one good answer (of %d matching at all): %q", len(found), total, texts(found))
	}
	// A scope keeps what applies there: its own entries and the ones that apply everywhere.
	for _, e := range found {
		if e.ID == elsewhere.ID {
			t.Errorf("a search in repo:infra returned repo:billing's entry")
		}
	}
	if got, _, _, _ := s.Search(RecallQuery{Text: "small commits", Scope: here}); len(got) == 0 || !strings.HasPrefix(got[0].Text, "Prefer small commits") {
		t.Errorf("a search in repo:infra did not find the entry that applies everywhere: %q", texts(got))
	}
	if got, _, _, _ := s.Search(RecallQuery{Text: "spot plan commitment constraint"}); !slices.ContainsFunc(got, func(e Entry) bool { return e.ID == elsewhere.ID }) {
		t.Errorf("a search with no scope did not reach repo:billing: %q", texts(got))
	}
	// A question nothing is about still returns the nearest, and says they are weak.
	if got, _, _, weak := s.Search(RecallQuery{Text: "preferred answer length for short replies with node details", Scope: here}); len(got) == 0 || !weak {
		t.Errorf("a question nothing is about: %d results, weak %v", len(got), weak)
	}
	if got, total, _, _ := s.Search(RecallQuery{Text: "kangaroo marsupial", Scope: here}); len(got) != 0 || total != 0 {
		t.Errorf("a question sharing no words with anything returned %q", texts(got))
	}
}

// A long entry has a map: its paragraphs, or when there are too many of them
// its headings and the paragraphs between, each with where it starts.
func TestOutlineMapsALongText(t *testing.T) {
	text := "Capacity plan for the year.\n\n## January: baseline\n\nMeasured the baseline.\nIt was low.\n\n## February: first wave\n\nMoved the batch pools.\n\n## March: second wave\n\nMoved the api pools."
	got := Outline(text, 60)
	if len(got) != 7 || got[0].Text != "Capacity plan for the year." || got[1].Text != "## January: baseline" || got[2].Text != "Measured the baseline." {
		t.Fatalf("outline = %+v", got)
	}
	runes := []rune(text)
	for _, p := range got {
		if !strings.HasPrefix(string(runes[p.At:]), strings.TrimSuffix(p.Text, "…")) {
			t.Errorf("part %q is not at character %d", p.Text, p.At)
		}
	}
	// With more paragraphs than fit, the map keeps the start and the headings.
	if got := Outline(text, 4); len(got) != 4 || got[0].At != 0 || !strings.HasPrefix(got[3].Text, "## March") {
		t.Errorf("outline limited to four = %+v", got)
	}
	// A long stretch with no heading is still on the map: the room left
	// after the headings goes to the paragraphs that divide it.
	notes := "Working notes.\n\n" + strings.Repeat("An early paragraph with no heading over it.\n\n", 80) + "## Late heading\n\nThe end."
	got = Outline(notes, 10)
	gap := 0
	for i := 1; i < len(got); i++ {
		gap = max(gap, got[i].At-got[i-1].At)
	}
	if len(got) != 10 || got[0].At != 0 || gap > len(notes)/4 {
		t.Errorf("outline of notes with one late heading: %d parts, longest stretch %d of %d", len(got), gap, len(notes))
	}
	// With no headings to fall back on, it still spans the text.
	many := strings.Repeat("A paragraph.\n\n", 100)
	if got := Outline(many, 10); len(got) == 0 || len(got) > 10 || got[len(got)-1].At < len(many)/2 {
		t.Errorf("outline of a hundred paragraphs limited to ten: %d parts, last at %d", len(got), got[len(got)-1].At)
	}
}

// Where in a long entry is the thing asked about?
func TestPassagesFindsThePlaceInALongText(t *testing.T) {
	filler := strings.Repeat("The plan was discussed again and nothing about the plan changed that week. ", 120)
	text := "Capacity plan.\n\n" + filler + "The quota ceiling for the eu region is 4,200 vCPU, raised by ticket in March. " + filler +
		"The plan for the us region has no ceiling. " + filler
	got := Passages(text, "what is the quota ceiling in the eu region plan", 2, 300)
	if len(got) == 0 || !strings.Contains(got[0].Text, "4,200 vCPU") {
		t.Fatalf("the best passage is %+v", got)
	}
	// It says where, so that reading can go on from there.
	if at := strings.Index(text, "The quota ceiling"); got[0].At > at || got[0].At < at-300 {
		t.Errorf("the passage is reported at character %d; the sentence is at %d", got[0].At, at)
	}
	if len(got) == 2 && got[1].At < got[0].At+300 && got[0].At < got[1].At+300 {
		t.Errorf("two passages overlap: %d and %d", got[0].At, got[1].At)
	}
	if got := Passages(text, "kangaroo", 2, 300); len(got) != 0 {
		t.Errorf("a word the text does not have found %+v", got)
	}

	// A text with paragraphs is answered in its own paragraphs, whole, in the
	// order they come, whichever matches best.
	notes := "# Rollout\n\nGeneral remarks about the rollout.\n\n## March\n\nThe ceiling was 4,200 vCPU in March.\n\n" +
		strings.Repeat("An unrelated paragraph about lunch.\n\n", 40) +
		"## November\n\nOn November 10 the quota ceiling was raised: the eu region ceiling is now 6,000 vCPU."
	got = Passages(notes, "eu region quota ceiling", 2, 300)
	if len(got) != 2 || got[0].Text != "The ceiling was 4,200 vCPU in March." || !strings.HasPrefix(got[1].Text, "On November 10") {
		t.Fatalf("passages of a text with paragraphs = %+v", got)
	}
	// Each says which part of the document it is in.
	if got[0].Under != "## March" || got[1].Under != "## November" {
		t.Errorf("passages stand under %q and %q", got[0].Under, got[1].Under)
	}
	// A paragraph too long to show whole stops where a sentence does, and says it goes on.
	long := strings.Repeat("The quota ceiling was discussed at length. ", 30)
	if cut := Passages(long, "quota ceiling", 1, 300); len(cut) != 1 || !strings.HasSuffix(cut[0].Text, "at length. …") {
		t.Errorf("a passage of a long paragraph = %+v", cut)
	}
	for _, p := range got {
		if at := strings.Index(notes, p.Text); len([]rune(notes[:at])) != p.At {
			t.Errorf("passage %q reported at %d, is at %d", p.Text, p.At, len([]rune(notes[:at])))
		}
	}
}

// A note's file is never overwritten, so two notes may never be given one ID.
func TestM2_NoTwoNotesShareAnID(t *testing.T) {
	s := &Store{}
	seen := map[string]bool{}
	for i := 0; i < 20000; i++ {
		id := s.newNote("entry").ID
		if seen[id] {
			t.Fatalf("ID %s given twice, at note %d", id, i)
		}
		seen[id] = true
	}
}

func TestM5_EachKindOfMomentHasItsOwnFlashbackPolicy(t *testing.T) {
	s := newStore(t)
	add := func(kind Kind, text string) Entry {
		t.Helper()
		e, err := s.Remember(NewNote{Separate: true, Entry: NewEntry{CreatedBy: AuthorAgent, Kind: kind, Text: text, Sources: []Source{{Human: "alex"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	pitfall := add(KindPitfall, "On this machine kubectl points at a shared cluster by default; name the local context on every command.")
	document := add(KindFact, "Cluster migration plan: how kubectl contexts, node pools and deploy scripts change in each wave.\n\nWave one moves the batch pools.")
	add(KindPreference, "Deploys are frozen on Fridays; ask before running a deploy script then.")
	purge := add(KindProcedure, "After deploying the web app, purge the CDN cache for the index page, or users keep the old bundle.")

	before := func(session, command string, names ...string) []string {
		return texts(s.Flashback(FlashbackQuery{SessionID: session, Event: "pre-tool", Text: command, Names: names}))
	}
	// Before a command: guidance that names a program it runs, and nothing else.
	if got := before("a", "kubectl get nodes", "kubectl"); !equal(got, []string{pitfall.Text}) {
		t.Errorf("before a kubectl command: %q, want the pitfall about kubectl and not the document that mentions it", got)
	}
	// When several entries name the program, the one that shares most with
	// the rest of the command comes first: it is about this use of it.
	add(KindPreference, "Reports on spend include live figures; when the billing export is late, fall back to gcloud for them.")
	add(KindPreference, "Check a cloud feature against the live API, and never offer changes made by hand with gcloud for managed infrastructure.")
	teleport := add(KindProcedure, "Shell access to the bastion goes through Teleport: `tsh ssh ops@bastion-1`. Do not use `gcloud compute ssh`.")
	if got := before("g", "gcloud compute ssh bastion-2", "gcloud"); len(got) != 2 || got[0] != teleport.Text {
		t.Errorf("before gcloud compute ssh: %q, want the entry about ssh to the bastion first", got)
	}
	// Not the words of the command or of a script inside it...
	if got := before("b", "python3 - <<E\n# deploy the cluster migration plan to each context on this machine, by wave and node pool\nE", "python3"); len(got) != 0 {
		t.Errorf("a script's words brought up %q", got)
	}
	// ...and not the parts of a script's file name.
	if got := before("c", "scripts/deploy.sh devvm", "deploy.sh"); len(got) != 0 {
		t.Errorf("a script named deploy.sh brought up %q", got)
	}
	// A prompt is prose, and may bring up a document about what it asks.
	if got := texts(s.Flashback(FlashbackQuery{SessionID: "d", Event: "prompt", Text: "what is the cluster migration plan for the node pools in each wave?"})); !equal(got, []string{document.Text}) {
		t.Errorf("a prompt about the plan brought up %q", got)
	}
	// The end of a reply makes the turn go on, so only what might change the
	// answer counts there: a procedure the reply skipped, not a document on
	// the same subject.
	reply := "The cluster migration plan is done for wave one: the node pools moved and each kubectl context was changed. I deployed the web app as well."
	if got := texts(s.Flashback(FlashbackQuery{SessionID: "e", Event: "proposal", Text: reply})); len(got) != 0 {
		t.Errorf("a reply on the plan's subject was interrupted with %q", got)
	}
	done := "The deploy of the web app finished. Users may need a hard refresh to get the new bundle from the index page."
	if got := texts(s.Flashback(FlashbackQuery{SessionID: "f", Event: "proposal", Text: done})); !equal(got, []string{purge.Text}) {
		t.Errorf("a reply that skipped the purge was answered with %q", got)
	}
}

func TestM5_TheSessionIndexFitsABudgetAndPutsTrustedEntriesFirst(t *testing.T) {
	s := newStore(t)
	passed, unverified := mustEpisode(t, s, OutcomePassed), mustEpisode(t, s, OutcomeUnverified)
	add := func(episode Episode, scope, text string) Entry {
		t.Helper()
		e, err := s.AddEntry(NewEntry{
			CreatedBy: AuthorAgent, Kind: KindFact, Text: text, Scope: strings.Fields(scope),
			Sources: []Source{{EpisodeID: episode.ID}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	guess := add(unverified, "repo:api", "The cache may need warming after deploys.")
	doubted := add(passed, "repo:api", "CI runs on every push.")
	trusted := add(passed, "repo:api", "Tests need the TZ variable set to UTC.")
	add(passed, "repo:web", "The web app uses pnpm.")
	everywhere := add(passed, "", "Prefer small commits.")
	if err := s.Flag(doubted.ID, OriginWorker, "Only on pull requests now."); err != nil {
		t.Fatal(err)
	}

	entries, total := s.Index(IndexQuery{SessionID: "s1", Scope: []string{"repo:api"}})
	want := []string{everywhere.Text, trusted.Text, doubted.Text, guess.Text}
	if total != 4 || !equal(texts(entries), want) {
		t.Errorf("index for repo:api: total %d, %q\nwant %q", total, texts(entries), want)
	}

	// A tight budget keeps the best entries and reports how many there are.
	entries, total = s.Index(IndexQuery{Scope: []string{"repo:api"}, Budget: 60})
	if total != 4 || len(entries) != 2 || entries[0].ID != everywhere.ID {
		t.Errorf("index with a budget of 60 characters: total %d, %q", total, texts(entries))
	}

	// When the index is cut, what an agent must have in front of it comes
	// before what it can look up: preferences and pitfalls before procedures
	// and facts, whatever their age.
	kinds := newStore(t)
	for _, n := range []struct {
		kind Kind
		text string
	}{
		{KindPreference, "Old preference: answers start with the conclusion."},
		{KindFact, "A fact noted later."},
		{KindProcedure, "A procedure noted later still."},
		{KindPitfall, "A pitfall noted after that."},
		{KindFact, "The newest note of all, a fact."},
	} {
		if _, err := kinds.Remember(NewNote{Separate: true, Entry: NewEntry{CreatedBy: AuthorAgent, Kind: n.kind, Text: n.text, Sources: []Source{{Human: "alex"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	ordered, _ := kinds.Index(IndexQuery{})
	if got := texts(ordered); !strings.HasPrefix(got[0], "Old preference") || !strings.HasPrefix(got[1], "A pitfall") ||
		!strings.HasPrefix(got[2], "A procedure") || !strings.HasPrefix(got[3], "The newest note") {
		t.Errorf("index order by kind then age = %q", got)
	}

	// A long entry costs the budget only what the index shows of it.
	long := add(passed, "repo:docs", strings.Repeat("The style guide says many things. ", 40))
	short := add(passed, "repo:docs", "Docs are built with mdbook.")
	entries, _ = s.Index(IndexQuery{Scope: []string{"repo:docs"}, Budget: IndexLineLimit + len(short.Text) + len(everywhere.Text)})
	if len(entries) != 3 {
		t.Errorf("a long entry (%d characters) used more of the budget than its line: %d entries shown", len(long.Text), len(entries))
	}

	// What the index showed, flashback does not show again.
	got := s.Flashback(FlashbackQuery{SessionID: "s1", Text: "tests fail unless TZ is set to UTC", Scope: []string{"repo:api"}})
	if len(got) != 0 {
		t.Errorf("flashback repeated an indexed entry: %q", texts(got))
	}
}

func TestFlagCountsAContradictionAndLeavesTheEntryAsItWas(t *testing.T) {
	s := newStore(t)
	entry := mustAdd(t, s, "CI runs on every push.", mustEpisode(t, s, OutcomePassed))

	if err := s.Flag(entry.ID, OriginWorker, "CI only runs on pull requests now."); err != nil {
		t.Fatalf("flag: %v", err)
	}
	after, err := s.Entry(entry.ID)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	if after.Contradictions != 1 || after.Text != entry.Text || after.ValidTo != nil {
		t.Errorf("after one flag: %+v", after)
	}
	if err := s.Flag("no-such-entry", OriginWorker, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("flag on an unknown entry: got %v, want ErrNotFound", err)
	}
	if err := s.Flag(entry.ID, OriginWorker, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("flag with no reason: got %v, want ErrInvalid", err)
	}
}

// A stand-in where a password would go is how documentation shows the shape
// of a connection string. It is not a credential, and M6 is not a reason to
// refuse a document that explains how to connect to something.
func TestM6_AConnectionStringWithAStandInForItsPasswordIsStored(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	for _, text := range []string{
		"Connect with rediss://default:<token>@<PSC address>:6379 once the IAM binding exists.",
		"The URL is postgres://app:$DB_PASSWORD@db.internal/app and the password comes from the secret manager.",
		"Use amqp://user:${AMQP_PASS}@broker and https://deploy:***@registry.example for the examples; redis://default:PASSWORD@host works too.",
	} {
		if _, err := s.AddEntry(newEntry(text, episode)); err != nil {
			t.Errorf("a URL with a stand-in for its password was refused: %v\n%s", err, text)
		}
	}
	// The same shape with something real in that place is still refused.
	if _, err := s.AddEntry(newEntry("Connect with rediss://default:Zx81kq0vT3mPw9@10.0.0.5:6379 for now.", episode)); !errors.Is(err, ErrSecret) {
		t.Errorf("a URL with a real-looking password: %v, want ErrSecret", err)
	}
}

func TestM6_AWriteThatLooksLikeItHoldsACredentialIsRefused(t *testing.T) {
	s := newStore(t)
	episode := mustEpisode(t, s, OutcomePassed)
	entry := mustAdd(t, s, "The deploy key lives in the vault under ci/deploy.", episode)
	before := len(noteFiles(t, s))

	secrets := map[string]string{
		"an AWS access key":     "use AKIAIOSFODNN7EXAMPLE for the bucket",
		"a GitHub token":        "export GH_TOKEN=ghp_" + strings.Repeat("a1B2", 9),
		"a private key":         "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----",
		"an API key":            "the key is sk-ant-" + strings.Repeat("x9", 20),
		"a JSON web token":      "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"a bearer token":        "curl -H 'Authorization: Bearer abcdef0123456789abcdef0123456789'",
		"a URL with a password": "postgres://admin:hunter2secret@db.internal:5432/app",
		"an assigned password":  `set DB_PASSWORD in the env; currently password = "c0rrect-h0rse-battery"`,
	}
	for name, text := range secrets {
		attempts := map[string]error{
			"episode": func() error {
				_, err := s.AppendEpisode(NewEpisode{Origin: OriginWorker, Outcome: OutcomePassed, Body: text})
				return err
			}(),
			"entry": func() error { _, err := s.AddEntry(newEntry(text, episode)); return err }(),
			"note": func() error {
				_, err := s.Note(NewEpisode{Origin: OriginWorker, Body: text}, NewEntry{CreatedBy: AuthorAgent, Kind: KindFact, Text: "It worked."})
				return err
			}(),
			"supersede": func() error { _, err := s.Supersede(entry.ID, newEntry(text, episode)); return err }(),
			"flag":      s.Flag(entry.ID, OriginWorker, text),
		}
		for write, err := range attempts {
			if !errors.Is(err, ErrSecret) {
				t.Errorf("%s in a %s: got %v, want ErrSecret", name, write, err)
			} else if strings.Contains(err.Error(), text) {
				t.Errorf("%s in a %s: the error repeats the secret: %v", name, write, err)
			}
		}
	}
	if after := len(noteFiles(t, s)); after != before {
		t.Errorf("refused writes left %d note(s) behind", after-before)
	}

	// Ordinary text that mentions secrets without containing one is stored.
	harmless := []string{
		"The token: see the vault path secret/data/app.",
		"Rotate the api_key quarterly; the password policy requires 16 characters.",
		"Fetch from https://github.com/example/repo.git over https.",
	}
	for _, text := range harmless {
		if _, err := s.AddEntry(newEntry(text, episode)); err != nil {
			t.Errorf("harmless text %q was refused: %v", text, err)
		}
	}
}
