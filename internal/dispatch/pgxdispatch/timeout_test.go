package pgxdispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// TestCallTimeout: a starter or an admission that hangs counts as failed
// for the round, the loop goes on with other tasks, and the call is made
// again later.
func TestCallTimeout(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	s := newFake()
	startHang, admitHang := make(chan struct{}), make(chan struct{})
	s.hang["slow"] = startHang
	s.hang["admit:stuck"] = admitHang
	defer func() {
		select {
		case <-startHang:
		default:
			close(startHang)
		}
		select {
		case <-admitHang:
		default:
			close(admitHang)
		}
	}()
	inst, err := OpenWith(ctx, d.url, s.config(4), "b1", Options{CallTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	tr := dispatch.TransitionID
	submit(t, inst, "slow", 9)
	submit(t, inst, "stuck", 8)
	submit(t, inst, "fast", 1)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "fast", "fast#1"))
	eventually(t, "the hung calls to show as failed", func() bool {
		a, _ := inst.Explain(ctx, "slow")
		b, _ := inst.Explain(ctx, "stuck")
		return a.Reason.Kind == dispatch.ReasonStartFailed && strings.Contains(a.Reason.Detail, "in time") &&
			b.Reason.Kind == dispatch.ReasonAdmission && strings.Contains(b.Reason.Detail, "in time")
	})
	s.mu.Lock()
	delete(s.hang, "slow")
	delete(s.hang, "admit:stuck")
	s.mu.Unlock()
	close(startHang)
	close(admitHang)
	waitEvent(t, inst, tr(dispatch.TypeStarted, "slow", "slow#1"))
	waitEvent(t, inst, tr(dispatch.TypeStarted, "stuck", "stuck#1"))
	if n := s.startsOf("slow#1"); n < 2 {
		t.Errorf("slow#1 was started %d times, want the hung call and at least one more", n)
	}
}
