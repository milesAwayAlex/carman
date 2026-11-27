package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
)

// TypeUsageRoom is recorded when a window is expected to arrive at its
// reset with room under its target left unused at the present rate
// (ADR 0060); its subject is the window, its data a roomData.
const TypeUsageRoom = "usage.room"

// windowLength is how long a window lasts, by its name; zero for a window
// whose length is not known, which is not paced.
func windowLength(n string) time.Duration {
	switch {
	case n == "five_hour":
		return 5 * time.Hour
	case strings.HasPrefix(n, "seven_day"):
		return 7 * 24 * time.Hour
	}
	return 0
}

// rateSpan is how far back from the latest reading the rate is measured:
// two hours of a five-hour window, a day of a seven-day one.
func rateSpan(length time.Duration) time.Duration {
	if length <= 5*time.Hour {
		return 2 * time.Hour
	}
	return 24 * time.Hour
}

// pace is where one window stands against its line: a straight line from
// zero at the window's start to the target at its reset.
type pace struct {
	Window string    // its name, five_hour
	Model  string    // "" for a shared window, else the model whose own it is
	ReadAt time.Time // the latest reading of it
	Used   float64   // utilization at that reading
	Start  time.Time
	Reset  time.Time
	Target float64
	Line   float64 // the line's value now
	Rate   float64 // utilization per hour, lately
	// RateSince is the earliest point the rate was measured from; the
	// window's start when there was no earlier reading of it.
	RateSince time.Time
	Reset0    bool // the reset has passed since the latest reading
}

// lineAt is the line's value at t.
func (p pace) lineAt(t time.Time) float64 {
	f := float64(t.Sub(p.Start)) / float64(p.Reset.Sub(p.Start))
	return p.Target * math.Min(1, math.Max(0, f))
}

// Ahead is how far above its line the window is, in utilization; negative
// when under it.
func (p pace) Ahead() float64 { return p.Used - p.Line }

// AtReset is the utilization expected at the reset at the present rate.
func (p pace) AtReset() float64 {
	return math.Min(1, p.Used+p.Rate*p.Reset.Sub(p.ReadAt).Hours())
}

// Unused is how much under the target is expected to be left at the reset.
func (p pace) Unused() float64 { return math.Max(0, p.Target-p.AtReset()) }

// ReachesTarget is when the target is reached at the present rate, zero
// when not before the reset.
func (p pace) ReachesTarget() time.Time {
	if p.Used >= p.Target {
		return p.ReadAt
	}
	if p.Rate <= 0 {
		return time.Time{}
	}
	t := p.ReadAt.Add(time.Duration((p.Target - p.Used) / p.Rate * float64(time.Hour)))
	if t.Before(p.Reset) {
		return t
	}
	return time.Time{}
}

// OnLineAt is when the line reaches the window's present utilization: if
// nothing more were used, when work held for it would be admitted.
func (p pace) OnLineAt() time.Time {
	if p.Used >= p.Target {
		return p.Reset
	}
	return p.Start.Add(time.Duration(p.Used / p.Target * float64(p.Reset.Sub(p.Start))))
}

// point is one reading of one window.
type point struct {
	at    time.Time
	used  float64
	reset time.Time
}

// sameReset says two readings are of the same window: a reset can move
// by a little between readings, a new window's by its length.
func sameReset(a, b time.Time) bool { return a.Sub(b).Abs() < 30*time.Minute }

// paces is every window's pace by the readings, in the log's order and
// since the account last changed: the shared windows from every model's
// readings, a model's own from its own.
func paces(rs []dispatch.Record, now time.Time, target float64) []pace {
	type key struct{ window, model string }
	series := map[key][]point{}
	for _, r := range rs {
		var rd reading
		if json.Unmarshal(r.Data, &rd) != nil {
			continue
		}
		for n, w := range rd.Info.Windows {
			if windowLength(n) == 0 || w.ResetsAt == 0 {
				continue
			}
			k := key{window: n}
			if !sharedWindows[n] {
				k.model = rd.Model
			}
			series[k] = append(series[k], point{at: r.Time, used: w.Utilization, reset: w.resets()})
		}
	}
	var out []pace
	for k, ps := range series {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].at.Before(ps[j].at) })
		out = append(out, paceOf(k.window, k.model, ps, now, target))
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Model == "") != (out[j].Model == "") {
			return out[i].Model == ""
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Window < out[j].Window
	})
	return out
}

