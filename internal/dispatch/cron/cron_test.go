package cron

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The arithmetic of "the next tick after T", over a table of times,
// including a zone on the days its clock changes.
func TestR2NextAndPrev(t *testing.T) {
	for _, c := range []struct {
		expr, zone, after, next string
	}{
		{"* * * * *", "", "2026-11-24T10:00:00Z", "2026-11-24T10:01:00Z"},
		{"* * * * *", "", "2026-11-24T10:00:30Z", "2026-11-24T10:01:00Z"},
		{"17 * * * *", "", "2026-11-24T10:17:00Z", "2026-11-24T11:17:00Z"},
		{"17 10 * * *", "", "2026-11-24T10:16:59Z", "2026-11-24T10:17:00Z"},
		{"17 10 * * *", "", "2026-12-31T10:17:00Z", "2027-01-01T10:17:00Z"},
		{"41 */12 * * *", "", "2026-11-24T12:41:00Z", "2026-11-25T00:41:00Z"},
		{"0 15 * * 1", "", "2026-11-24T00:00:00Z", "2026-11-30T15:00:00Z"}, // Tuesday to Monday
		{"0 0 29 2 *", "", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"0 0 1,15 * MON", "", "2026-11-24T00:00:00Z", "2026-11-30T00:00:00Z"}, // either day field
		{"0 9 * * 1-5", "Europe/Paris", "2026-11-24T08:00:00Z", "2026-11-25T08:00:00Z"},
		{"@hourly", "", "2026-11-24T10:59:59Z", "2026-11-24T11:00:00Z"},
		{"*/10 * * * * *", "", "2026-11-24T10:00:01Z", "2026-11-24T10:00:10Z"},
		// New York puts its clock forward at 02:00 on 2026-03-08: 02:30 is
		// never shown that day, so it does not tick until the next.
		{"30 2 * * *", "America/New_York", "2026-03-07T08:00:00Z", "2026-03-09T06:30:00Z"},
		{"0 * * * *", "America/New_York", "2026-03-08T06:30:00Z", "2026-03-08T07:00:00Z"}, // 01:00 EST to 03:00 EDT
		// It puts it back at 02:00 on 2026-11-01: 01:30 is shown twice.
		{"30 1 * * *", "America/New_York", "2026-11-01T00:00:00Z", "2026-11-01T05:30:00Z"},
		{"30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"},
		{"30 1 * * *", "America/New_York", "2026-11-01T06:30:00Z", "2026-11-02T06:30:00Z"},
		{"0 3 * * *", "Europe/London", "2026-10-24T12:00:00Z", "2026-10-25T03:00:00Z"}, // after the change, GMT
		{"0 3 * * *", "Europe/London", "2026-03-28T12:00:00Z", "2026-03-29T02:00:00Z"}, // after the change, BST
	} {
		e, err := Parse(c.expr, c.zone)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		after, want := at(t, c.after), at(t, c.next)
		if got := e.Next(after); !got.Equal(want) {
			t.Errorf("next tick of %q (%s) after %s is %s, want %s", c.expr, c.zone, c.after, got.UTC().Format(time.RFC3339), c.next)
		}
		// The tick is the last at or before itself, and the one before it
		// is the last before it.
		if got := e.Prev(want); !got.Equal(want) {
			t.Errorf("last tick of %q at or before %s is %s", c.expr, c.next, got.UTC().Format(time.RFC3339))
		}
		if p := e.Prev(want.Add(-time.Second)); !p.IsZero() && !e.Next(p).Equal(want) {
			t.Errorf("%q: the tick before %s is %s, whose next is %s", c.expr, c.next, p.UTC(), e.Next(p).UTC())
		}
	}
}

func TestR2Count(t *testing.T) {
	e, _ := Parse("* * * * *", "")
	a, b := at(t, "2026-11-24T10:00:00Z"), at(t, "2026-11-24T10:05:00Z")
	if n := e.Count(a, b, 100); n != 4 {
		t.Errorf("ticks strictly between 10:00 and 10:05: %d, want 4", n)
	}
	if n := e.Count(a, b, 2); n != 2 {
		t.Errorf("counting at most 2: %d", n)
	}
	never, _ := Parse("0 0 30 2 *", "")
	if !never.Next(a).IsZero() || !never.Prev(a).IsZero() {
		t.Error("30 February ticks")
	}
}

func TestR2ParseRefuses(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "x * * * *", "* * * * * * *"} {
		if _, err := Parse(bad, ""); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := Parse("* * * * *", "Mars/Olympus"); err == nil {
		t.Error("an unknown zone was accepted")
	}
}
