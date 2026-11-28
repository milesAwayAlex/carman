package main

import (
	"os"
	"testing"
)

// TestMain keeps the tests' records of harnesses out of the machine's own:
// a hook run by a test as a session of the owner's machine would otherwise
// write under the home of whoever runs the tests, where a watcher reads.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "harness")
	if err != nil {
		panic(err)
	}
	os.Setenv("DISPATCHER_HARNESS_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
