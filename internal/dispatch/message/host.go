package message

import (
	"context"
	"fmt"
	"time"
)

// Sessions on the owner's own machine (ADR 0049): outside the cluster, with
// no pod, proved by the token of the host role, which stands for one place.
// Every session there is of the agent named for the place.

// HostQuiet is how long a session on the owner's machine that is not
// listening may go unheard from before it is taken to have ended: a day.
// An idle session keeps a waiter (for up to fourteen days), so one that is
// neither listening nor heard from has most likely gone.
const HostQuiet = 24 * time.Hour

func (s *Service) isPlace(agent string) bool { return s.Place != "" && agent == s.Place }

// hostRunning says whether a session on the owner's machine runs: it has
// not said it ended, and its waiter listens or a hook was heard lately.
// No pod tells.
func (s *Service) hostRunning(x Session) bool {
	if x.State == Ended {
		return false
	}
	return listening.listening(x.ID) || s.now().Sub(x.Seen) < HostQuiet
}

// waitForHost records why a message for a session on the owner's machine
// that is not running waits: nothing can start one there.
func (s *Service) waitForHost(ctx context.Context, m Message, x Session) (string, error) {
	called := ""
	if x.Name != "" {
		called = fmt.Sprintf(", or when a session called %s next starts there (%s/%s)", x.Name, x.Agent, x.Name)
	}
	reason := fmt.Sprintf("for session %s on the owner's machine (%s), which is not running; a message cannot start a session there, "+
		"so it waits, and is delivered when that session is resumed%s", short(x.ID), x.Agent, called)
	_, err := s.Store.Claim(ctx, s.waiting(m.ID, "host", Wait{Reason: reason}))
	return reason, err
}

// heirs are the addresses whose messages a session on the owner's machine
// takes besides its own: the earlier sessions there of the same name that
// no longer run, or are idle with nobody listening (a harness that died
// without a word). A name is how a person finds a session again; its id
// changes with every new conversation.
func (s *Service) heirs(ctx context.Context, c Caller, session string) ([]string, error) {
	if !c.Host || session == "" {
		return nil, nil
	}
	me, ok, err := s.Store.GetSession(ctx, session)
	if err != nil || !ok || me.Name == "" {
		return nil, err
	}
	all, err := s.Store.Sessions(ctx, c.Agent, s.now().Add(-s.keep()))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, x := range all {
		if x.ID == session || x.Name != me.Name {
			continue
		}
		if !s.hostRunning(x) || (x.State == Idle && !listening.listening(x.ID)) {
			out = append(out, SessionAddress(x.ID))
		}
	}
	return out, nil
}

func (s *Service) keep() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return 30 * 24 * time.Hour
}
