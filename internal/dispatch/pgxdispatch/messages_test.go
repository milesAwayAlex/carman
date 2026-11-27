package pgxdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/kube"
	"carman/internal/dispatch/message"
)

// fakeCluster has the pods named alive, and the agents given.
type fakeCluster struct {
	mu     sync.Mutex
	alive  map[string]bool // pod name
	agents map[string]message.AgentInfo
}

func (f *fakeCluster) PodAlive(_ context.Context, name, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[name], nil
}

func (f *fakeCluster) Agents(context.Context) (map[string]message.AgentInfo, error) {
	return f.agents, nil
}

func (f *fakeCluster) set(pod string, alive bool) {
	f.mu.Lock()
	f.alive[pod] = alive
	f.mu.Unlock()
}

func messaging(t *testing.T) (*message.Service, *fakeCluster, *Dispatcher) {
	t.Helper()
	d := factory(t).(db)
	inst, err := OpenWith(context.Background(), d.url, newFake().config(1), "b1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(inst.Kill)
	cl := &fakeCluster{alive: map[string]bool{}, agents: map[string]message.AgentInfo{
		"trial": {Name: "trial", Class: "agent-trial"},
		"desk":  {Name: "desk", Interactive: true},
		"slow":  {Name: "slow", Class: "agent-slow", Deadline: 2 * time.Hour},
	}}
	return &message.Service{Store: inst, Cluster: cl, Log: slog.Default(), Timeout: time.Minute}, cl, inst
}

func pod(agent, name string) message.Caller {
	return message.Caller{Agent: agent, Pod: name, PodUID: name + "-uid"}
}

func start(t *testing.T, s *message.Service, cl *fakeCluster, c message.Caller, session, name, state string) {
	t.Helper()
	cl.set(c.Pod, true)
	if err := s.Present(context.Background(), c, message.Presence{Session: session, Name: name, Kind: "interactive", State: state}); err != nil {
		t.Fatal(err)
	}
}

// TestR2MessageAddressing: owner, an agent by name, a session by its name
// or the start of its id; a name nobody has is refused, and says who there is.
func TestR2MessageAddressing(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	a := pod("trial", "pod-a")
	start(t, s, cl, a, "1111aaaa-0000", "alpha", message.Idle)
	for given, want := range map[string]string{
		"owner":                 "owner",
		"trial":                 "agent/trial",
		"agent/trial":           "agent/trial",
		"trial/alpha":           "session/1111aaaa-0000",
		"trial/1111":            "session/1111aaaa-0000",
		"session/1111aaaa-0000": "session/1111aaaa-0000",
		"session/1111":          "session/1111aaaa-0000",
	} {
		got, err := s.Resolve(ctx, given)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", given, got, err, want)
		}
	}
	for _, given := range []string{"nobody", "agent/nobody", "trial/beta", "session/9999", "x y"} {
		if _, err := s.Resolve(ctx, given); !errors.Is(err, message.ErrUnknown) {
			t.Errorf("Resolve(%q): %v, want ErrUnknown", given, err)
		}
	}
	res, err := s.Send(ctx, a, "1111aaaa-0000", "nobody", "hello", "")
	if err != nil || res.State != "refused" || !strings.Contains(res.Detail, "trial") {
		t.Fatalf("a message to nobody: %+v, %v", res, err)
	}
}

// TestR2MessageIdentity: a session is its agent's, and while it runs in
// one pod, no other pod may act as it; once its pod has gone, a pod of the
// same agent may (that is how it is resumed).
func TestR2MessageIdentity(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	a, b, other := pod("trial", "pod-a"), pod("trial", "pod-b"), pod("desk", "pod-d")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	start(t, s, cl, b, "sess-b", "beta", message.Busy)
	if _, err := s.Send(ctx, a, "sess-b", "trial/alpha", "as b", ""); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("pod a sending as session b, which runs in pod b: %v", err)
	}
	if _, err := s.Inbox(ctx, a, "sess-b"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("pod a reading session b's inbox: %v", err)
	}
	if _, err := s.Take(ctx, other, "sess-a", "tool", nil, 0); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another agent taking session a's messages: %v", err)
	}
	cl.set("pod-b", false)
	if _, err := s.Inbox(ctx, a, "sess-b"); err != nil {
		t.Errorf("session b's pod has gone; a pod of its agent resumes it: %v", err)
	}
	if _, err := s.Inbox(ctx, other, "sess-b"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another agent, after b's pod has gone: %v", err)
	}
}

