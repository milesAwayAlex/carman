// Package memory is the storage layer of the carman memory service.
//
// Memory is a library of markdown notes in a git repository (see files.go).
// Notes are written once and never changed, which carries requirement M2:
// episodes are append-only, and an entry changes only when a new entry
// supersedes it. The Store keeps everything in memory, built from the files
// when it opens, and has no method that edits or deletes a note.
package memory

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound means no episode or entry has the given ID.
	ErrNotFound = errors.New("memory: not found")
	// ErrNotCurrent means the entry has already been superseded.
	ErrNotCurrent = errors.New("memory: entry is not current")
	// ErrNoSource means an entry was offered without a valid source.
	ErrNoSource = errors.New("memory: an entry must cite at least one episode or named human")
	// ErrInvalid means a record was offered with a missing or unknown field.
	ErrInvalid = errors.New("memory: invalid record")
	// ErrSecret means a write looked like it contained a credential.
	ErrSecret = errors.New("memory: refused, secrets are not stored")
	// ErrLessTrusted means a note backed only by an agent's own observation
	// was offered in place of an entry that a person or a verified task backs.
	ErrLessTrusted = errors.New("memory: an unverified note cannot replace a trusted entry")
	// ErrRunFull means a consolidation run has reached its cap on changes.
	ErrRunFull = errors.New("memory: the run has reached its cap on changes")
	// ErrSimilar means memory already holds an entry close to the one offered.
	// The error is a *SimilarError, which carries those entries.
	ErrSimilar = errors.New("memory: a close entry already exists")
)

// SimilarError is returned in place of storing a note when memory already
// holds entries close to it, so the caller can say whether the note replaces
// one of them or stands beside them.
type SimilarError struct {
	Entries []Entry
}

func (e *SimilarError) Error() string {
	ids := make([]string, len(e.Entries))
	for i, entry := range e.Entries {
		ids[i] = entry.ID
	}
	return fmt.Sprintf("%v: %s", ErrSimilar, strings.Join(ids, ", "))
}

func (e *SimilarError) Unwrap() error { return ErrSimilar }

type Origin string

const (
	OriginWorker Origin = "worker"
	OriginLaptop Origin = "laptop"
)

type Outcome string

const (
	OutcomePassed     Outcome = "passed"
	OutcomeFailed     Outcome = "failed"
	OutcomeUnverified Outcome = "unverified"
)

type Kind string

const (
	KindFact       Kind = "fact"
	KindProcedure  Kind = "procedure"
	KindPitfall    Kind = "pitfall"
	KindPreference Kind = "preference"
)

type Author string

const (
	AuthorDreamer Author = "dreamer"
	AuthorAgent   Author = "agent"
	AuthorHuman   Author = "human"
)

type Status string

const (
	StatusUnverified Status = "unverified"
	StatusActive     Status = "active"
)

// NewEpisode is the raw record of one task, as offered by a caller.
type NewEpisode struct {
	Origin    Origin   `json:"origin"`
	TaskID    string   `json:"task_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Scope     []string `json:"scope"`
	Body      string   `json:"body"`
	Outcome   Outcome  `json:"outcome"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
}

// Episode is a stored episode. It never changes after it is written.
type Episode struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	NewEpisode
}

// Source says where an entry came from. Exactly one of EpisodeID and Human is
// set. Outcome is the cited episode's verification outcome; the store fills it
// in and ignores any value a caller supplies.
type Source struct {
	EpisodeID string  `json:"episode_id,omitempty"`
	Human     string  `json:"human,omitempty"`
	Outcome   Outcome `json:"outcome,omitempty"`
}

// NewEntry is a curated item, as offered by a caller.
type NewEntry struct {
	CreatedBy Author   `json:"created_by"`
	Kind      Kind     `json:"kind"`
	Text      string   `json:"text"`
	Scope     []string `json:"scope"`
	Sources   []Source `json:"sources"`
}

// Entry is a stored entry. Its note never changes. ValidTo, Status, Flags and
// Contradictions are worked out from other notes each time the entry is read:
//
//   - ValidTo is when the entry stopped being current: a later entry
//     superseded or absorbed it, it was retired, or the consolidation run
//     that wrote it was reverted.
//   - Status follows the sources (requirement M3): active when a named human
//     or an episode that passed verification backs the entry, unverified when
//     only failed or unverified episodes do.
//   - Flags are the reports that it is wrong; Contradictions counts them.
type Entry struct {
	ID             string     `json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	ValidFrom      time.Time  `json:"valid_from"`
	ValidTo        *time.Time `json:"valid_to,omitempty"`
	Supersedes     *string    `json:"supersedes,omitempty"`
	Absorbs        []string   `json:"absorbs,omitempty"` // further entries merged into this one
	Run            string     `json:"run,omitempty"`     // the consolidation run that wrote it
	Status         Status     `json:"status"`
	Contradictions int        `json:"contradictions"`
	Flags          []Flag     `json:"flags,omitempty"`
	// TextLength is the length of the whole text, in characters, when Text
	// holds only part of it; zero when Text is whole.
	TextLength int `json:"text_length,omitempty"`
	NewEntry
}

// Part returns the entry with at most limit characters of its text, starting
// at from. An entry can be a long working document; a list of entries, or a
// search result, must not put all of one into an agent's context. When text
// is left out, a last line says how much and how to read it.
func (e Entry) Part(from, limit int) Entry {
	runes := []rune(e.Text)
	if from <= 0 && len(runes) <= limit {
		return e
	}
	from = max(0, min(from, len(runes)))
	end := min(from+limit, len(runes))
	e.TextLength = len(runes)
	e.Text = string(runes[from:end])
	if end < len(runes) {
		e.Text += fmt.Sprintf("\n… [characters %d to %d of %d; `cmem get %s --from %d` continues]", from+1, end, len(runes), e.ID, end)
	}
	return e
}

// Flag is one report that an entry is wrong or out of date.
type Flag struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Origin    Origin    `json:"origin"`
	Reason    string    `json:"reason"`
}

// Run is one pass of the consolidation role. Everything it changes carries
// its ID, so the pass can be read back and undone as a unit (requirement M4).
type Run struct {
	ID         string     `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	Purpose    string     `json:"purpose"`
	Changes    []Change   `json:"changes"`
	Size       int        `json:"size"` // entries added, replaced or retired, against Cap
	Cap        int        `json:"cap"`
	RevertedAt *time.Time `json:"reverted_at,omitempty"`
	Reverted   string     `json:"reverted_because,omitempty"`
}

