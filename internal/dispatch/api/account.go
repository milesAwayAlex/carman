package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
)

// Which of the model's accounts the proxy holds, said from inside the
// cluster (ADR 0078, the manual mode): the owner, a session on the owner's
// machine and the agents the installation names may say; the meter follows
// what the log then names. No credential passes here: the meter has them.

// AccountHeld is which account the log names, and what the meter says.
type AccountHeld struct {
	Held  string    `json:"held,omitempty"`
	Since time.Time `json:"since,omitzero"`
	By    string    `json:"by,omitempty"`
	Why   string    `json:"why,omitempty"`
	// What the proxy's meter last said: the accounts it has a credential
	// for, the one it holds, and when it said so.
	Has   []string  `json:"has,omitempty"`
	Holds string    `json:"holds,omitempty"`
	Said  time.Time `json:"said,omitzero"`
	// Busy are the sessions in the middle of a turn: a change of account
	// loses the prompt cache of every live conversation, theirs at once.
	Busy []string `json:"busy,omitempty"`
}

// AccountRequest asks for another account to be held.
type AccountRequest struct {
	Account string `json:"account"`
	Why     string `json:"why"`
	// Now: though sessions are in the middle of a turn.
	Now bool `json:"now,omitempty"`
	// Session is the asker's own, which is in a turn by asking.
	Session string `json:"session,omitempty"`
}

func (s *Server) mayHold(c message.Caller) bool {
	return c.Owner || c.Host || (c.Agent != "" && c.Pod != "" && slices.Contains(s.AccountAgents, c.Agent))
}

func (s *Server) accountNow(r *http.Request, own string) (AccountHeld, error) {
	var out AccountHeld
	last, err := s.Store.Last(r.Context(), meter.TypeModelAccount, 1)
	if err != nil {
		return out, err
	}
	if len(last) > 0 {
		e := last[len(last)-1]
		var d struct{ By, Why string }
		_ = json.Unmarshal(e.Data, &d)
		out.Held, out.Since, out.By, out.Why = e.Subject, e.Time, d.By, d.Why
		if out.By == "" {
			out.By = e.Source
		}
	}
	s.metered.Lock()
	out.Has, out.Holds, out.Said = slices.Clone(s.meterHas), s.meterHolds, s.meterSaid
	s.metered.Unlock()
	if s.Messages != nil {
		busy, err := s.Messages.InTurn(r.Context())
		if err != nil {
			return out, err
		}
		for _, x := range busy {
			if x.ID == own {
				continue
			}
			n := x.Agent + "/" + x.Name
			if x.Name == "" {
				n = "session/" + x.ID
			}
			out.Busy = append(out.Busy, n)
		}
	}
	return out, nil
}

func (s *Server) accountHeld(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	c := callerOf(r)
	if !c.Owner && !c.Host && c.Pod == "" {
		return nil, fmt.Errorf("%w: which account the proxy holds is told to the owner, the owner's machine and agents in their pods", errForbidden)
	}
	return s.accountNow(r, r.URL.Query().Get("session"))
}

func (s *Server) accountHold(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	c := callerOf(r)
	if !s.mayHold(c) {
		return nil, fmt.Errorf("%w: which account the proxy holds is said by the owner, a session on the owner's machine, and the agents the installation names for it (egress.model.accounts.agents: %s); %s is none of them",
			errForbidden, orNoAgents(s.AccountAgents), c)
	}
	var req AccountRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Why) == "" {
		return nil, fmt.Errorf("%w: say why: it is recorded with the change", errBadRequest)
	}
	now, err := s.accountNow(r, req.Session)
	if err != nil {
		return nil, err
	}
	switch {
	case len(now.Has) == 0 || time.Since(now.Said) > 5*time.Minute:
		return nil, fmt.Errorf("%w: the proxy's meter has not said which accounts it has a credential for (it says so every quarter of a minute where egress.model.accounts.mode is manual); nothing is changed", errBadRequest)
	case !slices.Contains(now.Has, req.Account):
		return nil, fmt.Errorf("%w: the proxy has no credential for an account %q; it has: %s", errBadRequest, req.Account, strings.Join(now.Has, ", "))
	case req.Account == now.Held && req.Account == now.Holds:
		return nil, fmt.Errorf("%w: the proxy holds account %s already, since %s", errBadRequest, now.Held, now.Since.UTC().Format("2006-01-02 15:04 UTC"))
	case len(now.Busy) > 0 && !req.Now:
		return nil, fmt.Errorf("%w: a change of account loses the prompt cache of every live conversation, and these sessions are in the middle of a turn: %s. Wait for them, or say --now", errHeld, strings.Join(now.Busy, ", "))
	}
	by := message.Owner
	switch {
	case c.Host:
		by = "place/" + c.Agent
	case !c.Owner:
		by = "agent/" + c.Agent
	}
	from := now.Held
	if from == "" {
		from = now.Holds // the log named none yet: what the meter holds
	}
	data, _ := json.Marshal(map[string]any{"from": from, "to": req.Account, "by": by, "why": req.Why, "rule": "told", "session": req.Session, "in_turn": now.Busy})
	t := time.Now().UTC()
	if err := s.Store.Emit(r.Context(), dispatch.Event{ID: meter.TypeModelAccount + "/" + req.Account + "/" + t.Format(time.RFC3339Nano),
		Type: meter.TypeModelAccount, Source: "dispatcher", Subject: req.Account, Time: t, Data: data}); err != nil {
		return nil, err
	}
	now.Held, now.Since, now.By, now.Why = req.Account, t, by, req.Why
	return now, nil
}

func orNoAgents(a []string) string {
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, ", ")
}
