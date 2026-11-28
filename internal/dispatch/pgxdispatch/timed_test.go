package pgxdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// A message at a time (ADR 0074).

// hands is a clock a test moves.
type hands struct {
	mu sync.Mutex
	t  time.Time
}

func (h *hands) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *hands) add(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

// sentAs is the message sent under an id, if one was.
func sentAs(t *testing.T, inst *Dispatcher, id string) (message.Message, bool) {
	t.Helper()
	recs, err := inst.OfTypeAbout(context.Background(), message.TypeSent, id)
	if err != nil {
		t.Fatal(err)
	}
	var m message.Message
	if len(recs) == 0 {
		return m, false
	}
	if len(recs) > 1 {
		t.Fatalf("message %s was sent %d times", id, len(recs))
	}
	if err := json.Unmarshal(recs[0].Data, &m); err != nil {
		t.Fatal(err)
	}
	return m, true
}

// TestR2MessageAtATimeIsSentThen: a session asks for a message to itself at
// a time. It is listed, is not sent before, is sent once at the time as an
// ordinary message from the session, and a dispatcher started in between
// still sends it.
func TestR2MessageAtATimeIsSentThen(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Date(2026, 3, 1, 14, 2, 0, 0, time.UTC)}
	s.Now = clock.now
	a := pod("desk", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)

	res, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "send the next task", "15:40", message.SendOptions{})
	if err != nil || res.State != "timed" || !strings.Contains(res.Detail, "2026-03-01 15:40:00 UTC") || !strings.Contains(res.Detail, "1h38m") {
		t.Fatalf("asked for 15:40: %+v, %v", res, err)
	}
	list, err := s.Timed(ctx, a, "sess-a")
	if err != nil || len(list) != 1 || list[0].ID != res.ID || list[0].To != "session/sess-a" {
		t.Fatalf("the sender's list: %+v, %v", list, err)
	}
	if other, _ := s.Timed(ctx, pod("trial", "pod-t"), ""); len(other) != 0 {
		t.Errorf("another agent sees it: %+v", other)
	}
	if all, _ := s.Timed(ctx, message.Caller{Owner: true}, ""); len(all) != 1 {
		t.Errorf("the owner does not see it: %+v", all)
	}
	if st, err := s.Status(ctx, a, res.ID); err != nil || st.State != "timed" {
		t.Errorf("its status before its time: %+v, %v", st.State, err)
	}

	clock.add(97 * time.Minute) // 15:39
	if err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := sentAs(t, inst, res.ID); ok {
		t.Fatal("sent before its time")
	}

	// The dispatcher is started again: what was asked is in the log.
	again := &message.Service{Store: inst, Cluster: cl, Log: slog.Default(), Timeout: time.Minute, Now: clock.now}
	clock.add(time.Minute) // 15:40
	if err := again.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	m, ok := sentAs(t, inst, res.ID)
	if !ok || m.From != "session/sess-a" || m.To != "session/sess-a" || !strings.HasSuffix(m.Body, "send the next task") ||
		!strings.Contains(m.Body, "asked at 2026-03-01 14:02 UTC for 2026-03-01 15:40 UTC") {
		t.Fatalf("at its time: %+v, sent %v", m, ok)
	}
	// It is the session's to take, as any message is.
	taken, err := again.Take(ctx, a, "sess-a", "prompt", nil, 0)
	if err != nil || len(taken.Messages) != 1 || taken.Messages[0].ID != res.ID {
		t.Errorf("the session does not take it: %+v, %v", taken, err)
	}
	// Once: neither the first dispatcher nor the second sends it again.
	clock.add(time.Hour)
	for _, x := range []*message.Service{s, again} {
		if err := x.SendDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sentAs(t, inst, res.ID)
	if list, _ := again.Timed(ctx, a, "sess-a"); len(list) != 0 {
		t.Errorf("still listed after it was sent: %+v", list)
	}
	if st, err := again.Status(ctx, a, res.ID); err != nil || st.State == "timed" {
		t.Errorf("its status after: %+v, %v", st.State, err)
	}
}

// TestR2MessageAtATimeSentBeforeAStopIsNotSentTwice: the message has the id
// it was asked under, so a dispatcher that stopped between sending it and
// recording that finds it sent.
func TestR2MessageAtATimeSentBeforeAStopIsNotSentTwice(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Now().UTC()}
	s.Now = clock.now
	a := pod("desk", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	res, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "look again", "10m", message.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := message.Message{ID: res.ID, From: "session/sess-a", To: "session/sess-a", Body: "look again", Sent: clock.now()}
	data, _ := json.Marshal(m)
	emit(t, inst, dispatch.Event{ID: message.TypeSent + "/" + m.ID, Type: message.TypeSent, Source: message.Source, Subject: m.ID, Data: data})
	clock.add(11 * time.Minute)
	if err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	sentAs(t, inst, res.ID) // fails on two
	if list, _ := s.Timed(ctx, a, "sess-a"); len(list) != 0 {
		t.Errorf("still waits: %+v", list)
	}
}

