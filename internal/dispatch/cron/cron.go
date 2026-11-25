// Package cron reads cron expressions and says when they tick. It is the
// one place the dispatcher's schedules and its expectations of a CronJob
// compute ticks, so that both agree.
//
// An expression has five fields (minute, hour, day of month, month, day of
// week) or six, with seconds first. Kubernetes takes only the five; the six
// exist so that a test can watch a schedule fire in seconds through the
// same code. A field is *, ?, a number, a range a-b, a list, and any of
// these with /step. Months and days of the week may be named (JAN, MON);
// Sunday is 0 or 7. When both day fields are restricted, a day matches
// either, as in cron. @yearly, @annually, @monthly, @weekly, @daily,
// @midnight and @hourly are accepted.
//
// Ticks are the instants at which the wall clock of the expression's time
// zone reads a matching time. On the day the clock is put forward, a time
// it skips does not tick; on the day it is put back, a time it shows twice
// ticks twice.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the zones, whatever the image has
)

// Expr is a parsed expression in a time zone.
type Expr struct {
	second, minute, hour, dom, month, dow bits
	domAny, dowAny                        bool
	seconds                               bool
	loc                                   *time.Location
}

type bits uint64

func (b bits) has(n int) bool { return b&(1<<uint(n)) != 0 }

var descriptors = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var (
	monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
	dayNames   = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
)

// Parse reads expr in the zone named (an IANA name; UTC when empty).
func Parse(expr, zone string) (*Expr, error) {
	loc := time.UTC
	if zone != "" {
		l, err := time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("time zone %q: %w", zone, err)
		}
		loc = l
	}
	s := strings.TrimSpace(expr)
	if d, ok := descriptors[strings.ToLower(s)]; ok {
		s = d
	}
	f := strings.Fields(s)
	e := &Expr{loc: loc}
	switch len(f) {
	case 5:
		f = append([]string{"0"}, f...)
	case 6:
		e.seconds = true
	default:
		return nil, fmt.Errorf("cron expression %q: five fields (minute hour day month weekday), or six with seconds first", expr)
	}
	var err error
	for i, spec := range []struct {
		dst      *bits
		lo, hi   int
		names    map[string]int
		what     string
		starFlag *bool
	}{
		{&e.second, 0, 59, nil, "second", nil},
		{&e.minute, 0, 59, nil, "minute", nil},
		{&e.hour, 0, 23, nil, "hour", nil},
		{&e.dom, 1, 31, nil, "day of month", &e.domAny},
		{&e.month, 1, 12, monthNames, "month", nil},
		{&e.dow, 0, 7, dayNames, "day of week", &e.dowAny},
	} {
		if *spec.dst, err = field(f[i], spec.lo, spec.hi, spec.names); err != nil {
			return nil, fmt.Errorf("cron expression %q, %s: %w", expr, spec.what, err)
		}
		if spec.starFlag != nil {
			*spec.starFlag = f[i][0] == '*' || f[i][0] == '?'
		}
	}
	if e.dow.has(7) {
		e.dow |= 1
	}
	return e, nil
}

func field(s string, lo, hi int, names map[string]int) (bits, error) {
	var b bits
	for _, item := range strings.Split(s, ",") {
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("step %q is not a positive whole number", stepText)
			}
			step = n
		}
		var from, to int
		switch {
		case rng == "*" || rng == "?":
			from, to = lo, hi
		default:
			a, z, isRange := strings.Cut(rng, "-")
			var err error
			if from, err = value(a, names); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = value(z, names); err != nil {
					return 0, err
				}
			} else if hasStep {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q is outside %d-%d", item, lo, hi)
		}
		for n := from; n <= to; n += step {
			b |= 1 << uint(n)
		}
	}
	return b, nil
}

func value(s string, names map[string]int) (int, error) {
	if n, ok := names[strings.ToUpper(s)]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}

// Location is the expression's time zone.
func (e *Expr) Location() *time.Location { return e.loc }

// Step is the finest an expression can tick: a minute, or a second with six fields.
func (e *Expr) Step() time.Duration {
	if e.seconds {
		return time.Second
	}
	return time.Minute
}

func (e *Expr) dayMatches(l time.Time) bool {
	dom, dow := e.dom.has(l.Day()), e.dow.has(int(l.Weekday()))
	switch {
	case e.domAny && e.dowAny:
		return true
	case e.domAny:
		return dow
	case e.dowAny:
		return dom
	}
	return dom || dow
}

// span bounds a search: an expression that matches nothing in it (30 February)
// never ticks.
const span = 5 * 366 * 24 * time.Hour

// ErrNever is what Next and Prev say through a zero time: no tick in five years.
var ErrNever = errors.New("cron: no tick within five years")

// Next is the first tick strictly after t, or the zero time if there is
// none within five years.
func (e *Expr) Next(t time.Time) time.Time {
	step := e.Step()
	c := t.Truncate(step).Add(step)
	limit := t.Add(span)
	ahead := func(to time.Time) time.Time { // never stand still or go back
		if to.After(c) {
			return to
		}
		return c.Add(step)
	}
	for c.Before(limit) {
		l := c.In(e.loc)
		switch {
		case !e.month.has(int(l.Month())):
			c = ahead(time.Date(l.Year(), l.Month()+1, 1, 0, 0, 0, 0, e.loc))
		case !e.dayMatches(l):
			c = ahead(time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, e.loc))
		case !e.hour.has(l.Hour()):
			c = c.Add(time.Duration(60-l.Minute())*time.Minute - time.Duration(l.Second())*time.Second)
		case !e.minute.has(l.Minute()):
			c = c.Add(time.Minute - time.Duration(l.Second())*time.Second)
		case !e.second.has(l.Second()):
			c = c.Add(time.Second)
		default:
			return c
		}
	}
	return time.Time{}
}

// Prev is the last tick at or before t, or the zero time if there is none
// within five years.
func (e *Expr) Prev(t time.Time) time.Time {
	step := e.Step()
	c := t.Truncate(step)
	limit := t.Add(-span)
	back := func(to time.Time) time.Time {
		if to.Before(c) {
			return to
		}
		return c.Add(-step)
	}
	for c.After(limit) {
		l := c.In(e.loc)
		switch {
		case !e.month.has(int(l.Month())):
			c = back(time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, e.loc).Add(-step))
		case !e.dayMatches(l):
			c = back(time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, e.loc).Add(-step))
		case !e.hour.has(l.Hour()):
			c = c.Add(-time.Duration(l.Minute())*time.Minute - time.Duration(l.Second())*time.Second - step)
		case !e.minute.has(l.Minute()):
			c = c.Add(-time.Duration(l.Second())*time.Second - step)
		case !e.second.has(l.Second()):
			c = c.Add(-time.Second)
		default:
			return c
		}
	}
	return time.Time{}
}

// Count is how many ticks fall strictly between a and b, counting at most
// max of them.
func (e *Expr) Count(a, b time.Time, max int) int {
	n := 0
	for t := e.Next(a); !t.IsZero() && t.Before(b) && n < max; t = e.Next(t) {
		n++
	}
	return n
}
