// Package server exposes the memory store over HTTP.
//
// Every caller presents a bearer token that maps to a role, and each route
// lists the roles allowed to use it. There is no route that edits or deletes a
// record (requirement M2). A working agent adds entries as notes, and may
// replace an entry with a note that is at least as well backed (ADR 0017).
// Adding or superseding entries without those limits is for the consolidation
// role alone, which can do nothing else but retire one. Every change it makes
// belongs to a run, which is logged, capped and can be reverted as a unit
// (requirement M4).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"carman/internal/memory"
)

type Role string

const (
	RoleLaptop  Role = "laptop"  // the owner's interactive sessions
	RoleWorker  Role = "worker"  // unattended task agents
	RoleDreamer Role = "dreamer" // the consolidation agent
)

var (
	agents   = []Role{RoleLaptop, RoleWorker}
	everyone = []Role{RoleLaptop, RoleWorker, RoleDreamer}
	dreamer  = []Role{RoleDreamer}
)

// ParseTokens reads "role=token,role=token". A role may appear more than once.
func ParseTokens(s string) (map[string]Role, error) {
	tokens := map[string]Role{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		role, token, ok := strings.Cut(pair, "=")
		if !ok || token == "" || !slices.Contains(everyone, Role(role)) {
			return nil, fmt.Errorf("token entries look like role=token with role one of laptop, worker, dreamer; got %q", role)
		}
		tokens[token] = Role(role)
	}
	if len(tokens) == 0 {
		return nil, errors.New("no tokens configured")
	}
	return tokens, nil
}

type Server struct {
	store  *memory.Store
	tokens map[string]Role
	log    *slog.Logger
}

func New(store *memory.Store, tokens map[string]Role, log *slog.Logger) *Server {
	return &Server{store: store, tokens: tokens, log: log}
}

// Route is one operation of the interface.
type Route struct {
	Method  string
	Pattern string
	Roles   []Role
	Creates bool // the operation adds a note to the library
	handle  func(r *http.Request, role Role) (any, error)
}

// Routes lists every operation the interface offers.
func (s *Server) Routes() []Route {
	return []Route{
		{http.MethodPost, "/v1/episodes", agents, true, s.recordEpisode},
		{http.MethodGet, "/v1/episodes/{id}", everyone, false, s.getEpisode},
		{http.MethodPost, "/v1/notes", agents, true, s.note},
		{http.MethodGet, "/v1/recall", everyone, false, s.recall},
		{http.MethodGet, "/v1/entries/{id}", everyone, false, s.getEntry},
		{http.MethodPost, "/v1/entries/{id}/flag", agents, true, s.flag},
		{http.MethodGet, "/v1/entries", everyone, false, s.listEntries},
		{http.MethodPost, "/v1/entries", dreamer, true, s.addEntry},
		{http.MethodPost, "/v1/entries/{id}/supersede", dreamer, true, s.supersede},
		{http.MethodPost, "/v1/entries/{id}/retire", dreamer, true, s.retire},
		{http.MethodPost, "/v1/runs", dreamer, true, s.startRun},
		{http.MethodGet, "/v1/runs", everyone, false, s.listRuns},
		{http.MethodGet, "/v1/runs/{id}", everyone, false, s.getRun},
		{http.MethodPost, "/v1/runs/{id}/revert", dreamer, true, s.revertRun},
		// These two read, but also log what they showed so that a session is
		// not shown the same entry twice; hence POST.
		{http.MethodPost, "/v1/index", agents, false, s.index},
		{http.MethodPost, "/v1/flashback", agents, false, s.flashback},
		{http.MethodPost, "/v1/brief", agents, false, s.brief},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, route := range s.Routes() {
		mux.HandleFunc(route.Method+" "+route.Pattern, s.serve(route))
	}
	return mux
}

var (
	errUnauthorized = errors.New("missing or unknown token")
	errForbidden    = errors.New("this role may not do that")
	errBadRequest   = errors.New("bad request")
)

func (s *Server) serve(route Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		role, err := s.authenticate(r)
		if err == nil && !slices.Contains(route.Roles, role) {
			err = fmt.Errorf("%w: %s %s is for %v", errForbidden, route.Method, route.Pattern, route.Roles)
		}
		var body any
		if err == nil {
			body, err = route.handle(r, role)
		}
		status := http.StatusOK
		if err != nil {
			status = statusFor(err)
			message := err.Error()
			switch {
			case status == http.StatusInternalServerError:
				s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "role", role, "error", err)
				message = "internal error"
			case errors.Is(err, memory.ErrSecret):
				// Requirement M6: the refusal is logged, the secret is not.
				// The error names only the kind of credential it resembled.
				s.log.Warn("write refused", "method", r.Method, "path", r.URL.Path, "role", role, "reason", message)
			}
			failure := map[string]any{"error": message}
			var similar *memory.SimilarError
			if errors.As(err, &similar) {
				failure["similar"] = parts(similar.Entries, listedText)
			}
			body = failure
		} else if route.Creates {
			status = http.StatusCreated
		}
		// One line per request, with no bodies or query text, so the log
		// shows how memory is being used without holding any of its content.
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "role", role, "status", status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, errUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, errForbidden):
		return http.StatusForbidden
	case errors.Is(err, memory.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, memory.ErrNotCurrent), errors.Is(err, memory.ErrSimilar), errors.Is(err, memory.ErrLessTrusted),
		errors.Is(err, memory.ErrRunFull):
		return http.StatusConflict
	case errors.Is(err, memory.ErrSecret):
		return http.StatusUnprocessableEntity
	case errors.Is(err, memory.ErrInvalid), errors.Is(err, memory.ErrNoSource), errors.Is(err, errBadRequest):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) authenticate(r *http.Request) (Role, error) {
	offered, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || offered == "" {
		return "", errUnauthorized
	}
	var found Role
	for token, role := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(offered)) == 1 {
			found = role
		}
	}
	if found == "" {
		return "", errUnauthorized
	}
	return found, nil
}