// TestR2MessageAtATimeIsCancelled: by its sender or the owner, not by
// another; a cancelled one is not sent.
func TestR2MessageAtATimeIsCancelled(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Now().UTC()}
	s.Now = clock.now
	a := pod("desk", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)
	start(t, s, cl, a, "sess-b", "beta", message.Idle)
	one, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "first", "30m", message.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.SendAt(ctx, a, "sess-a", "owner", "second", "30m", message.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTimed(ctx, a, "sess-b", one.ID); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another session cancelled it: %v", err)
	}
	if c, err := s.CancelTimed(ctx, a, "sess-a", one.ID); err != nil || c.ID != one.ID {
		t.Errorf("its sender could not cancel it: %v", err)
	}
	if _, err := s.CancelTimed(ctx, a, "sess-a", one.ID); !errors.Is(err, message.ErrInvalid) {
		t.Errorf("cancelled twice: %v", err)
	}
	if _, err := s.CancelTimed(ctx, message.Caller{Owner: true}, "", two.ID); err != nil {
		t.Errorf("the owner could not cancel it: %v", err)
	}
	if _, err := s.CancelTimed(ctx, a, "sess-a", "no-such"); !errors.Is(err, message.ErrUnknown) {
		t.Errorf("cancelling what was never asked: %v", err)
	}
	clock.add(time.Hour)
	if err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{one.ID, two.ID} {
		if _, ok := sentAs(t, inst, id); ok {
			t.Errorf("cancelled message %s was sent", id)
		}
		if st, err := s.Status(ctx, a, id); err != nil || st.State != "cancelled" {
			t.Errorf("status of a cancelled one: %q, %v", st.State, err)
		}
	}
	// One sent is no longer there to cancel.
	three, _ := s.SendAt(ctx, a, "sess-a", "desk/alpha", "third", "1m", message.SendOptions{})
	clock.add(2 * time.Minute)
	_ = s.SendDue(ctx)
	if _, err := s.CancelTimed(ctx, a, "sess-a", three.ID); !errors.Is(err, message.ErrInvalid) {
		t.Errorf("cancelled after it was sent: %v", err)
	}
}

// TestR2MessageAtATimeKeepsTheRulesAtItsTime: who may write is judged when
// the message is sent, not only when it is asked for; one refused then is
// recorded and its sender told.
func TestR2MessageAtATimeKeepsTheRulesAtItsTime(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Now().UTC()}
	s.Now = clock.now
	d := pod("desk", "pod-d")
	start(t, s, cl, d, "sess-d", "front", message.Idle)

	// Not allowed now: said at once, nothing kept.
	if _, err := s.SendAt(ctx, d, "sess-d", "trial", "run it", "10m", message.SendOptions{}); !errors.Is(err, message.ErrForbidden) || !strings.Contains(err.Error(), "writtenBy") {
		t.Fatalf("asked without the right to write: %v", err)
	}
	if _, err := s.SendAt(ctx, d, "sess-d", "nobody-here", "run it", "10m", message.SendOptions{}); !errors.Is(err, message.ErrUnknown) {
		t.Fatalf("asked for an addressee there is not: %v", err)
	}
	if list, _ := s.Timed(ctx, d, "sess-d"); len(list) != 0 {
		t.Fatalf("a refused one is kept: %+v", list)
	}

	// Allowed when asked, and no longer at the time.
	s.Writers = map[string][]string{"trial": {"desk"}}
	res, err := s.SendAt(ctx, d, "sess-d", "trial", "run it", "10m", message.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ok2, err := s.SendAt(ctx, d, "sess-d", "owner", "it is time", "10m", message.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.Writers = nil
	clock.add(11 * time.Minute)
	if err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, sent := sentAs(t, inst, res.ID); sent {
		t.Fatal("sent to an agent its sender may no longer write to")
	}
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeRefused, res.ID); len(recs) != 1 {
		t.Errorf("the refusal at its time is not recorded: %d", len(recs))
	}
	if st, err := s.Status(ctx, d, res.ID); err != nil || st.State != "refused" {
		t.Errorf("its status: %q, %v", st.State, err)
	}
	if m, sent := sentAs(t, inst, ok2.ID); !sent || m.To != "owner" {
		t.Errorf("the one still allowed was not sent: %+v", m)
	}
	told := 0
	recs, _ := inst.Last(ctx, message.TypeSent, 50)
	for _, r := range recs {
		var m message.Message
		if json.Unmarshal(r.Data, &m) == nil && m.From == "dispatcher" && m.To == "session/sess-d" &&
			strings.Contains(m.Body, "was not sent") && strings.Contains(m.Body, res.ID) && strings.Contains(m.Body, "run it") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("the sender was told %d times that it was not sent", told)
	}
	_ = s.SendDue(ctx)
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeRefused, res.ID); len(recs) != 1 {
		t.Errorf("refused again at the next pass: %d", len(recs))
	}
}

