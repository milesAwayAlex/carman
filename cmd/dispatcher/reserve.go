package main

import (
	"context"
	"fmt"

	"carman/internal/dispatch"
)

// reserveRule keeps Places of MaxRunning for work of priority From or more
// (ADR 0059): work below it takes at most MaxRunning-Places, so that a
// scheduled job does not wait behind long work.
type reserveRule struct {
	Places, From, MaxRunning int
}

func (r reserveRule) check() error {
	if r.Places < 0 || (r.Places > 0 && r.Places >= r.MaxRunning) {
		return fmt.Errorf("DISPATCHER_RESERVE_PLACES is %d: it has to leave work below priority %d at least one of the %d places", r.Places, r.From, r.MaxRunning)
	}
	return nil
}

func (r reserveRule) admit(_ context.Context, task dispatch.TaskSpec, running []dispatch.TaskSpec) (bool, string) {
	if r.Places == 0 || task.Priority >= r.From {
		return true, ""
	}
	below := 0
	for _, t := range running {
		if t.Priority < r.From {
			below++
		}
	}
	if below < r.MaxRunning-r.Places {
		return true, ""
	}
	return false, fmt.Sprintf("%d of %d places run work below priority %d, and %d are kept for work of that priority or more", below, r.MaxRunning, r.From, r.Places)
}

// admitAll admits a task that every rule admits, asking them in order and
// giving the first refusal.
func admitAll(rules ...dispatch.Admission) dispatch.Admission {
	var rs []dispatch.Admission
	for _, r := range rules {
		if r != nil {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return nil
	}
	return func(ctx context.Context, task dispatch.TaskSpec, running []dispatch.TaskSpec) (bool, string) {
		for _, r := range rs {
			if ok, why := r(ctx, task, running); !ok {
				return false, why
			}
		}
		return true, ""
	}
}
