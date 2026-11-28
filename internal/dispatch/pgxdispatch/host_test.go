package pgxdispatch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

// Sessions on the owner's own machine (ADR 0049): no pod, the host role's
// token, one place.

var host = message.Caller{Host: true, Agent: "host"}

func hostStart(t *testing.T, s *message.Service, session, name, state string) {
	t.Helper()
	if err := s.Present(context.Background(), host, message.Presence{Session: session, Name: name, Kind: "interactive", State: state}); err != nil {
		t.Fatal(err)
	}
}

// TestR2HostIdentity: a session on the owner's machine is the place's; a
// pod may not act as one, nor claim to be the place, and the place may not
// act as a pod's session. Its sessions are found as PLACE/NAME and stay
// running with no pod.
func TestR2HostIdentity(t *testing.T) {
	s, cl, _ := messaging(t)
	s.Place = "host"
	ctx := context.Background()
	a := pod("trial", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	hostStart(t, s, "h-1", "proj", message.Idle)

	if got, err := s.Resolve(ctx, "host/proj"); err != nil || got != "session/h-1" {
		t.Errorf("host/proj: %q, %v", got, err)
	}
	if _, err := s.Inbox(ctx, a, "h-1"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("a pod reading a host session's inbox: %v", err)
	}
	if _, err := s.Send(ctx, pod("host", "pod-x"), "", "trial", "I am the owner's machine", ""); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("a pod of an agent named for the place: %v", err)
	}
	if _, err := s.Take(ctx, host, "sess-a", "tool", nil, 0); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("the place taking a pod's session's messages: %v", err)
	}
	// The place itself sends nothing: an answer would have no session to
	// find (seen 2026-11-27: a reply from a session whose shell did not
	// know its id came "from agent/host").
	if _, err := s.Send(ctx, host, "", "trial", "from nobody in particular", ""); !errors.Is(err, message.ErrInvalid) {
		t.Errorf("the place sending with no session: %v", err)
	}
	// No pod, and the cluster says so of every pod: the sweep leaves it.
	if err := (&message.Router{Service: s, Logger: slog.Default()}).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	x, _, _ := s.Store.GetSession(ctx, "h-1")
	if run, _ := s.Running(ctx, x); !run {
		t.Errorf("a host session heard from just now is not running: %+v", x)
	}
	// A day without a word, and nobody listening: it has ended.
	s.Now = func() time.Time { return time.Now().Add(message.HostQuiet + time.Hour) }
	if err := (&message.Router{Service: s, Logger: slog.Default()}).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if x, _, _ = s.Store.GetSession(ctx, "h-1"); x.State != message.Ended {
		t.Errorf("a host session not heard from for a day: %+v", x)
	}
}

// TestR2HostRights: a session on the owner's machine writes to any agent
// and session; an answer to it comes back to it; an agent writes to it
// only when answering or listed under the place's writtenBy; a message to
// the owner is the owner's notice, and the owner's inbox is not the host's.
func TestR2HostRights(t *testing.T) {
	s, cl, inst := messaging(t)
	s.Place = "host"
	ctx := context.Background()
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "sess-d", "front", message.Idle)
	hostStart(t, s, "h-1", "proj", message.Busy)

	ask, err := s.Send(ctx, host, "h-1", "desk/front", "what is 2+2?", "")
	if err != nil || ask.State == "refused" {
		t.Fatalf("host to an agent's session, nothing listed: %+v, %v", ask, err)
	}
	if r, err := s.Send(ctx, host, "h-1", "trial", "run something", ""); err != nil || r.State == "refused" {
		t.Errorf("host to an agent by name: %+v, %v", r, err)
	}
	tk, err := s.Take(ctx, d, "sess-d", "wake", nil, 0)
	if err != nil || len(tk.Messages) != 1 || !strings.Contains(tk.Text, "not the owner") || !strings.Contains(tk.Text, "owner's own session proj") {
		t.Errorf("what desk is told: %+v, %v", tk, err)
	}
	ans, err := s.Send(ctx, d, "sess-d", "", "4", ask.ID)
	if err != nil || ans.State == "refused" || ans.To != "session/h-1" {
		t.Errorf("desk answers the host session: %+v, %v", ans, err)
	}
	tk, err = s.Take(ctx, host, "h-1", "tool", nil, 0)
	if err != nil || len(tk.Messages) != 1 || tk.Messages[0].ID != ans.ID {
		t.Errorf("the answer reaches the host session: %+v, %v", tk, err)
	}
	if r, err := s.Send(ctx, d, "sess-d", "host/proj", "unasked", ""); err != nil || r.State != "refused" {
		t.Errorf("desk writes to the host, not listed: %+v, %v", r, err)
	}
	s.Writers = map[string][]string{"host": {"desk"}}
	if r, err := s.Send(ctx, d, "sess-d", "host/proj", "listed", ""); err != nil || r.State == "refused" {
		t.Errorf("desk writes to the host, listed: %+v, %v", r, err)
	}

	toOwner, err := s.Send(ctx, host, "h-1", "owner", "for the owner", "")
	if err != nil || toOwner.To != message.Owner {
		t.Fatalf("host to the owner: %+v, %v", toOwner, err)
	}
	items, err := s.Inbox(ctx, host, "h-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.To == message.Owner {
			t.Errorf("the host's inbox shows the owner's message %s", it.ID)
		}
	}
	if _, err := s.Read(ctx, host, "h-1", []string{toOwner.ID}); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("the host marking the owner's message read: %v", err)
	}
	owner, err := s.Inbox(ctx, message.Caller{Owner: true}, "")
	if err != nil || len(owner) != 1 || owner[0].ID != toOwner.ID || !owner[0].Host {
		t.Errorf("the owner's inbox: %+v, %v", owner, err)
	}
	_ = inst
}

