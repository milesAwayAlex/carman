package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// R7: work that starts by itself starts only when the plan's usage is
// known to have room: a window at or over the mark, a reading too old, or
// no reading at all, is not room; a window that has reset is.
func TestR7_UsageRoomForWorkThatStartsByItself(t *testing.T) {
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	reading := func(age time.Duration, five, seven float64, resetIn time.Duration) []dispatch.Record {
		rd := reading{Model: "m", Info: limitInfo{Windows: map[string]window{
			"five_hour": {Utilization: five, ResetsAt: float64(now.Add(resetIn).Unix())},
			"seven_day": {Utilization: seven, ResetsAt: float64(now.Add(72 * time.Hour).Unix())},
		}}}
		b, _ := json.Marshal(rd)
		return []dispatch.Record{{Seq: 1, Event: dispatch.Event{Type: TypeUsageObserved, Time: now.Add(-age), Data: b}}}
	}
	for _, c := range []struct {
		name string
		rs   []dispatch.Record
		room bool
		why  string
	}{
		{"both windows low", reading(time.Minute, 0.2, 0.3, time.Hour), true, ""},
		{"the five-hour window high", reading(time.Minute, 0.7, 0.3, time.Hour), false, "five-hour window is at 70%"},
		{"the seven-day window high", reading(time.Minute, 0.2, 0.9, time.Hour), false, "at 90%"},
		{"a window high that has reset since", reading(time.Hour, 0.7, 0.3, -time.Minute), true, ""},
		{"a reading too old", reading(3*time.Hour, 0.2, 0.3, time.Hour), false, "older than"},
		{"no reading", nil, false, "no usage reading"},
	} {
		room := usageRoom(func(context.Context) ([]dispatch.Record, error) { return c.rs, nil }, 0.5, 2*time.Hour, func() time.Time { return now })
		ok, why := room(context.Background())
		if ok != c.room || !strings.Contains(why, c.why) {
			t.Errorf("%s: room %v (%q); want %v with %q", c.name, ok, why, c.room, c.why)
		}
	}
}

// R7: with pacing's word (ADR 0060), work that starts by itself starts
// when every shared window of the latest reading is expected to leave room
// unused at its reset: a usage.room recorded for that reading and window.
func TestR7_RoomFromPacingForWorkThatStartsByItself(t *testing.T) {
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	rd := reading{Model: "m", Info: limitInfo{Windows: map[string]window{
		"five_hour": {Utilization: 0.2, ResetsAt: float64(now.Add(time.Hour).Unix())},
		"seven_day": {Utilization: 0.4, ResetsAt: float64(now.Add(72 * time.Hour).Unix())},
	}}}
	b, _ := json.Marshal(rd)
	readings := func(age time.Duration) func(context.Context) ([]dispatch.Record, error) {
		return func(context.Context) ([]dispatch.Record, error) {
			return []dispatch.Record{{Seq: 7, Event: dispatch.Event{Type: TypeUsageObserved, Time: now.Add(-age), Data: b}}}, nil
		}
	}
	room := func(seq int64, win string, unused float64) dispatch.Record {
		d, _ := json.Marshal(roomData{Window: win, AtReset: 0.95 - unused, Unused: unused})
		return dispatch.Record{Event: dispatch.Event{ID: fmt.Sprintf("%s/%d/%s", TypeUsageRoom, seq, win), Type: TypeUsageRoom, Subject: win, Data: d}}
	}
	for _, c := range []struct {
		name  string
		age   time.Duration
		rooms []dispatch.Record
		room  bool
		why   string
	}{
		{"both windows have room", time.Minute, []dispatch.Record{room(7, "five_hour", 0.5), room(7, "seven_day", 0.2)}, true, "seven-day window is expected to end near 75%, 20 points unused"},
		{"one window has none", time.Minute, []dispatch.Record{room(7, "five_hour", 0.5)}, false, "does not expect the seven-day window to leave room"},
		{"the room was an earlier reading's", time.Minute, []dispatch.Record{room(6, "five_hour", 0.5), room(6, "seven_day", 0.2)}, false, "does not expect"},
		{"a reading too old", 3 * time.Hour, []dispatch.Record{room(7, "five_hour", 0.5), room(7, "seven_day", 0.2)}, false, "older than"},
	} {
		rule := paceRoom(readings(c.age), func(context.Context) ([]dispatch.Record, error) { return c.rooms, nil }, 2*time.Hour, func() time.Time { return now })
		ok, why := rule(context.Background())
		if ok != c.room || !strings.Contains(why, c.why) {
			t.Errorf("%s: room %v (%q); want %v with %q", c.name, ok, why, c.room, c.why)
		}
	}
}