// Change is one thing a run did.
type Change struct {
	At       time.Time `json:"at"`
	Action   string    `json:"action"`             // add, supersede or retire
	Entry    string    `json:"entry"`              // the entry added, or the one retired
	Replaced []string  `json:"replaced,omitempty"` // entries the new one took the place of
	Text     string    `json:"text"`               // the new entry's text, or the reason for retiring
}

// DefaultRunCap is how many entries one consolidation run may add, replace or
// retire. A run that wants more is doing too much to review.
const DefaultRunCap = 20

// Curation is one change by the consolidation role: a new entry, which may
// take the place of existing ones.
type Curation struct {
	Run        string
	Entry      NewEntry
	Supersedes string   // the entry this one replaces
	Absorbs    []string // further entries merged into it; needs Supersedes
	// Global makes the new entry apply everywhere. Without it, a new entry
	// that names no scope keeps the scope of the entries it replaces, and is
	// refused when those differ: widening or narrowing who sees an entry is
	// a decision, not a side effect of leaving a flag out.
	Global bool
	// From names entries whose sources the new entry inherits. The store
	// copies them, so a merged or reworded entry keeps what backed the
	// originals without the caller being able to claim a source of its own.
	From []string
}

// RecallQuery selects entries. Every field is optional.
type RecallQuery struct {
	Text  string    // words to search for; empty lists the newest entries
	Scope []string  // keep entries sharing at least one of these scope tags
	Kinds []Kind    // keep entries of these kinds
	AsOf  time.Time // what was current at this time; zero means now
	Limit int       // zero means DefaultRecallLimit
}

const DefaultRecallLimit = 10

// FlashbackQuery asks for entries worth volunteering at some moment of a
// session, given the text of that moment (a prompt, an error message).
type FlashbackQuery struct {
	SessionID  string   // entries already shown to this session are not repeated
	Event      string   // what prompted it, for the log
	Text       string   // the prompt or error text
	Scope      []string // the session's scope tags; entries with other tags are left out
	Names      []string // words that alone make an entry close enough, such as the command about to run
	Limit      int      // zero means DefaultFlashbackLimit
	MinMatched int      // distinct word stems that must be shared; zero means 2
	MinShare   float64  // how much of the entry or of the text must be covered; zero means 0.4
}

const DefaultFlashbackLimit = 3

// How much of a moment's text flashback matches, and the least share of its
// words an entry must account for. Seen in use: a shell command carrying a
// long script brought up three unrelated preferences, each of whose few
// opening words happened to occur somewhere in it.
const (
	momentLength = 1200
	momentShare  = 0.05
)

// IndexQuery asks for the compact list shown at the start of a session.
type IndexQuery struct {
	SessionID string
	Scope     []string
	Budget    int // roughly how many characters of entry text to return; zero means DefaultIndexBudget
}

// DefaultIndexBudget is about what Claude Code's built-in memory loads into
// every session (25 KB), so the two hold a similar number of lessons before
// anything is left out.
const DefaultIndexBudget = 24000

// IndexLineLimit is how much of an entry's text a list shows. The rest is one
// `cmem get` away, so a long entry costs the index no more than a short one.
const IndexLineLimit = 240

// NewNote is something an agent learned in the middle of a task, as offered
// by a caller.
type NewNote struct {
	Entry NewEntry
	// Evidence is what the agent observed. It is kept as an unverified
	// episode which the entry cites, so the claim and what prompted it stay
	// together. Nil when the entry rests on its other sources alone.
	Evidence *NewEpisode
	// Replaces names the current entry this note corrects or updates. A
	// replacement that names no scope keeps the scope of what it replaces.
	Replaces string
	// Global makes a replacement apply everywhere, whatever it replaces did.
	Global bool
	// Separate stores the note even though memory holds something close.
	Separate bool
}

// retirement is the record that an entry was withdrawn with nothing in its place.
type retirement struct {
	at  time.Time
	run string
}

type Store struct {
	lib library

	mu           sync.RWMutex
	episodes     map[string]Episode
	entries      map[string]Entry    // as written; view fills in the derived fields
	supersededBy map[string][]string // entry ID -> the entries written in its place, oldest first
	retirements  map[string][]retirement
	flags        map[string][]Flag
	runs         map[string]*Run
	// RunCap is the most entries one consolidation run may change.
	RunCap   int
	loaded   []string // the note files read at Open, for Recover
	corpus   *corpus
	shown    map[string]map[string]bool // session ID -> entries already put in front of it
	last     time.Time
	problems []string
}

// Open loads the library in dir, creating it if it does not exist.
func Open(dir string) (*Store, error) {
	lib, err := openLibrary(dir)
	if err != nil {
		return nil, fmt.Errorf("memory: open %s: %w", dir, err)
	}
	s := &Store{
		lib: lib, episodes: map[string]Episode{}, entries: map[string]Entry{},
		supersededBy: map[string][]string{}, retirements: map[string][]retirement{}, flags: map[string][]Flag{},
		runs: map[string]*Run{}, RunCap: DefaultRunCap, corpus: newCorpus(),
		shown: map[string]map[string]bool{},
	}
	notes, problems, err := lib.load()
	if err != nil {
		return nil, fmt.Errorf("memory: load %s: %w", dir, err)
	}
	s.problems = problems
	for _, n := range notes {
		s.loaded = append(s.loaded, n.path())
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Created.Before(notes[j].Created) })
	for _, n := range notes {
		s.apply(n)
	}
	s.loadShown()
	return s, nil
}

