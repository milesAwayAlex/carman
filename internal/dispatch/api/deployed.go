package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/improve"
)

// Deployed is the latest deploy the log records (installation.deployed):
// the commit of the platform running, since when, and the subjects of the
// commits it brought. Only what a public repository's log says.
type Deployed struct {
	Commit   string    `json:"commit"`
	Previous string    `json:"previous,omitempty"`
	Since    time.Time `json:"since"`
	Commits  []string  `json:"commits,omitempty"`
	More     int       `json:"more,omitempty"`
}

func (s *Server) deployed(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	rs, err := s.Store.Last(r.Context(), improve.TypeDeployed, 1)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("%w: no deploy is recorded in the dispatcher's log", dispatch.ErrNotFound)
	}
	e := rs[len(rs)-1]
	var d improve.Deployed
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return nil, fmt.Errorf("the latest %s cannot be read: %w", improve.TypeDeployed, err)
	}
	return Deployed{Commit: d.Commit, Previous: d.Previous, Since: e.Time, Commits: d.Commits, More: d.More}, nil
}
