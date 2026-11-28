package message

import (
	"testing"
	"time"
)

// TestR2MessageAtATimeReadsTheTime: a clock time in UTC is the next time it
// is that, a duration is from now, a date and time is itself.
func TestR2MessageAtATimeReadsTheTime(t *testing.T) {
	now := time.Date(2026, 3, 1, 14, 2, 30, 0, time.UTC)
	for text, want := range map[string]time.Time{
		"15:40":                     time.Date(2026, 3, 1, 15, 40, 0, 0, time.UTC),
		"15:40 UTC":                 time.Date(2026, 3, 1, 15, 40, 0, 0, time.UTC),
		"15:40Z":                    time.Date(2026, 3, 1, 15, 40, 0, 0, time.UTC),
		"9:05":                      time.Date(2026, 3, 2, 9, 5, 0, 0, time.UTC),
		"14:02":                     time.Date(2026, 3, 1, 14, 2, 0, 0, time.UTC), // the minute that is now: at once
		"14:01":                     time.Date(2026, 3, 2, 14, 1, 0, 0, time.UTC),
		"2026-03-02T08:00:00":       time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
		"2026-03-02 08:00:00":       time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
		"90m":                       now.Add(90 * time.Minute),
		"+1h30m":                    now.Add(90 * time.Minute),
		"45s":                       now.Add(45 * time.Second),
		"2026-03-02T08:00Z":         time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
		"2026-03-02 08:00":          time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
		"2026-03-02T08:00:00Z":      time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
		"2026-03-02T10:00:00+02:00": time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
	} {
		got, err := ParseAt(text, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "soon", "25:00", "15:61", "-5m", "tomorrow", "3pm"} {
		if got, err := ParseAt(text, now); err == nil {
			t.Errorf("%q was read as %v", text, got)
		}
	}
}
