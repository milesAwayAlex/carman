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
	// TypeUndelivered: a delivery by wake went to a harness that had gone
	// (ADR 0053). ID message.undelivered/MSG, so once for a message; the
	// message is then delivered once more, as message.delivered/MSG/again.
	TypeUndelivered = "message.undelivered"
	TypeWaiting     = "message.waiting" // ID message.waiting/MSG/KEY; data a Wait
	TypeRefused     = "message.refused" // a message that could not be addressed; data a Message and why

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
var Types = []string{TypeSent, TypeDelivered, TypeUndelivered, TypeRead, TypeWaiting, TypeRefused,
	TypeSessionStarted, TypeSessionEnded, TypeWatch, TypeWatched, TypeClientRefused, TypeClientRefusedEnded, TypeWaited}

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
	// Again is set on a message as it is handed over a second time, after
	// a wake that reached a harness that had gone; never in the log.
	Again bool `json:"again,omitempty"`
	// AgainAfterLimit: the sender asks that, if its run stops at the plan's
	// usage limit, the dispatcher send it again once after the reset.
	AgainAfterLimit bool `json:"again_after_limit,omitempty"`
	// Model is the model the run a message starts is to use, where the
	// dispatcher itself sends it for the owner's queue (ADR
	// new-the-queue-runs-itself); a sender cannot say it.
	Model string `json:"model,omitempty"`
	// Resends is the message this one sends again, after a limit.
	Resends string `json:"resends,omitempty"`
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
	// Woke are the messages a wake handed this session with no word from
	// it since: if its harness is then found gone, they were handed to
	// nobody (ADR 0053).
	Woke []string `json:"woke,omitempty"`
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
	// timed: the messages asked for at a time (timed.go).
	timed timing

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
	// RunsPerSender is how many runs one sender's new messages may start in
	// RunsPeriod (DefaultRunsPerSender in DefaultRunsPeriod when zero; below
	// zero, no limit).
	RunsPerSender int
	RunsPeriod    time.Duration
	// AgainAfterLimit sends every message whose run stops at the plan's
	// usage limit again once after the reset, as if its sender had asked.
	AgainAfterLimit bool
	// Writers are, for each agent, the other agents that may write to it
	// and its sessions (ADR 0047). An agent not here has none.
	Writers map[string][]string
	// Place is the name of the owner's own machine, whose sessions run
	// outside the cluster (ADR 0049); empty when there is none.
	Place string
	// WaitNotice is how long a message may wait undelivered before its
	// sender is told why, once (DefaultWaitNotice when zero; below zero,
	// never).
	WaitNotice time.Duration
	Log        *slog.Logger
	Now        func() time.Time

	// refused are the pods whose client is being refused.
	refused refusals
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
	return s.send(ctx, c, session, given, body, answers, false, SendOptions{})
}

// SendOptions are what a sender may ask beside the message.
type SendOptions struct {
	// AgainAfterLimit: sent again once after the reset if its run stops at
	// the plan's usage limit.
	AgainAfterLimit bool
	// id: the id it is sent under, for a message asked for at a time.
	id string
	// Model: the model its run is to use (SendQueued only).
	Model string
}

// SendQueued sends an item of the owner's queue to an agent, as the owner,
// for the dispatcher itself (ADR new-the-queue-runs-itself): the run it
// starts uses the model given, and its answer goes to the owner like any
// answer to him. The id is the message's, made by the caller, which has
// recorded it before sending.
func (s *Service) SendQueued(ctx context.Context, id, to, body, model string) (Result, error) {
	return s.send(ctx, Caller{Owner: true}, "", to, body, "", true, SendOptions{Model: model, id: id})
}

// SendWith is Send with options.
func (s *Service) SendWith(ctx context.Context, c Caller, session, given, body, answers string, o SendOptions) (Result, error) {
	return s.send(ctx, c, session, given, body, answers, false, o)
}

// SendVouched sends a message as the caller to an addressee the dispatcher
// itself vouches for: a brief that passed the improvement loop's gate, or
// its one line for the owner (ADR 0058). The lists of who may write to an
// agent (ADR 0047) are not asked; every other rule of Send holds.
func (s *Service) SendVouched(ctx context.Context, c Caller, session, to, body string) (Result, error) {
	return s.send(ctx, c, session, to, body, "", true, SendOptions{})
}

// NewID is an id for something in the log: when, and six random bytes.
func NewID() string { return newID() }