// Recover puts right what a stop in the middle of a write leaves behind: the
// lock files with which git would refuse every later write, and notes that
// are whole on disk but in no commit, which it commits. It returns how many
// notes it committed.
//
// Only the service calls this, once, as it starts and before it serves. It is
// the library's only writer, so nothing it clears can be in use. A program
// that opens the library beside a running service (cmem check) must not.
func (s *Store) Recover() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lib.clearStaleLocks()
	valid := map[string]bool{}
	for _, path := range s.loaded {
		valid[path] = true
	}
	n, err := s.lib.recover(valid)
	if err != nil {
		return 0, fmt.Errorf("memory: recover: %w", err)
	}
	return n, nil
}

// Close is here for callers that manage a Store's lifetime; nothing is held open.
func (s *Store) Close() {}

// apply adds one note to the in-memory state. Loading and writing share it.
func (s *Store) apply(n note) {
	switch n.Type {
	case typeEpisode:
		s.episodes[n.ID] = Episode{ID: n.ID, CreatedAt: n.Created, NewEpisode: NewEpisode{
			Origin: n.Origin, TaskID: n.TaskID, SessionID: n.SessionID, Scope: nonNil(n.Scope),
			Body: n.body, Outcome: n.Outcome, CostUSD: n.CostUSD,
		}}
	case typeEntry:
		e := Entry{ID: n.ID, CreatedAt: n.Created, ValidFrom: n.Created, NewEntry: NewEntry{
			CreatedBy: n.CreatedBy, Kind: n.Kind, Text: n.body, Scope: nonNil(n.Scope),
		}}
		for _, src := range n.Sources {
			e.Sources = append(e.Sources, Source{EpisodeID: src.Episode, Human: src.Human})
		}
		e.Run, e.Absorbs = n.Run, n.Absorbs
		var replaced []string
		if old := n.Supersedes; old != "" {
			e.Supersedes = &old
			replaced = append(replaced, old)
		}
		replaced = append(replaced, n.Absorbs...)
		for _, old := range replaced {
			if first, taken := s.replacement(old, n.Created); taken {
				s.problems = append(s.problems, fmt.Sprintf("entry %s is superseded by both %s and %s; the first stands", old, first, n.ID))
			}
			s.supersededBy[old] = append(s.supersededBy[old], n.ID)
		}
		s.entries[n.ID] = e
		s.corpus.add(n.ID, n.body)
		action := "add"
		if len(replaced) > 0 {
			action = "supersede"
		}
		s.logChange(n.Run, Change{At: n.Created, Action: action, Entry: n.ID, Replaced: replaced, Text: n.body}, max(1, len(replaced)))
	case typeFlag:
		s.flags[n.Entry] = append(s.flags[n.Entry], Flag{ID: n.ID, CreatedAt: n.Created, Origin: n.Origin, Reason: n.body})
	case typeRetirement:
		s.retirements[n.Entry] = append(s.retirements[n.Entry], retirement{at: n.Created, run: n.Run})
		s.logChange(n.Run, Change{At: n.Created, Action: "retire", Entry: n.Entry, Text: n.body}, 1)
	case typeRun:
		s.runs[n.ID] = &Run{ID: n.ID, CreatedAt: n.Created, Purpose: n.body, Changes: []Change{}}
	case typeRevert:
		if run := s.runs[n.Run]; run != nil && run.RevertedAt == nil {
			at := n.Created
			run.RevertedAt, run.Reverted = &at, n.body
		}
	}
	if n.Created.After(s.last) {
		s.last = n.Created
	}
}

func (s *Store) logChange(run string, change Change, size int) {
	if r := s.runs[run]; r != nil {
		r.Changes = append(r.Changes, change)
		r.Size += size
	}
}

// inEffect reports whether what a run wrote counted at the given time; a zero
// time means now. Notes outside any run always count.
func (s *Store) inEffect(run string, at time.Time) bool {
	r := s.runs[run]
	return r == nil || r.RevertedAt == nil || (!at.IsZero() && r.RevertedAt.After(at))
}

// replacement returns the entry standing in place of the entry id at the given
// time, if there is one; a zero time means now.
func (s *Store) replacement(id string, at time.Time) (string, bool) {
	for _, by := range s.supersededBy[id] {
		n := s.entries[by]
		if (at.IsZero() || !n.CreatedAt.After(at)) && s.inEffect(n.Run, at) {
			return by, true
		}
	}
	return "", false
}

// ended returns when an entry stopped being current, judged by what had
// happened by the given time; a zero time means now.
func (s *Store) ended(e Entry, at time.Time) (time.Time, bool) {
	var end time.Time
	consider := func(t time.Time) {
		if (at.IsZero() || !t.After(at)) && (end.IsZero() || t.Before(end)) {
			end = t
		}
	}
	if r := s.runs[e.Run]; r != nil && r.RevertedAt != nil {
		consider(*r.RevertedAt)
	}
	if by, ok := s.replacement(e.ID, at); ok {
		consider(s.entries[by].CreatedAt)
	}
	for _, r := range s.retirements[e.ID] {
		if s.inEffect(r.run, at) {
			consider(r.at)
		}
	}
	return end, !end.IsZero()
}

// newNote gives a note its ID and time. Times strictly increase, so the order
// of notes is never ambiguous.
func (s *Store) newNote(noteType string) note {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(s.last) {
		now = s.last.Add(time.Microsecond)
	}
	s.last = now
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	suffix := make([]byte, 4)
	rand.Read(suffix)
	for i, b := range suffix {
		suffix[i] = alphabet[int(b)%len(alphabet)]
	}
	var n note
	n.ID = now.Format("20060102-150405") + "-" + string(suffix)
	n.Type, n.Created = noteType, now
	return n
}

