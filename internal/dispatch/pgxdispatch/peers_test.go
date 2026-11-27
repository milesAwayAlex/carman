package pgxdispatch

import (
	"context"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// What `peers` lists: who is worth writing to, and what a message does.

func peers(t *testing.T, s *message.Service, c message.Caller, all bool) map[string]message.Peer {
	t.Helper()
	ps, err := s.Peers(context.Background(), c, time.Hour, all)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]message.Peer{}
	for _, p := range ps {
		out[p.Address] = p
	}
	return out
}

// run is one run of an agent in a pod of its own, which has ended.
func run(t *testing.T, s *message.Service, cl *fakeCluster, agent, session, name string) {
	t.Helper()
	p := pod(agent, "pod-"+session)
	start(t, s, cl, p, session, name, message.Busy)
	cl.set(p.Pod, false)
}

// TestR2PeersFinishedRunsDoNotCrowd: running sessions are listed, first;
// of those not running only the latest of each agent and name, or one a
// message was exchanged with; --all lists every one.
func TestR2PeersFinishedRunsDoNotCrowd(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	owner := message.Caller{Owner: true}
	run(t, s, cl, "trial", "run-1", "")
	run(t, s, cl, "trial", "run-2", "")
	run(t, s, cl, "trial", "run-3", "")
	run(t, s, cl, "trial", "old-alpha", "alpha")
	start(t, s, cl, pod("trial", "pod-live"), "live", "beta", message.Busy)
	run(t, s, cl, "trial", "run-4", "")
	if res, err := s.Send(ctx, owner, "", "session/run-1", "how did it go?", ""); err != nil || res.State == "refused" {
		t.Fatalf("a message to an old run: %+v, %v", res, err)
	}

	got := peers(t, s, owner, false)
	for _, want := range []string{"session/live", "session/run-4", "session/old-alpha", "session/run-1"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s is not listed", want)
		}
	}
	for _, hidden := range []string{"session/run-2", "session/run-3"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("%s, an older finished run nobody wrote to, is listed", hidden)
		}
	}
	ps, err := s.Peers(ctx, owner, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range ps {
		if p.Kind == "session" {
			order = append(order, p.Address)
		}
	}
	if len(order) == 0 || order[0] != "session/live" || !got["session/live"].Running {
		t.Errorf("running sessions first: %v", order)
	}
	if len(order) > 1 && order[1] != "session/run-4" {
		t.Errorf("then the one seen last: %v", order)
	}
	if every := peers(t, s, owner, true); len(every) != len(got)+2 {
		t.Errorf("all lists %d peers, want %d", len(every), len(got)+2)
	}
}

// TestR2PeersAgentsGone: an agent the cluster does not define is listed
// only while a session of it that the list shows runs; its finished
// sessions are not listed, for a message reaches nobody there.
func TestR2PeersAgentsGone(t *testing.T) {
	s, cl, _ := messaging(t)
	owner := message.Caller{Owner: true}
	run(t, s, cl, "trial-old", "gone-1", "x")
	start(t, s, cl, pod("trial-e", "pod-e"), "e-1", "y", message.Idle)

	got := peers(t, s, owner, false)
	if _, ok := got["agent/trial-old"]; ok {
		t.Errorf("an agent the cluster does not define, with nothing running, is listed")
	}
	if _, ok := got["session/gone-1"]; ok {
		t.Errorf("a finished session of an agent that is gone is listed")
	}
	if _, ok := got["agent/trial-e"]; !ok {
		t.Errorf("an agent with an interactive session running is not listed")
	}
	cl.set("pod-e", false)
	if _, ok := peers(t, s, owner, false)["agent/trial-e"]; ok {
		t.Errorf("an agent that is gone is listed once its session has stopped")
	}
	// Agents the cluster defines are listed whether or not they ran.
	for _, a := range []string{"agent/trial", "agent/slow", "agent/desk"} {
		if _, ok := got[a]; !ok {
			t.Errorf("%s is not listed", a)
		}
	}
}

// TestR2PeersPlaceIsNoAgent: the owner's machine is a place, not an agent
// a message starts; its sessions are listed, by their durable address.
func TestR2PeersPlaceIsNoAgent(t *testing.T) {
	s, _, _ := messaging(t)
	s.Place = "host"
	ctx := context.Background()
	hostStart(t, s, "h-old", "proj", message.Busy)
	if err := s.Present(ctx, host, message.Presence{Session: "h-old", State: message.Ended}); err != nil {
		t.Fatal(err)
	}
	hostStart(t, s, "h-1", "proj", message.Busy)
	hostStart(t, s, "h-2", "other", message.Busy)
	if err := s.Present(ctx, host, message.Presence{Session: "h-2", State: message.Ended}); err != nil {
		t.Fatal(err)
	}

	got := peers(t, s, message.Caller{Owner: true}, false)
	if _, ok := got["agent/host"]; ok {
		t.Errorf("the place is listed as an agent")
	}
	if p := got["session/h-1"]; p.Named != "host/proj" || !p.Running {
		t.Errorf("the running session there: %+v", p)
	}
	if p, ok := got["session/h-2"]; !ok || p.Named != "host/other" || p.Running {
		t.Errorf("the latest of its name, not running: %+v", p)
	}
	if _, ok := got["session/h-old"]; ok {
		t.Errorf("an earlier session of a name a later one has is listed")
	}
}

