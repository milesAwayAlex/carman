package message

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Who may write to an agent and its sessions (ADR 0047): the owner; the
// agent's own sessions; a session answering a message it was sent; and
// the agents its definition lists (writtenBy). Nobody else.

// ParseWriters reads the lists the installation gives (a JSON object:
// agent name to the agents that may write to it).
func ParseWriters(raw string) (map[string][]string, error) {
	out := map[string][]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("the lists of who may write to each agent: %w", err)
	}
	return out, nil
}

// targetAgent is the agent an address belongs to; empty for the owner.
func (s *Service) targetAgent(ctx context.Context, to string) (string, error) {
	kind, rest := Split(to)
	switch kind {
	case Owner:
		return "", nil
	case "agent":
		return rest, nil
	}
	x, ok, err := s.Store.GetSession(ctx, rest)
	if err != nil || !ok {
		return "", err
	}
	return x.Agent, nil
}

// mayWrite says whether agent from may write to agent target, leaving
// answers aside.
func (s *Service) mayWrite(from, target string) bool {
	// The owner's own sessions write as the owner may (ADR 0049).
	return target == "" || from == target || s.isPlace(from) || slices.Contains(s.Writers[target], from)
}

// answering says whether a message from the caller to to answers a
// message the caller was sent, back to whoever sent it.
func (s *Service) answering(ctx context.Context, c Caller, to, answers string) (bool, error) {
	if answers == "" {
		return false, nil
	}
	orig, ok, err := s.message(ctx, answers)
	if err != nil || !ok || orig.From != to {
		return false, err
	}
	got, err := s.targetAgent(ctx, orig.To)
	return got == c.Agent && got != "", err
}

// refusal says who may write to an agent, for a sender that may not.
func (s *Service) refusal(from, target string) string {
	who := "the owner and its own sessions"
	if w := s.Writers[target]; len(w) > 0 {
		who = "the owner, its own sessions and agents " + strings.Join(w, ", ")
	}
	return fmt.Sprintf("agent %s, and its sessions, may be written to only by %s, and not by agent %s. "+
		"You may still answer a message it sends you. To have %s do something, ask the owner (dispatcher send owner 'TEXT'), "+
		"or an agent that may write to it; to be allowed, the owner adds %s to its writtenBy",
		target, who, from, target, from)
}
