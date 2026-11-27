// Package api is the dispatcher's HTTP interface and its client. Every
// caller presents a bearer token that maps to a role, and each route lists
// the roles allowed to use it, as the memory service does.
//
// The roles:
//   - owner: everything; submits work, reads the tasks and the log. The
//     service knows its token only by the token's SHA-256 (ADR 0051).
//   - agent: any agent or watcher. It may only emit events, and not of the
//     types the dispatcher and its job watcher write (task.*, job.*,
//     schedule.*): it can say what it saw, not that work started or ended.
//     Its events' IDs and sources are marked as an agent's, so that it
//     cannot take an ID the dispatcher will want.
//   - backup: reads a dump of the database, and nothing else.
//   - meter: the egress proxy's meter (ADR 0050), which emits what agents
//     used of the model as it saw them, and nothing else. It has no token
//     of its own: it is the proxy's pod, by the token the cluster makes
//     for that pod, and only it may emit model.* events.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/improve"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
	"carman/internal/dispatch/trigger"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAgent  Role = "agent"
	RoleBackup Role = "backup"
	// RoleHost is the owner's own machine, whose sessions run outside the
	// cluster (ADR 0049). It may only message.
	RoleHost  Role = "host"
	RoleMeter Role = "meter"
)

var roles = []Role{RoleOwner, RoleAgent, RoleBackup, RoleHost}

// ParseTokens reads "role=token,role=token". A role may appear more than once.
func ParseTokens(s string) (map[string]Role, error) {
	tokens := map[string]Role{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		role, token, ok := strings.Cut(pair, "=")
		if !ok || token == "" || !slices.Contains(roles, Role(role)) {
			return nil, fmt.Errorf("token entries look like role=token with role one of owner, agent, backup, host; got %q", role)
		}
		tokens[token] = Role(role)
	}
	if len(tokens) == 0 {
		return nil, errors.New("no tokens configured")
	}
	return tokens, nil
}

// Store is the dispatcher and the reads the service shows.
type Store interface {
	dispatch.Dispatcher
	Tasks(ctx context.Context, state dispatch.State, limit int) ([]dispatch.TaskStatus, error)
	EventsOf(ctx context.Context, after int64, typ string, limit int) ([]dispatch.Record, error)
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
}

type Server struct {
	Store  Store
	Tokens map[string]Role
	// OwnerSHA256 is the SHA-256 of the owner's token (ADR 0051): the
	// token itself stays on the owner's machine.
	OwnerSHA256 []byte
	// Check says whether a task can be started at all (its class names a
	// template, its payload is allowed); nil accepts every task.
	Check func(context.Context, dispatch.TaskSpec) error
	// Dump writes a dump of the database to the file; nil offers none.
	Dump func(context.Context, *os.File) error
	// Messages is messaging (ADR 0045), and Verify says which agent's pod a
	// token the cluster made for the dispatcher is; without them, there
	// are no messages.
	Messages *message.Service
	Verify   func(context.Context, string) (message.Caller, error)
	// ProxyLimits is each agent's limit at the proxy by its definition
	// (ADR 0064).
	ProxyLimits map[string]meter.Limit
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Improve is the list of improvements to the platform (ADR 0058).
	Improve *improve.Service
	// Outside takes what watches saw and what hooks were told (ADR 0063).
	Outside *trigger.Outside
	Log     *slog.Logger
}

// Reserved are the prefixes of the types only the dispatcher, its job
// watcher and the owner write (installation.*: what a deploy says it
// deployed); and, for model.*, the meter.
var Reserved = []string{"task.", "job.", "schedule.", "installation.", MeterPrefix}

// MeterPrefix is the prefix of the types the meter writes, and the only
// ones it may.
const MeterPrefix = "model."

type route struct {
	method, pattern string
	roles           []Role
	handle          func(w http.ResponseWriter, r *http.Request, role Role) (any, error)
}