// TestR2PeersNamedAddress: a session that its name reaches is listed with
// AGENT/NAME, which resolves to it, as its short id does; an earlier one
// of the same name is not reached by the name and does not carry it.
func TestR2PeersNamedAddress(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	owner := message.Caller{Owner: true}
	run(t, s, cl, "desk", "0aaa1111-old", "paris-vpn")
	if _, err := s.Send(ctx, owner, "", "session/0aaa1111-old", "still there?", ""); err != nil {
		t.Fatal(err)
	}
	start(t, s, cl, pod("desk", "pod-d"), "0bbb2222-new", "paris-vpn", message.Idle)

	got := peers(t, s, owner, false)
	p := got["session/0bbb2222-new"]
	if p.Named != "desk/paris-vpn" {
		t.Fatalf("the session its name reaches: %+v", p)
	}
	for _, given := range []string{p.Named, "session/0bbb2222"} {
		if to, err := s.Resolve(ctx, given); err != nil || to != p.Address {
			t.Errorf("Resolve(%q) = %q, %v; want %s", given, to, err, p.Address)
		}
	}
	if old, ok := got["session/0aaa1111-old"]; !ok || old.Named != "" {
		t.Errorf("the earlier session, written to: %+v, listed %v", old, ok)
	}
}

// TestR2PeersRoutineNobodyWritesTo: an agent with no job to start and no
// session a person works in (a routine's headless runs) is not listed: a
// message to it would reach nobody.
func TestR2PeersRoutineNobodyWritesTo(t *testing.T) {
	s, cl, _ := messaging(t)
	owner := message.Caller{Owner: true}
	cl.agents["reading"] = message.AgentInfo{Name: "reading"}
	p := pod("usage", "pod-u")
	cl.set(p.Pod, true)
	if err := s.Present(context.Background(), p, message.Presence{Session: "u-1", Kind: "headless", State: message.Busy}); err != nil {
		t.Fatal(err)
	}
	got := peers(t, s, owner, false)
	if _, ok := got["agent/usage"]; ok {
		t.Errorf("an agent whose runs take no messages is listed")
	}
	if _, ok := got["agent/reading"]; ok {
		t.Errorf("a defined agent with neither a job nor a standing pod is listed")
	}
	if _, ok := got["session/u-1"]; !ok {
		t.Errorf("a running session is not listed")
	}
	cl.set(p.Pod, false)
	if _, ok := peers(t, s, owner, false)["session/u-1"]; ok {
		t.Errorf("a finished run of an agent the cluster does not define is listed")
	}
}

// TestR2PeersSayWhatWaitsOnTheOwner: for going to a session (ADR 0057), a
// peer says its pod, how it runs, whether its agent is a standing one,
// and, to the owner and the owner's machine only, how many messages it
// sent the owner that he has not read.
func TestR2PeersSayWhatWaitsOnTheOwner(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "s-asks", "vpn", message.Idle)
	start(t, s, cl, d, "s-quiet", "notes", message.Idle)
	for _, body := range []string{"which one?", "and this?"} {
		if _, err := s.Send(ctx, d, "s-asks", "owner", body, ""); err != nil {
			t.Fatal(err)
		}
	}
	owner := message.Caller{Owner: true}
	got := peers(t, s, owner, false)
	if p := got["session/s-asks"]; p.ForOwner != 2 || p.Pod != "pod-d" || p.Mode != "interactive" {
		t.Errorf("a session with two messages for the owner: %+v", p)
	}
	if p := got["session/s-quiet"]; p.ForOwner != 0 {
		t.Errorf("a session with none: %+v", p)
	}
	if p := peers(t, s, message.Caller{Host: true, Agent: "host"}, false)["session/s-asks"]; p.ForOwner != 2 {
		t.Errorf("as the owner's machine sees it: %+v", p)
	}
	if p := peers(t, s, pod("other", "pod-o"), false)["session/s-asks"]; p.ForOwner != 0 {
		t.Errorf("another agent is told what waits on the owner: %+v", p)
	}
	// Read, it no longer waits.
	in, err := s.Inbox(ctx, owner, "")
	if err != nil || len(in) != 2 {
		t.Fatalf("the owner's inbox: %d, %v", len(in), err)
	}
	if _, err := s.Read(ctx, owner, "", []string{in[0].ID, in[1].ID}); err != nil {
		t.Fatal(err)
	}
	if p := peers(t, s, owner, false)["session/s-asks"]; p.ForOwner != 0 {
		t.Errorf("after the owner read them: %+v", p)
	}
}
