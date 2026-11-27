// Package message is messaging between sessions, agents and the owner
// (ADR 0045). A message is an event in the dispatcher's log, and so is what
// became of it: waiting, delivered into its session (and how), read,
// answered. Whoever it is for gets it whether its session is running, idle
// or not running at all: a session that is not running is started again as
// its agent, through a task, with the message as its next input.
//
// Addresses:
//
//	owner           the owner, through a notice
//	agent/NAME      an agent by its name: a new conversation of that agent
//	session/ID      a session: a conversation, wherever it is or is not running
//
// A sender may also write NAME for agent/NAME, and NAME/X for the session
// of agent NAME that calls itself X, or whose id starts with X.
package message

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"carman/internal/dispatch"
)

const (
	TypeSent      = "message.sent"      // subject the message's id; data a Message
	TypeDelivered = "message.delivered" // ID message.delivered/MSG; data a Delivery
	TypeRead      = "message.read"      // ID message.read/MSG; data a Delivery (who read it)
	TypeWaiting   = "message.waiting"   // ID message.waiting/MSG/KEY; data a Wait
	TypeRefused   = "message.refused"   // a message that could not be addressed; data a Message and why

	TypeSessionStarted = "session.started" // subject the session's id; data a Session
	TypeSessionEnded   = "session.ended"   // subject the session's id; data a Session and why
	TypeWatch          = "session.watch"   // subject the watched session; data who to tell
	TypeWatched        = "session.watched" // ID session.watched/WATCH: the watch was answered

	Source = "messages"
	Owner  = "owner"

	// MaxDepth is how many answers one exchange between sessions may have.
	MaxDepth = 6

	// TaskPrefix starts the ID of every task a message becomes.
	TaskPrefix = "message/"
)

// Types are the event types of messaging, for their retention.
var Types = []string{TypeSent, TypeDelivered, TypeRead, TypeWaiting, TypeRefused,
	TypeSessionStarted, TypeSessionEnded, TypeWatch, TypeWatched}

// Message is what a message.sent event carries.
type Message struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`            // owner, agent/NAME or session/ID
	Agent   string    `json:"agent,omitempty"` // the sender's agent
	Name    string    `json:"name,omitempty"`  // the sending session's name
	Pod     string    `json:"pod,omitempty"`   // where the sender was
	To      string    `json:"to"`              // as resolved
	Given   string    `json:"given,omitempty"` // as the sender wrote it
	Body    string    `json:"body"`
	Answers string    `json:"answers,omitempty"` // the message this answers
	Sent    time.Time `json:"sent"`
	Reason  string    `json:"reason,omitempty"` // for a refused one: why
	Depth   int       `json:"depth,omitempty"`  // how many answers deep it is
	Host    bool      `json:"host,omitempty"`   // sent by a session on the owner's machine (ADR 0049)
}

// Delivery is how a message reached a session, or who read it.
type Delivery struct {
	Session string `json:"session"`
	How     string `json:"how"` // prompt, tool, stop, wake, start, inbox, notice, reply
	Pod     string `json:"pod,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// Wait says why a message waits, and for what.
type Wait struct {
	Reason string `json:"reason"`
	Task   string `json:"task,omitempty"`
}

// Session is where a session is and what it is doing.
type Session struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	Name    string    `json:"name,omitempty"`
	Pod     string    `json:"pod,omitempty"`
	PodUID  string    `json:"pod_uid,omitempty"`
	Kind    string    `json:"kind,omitempty"` // interactive or headless
	State   string    `json:"state"`          // busy, idle or ended
	Cwd     string    `json:"cwd,omitempty"`
	Started time.Time `json:"started,omitzero"`
	Seen    time.Time `json:"seen,omitzero"`
}

const (
	Busy  = "busy"
	Idle  = "idle"
	Ended = "ended"
)

// SessionAddress and AgentAddress are canonical addresses.
func SessionAddress(id string) string { return "session/" + id }
func AgentAddress(name string) string { return "agent/" + name }

