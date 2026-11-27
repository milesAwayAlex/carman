package improve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

// log is the part of the dispatcher's log the list needs, in memory.
type log struct {
	mu   sync.Mutex
	recs []dispatch.Record
	now  func() time.Time
}

func (l *log) Emit(ctx context.Context, e dispatch.Event) error {
	_, err := l.Claim(ctx, e)
	return err
}

func (l *log) Claim(_ context.Context, e dispatch.Event) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.ID == e.ID {
			return false, nil
		}
	}
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	l.recs = append(l.recs, dispatch.Record{Seq: int64(len(l.recs) + 1), Event: e})
	return true, nil
}

func (l *log) EventsOfTypes(_ context.Context, after int64, types []string, limit int) ([]dispatch.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []dispatch.Record
	for _, r := range l.recs {
		if r.Seq > after && contains(types, r.Type) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (l *log) Last(_ context.Context, typ string, n int) ([]dispatch.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []dispatch.Record
	for _, r := range l.recs {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// sent records what the gate handed over.
type sent struct{ to, body []string }

func (s *sent) SendVouched(_ context.Context, _ message.Caller, _, to, body string) (message.Result, error) {
	s.to, s.body = append(s.to, to), append(s.body, body)
	return message.Result{ID: "msg-" + to, To: to, State: "waiting"}, nil
}

var (
	owner    = message.Caller{Owner: true}
	surveyor = message.Caller{Agent: "watch", Pod: "watch-1"}
	platform = message.Caller{Agent: "platform", Pod: "platform-0"}
	stranger = message.Caller{Agent: "other", Pod: "other-1"}
	shared   = message.Caller{Agent: "watch"} // the shared agent token: no pod vouches for it
)

func setup(t *testing.T, edit func(*Config)) (*Service, *log, *sent, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	l := &log{now: func() time.Time { return now }}
	out := &sent{}
	cfg := Config{Agents: []string{"platform"}, Surveyor: "watch", To: []string{"agent/builder"}, BriefsPerDay: 1}
	if edit != nil {
		edit(&cfg)
	}
	return &Service{Store: l, Config: cfg, Sender: out, Context: "local", Now: func() time.Time { return now }}, l, out, &now
}

func note(id string) Note {
	return Note{ID: id, Title: "The title of " + id, Why: "because", Kind: "gap", Size: "small", Evidence: []string{"run x: it failed"}}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// One thing is one item: an id the list has is refused with what is there,
// and more evidence is added to it.
func TestListOneItemForOneThing(t *testing.T) {
	s, _, _, _ := setup(t, nil)
	ctx := context.Background()
	it := must[Item](t)(s.Add(ctx, surveyor, note("answer-where-it-arrives")))
	if it.State != Noticed || it.Who != "agent/watch" {
		t.Fatalf("a new item is %s by %s", it.State, it.Who)
	}
	if _, err := s.Add(ctx, platform, note("answer-where-it-arrives")); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("the same id twice: %v", err)
	}
	it = must[Item](t)(s.Change(ctx, platform, it.ID, Change{Evidence: []string{"message m: it was awkward"}}))
	if len(it.Evidence) != 2 || it.State != Noticed || len(it.History) != 2 {
		t.Fatalf("after more evidence: %+v", it)
	}
	if list := must[[]Item](t)(s.List(ctx, owner, "")); len(list) != 1 {
		t.Fatalf("the list has %d items", len(list))
	}
}

// Who may read and write the list, and who decides.
func TestListWhoMay(t *testing.T) {
	s, _, _, _ := setup(t, nil)
	ctx := context.Background()
	must[Item](t)(s.Add(ctx, platform, note("an-item")))
	for name, c := range map[string]message.Caller{"an agent not named": stranger, "the shared agent token": shared} {
		if _, err := s.List(ctx, c, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s reads the list: %v", name, err)
		}
		if _, err := s.Add(ctx, c, note("another")); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s adds to the list: %v", name, err)
		}
	}
	if _, err := s.Evidence(ctx, platform, 0, 0, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent of the list that is not the surveyor reads the evidence: %v", err)
	}
	for _, c := range []message.Caller{surveyor, platform, {Host: true, Agent: "host"}} {
		for _, state := range []string{Agreed, Declined} {
			if _, err := s.Change(ctx, c, "an-item", Change{State: state, Reason: "x"}); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s sets %s: %v", who(c), state, err)
			}
		}
	}
	if _, err := s.Change(ctx, owner, "an-item", Change{State: Declined}); err == nil {
		t.Error("declined with no reason")
	}
	must[Item](t)(s.Change(ctx, owner, "an-item", Change{State: Declined, Reason: "not worth it"}))
	if _, err := s.Change(ctx, platform, "an-item", Change{State: Building}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent builds what the owner declined: %v", err)
	}
	it := must[Item](t)(s.Change(ctx, owner, "an-item", Change{State: Agreed}))
	it = must[Item](t)(s.Change(ctx, platform, it.ID, Change{State: Building}))
	if it.By != "agent/platform" {
		t.Errorf("being built by %q", it.By)
	}
	if _, err := s.Change(ctx, platform, it.ID, Change{State: Done}); err == nil {
		t.Error("done with no commit")
	}
	it = must[Item](t)(s.Change(ctx, platform, it.ID, Change{State: Done, Commit: "abc1234"}))
	if it.State != Done || it.Commit != "abc1234" {
		t.Errorf("done: %+v", it)
	}
}

// The gate: what the owner has not agreed is held; an agreed item passes
// once, to a named addressee, within the day's budget and the usage's room;
// with sending off it is recorded and nothing is sent.
func TestGateHoldsAndWithholds(t *testing.T) {
	room, why := true, ""
	s, l, out, now := setup(t, nil)
	s.Room = func(context.Context) (bool, string) { return room, why }
	ctx := context.Background()
	must[Item](t)(s.Add(ctx, surveyor, note("first")))
	must[Item](t)(s.Add(ctx, surveyor, note("second")))
	held := func(id, to, want string) {
		t.Helper()
		_, err := s.Brief(ctx, surveyor, "", id, to, "Do it.")
		if !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), want) {
			t.Fatalf("a brief for %s to %s: %v; want held with %q", id, to, err, want)
		}
	}
	if _, err := s.Brief(ctx, platform, "", "first", "agent/builder", "Do it."); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent that is not the surveyor asks for a brief: %v", err)
	}
	held("first", "agent/builder", "waits for the owner's decision")
	must[Item](t)(s.Change(ctx, owner, "first", Change{State: Agreed}))
	must[Item](t)(s.Change(ctx, owner, "second", Change{State: Agreed}))
	held("first", "agent/desk", "only to the addressees")
	room, why = false, "the five-hour window is at 93%"
	held("first", "agent/builder", "93%")
	room = true
	it := must[Item](t)(s.Brief(ctx, surveyor, "", "first", "agent/builder", "Do it."))
	if it.Brief == nil || !it.Brief.Withheld || it.State != Agreed || len(out.to) != 0 {
		t.Fatalf("with sending off: %+v, sent %v", it.Brief, out.to)
	}
	held("first", "agent/builder", "written already")
	held("second", "agent/builder", "budget")
	*now = now.Add(25 * time.Hour)
	must[Item](t)(s.Brief(ctx, surveyor, "", "second", "agent/builder", "Do the second."))
	n := 0
	for _, r := range l.recs {
		if r.Type == TypeBriefed {
			n++
		}
	}
	if n != 2 || len(out.to) != 0 {
		t.Fatalf("%d briefs recorded, %d sent", n, len(out.to))
	}
}