func (s *Server) routes() []route {
	owner, emitters, dumpers := []Role{RoleOwner}, []Role{RoleOwner, RoleAgent, RoleMeter}, []Role{RoleOwner, RoleBackup}
	messengers := []Role{RoleOwner, RoleAgent, RoleHost}
	return []route{
		{http.MethodPost, "/v1/events", emitters, s.emit},
		{http.MethodGet, "/v1/events", owner, s.events},
		{http.MethodPost, "/v1/tasks", owner, s.submit},
		{http.MethodGet, "/v1/tasks", owner, s.tasks},
		{http.MethodGet, "/v1/tasks/{id}", owner, s.task},
		{http.MethodGet, "/v1/dump", dumpers, s.dump},
		{http.MethodGet, "/v1/meter/standing", []Role{RoleOwner, RoleMeter}, s.standing},
		{http.MethodGet, "/v1/usage/latest", []Role{RoleOwner, RoleAgent}, s.usageLatest},
		{http.MethodGet, "/v1/deployed", messengers, s.deployed},
		{http.MethodGet, "/v1/whoami", messengers, s.whoami},
		{http.MethodGet, "/v1/peers", messengers, s.peers},
		{http.MethodPost, "/v1/messages", messengers, s.send},
		{http.MethodGet, "/v1/messages/{id}", messengers, s.messageStatus},
		{http.MethodPost, "/v1/messages/{id}/answer", messengers, s.answer},
		{http.MethodGet, "/v1/inbox", messengers, s.inbox},
		{http.MethodPost, "/v1/inbox/take", messengers, s.take},
		{http.MethodPost, "/v1/inbox/read", messengers, s.read},
		{http.MethodPost, "/v1/sessions", messengers, s.presence},
		{http.MethodPost, "/v1/watch", messengers, s.watch},
		{http.MethodPost, "/v1/senders/lift", owner, s.lift},
		{http.MethodGet, "/v1/attend", []Role{RoleAgent}, s.attending},
		{http.MethodPost, "/v1/attend/gone", []Role{RoleAgent}, s.gone},
		{http.MethodPost, "/v1/attend/hold", []Role{RoleAgent}, s.hold},
		{http.MethodPost, "/v1/watches/{name}", []Role{RoleOwner, RoleAgent}, s.observed},
		{http.MethodGet, "/v1/improvements", messengers, s.improvements},
		{http.MethodPost, "/v1/improvements", messengers, s.improvementAdd},
		{http.MethodGet, "/v1/improvements/evidence", messengers, s.evidence},
		{http.MethodPost, "/v1/improvements/survey", messengers, s.surveyed},
		{http.MethodGet, "/v1/improvements/{id}", messengers, s.improvement},
		{http.MethodPost, "/v1/improvements/{id}", messengers, s.improvementChange},
		{http.MethodPost, "/v1/improvements/{id}/brief", messengers, s.improvementBrief},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.method+" "+rt.pattern, s.serve(rt))
	}
	// A hook's caller has no token of ours: it is checked by its signature.
	mux.HandleFunc("POST /v1/hooks/{name}", s.hook)
	return mux
}

var (
	errUnauthorized = errors.New("missing or unknown token")
	errForbidden    = errors.New("this role may not do that")
	errBadRequest   = errors.New("bad request")
	errWritten      = errors.New("the answer was written")
	// errHeld: a rule holds back what was asked, for now (409).
	errHeld = errors.New("held")
)

func (s *Server) serve(rt route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		role, caller, err := s.authenticate(r)
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller))
		if err == nil && !slices.Contains(rt.roles, role) {
			err = fmt.Errorf("%w: %s %s is for %v", errForbidden, rt.method, rt.pattern, rt.roles)
		}
		var body any
		if err == nil {
			body, err = rt.handle(w, r, role)
		}
		if errors.Is(err, errWritten) {
			return
		}
		status := http.StatusOK
		if err != nil {
			status = statusFor(err)
			if status == http.StatusInternalServerError {
				s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "role", role, "error", err)
			}
			body = map[string]string{"error": err.Error()}
		}
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
	case errors.Is(err, dispatch.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, errHeld):
		return http.StatusConflict
	case errors.Is(err, errBadRequest), errors.Is(err, dispatch.ErrInvalid):
		return http.StatusBadRequest
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

