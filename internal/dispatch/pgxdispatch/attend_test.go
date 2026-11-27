package pgxdispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"carman/internal/dispatch/message"
)

// A standing agent's pod attends to its own sessions (ADR 0053).

func waitReasons(t *testing.T, s *message.Service, id string) string {
	t.Helper()
	st, err := s.Status(context.Background(), message.Caller{Owner: true}, id)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range st.Events {
		if e.Type == message.TypeWaiting {
			b.Write(e.Data)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestR2StandingHarnessGone: a session the dispatcher believes idle in a
// pod that stays is said by the pod to have no harness; it has ended, what
// is sent to it waits for the pod to open it again, the pod is told which
// name to open, and the session of that name opened again takes it.
func TestR2StandingHarnessGone(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "s-old", "vpn", message.Idle)
	start(t, s, cl, d, "s-other", "notes", message.Busy)

	a, err := s.Attending(ctx, d)
	if err != nil || len(a.Running) != 2 || len(a.Waiting) != 0 {
		t.Fatalf("before: %+v, %v", a, err)
	}
	early, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/vpn", "are you there?", "")
	if err != nil || !strings.Contains(early.Detail, "idle") {
		t.Fatalf("to the session believed idle: %+v, %v", early, err)
	}
	if err := s.HarnessGone(ctx, d, "s-old", "no process"); err != nil {
		t.Fatal(err)
	}
	x, _, _ := s.Store.GetSession(ctx, "s-old")
	if x.State != message.Ended {
		t.Errorf("a session whose harness has gone: %+v", x)
	}
	if w := waitReasons(t, s, early.ID); !strings.Contains(w, "not open") {
		t.Errorf("the message sent before its end is not routed as to a session not open: %s", w)
	}
	late, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/vpn", "and now?", "")
	if err != nil || !strings.Contains(late.Detail, "not open") || !strings.Contains(late.Detail, "agent-open") {
		t.Errorf("to the session that ended: %+v, %v", late, err)
	}
	a, err = s.Attending(ctx, d)
	if err != nil || len(a.Running) != 1 || len(a.Waiting) != 1 || a.Waiting[0].Name != "vpn" || len(a.Waiting[0].Messages) != 2 {
		t.Fatalf("after: %+v, %v", a, err)
	}
	// Opened again, the harness starts a new conversation of that name.
	start(t, s, cl, d, "s-new", "vpn", message.Idle)
	tk, err := s.Take(ctx, d, "s-new", "start", nil, 0)
	if err != nil || len(tk.Messages) != 2 {
		t.Errorf("the session opened again takes what waited: %+v, %v", tk, err)
	}
	if a, _ = s.Attending(ctx, d); len(a.Waiting) != 0 {
		t.Errorf("nothing waits once taken: %+v", a)
	}
	// Saying so again changes nothing.
	if err := s.HarnessGone(ctx, d, "s-old", "again"); err != nil {
		t.Error(err)
	}
}

// TestR2StandingAttendIsThePodsOwn: a pod attends only to its own agent's
// sessions in itself: another agent's pod, another pod of the same agent
// that is alive, the owner and the owner's machine are refused, and a
// pod is not told of another agent's sessions.
func TestR2StandingAttendIsThePodsOwn(t *testing.T) {
	s, cl, _ := messaging(t)
	s.Place = "host"
	ctx := context.Background()
	d := pod("desk", "pod-d")
	tr := pod("trial", "pod-t")
	start(t, s, cl, d, "s-d", "vpn", message.Idle)
	start(t, s, cl, tr, "s-t", "x", message.Busy)
	m, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/vpn", "hello", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.HarnessGone(ctx, tr, "s-d", ""); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another agent's pod ending desk's session: %v", err)
	}
	if err := s.Hold(ctx, tr, m.ID, "x", "because"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another agent's pod saying why desk's message waits: %v", err)
	}
	d2 := pod("desk", "pod-d2")
	cl.set("pod-d2", true)
	if err := s.HarnessGone(ctx, d2, "s-d", ""); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another pod of the agent ending a session of this one: %v", err)
	}
	for _, c := range []message.Caller{{Owner: true}, {Host: true, Agent: "host"}} {
		if _, err := s.Attending(ctx, c); !errors.Is(err, message.ErrForbidden) {
			t.Errorf("%v attending: %v", c, err)
		}
		if err := s.HarnessGone(ctx, c, "s-d", ""); !errors.Is(err, message.ErrForbidden) {
			t.Errorf("%v ending a session: %v", c, err)
		}
	}
	a, err := s.Attending(ctx, tr)
	if err != nil || len(a.Running) != 1 || a.Running[0].ID != "s-t" {
		t.Errorf("trial is told only of its own: %+v, %v", a, err)
	}
	if x, _, _ := s.Store.GetSession(ctx, "s-d"); x.State == message.Ended {
		t.Errorf("a refused call ended the session: %+v", x)
	}
}

// TestR2StandingNotRemembered: a message for a session the pod does not
// open (forgotten there) waits, and its sender reads why.
func TestR2StandingNotRemembered(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "s-d", "old", message.Idle)
	if err := s.Present(ctx, d, message.Presence{Session: "s-d", State: "ended"}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/old", "still there?", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Hold(ctx, d, m.ID, "forgotten", "session old is not remembered in pod pod-d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Hold(ctx, d, m.ID, "bad key!", "x"); !errors.Is(err, message.ErrInvalid) {
		t.Errorf("a key that is no name: %v", err)
	}
	if w := waitReasons(t, s, m.ID); !strings.Contains(w, "not remembered") {
		t.Errorf("the sender is not told why it waits: %s", w)
	}
}
