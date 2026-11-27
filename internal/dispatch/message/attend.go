package message

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"carman/internal/dispatch"
)

// A standing agent's pod attends to its own sessions (ADR 0053): it says
// when a harness the dispatcher believes is running has gone, and it opens
// again a named session that a message waits for. These calls are the
// pod's, for its own agent, and say nothing of any other.

// Attend is what a standing agent's pod is told by the dispatcher.
type Attend struct {
	// Running are the sessions of the caller's agent the dispatcher
	// believes run in the caller's pod.
	Running []Session `json:"running"`
	// Waiting are the messages that wait for a named session of the
	// caller's agent that is not running, by name.
	Waiting []Awaited `json:"waiting"`
	// Unheard are the sessions that run in the caller's pod, idle with no
	// listener, that messages wait for.
	Unheard []Unheard `json:"unheard"`
}

// Awaited is a name of a session that messages wait for.
type Awaited struct {
	Name     string   `json:"name"`
	Session  string   `json:"session"` // the latest session of that name
	Cwd      string   `json:"cwd,omitempty"`
	Messages []string `json:"messages"`
}

func (s *Service) podOnly(c Caller) error {
	if c.Owner || c.Host || c.Pod == "" || c.PodUID == "" || s.isPlace(c.Agent) {
		return fmt.Errorf("%w: only a standing agent's pod attends to its sessions", ErrForbidden)
	}
	return nil
}

// Attending is what the caller's pod has to attend to.
func (s *Service) Attending(ctx context.Context, c Caller) (Attend, error) {
	out := Attend{Running: []Session{}, Waiting: []Awaited{}, Unheard: []Unheard{}}
	if err := s.podOnly(c); err != nil {
		return out, err
	}
	all, err := s.Store.Sessions(ctx, c.Agent, s.now().Add(-s.keep()))
	if err != nil {
		return out, err
	}
	byName := map[string][]Session{} // the latest first
	running := map[string]bool{}
	for _, x := range all {
		on, err := s.Running(ctx, x)
		if err != nil {
			return out, err
		}
		if on && x.PodUID == c.PodUID {
			out.Running = append(out.Running, x)
		}
		if x.Name == "" || x.Kind != "interactive" {
			continue
		}
		if on {
			running[x.Name] = true
		}
		byName[x.Name] = append(byName[x.Name], x)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if running[n] {
			// A session of that name runs: it takes them (heirs).
			continue
		}
		var to []string
		for _, x := range byName[n] {
			to = append(to, SessionAddress(x.ID))
		}
		recs, err := s.Store.Unmarked(ctx, to, "delivered", 50)
		if err != nil {
			return out, err
		}
		if len(recs) == 0 {
			continue
		}
		a := Awaited{Name: n, Session: byName[n][0].ID, Cwd: byName[n][0].Cwd}
		for _, r := range recs {
			a.Messages = append(a.Messages, r.Subject)
		}
		out.Waiting = append(out.Waiting, a)
	}
	if out.Unheard, err = s.unheard(ctx, c, out.Running); err != nil {
		return out, err
	}
	return out, nil
}

// HarnessGone records that a session the dispatcher believed running in
// the caller's pod has no harness any more, and routes what waits for it.
func (s *Service) HarnessGone(ctx context.Context, c Caller, session, why string) error {
	if err := s.podOnly(c); err != nil {
		return err
	}
	x, ok, err := s.Store.GetSession(ctx, session)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: session %s", ErrUnknown, short(session))
	}
	if x.Agent != c.Agent {
		return fmt.Errorf("%w: session %s is agent %s's, and this pod is agent %s's", ErrForbidden, short(session), x.Agent, c.Agent)
	}
	if x.PodUID != c.PodUID {
		return fmt.Errorf("%w: session %s last ran in pod %s, not in this one", ErrForbidden, short(session), x.Pod)
	}
	if x.State == Ended {
		return nil
	}
	// What a wake handed it, with no word from it since, was handed to a
	// harness that had gone or was going: it is delivered once more. Only
	// here, where the pod's watcher saw the process gone; a session that
	// said it ended had the message. One it read or answered is left.
	for _, id := range x.Woke {
		if read, err := s.Store.OfTypeAbout(ctx, TypeRead, id); err != nil {
			return err
		} else if len(read) > 0 {
			continue
		}
		data, _ := json.Marshal(map[string]string{"session": SessionAddress(x.ID), "pod": x.Pod,
			"why": "it was handed by a wake to a harness that has gone from its pod, with no word from the session after; it is delivered once more"})
		if _, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeUndelivered + "/" + id, Type: TypeUndelivered, Source: Source, Subject: id, Data: data}); err != nil {
			return err
		}
	}
	x.State, x.Woke = Ended, nil
	if _, _, err := s.Store.PutSession(ctx, x); err != nil {
		return err
	}
	return s.ended(ctx, x, "its harness has gone from its pod ("+strings.TrimSpace(or(why, "no word"))+")")
}

// Hold records why a message for a session of the caller's agent still
// waits, so that its sender sees it (dispatcher message ID). Key names
// the reason, so that the same one is said once.
func (s *Service) Hold(ctx context.Context, c Caller, msg, key, reason string) error {
	if err := s.podOnly(c); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" || key == "" || !nameRe.MatchString(key) {
		return fmt.Errorf("%w: say why it waits, and a key of letters, digits, '.', '_' and '-'", ErrInvalid)
	}
	m, ok, err := s.message(ctx, msg)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no message %s", ErrUnknown, msg)
	}
	kind, rest := Split(m.To)
	if kind != "session" || !s.sessionOf(ctx, rest, c.Agent) {
		return fmt.Errorf("%w: message %s is not for a session of agent %s", ErrForbidden, msg, c.Agent)
	}
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	_, err = s.Store.Claim(ctx, s.waiting(m.ID, "attend-"+key, Wait{Reason: reason}))
	return err
}

// waitForOpen records that a message for a named session of a standing
// agent that is not open waits for the agent's pod to open it.
func (s *Service) waitForOpen(ctx context.Context, m Message, x Session) (string, error) {
	reason := fmt.Sprintf("for session %s of agent %s (%s/%s), which is not open; the agent's pod opens it again on its conversation "+
		"if it remembers it (agent-open), and it takes the message as it starts. If the pod does not, it says why here", short(x.ID), x.Agent, x.Agent, x.Name)
	_, err := s.Store.Claim(ctx, s.waiting(m.ID, "open", Wait{Reason: reason}))
	return reason, err
}
