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
		"desk":  {Name: "desk"},
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
	if r, err := s.Answer(ctx, run, "sess-b", res.ID, "awake"); err != nil || r.To != "session/sess-a" {
		t.Errorf("the run's answer: %+v, %v", r, err)
	}
	if r, err := s.Answer(ctx, run, "sess-b", res.ID, "again"); err != nil || r.State != "answered" {
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
