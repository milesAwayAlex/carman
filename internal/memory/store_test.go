package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