// origin is where a record comes from. It is taken from the caller's
// credential, never from the request, so a caller cannot claim another origin.
func origin(role Role) memory.Origin {
	if role == RoleLaptop {
		return memory.OriginLaptop
	}
	return memory.OriginWorker
}

func decode(r *http.Request, into any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return nil
}

// EpisodeRequest is the body of POST /v1/episodes.
type EpisodeRequest struct {
	TaskID    string         `json:"task_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Scope     []string       `json:"scope,omitempty"`
	Body      string         `json:"body"`
	Outcome   memory.Outcome `json:"outcome"`
	CostUSD   *float64       `json:"cost_usd,omitempty"`
}

func (s *Server) recordEpisode(r *http.Request, role Role) (any, error) {
	var req EpisodeRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.store.AppendEpisode(memory.NewEpisode{
		Origin: origin(role), TaskID: req.TaskID, SessionID: req.SessionID,
		Scope: req.Scope, Body: req.Body, Outcome: req.Outcome, CostUSD: req.CostUSD,
	})
}

func (s *Server) getEpisode(r *http.Request, _ Role) (any, error) {
	return s.store.Episode(r.PathValue("id"))
}

// How much of an entry's text each kind of answer carries. An entry can be a
// document of hundreds of thousands of characters; a list or a search result
// that carried all of it would fill the context of the agent that asked. The
// whole text is always available from GET /v1/entries/{id}.
const (
	listedText = 300  // in the index, a flashback, a brief, or a close-entry answer: one line is shown
	foundText  = 700  // in a recall result, or after a flag: enough to tell whether it is the one wanted
	browseText = 600  // in the list of every current entry
	pageText   = 8000 // the most one request for part of an entry returns
)

// Why these are small. An agent's harness keeps only so much of a command's
// output in the conversation (Claude Code: about 30,000 characters) and puts
// the rest in a file, which the agent then has to go and read. Seen in use:
// two `cmem get` and a recall in one command came to 49,500 characters. A
// search is for finding the entry; reading it is a separate, paged step.

func parts(entries []memory.Entry, limit int) []memory.Entry {
	out := make([]memory.Entry, len(entries))
	for i, e := range entries {
		out[i] = e.Part(0, limit)
	}
	return out
}

// getEntry returns one entry whole, or the part of its text that from and
// limit ask for.
func (s *Server) getEntry(r *http.Request, _ Role) (any, error) {
	entry, err := s.store.Entry(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	params := r.URL.Query()
	if params.Get("outline") != "" {
		// A map of the entry: where its parts start and how each begins.
		outline := memory.Outline(entry.Text, 40)
		entry = entry.Part(0, openingText)
		entry.Text, _, _ = strings.Cut(entry.Text, "\n… [characters ")
		entry.Passages = outline
		return entry, nil
	}
	if words := params.Get("find"); words != "" {
		// Not the text, but the places in it that the words point at.
		// The few parts of the text that best match, in the order they come.
		passages := memory.Passages(entry.Text, words, 3, findText)
		entry.TextLength = len([]rune(entry.Text))
		entry.Text = ""
		entry.Passages = passages
		if entry.Passages == nil {
			entry.Passages = []memory.Passage{}
		}
		return entry, nil
	}
	if params.Get("limit") == "" {
		return entry, nil
	}
	from, _ := strconv.Atoi(params.Get("from"))
	limit, err := strconv.Atoi(params.Get("limit"))
	if err != nil || limit < 1 || from < 0 {
		return nil, fmt.Errorf("%w: from and limit count characters and cannot be negative", errBadRequest)
	}
	return entry.Part(from, min(limit, pageText)), nil
}

// NoteRequest is the body of POST /v1/notes. A note needs evidence (what the
// agent observed), a named human, or both.
//
// When memory already holds an entry close to the note, the answer is 409 with
// those entries under "similar" and nothing is stored. The caller sends the
// note again with Replaces set to the entry it corrects, or with Separate.
type NoteRequest struct {
	Kind      memory.Kind `json:"kind"`
	Text      string      `json:"text"`
	Scope     []string    `json:"scope,omitempty"`
	Evidence  string      `json:"evidence,omitempty"`
	Human     string      `json:"human,omitempty"`
	Replaces  string      `json:"replaces,omitempty"`
	Separate  bool        `json:"separate,omitempty"`
	Global    bool        `json:"global,omitempty"` // with Replaces: the replacement applies everywhere
	TaskID    string      `json:"task_id,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
}