// TestR2HostWaits: a message to a session on the owner's machine that is
// not running starts nothing; it waits, the sender is told why, and the
// next session of that name there is handed it when it starts.
func TestR2HostWaits(t *testing.T) {
	s, cl, inst := messaging(t)
	s.Place = "host"
	s.Writers = map[string][]string{"host": {"trial"}}
	ctx := context.Background()
	a := pod("trial", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	hostStart(t, s, "h-old", "proj", message.Idle)
	hostStart(t, s, "h-other", "other", message.Idle)
	if err := s.Present(ctx, host, message.Presence{Session: "h-old", State: message.Ended}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(ctx, a, "sess-a", "host/proj", "are you there?", "")
	if err != nil || res.State != "waiting" || !strings.Contains(res.Detail, "cannot start a session there") || !strings.Contains(res.Detail, "host/proj") {
		t.Fatalf("send to a host session not running: %+v, %v", res, err)
	}
	if _, err := inst.Explain(ctx, message.TaskPrefix+res.ID); !errors.Is(err, dispatch.ErrNotFound) {
		t.Errorf("a task was made for a host session: %v", err)
	}
	// Another name there does not take it.
	if tk, err := s.Take(ctx, host, "h-other", "start", nil, 0); err != nil || len(tk.Messages) != 0 {
		t.Errorf("a session of another name took it: %+v, %v", tk, err)
	}
	hostStart(t, s, "h-new", "proj", message.Idle)
	tk, err := s.Take(ctx, host, "h-new", "start", nil, 0)
	if err != nil || len(tk.Messages) != 1 || tk.Messages[0].ID != res.ID {
		t.Fatalf("the next session called proj: %+v, %v", tk, err)
	}
	if r, err := s.Send(ctx, host, "h-new", "", "yes", res.ID); err != nil || r.To != "session/sess-a" {
		t.Errorf("its answer: %+v, %v", r, err)
	}
	// The one that was not running, resumed, has nothing left to take.
	hostStart(t, s, "h-old", "proj", message.Busy)
	if tk, err := s.Take(ctx, host, "h-old", "start", nil, 0); err != nil || len(tk.Messages) != 0 {
		t.Errorf("delivered twice: %+v, %v", tk, err)
	}
}

// TestR2HostAttendsToItsSessions: the owner's machine, with the place's
// token, is told which of its sessions are believed to run and which names
// messages wait for, says one has gone, and says why a message waits. A
// pod is told nothing of the place and may say nothing of it, and the
// place nothing of a pod's sessions.
func TestR2HostAttendsToItsSessions(t *testing.T) {
	s, cl, inst := messaging(t)
	s.Place = "host"
	ctx := context.Background()
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "sess-d", "front", message.Idle)
	hostStart(t, s, "h-1", "proj", message.Idle)
	hostStart(t, s, "h-2", "notes", message.Idle)

	at, err := s.Attending(ctx, host)
	if err != nil || len(at.Running) != 2 || len(at.Waiting) != 0 {
		t.Fatalf("what the place attends to: %+v, %v", at, err)
	}
	if at, _ := s.Attending(ctx, d); len(at.Running) != 1 || at.Running[0].ID != "sess-d" {
		t.Errorf("a pod is told of the place's sessions: %+v", at)
	}
	if err := s.HarnessGone(ctx, d, "h-1", "x"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("a pod said a session on the owner's machine had gone: %v", err)
	}
	if err := s.HarnessGone(ctx, host, "sess-d", "x"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("the place said a pod's session had gone: %v", err)
	}
	if err := s.HarnessGone(ctx, message.Caller{Owner: true}, "h-1", "x"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("the owner's token attends: %v", err)
	}

	// Its harness has gone: ended, with why, and a message for it waits for
	// the next session of its name, which the place is told.
	if err := s.HarnessGone(ctx, host, "h-2", "its process is not there"); err != nil {
		t.Fatal(err)
	}
	x, _, _ := inst.GetSession(ctx, "h-2")
	if x.State != message.Ended {
		t.Errorf("not ended: %+v", x)
	}
	recs, _ := inst.OfTypeAbout(ctx, message.TypeSessionEnded, "h-2")
	if len(recs) != 1 || !strings.Contains(string(recs[0].Data), "gone from the owner's machine") {
		t.Errorf("the end is not recorded with why: %d", len(recs))
	}
	res, err := s.Send(ctx, message.Caller{Owner: true}, "", "host/notes", "are you there?", "")
	if err != nil || !strings.Contains(res.Detail, "not running") {
		t.Fatalf("a message to it: %+v, %v", res, err)
	}
	at, err = s.Attending(ctx, host)
	if err != nil || len(at.Running) != 1 || len(at.Waiting) != 1 || at.Waiting[0].Name != "notes" || len(at.Waiting[0].Messages) != 1 {
		t.Fatalf("after it went: %+v, %v", at, err)
	}
	if err := s.Hold(ctx, host, res.ID, "host-notice", "a notice was shown on the owner's machine"); err != nil {
		t.Errorf("the place saying why it waits: %v", err)
	}
	if err := s.Hold(ctx, d, res.ID, "host-notice", "x"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("a pod saying why the place's message waits: %v", err)
	}
	// A session of the name starts: it takes the message, and nothing waits.
	hostStart(t, s, "h-3", "notes", message.Busy)
	if taken, err := s.Take(ctx, host, "h-3", "prompt", nil, 0); err != nil || len(taken.Messages) != 1 {
		t.Errorf("the next session of the name: %+v, %v", taken, err)
	}
	if at, _ := s.Attending(ctx, host); len(at.Waiting) != 0 {
		t.Errorf("still waited for: %+v", at.Waiting)
	}
}
