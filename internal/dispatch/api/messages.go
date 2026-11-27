package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"carman/internal/dispatch/message"
)

// Messaging (ADR 0045). The owner's token speaks as the owner. An agent
// speaks with its pod's own token for the dispatcher, which says which pod
// and so which agent it is; the shared agent token says neither, and may
// not message.

type callerKey struct{}

func callerOf(r *http.Request) message.Caller {
	c, _ := r.Context().Value(callerKey{}).(message.Caller)
	return c
}

func (s *Server) messaging(r *http.Request) (*message.Service, message.Caller, error) {
	if s.Messages == nil {
		return nil, message.Caller{}, fmt.Errorf("%w: this dispatcher carries no messages", errForbidden)
	}
	c := callerOf(r)
	if !c.Owner && !c.Host && c.Pod == "" {
		return nil, c, fmt.Errorf("%w: messages need the pod's own token for the dispatcher (DISPATCHER_IDENTITY), not the shared agent token", errForbidden)
	}
	return s.Messages, c, nil
}

func messageErr(err error) error {
	switch {
	case errors.Is(err, message.ErrForbidden):
		return fmt.Errorf("%w: %v", errForbidden, err)
	case errors.Is(err, message.ErrUnknown):
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return err
}

type SendRequest struct {
	To      string `json:"to,omitempty"`
	Body    string `json:"body"`
	Answers string `json:"answers,omitempty"`
	Session string `json:"session,omitempty"` // the sender's own session, if it is one
	// AgainAfterLimit: send it again once after the reset if its run stops
	// at the plan's usage limit.
	AgainAfterLimit bool `json:"again_after_limit,omitempty"`
	// Failed, for an answer: the run did not finish.
	Failed bool `json:"failed,omitempty"`
}

func (s *Server) send(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req SendRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.To == "" && req.Answers == "" {
		return nil, fmt.Errorf("%w: say who it is to, or which message it answers", errBadRequest)
	}
	res, err := m.SendWith(r.Context(), c, req.Session, req.To, req.Body, req.Answers, message.SendOptions{AgainAfterLimit: req.AgainAfterLimit})
	return res, messageErr(err)
}

func (s *Server) answer(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req SendRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := m.Answer(r.Context(), c, req.Session, r.PathValue("id"), req.Body, req.Failed)
	return res, messageErr(err)
}

func (s *Server) messageStatus(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	st, err := m.Status(r.Context(), c, r.PathValue("id"))
	return st, messageErr(err)
}

type TakeRequest struct {
	Session string   `json:"session"`
	How     string   `json:"how"`
	IDs     []string `json:"ids,omitempty"`
	Wait    string   `json:"wait,omitempty"`
}

func (s *Server) take(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req TakeRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	var wait time.Duration
	if req.Wait != "" {
		if wait, err = time.ParseDuration(req.Wait); err != nil || wait < 0 || wait > 48*time.Hour {
			return nil, fmt.Errorf("%w: wait is a duration up to 48h", errBadRequest)
		}
	}
	if req.How == "" {
		req.How = "inbox"
	}
	t, err := m.Take(r.Context(), c, req.Session, req.How, req.IDs, wait)
	return t, messageErr(err)
}

func (s *Server) inbox(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	items, err := m.Inbox(r.Context(), c, r.URL.Query().Get("session"))
	return items, messageErr(err)
}

type ReadRequest struct {
	Session string   `json:"session,omitempty"`
	IDs     []string `json:"ids"`
}

func (s *Server) read(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req ReadRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	done, err := m.Read(r.Context(), c, req.Session, req.IDs)
	return map[string]any{"read": done}, messageErr(err)
}

func (s *Server) presence(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var p message.Presence
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return map[string]string{"session": p.Session}, messageErr(m.Present(r.Context(), c, p))
}

type WatchRequest struct {
	Session string `json:"session,omitempty"`
	Target  string `json:"target"`
}

func (s *Server) watch(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req WatchRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	id, err := m.Watch(r.Context(), c, req.Session, req.Target)
	return map[string]string{"watch": id}, messageErr(err)
}

func (s *Server) peers(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	since := 7 * 24 * time.Hour
	if v := r.URL.Query().Get("since"); v != "" {
		if since, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("%w: since is a duration", errBadRequest)
		}
	}
	all := r.URL.Query().Get("all") != ""
	return m.Peers(r.Context(), c, since, all)
}

// who is the caller, for a client that wants to know what the dispatcher
// makes of its token.
func (s *Server) whoami(_ http.ResponseWriter, r *http.Request, role Role) (any, error) {
	c := callerOf(r)
	return map[string]any{"role": role, "owner": c.Owner, "host": c.Host, "agent": c.Agent, "pod": c.Pod}, nil
}

var _ = context.Background

// A standing agent's pod attending to its own sessions (ADR 0053).

type GoneRequest struct {
	Session string `json:"session"`
	Why     string `json:"why,omitempty"`
}

// LimitedRequest: a session's harness stopped at the plan's usage limit.
type LimitedRequest struct {
	Session string `json:"session"`
	Key     string `json:"key"`
	Words   string `json:"words"`
}

func (s *Server) limited(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req LimitedRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := m.HarnessLimited(r.Context(), c, req.Session, req.Key, req.Words)
	return map[string]int{"told": n}, messageErr(err)
}

type HoldRequest struct {
	Message string `json:"message"`
	Key     string `json:"key"`
	Reason  string `json:"reason"`
}

func (s *Server) attending(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	a, err := m.Attending(r.Context(), c)
	return a, messageErr(err)
}

func (s *Server) gone(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req GoneRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return map[string]string{"session": req.Session}, messageErr(m.HarnessGone(r.Context(), c, req.Session, req.Why))
}

func (s *Server) hold(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req HoldRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return map[string]string{"message": req.Message}, messageErr(m.Hold(r.Context(), c, req.Message, req.Key, req.Reason))
}

// LiftRequest names the sender whose limit on runs the owner lifts.
type LiftRequest struct {
	Sender string `json:"sender"`
}

func (s *Server) lift(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	m, c, err := s.messaging(r)
	if err != nil {
		return nil, err
	}
	var req LiftRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := m.Lift(r.Context(), c, req.Sender)
	return map[string]int{"started": n}, messageErr(err)
}