func (s *Store) episodeNote(e NewEpisode) (note, error) {
	switch {
	case strings.TrimSpace(e.Body) == "":
		return note{}, fmt.Errorf("%w: an episode needs a body", ErrInvalid)
	case !slices.Contains([]Origin{OriginWorker, OriginLaptop}, e.Origin):
		return note{}, fmt.Errorf("%w: unknown origin %q", ErrInvalid, e.Origin)
	case !slices.Contains([]Outcome{OutcomePassed, OutcomeFailed, OutcomeUnverified}, e.Outcome):
		return note{}, fmt.Errorf("%w: unknown outcome %q", ErrInvalid, e.Outcome)
	}
	if err := checkSecrets(append([]string{e.Body, e.TaskID, e.SessionID}, e.Scope...)...); err != nil {
		return note{}, err
	}
	n := s.newNote(typeEpisode)
	n.Origin, n.Outcome, n.TaskID, n.SessionID = e.Origin, e.Outcome, e.TaskID, e.SessionID
	n.Scope, n.CostUSD, n.body = e.Scope, e.CostUSD, strings.TrimSpace(e.Body)
	return n, nil
}

// entryNote checks an offered entry and turns it into a note. Its sources must
// exist, either already in the library or among the notes being written with it.
func (s *Store) entryNote(e NewEntry, supersedes string, with ...note) (note, error) {
	switch {
	case strings.TrimSpace(e.Text) == "":
		return note{}, fmt.Errorf("%w: an entry needs text", ErrInvalid)
	case !slices.Contains([]Kind{KindFact, KindProcedure, KindPitfall, KindPreference}, e.Kind):
		return note{}, fmt.Errorf("%w: unknown kind %q", ErrInvalid, e.Kind)
	case !slices.Contains([]Author{AuthorDreamer, AuthorAgent, AuthorHuman}, e.CreatedBy):
		return note{}, fmt.Errorf("%w: unknown author %q", ErrInvalid, e.CreatedBy)
	case len(e.Sources) == 0:
		return note{}, ErrNoSource
	}
	// A text that ends in the line Part adds was copied from a list or a
	// search result, and is only the beginning of an entry.
	if strings.Contains(e.Text, "\n… [characters ") && strings.Contains(e.Text, "` continues]") {
		return note{}, fmt.Errorf("%w: this text is part of an entry, cut where a list showed it; read the whole with cmem get first", ErrInvalid)
	}
	fields := append([]string{e.Text}, e.Scope...)
	var sources []sourceHeader
	for _, src := range e.Sources {
		if (src.EpisodeID == "") == (src.Human == "") {
			return note{}, fmt.Errorf("%w: a source names either an episode or a human", ErrNoSource)
		}
		if src.EpisodeID != "" {
			_, stored := s.episodes[src.EpisodeID]
			pending := slices.ContainsFunc(with, func(n note) bool { return n.Type == typeEpisode && n.ID == src.EpisodeID })
			if !stored && !pending {
				return note{}, fmt.Errorf("source episode %q: %w", src.EpisodeID, ErrNotFound)
			}
		}
		fields = append(fields, src.Human)
		sources = append(sources, sourceHeader{Episode: src.EpisodeID, Human: src.Human})
	}
	if err := checkSecrets(fields...); err != nil {
		return note{}, err
	}
	n := s.newNote(typeEntry)
	n.Kind, n.CreatedBy, n.Scope, n.Supersedes, n.Sources = e.Kind, e.CreatedBy, e.Scope, supersedes, sources
	n.body = strings.TrimSpace(e.Text)
	return n, nil
}

// write stores the notes in one commit and then adds them to memory.
func (s *Store) write(message string, notes ...note) error {
	if err := s.lib.write(message, notes...); err != nil {
		return err
	}
	for _, n := range notes {
		s.apply(n)
	}
	return nil
}

func summary(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if runes := []rune(line); len(runes) > 60 {
		line = string(runes[:60]) + "…"
	}
	return line
}

// AppendEpisode stores a new episode.
func (s *Store) AppendEpisode(e NewEpisode) (Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.episodeNote(e)
	if err == nil {
		err = s.write(fmt.Sprintf("Record %s episode from %s: %s", n.Outcome, n.Origin, summary(n.body)), n)
	}
	if err != nil {
		return Episode{}, fmt.Errorf("memory: append episode: %w", err)
	}
	return s.episodes[n.ID], nil
}

// Episode returns the episode with the given ID.
func (s *Store) Episode(id string) (Episode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.episodes[id]
	if !ok {
		return Episode{}, fmt.Errorf("memory: episode %q: %w", id, ErrNotFound)
	}
	return e, nil
}

// AddEntry stores a new entry that supersedes nothing.
func (s *Store) AddEntry(e NewEntry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.entryNote(e, "")
	if err == nil {
		err = s.write(fmt.Sprintf("Add %s: %s", n.Kind, summary(n.body)), n)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("memory: add entry: %w", err)
	}
	return s.view(s.entries[n.ID]), nil
}

// Note stores something an agent observed in the middle of a task, before the
// task has an episode of its own. The evidence is kept as an unverified
// episode and the entry cites it. The entry may cite further sources as well.
func (s *Store) Note(evidence NewEpisode, e NewEntry) (Entry, error) {
	return s.Remember(NewNote{Entry: e, Evidence: &evidence, Separate: true})
}