func (s *Server) note(r *http.Request, role Role) (any, error) {
	var req NoteRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	entry := memory.NewEntry{CreatedBy: memory.AuthorAgent, Kind: req.Kind, Text: req.Text, Scope: req.Scope}
	if req.Human != "" {
		// Only on the laptop is there a human present to be cited.
		if role != RoleLaptop {
			return nil, fmt.Errorf("%w: only the laptop role may cite a human", errForbidden)
		}
		entry.Sources = []memory.Source{{Human: req.Human}}
	}
	if req.Global && len(req.Scope) > 0 {
		return nil, fmt.Errorf("%w: a note is global or has a scope, not both", errBadRequest)
	}
	note := memory.NewNote{Entry: entry, Replaces: req.Replaces, Separate: req.Separate, Global: req.Global}
	switch {
	case req.Evidence != "":
		note.Evidence = &memory.NewEpisode{
			Origin: origin(role), TaskID: req.TaskID, SessionID: req.SessionID, Scope: req.Scope, Body: req.Evidence,
		}
	case req.Human == "":
		return nil, fmt.Errorf("%w: say what you observed (evidence)", memory.ErrNoSource)
	}
	return s.store.Remember(note)
}

func (s *Server) recall(r *http.Request, _ Role) (any, error) {
	params := r.URL.Query()
	q := memory.RecallQuery{Text: params.Get("q"), Scope: params["scope"]}
	for _, kind := range params["kind"] {
		q.Kinds = append(q.Kinds, memory.Kind(kind))
	}
	if raw := params.Get("as_of"); raw != "" {
		asOf, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: as_of must be an RFC 3339 time: %v", errBadRequest, err)
		}
		q.AsOf = asOf
	}
	if raw := params.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return nil, fmt.Errorf("%w: limit must be between 1 and 100", errBadRequest)
		}
		q.Limit = limit
	}
	// The plain answer, a list of entries, is what clients have always had.
	// With found=1 the answer also says how the search went, and each entry
	// carries its opening and the passage that matched rather than as much
	// text as fits: enough to pick the entry, which is then read with get.
	if params.Get("found") == "" {
		return parts(s.store.Recall(q), foundText), nil
	}
	// The right entry is nearly always among the first few, and the answer
	// says how many more there are.
	if q.Limit == 0 {
		q.Limit = foundCount
	}
	entries, total, near, weak := s.store.Search(q)
	found := Found{Entries: make([]memory.Entry, len(entries)), Total: total, Near: near, Weak: weak}
	for i, e := range entries {
		opening, rest, _ := strings.Cut(strings.TrimSpace(e.Text), "\n\n")
		length := len([]rune(e.Text))
		if len([]rune(opening)) > openingText {
			opening = string([]rune(opening)[:openingText])
		}
		if strings.TrimSpace(rest) != "" || len([]rune(opening)) < length {
			e.TextLength = length
			// Where in the rest of it the question's words are.
			for _, p := range memory.Passages(e.Text, q.Text, 1, passageText) {
				if p.At > len([]rune(opening)) {
					e.Passages = append(e.Passages, p)
				}
			}
		}
		e.Text = opening
		found.Entries[i] = e
	}
	return found, nil
}

