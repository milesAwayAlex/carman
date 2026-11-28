package message

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
		heirs, err := s.heirs(ctx, c, session)
		if err != nil {
			return nil, err
		}
		to = append(to, heirs...)
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
		return err == nil && ok && x.Agent == c.Agent && (session == "" || c.Host)
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
		case TypeUndelivered:
			if st.State == "delivered" {
				st.State = "waiting"
			}
		case TypeRead:
			st.State = "read"
		case TypeTimed:
			// Asked for at a time, and not sent yet.
			var t Timed
			if json.Unmarshal(r.Data, &t) == nil && st.Message.ID == "" {
				st.Message = Message{ID: t.ID, From: t.From, Agent: t.Agent, To: t.To, Given: t.Given, Body: t.Body, Sent: t.At}
				st.State = "timed"
			}
		case TypeTimedDone:
			var d TimedDone
			if json.Unmarshal(r.Data, &d) == nil && d.How != "sent" && st.State == "timed" {
				st.State = d.How
			}
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
	Address string `json:"address"`
	Kind    string `json:"kind"` // owner, agent, session
	Agent   string `json:"agent,omitempty"`
	Name    string `json:"name,omitempty"`
	// Named is the address by name that reaches this session, AGENT/NAME or
	// PLACE/NAME, which outlives its id; empty when none does.
	Named   string `json:"named,omitempty"`
	Running bool   `json:"running,omitempty"`
	State   string `json:"state"` // busy, idle, not running; for an agent: what a message starts
	Where   string `json:"where,omitempty"`
	// NotWritable says why the caller may not write to it (ADR 0047);
	// empty when it may.
	NotWritable string    `json:"not_writable,omitempty"`
	Seen        time.Time `json:"seen,omitzero"`
	// For going to a session (ADR 0057): the pod it is or was last in, how
	// it runs (interactive or headless), whether its agent is a standing
	// one, and, for the owner and the owner's machine only, how many
	// messages it sent the owner that the owner has not read. An agent
	// says Standing too: a session can be opened in its pod.
	Pod      string `json:"pod,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Standing bool   `json:"standing,omitempty"`
	ForOwner int    `json:"for_owner,omitempty"`
	// Refused says, for a session whose pod's client the dispatcher is
	// refusing, since when and why; empty otherwise.
	Refused string `json:"refused,omitempty"`
}

// Peers are who is worth writing to: the owner, the agents a message
// reaches, and the sessions seen within since. Sessions that run come
// first; of those that do not, only those a message still reaches: the
// latest of their agent and name, or one that has exchanged a message,
// and only of an agent the cluster defines or of the place. All lists
// every session seen. Those the caller may not write to say so.
func (s *Service) Peers(ctx context.Context, c Caller, since time.Duration, all bool) ([]Peer, error) {
	defined, err := s.Cluster.Agents(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := s.Store.Sessions(ctx, "", s.now().Add(-since))
	if err != nil {
		return nil, err
	}
	talked := map[string]bool{}
	if !all {
		addrs, err := s.Store.Correspondents(ctx, s.now().Add(-since))
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			talked[a] = true
		}
	}
	// What each session sent the owner that is not read: how many. For
	// the owner and the owner's machine, which is where the owner looks.
	forOwner := map[string]int{}
	if c.Owner || c.Host {
		recs, err := s.Store.Unmarked(ctx, []string{Owner}, "read", 200)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			var m Message
			if json.Unmarshal(r.Data, &m) == nil {
				forOwner[m.From]++
			}
		}
	}
	var running, stopped []Peer
	interactive := map[string]bool{} // agents with an interactive session listed
	latest := map[[2]string]bool{}
	for _, x := range sessions { // the latest first
		key := [2]string{x.Agent, x.Name}
		first := !latest[key]
		latest[key] = true
		on, err := s.Running(ctx, x)
		if err != nil {
			return nil, err
		}
		_, known := defined[x.Agent]
		if !on && !all && (!first && !talked[SessionAddress(x.ID)] || !known && !s.isPlace(x.Agent)) {
			continue
		}
		where := "in " + x.Pod
		if s.isPlace(x.Agent) {
			where = "on the owner's machine"
		}
		p := Peer{Address: SessionAddress(x.ID), Kind: "session", Agent: x.Agent, Name: x.Name, Seen: x.Seen, Running: on,
			State: "not running (kept)", Where: "last " + where, NotWritable: s.notWritable(c, x.Agent),
			Pod: x.Pod, Mode: x.Kind, Standing: defined[x.Agent].Interactive, ForOwner: forOwner[SessionAddress(x.ID)]}
		// A name reaches the session seen last of that name (Resolve).
		if x.Name != "" && first {
			p.Named = x.Agent + "/" + x.Name
		}
		if s.isPlace(x.Agent) && !on {
			p.State = "not running: a message waits for it, or for the next session of its name there"
		}
		if x.Kind == "interactive" {
			interactive[x.Agent] = true
		}
		if on {
			p.State, p.Where = x.State, x.Kind+" "+where
			if x.State == Idle && !listening.listening(x.ID) {
				p.State = "idle, not listening: it gets messages at its next turn"
			}
			if r, ok := s.refusedNow(x.PodUID); ok && !(x.State == Idle && listening.listening(x.ID)) {
				p.Refused = r.words()
				p.State += "; " + p.Refused
			}
			running = append(running, p)
			continue
		}
		stopped = append(stopped, p)
	}

	out := []Peer{{Address: Owner, Kind: Owner, State: "a notice (withheld while notices are off); read with dispatch inbox"}}
	// An agent is listed when a message to it does something: starts a
	// run, or waits for a session of it that a person works in. The place
	// is no agent (ADR 0049).
	names := map[string]AgentInfo{}
	for n, a := range defined {
		if a.Class != "" || a.Interactive || interactive[n] {
			names[n] = a
		}
	}
	for n := range interactive {
		if _, ok := names[n]; !ok {
			names[n] = AgentInfo{Name: n, Interactive: true}
		}
	}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		if s.isPlace(n) {
			continue
		}
		state := "a message starts a new conversation, from " + names[n].Class
		if names[n].Class == "" {
			state = "no job to start: a message waits for one of its sessions"
		}
		out = append(out, Peer{Address: AgentAddress(n), Kind: "agent", Agent: n, State: state, NotWritable: s.notWritable(c, n),
			Standing: defined[n].Interactive})
	}
	out = append(out, running...)
	return append(out, stopped...), nil
}

// notWritable says, for a peer of agent target, why the caller may not
// write to it unless answering it.
func (s *Service) notWritable(c Caller, target string) string {
	if c.Owner || s.mayWrite(c.Agent, target) {
		return ""
	}
	who := "the owner and its own sessions"
	if w := s.Writers[target]; len(w) > 0 {
		who += ", and agents " + strings.Join(w, ", ")
	}
	return "not writable by you (only " + who + "; you may answer a message from it)"
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
	if err := r.Service.CloseRefusals(ctx); err != nil && ctx.Err() == nil {
		r.Logger.Warn("ending the refusals an earlier dispatcher left open", "error", err)
	}
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warn("routing messages", "error", err)
		}
		if time.Since(sweep) > 30*time.Second {
			if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
				r.Logger.Warn("looking for sessions whose pod has gone", "error", err)
			}
			if err := r.Service.SendAgain(ctx); err != nil && ctx.Err() == nil {
				r.Logger.Warn("sending again what stopped at the plan's limit", "error", err)
			}
			if err := r.Service.TellWaiting(ctx); err != nil && ctx.Err() == nil {
				r.Logger.Warn("telling senders of messages that have waited", "error", err)
			}
			r.Service.QuietRefusals(ctx)
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
	// What was asked for at a time that has come is sent first, and routed
	// with the rest.
	if err := s.SendDue(ctx); err != nil && ctx.Err() == nil {
		r.Logger.Warn("sending the messages whose time has come", "error", err)
	}
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
		if s.isPlace(x.Agent) {
			// No pod tells: a day without a word, or a waiter, is the end.
			if s.hostRunning(x) {
				continue
			}
			x.State = Ended
			if _, _, err := s.Store.PutSession(ctx, x); err != nil {
				return err
			}
			if err := s.ended(ctx, x, "not heard from for a day"); err != nil {
				return err
			}
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