func (s *Service) send(ctx context.Context, c Caller, session, given, body, answers string, vouched bool, o SendOptions) (Result, error) {
	if strings.TrimSpace(body) == "" {
		return Result{}, fmt.Errorf("%w: a message needs a body", ErrInvalid)
	}
	if len(body) > 64<<10 {
		return Result{}, fmt.Errorf("%w: a message is at most 64 KiB; put more in a file or a repository and send where it is", ErrInvalid)
	}
	m := Message{ID: newID(), Given: given, Body: body, Answers: answers, Sent: s.now().UTC(), AgainAfterLimit: o.AgainAfterLimit, Model: o.Model}
	if o.id != "" {
		m.ID = o.id
	}
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
	if !c.Owner && !vouched {
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
	return s.route(ctx, m, false)
}

func (s *Service) route(ctx context.Context, m Message, lifted bool) (string, error) {
	kind, rest := Split(m.To)
	switch kind {
	case Owner:
		return "for the owner: a notice, sent unless notices are off, when it is withheld; the owner reads it with dispatch inbox", nil
	case "agent":
		return s.start(ctx, m, rest, "", lifted)
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
		words, key := s.hearing(ctx, x)
		if key != "" {
			// It does not reach the session by itself: the message says why.
			if _, err := s.Store.Claim(ctx, s.waiting(m.ID, key, Wait{Reason: words})); err != nil {
				return "", err
			}
		}
		return words, nil
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
	return s.start(ctx, m, x.Agent, x.ID, lifted)
}

// start submits the task that has the message taken: the session resumed
// as its agent, or a new conversation of the agent.
func (s *Service) start(ctx context.Context, m Message, agent, session string, lifted bool) (string, error) {
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
	if limited, why, err := s.overLimit(ctx, m, task, lifted); err != nil {
		return "", err
	} else if limited {
		reason := fmt.Sprintf("for %s; held: %s", what, why)
		if _, err := s.Store.Claim(ctx, s.waiting(m.ID, "limit", Wait{Reason: reason})); err != nil {
			return "", err
		}
		data, _ := json.Marshal(map[string]string{"message": m.ID})
		_, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeHeld + "/" + m.ID, Type: TypeHeld, Source: Source, Subject: m.From, Data: data})
		return reason, err
	}
	env := map[string]string{"TASK_MESSAGE": m.ID}
	if m.Model != "" {
		env["TASK_MODEL"] = m.Model
	}
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
	if counted(m) {
		if _, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeRun + "/" + m.ID, Type: TypeRun, Source: Source, Subject: m.From,
			Data: json.RawMessage(fmt.Sprintf(`{"task":%q}`, task))}); err != nil {
			return "", err
		}
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
// to it, unless the session answered it by itself. A failed run whose last
// words are the plan's usage limit is told as that (limited).
func (s *Service) Answer(ctx context.Context, c Caller, session, id, body string, failed bool) (Result, error) {
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
	if failed {
		if reset, ok := LimitReset(body, s.now()); ok {
			return s.limited(ctx, c, session, id, body, reset)
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

// The limit on runs started by messages (RunsPerSender in RunsPeriod): a
// session in a loop, or one told to by text it read, could otherwise start
// runs until the plan's limit. The owner is not limited, nor an answer
// (an exchange has its own cap, MaxDepth).
const (
	// TypeRun records a run started by a message, about its sender.
	TypeRun = "message.run"
	// TypeHeld records a message held by the limit, about its sender.
	TypeHeld = "message.held"
	// TypeLifted records that the owner lifted a sender's limit.
	TypeLifted = "message.limit.lifted"
	// DefaultRunsPerSender and DefaultRunsPeriod are the limit when the
	// installation sets none: a busy evening's session started a dozen.
	DefaultRunsPerSender = 24
	DefaultRunsPeriod    = 3 * time.Hour
)

func counted(m Message) bool { return m.From != Owner && m.Answers == "" }

// overLimit says whether starting this message's run would pass its
// sender's limit, and why, as the sender is told.
func (s *Service) overLimit(ctx context.Context, m Message, task string, lifted bool) (bool, string, error) {
	if lifted || !counted(m) || s.RunsPerSender < 0 {
		return false, "", nil
	}
	if _, err := s.Store.Explain(ctx, task); err == nil {
		return false, "", nil // started already: routing again starts nothing
	} else if !errors.Is(err, dispatch.ErrNotFound) {
		return false, "", err
	}
	max, period := s.RunsPerSender, s.RunsPeriod
	if max == 0 {
		max = DefaultRunsPerSender
	}
	if period <= 0 {
		period = DefaultRunsPeriod
	}
	n, err := s.runsSince(ctx, m.From, s.now().Add(-period))
	if err != nil || n < max {
		return false, "", err
	}
	return true, fmt.Sprintf("%s has started %d runs by messages in the last %s, the most this installation allows (dispatcher.messages.runsPerSender); "+
		"it starts when the owner lifts the limit (dispatch lift %s), and the sender's next message after the period is not held", m.From, n, period, m.From), nil
}

// runsSince counts the runs a sender's messages started since a time, and
// since the owner last lifted its limit.
func (s *Service) runsSince(ctx context.Context, sender string, since time.Time) (int, error) {
	lifts, err := s.Store.OfTypeAbout(ctx, TypeLifted, sender)
	if err != nil {
		return 0, err
	}
	var after int64
	for _, l := range lifts {
		after = max(after, l.Seq)
	}
	runs, err := s.Store.OfTypeAbout(ctx, TypeRun, sender)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range runs {
		if r.Seq > after && !r.Time.Before(since) {
			n++
		}
	}
	return n, nil
}

// Lift is the owner lifting a sender's limit: its count starts again, and
// the messages it held start now. It says how many.
func (s *Service) Lift(ctx context.Context, c Caller, sender string) (int, error) {
	if !c.Owner {
		return 0, fmt.Errorf("%w: only the owner lifts a sender's limit", ErrForbidden)
	}
	if strings.TrimSpace(sender) == "" {
		return 0, fmt.Errorf("%w: say whose limit: the sender's address, as the hold names it", ErrInvalid)
	}
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeLifted + "/" + newID(), Type: TypeLifted, Source: Source, Subject: sender, Data: json.RawMessage(`{}`)}); err != nil {
		return 0, err
	}
	held, err := s.Store.OfTypeAbout(ctx, TypeHeld, sender)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range held {
		var d struct{ Message string }
		if json.Unmarshal(h.Data, &d) != nil {
			continue
		}
		if _, err := s.Store.Explain(ctx, TaskPrefix+d.Message); err == nil {
			continue // started already
		}
		m, ok, err := s.message(ctx, d.Message)
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		if _, err := s.route(ctx, m, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