// paceOf is one window's pace from its readings, oldest first. Only the
// readings of the latest reading's window count; the window's start, at
// zero, is a reading of it too. The rate is the slope from the earliest
// reading within rateSpan of the latest (or the latest one before it) to
// the latest: with one reading, its average since the window began.
func paceOf(n, model string, ps []point, now time.Time, target float64) pace {
	last := ps[len(ps)-1]
	length := windowLength(n)
	p := pace{Window: n, Model: model, ReadAt: last.at, Used: last.used, Reset: last.reset,
		Start: last.reset.Add(-length), Target: target}
	if !p.Reset.After(now) {
		// It has reset since: a new window, at zero, begins.
		p.Reset0 = true
		for !p.Reset.After(now) {
			p.Reset = p.Reset.Add(length)
		}
		p.Start = p.Reset.Add(-length)
		p.Used, p.ReadAt, p.RateSince = 0, p.Start, p.Start
		p.Line = p.lineAt(now)
		return p
	}
	from := point{at: p.Start, used: 0}
	for i := len(ps) - 2; i >= 0; i-- {
		q := ps[i]
		if !sameReset(q.reset, last.reset) || !q.at.After(p.Start) {
			break
		}
		from = q
		if last.at.Sub(q.at) >= rateSpan(length) {
			break
		}
	}
	if h := last.at.Sub(from.at).Hours(); h > 0 {
		p.Rate = math.Max(0, (last.used-from.used)/h)
	}
	p.RateSince = from.at
	p.Line = p.lineAt(now)
	return p
}

// pct says a utilization as whole percentage points.
func pct(u float64) string { return fmt.Sprintf("%.0f%%", u*100) }

// paceLine is a window's pace as a person reads it at a glance.
func paceLine(p pace, now time.Time) string {
	name := strings.ReplaceAll(p.Window, "_", "-")
	if p.Model != "" {
		name += " (" + p.Model + ")"
	}
	when := func(t time.Time) string {
		if t.Sub(now) < 24*time.Hour {
			return t.Format("15:04 UTC")
		}
		return t.Format("Mon 15:04 UTC")
	}
	head := fmt.Sprintf("  %-22s %3s, line %3s (%s by %s, in %s)", name, pct(p.Used), pct(p.Line), pct(p.Target), when(p.Reset), span(p.Reset.Sub(now)))
	if p.Reset0 {
		return head + ": reset since the last reading, no reading since"
	}
	rate := fmt.Sprintf("%.1f%%/h", p.Rate*100)
	if reach := p.ReachesTarget(); !reach.IsZero() {
		state := "AHEAD by " + points(p.Ahead())
		if p.Ahead() <= 0 {
			state = "on or under its line"
		}
		return fmt.Sprintf("%s: %s; at %s it reaches %s at %s, %s before its reset", head, state, rate, pct(p.Target), when(reach), span(p.Reset.Sub(reach)))
	}
	if p.Ahead() > 0 {
		return fmt.Sprintf("%s: AHEAD by %s; at %s it ends near %s", head, points(p.Ahead()), rate, pct(p.AtReset()))
	}
	return fmt.Sprintf("%s: under by %s; at %s it ends near %s, %s UNUSED", head, points(-p.Ahead()), rate, pct(p.AtReset()), points(p.Unused()))
}

// span says a duration to the minute, in days and hours past a day.
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

func points(u float64) string { return fmt.Sprintf("%.0f points", u*100) }

// paceLines is every window's pace, for dispatch status.
func paceLines(rs []dispatch.Record, now time.Time, target float64) []string {
	ps := paces(rs, now, target)
	if len(ps) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("  pace, against a line to %s at each reset:", pct(target))}
	for _, p := range ps {
		out = append(out, paceLine(p, now))
	}
	return out
}

