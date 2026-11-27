package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"carman/internal/dispatch/trigger"
)

// Events from outside (ADR 0063): what a watch's job saw, and what a hook
// was told.

// observed takes a watch's observation, from the owner or from that
// watch's own job, which the cluster vouches for by its pod.
func (s *Server) observed(_ http.ResponseWriter, r *http.Request, role Role) (any, error) {
	if s.Outside == nil {
		return nil, fmt.Errorf("%w: this dispatcher has no watches", errForbidden)
	}
	name := r.PathValue("name")
	if c := callerOf(r); role != RoleOwner && (c.Pod == "" || c.Agent != trigger.WatcherName(name)) {
		return nil, fmt.Errorf("%w: what the watch %s saw is said by its own job (%s, known by its pod's token), not by %s", errForbidden, name, trigger.WatcherName(name), c)
	}
	var ob trigger.Observation
	if err := decode(r, &ob); err != nil {
		return nil, err
	}
	changed, err := s.Outside.Observed(r.Context(), name, ob)
	if errors.Is(err, trigger.ErrUnknown) {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return map[string]bool{"changed": changed}, err
}

// hook takes a call from outside. It says as little as it can: 204 when
// the call was signed and recorded, 403 otherwise, whether or not there is
// such a hook.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || s.Outside == nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	header := "X-Hub-Signature-256"
	if h, ok := s.Outside.Hooks[name]; ok && h.Header != "" {
		header = h.Header
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		delivery = r.Header.Get("X-Delivery")
	}
	id, err := s.Outside.Received(r.Context(), name, r.Header.Get(header), delivery, body)
	switch {
	case errors.Is(err, trigger.ErrUnknown), errors.Is(err, trigger.ErrForbidden):
		s.Log.Warn("a call at a hook was refused", "hook", name, "error", err)
		w.WriteHeader(http.StatusForbidden)
	case err != nil:
		s.Log.Error("a call at a hook failed", "hook", name, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}