// TestR2MessageAtATimeHasLimits: not in the past, not further than a week,
// and no more than a set number waiting for one sender.
func TestR2MessageAtATimeHasLimits(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Date(2026, 3, 1, 14, 2, 0, 0, time.UTC)}
	s.Now = clock.now
	a := pod("desk", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)
	for _, at := range []string{"2026-03-01T13:00Z", "200h", "soon", "-5m", ""} {
		if _, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "x", at, message.SendOptions{}); !errors.Is(err, message.ErrInvalid) {
			t.Errorf("--at %q: %v", at, err)
		}
	}
	for i := range message.TimedPending {
		if _, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "x", "1h", message.SendOptions{}); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, err := s.SendAt(ctx, a, "sess-a", "desk/alpha", "x", "1h", message.SendOptions{}); !errors.Is(err, message.ErrInvalid) || !strings.Contains(err.Error(), "already wait") {
		t.Errorf("one more than may wait: %v", err)
	}
	// The limit is the agent's, whatever session its pod names: another
	// session of it is held too, a session nobody has heard of is refused,
	// and another agent is not held.
	start(t, s, cl, a, "sess-b", "beta", message.Idle)
	if _, err := s.SendAt(ctx, a, "sess-b", "desk/alpha", "x", "1h", message.SendOptions{}); !errors.Is(err, message.ErrInvalid) {
		t.Errorf("another session of the same agent: %v", err)
	}
	o := pod("trial", "pod-o")
	start(t, s, cl, o, "sess-o", "", message.Busy)
	if _, err := s.SendAt(ctx, o, "sess-o", "owner", "x", "1h", message.SendOptions{}); err != nil {
		t.Errorf("another agent: %v", err)
	}
	for _, made := range []string{"made-up-1", "made-up-2"} {
		if _, err := s.SendAt(ctx, o, made, "owner", "x", "1h", message.SendOptions{}); !errors.Is(err, message.ErrInvalid) || !strings.Contains(err.Error(), "knows no session") {
			t.Errorf("as a session the dispatcher has not heard of: %v", err)
		}
	}
	if list, _ := s.Timed(ctx, message.Caller{Owner: true}, ""); len(list) != message.TimedPending+1 {
		t.Errorf("%d wait, want %d", len(list), message.TimedPending+1)
	}
}

// TestR2MessageAtATimeThroughTheDoor: asked for, listed and cancelled by a
// pod's client over the API, and sent by the router's pass at its time.
func TestR2MessageAtATimeThroughTheDoor(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	clock := &hands{t: time.Now().UTC()}
	s.Now = clock.now
	a := pod("desk", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)
	srv := &api.Server{Store: inst, Messages: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify: func(context.Context, string) (message.Caller, error) { return a, nil }}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c := &api.Client{URL: hs.URL, Token: "a.b.c"}
	me := url.Values{"session": {"sess-a"}}

	var one, two message.Result
	if err := c.Do(ctx, http.MethodPost, "/v1/messages", nil, api.SendRequest{To: "desk/alpha", Body: "one", Session: "sess-a", At: "20m"}, &one); err != nil || one.State != "timed" {
		t.Fatalf("asking: %+v, %v", one, err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/messages", nil, api.SendRequest{To: "desk/alpha", Body: "two", Session: "sess-a", At: "20m"}, &two); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/messages", nil, api.SendRequest{To: "desk/alpha", Body: "x", Session: "sess-a", At: "whenever"}, nil); err == nil || !strings.Contains(err.Error(), "not a time") {
		t.Errorf("a time that is none: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/messages", nil, api.SendRequest{Answers: one.ID, Body: "x", Session: "sess-a", At: "5m"}, nil); err == nil {
		t.Error("an answer at a time was taken")
	}
	var list []message.Timed
	if err := c.Do(ctx, http.MethodGet, "/v1/timed", me, nil, &list); err != nil || len(list) != 2 {
		t.Fatalf("listing: %+v, %v", list, err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/timed/"+two.ID+"/cancel", me, nil, nil); err != nil {
		t.Fatalf("cancelling: %v", err)
	}
	clock.add(21 * time.Minute)
	r := &message.Router{Service: s, Logger: slog.Default()}
	if err := r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if m, ok := sentAs(t, inst, one.ID); !ok || !strings.HasSuffix(m.Body, "one") {
		t.Errorf("not sent by the router's pass: %+v", m)
	}
	if _, ok := sentAs(t, inst, two.ID); ok {
		t.Error("the cancelled one was sent")
	}
}