// Split is an address's kind (owner, agent, session) and what follows it.
func Split(addr string) (kind, rest string) {
	if addr == Owner {
		return Owner, ""
	}
	k, r, ok := strings.Cut(addr, "/")
	if !ok {
		return "", addr
	}
	return k, r
}

// Store is what messaging needs from the log.
type Store interface {
	Emit(ctx context.Context, e dispatch.Event) error
	Claim(ctx context.Context, e dispatch.Event) (bool, error)
	Unmarked(ctx context.Context, to []string, kind string, limit int) ([]dispatch.Record, error)
	About(ctx context.Context, subject string) ([]dispatch.Record, error)
	Answers(ctx context.Context, id string) ([]dispatch.Record, error)
	OfTypeAbout(ctx context.Context, typ, subject string) ([]dispatch.Record, error)
	PutSession(ctx context.Context, s Session) (Session, bool, error)
	GetSession(ctx context.Context, id string) (Session, bool, error)
	Sessions(ctx context.Context, agent string, since time.Time) ([]Session, error)
	// Correspondents are the session addresses that sent or were sent a
	// message since a time.
	Correspondents(ctx context.Context, since time.Time) ([]string, error)
	ForgetSessions(ctx context.Context, before time.Time) error
	Submit(ctx context.Context, t dispatch.TaskSpec) error
	Explain(ctx context.Context, taskID string) (dispatch.TaskStatus, error)
	EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error)
	Mark(ctx context.Context, name string, v any) (bool, error)
	SetMark(ctx context.Context, name string, v any) error
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
}

