package memory

import (
	"context"
	"os"
	"testing"

	"carman/internal/dispatch"
	"carman/internal/dispatch/dispatchtest"
)

// RestartEnv, when set, runs the restart tests too; they must fail here,
// since this implementation keeps nothing across a restart.
const RestartEnv = "DISPATCH_TEST_MEMORY_RESTART"

// db opens every instance on a fresh store, so nothing outlives an instance.
type db struct{}

func (db) Open(cfg dispatch.Config, build string) (dispatchtest.Instance, error) {
	return New(cfg), nil
}

// Emit while down goes nowhere.
func (db) Emit(ctx context.Context, e dispatch.Event) error { return nil }

func factory(t *testing.T) dispatchtest.Database { return db{} }

// shared opens every instance on one store, as if it were a perfect
// database.
type shared struct{ *Store }

func (s shared) Open(cfg dispatch.Config, build string) (dispatchtest.Instance, error) {
	return Open(s.Store, cfg), nil
}

func TestAcceptance(t *testing.T) {
	if os.Getenv(RestartEnv) != "" {
		dispatchtest.Run(t, factory)
	} else {
		dispatchtest.RunWithoutRestart(t, factory)
	}
}

// TestAcceptanceSharedStore shows that the restart tests can be passed:
// with state that outlives an instance, the whole suite passes (R6).
func TestAcceptanceSharedStore(t *testing.T) {
	dispatchtest.Run(t, func(t *testing.T) dispatchtest.Database { return shared{NewStore()} })
}
