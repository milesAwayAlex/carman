package notice

import (
	"context"
	"encoding/json"
	"fmt"
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
	f.add(kube.TypeJobEnded, "agent-x", kube.JobEnded{Job: "agent-x-29400000", Template: "agent-x", By: "cronjob", Outcome: "failed", Reason: "BackoffLimitExceeded", ExitCode: &one,
		Why: &kube.Why{Termination: "OOMKilled", Killed: "memory", TerminationMessage: "note-of-its-own", LogTail: []string{"a-line-of-its-log"}}})
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
	if !strings.Contains(w[0].Text, "kubectl --context CTX -n ns logs job/agent-x-29400000") || !strings.Contains(w[0].Text, "exit code 1, OOMKilled, killed for its memory.") ||
		!strings.Contains(w[0].Text, "scripts/deploy.sh CTX dispatch log --type job.ended") || strings.Contains(w[0].Text, "note-of-its-own") || strings.Contains(w[0].Text, "a-line-of-its-log") {
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

// TestR10AMessageToTheOwnerSaysWhatItIsAbout: the notice for a message to
// the owner carries the message's first line, which says roughly what it is
// about, and how to read and answer it; the rest of the message stays in
// the log, since the notice's channel is not private (ADR 0048).
func TestR10AMessageToTheOwnerSaysWhatItIsAbout(t *testing.T) {
	n := &Notifier{}
	data, _ := json.Marshal(map[string]string{"id": "m1", "from": "session/s1", "agent": "builder", "name": "message", "to": "owner",
		"body": "The weekly review is done and one pool needs a decision.\n\nThe pool behind account 4112-88 costs more than its sibling."})
	no, ok := n.Compose(context.Background(), dispatch.Record{Event: dispatch.Event{Type: TypeMessage, Subject: "m1", Data: data}})
	if !ok {
		t.Fatal("a message to the owner is a notice")
	}
	if strings.Contains(no.Text, "4112-88") || strings.Contains(no.Title, "4112-88") {
		t.Errorf("the notice carries more than the first line: %q / %q", no.Title, no.Text)
	}
	for _, want := range []string{"The weekly review is done and one pool needs a decision.", "dispatch message m1", "dispatch reply m1", "builder", "To go to its session: carman go s1 (", "CARMAN_CHECKOUT"} {
		if !strings.Contains(no.Title+" "+no.Text, want) {
			t.Errorf("the notice lacks %q: %q / %q", want, no.Title, no.Text)
		}
	}
	// The owner's way to a session is carman go (ADR 0057), by the start of
	// the id and no more of it; the checkout's script is no longer named.
	if strings.Contains(no.Text, "agent resume") {
		t.Errorf("the notice names the checkout's script: %q", no.Text)
	}
	data, _ = json.Marshal(map[string]string{"id": "m3", "from": "session/0123456789abcdef", "agent": "desk", "name": "vpn", "to": "owner", "body": "Done."})
	no, _ = n.Compose(context.Background(), dispatch.Record{Event: dispatch.Event{Type: TypeMessage, Subject: "m3", Data: data}})
	if !strings.Contains(no.Text, "carman go 01234567 (") {
		t.Errorf("a long id is not cut to its start: %q", no.Text)
	}
	// A session on the owner's own machine is in a terminal of theirs:
	// there is nowhere to be taken.
	n.Place = "host"
	data, _ = json.Marshal(map[string]string{"id": "m2", "from": "session/0123456789abcdef", "agent": "host", "name": "proj", "to": "owner", "body": "Done."})
	no, _ = n.Compose(context.Background(), dispatch.Record{Event: dispatch.Event{Type: TypeMessage, Subject: "m2", Data: data}})
	if strings.Contains(no.Text, "carman go") {
		t.Errorf("a notice from the owner's machine offers to go to its session: %q", no.Text)
	}
}

// TestR10ANoticeIsCheckedBeforeItIsHandedOn: a notice whose words look like
// a credential, or match a shape the installation names, is replaced by one
// that says it was withheld and where to read it, and the replacement is
// recorded; one with neither goes as written (ADR 0048).
func TestR10ANoticeIsCheckedBeforeItIsHandedOn(t *testing.T) {
	shapes, err := Shapes("[a-z0-9.]+@example\\.org\n\\bacct-[0-9]{6}\\b\n(unclosed\n")
	if err == nil || len(shapes) != 3 || !shapes[2].MatchString("an (UNCLOSED one") {
		t.Fatalf("shapes %v, %v", shapes, err)
	}
	f := &fakeLog{mark: []byte(`{"seq":0}`)}
	n := &Notifier{Log: f, Enabled: true, Class: "notify", Context: "ctx", Namespace: "ns", Withhold: shapes, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i, first := range []string{
		"Done: the token is ghp_" + strings.Repeat("a1", 20),
		"Write to Ops@Example.org about it",
		"The pool for acct-123456 is full",
		"The weekly review is done.",
	} {
		f.add(TypeMessage, "m", map[string]string{"id": fmt.Sprintf("m%d", i), "from": "session/s1", "agent": "builder", "to": "owner", "body": first + "\nmore"})
	}
	if err := n.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.submitted) != 4 {
		t.Fatalf("%d notices sent", len(f.submitted))
	}
	for i, task := range f.submitted {
		var p kube.Payload
		json.Unmarshal(task.Payload, &p)
		words := p.Env["TASK_TITLE"] + " " + p.Env["TASK_TEXT"]
		held := strings.Contains(words, "A notice was withheld")
		if held != (i < 3) || strings.Contains(words, "ghp_") || strings.Contains(strings.ToLower(words), "example.org") || strings.Contains(words, "acct-") {
			t.Errorf("notice %d: %q", i, words)
		}
		if held && !strings.Contains(words, fmt.Sprintf("dispatch message m%d", i)) {
			t.Errorf("notice %d does not say where to read it: %q", i, words)
		}
	}
	replaced := 0
	for _, r := range f.recs {
		if r.Type == TypeReplaced {
			replaced++
		}
	}
	if replaced != 3 {
		t.Errorf("%d replacements recorded", replaced)
	}
}