// Switched on, a brief that passes goes to its addressee and the item is
// being built by it; a kind the installation lets start by itself needs no
// agreement, and another kind still does.
func TestGateSendsWhenSwitchedOn(t *testing.T) {
	s, _, out, _ := setup(t, func(c *Config) {
		c.Send, c.BriefsPerDay = true, 5
		c.Auto.Kinds, c.Auto.Sizes = []string{"fault"}, []string{"small"}
	})
	ctx := context.Background()
	fault := note("a-fault")
	fault.Kind = "fault"
	must[Item](t)(s.Add(ctx, surveyor, fault))
	must[Item](t)(s.Add(ctx, surveyor, note("a-gap")))
	it := must[Item](t)(s.Brief(ctx, surveyor, "s1", "a-fault", "agent/builder", "Fix it."))
	if it.State != Building || it.By != "agent/builder" || it.Brief.Message != "msg-agent/builder" {
		t.Fatalf("after a brief was sent: %+v", it)
	}
	if len(out.body) != 1 || !strings.HasPrefix(out.body[0], "Fix it.") || !strings.Contains(out.body[0], "nothing is merged or deployed") {
		t.Fatalf("sent: %q", out.body)
	}
	if _, err := s.Brief(ctx, surveyor, "s1", "a-gap", "agent/builder", "Build it."); !errors.Is(err, ErrHeld) {
		t.Fatalf("a kind that does not start by itself: %v", err)
	}
	if _, err := s.Brief(ctx, surveyor, "s1", "a-fault", "agent/builder", "Fix it again."); !errors.Is(err, ErrHeld) {
		t.Fatalf("an item being built is briefed again: %v", err)
	}
}

