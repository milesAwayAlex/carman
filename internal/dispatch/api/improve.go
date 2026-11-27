package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"carman/internal/dispatch/improve"
	"carman/internal/dispatch/message"
)

// The list of improvements to the platform, its evidence and its gate
// (ADR 0058). Who may do what is the improve package's to say: the owner,
// the owner's machine, and agents known by their pod's own token.

func (s *Server) improving(r *http.Request) (*improve.Service, message.Caller, error) {
	if s.Improve == nil {
		return nil, message.Caller{}, fmt.Errorf("%w: this dispatcher keeps no list of improvements", errForbidden)
	}
	return s.Improve, callerOf(r), nil
}

func improveErr(err error) error {
	switch {
	case errors.Is(err, improve.ErrForbidden):
		return fmt.Errorf("%w: %v", errForbidden, err)
	case errors.Is(err, improve.ErrHeld):
		return fmt.Errorf("%w%s", errHeld, strings.TrimPrefix(err.Error(), improve.ErrHeld.Error()))
	}
	return messageErr(err)
}

func (s *Server) improvements(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	out, err := im.List(r.Context(), c, r.URL.Query().Get("state"))
	return out, improveErr(err)
}

func (s *Server) improvement(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	out, err := im.Get(r.Context(), c, r.PathValue("id"))
	return out, improveErr(err)
}

func (s *Server) improvementAdd(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	var n improve.Note
	if err := decode(r, &n); err != nil {
		return nil, err
	}
	out, err := im.Add(r.Context(), c, n)
	return out, improveErr(err)
}

func (s *Server) improvementChange(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	var ch improve.Change
	if err := decode(r, &ch); err != nil {
		return nil, err
	}
	out, err := im.Change(r.Context(), c, r.PathValue("id"), ch)
	return out, improveErr(err)
}

// BriefRequest asks for a brief to be handed over.
type BriefRequest struct {
	To      string `json:"to"`
	Text    string `json:"text"`
	Session string `json:"session,omitempty"`
}

func (s *Server) improvementBrief(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	var req BriefRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out, err := im.Brief(r.Context(), c, req.Session, r.PathValue("id"), req.To, req.Text)
	return out, improveErr(err)
}

func (s *Server) evidence(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	if v := q.Get("event"); v != "" {
		seq, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: event is a place in the log, a number", errBadRequest)
		}
		out, err := im.EvidenceAt(r.Context(), c, seq)
		return out, improveErr(err)
	}
	after := int64(-1)
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return nil, fmt.Errorf("%w: after is a place in the log, a number", errBadRequest)
		}
	}
	limit, err := intParam(r, "limit", 200, 1000)
	if err != nil {
		return nil, err
	}
	width, err := intParam(r, "width", 400, 0)
	if err != nil {
		return nil, err
	}
	out, err := im.Evidence(r.Context(), c, after, int(limit), int(width))
	return out, improveErr(err)
}

// SurveyRequest is the surveyor's word that it read the evidence.
type SurveyRequest struct {
	Through int64  `json:"through"`
	Report  string `json:"report,omitempty"`
	Session string `json:"session,omitempty"`
}

func (s *Server) surveyed(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	im, c, err := s.improving(r)
	if err != nil {
		return nil, err
	}
	var req SurveyRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out, err := im.Surveyed(r.Context(), c, req.Session, req.Through, req.Report)
	return out, improveErr(err)
}
