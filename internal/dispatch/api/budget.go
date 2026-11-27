package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"carman/internal/dispatch/meter"
)

// usageKept is how many model.usage records are looked back over for the
// standing against limits: two days of a busy installation.
const usageKept = 5000

// limits is each agent's limit at the proxy: its definition's, and over
// it the owner's latest model.limit.
func (s *Server) limits(r *http.Request) (map[string]meter.AgentStanding, error) {
	out := map[string]meter.AgentStanding{}
	for a, l := range s.ProxyLimits {
		out[a] = meter.AgentStanding{Limit: l, By: "definition"}
	}
	set, err := s.Store.Last(r.Context(), meter.TypeModelLimit, 1000)
	if err != nil {
		return nil, err
	}
	for _, e := range set {
		if e.Source != string(RoleOwner) || e.Subject == "" {
			continue
		}
		var l struct {
			meter.Limit
			Definition bool `json:"definition"`
		}
		if json.Unmarshal(e.Data, &l) != nil {
			continue
		}
		if l.Definition {
			if d, ok := s.ProxyLimits[e.Subject]; ok {
				out[e.Subject] = meter.AgentStanding{Limit: d, By: "definition"}
			} else {
				delete(out, e.Subject)
			}
			continue
		}
		out[e.Subject] = meter.AgentStanding{Limit: l.Limit, By: "owner"}
	}
	return out, nil
}

// standing is each limited agent's standing by the log. The meter asks
// with meter=NAME&since=TIME, and what it counted itself since then is
// left out.
func (s *Server) standing(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, fmt.Errorf("%w: since is a time (RFC 3339): %v", errBadRequest, err)
		}
		since = t
	}
	name := r.URL.Query().Get("meter")
	limits, err := s.limits(r)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	var recs []meter.Record
	if len(limits) > 0 {
		evs, err := s.Store.Last(r.Context(), meter.TypeModelUsage, usageKept)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			var rec meter.Record
			if json.Unmarshal(e.Data, &rec) != nil || now.Sub(rec.To) > 48*time.Hour {
				continue
			}
			recs = append(recs, rec)
		}
	}
	return meter.Tally(limits, recs, now, func(rec meter.Record) bool {
		return name != "" && rec.Meter == name && !rec.From.Before(since)
	}), nil
}

// UsageLatest is when each model's standing was last read, since the
// account at the proxy was last changed.
type UsageLatest struct {
	Models map[string]time.Time `json:"models"`
}

// usageLatest lets the reading job leave out a model whose standing is
// fresh.
func (s *Server) usageLatest(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	rs, err := s.Store.Last(r.Context(), meter.TypeUsageObserved, 300)
	if err != nil {
		return nil, err
	}
	changed, err := s.Store.Last(r.Context(), "model.account", 1)
	if err != nil {
		return nil, err
	}
	var after int64
	if len(changed) > 0 {
		after = changed[len(changed)-1].Seq
	}
	out := UsageLatest{Models: map[string]time.Time{}}
	for _, e := range rs {
		if e.Seq <= after {
			continue
		}
		var rd struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(e.Data, &rd) != nil || rd.Model == "" {
			continue
		}
		if e.Time.After(out.Models[rd.Model]) {
			out.Models[rd.Model] = e.Time
		}
	}
	return out, nil
}
