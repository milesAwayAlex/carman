package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

type fakeStore struct {
	dispatch.Dispatcher // calls not overridden panic
	emitted             []dispatch.Event
	submitted           []dispatch.TaskSpec
}

func (f *fakeStore) Emit(_ context.Context, e dispatch.Event) error {
	f.emitted = append(f.emitted, e)
	return nil
}
func (f *fakeStore) Submit(_ context.Context, t dispatch.TaskSpec) error {
	f.submitted = append(f.submitted, t)
	return nil
}
func (f *fakeStore) Explain(_ context.Context, id string) (dispatch.TaskStatus, error) {
	for _, t := range f.submitted {
		if t.ID == id {
			return dispatch.TaskStatus{Spec: t, State: dispatch.Pending}, nil
		}
	}
	return dispatch.TaskStatus{}, dispatch.ErrNotFound
}
func (f *fakeStore) Tasks(context.Context, dispatch.State, int) ([]dispatch.TaskStatus, error) {
	return nil, nil
}
func (f *fakeStore) EventsOf(context.Context, int64, string, int) ([]dispatch.Record, error) {
	return nil, nil
}
func (f *fakeStore) Last(context.Context, string, int) ([]dispatch.Record, error) { return nil, nil }
func (f *fakeStore) Events(context.Context, int64, int) ([]dispatch.Record, error) {
	return nil, nil
}

// TestR7Roles: an agent may only emit, not as the dispatcher or its
// watcher, and under an ID marked as its own; the backup may only read a
// dump; the owner submits, and a task whose class names no template is
// refused at once.
func TestR7Roles(t *testing.T) {
	store := &fakeStore{}
	s := &Server{
		Store:  store,
		Tokens: map[string]Role{"o": RoleOwner, "a": RoleAgent, "b": RoleBackup},
		Check: func(_ context.Context, spec dispatch.TaskSpec) error {
			if spec.Class != "sleeper" {
				return dispatch.ErrInvalid
			}
			return nil
		},
		Dump: func(_ context.Context, f *os.File) error { _, err := f.WriteString("-- dump\n"); return err },
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(token, method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	task := `{"id":"t1","class":"sleeper","attempt_timeout":"1m"}`
	for _, c := range []struct {
		token, method, path, body string
		want                      int
	}{
		{"nobody", "GET", "/v1/tasks?state=pending", "", 401},
		{"a", "POST", "/v1/events", `{"id":"e1","type":"ticket.opened","subject":"T-1","source":"desk"}`, 200},
		{"a", "POST", "/v1/events", `{"type":"task.finished","subject":"t1"}`, 403},
		{"a", "POST", "/v1/events", `{"type":"job.ended"}`, 403},
		{"a", "POST", "/v1/tasks", task, 403},
		{"a", "GET", "/v1/events", "", 403},
		{"a", "GET", "/v1/dump", "", 403},
		{"b", "POST", "/v1/events", `{"type":"x"}`, 403},
		{"b", "GET", "/v1/tasks/t1", "", 403},
		{"b", "GET", "/v1/dump", "", 200},
		{"o", "POST", "/v1/tasks", `{"id":"t2","class":"nothing","attempt_timeout":"1m"}`, 400},
		{"o", "POST", "/v1/tasks", `{"id":"t3","class":"sleeper"}`, 400},
		{"o", "POST", "/v1/tasks", task, 200},
		{"o", "GET", "/v1/tasks/t1", "", 200},
		{"o", "GET", "/v1/tasks/none", "", 404},
		{"o", "POST", "/v1/events", `{"id":"f","type":"task.finished","subject":"t1"}`, 200},
		{"o", "GET", "/v1/events?type=job.ended&last=5", "", 200},
	} {
		if got := call(c.token, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s as %s: %d, want %d", c.method, c.path, c.body, c.token, got, c.want)
		}
	}
	if len(store.submitted) != 1 || store.submitted[0].ID != "t1" {
		t.Errorf("submitted %+v", store.submitted)
	}
	if len(store.emitted) != 2 || store.emitted[0].ID != "agent/e1" || store.emitted[0].Source != "agent/desk" ||
		store.emitted[1].ID != "f" || store.emitted[1].Source != "owner" {
		t.Errorf("emitted %+v", store.emitted)
	}
}

// TestR2HostRole: the host role's token is a session on the owner's machine
// (ADR 0049): it may message, as the place the dispatcher names, and do
// nothing else; with no place named, it is refused.
func TestR2HostRole(t *testing.T) {
	s := &Server{Store: &fakeStore{}, Tokens: map[string]Role{"h": RoleHost},
		Messages: &message.Service{Place: "host"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer h")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := call("GET", "/v1/whoami", ""); code != 200 || !strings.Contains(body, `"host":true`) || !strings.Contains(body, `"agent":"host"`) {
		t.Errorf("whoami as the host: %d %s", code, body)
	}
	for _, c := range [][2]string{{"POST", "/v1/events"}, {"POST", "/v1/tasks"}, {"GET", "/v1/events"}, {"GET", "/v1/dump"}} {
		if code, _ := call(c[0], c[1], `{"type":"x"}`); code != 403 {
			t.Errorf("%s %s as the host: %d, want 403", c[0], c[1], code)
		}
	}
	s.Messages.Place = ""
	if code, _ := call("GET", "/v1/whoami", ""); code != 401 {
		t.Errorf("the host's token where no place is named: %d, want 401", code)
	}
	if _, err := ParseTokens("owner=o,host=h"); err != nil {
		t.Errorf("a host token: %v", err)
	}
}

// TestR10MeterRole: the egress proxy's pod (ADR 0050) may emit what agents
// used of the model and nothing else; nobody else but the owner may emit
// model.*, so an agent cannot add to or answer for the proxy's record.
func TestR10MeterRole(t *testing.T) {
	store := &fakeStore{}
	s := &Server{
		Store:  store,
		Tokens: map[string]Role{"o": RoleOwner, "a": RoleAgent},
		Verify: func(_ context.Context, tok string) (message.Caller, error) {
			switch tok {
			case "meter.pod.token":
				return message.Caller{Meter: true, Pod: "egress-1"}, nil
			case "agent.pod.token":
				return message.Caller{Agent: "dreamer", Pod: "dreamer-1"}, nil
			}
			return message.Caller{}, errors.New("no")
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(token, method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		token, method, path, body string
		want                      int
	}{
		{"meter.pod.token", "POST", "/v1/events", `{"id":"u1","type":"model.usage","subject":"dreamer","source":"proxy"}`, 200},
		{"meter.pod.token", "POST", "/v1/events", `{"type":"run.usage","subject":"dreamer"}`, 403},
		{"meter.pod.token", "GET", "/v1/events", "", 403},
		{"meter.pod.token", "GET", "/v1/inbox", "", 403},
		{"agent.pod.token", "POST", "/v1/events", `{"type":"model.usage","subject":"dreamer"}`, 403},
		{"a", "POST", "/v1/events", `{"type":"model.usage","subject":"dreamer"}`, 403},
		{"o", "POST", "/v1/events", `{"type":"model.usage","subject":"dreamer"}`, 200},
	} {
		if got := call(c.token, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s as %s: %d, want %d", c.method, c.path, c.body, c.token, got, c.want)
		}
	}
	if len(store.emitted) != 2 || store.emitted[0].ID != "meter/u1" || store.emitted[0].Source != "meter/proxy" {
		t.Errorf("emitted %+v", store.emitted)
	}
}
