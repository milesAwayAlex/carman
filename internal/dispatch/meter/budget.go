package meter

import (
	"fmt"
	"sync"
	"time"

	"carman/internal/dispatch/message"
)

// TypeModelLimit is the owner's word on an agent's limit at the proxy,
// over what its definition says: data a Limit, subject the agent. An
// empty Limit lifts it.
const TypeModelLimit = "model.limit"

// Limit is an agent's limit at the proxy, in weighted tokens (Weighted).
// Zero is no limit.
type Limit struct {
	// Day is all the agent's pods together over a day, from 00:00 UTC.
	Day int64 `json:"day,omitempty"`
	// Pod is one pod over its life: a job's run.
	Pod int64 `json:"pod,omitempty"`
}

func (l Limit) None() bool { return l.Day <= 0 && l.Pod <= 0 }

// weights are what a token of each kind counts for, in input tokens: the
// ratios of the API's prices, which hold across its models.
var weights = map[string]float64{
	"input_tokens":                1,
	"output_tokens":               5,
	"cache_creation_input_tokens": 1.25,
	"cache_read_input_tokens":     0.1,
}

// Weighted is an answer's tokens as one count, in input tokens.
func Weighted(tokens map[string]int64) int64 {
	var sum float64
	for k, w := range weights {
		sum += w * float64(tokens[k])
	}
	return int64(sum + 0.5)
}

// Day is the day a time counts in, by its UTC date.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// PodKey is how a pod's use is counted: by its UID, or its name.
func PodKey(pod, uid string) string {
	if uid != "" {
		return uid
	}
	return pod
}

// Standing is the dispatcher's word on agents' limits and what each used
// by the log's model.usage records: what the meter enforces, and what
// dispatch status shows.
type Standing struct {
	Day    string                   `json:"day"`
	Agents map[string]AgentStanding `json:"agents"`
}

type AgentStanding struct {
	Limit Limit `json:"limit"`
	// By says where the limit is from: "definition", or "owner" (a
	// model.limit event).
	By string `json:"by,omitempty"`
	// Used is the day's use, and Pods each pod's, of the records counted.
	Used int64            `json:"used"`
	Pods map[string]int64 `json:"pods,omitempty"`
	// Refused is the day's calls refused for the limit.
	Refused int64 `json:"refused,omitempty"`
}

// Tally adds up records for the agents that have a limit. The day is
// now's; a pod's use is over every record given. Records skip says are
// counted elsewhere (the asking meter's own, since it started) are left
// out.
func Tally(limits map[string]AgentStanding, records []Record, now time.Time, skip func(Record) bool) Standing {
	s := Standing{Day: Day(now), Agents: map[string]AgentStanding{}}
	for a, st := range limits {
		if st.Limit.None() {
			continue
		}
		st.Used, st.Pods, st.Refused = 0, nil, 0
		s.Agents[a] = st
	}
	for _, r := range records {
		st, ok := s.Agents[r.Agent]
		if !ok || (skip != nil && skip(r)) {
			continue
		}
		n := Weighted(r.Tokens)
		if Day(r.To) == s.Day {
			st.Used += n
			st.Refused += r.OverBudget
		}
		if k := PodKey(r.Pod, r.PodUID); k != "" && n > 0 {
			if st.Pods == nil {
				st.Pods = map[string]int64{}
			}
			st.Pods[k] += n
		}
		s.Agents[r.Agent] = st
	}
	return s
}

// Budget holds agents to their limits: the dispatcher's standing, plus
// what this meter counted since it started (which the standing leaves
// out). Until a standing has come, nobody is held.
type Budget struct {
	Now func() time.Time

	mu       sync.Mutex
	standing *Standing
	day      string
	ownDay   map[string]int64
	ownPod   map[string]int64
}

func (b *Budget) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Set takes a standing from the dispatcher.
func (b *Budget) Set(s Standing) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.standing = &s
}

// Count adds what an answer used.
func (b *Budget) Count(who message.Caller, tokens map[string]int64) {
	if b == nil {
		return
	}
	n := Weighted(tokens)
	if n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	b.ownDay[who.Agent] += n
	b.ownPod[PodKey(who.Pod, who.PodUID)] += n
}

func (b *Budget) roll() {
	if d := Day(b.now()); d != b.day || b.ownDay == nil {
		b.day, b.ownDay = d, map[string]int64{}
		if b.ownPod == nil {
			b.ownPod = map[string]int64{}
		}
	}
}

// Over says why a call of who's is refused, if it is.
func (b *Budget) Over(who message.Caller) (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.standing == nil {
		return "", false
	}
	st, ok := b.standing.Agents[who.Agent]
	if !ok || st.Limit.None() {
		return "", false
	}
	b.roll()
	day := b.ownDay[who.Agent]
	if b.standing.Day == b.day {
		day += st.Used
	}
	pk := PodKey(who.Pod, who.PodUID)
	pod := b.ownPod[pk] + st.Pods[pk]
	how := "its definition"
	if st.By == "owner" {
		how = "the owner"
	}
	switch {
	case st.Limit.Day > 0 && day >= st.Limit.Day:
		return fmt.Sprintf(refusalPrefix+"agent %s has used %s of its budget of %s weighted tokens for today (set by %s; the day starts again at 00:00 UTC; the owner can raise or lift it with dispatcher limit %s)",
			who.Agent, Count(day), Count(st.Limit.Day), how, who.Agent), true
	case st.Limit.Pod > 0 && pod >= st.Limit.Pod:
		return fmt.Sprintf(refusalPrefix+"this pod of agent %s has used %s of its budget of %s weighted tokens for one pod (set by %s; the owner can raise or lift it with dispatcher limit %s)",
			who.Agent, Count(pod), Count(st.Limit.Pod), how, who.Agent), true
	}
	return "", false
}

// Count is a number of tokens as a person reads it: 1.2M, 340k.
func Count(n int64) string {
	switch {
	case n >= 10_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