// The owner is told in one line, of what is new to decide, at most once a
// day; with notices off the line is only recorded.
func TestSurveyTellsTheOwnerLittle(t *testing.T) {
	s, _, out, now := setup(t, nil)
	ctx := context.Background()
	must[Item](t)(s.Add(ctx, surveyor, note("one")))
	sv := must[Survey](t)(s.Surveyed(ctx, surveyor, "", 10, "read 3 lines"))
	if sv.Notice == "" || sv.NoticeWithheld == "" || len(out.to) != 0 || sv.Waiting != 1 {
		t.Fatalf("with notices off: %+v, sent %v", sv, out.to)
	}
	if strings.Contains(sv.Notice, "one") && strings.Contains(sv.Notice, "title") {
		t.Fatalf("the notice names an item: %q", sv.Notice)
	}
	if through := must[int64](t)(s.LastSurvey(ctx)); through != 10 {
		t.Fatalf("the last survey read through %d", through)
	}
	s.Config.Notify = true
	*now = now.Add(time.Hour)
	// The withheld line counted as said: nothing new, nothing sent.
	if sv = must[Survey](t)(s.Surveyed(ctx, surveyor, "", 12, "")); sv.Notice != "" || len(out.to) != 0 {
		t.Fatalf("nothing new: %+v", sv)
	}
	must[Item](t)(s.Add(ctx, surveyor, note("two")))
	if sv = must[Survey](t)(s.Surveyed(ctx, surveyor, "", 14, "")); sv.Notice != "" || len(out.to) != 0 {
		t.Fatalf("within a day of the last notice: %+v", sv)
	}
	*now = now.Add(24 * time.Hour)
	sv = must[Survey](t)(s.Surveyed(ctx, surveyor, "", 14, ""))
	if len(out.to) != 1 || out.to[0] != message.Owner || sv.NoticeMessage == "" || !strings.Contains(out.body[0], "2 improvement(s)") {
		t.Fatalf("a day later: %+v, sent %v", sv, out.body)
	}
	*now = now.Add(48 * time.Hour)
	if sv = must[Survey](t)(s.Surveyed(ctx, surveyor, "", 9, "")); len(out.to) != 1 || sv.Through != 14 {
		t.Fatalf("nothing new two days later: %+v, sent %d", sv, len(out.to))
	}
}

