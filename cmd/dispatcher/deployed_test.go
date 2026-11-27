package main

import (
	"testing"
	"time"

	"carman/internal/dispatch/api"
)

func TestDeployedText(t *testing.T) {
	got := deployedText(api.Deployed{Commit: "abc1234", Previous: "0ff1ce0", Since: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC),
		Commits: []string{"abc1234 the latest"}, More: 1})
	want := "deployed: abc1234 since 2026-11-27T09:00:00Z\nbefore it: 0ff1ce0; the commits it brought, newest first:\n  abc1234 the latest\n  and 1 more\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