func (s *Server) authenticate(r *http.Request) (Role, message.Caller, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", message.Caller{}, errUnauthorized
	}
	if sum := sha256.Sum256([]byte(token)); len(s.OwnerSHA256) == sha256.Size && subtle.ConstantTimeCompare(sum[:], s.OwnerSHA256) == 1 {
		return RoleOwner, message.Caller{Owner: true}, nil
	}
	for known, role := range s.Tokens {
		if subtle.ConstantTimeCompare([]byte(known), []byte(token)) == 1 {
			if role == RoleHost {
				if s.Messages == nil || s.Messages.Place == "" {
					return "", message.Caller{}, fmt.Errorf("%w: this dispatcher names no place for the owner's machine (DISPATCHER_PLACE)", errUnauthorized)
				}
				return role, message.Caller{Host: true, Agent: s.Messages.Place}, nil
			}
			return role, message.Caller{Owner: role == RoleOwner}, nil
		}
	}
	// A pod's own token for the dispatcher: an agent, and which one.
	if s.Verify != nil && strings.Count(token, ".") == 2 {
		c, err := s.Verify(r.Context(), token)
		if err != nil {
			s.Log.Warn("a token was refused", "error", err)
			return "", message.Caller{}, fmt.Errorf("%w: %v", errUnauthorized, err)
		}
		if c.Meter {
			return RoleMeter, c, nil
		}
		return RoleAgent, c, nil
	}
	return "", message.Caller{}, errUnauthorized
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return nil
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// EmitRequest is an event as a caller sends it. ID and Source may be empty.
type EmitRequest struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Subject string          `json:"subject,omitempty"`
	Source  string          `json:"source,omitempty"`
	Time    time.Time       `json:"time,omitzero"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (s *Server) emit(_ http.ResponseWriter, r *http.Request, role Role) (any, error) {
	var req EmitRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Type == "" {
		return nil, fmt.Errorf("%w: an event needs a type", errBadRequest)
	}
	e := dispatch.Event{ID: req.ID, Type: req.Type, Subject: req.Subject, Source: req.Source, Time: req.Time, Data: req.Data}
	if e.ID == "" {
		e.ID = randomID()
	}
	// The meter writes what it saw, and readings of the account's standing
	// from the answers' headers; an agent's limit is the owner's word.
	if role == RoleMeter && (!strings.HasPrefix(e.Type, MeterPrefix) || e.Type == meter.TypeModelLimit) && e.Type != meter.TypeUsageObserved {
		return nil, fmt.Errorf("%w: the meter emits only events of type %s* and %s, and not %s", errForbidden, MeterPrefix, meter.TypeUsageObserved, meter.TypeModelLimit)
	}
	if role != RoleOwner && role != RoleMeter {
		for _, p := range append(slices.Clone(Reserved), trigger.Reserved...) {
			if strings.HasPrefix(e.Type, p) {
				return nil, fmt.Errorf("%w: events of type %s* are not this role's to emit", errForbidden, p)
			}
		}
	}
	if role != RoleOwner {
		prefix := string(role)
		if c := callerOf(r); c.Agent != "" {
			prefix += "/" + c.Agent
		}
		e.ID = prefix + "/" + e.ID
		e.Source = strings.TrimSuffix(prefix+"/"+e.Source, "/")
	} else if e.Source == "" {
		e.Source = string(RoleOwner)
	}
	if err := s.Store.Emit(r.Context(), e); err != nil {
		return nil, err
	}
	return map[string]string{"id": e.ID}, nil
}

// Record is a log entry as the API shows it.
type Record struct {
	Seq int64 `json:"seq"`
	dispatch.Event
}

func intParam(r *http.Request, name string, def, most int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s is a number", errBadRequest, name)
	}
	if most > 0 && n > most {
		n = most
	}
	return n, nil
}

// events is the log: after a sequence number (?after=, ?type=, ?limit=),
// or its last records (?last=, ?type=).
func (s *Server) events(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	typ := r.URL.Query().Get("type")
	limit, err := intParam(r, "limit", 100, 1000)
	if err != nil {
		return nil, err
	}
	var recs []dispatch.Record
	if r.URL.Query().Has("last") {
		last, err := intParam(r, "last", 20, 1000)
		if err != nil {
			return nil, err
		}
		recs, err = s.Store.Last(r.Context(), typ, int(last))
		if err != nil {
			return nil, err
		}
	} else {
		after, err := intParam(r, "after", 0, 0)
		if err != nil {
			return nil, err
		}
		if typ == "" {
			recs, err = s.Store.Events(r.Context(), after, int(limit))
		} else {
			recs, err = s.Store.EventsOf(r.Context(), after, typ, int(limit))
		}
		if err != nil {
			return nil, err
		}
	}
	out := make([]Record, len(recs))
	for i, rec := range recs {
		out[i] = Record{Seq: rec.Seq, Event: rec.Event}
	}
	return out, nil
}

// SubmitRequest is a task as a caller sends it; the timeout is a duration
// such as "1h".
type SubmitRequest struct {
	ID             string          `json:"id"`
	Class          string          `json:"class"`
	Priority       int             `json:"priority,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	StartBy        time.Time       `json:"start_by,omitzero"`
	AttemptTimeout string          `json:"attempt_timeout"`
	// After is a class the task starts after (dispatch.TaskSpec.After);
	// AfterWithin a duration, such as 1h.
	After       string `json:"after,omitempty"`
	AfterWithin string `json:"after_within,omitempty"`
}

