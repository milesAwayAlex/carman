package api

import (
	"errors"
	"fmt"
	"net/http"

	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/message"
)

// What the model's usage is for (ADR new-the-plan-of-the-models-budget):
// the owner's priorities, their shares and the work under each. Who may
// do what is the budget package's to say.

func (s *Server) budgeting(r *http.Request) (*budget.Service, message.Caller, error) {
	if s.Budget == nil {
		return nil, message.Caller{}, fmt.Errorf("%w: this dispatcher keeps no priorities", errForbidden)
	}
	return s.Budget, callerOf(r), nil
}

func budgetErr(err error) error {
	if errors.Is(err, budget.ErrForbidden) {
		return fmt.Errorf("%w: %v", errForbidden, err)
	}
	return err
}

func (s *Server) priorities(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	b, c, err := s.budgeting(r)
	if err != nil {
		return nil, err
	}
	out, err := b.View(r.Context(), c)
	return out, budgetErr(err)
}

func (s *Server) prioritySet(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	b, c, err := s.budgeting(r)
	if err != nil {
		return nil, err
	}
	var p budget.Priority
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	out, err := b.Set(r.Context(), c, p)
	return out, budgetErr(err)
}

func (s *Server) priorityItem(_ http.ResponseWriter, r *http.Request, _ Role) (any, error) {
	b, c, err := s.budgeting(r)
	if err != nil {
		return nil, err
	}
	var ch budget.Change
	if err := decode(r, &ch); err != nil {
		return nil, err
	}
	out, err := b.Change(r.Context(), c, ch)
	return out, budgetErr(err)
}