// findText is the most characters of one part that `get ?find=` returns: a
// paragraph of ordinary length whole, so that the part often is the answer.
const findText = 1000

// foundCount is how many entries a recall made with found=1 returns when
// the caller names no limit.
const foundCount = 5

// Found is the answer to a recall made with found=1.
type Found struct {
	Entries []memory.Entry `json:"entries"`
	Total   int            `json:"total"` // how many entries matched at all
	Near    int            `json:"near"`  // how many of those match about as well as the best
	Weak    bool           `json:"weak"`  // even the best match shares few of the question's words
}

const (
	openingText = 400 // how much of an entry's first paragraph a search result shows
	passageText = 320 // how long a matched passage is
)

// IndexRequest is the body of POST /v1/index: the list for a session's start.
type IndexRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	Scope     []string `json:"scope,omitempty"`
	Budget    int      `json:"budget,omitempty"`
}

// IndexResponse carries the entries that fit the budget and how many applied.
type IndexResponse struct {
	Entries []memory.Entry `json:"entries"`
	Total   int            `json:"total"`
}

func (s *Server) index(r *http.Request, _ Role) (any, error) {
	var req IndexRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	entries, total := s.store.Index(memory.IndexQuery{SessionID: req.SessionID, Scope: req.Scope, Budget: req.Budget})
	return IndexResponse{parts(entries, listedText), total}, nil
}

