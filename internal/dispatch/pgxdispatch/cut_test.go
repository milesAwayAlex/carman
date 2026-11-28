package pgxdispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// TestR2CutTurnIsToldToGoOn: a turn that ends on an error of the model's
// API (the harness's StopFailure hook says so; no Stop hook runs) leaves
// the session idle, is recorded, and the dispatcher tells the session to
// go on: after a minute, twice as long after each further cut in a row,
// and not after the fifth. A turn that ends well starts the count anew.
func TestR2CutTurnIsToldToGoOn(t *testing.T) {
	s, _, _ := messaging(t)
	now := clocked(s)
	ctx := context.Background()
	d := pod("desk", "pod-cut")
	say := func(state, cut string) {
		t.Helper()
		if err := s.Present(ctx, d, message.Presence{Session: "cut-1", Name: "work", Kind: "interactive", State: state, Cut: cut}); err != nil {
			t.Fatal(err)
		}
	}
	told := func() int { return len(fromDispatcherTo(t, s, "session/cut-1")) }
	resume := func() {
		t.Helper()
		if err := s.Resume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	say(message.Idle, "")
	say(message.Busy, "")
	say(message.Idle, "server_error: the proxy could not reach the model's API")
	if x, _, _ := s.Store.GetSession(ctx, "cut-1"); x.State != message.Idle || x.Cuts != 1 || !x.Resume {
		t.Fatalf("after a cut: %+v", x)
	}
	if ev := eventsOf(t, s, message.TypeSessionCut); len(ev) != 1 || !strings.Contains(ev[0], "server_error") || !strings.Contains(ev[0], `"cuts":1`) {
		t.Fatalf("recorded: %v", ev)
	}
	// Not at once: the error may last a moment.
	resume()
	if told() != 0 {
		t.Fatalf("told at once")
	}
	now.add(message.ResumeAfter + time.Second)
	resume()
	resume()
	body := fromDispatcherTo(t, s, "session/cut-1")
	if len(body) != 1 || !strings.Contains(body[0], "cut by an error of the model's API") || !strings.Contains(body[0], "Go on with what you were doing") {
		t.Fatalf("told: %v", body)
	}
	// The turn that message starts is cut too: twice as long.
	say(message.Busy, "")
	say(message.Idle, "server_error")
	now.add(message.ResumeAfter + time.Second)
	resume()
	if told() != 1 {
		t.Fatalf("the second time after one minute: %d", told())
	}
	now.add(message.ResumeAfter)
	resume()
	if told() != 2 {
		t.Fatalf("the second time after two minutes: %d", told())
	}
	// A turn that ends well: the next cut is the first again.
	say(message.Busy, "")
	say(message.Idle, "")
	say(message.Busy, "")
	say(message.Idle, "overloaded")
	if x, _, _ := s.Store.GetSession(ctx, "cut-1"); x.Cuts != 1 {
		t.Fatalf("after a turn that ended well, cuts: %d", x.Cuts)
	}
	now.add(message.ResumeAfter + time.Second)
	resume()
	if told() != 3 {
		t.Fatalf("after a new first cut: %d", told())
	}
	// Cut again and again: told five times in a row, and no more.
	for i := 2; i <= message.MaxCuts+2; i++ {
		say(message.Busy, "")
		say(message.Idle, "overloaded")
		now.add(time.Hour)
		resume()
	}
	if told() != 3+message.MaxCuts-1 {
		t.Fatalf("told %d times, want %d", told(), 3+message.MaxCuts-1)
	}
	if ev := eventsOf(t, s, message.TypeSessionCut); !strings.Contains(strings.Join(ev, "\n"), `"gave_up":true`) {
		t.Fatalf("giving up is not recorded: %v", ev)
	}
	// A person's line starts a turn; it ends well; all is as before.
	say(message.Busy, "")
	say(message.Idle, "")
	if x, _, _ := s.Store.GetSession(ctx, "cut-1"); x.Cuts != 0 || x.Resume {
		t.Fatalf("after it ended well: %+v", x)
	}
}
