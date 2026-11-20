package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"carman/internal/memory"
)

// A copy of the library that leaves the service: `cmem backup FILE` asks the
// service for all of it as a git bundle, and `cmem restore FILE DIR` makes a
// library of it that `cmem serve` can run. Both act on the machine where they
// are typed. Because notes are only ever added, a copy is never wrong, only
// behind.

func (c *command) backup(args []string) error {
	if _, err := c.parse(args); err != nil {
		return err
	}
	if c.flags.NArg() != 1 {
		return errors.New("name the file to write, for example: cmem backup library.bundle")
	}
	file := c.flags.Arg(0)
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("%s already exists; a backup is never written over another", file)
	}
	var export memory.Export
	if err := c.call(http.MethodGet, "/v1/library", nil, &export); err != nil {
		return err
	}
	if export.Format != "git-bundle" || len(export.Bundle) == 0 {
		return fmt.Errorf("the service answered with a copy in a form this cmem does not know (%q)", export.Format)
	}
	// Readable by its owner alone: it is everything memory holds.
	if err := os.WriteFile(file, export.Bundle, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Wrote %s: %d current entries with their whole history, as of commit %.12s.\n", file, export.Entries, export.Commit)
	fmt.Fprintf(c.stdout, "To make a library of it: cmem restore %s DIR\n", file)
	return nil
}

func (c *command) restore(args []string) error {
	if _, err := c.parse(args); err != nil {
		return err
	}
	if c.flags.NArg() != 2 {
		return errors.New("name the backup and the directory to make: cmem restore library.bundle DIR")
	}
	file, dir := c.flags.Arg(0), c.flags.Arg(1)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty; a library is restored into a new directory, never over one", dir)
	}
	bundle, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(c.ctx, "git", "clone", "--quiet", bundle, dir).CombinedOutput(); err != nil {
		return fmt.Errorf("%s is not a backup this can read: %s", file, strings.TrimSpace(string(out)))
	}
	// The copy stands by itself; it does not point back at the file it came from.
	exec.CommandContext(c.ctx, "git", "-C", dir, "remote", "remove", "origin").Run()
	store, err := memory.Open(dir)
	if err != nil {
		return err
	}
	problems, err := store.Check()
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("the restored library has %d problem(s); the first: %s", len(problems), problems[0])
	}
	fmt.Fprintf(c.stdout, "Restored a library at %s: %d current entries.\n", dir, len(store.CurrentEntries(timeZero)))
	fmt.Fprintf(c.stdout, "To serve it on this machine:\n  CMEM_TOKENS=laptop=SOME-TOKEN cmem serve --library %s --listen 127.0.0.1:8378\n"+
		"and point sessions at it with CMEM_URL=http://127.0.0.1:8378 and CMEM_TOKEN=SOME-TOKEN.\n", dir)
	return nil
}
