package pgxdispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// A session that does not hear is noticed (ADR 0075).

// clock is a time a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func clocked(s *message.Service) *clock {
	c := &clock{t: time.Now()}
	s.Now = c.now
	return c
}

func eventsOf(t *testing.T, s *message.Service, typ string) []string {
	t.Helper()
	recs, err := s.Store.Last(context.Background(), typ, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, string(r.Data))
	}
	return out
}

// fromDispatcherTo are the bodies of the dispatcher's own messages to an
// address.
func fromDispatcherTo(t *testing.T, s *message.Service, to string) []string {
	t.Helper()
	recs, err := s.Store.Last(context.Background(), message.TypeSent, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		var m message.Message
		if json.Unmarshal(r.Data, &m) == nil && m.From == "dispatcher" && m.To == to {
			out = append(out, m.Body)
		}
	}
	return out
}

// deliveredEvent is a message's delivery, as a hook's take records it.
func deliveredEvent(id string) dispatch.Event {
	return dispatch.Event{ID: message.TypeDelivered + "/" + id, Type: message.TypeDelivered, Source: message.Source, Subject: id,
		Data: json.RawMessage(`{"session":"session/ws-b","how":"prompt"}`)}
}

// TestR2RefusedClientIsRecordedOnce: a pod whose client the dispatcher
// refuses again and again is recorded once when that goes on, not at each
// refusal, the owner is told once, and its sessions are shown as not
// hearing to whoever lists them or writes to them; when its client is
// hearing: one that has a listener is not. When nothing more is heard of
// the client, that is recorded too, and a dispatcher that starts again ends
// what an earlier one left open.
func TestR2RefusedClientIsRecordedOnce(t *testing.T) {
	s, cl, _ := messaging(t)
	now := clocked(s)
	ctx := context.Background()
	d := pod("desk", "pod-rf")
	start(t, s, cl, d, "rf-1", "vpn", message.Idle)
	const why = "it shows a token that expired"

	// A few refusals at once are a command that ended: nothing is recorded.
	for range 4 {
		s.Refused(ctx, d.Pod, d.PodUID, why)
	}
	if got := eventsOf(t, s, message.TypeClientRefused); len(got) != 0 {
		t.Fatalf("recorded at the first refusals: %v", got)
	}
	// They go on: recorded once, however many follow.
	now.add(message.RefusedFor + time.Second)
	for range 50 {
		s.Refused(ctx, d.Pod, d.PodUID, why)
	}
	got := eventsOf(t, s, message.TypeClientRefused)
	if len(got) != 1 || !strings.Contains(got[0], `"agent":"desk"`) || !strings.Contains(got[0], `"rf-1"`) || !strings.Contains(got[0], why) {
		t.Fatalf("refusals that go on are not recorded once, with the pod's agent and sessions: %v", got)
	}
	if told := fromDispatcherTo(t, s, message.Owner); len(told) != 1 || !strings.Contains(told[0], "pod-rf") || !strings.Contains(told[0], "desk/vpn") {
		t.Errorf("the owner is not told once, with the pod and its sessions: %v", told)
	}
	// Whoever lists the session, or writes to it, is told it does not hear.
	peers, err := s.Peers(ctx, message.Caller{Owner: true}, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, p := range peers {
		if p.Address == "session/rf-1" {
			seen = strings.Contains(p.State, "been refused by the dispatcher") && p.Refused != ""
		}
	}
	if !seen {
		t.Errorf("peers does not say the session's pod is refused: %+v", peers)
	}
	res, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/vpn", "are you there?", "")
	if err != nil || !strings.Contains(res.Detail, "been refused by the dispatcher") {
		t.Errorf("the sender is not told the pod is refused: %+v, %v", res, err)
	}
	if w := waitReasons(t, s, res.ID); !strings.Contains(w, "been refused by the dispatcher") {
		t.Errorf("the message does not say why it waits: %s", w)
	}
	if open, err := s.OpenRefusals(ctx); err != nil || len(open) != 1 || open[0].Pod != "pod-rf" {
		t.Errorf("the refusal that goes on: %+v, %v", open, err)
	}

	// Nothing more is heard of it: it has stopped, which is recorded, and
	// it is no longer shown. Another client of the pod being accepted
	// meanwhile says nothing of the one refused.
	now.add(message.RefusedQuiet + time.Second)
	s.QuietRefusals(ctx)
	if ended := eventsOf(t, s, message.TypeClientRefusedEnded); len(ended) != 1 || !strings.Contains(ended[0], "no refusal for") {
		t.Errorf("that it stopped is not recorded: %v", ended)
	}
	if open, _ := s.OpenRefusals(ctx); len(open) != 0 {
		t.Errorf("still shown as going on: %+v", open)
	}
	if res, _ := s.Send(ctx, message.Caller{Owner: true}, "", "desk/vpn", "and now?", ""); strings.Contains(res.Detail, "refused") {
		t.Errorf("still said to be refused: %+v", res)
	}

	// It goes on again: recorded anew.
	for range 5 {
		s.Refused(ctx, d.Pod, d.PodUID, why)
	}
	now.add(message.RefusedFor + time.Second)
	s.Refused(ctx, d.Pod, d.PodUID, why)
	if got := eventsOf(t, s, message.TypeClientRefused); len(got) != 2 {
		t.Fatalf("a second run of refusals is not recorded anew: %v", got)
	}

	// A dispatcher that starts again knows nothing of what one before it
	// left open: it ends it in the log.
	again := &message.Service{Store: s.Store, Cluster: s.Cluster, Log: slog.Default(), Now: now.now}
	if err := again.CloseRefusals(ctx); err != nil {
		t.Fatal(err)
	}
	if open, _ := again.OpenRefusals(ctx); len(open) != 0 {
		t.Errorf("a dispatcher started again leaves an earlier one's refusals open: %+v", open)
	}
}

// TestR2RefusedClientIsCountedAtTheDoor: the service counts a token that is
// refused and still names its pod; another client of the pod being
// accepted does not end it.
func TestR2RefusedClientIsCountedAtTheDoor(t *testing.T) {
	s, cl, inst := messaging(t)
	now := clocked(s)
	d := pod("desk", "pod-door")
	start(t, s, cl, d, "door-1", "vpn", message.Idle)
	srv := &api.Server{Store: inst, Messages: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify: func(_ context.Context, token string) (message.Caller, error) {
			if token == "old.old.old" {
				return message.Caller{}, &message.RefusedCaller{Pod: d.Pod, PodUID: d.PodUID, Why: "expired", Err: message.ErrForbidden}
			}
			return d, nil
		}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	call := func(token string) int {
		req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for range 5 {
		if code := call("old.old.old"); code != http.StatusUnauthorized {
			t.Fatalf("an expired token: %d", code)
		}
	}
	now.add(message.RefusedFor + time.Second)
	call("old.old.old")
	if got := eventsOf(t, s, message.TypeClientRefused); len(got) != 1 {
		t.Fatalf("refusals at the door are not recorded: %v", got)
	}
	if code := call("new.new.new"); code != http.StatusOK {
		t.Fatalf("a good token: %d", code)
	}
	if ended := eventsOf(t, s, message.TypeClientRefusedEnded); len(ended) != 0 {
		t.Errorf("another client's accepted call ends it: %v", ended)
	}
}

// TestR2UnheardSessionIsTheWatchersToWake: a message for a session that is
// open and idle with no listener says so, to its sender and on itself; the
// session's own pod, and no other, is told to give it a turn, once the
// message has waited a moment, until the session is busy or has taken it;
// and why the pod could not is recorded on the message.
func TestR2UnheardSessionIsTheWatchersToWake(t *testing.T) {
	s, cl, _ := messaging(t)
	now := clocked(s)
	ctx := context.Background()
	d := pod("desk", "pod-uh")
	start(t, s, cl, d, "uh-1", "notes", message.Idle)
	start(t, s, cl, d, "uh-2", "other", message.Idle)
	res, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/notes", "are you there?", "")
	if err != nil || !strings.Contains(res.Detail, "not listening") || !strings.Contains(res.Detail, "watcher") {
		t.Fatalf("to an idle session with no listener: %+v, %v", res, err)
	}
	if w := waitReasons(t, s, res.ID); !strings.Contains(w, "not listening") {
		t.Errorf("the message does not say why it waits: %s", w)
	}
	// A listener is away for a moment between two calls: not at once.
	if a, err := s.Attending(ctx, d); err != nil || len(a.Unheard) != 0 {
		t.Fatalf("told at once: %+v, %v", a, err)
	}
	now.add(message.UnheardGrace + time.Second)
	a, err := s.Attending(ctx, d)
	if err != nil || len(a.Unheard) != 1 || a.Unheard[0].Session != "uh-1" || a.Unheard[0].Name != "notes" || len(a.Unheard[0].Messages) != 1 || a.Unheard[0].Messages[0] != res.ID {
		t.Fatalf("the pod is not told which session to wake: %+v, %v", a, err)
	}
	// Another agent's pod is told nothing of it.
	other := pod("trial", "pod-uh-t")
	start(t, s, cl, other, "uh-t", "x", message.Idle)
	if a, err := s.Attending(ctx, other); err != nil || len(a.Unheard) != 0 {
		t.Errorf("another agent's pod is told: %+v, %v", a, err)
	}
	// Why the pod could not is on the message, once.
	for range 2 {
		if err := s.Hold(ctx, d, res.ID, "wake-attended", "a person's terminal is at its window"); err != nil {
			t.Fatal(err)
		}
	}
	if w := waitReasons(t, s, res.ID); strings.Count(w, "a person's terminal") != 1 {
		t.Errorf("the pod's reason is not recorded once: %s", w)
	}
	// Busy, it gets it by its hooks; idle again, it is the pod's again.
	start(t, s, cl, d, "uh-1", "notes", message.Busy)
	if a, _ := s.Attending(ctx, d); len(a.Unheard) != 0 {
		t.Errorf("a busy session is to be woken: %+v", a)
	}
	start(t, s, cl, d, "uh-1", "notes", message.Idle)
	if a, _ := s.Attending(ctx, d); len(a.Unheard) != 1 {
		t.Errorf("idle again with the message still waiting: %+v", a)
	}
	// The line typed starts a turn, whose hook takes it.
	if tk, err := s.Take(ctx, d, "uh-1", "prompt", nil, 0); err != nil || len(tk.Messages) != 1 {
		t.Fatalf("the turn's hook takes it: %+v, %v", tk, err)
	}
	if a, _ := s.Attending(ctx, d); len(a.Unheard) != 0 {
		t.Errorf("still to be woken once taken: %+v", a)
	}
}

// TestR2WaitedMessageTellsItsSender: a message not delivered within the
// time set tells its sender why, once: a session that runs, and the owner;
// a run that sent it and has ended is not started again to hear it, and
// has it on the message. One that was delivered, one for the owner, and
// the dispatcher's own, tell nobody.
func TestR2WaitedMessageTellsItsSender(t *testing.T) {
	s, cl, _ := messaging(t)
	now := clocked(s)
	s.WaitNotice = 10 * time.Minute
	s.Writers = map[string][]string{"desk": {"trial"}}
	ctx := context.Background()
	if err := s.TellWaiting(ctx); err != nil { // the first look only takes its place in the log
		t.Fatal(err)
	}
	d := pod("desk", "pod-w")
	start(t, s, cl, d, "ws-a", "alpha", message.Busy)
	start(t, s, cl, d, "ws-b", "beta", message.Idle)
	waits, err := s.Send(ctx, d, "ws-a", "desk/beta", "when you can", "")
	if err != nil {
		t.Fatal(err)
	}
	taken, err := s.Send(ctx, d, "ws-a", "desk/beta", "taken at once", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only the second is taken (as by a run that names it): the first waits.
	if _, err := s.Store.Claim(ctx, deliveredEvent(taken.ID)); err != nil {
		t.Fatal(err)
	}
	owners, err := s.Send(ctx, message.Caller{Owner: true}, "", "desk/beta", "from the owner", "")
	if err != nil {
		t.Fatal(err)
	}
	toOwner, err := s.Send(ctx, d, "ws-a", "owner", "for the owner", "")
	if err != nil {
		t.Fatal(err)
	}
	// A run that sends and ends.
	h := pod("trial", "pod-w-h")
	cl.set(h.Pod, true)
	if err := s.Present(ctx, h, message.Presence{Session: "ws-h", Kind: "headless", State: message.Busy}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Send(ctx, h, "ws-h", "desk/beta", "from a run", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Present(ctx, h, message.Presence{Session: "ws-h", Kind: "headless", State: message.Ended}); err != nil {
		t.Fatal(err)
	}

	now.add(5 * time.Minute)
	if err := s.TellWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(t, s, message.TypeWaited); len(got) != 0 {
		t.Fatalf("told before the time: %v", got)
	}
	now.add(6 * time.Minute)
	for range 2 {
		if err := s.TellWaiting(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waited := map[string]string{}
	recs, _ := s.Store.Last(ctx, message.TypeWaited, 100)
	for _, r := range recs {
		waited[r.Subject] = string(r.Data)
	}
	if len(recs) != 3 || !strings.Contains(waited[waits.ID], "not listening") || !strings.Contains(waited[waits.ID], `"told":true`) ||
		!strings.Contains(waited[owners.ID], `"told":true`) || !strings.Contains(waited[runs.ID], `"told":false`) {
		t.Fatalf("what waited is not recorded once each, with why and whether its sender was told: %v", waited)
	}
	if _, ok := waited[taken.ID]; ok {
		t.Errorf("a delivered message is told as waiting")
	}
	if _, ok := waited[toOwner.ID]; ok {
		t.Errorf("a message for the owner is told as waiting")
	}
	if told := fromDispatcherTo(t, s, "session/ws-a"); len(told) != 1 || !strings.Contains(told[0], waits.ID) || !strings.Contains(told[0], "not listening") || !strings.Contains(told[0], "desk/beta") {
		t.Errorf("the sending session is not told once, with why: %v", told)
	}
	if told := fromDispatcherTo(t, s, message.Owner); len(told) != 1 || !strings.Contains(told[0], owners.ID) {
		t.Errorf("the owner is not told once: %v", told)
	}
	if told := fromDispatcherTo(t, s, "session/ws-h"); len(told) != 0 {
		t.Errorf("a run that ended is started again to hear that its message waits: %v", told)
	}
	// What the dispatcher told ws-a waits too (ws-a never takes it): it
	// tells nobody of itself.
	now.add(11 * time.Minute)
	if err := s.TellWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(t, s, message.TypeWaited); len(got) != 3 {
		t.Errorf("the dispatcher's own message that waits is told of: %v", got)
	}
}

// TestR2WaitedMessageIsFoundAmongMany: a sender is told of a message that
// has waited however many older ones wait for the same addressee.
func TestR2WaitedMessageIsFoundAmongMany(t *testing.T) {
	s, cl, _ := messaging(t)
	now := clocked(s)
	s.WaitNotice = 10 * time.Minute
	ctx := context.Background()
	if err := s.TellWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	d := pod("desk", "pod-m")
	start(t, s, cl, d, "wm-a", "alpha", message.Busy)
	start(t, s, cl, d, "wm-b", "beta", message.Busy)
	owner := message.Caller{Owner: true}
	for range 205 {
		if _, err := s.Send(ctx, owner, "", "desk/beta", "one of many", ""); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.Send(ctx, d, "wm-a", "desk/beta", "the newest", "")
	if err != nil {
		t.Fatal(err)
	}
	now.add(11 * time.Minute)
	for range 5 {
		if err := s.TellWaiting(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if recs, _ := s.Store.OfTypeAbout(ctx, message.TypeWaited, last.ID); len(recs) != 1 {
		t.Errorf("the newest of many that wait is not told to its sender: %d", len(recs))
	}
}

// TestR2RefusedClientIsRecordedThoughItHangsUp: the refusal is recorded,
// and the owner told, on a request whose client has gone.
func TestR2RefusedClientIsRecordedThoughItHangsUp(t *testing.T) {
	s, cl, inst := messaging(t)
	now := clocked(s)
	d := pod("desk", "pod-hang")
	start(t, s, cl, d, "hang-1", "vpn", message.Idle)
	srv := &api.Server{Store: inst, Messages: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify: func(context.Context, string) (message.Caller, error) {
			return message.Caller{}, &message.RefusedCaller{Pod: d.Pod, PodUID: d.PodUID, Why: "expired", Err: message.ErrForbidden}
		}}
	call := func(ctx context.Context) {
		req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer old.old.old")
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	for range 5 {
		call(context.Background())
	}
	now.add(message.RefusedFor + time.Second)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	call(gone)
	if got := eventsOf(t, s, message.TypeClientRefused); len(got) != 1 {
		t.Fatalf("recorded %d times on a request whose client had gone", len(got))
	}
	if told := fromDispatcherTo(t, s, message.Owner); len(told) != 1 {
		t.Errorf("the owner was told %d times", len(told))
	}
}