// TestR2MessageInbox: a message to a running session is delivered once,
// however many ask at the same moment; it stays in the inbox, delivered,
// until read; answering it reads it, and the sender sees the answer.
func TestR2MessageInbox(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	a, b := pod("trial", "pod-a"), pod("trial", "pod-b")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	start(t, s, cl, b, "sess-b", "beta", message.Busy)
	res, err := s.Send(ctx, a, "sess-a", "trial/beta", "what is 2+2?", "")
	if err != nil || res.State != "waiting" || !strings.Contains(res.Detail, "busy") {
		t.Fatalf("send: %+v, %v", res, err)
	}
	var wg sync.WaitGroup
	got := make([]int, 4)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk, err := s.Take(ctx, b, "sess-b", "tool", nil, 0)
			if err != nil {
				t.Error(err)
			}
			got[i] = len(tk.Messages)
			if len(tk.Messages) == 1 && (!strings.Contains(tk.Text, "what is 2+2?") || !strings.Contains(tk.Text, "not from the owner") || !strings.Contains(tk.Text, "dispatcher reply "+res.ID)) {
				t.Errorf("what the session is told: %s", tk.Text)
			}
		}(i)
	}
	wg.Wait()
	if total := got[0] + got[1] + got[2] + got[3]; total != 1 {
		t.Fatalf("delivered %d times, want once (%v)", total, got)
	}
	items, err := s.Inbox(ctx, b, "sess-b")
	if err != nil || len(items) != 1 || items[0].Delivered == nil || items[0].Delivered.How != "tool" {
		t.Fatalf("inbox after delivery: %+v, %v", items, err)
	}
	if _, err := s.Read(ctx, a, "sess-a", []string{res.ID}); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("the sender marking the message read: %v", err)
	}
	ans, err := s.Send(ctx, b, "sess-b", "", "4", res.ID)
	if err != nil || ans.To != "session/sess-a" {
		t.Fatalf("reply: %+v, %v", ans, err)
	}
	if items, _ := s.Inbox(ctx, b, "sess-b"); len(items) != 0 {
		t.Errorf("answering did not read it: %+v", items)
	}
	st, err := s.Status(ctx, a, res.ID)
	if err != nil || st.State != "answered" || len(st.Answers) != 1 || st.Answers[0].Body != "4" {
		t.Fatalf("status: %+v, %v", st, err)
	}
	if _, err := s.Status(ctx, pod("desk", "pod-d"), res.ID); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("another agent reading what became of it: %v", err)
	}
	// The log has the message's life under its id.
	recs, _ := inst.About(ctx, res.ID)
	var types []string
	for _, r := range recs {
		types = append(types, r.Type)
	}
	if strings.Join(types, " ") != "message.sent message.delivered message.read" {
		t.Errorf("the log of the message: %v", types)
	}
}