// Remember stores a note from a working agent. Two rules keep such notes from
// degrading memory as it grows:
//
//   - A note close to an entry memory already holds is not stored until the
//     caller says which it is: a replacement for that entry, or a separate
//     thing. Otherwise every correction would leave its predecessor current.
//   - A note may replace an entry only if it is at least as well backed. An
//     agent's own unverified observation cannot displace what a person said or
//     a verified task showed; it can be flagged instead.
func (s *Store) Remember(n NewNote) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := n.Entry
	var with []note
	if n.Evidence != nil {
		evidence := *n.Evidence
		evidence.Outcome = OutcomeUnverified
		episode, err := s.episodeNote(evidence)
		if err != nil {
			return Entry{}, fmt.Errorf("memory: note: %w", err)
		}
		e.Sources = append(slices.Clone(e.Sources), Source{EpisodeID: episode.ID})
		with = append(with, episode)
	}
	message := "Note"
	switch {
	case n.Replaces != "":
		old, ok := s.entries[n.Replaces]
		if !ok {
			return Entry{}, fmt.Errorf("memory: note: entry %q: %w", n.Replaces, ErrNotFound)
		}
		if !s.current(old, time.Time{}) {
			return Entry{}, fmt.Errorf("memory: note: replace %s: %w", n.Replaces, ErrNotCurrent)
		}
		if s.view(old).Status == StatusActive && s.view(Entry{NewEntry: e}).Status != StatusActive {
			return Entry{}, fmt.Errorf("memory: note: replace %s: %w", n.Replaces, ErrLessTrusted)
		}
		switch {
		case n.Global:
			e.Scope = nil
		case len(e.Scope) == 0:
			e.Scope = slices.Clone(old.Scope)
		}
		message = "Replace " + n.Replaces + " with"
	case !n.Separate:
		if near := s.similar(e.Text, e.Scope); len(near) > 0 {
			return Entry{}, &SimilarError{Entries: near}
		}
	}
	entry, err := s.entryNote(e, n.Replaces, with...)
	if err == nil {
		err = s.write(fmt.Sprintf("%s %s: %s", message, entry.Kind, summary(entry.body)), append(with, entry)...)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("memory: note: %w", err)
	}
	return s.view(s.entries[entry.ID]), nil
}

// A new note counts as close to an existing entry when they share most of the
// shorter one's word stems (at least 3, covering 60% of it), or a good part of
// a longer one's (at least 5, covering 40%). The second rule is for a lesson
// rewritten in other words around the same names. Both bars are above
// flashback's, because asking an agent "does this replace that?" about an
// unrelated entry costs a turn. They were set on the lessons agents wrote in
// the transfer benchmark and on the retrieval set, which is a small sample.
func closeEnough(matched int, share float64) bool {
	return (matched >= 3 && share >= 0.6) || (matched >= 5 && share >= 0.4)
}

const similarLimit = 3

// similar returns the current entries a new note with this text is close to,
// closest first.
func (s *Store) similar(text string, scope []string) []Entry {
	// Opening against opening. A long document offered as a new note holds
	// most of the words of every existing entry's opening, and would look
	// close to all of them if its whole text were the measure.
	query := newDocument(text).head
	type hit struct {
		entry Entry
		share float64
	}
	var hits []hit
	for id, e := range s.entries {
		if !s.current(e, time.Time{}) || !applies(e, scope) {
			continue
		}
		if matched, share := s.corpus.overlap(id, query); closeEnough(matched, share) {
			hits = append(hits, hit{e, share})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if a.share != b.share {
			if a.share > b.share {
				return -1
			}
			return 1
		}
		return newestFirst(a.entry, b.entry)
	})
	var out []Entry
	for _, h := range hits[:min(similarLimit, len(hits))] {
		out = append(out, s.view(h.entry))
	}
	return out
}

// Supersede stores a new entry in place of the current entry oldID. The old
// entry is kept: it stops being current at the moment the new one starts.
func (s *Store) Supersede(oldID string, e NewEntry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mustBeCurrent(oldID); err != nil {
		return Entry{}, fmt.Errorf("memory: supersede: %w", err)
	}
	n, err := s.entryNote(e, oldID)
	if err == nil {
		err = s.write(fmt.Sprintf("Supersede %s: %s", oldID, summary(n.body)), n)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("memory: supersede %s: %w", oldID, err)
	}
	return s.view(s.entries[n.ID]), nil
}

func (s *Store) mustBeCurrent(id string) error {
	e, ok := s.entries[id]
	if !ok {
		return fmt.Errorf("entry %q: %w", id, ErrNotFound)
	}
	if !s.current(e, time.Time{}) {
		return fmt.Errorf("entry %s: %w", id, ErrNotCurrent)
	}
	return nil
}

// StartRun opens a consolidation run. The purpose is a line for whoever reads
// the log later.
func (s *Store) StartRun(purpose string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(purpose) == "" {
		return Run{}, fmt.Errorf("memory: start run: %w: say what the run is for", ErrInvalid)
	}
	if err := checkSecrets(purpose); err != nil {
		return Run{}, fmt.Errorf("memory: start run: %w", err)
	}
	n := s.newNote(typeRun)
	n.body = strings.TrimSpace(purpose)
	if err := s.write("Start consolidation run: "+summary(n.body), n); err != nil {
		return Run{}, fmt.Errorf("memory: start run: %w", err)
	}
	return s.runView(s.runs[n.ID]), nil
}

func (s *Store) runView(r *Run) Run {
	out := *r
	out.Changes = slices.Clone(r.Changes)
	out.Cap = s.RunCap
	return out
}

// Run returns one consolidation run with everything it changed.
func (s *Store) Run(id string) (Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.runs[id]
	if r == nil {
		return Run{}, fmt.Errorf("memory: run %q: %w", id, ErrNotFound)
	}
	return s.runView(r), nil
}