// Task is a task's status as the API shows it.
type Task struct {
	ID             string          `json:"id"`
	Class          string          `json:"class"`
	Priority       int             `json:"priority"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	StartBy        time.Time       `json:"start_by,omitzero"`
	AttemptTimeout string          `json:"attempt_timeout"`
	State          dispatch.State  `json:"state"`
	Attempt        int             `json:"attempt"`
	Reason         string          `json:"reason,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	Wait           *dispatch.Wait  `json:"wait,omitempty"`
	After          string          `json:"after,omitempty"`
	AfterWithin    string          `json:"after_within,omitempty"`
}

func view(st dispatch.TaskStatus) Task {
	return Task{
		ID: st.Spec.ID, Class: st.Spec.Class, Priority: st.Spec.Priority, Payload: st.Spec.Payload,
		StartBy: st.Spec.StartBy, AttemptTimeout: st.Spec.AttemptTimeout.String(),
		State: st.State, Attempt: st.Attempt, Reason: string(st.Reason.Kind), Detail: st.Reason.Detail, Wait: st.Reason.Wait,
		After: st.Spec.After, AfterWithin: afterWithin(st.Spec),
	}
}

func afterWithin(t dispatch.TaskSpec) string {
	if t.After == "" {
		return ""
	}
	return t.AfterWithin.String()
}

func (s *Server) submit(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	var req SubmitRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	timeout, err := time.ParseDuration(req.AttemptTimeout)
	if err != nil || timeout <= 0 {
		return nil, fmt.Errorf("%w: attempt_timeout is a positive duration, such as 1h", errBadRequest)
	}
	spec := dispatch.TaskSpec{ID: req.ID, Class: req.Class, Priority: req.Priority, Payload: req.Payload,
		StartBy: req.StartBy, AttemptTimeout: timeout, After: req.After}
	if spec.ID == "" || spec.Class == "" {
		return nil, fmt.Errorf("%w: a task needs an id and a class", errBadRequest)
	}
	if req.After != "" || req.AfterWithin != "" {
		if spec.AfterWithin, err = time.ParseDuration(req.AfterWithin); err != nil || spec.After == "" || spec.AfterWithin <= 0 {
			return nil, fmt.Errorf("%w: after names a class and after_within is a positive duration, such as 1h", errBadRequest)
		}
	}
	if s.Check != nil {
		if err := s.Check(r.Context(), spec); err != nil {
			return nil, err
		}
	}
	if err := s.Store.Submit(r.Context(), spec); err != nil {
		return nil, err
	}
	st, err := s.Store.Explain(r.Context(), spec.ID)
	if err != nil {
		return nil, err
	}
	return view(st), nil
}

func (s *Server) tasks(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	state := dispatch.State(r.URL.Query().Get("state"))
	limit, err := intParam(r, "limit", 100, 1000)
	if err != nil {
		return nil, err
	}
	sts, err := s.Store.Tasks(r.Context(), state, int(limit))
	if err != nil {
		return nil, err
	}
	out := make([]Task, len(sts))
	for i, st := range sts {
		out[i] = view(st)
	}
	return out, nil
}

func (s *Server) task(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	st, err := s.Store.Explain(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return view(st), nil
}

// dump makes the whole dump in a file first, so that the answer has a
// length and a dump that failed half way is never taken for a whole one.
func (s *Server) dump(w http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	if s.Dump == nil {
		return nil, fmt.Errorf("%w: this service makes no dumps", dispatch.ErrNotFound)
	}
	f, err := os.CreateTemp("", "dump-*.sql")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := s.Dump(r.Context(), f); err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, f); err != nil {
		s.Log.Error("sending a dump", "error", err)
	}
	return nil, errWritten
}