// The evidence: what failed and what was said, not what went as expected;
// a message's words only from a sender the installation lists.
func TestEvidenceShowsWordsOnlyOfListedSenders(t *testing.T) {
	s, l, _, _ := setup(t, func(c *Config) { c.MessagesOf = []string{"platform"} })
	ctx := context.Background()
	emit := func(typ string, data any) {
		b, _ := json.Marshal(data)
		l.Emit(ctx, dispatch.Event{ID: typ + "/" + string(rune('a'+len(l.recs))), Type: typ, Source: "test", Data: b})
	}
	emit("job.ended", map[string]any{"job": "ok-1", "outcome": "succeeded"})
	emit("job.ended", map[string]any{"job": "bad-1", "template": "agent-x", "outcome": "failed", "reason": "DeadlineExceeded"})
	emit(message.TypeSent, message.Message{ID: "m1", From: "session/1", Agent: "platform", Name: "loop", To: "session/2", Body: "Done. What was awkward: the trial took two tries."})
	emit(message.TypeSent, message.Message{ID: "m2", From: "session/3", Agent: "desk", Name: "vpn", To: "session/4", Body: "a customer's name"})
	emit("usage.observed", map[string]any{"model": "m", "rate_limit_info": map[string]any{"status": "allowed"}})
	emit("task.done", map[string]any{})
	emit(TypeDeployed, Deployed{Commit: "1111111111111111111111111111111111111111", Previous: "2222222222222222222222222222222222222222", Commits: []string{"abc1234 A fix", "def5678 Another"}, More: 3})
	ev := must[Evidence](t)(s.Evidence(ctx, surveyor, -1, 0, 0))
	if len(ev.Lines) != 4 || ev.Through != 7 || ev.More {
		t.Fatalf("the evidence: %+v", ev)
	}
	if d := ev.Lines[3].Text; !strings.Contains(d, "is now 111111111111") || !strings.Contains(d, "5 commit(s) since: abc1234 A fix; def5678 Another") {
		t.Errorf("what was deployed: %q", d)
	}
	all := ev.Lines[0].Text + "\n" + ev.Lines[1].Text + "\n" + ev.Lines[2].Text
	if !strings.Contains(all, "bad-1") || !strings.Contains(all, "the trial took two tries") {
		t.Errorf("a failure or a listed sender's words are missing:\n%s", all)
	}
	if strings.Contains(all, "customer") || !strings.Contains(all, "desk/vpn to session/4 (17 characters") {
		t.Errorf("a sender that is not listed:\n%s", all)
	}
	if one, err := s.EvidenceAt(ctx, surveyor, 4); err != nil || strings.Contains(one.Text, "customer") {
		t.Errorf("one event whole: %+v, %v", one, err)
	}
	if ev = must[Evidence](t)(s.Evidence(ctx, surveyor, 0, 2, 20)); len(ev.Lines) != 2 || !ev.More || ev.Through != 3 || len(ev.Lines[1].Text) > 24 {
		t.Errorf("a page of two: %+v", ev)
	}
	// The next survey starts where this one read to.
	must[Survey](t)(s.Surveyed(ctx, surveyor, "", ev.Through, ""))
	if ev = must[Evidence](t)(s.Evidence(ctx, surveyor, -1, 0, 0)); len(ev.Lines) != 2 || ev.After != 3 {
		t.Errorf("after a survey through 3: %+v", ev)
	}
}

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig("")
	if err != nil || c.Send || c.Notify || c.Surveyor != "" || c.BriefsPerDay != 0 {
		t.Fatalf("nothing said: %+v, %v", c, err)
	}
	c, err = ParseConfig(`{"agents":["platform"],"surveyor":"watch","messagesOf":["platform","host"],"to":["agent/builder"],
		"auto":{"kinds":["fault"],"sizes":["small"]},"briefsPerDay":2,"send":false,"notify":false,"usage":{"below":0.5,"maxAge":"2h"}}`)
	if err != nil || c.Surveyor != "watch" || c.Usage.Below != 0.5 || c.Auto.Kinds[0] != "fault" {
		t.Fatalf("%+v, %v", c, err)
	}
	for _, bad := range []string{`{"auto":{"kinds":["big"]}}`, `{"usage":{"below":2}}`, `{"sendd":true}`} {
		if _, err := ParseConfig(bad); err == nil {
			t.Errorf("%s is taken", bad)
		}
	}
}
