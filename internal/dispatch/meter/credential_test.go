package meter

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// secretDir lays out a mounted secret as the cluster does: the key is a
// link through "..data" to a directory of the moment.
type secretDir struct {
	t   *testing.T
	dir string
	n   int
}

func newSecretDir(t *testing.T, token string) *secretDir {
	s := &secretDir{t: t, dir: t.TempDir()}
	s.swap(token)
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(s.dir, "token")); err != nil {
		t.Fatal(err)
	}
	return s
}

// swap writes a new directory and swaps the link to it, as the cluster does.
func (s *secretDir) swap(token string) {
	s.n++
	ts := "..2026_11_27_00_00_0" + string(rune('0'+s.n))
	must(s.t, os.Mkdir(filepath.Join(s.dir, ts), 0o755))
	must(s.t, os.WriteFile(filepath.Join(s.dir, ts, "token"), []byte(token), 0o644))
	must(s.t, os.Symlink(ts, filepath.Join(s.dir, "..data_tmp")))
	must(s.t, os.Rename(filepath.Join(s.dir, "..data_tmp"), filepath.Join(s.dir, "..data")))
}

func (s *secretDir) path() string { return filepath.Join(s.dir, "token") }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// TestR10MeterCredentialChanges: a request before a change of the mounted
// secret has the old credential, one in flight during it finishes on the
// old one, and one after has the new one, with no restart; the change is
// counted and logged without the credential.
func TestR10MeterCredentialChanges(t *testing.T) {
	sd := newSecretDir(t, "account-a-token\n")
	var logs syncBuf
	cf := &CredentialFile{Path: sd.path(), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	must(t, cf.Load())

	got, more := make(chan seen, 4), make(chan struct{})
	front := meterFor(t, model(t, got, more), &Ledger{Idle: time.Minute, Longest: time.Hour}, cf.Get)

	whole := func() string {
		resp := post(t, front, "/v1/messages?whole=1", "10.0.0.1", "Bearer placeholder")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return (<-got).auth
	}
	if a := whole(); a != "Bearer account-a-token" {
		t.Errorf("before: %q", a)
	}

	// A streamed answer is under way while the secret changes.
	during := post(t, front, "/v1/messages", "10.0.0.1", "Bearer placeholder")
	defer during.Body.Close()
	if a := (<-got).auth; a != "Bearer account-a-token" {
		t.Errorf("during, at its start: %q", a)
	}
	sd.swap("account-b-token")
	cf.Check()
	if cf.Changes() != 1 {
		t.Errorf("changes: %d", cf.Changes())
	}
	if a := whole(); a != "Bearer account-b-token" {
		t.Errorf("after: %q", a)
	}
	close(more)
	b, err := io.ReadAll(during.Body)
	if err != nil || !strings.HasSuffix(string(b), streamRest) {
		t.Errorf("the answer in flight was cut: %v %q", err, b)
	}

	l := logs.String()
	if !strings.Contains(l, LogCredentialChanged) || !strings.Contains(l, "changes=1") {
		t.Errorf("log: %s", l)
	}
	if strings.Contains(l, "account-") {
		t.Errorf("the log holds a credential: %s", l)
	}
}

// TestR10MeterCredentialNeverEmpty: an empty or missing file, or one that
// says the same again, leaves the credential in use as it was.
func TestR10MeterCredentialNeverEmpty(t *testing.T) {
	sd := newSecretDir(t, "first")
	cf := &CredentialFile{Path: sd.path(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	must(t, cf.Load())

	sd.swap("  \n")
	cf.Check()
	if v, _ := cf.Get(); v != "Bearer first" {
		t.Errorf("after an empty file: %q", v)
	}
	must(t, os.Remove(filepath.Join(sd.dir, "..data")))
	cf.Check()
	if v, _ := cf.Get(); v != "Bearer first" {
		t.Errorf("after a missing file: %q", v)
	}
	sd.swap("first")
	cf.Check()
	if cf.Changes() != 0 {
		t.Errorf("the same credential again counted as a change: %d", cf.Changes())
	}
	sd.swap("Bearer second")
	cf.Check()
	if v, _ := cf.Get(); v != "Bearer second" || cf.Changes() != 1 {
		t.Errorf("after a change: %q, %d", v, cf.Changes())
	}

	// A file that holds nothing at the start gives no credential, and the
	// first one written is taken.
	empty := newSecretDir(t, "")
	cf = &CredentialFile{Path: empty.path(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if cf.Load() == nil {
		t.Error("an empty file loaded")
	}
	if _, err := cf.Get(); err == nil {
		t.Error("a credential from an empty file")
	}
	empty.swap("late")
	cf.Check()
	if v, _ := cf.Get(); v != "Bearer late" || cf.Changes() != 0 {
		t.Errorf("the first one: %q, %d", v, cf.Changes())
	}
}
