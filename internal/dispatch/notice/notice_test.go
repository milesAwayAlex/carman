package notice

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/kube"
)

type fakeLog struct {
	recs      []dispatch.Record
	submitted []dispatch.TaskSpec
	mark      []byte
}

func (f *fakeLog) add(typ, subject string, data any) {
	b, _ := json.Marshal(data)
	f.recs = append(f.recs, dispatch.Record{Seq: int64(len(f.recs) + 1), Event: dispatch.Event{ID: typ + "/" + subject + "/" + time.Now().String(), Type: typ, Subject: subject, Data: b}})
}
func (f *fakeLog) EventsOfTypes(_ context.Context, after int64, types []string, limit int) ([]dispatch.Record, error) {
	var out []dispatch.Record
	for _, r := range f.recs {
		if r.Seq > after && slices.Contains(types, r.Type) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeLog) Explain(context.Context, string) (dispatch.TaskStatus, error) {
	return dispatch.TaskStatus{}, dispatch.ErrNotFound
}
func (f *fakeLog) Submit(_ context.Context, t dispatch.TaskSpec) error {
	f.submitted = append(f.submitted, t)
	return nil
}
func (f *fakeLog) Emit(_ context.Context, e dispatch.Event) error {
	f.recs = append(f.recs, dispatch.Record{Seq: int64(len(f.recs) + 1), Event: e})
	return nil
}
func (f *fakeLog) Mark(_ context.Context, _ string, v any) (bool, error) {
	if f.mark == nil {
		return false, nil
	}
	return true, json.Unmarshal(f.mark, v)
}
func (f *fakeLog) SetMark(_ context.Context, _ string, v any) error {
	f.mark, _ = json.Marshal(v)
	return nil
}
func (f *fakeLog) Last(context.Context, string, int) ([]dispatch.Record, error) {
	if len(f.recs) == 0 {
		return nil, nil
	}
	return f.recs[len(f.recs)-1:], nil
}

func (f *fakeLog) withheld() []Notice {
	var out []Notice
	for _, r := range f.recs {
		if r.Type == TypeWithheld {
			var n Notice
			json.Unmarshal(r.Data, &n)
			out = append(out, n)
		}
	}
	return out
}

// TestR10Notices: one notice per event that calls for one, never twice;
// withheld while off; none for a failed notice or for a program that told
// the owner itself.
func TestR10Notices(t *testing.T) {
	ctx := context.Background()
	f := &fakeLog{}
	f.add(kube.TypeJobEnded, "agent-old", kube.JobEnded{Job: "agent-old-1", Template: "agent-old", Outcome: "failed"})
	n := &Notifier{Log: f, Class: "notify", Context: "CTX", Namespace: "ns", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.Pass(ctx) // the first pass starts at the end: the old failure is not told
	one := 1
	f.add(kube.TypeJobEnded, "agent-x", kube.JobEnded{Job: "agent-x-29400000", Template: "agent-x", By: "cronjob", Outcome: "failed", Reason: "BackoffLimitExceeded", ExitCode: &one})
	f.add(kube.TypeJobEnded, "agent-x", kube.JobEnded{Job: "agent-x-29400001", Template: "agent-x", Outcome: "succeeded"})
	f.add(kube.TypeJobEnded, "notify", kube.JobEnded{Job: "notice-1", Template: "notify", Outcome: "failed"})
	f.add(kube.TypeJobEnded, "backup", kube.JobEnded{Job: "backup-1", Template: "backup", Outcome: "failed", ExitCode: &one, NotifiesOwner: true})
	f.add(kube.TypeJobEnded, "backup", kube.JobEnded{Job: "backup-2", Template: "backup", Outcome: "failed", Reason: "DeadlineExceeded", NotifiesOwner: true})
	f.add(kube.TypeRunMissed, "agent-y", kube.RunMissed{CronJob: "agent-y", Schedule: "0 6 * * *", Tick: time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC), Grace: "1h0m0s"})
	tick := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	f.add(dispatch.TypeMissed, "usage", dispatch.Transition{Tick: &tick, Skipped: 2})
	for range 2 {
		if err := n.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w := f.withheld()
	var titles []string
	for _, x := range w {
		titles = append(titles, x.Title)
	}
	want := []string{"agent-x failed", "backup failed", "agent-y did not run", "The schedule usage skipped 2 run(s)"}
	if !slices.Equal(titles, want) {
		t.Fatalf("withheld %q, want %q", titles, want)
	}
	if !strings.Contains(w[0].Text, "kubectl --context CTX -n ns logs job/agent-x-29400000") || !strings.Contains(w[0].Text, "exit code 1") {
		t.Errorf("the text says %q", w[0].Text)
	}
	if !strings.Contains(w[2].Text, "2026-11-24 06:00 UTC") {
		t.Errorf("the text says %q", w[2].Text)
	}
	n.Enabled = true
	f.add(kube.TypeRunMissed, "agent-z", kube.RunMissed{CronJob: "agent-z", Tick: tick})
	n.Pass(ctx)
	n.Pass(ctx)
	if len(f.submitted) != 1 || f.submitted[0].Class != "notify" || !strings.HasPrefix(f.submitted[0].ID, "notice/run.missed/agent-z") {
		t.Fatalf("submitted %+v", f.submitted)
	}
	p, err := kube.ParsePayload(f.submitted[0].Payload)
	if err != nil || p.Env["TASK_TITLE"] != "agent-z did not run" {
		t.Errorf("payload %+v, %v", p, err)
	}
}

// TestR10AMessageToTheOwnerIsNotCarried: the notice for a message to the
// owner says that there is one and how to read and answer it, and has none
// of what the message says; the notice's channel is treated as open.
func TestR10AMessageToTheOwnerIsNotCarried(t *testing.T) {
	n := &Notifier{}
	data, _ := json.Marshal(map[string]string{"id": "m1", "from": "session/s1", "agent": "builder", "name": "message", "to": "owner",
		"body": "The fleet in region nine costs 4,112 a month."})
	no, ok := n.Compose(context.Background(), dispatch.Record{Event: dispatch.Event{Type: TypeMessage, Subject: "m1", Data: data}})
	if !ok {
		t.Fatal("a message to the owner is a notice")
	}
	if strings.Contains(no.Text, "4,112") || strings.Contains(no.Text, "region nine") || strings.Contains(no.Title, "4,112") {
		t.Errorf("the notice carries the message: %q / %q", no.Title, no.Text)
	}
	for _, want := range []string{"dispatch message m1", "dispatch reply m1", "builder"} {
		if !strings.Contains(no.Title+" "+no.Text, want) {
			t.Errorf("the notice lacks %q: %q / %q", want, no.Title, no.Text)
		}
	}
}
