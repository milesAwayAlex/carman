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
	"carman/internal/dispatch/message"
)

type accountStore struct {
	fakeStore
	recs []dispatch.Record
}

func (a *accountStore) Emit(_ context.Context, e dispatch.Event) error {
	a.recs = append(a.recs, dispatch.Record{Seq: int64(len(a.recs) + 1), Event: e})
	return nil
}

func (a *accountStore) Last(_ context.Context, typ string, n int) ([]dispatch.Record, error) {
	var out []dispatch.Record
	for _, r := range a.recs {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// The owner and the owner's machine tell the proxy which account to hold
// through the dispatcher; a name the meter has no credential for, and a
// caller the installation does not name, are refused; nothing is changed
// before the meter has said what it has.
func TestAccountHeldIsSaidThroughTheDispatcher(t *testing.T) {
	store := &accountStore{}
	s := &Server{Store: store, Tokens: map[string]Role{"o": RoleOwner, "h": RoleHost, "a": RoleAgent, "m": RoleMeter},
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
	hold := `{"account":"two","why":"its week ends first"}`
	if code, body := call("o", "POST", "/v1/model/account", hold); code != 400 || !strings.Contains(body, "has not said which accounts") {
		t.Fatalf("before the meter spoke: %d %s", code, body)
	}
	if code, _ := call("m", "GET", "/v1/meter/accounts?have=one,two&holding=one", ""); code != 200 {
		t.Fatalf("the meter says what it has: %d", code)
	}
	if code, body := call("o", "POST", "/v1/model/account", `{"account":"three","why":"x"}`); code != 400 || !strings.Contains(body, "no credential for an account") || !strings.Contains(body, "one, two") {
		t.Fatalf("a name the meter has not: %d %s", code, body)
	}
	if code, body := call("o", "POST", "/v1/model/account", `{"account":"two"}`); code != 400 || !strings.Contains(body, "say why") {
		t.Fatalf("with no reason: %d %s", code, body)
	}
	if code, body := call("a", "POST", "/v1/model/account", hold); code != 403 || !strings.Contains(body, "egress.model.accounts.agents") {
		t.Fatalf("a caller not named: %d %s", code, body)
	}
	if code, body := call("o", "POST", "/v1/model/account", hold); code != 200 || !strings.Contains(body, `"held":"two"`) {
		t.Fatalf("the owner: %d %s", code, body)
	}
	if n := len(store.recs); n != 1 || store.recs[0].Type != "model.account" || store.recs[0].Subject != "two" ||
		!strings.Contains(string(store.recs[0].Data), `"by":"owner"`) || !strings.Contains(string(store.recs[0].Data), "its week ends first") {
		t.Fatalf("recorded: %+v", store.recs)
	}
	// What the log names, what the meter holds, and that it has not followed yet.
	if code, body := call("o", "GET", "/v1/model/account", ""); code != 200 || !strings.Contains(body, `"held":"two"`) || !strings.Contains(body, `"holds":"one"`) {
		t.Fatalf("which is held: %d %s", code, body)
	}
	_, _ = call("m", "GET", "/v1/meter/accounts?have=one,two&holding=two", "")
	if code, body := call("o", "POST", "/v1/model/account", hold); code != 400 || !strings.Contains(body, "already") {
		t.Fatalf("the same again: %d %s", code, body)
	}
	_ = message.Owner
}
