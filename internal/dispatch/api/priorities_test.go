package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carman/internal/dispatch"
	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/message"
)

type budgetLog struct{ recs []dispatch.Record }

func (l *budgetLog) Claim(_ context.Context, e dispatch.Event) (bool, error) {
	for _, r := range l.recs {
		if r.ID == e.ID {
			return false, nil
		}
	}
	l.recs = append(l.recs, dispatch.Record{Seq: int64(len(l.recs) + 1), Event: e})
	return true, nil
}

func (l *budgetLog) EventsOfTypes(_ context.Context, after int64, _ []string, _ int) ([]dispatch.Record, error) {
	var out []dispatch.Record
	for _, r := range l.recs {
		if r.Seq > after {
			out = append(out, r)
		}
	}
	return out, nil
}

// The owner and the owner's machine say what the usage is for through the
// API; a caller that is no agent's pod reads nothing of it.
func TestPrioritiesThroughTheAPI(t *testing.T) {
	s := &Server{Store: &fakeStore{}, Tokens: map[string]Role{"o": RoleOwner, "h": RoleHost, "a": RoleAgent},
		Messages: &message.Service{Place: "laptop"}, Budget: &budget.Service{Store: &budgetLog{}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(token, method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := call("o", "POST", "/v1/budget/priorities", `{"name":"platform","share":60,"default":true}`); code != 200 || !strings.Contains(body, `"part":1`) {
		t.Fatalf("the owner sets a priority: %d %s", code, body)
	}
	if code, body := call("h", "POST", "/v1/budget/priorities", `{"name":"benchmark","share":40}`); code != 200 || !strings.Contains(body, `"part":0.4`) {
		t.Fatalf("the owner's machine sets one: %d %s", code, body)
	}
	if code, body := call("h", "POST", "/v1/budget/items", `{"priority":"benchmark","title":"the run that answers","hard":true}`); code != 200 || !strings.Contains(body, `"id":"benchmark-1"`) {
		t.Fatalf("an item: %d %s", code, body)
	}
	if code, body := call("o", "GET", "/v1/budget/priorities", ""); code != 200 || !strings.Contains(body, "the run that answers") || !strings.Contains(body, `"who":"place/laptop"`) {
		t.Fatalf("the priorities: %d %s", code, body)
	}
	if code, body := call("a", "GET", "/v1/budget/priorities", ""); code != 403 {
		t.Fatalf("a caller with no pod read the priorities: %d %s", code, body)
	}
	if code, body := call("o", "POST", "/v1/budget/priorities", `{"name":"x","share":-1}`); code != 400 {
		t.Fatalf("a share below nothing: %d %s", code, body)
	}
	s.Budget = nil
	if code, _ := call("o", "GET", "/v1/budget/priorities", ""); code != 403 {
		t.Fatalf("a dispatcher with no priorities: %d", code)
	}
}
