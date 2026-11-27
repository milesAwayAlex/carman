package message

import (
	"testing"
	"time"
)

// TestR2LimitReset: the harness's words at the plan's limit are known, and
// when it resets is read from them (ADR new-a-limit-is-an-outcome).
func TestR2LimitReset(t *testing.T) {
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for words, want := range map[string]time.Time{
		"You've hit your 5-hour limit · resets 10:40am (UTC)":                          time.Date(2026, 11, 27, 10, 40, 0, 0, time.UTC),
		"You've hit your individual spend limit · your session limit resets 6am (UTC)": time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC),
		"You've hit your weekly limit · resets Dec 1, 5pm (UTC)":                       time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC),
		"Claude usage limit reached.":                                                  {},
	} {
		got, ok := LimitReset(words, now)
		if !ok || !got.Equal(want) {
			t.Errorf("LimitReset(%q) = %v, %v; want %v", words, got, ok, want)
		}
	}
	for _, words := range []string{"Done: three entries merged.", "The limit on runs per sender is 24."} {
		if _, ok := LimitReset(words, now); ok {
			t.Errorf("LimitReset(%q) says it is the plan's limit", words)
		}
	}
}
