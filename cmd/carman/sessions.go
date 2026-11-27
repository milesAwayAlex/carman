package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"carman/internal/dispatch/message"
)

// session is one session the owner can go to, or is told why not.
type session struct {
	ID      string // the whole id
	Agent   string
	Name    string
	Pod     string
	Mode    string // interactive, or headless
	State   string // busy, idle, or not running
	Running bool
	// ForOwner is how many messages it sent the owner that are not read.
	ForOwner int
	Standing bool // in a standing agent's pod: gone to at its window
	Seen     time.Time
	// Latest says a name reaches this one: the last seen of its name.
	Latest bool
}

// By is what a session is gone to by, and what --print gives: AGENT/NAME
// for the latest of a name, else session/ID.
func (s session) By() string {
	if s.Name != "" && s.Latest {
		return s.Agent + "/" + s.Name
	}
	return "session/" + s.ID
}

// rank orders sessions by how much each needs the owner: one that sent him
// a message he has not read and now waits (idle), then one that sent him
// one and works on, then idle ones, then busy ones, then those not
// running. Within a rank, the one heard from last comes first.
func (s session) rank() int {
	switch {
	case !s.Running:
		return 4
	case s.ForOwner > 0 && s.State == message.Idle:
		return 0
	case s.ForOwner > 0:
		return 1
	case s.State == message.Idle:
		return 2
	}
	return 3
}

// sessions are the peers the owner can go to, in order. Sessions on the
// owner's own machine are not among them: they are in a terminal of his
// already. A session not running is listed when it can be gone to: a
// standing agent's with a name (its window is opened again), or a job's
// kept conversation.
func sessions(peers []message.Peer) []session {
	var out []session
	for _, p := range peers {
		if p.Kind != "session" || strings.Contains(p.Where, "on the owner's machine") {
			continue
		}
		_, id := message.Split(p.Address)
		s := session{ID: id, Agent: p.Agent, Name: p.Name, Pod: p.Pod, Mode: p.Mode, Running: p.Running, ForOwner: p.ForOwner,
			Standing: p.Standing, Seen: p.Seen, Latest: p.Named != "", State: "not running"}
		if p.Running {
			// "idle, not listening: ..." is idle.
			s.State = message.Busy
			if strings.HasPrefix(p.State, message.Idle) {
				s.State = message.Idle
			}
		} else if s.Standing && (s.Name == "" || !s.Latest) {
			// A standing agent's session with no window to open again.
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].rank(), out[j].rank(); a != b {
			return a < b
		}
		return out[i].Seen.After(out[j].Seen)
	})
	return out
}

// line is how a session is shown: what it is gone to by, then what it is doing.
func (s session) line(now time.Time) string {
	what := s.State
	if s.ForOwner > 0 {
		what += fmt.Sprintf(", %d unread for you", s.ForOwner)
	}
	switch {
	case !s.Running && !s.Standing:
		what += ", a job's conversation"
	case s.Running && s.Mode != "interactive":
		what += ", headless"
	}
	where := s.Pod
	if !s.Seen.IsZero() {
		where += ", " + ago(now.Sub(s.Seen))
	}
	return fmt.Sprintf("%-32s %-28s %s  %s", s.By(), what, short(s.ID), where)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// resolve is the one session a word names: AGENT/NAME, session/ID, a name
// alone, or the start of an id (four characters or more). Of two agents'
// sessions of one name, the one that runs is meant when only one does. More than one,
// or none, is an error that says which.
func resolve(all []session, word string) (session, error) {
	var hits []session
	add := func(match func(session) bool) {
		if len(hits) > 0 {
			return
		}
		for _, s := range all {
			if match(s) {
				hits = append(hits, s)
			}
		}
	}
	word = strings.TrimPrefix(word, "session/")
	add(func(s session) bool { return s.ID == word })
	add(func(s session) bool { return s.Name != "" && s.Latest && s.Agent+"/"+s.Name == word })
	add(func(s session) bool { return s.Name != "" && s.Latest && s.Name == word })
	byName := len(hits) > 0
	add(func(s session) bool { return len(word) >= 4 && strings.HasPrefix(s.ID, word) })
	if len(hits) > 1 && byName {
		var running []session
		for _, s := range hits {
			if s.Running {
				running = append(running, s)
			}
		}
		if len(running) == 1 {
			hits = running
		}
	}
	switch len(hits) {
	case 0:
		return session{}, fmt.Errorf("no session is called %q (carman go --list shows them)", word)
	case 1:
		return hits[0], nil
	}
	var names []string
	for _, s := range hits {
		names = append(names, s.By())
	}
	return session{}, fmt.Errorf("%q is more than one session: %s", word, strings.Join(names, ", "))
}
