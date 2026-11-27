package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestR2ClientShowsTheTokenOfNow: a pod's token for the dispatcher lasts
// an hour and the cluster replaces the file before then; a client that
// runs for longer (a session's listener, the watcher) shows at each call
// what the file holds then, not what it held at its start.
func TestR2ClientShowsTheTokenOfNow(t *testing.T) {
	var shown []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shown = append(shown, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	call := func(c *Client) {
		t.Helper()
		if err := c.Do(context.Background(), http.MethodGet, "/", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	write("first\n")
	c := &Client{URL: srv.URL, Token: "first", TokenFile: file}
	call(c)
	write("second\n")
	call(c)
	// A file that is gone or empty for a moment: the token it started with.
	write("")
	call(c)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	call(c)
	// No file named: Token, as before.
	call(&Client{URL: srv.URL, Token: "plain"})
	want := []string{"Bearer first", "Bearer second", "Bearer first", "Bearer first", "Bearer plain"}
	if len(shown) != len(want) {
		t.Fatalf("shown %q, want %q", shown, want)
	}
	for i := range want {
		if shown[i] != want[i] {
			t.Errorf("call %d showed %q, want %q", i+1, shown[i], want[i])
		}
	}
}