// Runs returns every consolidation run, newest first.
func (s *Store) Runs() []Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Run{}
	for _, r := range s.runs {
		out = append(out, s.runView(r))
	}
	slices.SortFunc(out, func(a, b Run) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// room checks that a run exists, has not been reverted, and can take size
// more changes under the cap.
func (s *Store) room(run string, size int) error {
	r := s.runs[run]
	switch {
	case r == nil:
		return fmt.Errorf("run %q: %w", run, ErrNotFound)
	case r.RevertedAt != nil:
		return fmt.Errorf("%w: run %s was reverted; start a new one", ErrInvalid, run)
	case r.Size+size > s.RunCap:
		return fmt.Errorf("%w: run %s has changed %d of at most %d entries", ErrRunFull, run, r.Size, s.RunCap)
	}
	return nil
}

// Curate stores a change by the consolidation role as part of a run: a new
// entry, optionally in place of one or more current entries.
func (s *Store) Curate(c Curation) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(err error) (Entry, error) { return Entry{}, fmt.Errorf("memory: curate: %w", err) }
	if len(c.Absorbs) > 0 && c.Supersedes == "" {
		return fail(fmt.Errorf("%w: to merge entries, supersede one and absorb the rest", ErrInvalid))
	}
	if err := s.room(c.Run, max(1, len(c.Absorbs)+min(1, len(c.Supersedes)))); err != nil {
		return fail(err)
	}
	seen := map[string]bool{}
	for _, old := range append([]string{c.Supersedes}, c.Absorbs...) {
		if old == "" {
			continue
		}
		if seen[old] {
			return fail(fmt.Errorf("%w: entry %s is named twice", ErrInvalid, old))
		}
		seen[old] = true
		if err := s.mustBeCurrent(old); err != nil {
			return fail(err)
		}
	}
	e := c.Entry
	switch {
	case c.Global:
		e.Scope = nil
	case len(e.Scope) == 0 && c.Supersedes != "":
		e.Scope = slices.Clone(s.entries[c.Supersedes].Scope)
		for _, old := range c.Absorbs {
			if !sameTags(s.entries[old].Scope, e.Scope) {
				return fail(fmt.Errorf("%w: the entries being merged have different scopes; name the scope of the merged entry, or say that it is global", ErrInvalid))
			}
		}
	}
	e.Sources = slices.Clone(e.Sources)
	for _, id := range c.From {
		from, ok := s.entries[id]
		if !ok {
			return fail(fmt.Errorf("entry %q: %w", id, ErrNotFound))
		}
		for _, src := range from.Sources {
			src.Outcome = ""
			if !slices.Contains(e.Sources, src) {
				e.Sources = append(e.Sources, src)
			}
		}
	}
	n, err := s.entryNote(e, c.Supersedes)
	if err != nil {
		return fail(err)
	}
	n.Run, n.Absorbs = c.Run, c.Absorbs
	message := fmt.Sprintf("Add %s: %s", n.Kind, summary(n.body))
	if c.Supersedes != "" {
		message = fmt.Sprintf("Supersede %s: %s", strings.Join(append([]string{c.Supersedes}, c.Absorbs...), ", "), summary(n.body))
	}
	if err := s.write(message, n); err != nil {
		return fail(err)
	}
	return s.view(s.entries[n.ID]), nil
}

// Retire withdraws a current entry with nothing in its place, as part of a
// run. The entry's note stays; it stops being current.
func (s *Store) Retire(run, entryID, reason string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(err error) (Entry, error) { return Entry{}, fmt.Errorf("memory: retire: %w", err) }
	if strings.TrimSpace(reason) == "" {
		return fail(fmt.Errorf("%w: say why the entry is retired", ErrInvalid))
	}
	if err := checkSecrets(reason); err != nil {
		return fail(err)
	}
	if err := s.room(run, 1); err != nil {
		return fail(err)
	}
	if err := s.mustBeCurrent(entryID); err != nil {
		return fail(err)
	}
	n := s.newNote(typeRetirement)
	n.Entry, n.Run, n.body = entryID, run, strings.TrimSpace(reason)
	if err := s.write(fmt.Sprintf("Retire %s: %s", entryID, summary(n.body)), n); err != nil {
		return fail(err)
	}
	return s.view(s.entries[entryID]), nil
}

// Revert undoes a whole run: the entries it added stop being current, and the
// entries it replaced or retired are current again unless something outside
// the run has since replaced them. Nothing is deleted; the revert is a note.
func (s *Store) Revert(run, reason string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(err error) (Run, error) { return Run{}, fmt.Errorf("memory: revert: %w", err) }
	r := s.runs[run]
	switch {
	case r == nil:
		return fail(fmt.Errorf("run %q: %w", run, ErrNotFound))
	case r.RevertedAt != nil:
		return fail(fmt.Errorf("%w: run %s is already reverted", ErrInvalid, run))
	case strings.TrimSpace(reason) == "":
		return fail(fmt.Errorf("%w: say why the run is reverted", ErrInvalid))
	}
	if err := checkSecrets(reason); err != nil {
		return fail(err)
	}
	n := s.newNote(typeRevert)
	n.Run, n.body = run, strings.TrimSpace(reason)
	if err := s.write(fmt.Sprintf("Revert run %s: %s", run, summary(n.body)), n); err != nil {
		return fail(err)
	}
	return s.runView(r), nil
}

// Flag records that an agent found an entry wrong or stale. The entry's note
// is not touched; the flag is a note of its own that points at it.
func (s *Store) Flag(entryID string, origin Origin, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[entryID]; !ok {
		return fmt.Errorf("memory: flag: entry %q: %w", entryID, ErrNotFound)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("memory: flag: %w: a flag needs a reason", ErrInvalid)
	}
	if err := checkSecrets(reason); err != nil {
		return fmt.Errorf("memory: flag: %w", err)
	}
	n := s.newNote(typeFlag)
	n.Entry, n.Origin, n.body = entryID, origin, strings.TrimSpace(reason)
	if err := s.write(fmt.Sprintf("Flag %s: %s", entryID, summary(n.body)), n); err != nil {
		return fmt.Errorf("memory: flag: %w", err)
	}
	return nil
}

// Entry returns the entry with the given ID, whether or not it is current.
func (s *Store) Entry(id string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return Entry{}, fmt.Errorf("memory: entry %q: %w", id, ErrNotFound)
	}
	return s.view(e), nil
}

// view fills in the fields of an entry that are worked out from other notes.
func (s *Store) view(e Entry) Entry {
	e.Sources = slices.Clone(e.Sources)
	e.Status = StatusUnverified
	for i, src := range e.Sources {
		if src.Human != "" {
			e.Status = StatusActive
			continue
		}
		outcome := s.episodes[src.EpisodeID].Outcome
		e.Sources[i].Outcome = outcome
		if outcome == OutcomePassed {
			e.Status = StatusActive
		}
	}
	if end, over := s.ended(e, time.Time{}); over {
		e.ValidTo = &end
	}
	e.Flags = slices.Clone(s.flags[e.ID])
	e.Contradictions = len(e.Flags)
	return e
}