// FlashbackRequest is the body of POST /v1/flashback: the text of a moment in
// a session, for which closely matching entries are wanted.
type FlashbackRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	Event     string   `json:"event,omitempty"`
	Text      string   `json:"text"`
	Scope     []string `json:"scope,omitempty"`
	Names     []string `json:"names,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

func (s *Server) flashback(r *http.Request, _ Role) (any, error) {
	var req FlashbackRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return parts(s.store.Flashback(memory.FlashbackQuery{
		SessionID: req.SessionID, Event: req.Event, Text: req.Text, Scope: req.Scope, Names: req.Names, Limit: req.Limit,
	}), listedText), nil
}

// BriefRequest is the body of POST /v1/brief, which serves a harness that has
// a small model choose entries for a task. Without Pick it answers with the
// entries the session has not been shown. With Pick it answers with those
// entries and records them as shown. The service itself calls no model.
type BriefRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	Scope     []string `json:"scope,omitempty"`
	Pick      []string `json:"pick,omitempty"`
	Event     string   `json:"event,omitempty"` // what showed them, for the log; "brief" unless said
}

func (s *Server) brief(r *http.Request, _ Role) (any, error) {
	var req BriefRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Pick != nil {
		event := req.Event
		if event == "" {
			event = "brief"
		}
		return parts(s.store.Show(req.SessionID, event, req.Pick), listedText), nil
	}
	return parts(s.store.Unshown(req.SessionID, req.Scope), listedText), nil
}

// FlagRequest is the body of POST /v1/entries/{id}/flag.
type FlagRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) flag(r *http.Request, role Role) (any, error) {
	var req FlagRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if err := s.store.Flag(id, origin(role), req.Reason); err != nil {
		return nil, err
	}
	entry, err := s.store.Entry(id)
	return entry.Part(0, foundText), err
}

// EntryRequest is the body of the consolidation role's add and supersede.
// Every change names the run it belongs to.
type EntryRequest struct {
	Run     string          `json:"run"`
	Kind    memory.Kind     `json:"kind"`
	Text    string          `json:"text"`
	Scope   []string        `json:"scope,omitempty"`
	Sources []memory.Source `json:"sources,omitempty"` // episodes the entry rests on
	From    []string        `json:"from,omitempty"`    // entries whose sources it inherits
	Absorbs []string        `json:"absorbs,omitempty"` // supersede only: further entries merged into it
	Global  bool            `json:"global,omitempty"`  // supersede only: the new entry applies everywhere
}

func (req EntryRequest) curation(supersedes string) (memory.Curation, error) {
	for _, src := range req.Sources {
		// The consolidation role has no person beside it to cite. An entry it
		// rewrites keeps the person who backed the original through From.
		if src.Human != "" {
			return memory.Curation{}, fmt.Errorf("%w: the consolidation role cites episodes; to keep an entry's human source, name the entry in from", errForbidden)
		}
	}
	if req.Run == "" {
		return memory.Curation{}, fmt.Errorf("%w: name the run this change belongs to", errBadRequest)
	}
	if req.Global && len(req.Scope) > 0 {
		return memory.Curation{}, fmt.Errorf("%w: an entry is global or has a scope, not both", errBadRequest)
	}
	return memory.Curation{
		Run: req.Run, Supersedes: supersedes, Absorbs: req.Absorbs, From: req.From, Global: req.Global,
		Entry: memory.NewEntry{
			CreatedBy: memory.AuthorDreamer, Kind: req.Kind, Text: req.Text, Scope: req.Scope, Sources: req.Sources,
		},
	}, nil
}

func (s *Server) addEntry(r *http.Request, _ Role) (any, error) {
	var req EntryRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if len(req.Absorbs) > 0 {
		return nil, fmt.Errorf("%w: to merge entries, supersede one and absorb the rest", errBadRequest)
	}
	c, err := req.curation("")
	if err != nil {
		return nil, err
	}
	return s.store.Curate(c)
}

func (s *Server) supersede(r *http.Request, _ Role) (any, error) {
	var req EntryRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	c, err := req.curation(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return s.store.Curate(c)
}

// RetireRequest is the body of POST /v1/entries/{id}/retire.
type RetireRequest struct {
	Run    string `json:"run"`
	Reason string `json:"reason"`
}

func (s *Server) retire(r *http.Request, _ Role) (any, error) {
	var req RetireRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.store.Retire(req.Run, r.PathValue("id"), req.Reason)
}

// listEntries returns every current entry, oldest first, with its sources and
// flags: what the consolidation role reads before it changes anything.
func (s *Server) listEntries(_ *http.Request, _ Role) (any, error) {
	return parts(s.store.CurrentEntries(time.Time{}), browseText), nil
}

// RunRequest is the body of POST /v1/runs.
type RunRequest struct {
	Purpose string `json:"purpose"`
}

func (s *Server) startRun(r *http.Request, _ Role) (any, error) {
	var req RunRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.store.StartRun(req.Purpose)
}

func (s *Server) listRuns(_ *http.Request, _ Role) (any, error) { return s.store.Runs(), nil }

func (s *Server) getRun(r *http.Request, _ Role) (any, error) { return s.store.Run(r.PathValue("id")) }

// RevertRequest is the body of POST /v1/runs/{id}/revert.
type RevertRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) revertRun(r *http.Request, _ Role) (any, error) {
	var req RevertRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.store.Revert(r.PathValue("id"), req.Reason)
}