// TestR2MessageSleepingSession: a message for a session that is not running
// becomes a task that resumes it as its agent; a second one waits for that
// task; a message to an agent by name becomes a new conversation; an agent
// with no job has its message wait.
func TestR2MessageSleepingSession(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	a, b := pod("trial", "pod-a"), pod("trial", "pod-b")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	start(t, s, cl, b, "sess-b", "beta", message.Idle)
	cl.set("pod-b", false) // gone without a word: the sweep finds it
	if err := (&message.Router{Service: s, Logger: slog.Default()}).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(ctx, a, "sess-a", "trial/beta", "wake up", "")
	if err != nil || !strings.Contains(res.Detail, "not running") {
		t.Fatalf("send: %+v, %v", res, err)
	}
	task := message.TaskPrefix + res.ID
	st, err := inst.Explain(ctx, task)
	if err != nil || st.Spec.Class != "agent-trial" {
		t.Fatalf("the task: %+v, %v", st, err)
	}
	if st.Spec.AttemptTimeout != time.Minute {
		t.Errorf("an agent whose job has no deadline: attempts may run %s, want the service's %s", st.Spec.AttemptTimeout, time.Minute)
	}
	var p kube.Payload
	_ = json.Unmarshal(st.Spec.Payload, &p)
	if p.Env["TASK_MESSAGE"] != res.ID || p.Env["TASK_SESSION"] != "sess-b" {
		t.Errorf("payload: %+v", p)
	}
	if _, err := kube.ParsePayload(st.Spec.Payload); err != nil {
		t.Errorf("the starter refuses the payload: %v", err)
	}
	res2, err := s.Send(ctx, a, "sess-a", "trial/beta", "and this", "")
	if err != nil || !strings.Contains(res2.Detail, task) {
		t.Errorf("a second message while the task is open: %+v, %v", res2, err)
	}
	if _, err := inst.Explain(ctx, message.TaskPrefix+res2.ID); !errors.Is(err, dispatch.ErrNotFound) {
		t.Errorf("a second task for the same session: %v", err)
	}
	// The run takes both: the one it was started for, and what waits for its session.
	run := pod("trial", "pod-run")
	cl.set("pod-run", true)
	tk, err := s.Take(ctx, run, "sess-b", "start", []string{res.ID}, 0)
	if err != nil || len(tk.Messages) != 2 {
		t.Fatalf("the run takes: %+v, %v", tk, err)
	}
	if r, err := s.Answer(ctx, run, "sess-b", res.ID, "awake", false); err != nil || r.To != "session/sess-a" {
		t.Errorf("the run's answer: %+v, %v", r, err)
	}
	if r, err := s.Answer(ctx, run, "sess-b", res.ID, "again", false); err != nil || r.State != "answered" {
		t.Errorf("a second answer: %+v, %v", r, err)
	}

	byName, err := s.Send(ctx, a, "sess-a", "trial", "a new one", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err = inst.Explain(ctx, message.TaskPrefix+byName.ID)
	if err != nil || strings.Contains(string(st.Spec.Payload), "TASK_SESSION") {
		t.Errorf("an agent by name: %+v, %v", st, err)
	}
	tk, err = s.Take(ctx, run, "new-sess", "start", []string{byName.ID}, 0)
	if err != nil || len(tk.Messages) != 1 {
		t.Errorf("the new conversation takes it: %+v, %v", tk, err)
	}
	// The agent's own time limit: its job's deadline, and the overrun a
	// scheduled run of it is allowed.
	s.Writers = map[string][]string{"slow": {"trial"}, "desk": {"trial"}}
	slow, err := s.Send(ctx, a, "sess-a", "slow", "take your time", "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err = inst.Explain(ctx, message.TaskPrefix+slow.ID); err != nil || st.Spec.AttemptTimeout != 2*time.Hour+message.Overrun {
		t.Errorf("an agent whose job may run two hours: %+v, %v", st.Spec, err)
	}
	desk, err := s.Send(ctx, a, "sess-a", "desk", "hello desk", "")
	if err != nil || !strings.Contains(desk.Detail, "no job") {
		t.Errorf("an agent with no job: %+v, %v", desk, err)
	}
}

// TestR2MessageWakes: an idle session's waiter is handed a message the
// moment it is sent, and gives up when its session becomes busy.
func TestR2MessageWakes(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	a, b := pod("trial", "pod-a"), pod("trial", "pod-b")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	start(t, s, cl, b, "sess-b", "beta", message.Idle)
	got := make(chan message.Taken, 1)
	go func() {
		tk, err := s.Take(ctx, b, "sess-b", "wake", nil, time.Minute)
		if err != nil {
			t.Error(err)
		}
		got <- tk
	}()
	time.Sleep(300 * time.Millisecond)
	sent := time.Now()
	if _, err := s.Send(ctx, a, "sess-a", "trial/beta", "are you there?", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case tk := <-got:
		if len(tk.Messages) != 1 || time.Since(sent) > 2*time.Second {
			t.Errorf("woken with %+v after %v", tk, time.Since(sent))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was not woken")
	}
	go func() {
		tk, _ := s.Take(ctx, b, "sess-b", "wake", nil, time.Minute)
		got <- tk
	}()
	time.Sleep(300 * time.Millisecond)
	start(t, s, cl, b, "sess-b", "beta", message.Busy)
	select {
	case tk := <-got:
		if !tk.Busy {
			t.Errorf("a waiter whose session became busy: %+v", tk)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter did not give up when its session became busy")
	}
}

// TestR2MessageExchangeEnds: two sessions answering each other's answers
// are stopped after MaxDepth answers; the owner's word is not.
func TestR2MessageExchangeEnds(t *testing.T) {
	s, cl, _ := messaging(t)
	ctx := context.Background()
	a, b := pod("trial", "pod-a"), pod("trial", "pod-b")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	start(t, s, cl, b, "sess-b", "beta", message.Busy)
	res, err := s.Send(ctx, a, "sess-a", "trial/beta", "ping", "")
	if err != nil {
		t.Fatal(err)
	}
	who := []message.Caller{b, a}
	sess := []string{"sess-b", "sess-a"}
	for i := 1; ; i++ {
		next, err := s.Send(ctx, who[i%2], sess[i%2], "", "pong", res.ID)
		if i <= message.MaxDepth {
			if err != nil {
				t.Fatalf("answer %d: %v", i, err)
			}
			res = next
			continue
		}
		if !errors.Is(err, message.ErrInvalid) {
			t.Fatalf("answer %d was not refused: %+v, %v", i, next, err)
		}
		break
	}
}

// TestR2MessageWhoMayWrite: with nothing listed, only the owner and the
// agent's own sessions may write to an agent or its sessions; an agent
// listed in its writtenBy may; anyone may answer a message it was sent;
// a refusal says who may, and is recorded (ADR 0047).
func TestR2MessageWhoMayWrite(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	a := pod("trial", "pod-a")
	d := pod("desk", "pod-d")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)
	start(t, s, cl, d, "sess-d", "front", message.Idle)
	start(t, s, cl, pod("trial", "pod-b"), "sess-b", "beta", message.Busy)

	refused := func(what string, res message.Result, err error) {
		t.Helper()
		if err != nil || res.State != "refused" {
			t.Errorf("%s: want refused, got %+v, %v", what, res, err)
		}
	}
	sent := func(what string, res message.Result, err error) {
		t.Helper()
		if err != nil || res.State == "refused" {
			t.Errorf("%s: want sent, got %+v, %v", what, res, err)
		}
	}

	// Nothing listed: another agent may not, by name or to a session.
	res, err := s.Send(ctx, d, "sess-d", "trial", "run something", "")
	refused("desk to trial by name", res, err)
	if !strings.Contains(res.Detail, "only by the owner and its own sessions") || !strings.Contains(res.Detail, "writtenBy") {
		t.Errorf("the refusal does not say who may: %s", res.Detail)
	}
	recs, err := inst.OfTypeAbout(ctx, message.TypeRefused, res.ID)
	if err != nil || len(recs) != 1 {
		t.Errorf("the refusal is not recorded: %d, %v", len(recs), err)
	}
	res, err = s.Send(ctx, d, "sess-d", "trial/alpha", "hello", "")
	refused("desk to a session of trial", res, err)
	// The owner may; sessions of one agent may write to each other; any
	// agent may write to the owner.
	res, err = s.Send(ctx, message.Caller{Owner: true}, "", "trial/alpha", "from the owner", "")
	sent("the owner", res, err)
	res, err = s.Send(ctx, a, "sess-a", "trial/beta", "sibling", "")
	sent("a sibling session", res, err)
	res, err = s.Send(ctx, d, "sess-d", "owner", "to the owner", "")
	sent("to the owner", res, err)

	// A session answers a message it was sent, whoever sent it; it may not
	// use that answer to reach anyone else.
	ask, err := s.Send(ctx, a, "sess-a", "desk/front", "what is 2+2?", "")
	refused("trial to desk, nothing listed", ask, err)
	s.Writers = map[string][]string{"desk": {"trial"}}
	ask, err = s.Send(ctx, a, "sess-a", "desk/front", "what is 2+2?", "")
	sent("trial to desk, listed", ask, err)
	res, err = s.Send(ctx, d, "sess-d", "", "4", ask.ID)
	sent("desk answers trial", res, err)
	res, err = s.Send(ctx, d, "sess-d", "trial/beta", "4", ask.ID)
	refused("an answer sent elsewhere", res, err)
	// The list is one way.
	res, err = s.Send(ctx, d, "sess-d", "trial", "new work", "")
	refused("desk to trial, desk listed only for itself", res, err)
	if !strings.Contains(res.Detail, "not by agent desk") {
		t.Errorf("the refusal: %s", res.Detail)
	}

	// Peers marks what the caller may not write to.
	ps, err := s.Peers(ctx, d, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		want := p.Agent == "trial" || p.Agent == "slow"
		if got := p.NotWritable != ""; got != want {
			t.Errorf("peers for desk: %s marked %v, want %v (%q)", p.Address, got, want, p.NotWritable)
		}
	}
	ps, err = s.Peers(ctx, message.Caller{Owner: true}, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.NotWritable != "" {
			t.Errorf("peers for the owner: %s marked %q", p.Address, p.NotWritable)
		}
	}
}

// TestR2MessageRunsPerSender: a sender's new messages start at most so many
// runs in a period; past it a message is held and the sender told why,
// until the owner lifts it, which starts what was held. The owner and an
// answer are not limited.
func TestR2MessageRunsPerSender(t *testing.T) {
	s, cl, inst := messaging(t)
	s.RunsPerSender, s.RunsPeriod = 2, time.Hour
	ctx := context.Background()
	a := pod("trial", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	var last message.Result
	for i := 0; i < 3; i++ {
		res, err := s.Send(ctx, a, "sess-a", "agent/trial", "go", "")
		if err != nil {
			t.Fatal(err)
		}
		last = res
	}
	if !strings.Contains(last.Detail, "held") || !strings.Contains(last.Detail, "dispatch lift session/sess-a") {
		t.Fatalf("the third message was not held: %+v", last)
	}
	if _, err := inst.Explain(ctx, message.TaskPrefix+last.ID); !errors.Is(err, dispatch.ErrNotFound) {
		t.Errorf("a held message's run was started: %v", err)
	}
	// The owner is not limited.
	if res, err := s.Send(ctx, message.Caller{Owner: true}, "", "agent/trial", "go", ""); err != nil || strings.Contains(res.Detail, "held") {
		t.Errorf("the owner's message: %+v, %v", res, err)
	}
	if _, err := s.Lift(ctx, a, "session/sess-a"); !errors.Is(err, message.ErrForbidden) {
		t.Errorf("a pod lifted its own limit: %v", err)
	}
	n, err := s.Lift(ctx, message.Caller{Owner: true}, "session/sess-a")
	if err != nil || n != 1 {
		t.Fatalf("lifted: %d, %v", n, err)
	}
	if _, err := inst.Explain(ctx, message.TaskPrefix+last.ID); err != nil {
		t.Errorf("the held message did not start when lifted: %v", err)
	}
	// After the lift the count starts again.
	if res, _ := s.Send(ctx, a, "sess-a", "agent/trial", "go", ""); strings.Contains(res.Detail, "held") {
		t.Errorf("held after the lift: %+v", res)
	}
}

// TestR2MessageRunAtLimit: a run started for a message that stops at the
// plan's usage limit is recorded as that; the sender is told the work was
// not done and when the limit resets; asked for, the dispatcher sends the
// message again once after the reset, and not a third time.
func TestR2MessageRunAtLimit(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	a := pod("trial", "pod-a")
	start(t, s, cl, a, "sess-a", "alpha", message.Busy)
	run := pod("trial", "pod-run")
	cl.set("pod-run", true)
	const words = "You've hit your 5-hour limit · resets 10:40am (UTC)"

	// Not asked: told, not sent again.
	plain, err := s.Send(ctx, a, "sess-a", "trial", "do one thing", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Take(ctx, run, "run-1", "start", []string{plain.ID}, 0); err != nil {
		t.Fatal(err)
	}
	r, err := s.Answer(ctx, run, "run-1", plain.ID, words, true)
	if err != nil || r.To != "session/sess-a" {
		t.Fatalf("the answer: %+v, %v", r, err)
	}
	told := body(t, inst, r.ID)
	for _, want := range []string{"Not done", "2026-11-27 10:40 UTC", "not sent again by itself", words} {
		if !strings.Contains(told, want) {
			t.Errorf("the sender is told %q, without %q", told, want)
		}
	}
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeLimited, plain.ID); len(recs) != 1 {
		t.Errorf("message.limited recorded %d times", len(recs))
	}

	// Asked: sent again once, after the reset.
	asked, err := s.SendWith(ctx, a, "sess-a", "trial", "do another", "", message.SendOptions{AgainAfterLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Answer(ctx, run, "run-2", asked.ID, words, true)
	if err != nil || !strings.Contains(body(t, inst, r.ID), "again by itself, once, at about 10:45 UTC") {
		t.Fatalf("the answer when asked: %+v, %v: %s", r, err, body(t, inst, r.ID))
	}
	if err := s.SendAgain(ctx); err != nil {
		t.Fatal(err)
	}
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeResent, asked.ID); len(recs) != 0 {
		t.Fatal("sent again before the reset")
	}
	now = now.Add(2 * time.Hour)
	for range 2 {
		if err := s.SendAgain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := inst.OfTypeAbout(ctx, message.TypeResent, asked.ID)
	if len(recs) != 1 {
		t.Fatalf("sent again %d times", len(recs))
	}
	var resent struct{ Again string }
	_ = json.Unmarshal(recs[0].Data, &resent)
	if _, err := inst.Explain(ctx, message.TaskPrefix+resent.Again); err != nil {
		t.Errorf("no run started for the message sent again: %v", err)
	}
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeResent, plain.ID); len(recs) != 0 {
		t.Error("a message whose sender did not ask was sent again")
	}
	// At the limit again: told so, not sent a third time.
	r, err = s.Answer(ctx, run, "run-3", resent.Again, words, true)
	if err != nil || !strings.Contains(body(t, inst, r.ID), "already sent again once") {
		t.Errorf("the second limit: %+v, %v", r, err)
	}
	now = now.Add(24 * time.Hour)
	_ = s.SendAgain(ctx)
	if recs, _ := inst.OfTypeAbout(ctx, message.TypeResent, resent.Again); len(recs) != 0 {
		t.Error("sent a third time")
	}
	// The same words from a run that did not fail are its answer.
	other, _ := s.Send(ctx, a, "sess-a", "trial", "quote it", "")
	if r, _ := s.Answer(ctx, run, "run-4", other.ID, words, false); body(t, inst, r.ID) != words {
		t.Errorf("a run that finished: %q", body(t, inst, r.ID))
	}
}

func body(t *testing.T, inst *Dispatcher, id string) string {
	t.Helper()
	recs, err := inst.OfTypeAbout(context.Background(), message.TypeSent, id)
	if err != nil || len(recs) == 0 {
		return ""
	}
	var m message.Message
	_ = json.Unmarshal(recs[0].Data, &m)
	return m.Body
}

// TestR2MessageSessionAtLimit: a running session whose harness stopped at
// the plan's usage limit, as its pod's watcher says: the sender of what it
// took and has not answered, and whoever watches it, are told once and
// when the limit resets; another pod may not say it.
func TestR2MessageSessionAtLimit(t *testing.T) {
	s, cl, inst := messaging(t)
	ctx := context.Background()
	s.Now = func() time.Time { return time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC) }
	a, b, w := pod("trial", "pod-a"), pod("desk", "pod-b"), pod("trial", "pod-w")
	start(t, s, cl, a, "sess-a", "alpha", message.Idle)
	start(t, s, cl, b, "sess-b", "beta", message.Busy)
	start(t, s, cl, w, "sess-w", "watcher", message.Idle)
	s.Writers = map[string][]string{"desk": {"trial"}}
	sent, err := s.Send(ctx, a, "sess-a", "desk/beta", "a long task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Take(ctx, b, "sess-b", "tool", []string{sent.ID}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Watch(ctx, w, "sess-w", "desk/beta"); err != nil {
		t.Fatal(err)
	}
	const words = "You've hit your 5-hour limit · resets 10:40am (UTC)"
	if _, err := s.HarnessLimited(ctx, a, "sess-b", "u-stop", words); err == nil {
		t.Error("another agent's pod says the session stopped")
	}
	n, err := s.HarnessLimited(ctx, b, "sess-b", "u-stop", words)
	if err != nil || n != 2 {
		t.Fatalf("told %d, %v; want the sender and the watcher", n, err)
	}
	if n, _ := s.HarnessLimited(ctx, b, "sess-b", "u-stop", words); n != 0 {
		t.Errorf("the same stop told again: %d", n)
	}
	for _, to := range []string{"session/sess-a", "session/sess-w"} {
		recs, _ := inst.Unmarked(ctx, []string{to}, "read", 10)
		found := false
		for _, r := range recs {
			var m message.Message
			_ = json.Unmarshal(r.Data, &m)
			found = found || (m.From == "dispatcher" && strings.Contains(m.Body, "stopped at the plan's usage limit, which resets at 2026-11-27 10:40 UTC"))
		}
		if !found {
			t.Errorf("%s is not told", to)
		}
	}
	// A later stop: the message was told of already, the watcher again.
	if n, _ := s.HarnessLimited(ctx, b, "sess-b", "u-stop-2", words); n != 1 {
		t.Errorf("a second stop told %d, want the watcher only", n)
	}
}
