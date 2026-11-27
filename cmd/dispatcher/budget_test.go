package main

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/meter"
)

// TestR7FreshReadingsSkipped: the reading job leaves out a model whose
// standing was read within its period, less a margin for its own last
// reading.
func TestR7FreshReadingsSkipped(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	latest := map[string]time.Time{
		"cheap":  now.Add(-20 * time.Minute), // the proxy saw it lately
		"hourly": now.Add(-55 * time.Minute), // the job's own, an hour ago
		"strong": now.Add(-4 * time.Hour),    // read every 6 hours: fresh
	}
	got := freshFilter([]string{"cheap=1", "hourly=1", "strong=6", "never=6"}, latest, now, io.Discard)
	if !slices.Equal(got, []string{"hourly=1", "never=6"}) {
		t.Errorf("due: %v", got)
	}
}

func TestR7LimitWords(t *testing.T) {
	for in, want := range map[string]int64{"5M": 5_000_000, "800k": 800_000, "1.5m": 1_500_000, "1234": 1234} {
		if n, err := parseTokens(in); err != nil || n != want {
			t.Errorf("%s: %d %v", in, n, err)
		}
	}
	if _, err := parseTokens("lots"); err == nil {
		t.Error("lots read as a number")
	}
	lines := strings.Join(limitLines(meter.Standing{Day: "2026-11-27", Agents: map[string]meter.AgentStanding{
		"desk":    {Limit: meter.Limit{Day: 4_000_000}, Used: 4_100_000, By: "owner", Refused: 2},
		"dreamer": {Limit: meter.Limit{Pod: 1_000_000}, Pods: map[string]int64{"u1": 300_000}, By: "definition"},
	}}), "\n")
	for _, want := range []string{"desk: today 4.1M of 4.0M (102%) SPENT; 2 calls refused today (set by owner)", "dreamer: a pod at most 1.0M, the fullest 300k (set by definition)"} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in:\n%s", want, lines)
		}
	}
}
