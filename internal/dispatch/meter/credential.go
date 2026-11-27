package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogCredentialChanged is what the meter logs when it takes a new
// credential, with the count of changes since it started (ADR 0055).
// scripts/deploy.sh waits for it.
const LogCredentialChanged = "the model's credential changed"

// CredentialFile holds the model's credential, read from a file a secret
// is mounted as. The cluster changes such a file by writing a new
// directory beside it and swapping the symlink "..data" to it, so the file
// is read again when that link or the file itself changes.
type CredentialFile struct {
	Path string
	// Every is how often the file is looked at (default 2s).
	Every time.Duration
	Log   *slog.Logger

	mu      sync.RWMutex
	value   string
	sig     string
	changes int
}

// Load reads the file the first time; it fails if the file holds nothing.
func (c *CredentialFile) Load() error {
	sig, v, err := c.read()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sig, c.value = sig, v
	c.mu.Unlock()
	c.log().Info("the model's credential is read")
	return nil
}

// Get is the Authorization header value in use now.
func (c *CredentialFile) Get() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.value == "" {
		return "", errors.New("no credential has been read")
	}
	return c.value, nil
}

// Changes is how many times the credential has changed since Load.
func (c *CredentialFile) Changes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.changes
}

// Run looks at the file until ctx ends.
func (c *CredentialFile) Run(ctx context.Context) {
	every := c.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Check()
		}
	}
}

// Check reads the file again if it changed, and takes what it holds unless
// that is empty or unreadable.
func (c *CredentialFile) Check() {
	c.mu.RLock()
	old, had := c.sig, c.value != ""
	c.mu.RUnlock()
	if s, err := c.signature(); err == nil && s == old && had {
		return
	}
	sig, v, err := c.read()
	if err != nil {
		c.log().Warn("the model's credential file cannot be used; keeping the credential in use", "error", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sig = sig
	if v == c.value {
		return
	}
	first := c.value == ""
	c.value = v
	if first {
		c.log().Info("the model's credential is read")
		return
	}
	c.changes++
	c.log().Info(LogCredentialChanged, "changes", c.changes)
}

// read takes the file only when it did not change while being read, so a
// file written in place is not taken half-written.
func (c *CredentialFile) read() (string, string, error) {
	before, err := c.signature()
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(c.Path)
	if err != nil {
		return "", "", err
	}
	after, err := c.signature()
	if err != nil {
		return "", "", err
	}
	if before != after {
		return "", "", errors.New("the file changed while it was read")
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", "", errors.New("the file is empty")
	}
	if !strings.HasPrefix(v, "Bearer ") {
		v = "Bearer " + v
	}
	return after, v, nil
}

// signature changes when the cluster swaps the link or the file changes;
// it holds nothing of what the file says.
func (c *CredentialFile) signature() (string, error) {
	link, _ := os.Readlink(filepath.Join(filepath.Dir(c.Path), "..data"))
	fi, err := os.Stat(c.Path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s|%d|%d", link, fi.Size(), fi.ModTime().UnixNano()), nil
}

func (c *CredentialFile) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
