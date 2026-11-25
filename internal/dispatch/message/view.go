package message

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"carman/internal/dispatch"
)

// Inbox is what has been sent to the caller and not yet read: to the
// session it names and to its agent by name, or, for the owner, to the
// owner. Delivered ones are there too, marked, until read.
type InboxItem struct {
	Message
	Delivered *Delivery `json:"delivered,omitempty"`
}

func (s *Service) Inbox(ctx context.Context, c Caller, session string) ([]InboxItem, error) {
	to := []string{Owner}
	if !c.Owner {
		if _, _, err := s.Act(ctx, c, session); err != nil {
			return nil, err
		}
		to = []string{AgentAddress(c.Agent)}
		if session != "" {
			to = append(to, SessionAddress(session))
		}
	}
	recs, err := s.Store.Unmarked(ctx, to, "read", 200)
	if err != nil {
		return nil, err
	}
	out := make([]InboxItem, 0, len(recs))
	for _, r := range recs {
		var it InboxItem
		if json.Unmarshal(r.Data, &it.Message) != nil {
			continue
		}
		if ds, err := s.Store.OfTypeAbout(ctx, TypeDelivered, it.ID); err == nil && len(ds) > 0 {
			var d Delivery
			if json.Unmarshal(ds[0].Data, &d) == nil {
				it.Delivered = &d
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// Read marks messages read by the caller; only messages to it.
func (s *Service) Read(ctx context.Context, c Caller, session string, ids []string) ([]string, error) {
	if !c.Owner {
		if _, _, err := s.Act(ctx, c, session); err != nil {
			return nil, err
		}
	}
	var done []string
	for _, id := range ids {
		m, ok, err := s.message(ctx, id)
		if err != nil {
			return done, err
		}
		if !ok || !s.isFor(c, session, m) {
			return done, fmt.Errorf("%w: message %s is not to you", ErrForbidden, id)
		}
		if _, err := s.Store.Claim(ctx, s.mark(TypeRead, id, Delivery{Session: from(c, session), How: "read", Pod: c.Pod, Agent: c.Agent})); err != nil {
			return done, err
		}
		done = append(done, id)
	}
	return done, nil
}

func (s *Service) isFor(c Caller, session string, m Message) bool {
	if c.Owner {
		return m.To == Owner
	}
	if m.To == AgentAddress(c.Agent) || (session != "" && m.To == SessionAddress(session)) {
		return true
	}
	// A message to another session of the caller's agent is the agent's.
	if kind, rest := Split(m.To); kind == "session" {
		x, ok, err := s.Store.GetSession(context.Background(), rest)
		return err == nil && ok && x.Agent == c.Agent && session == ""
	}
	return false
}

// Status is what became of a message: its events, and its answers.
type Status struct {
	Message Message              `json:"message"`
	State   string               `json:"state"` // refused, waiting, delivered, read, answered
	Events  []dispatch.Record    `json:"events"`
	Answers []Message            `json:"answers,omitempty"`
	Task    *dispatch.TaskStatus `json:"-"`
	TaskNow string               `json:"task,omitempty"`
}

// Status is shown to the owner, to the sender's agent and to the addressee's.
func (s *Service) Status(ctx context.Context, c Caller, id string) (Status, error) {
	recs, err := s.Store.About(ctx, id)
	if err != nil {
		return Status{}, err
	}
	var st Status
	for _, r := range recs {
		switch r.Type {
		case TypeSent, TypeRefused:
			_ = json.Unmarshal(r.Data, &st.Message)
			st.State = map[string]string{TypeSent: "waiting", TypeRefused: "refused"}[r.Type]
		case TypeDelivered:
			if st.State == "waiting" {
				st.State = "delivered"
			}
		case TypeRead:
			st.State = "read"
		case TypeWaiting:
			var w Wait
			if json.Unmarshal(r.Data, &w) == nil && w.Task != "" {
				if ts, err := s.Store.Explain(ctx, w.Task); err == nil {
					st.TaskNow = fmt.Sprintf("%s: %s, attempt %d", w.Task, ts.State, ts.Attempt)
					if ts.Reason.Kind != "" {
						st.TaskNow += fmt.Sprintf(" (%s%s)", ts.Reason.Kind, prefixed(ts.Reason.Detail))
					}
				}
			}
		}
		st.Events = append(st.Events, r)
	}
	if st.Message.ID == "" {
		return Status{}, fmt.Errorf("%w: no message %s", ErrUnknown, id)
	}
	if !c.Owner && st.Message.Agent != c.Agent && !s.isFor(c, "", st.Message) && st.Message.To != AgentAddress(c.Agent) {
		if kind, rest := Split(st.Message.To); kind != "session" || !s.sessionOf(ctx, rest, c.Agent) {
			return Status{}, fmt.Errorf("%w: message %s is neither from nor to agent %s", ErrForbidden, id, c.Agent)
		}
	}
	answers, err := s.Store.Answers(ctx, id)
	if err != nil {
		return st, err
	}
	for _, r := range answers {
		var m Message
		if json.Unmarshal(r.Data, &m) == nil {
			st.Answers = append(st.Answers, m)
		}
	}
	if len(st.Answers) > 0 {
		st.State = "answered"
	}
	return st, nil
}

func (s *Service) sessionOf(ctx context.Context, session, agent string) bool {
	x, ok, err := s.Store.GetSession(ctx, session)
	return err == nil && ok && x.Agent == agent
}

// Peer is someone who can be written to.
type Peer struct {
	Address string    `json:"address"`
	Kind    string    `json:"kind"` // owner, agent, session
	Agent   string    `json:"agent,omitempty"`
	Name    string    `json:"name,omitempty"`
	State   string    `json:"state"` // busy, idle, not running; for an agent: what a message starts
	Where   string    `json:"where,omitempty"`
	Seen    time.Time `json:"seen,omitzero"`
}

// Peers are who can be written to: the owner, the agents by name, and the
// sessions seen lately, running or not.
func (s *Service) Peers(ctx context.Context, since time.Duration) ([]Peer, error) {
	out := []Peer{{Address: Owner, Kind: Owner, State: "a notice (withheld while notices are off); read with dispatch inbox"}}
	agents, err := s.agents(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		a := agents[n]
		state := "a message starts a new conversation, from " + a.Class
		if a.Class == "" {
			state = "no job to start: a message waits for one of its sessions"
		}
		out = append(out, Peer{Address: AgentAddress(n), Kind: "agent", Agent: n, State: state})
	}
	sessions, err := s.Store.Sessions(ctx, "", s.now().Add(-since))
	if err != nil {
		return nil, err
	}
	for _, x := range sessions {
		running, err := s.Running(ctx, x)
		if err != nil {
			return nil, err
		}
		p := Peer{Address: SessionAddress(x.ID), Kind: "session", Agent: x.Agent, Name: x.Name, Seen: x.Seen, State: "not running (kept)", Where: "last in " + x.Pod}
		if running {
			p.State, p.Where = x.State, x.Kind+" in "+x.Pod
			if x.State == Idle && !listening.listening(x.ID) {
				p.State = "idle, not listening: it gets messages at its next turn"
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Router does, in the background, what the log asks of messaging: routes
// what was sent (again, after a restart that came between the send and
// its routing), and finds sessions whose pod has gone without a word.
type Router struct {
	Service *Service
	Logger  *slog.Logger
	Every   time.Duration
}

const routerMark = "messages"

func (r *Router) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 3 * time.Second
	}
	sweep := time.Time{}
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warn("routing messages", "error", err)
		}
		if time.Since(sweep) > 30*time.Second {
			if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
				r.Logger.Warn("looking for sessions whose pod has gone", "error", err)
			}
			sweep = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Pass routes the messages sent since the last pass.
func (r *Router) Pass(ctx context.Context) error {
	s := r.Service
	var p struct {
		Seq int64 `json:"seq"`
	}
	found, err := s.Store.Mark(ctx, routerMark, &p)
	if err != nil {
		return err
	}
	if !found {
		last, err := s.Store.Last(ctx, "", 1)
		if err != nil {
			return err
		}
		if len(last) > 0 {
			p.Seq = last[0].Seq
		}
		return s.Store.SetMark(ctx, routerMark, p)
	}
	recs, err := s.Store.EventsOfTypes(ctx, p.Seq, []string{TypeSent}, 100)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		var m Message
		if json.Unmarshal(rec.Data, &m) == nil && m.To != Owner {
			if ds, err := s.Store.OfTypeAbout(ctx, TypeDelivered, m.ID); err != nil {
				return err
			} else if len(ds) == 0 {
				if _, err := s.Route(ctx, m); err != nil {
					r.Logger.Warn("routing a message", "message", m.ID, "error", err)
				}
			}
		}
		p.Seq = rec.Seq
	}
	if len(recs) > 0 {
		return s.Store.SetMark(ctx, routerMark, p)
	}
	return nil
}

// Sweep finds sessions that say they run in a pod that has gone, records
// that they ended, and routes what waits for them.
func (r *Router) Sweep(ctx context.Context) error {
	s := r.Service
	sessions, err := s.Store.Sessions(ctx, "", time.Time{})
	if err != nil {
		return err
	}
	for _, x := range sessions {
		if x.State == Ended {
			continue
		}
		alive, err := s.Cluster.PodAlive(ctx, x.Pod, x.PodUID)
		if err != nil {
			return err
		}
		if alive {
			continue
		}
		x.State = Ended
		if _, _, err := s.Store.PutSession(ctx, x); err != nil {
			return err
		}
		if err := s.ended(ctx, x, "its pod "+x.Pod+" has gone"); err != nil {
			return err
		}
	}
	if s.Retention > 0 {
		return s.Store.ForgetSessions(ctx, s.now().Add(-s.Retention))
	}
	return nil
}