// AgentInfo is an agent the cluster knows: the CronJob whose template
// starts a run of it, if it has one.
type AgentInfo struct {
	Name        string `json:"name"`
	Class       string `json:"class,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	// Deadline is how long the agent's Job may run (its template's
	// activeDeadlineSeconds); zero when it has none.
	Deadline time.Duration `json:"deadline,omitempty"`
}

// Overrun is how long an attempt of an agent's run may go on beyond its
// Job's deadline, so that the Job's failure, not an overrun, is what the
// owner hears of (ADR 0043).
const Overrun = 10 * time.Minute

// Cluster is what messaging needs from the cluster.
type Cluster interface {
	// PodAlive says whether the pod of that name and UID is there and has
	// not ended.
	PodAlive(ctx context.Context, name, uid string) (bool, error)
	// Agents are the agents of the installation, by name.
	Agents(ctx context.Context) (map[string]AgentInfo, error)
}

// Caller is who is asking: the owner, an agent's pod, as the cluster
// vouches for it, or the owner's own machine (Host, ADR 0049), whose Agent
// is the place's name. The session a caller names is its own claim.
type Caller struct {
	Owner bool
	Host  bool
	// Meter is the egress proxy's pod (ADR 0050), which may only say what
	// agents used of the model.
	Meter  bool
	Agent  string
	Pod    string
	PodUID string
}

func (c Caller) String() string {
	if c.Owner {
		return Owner
	}
	if c.Host {
		return "place " + c.Agent + " (the owner's machine)"
	}
	return fmt.Sprintf("agent %s (pod %s)", c.Agent, c.Pod)
}

var (
	ErrUnknown   = errors.New("no such addressee")
	ErrForbidden = errors.New("not yours")
	ErrInvalid   = dispatch.ErrInvalid
)

// Service is messaging: sending, taking, reading, where sessions are.
type Service struct {
	Store   Store
	Cluster Cluster
	// Context and Namespace name the cluster in what the owner is told.
	Context, Namespace string
	// Priority is that of the tasks a message becomes. Timeout limits
	// their attempts where the agent's job has no deadline of its own
	// (thirty minutes when zero).
	Priority int
	Timeout  time.Duration
	// Retention is how long a session that has ended is remembered.
	Retention time.Duration
	// Writers are, for each agent, the other agents that may write to it
	// and its sessions (ADR 0047). An agent not here has none.
	Writers map[string][]string
	// Place is the name of the owner's own machine, whose sessions run
	// outside the cluster (ADR 0049); empty when there is none.
	Place string
	Log   *slog.Logger
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().UTC().Format("0102-1504") + "-" + hex.EncodeToString(b)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Resolve turns what a sender wrote into an address.
func (s *Service) Resolve(ctx context.Context, given string) (string, error) {
	given = strings.TrimSpace(given)
	kind, rest := Split(given)
	switch {
	case kind == Owner:
		return Owner, nil
	case kind == "" && nameRe.MatchString(rest):
		kind = "agent"
	}
	switch kind {
	case "agent":
		agents, err := s.agents(ctx)
		if err != nil {
			return "", err
		}
		if _, ok := agents[rest]; ok {
			return AgentAddress(rest), nil
		}
		names := make([]string, 0, len(agents))
		for n := range agents {
			names = append(names, n)
		}
		slices.Sort(names)
		return "", fmt.Errorf("%w: there is no agent %q; the agents are %s, and the owner is owner", ErrUnknown, rest, strings.Join(names, ", "))
	case "session":
		return s.resolveSession(ctx, "", rest)
	case "":
		return "", fmt.Errorf("%w: %q is not an address: write owner, an agent's name, AGENT/SESSION-NAME or session/ID", ErrUnknown, given)
	default:
		return s.resolveSession(ctx, kind, rest)
	}
}

func (s *Service) resolveSession(ctx context.Context, agent, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("%w: name a session", ErrUnknown)
	}
	if agent == "" {
		if _, ok, err := s.Store.GetSession(ctx, ref); err != nil || ok {
			return SessionAddress(ref), err
		}
	}
	all, err := s.Store.Sessions(ctx, agent, time.Time{})
	if err != nil {
		return "", err
	}
	var byName, byID []Session
	for _, x := range all {
		if x.Name == ref {
			byName = append(byName, x)
		} else if strings.HasPrefix(x.ID, ref) {
			byID = append(byID, x)
		}
	}
	// A name that several sessions had is the one that was seen last:
	// the harness gives a reopened session a new conversation.
	if len(byName) > 0 {
		return SessionAddress(byName[0].ID), nil
	}
	if len(byID) == 1 {
		return SessionAddress(byID[0].ID), nil
	}
	where := "known"
	if agent != "" {
		where = "of agent " + agent
	}
	if len(byID) > 1 {
		return "", fmt.Errorf("%w: more than one session %s starts with %q", ErrUnknown, where, ref)
	}
	return "", fmt.Errorf("%w: no session %s is called %q or has an id that starts with it", ErrUnknown, where, ref)
}

// Act is the session a caller acts as, checked against what is known of
// it: a session belongs to the agent whose pod first named it, and while
// it runs in one pod, no other pod may act as it. Which session of its own
// pod a caller is, is the caller's word.
func (s *Service) Act(ctx context.Context, c Caller, session string) (Session, bool, error) {
	if c.Owner {
		return Session{}, false, fmt.Errorf("%w: the owner is not a session", ErrForbidden)
	}
	if !c.Host && s.isPlace(c.Agent) {
		return Session{}, false, fmt.Errorf("%w: %s is the owner's machine, and a pod is not", ErrForbidden, c.Agent)
	}
	if session == "" {
		return Session{}, false, nil
	}
	known, ok, err := s.Store.GetSession(ctx, session)
	if err != nil || !ok {
		return Session{ID: session, Agent: c.Agent}, false, err
	}
	if known.Agent != c.Agent {
		return known, true, fmt.Errorf("%w: session %s is agent %s's, and this pod is agent %s's", ErrForbidden, short(session), known.Agent, c.Agent)
	}
	if known.State != Ended && known.PodUID != "" && known.PodUID != c.PodUID {
		alive, err := s.Cluster.PodAlive(ctx, known.Pod, known.PodUID)
		if err != nil {
			return known, true, err
		}
		if alive {
			return known, true, fmt.Errorf("%w: session %s is running in pod %s, not in this one", ErrForbidden, short(session), known.Pod)
		}
	}
	return known, true, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// from is the address a caller sends as.
func from(c Caller, session string) string {
	switch {
	case c.Owner:
		return Owner
	case session != "":
		return SessionAddress(session)
	}
	return AgentAddress(c.Agent)
}

// Result is what a sender is told.
type Result struct {
	ID     string `json:"id"`
	To     string `json:"to,omitempty"`
	State  string `json:"state"` // delivered, waiting, refused
	Detail string `json:"detail"`
}

// Send sends a message as the caller, from the session it names (if any).
func (s *Service) Send(ctx context.Context, c Caller, session, given, body, answers string) (Result, error) {
	if strings.TrimSpace(body) == "" {
		return Result{}, fmt.Errorf("%w: a message needs a body", ErrInvalid)
	}
	if len(body) > 64<<10 {
		return Result{}, fmt.Errorf("%w: a message is at most 64 KiB; put more in a file or a repository and send where it is", ErrInvalid)
	}
	m := Message{ID: newID(), Given: given, Body: body, Answers: answers, Sent: s.now().UTC()}
	if c.Host && session == "" {
		// The place is not an agent that a message starts: an answer to
		// "the place" would wait for whichever session there read it first.
		return Result{}, fmt.Errorf("%w: say which session sends this (--session ID, or DISPATCHER_SESSION): an answer has to find it", ErrInvalid)
	}
	if !c.Owner {
		me, _, err := s.Act(ctx, c, session)
		if err != nil {
			return Result{}, err
		}
		m.Agent, m.Name, m.Pod, m.Host = c.Agent, me.Name, c.Pod, c.Host
	}
	m.From = from(c, session)
	if answers != "" && given == "" {
		orig, ok, err := s.message(ctx, answers)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, fmt.Errorf("%w: no message %s to answer", ErrUnknown, answers)
		}
		given = orig.From
		m.Given = given
	}
	if answers != "" {
		orig, ok, err := s.message(ctx, answers)
		if err != nil {
			return Result{}, err
		}
		if ok {
			m.Depth = orig.Depth + 1
		}
		// Two sessions that answer each other's answers would go on for
		// ever, each woken by the other.
		if m.Depth > MaxDepth && !c.Owner && orig.From != Owner {
			return Result{}, fmt.Errorf("%w: this would be answer %d in one exchange between sessions; the most is %d. Send a new message if there is something new to say", ErrInvalid, m.Depth, MaxDepth)
		}
	}
	to, err := s.Resolve(ctx, given)
	if err != nil {
		if !errors.Is(err, ErrUnknown) {
			return Result{}, err
		}
		return s.refuse(ctx, m, err.Error())
	}
	if !c.Owner {
		target, err := s.targetAgent(ctx, to)
		if err != nil {
			return Result{}, err
		}
		if !s.mayWrite(c.Agent, target) {
			ok, err := s.answering(ctx, c, to, answers)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				m.To = to
				return s.refuse(ctx, m, s.refusal(c.Agent, target))
			}
		}
	}
	m.To = to
	data, _ := json.Marshal(m)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeSent + "/" + m.ID, Type: TypeSent, Source: Source, Subject: m.ID, Data: data}); err != nil {
		return Result{}, err
	}
	if answers != "" && !c.Owner {
		// Answering a message is having read it.
		_, _ = s.Store.Claim(ctx, s.mark(TypeRead, answers, Delivery{Session: m.From, How: "reply", Pod: c.Pod, Agent: c.Agent}))
	}
	detail, err := s.Route(ctx, m)
	if err != nil {
		return Result{}, err
	}
	return Result{ID: m.ID, To: to, State: "waiting", Detail: detail}, nil
}

// refuse records a message that is not sent, and why.
func (s *Service) refuse(ctx context.Context, m Message, why string) (Result, error) {
	m.Reason = why
	data, _ := json.Marshal(m)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeRefused + "/" + m.ID, Type: TypeRefused, Source: Source, Subject: m.ID, Data: data}); err != nil {
		return Result{}, err
	}
	return Result{ID: m.ID, To: m.To, State: "refused", Detail: why}, nil
}

func (s *Service) mark(typ, msg string, d Delivery) dispatch.Event {
	data, _ := json.Marshal(d)
	return dispatch.Event{ID: typ + "/" + msg, Type: typ, Source: Source, Subject: msg, Data: data}
}

// message is one message, by its id.
func (s *Service) message(ctx context.Context, id string) (Message, bool, error) {
	recs, err := s.Store.OfTypeAbout(ctx, TypeSent, id)
	if err != nil || len(recs) == 0 {
		return Message{}, false, err
	}
	var m Message
	return m, true, json.Unmarshal(recs[0].Data, &m)
}

// Running says whether a session is running now: its last word was not
// that it ended, and the pod it last ran in is there.
func (s *Service) Running(ctx context.Context, x Session) (bool, error) {
	if s.isPlace(x.Agent) {
		return s.hostRunning(x), nil
	}
	if x.State == Ended || x.Pod == "" {
		return false, nil
	}
	return s.Cluster.PodAlive(ctx, x.Pod, x.PodUID)
}

// Route does what a message needs done for it to reach its addressee, and
// says what that is. Doing it again does nothing more.
func (s *Service) Route(ctx context.Context, m Message) (string, error) {
	kind, rest := Split(m.To)
	switch kind {
	case Owner:
		return "for the owner: a notice, sent unless notices are off, when it is withheld; the owner reads it with dispatch inbox", nil
	case "agent":
		return s.start(ctx, m, rest, "")
	}
	x, ok, err := s.Store.GetSession(ctx, rest)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: session %s", ErrUnknown, rest)
	}
	running, err := s.Running(ctx, x)
	if err != nil {
		return "", err
	}
	if running {
		listening.tell(x.ID, "message")
		where := "in pod " + x.Pod
		if s.isPlace(x.Agent) {
			where = "on the owner's machine"
		}
		if x.State == Idle && listening.listening(x.ID) {
			return fmt.Sprintf("for session %s of agent %s, idle %s: it is woken by it", short(x.ID), x.Agent, where), nil
		}
		if x.State == Idle {
			return fmt.Sprintf("for session %s of agent %s, idle %s but not listening: it gets it at its next turn", short(x.ID), x.Agent, where), nil
		}
		return fmt.Sprintf("for session %s of agent %s, busy %s: it gets it after its next tool call, or when its turn ends", short(x.ID), x.Agent, where), nil
	}
	if s.isPlace(x.Agent) {
		return s.waitForHost(ctx, m, x)
	}
	if x.Kind == "interactive" && x.Name != "" {
		agents, err := s.agents(ctx)
		if err != nil {
			return "", err
		}
		if agents[x.Agent].Class == "" {
			// A standing agent: its pod opens the session again (ADR 0053).
			return s.waitForOpen(ctx, m, x)
		}
	}
	return s.start(ctx, m, x.Agent, x.ID)
}

// start submits the task that has the message taken: the session resumed
// as its agent, or a new conversation of the agent.
func (s *Service) start(ctx context.Context, m Message, agent, session string) (string, error) {
	agents, err := s.agents(ctx)
	if err != nil {
		return "", err
	}
	info := agents[agent]
	what := "a new conversation of agent " + agent
	if session != "" {
		what = fmt.Sprintf("session %s, which is not running, resumed as agent %s", short(session), agent)
	}
	if info.Class == "" {
		reason := fmt.Sprintf("for %s; agent %s has no job to run it in, so it waits until one of its sessions takes it", what, agent)
		_, err := s.Store.Claim(ctx, s.waiting(m.ID, "no-job", Wait{Reason: reason}))
		return reason, err
	}
	// One task at a time takes a session's messages: a second message
	// while the first's task is open is taken by that task.
	key := m.ID
	if session != "" {
		if open, ok, err := s.openTaskFor(ctx, session); err != nil {
			return "", err
		} else if ok {
			reason := fmt.Sprintf("for %s, by the task already open for it (%s)", what, open)
			_, err := s.Store.Claim(ctx, s.waiting(m.ID, open, Wait{Reason: reason, Task: open}))
			return reason, err
		}
	}
	task := TaskPrefix + key
	env := map[string]string{"TASK_MESSAGE": m.ID}
	if session != "" {
		env["TASK_SESSION"] = session
	}
	payload, _ := json.Marshal(map[string]any{"env": env})
	// The agent's own time limit: its Job's deadline, as for its scheduled runs.
	timeout := s.Timeout
	if info.Deadline > 0 {
		timeout = info.Deadline + Overrun
	} else if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	if err := s.Store.Submit(ctx, dispatch.TaskSpec{ID: task, Class: info.Class, Priority: s.Priority, Payload: payload, AttemptTimeout: timeout}); err != nil {
		return "", err
	}
	if session != "" {
		if err := s.Store.Emit(ctx, dispatch.Event{ID: "message.task/" + session + "/" + task, Type: "message.task", Source: Source, Subject: SessionAddress(session), Data: json.RawMessage(fmt.Sprintf(`{"task":%q}`, task))}); err != nil {
			return "", err
		}
	}
	reason := fmt.Sprintf("for %s: task %s, from %s", what, task, info.Class)
	if st, err := s.Store.Explain(ctx, task); err == nil && st.State == dispatch.Pending && st.Reason.Kind != "" {
		reason += fmt.Sprintf("; it waits to start (%s%s)", st.Reason.Kind, prefixed(st.Reason.Detail))
	}
	_, err = s.Store.Claim(ctx, s.waiting(m.ID, task, Wait{Reason: reason, Task: task}))
	return reason, err
}

func prefixed(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func (s *Service) waiting(msg, key string, w Wait) dispatch.Event {
	data, _ := json.Marshal(w)
	return dispatch.Event{ID: TypeWaiting + "/" + msg + "/" + key, Type: TypeWaiting, Source: Source, Subject: msg, Data: data}
}

// openTaskFor is a task that is taking a session's messages and has not
// ended.
func (s *Service) openTaskFor(ctx context.Context, session string) (string, bool, error) {
	recs, err := s.Store.OfTypeAbout(ctx, "message.task", SessionAddress(session))
	if err != nil {
		return "", false, err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		var t struct{ Task string }
		if json.Unmarshal(recs[i].Data, &t) != nil {
			continue
		}
		st, err := s.Store.Explain(ctx, t.Task)
		if errors.Is(err, dispatch.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if st.State == dispatch.Pending || st.State == dispatch.Running || st.State == dispatch.Waiting {
			return t.Task, true, nil
		}
	}
	return "", false, nil
}

// Answer sends what a run started for a message ended with, as the answer
// to it, unless the session answered it by itself.
func (s *Service) Answer(ctx context.Context, c Caller, session, id, body string) (Result, error) {
	answers, err := s.Store.Answers(ctx, id)
	if err != nil {
		return Result{}, err
	}
	for _, r := range answers {
		var m Message
		if json.Unmarshal(r.Data, &m) == nil && m.Agent == c.Agent {
			return Result{ID: m.ID, To: m.To, State: "answered", Detail: "the session answered it by itself"}, nil
		}
	}
	if strings.TrimSpace(body) == "" {
		body = "(The run started for this message ended without saying anything.)"
	}
	return s.Send(ctx, c, session, "", body, id)
}

// agents are the agents by name: those the cluster has a job for, and those
// that have had a session here (an interactive agent has no job).
func (s *Service) agents(ctx context.Context) (map[string]AgentInfo, error) {
	jobs, err := s.Cluster.Agents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]AgentInfo, len(jobs))
	for n, a := range jobs {
		out[n] = a
	}
	seen, err := s.Store.Sessions(ctx, "", s.now().Add(-30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	for _, x := range seen {
		if _, ok := out[x.Agent]; !ok {
			out[x.Agent] = AgentInfo{Name: x.Agent, Interactive: x.Kind == "interactive"}
		}
	}
	return out, nil
}