// current reports whether the entry was current at asOf; a zero asOf means now.
func (s *Store) current(e Entry, asOf time.Time) bool {
	if !asOf.IsZero() && e.CreatedAt.After(asOf) {
		return false
	}
	_, over := s.ended(e, asOf)
	return !over
}

// applies reports whether an entry belongs in a session with the given scope
// tags: it does when it shares a tag, or when either side has none.
func applies(e Entry, scope []string) bool {
	return len(scope) == 0 || len(e.Scope) == 0 || overlaps(e.Scope, scope)
}

// sameTags reports whether two scopes name the same tags, in any order.
func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, tag := range a {
		if !slices.Contains(b, tag) {
			return false
		}
	}
	return true
}

func overlaps(a, b []string) bool {
	return slices.ContainsFunc(a, func(tag string) bool { return slices.Contains(b, tag) })
}

// byTrustThenAge orders entries for a list an agent reads top-down, of which
// it may be shown only the top: trusted before unverified, undisputed before
// flagged, then by kind, then newest first.
//
// Kind comes before age because of what each kind is for. A preference or a
// pitfall changes what an agent does without its knowing to look; a fact or a
// procedure is something it can search for when the task calls for it. When
// the list is cut, the first two are the ones that must not be what is cut.
func byTrustThenAge(a, b Entry) int {
	if a.Status != b.Status {
		if a.Status == StatusActive {
			return -1
		}
		return 1
	}
	if a.Contradictions != b.Contradictions {
		return a.Contradictions - b.Contradictions
	}
	if ra, rb := kindRank[a.Kind], kindRank[b.Kind]; ra != rb {
		return ra - rb
	}
	return newestFirst(a, b)
}

var kindRank = map[Kind]int{KindPreference: 0, KindPitfall: 1, KindProcedure: 2, KindFact: 3}

func newestFirst(a, b Entry) int {
	if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// CurrentEntries returns the entries that were current at asOf, oldest first.
// A zero asOf means now.
func (s *Store) CurrentEntries(asOf time.Time) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.entries {
		if s.current(e, asOf) {
			out = append(out, s.view(e))
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return newestFirst(b, a) })
	return out
}

// Recall returns the entries that best match the query, best first. An entry
// matches when it shares any word stem with the query; ranking (see search.go)
// then rewards entries that share more of them.
func (s *Store) Recall(q RecallQuery) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	query := stems(q.Text)
	type hit struct {
		entry Entry
		score float64
	}
	var hits []hit
	for id, e := range s.entries {
		if !s.current(e, q.AsOf) ||
			(len(q.Scope) > 0 && !overlaps(e.Scope, q.Scope)) ||
			(len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind)) {
			continue
		}
		score := 0.0
		if len(query) > 0 {
			if score = s.corpus.score(id, query); score == 0 {
				continue
			}
		}
		hits = append(hits, hit{e, score})
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return newestFirst(a.entry, b.entry)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultRecallLimit
	}
	out := []Entry{}
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, s.view(h.entry))
	}
	return out
}

// Flashback returns the few entries that are close enough to the text of the
// moment to be worth volunteering, and that this session has not been shown
// yet. Closeness is word overlap (see corpus.overlap); the thresholds keep
// loosely related entries out, because an irrelevant memory costs context and
// can mislead.
func (s *Store) Flashback(q FlashbackQuery) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.Limit <= 0 {
		q.Limit = DefaultFlashbackLimit
	}
	if q.MinMatched <= 0 {
		q.MinMatched = 2
	}
	if q.MinShare <= 0 {
		q.MinShare = 0.4
	}
	// A moment can be long: a reply of several paragraphs, a command with a
	// script in it. A long text contains the few words of many short entries
	// without being about any of them, so only its beginning is matched, and
	// an entry must account for some share of it (momentShare) to count.
	moment := []rune(q.Text)
	if len(moment) > momentLength {
		moment = moment[:momentLength]
	}
	query := stems(string(moment))
	policy := policyFor(q.Event)
	if !policy.words {
		query = nil
	}
	programs := programNames(q.Names)
	type hit struct {
		entry   Entry
		named   bool
		matched int
		share   float64
	}
	var hits []hit
	for id, e := range s.entries {
		if !s.current(e, time.Time{}) || !applies(e, q.Scope) || s.shown[q.SessionID][id] {
			continue
		}
		if !policy.kinds[e.Kind] {
			continue
		}
		// A command such as `kubectl get nodes` shares one word with the
		// lesson about kubectl, which is too few for the overlap rule. An
		// entry whose opening names the program is about that program.
		named := namesProgram(e.Text, programs)
		matched, share := s.corpus.overlap(id, query)
		if len(query) > 0 && float64(matched)/float64(len(query)) < momentShare {
			matched, share = 0, 0
		}
		if named || (matched >= q.MinMatched && share >= q.MinShare) {
			hits = append(hits, hit{e, named, matched, share})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if a.named != b.named {
			if a.named {
				return -1
			}
			return 1
		}
		if a.share != b.share {
			if a.share > b.share {
				return -1
			}
			return 1
		}
		if a.matched != b.matched {
			return b.matched - a.matched
		}
		return newestFirst(a.entry, b.entry)
	})
	out := []Entry{}
	for _, h := range hits[:min(q.Limit, policy.limit, len(hits))] {
		out = append(out, s.view(h.entry))
	}
	s.markShown(q.SessionID, q.Event, out)
	return out
}