// curve is the pacing admission (ADR 0060): below the priority, a task
// runs while each window it draws on (the shared ones and its first
// model's own) is on or under its line, or resets within soon with room
// under the target left. It says when it expects to admit a held task:
// when the line reaches the window, if nothing more were used.
func (b budgetRule) curve(rs []dispatch.Record, ladder []string, now time.Time) (bool, string) {
	var held []string
	var until time.Time
	for _, p := range paces(rs, now, b.Target) {
		if p.Model != "" && (len(ladder) == 0 || p.Model != ladder[0]) {
			continue
		}
		if p.Reset0 || now.Sub(p.ReadAt) > b.MaxAge || p.Ahead() <= 0 {
			continue
		}
		left := p.Reset.Sub(now)
		if left <= b.Soon && p.Used < p.Target {
			continue
		}
		at := p.OnLineAt()
		if p.Used < p.Target && p.Reset.Add(-b.Soon).Before(at) {
			at = p.Reset.Add(-b.Soon)
		}
		if at.After(until) {
			until = at
		}
		held = append(held, fmt.Sprintf("%s is at %s, ahead of its line (%s now, %s at its reset at %s)",
			windowName(p.Window), pct(p.Used), pct(p.Line), pct(p.Target), p.Reset.Format("2006-01-02 15:04 UTC")))
	}
	if len(held) == 0 {
		return true, ""
	}
	return false, strings.Join(held, "; ") + fmt.Sprintf("; this task can wait, and is expected to be admitted about %s", until.Format("2006-01-02 15:04 UTC"))
}

// roomData is a usage.room event's data.
type roomData struct {
	Window    string    `json:"window"`
	Model     string    `json:"model,omitempty"`
	ReadAt    time.Time `json:"read_at"`
	Used      float64   `json:"utilization"`
	Line      float64   `json:"line"`
	Target    float64   `json:"target"`
	RatePerH  float64   `json:"rate_per_hour"`
	ResetsAt  time.Time `json:"resets_at"`
	AtReset   float64   `json:"expected_at_reset"`
	Unused    float64   `json:"unused"`
	RateSince time.Time `json:"rate_since"`
}

// roomEvents is a usage.room for each window expected to leave at least
// min under its target unused, by the latest reading (seq). Its ID is the
// reading's and the window's: recording the same twice records it once.
func roomEvents(rs []dispatch.Record, now time.Time, target, min float64) []dispatch.Event {
	if len(rs) == 0 {
		return nil
	}
	seq := rs[len(rs)-1].Seq
	var out []dispatch.Event
	for _, p := range paces(rs, now, target) {
		if p.Reset0 || p.Unused() < min {
			continue
		}
		b, _ := json.Marshal(roomData{Window: p.Window, Model: p.Model, ReadAt: p.ReadAt, Used: p.Used, Line: p.Line,
			Target: p.Target, RatePerH: p.Rate, ResetsAt: p.Reset, AtReset: p.AtReset(), Unused: p.Unused(), RateSince: p.RateSince})
		subject := p.Window
		if p.Model != "" {
			subject += "/" + p.Model
		}
		out = append(out, dispatch.Event{ID: fmt.Sprintf("%s/%d/%s", TypeUsageRoom, seq, subject), Type: TypeUsageRoom,
			Source: "dispatcher", Subject: subject, Time: now, Data: b})
	}
	return out
}

// watchRoom records usage.room after each new reading, looking every
// minute.
func watchRoom(ctx context.Context, readings func(context.Context) ([]dispatch.Record, error), emit func(context.Context, dispatch.Event) error,
	target, min float64, log *slog.Logger) {
	var seen int64
	for {
		if rs, err := readings(ctx); err != nil {
			log.Warn("reading usage for room", "error", err)
		} else if len(rs) > 0 && rs[len(rs)-1].Seq != seen {
			seen = rs[len(rs)-1].Seq
			for _, e := range roomEvents(rs, time.Now(), target, min) {
				if err := emit(ctx, e); err != nil {
					log.Warn("recording room", "error", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

type paceConfig struct {
	target, roomMin float64
	soon            time.Duration
	room            bool
}

// paceSettings reads DISPATCHER_PACE_*.
func paceSettings() (paceConfig, error) {
	c := paceConfig{target: 0.95, roomMin: 0.1, soon: time.Hour, room: os.Getenv("DISPATCHER_PACE_ROOM") == "on"}
	for _, f := range []struct {
		env string
		v   *float64
	}{{"DISPATCHER_PACE_TARGET", &c.target}, {"DISPATCHER_PACE_ROOM_MIN", &c.roomMin}} {
		if s := os.Getenv(f.env); s != "" {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v <= 0 || v > 1 {
				return c, fmt.Errorf("%s is a utilization between 0 and 1", f.env)
			}
			*f.v = v
		}
	}
	if s := os.Getenv("DISPATCHER_PACE_SOON"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return c, fmt.Errorf("DISPATCHER_PACE_SOON: %q is not a duration", s)
		}
		c.soon = d
	}
	return c, nil
}