// flashbackPolicy is what a kind of moment may bring up. The moments differ
// in what their text is and in what an interruption costs, and one rule for
// all of them was noisy in use: in one long session, 19 entries were brought
// up before shell commands and almost none bore on the command.
type flashbackPolicy struct {
	kinds map[Kind]bool // the kinds of entry worth volunteering at this moment
	words bool          // whether the moment's text is prose to match by its words
	limit int
}

var (
	anyKind = map[Kind]bool{KindFact: true, KindProcedure: true, KindPitfall: true, KindPreference: true}
	// What can change what the agent is about to do, as against what it
	// could look up: a pitfall, a preference, a procedure.
	guidance = map[Kind]bool{KindProcedure: true, KindPitfall: true, KindPreference: true}
)

func policyFor(event string) flashbackPolicy {
	switch event {
	case "pre-tool":
		// A command is not prose: its words are flags, paths and whatever a
		// script in it happens to contain. Only the programs it runs say what
		// it is about, and only guidance about one of them is worth a pause.
		return flashbackPolicy{kinds: guidance, words: false, limit: 2}
	case "proposal":
		// Here a match makes the agent's turn go on for another round. That
		// is worth it for something that might change its answer, and not
		// for a document on the same subject.
		return flashbackPolicy{kinds: guidance, words: true, limit: 2}
	}
	// A prompt, a failure, or a caller that does not say.
	return flashbackPolicy{kinds: anyKind, words: true, limit: DefaultFlashbackLimit}
}

// programNames keeps the names that are plainly a program: kubectl, terraform,
// pnpm. A path or a script name (deploy.sh) is not something an entry would
// be "about" in the same way, and matching its parts matched half the library.
func programNames(names []string) []string {
	var out []string
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if len(name) >= 3 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_-") == "" {
			out = append(out, name)
		}
	}
	return out
}

// namesProgram reports whether the entry's opening paragraph names one of the
// programs, as a word of its own.
func namesProgram(text string, programs []string) bool {
	if len(programs) == 0 {
		return false
	}
	opening, _, _ := strings.Cut(strings.TrimSpace(text), "\n\n")
	if runes := []rune(opening); len(runes) > headLength {
		opening = string(runes[:headLength])
	}
	words := strings.FieldsFunc(strings.ToLower(opening), func(r rune) bool {
		return !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z')
	})
	for _, word := range words {
		if slices.Contains(programs, strings.Trim(word, "-_")) {
			return true
		}
	}
	return false
}

// Index returns the compact list for the start of a session: the current
// entries that apply to its scope, most trustworthy and newest first, cut off
// at a budget. total is how many applied before the cut.
func (s *Store) Index(q IndexQuery) (entries []Entry, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.Budget <= 0 {
		q.Budget = DefaultIndexBudget
	}
	var all []Entry
	for _, e := range s.entries {
		if s.current(e, time.Time{}) && applies(e, q.Scope) {
			all = append(all, s.view(e))
		}
	}
	slices.SortFunc(all, byTrustThenAge)
	entries = []Entry{}
	used := 0
	for _, e := range all {
		// What a list shows of an entry is its first paragraph, up to the
		// line limit; that is what it costs.
		opening, _, _ := strings.Cut(strings.TrimSpace(e.Text), "\n\n")
		used += min(len(opening), IndexLineLimit)
		if used > q.Budget && len(entries) > 0 {
			break
		}
		entries = append(entries, e)
	}
	s.markShown(q.SessionID, "index", entries)
	return entries, len(all)
}

// Unshown returns the current entries that apply to a session's scope and
// have not been put in front of it yet, most trustworthy and newest first.
// It is what a harness hands to a small model to pick from when memory holds
// more than the index showed (ADR 0017). Nothing is marked as shown.
func (s *Store) Unshown(sessionID string, scope []string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Entry{}
	for id, e := range s.entries {
		if s.current(e, time.Time{}) && applies(e, scope) && !s.shown[sessionID][id] {
			out = append(out, s.view(e))
		}
	}
	slices.SortFunc(out, byTrustThenAge)
	return out
}

// Show returns the named entries that are current, in the order given, and
// records that the session has now seen them. Unknown and stale IDs are
// skipped: the caller is passing on a model's choice.
func (s *Store) Show(sessionID, event string, ids []string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Entry{}
	seen := map[string]bool{}
	for _, id := range ids {
		if e, ok := s.entries[id]; ok && !seen[id] && s.current(e, time.Time{}) {
			seen[id] = true
			out = append(out, s.view(e))
		}
	}
	s.markShown(sessionID, event, out)
	return out
}

// shownLog records which entries were put in front of which session. It is a
// log that can be thrown away: it stops repeats within a session and shows
// later what was surfaced. It lives outside the library proper, in .cmem.
func (s *Store) shownLog() string { return filepath.Join(s.lib.dir, ".cmem", "shown.jsonl") }

type shownRecord struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	Event   string    `json:"event"`
	Entry   string    `json:"entry"`
}

func (s *Store) markShown(session, event string, entries []Entry) {
	if session == "" || len(entries) == 0 {
		return
	}
	if s.shown[session] == nil {
		s.shown[session] = map[string]bool{}
	}
	f, err := os.OpenFile(s.shownLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		defer f.Close()
	}
	for _, e := range entries {
		s.shown[session][e.ID] = true
		if f != nil {
			json.NewEncoder(f).Encode(shownRecord{time.Now().UTC(), session, event, e.ID})
		}
	}
}

func (s *Store) loadShown() {
	data, err := os.ReadFile(s.shownLog())
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		var r shownRecord
		if json.Unmarshal([]byte(line), &r) == nil && r.Session != "" {
			if s.shown[r.Session] == nil {
				s.shown[r.Session] = map[string]bool{}
			}
			s.shown[r.Session][r.Entry] = true
		}
	}
}

// Check reports anything wrong with the library: notes that could not be
// read, and notes that were changed or removed after they were written.
func (s *Store) Check() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	changed, err := s.lib.check()
	if err != nil {
		return nil, fmt.Errorf("memory: check: %w", err)
	}
	return append(slices.Clone(s.problems), changed...), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
