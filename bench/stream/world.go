// Command stream is the task-stream benchmark's world and its scoring (ADR
// 0069).
//
//	stream world                 the world service: the systems an agent's commands reach
//	stream admin task ID DAY     (in the world's pod) the task that begins now
//	stream admin state           (in the world's pod) the world's state, as JSON
//	stream score TASKS STATE     pass or fail for each task, and the wrong calls before it
//
// The world has no model. Its rules are in this program and nowhere in an
// agent's pod: an agent learns them by calling it, through the `ops`
// command, and is answered as a real system answers. What a task is scored
// on is the world's state and its record of the calls, never the agent's
// own account.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Call is one command an agent ran against the world.
type Call struct {
	Task string   `json:"task"`
	Argv []string `json:"argv"`
	Code int      `json:"code"`
	Out  string   `json:"out,omitempty"`
	// Rule is the rule of the world that refused the call, if one did: the
	// measure of whether a lesson was known is how often its rule was run into.
	Rule string `json:"rule,omitempty"`
	// At is when the world answered it.
	At time.Time `json:"at"`
}

// State is what the world is and what was done to it.
type State struct {
	Day   int               `json:"day"`
	Task  string            `json:"task"`
	Facts map[string]string `json:"facts"`
	// Attached is what tasks handed in: TASK/NAME to its text.
	Attached map[string]string `json:"attached"`
	Calls    []Call            `json:"calls"`
	Seq      int               `json:"seq"`
	// After is the world's facts as each task left them: what the task is
	// scored on, since later tasks change them again.
	After map[string]map[string]string `json:"after,omitempty"`
}

// Begin ends the task that was going on, keeping the facts as it left
// them, and begins another.
func (w *World) Begin(task string, day int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keep()
	w.state.Task, w.state.Day = task, day
}

func (w *World) keep() {
	if w.state.Task == "" {
		return
	}
	if w.state.After == nil {
		w.state.After = map[string]map[string]string{}
	}
	facts := make(map[string]string, len(w.state.Facts))
	for k, v := range w.state.Facts {
		facts[k] = v
	}
	w.state.After[w.state.Task] = facts
}

// Snapshot is the state, with the task that is going on kept as it stands.
func (w *World) Snapshot() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keep()
	return w.state
}

// World is the state and the rules that change it.
type World struct {
	mu    sync.Mutex
	state State
	rules Rules
}

// Rules are a world's behaviour: what a command does, what it prints, how
// it ends, and which rule refused it, if one did.
type Rules func(w *World, argv []string) (code int, out string, rule string)

func newWorld(rules Rules) *World {
	return &World{state: State{Facts: map[string]string{}, Attached: map[string]string{}}, rules: rules}
}

// Do runs one command of an agent's and records it.
func (w *World) Do(argv []string) (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	code, out, rule := w.rules(w, argv)
	kept := out
	if len(kept) > 300 {
		kept = kept[:300]
	}
	w.state.Calls = append(w.state.Calls, Call{Task: w.state.Task, Argv: argv, Code: code, Out: kept, Rule: rule, At: time.Now().UTC()})
	return code, out
}

func (w *World) fact(key string) string { return w.state.Facts[key] }
func (w *World) set(key, value string)  { w.state.Facts[key] = value }
func (w *World) next(prefix string) string {
	w.state.Seq++
	return fmt.Sprintf("%s-%d", prefix, w.state.Seq)
}

// flags splits a command's words into those that stand alone and --name
// value pairs; a flag with no value has the value "".
func flags(args []string) (pos []string, named map[string]string) {
	named = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name, ok := strings.CutPrefix(a, "--"); ok {
			if n, v, ok := strings.Cut(name, "="); ok {
				named[n] = v
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				named[name] = args[i+1]
				i++
			} else {
				named[name] = ""
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, named
}

func serve(w *World, admin, listen string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ops", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Argv []string `json:"argv"`
		}
		if json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&in) != nil {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		code, out := w.Do(in.Argv)
		json.NewEncoder(rw).Encode(map[string]any{"code": code, "out": out})
	})
	guarded := func(h http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if admin == "" || r.Header.Get("X-Admin") != admin {
				http.Error(rw, "not found", http.StatusNotFound)
				return
			}
			h(rw, r)
		}
	}
	mux.HandleFunc("POST /admin/task", guarded(func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Task string `json:"task"`
			Day  int    `json:"day"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		w.Begin(in.Task, in.Day)
	}))
	mux.HandleFunc("GET /admin/state", guarded(func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(w.Snapshot())
	}))
	return http.ListenAndServe(listen, mux)
}

// Task is one ticket of a stream, and what it is scored on.
type Task struct {
	ID     string `json:"id"`
	Day    int    `json:"day"`
	Family string `json:"family,omitempty"`
	// Role in its family: meet (the lesson is first run into), use (it is
	// needed again), told (the ticket states it), neutral (no lesson bears
	// on it), wrong (a memory that should not be followed).
	Role   string `json:"role"`
	Ticket string `json:"ticket"`
	// Lesson is a pattern that an entry of memory holding the family's
	// lesson matches, for telling whether a session was given it.
	Lesson string `json:"lesson,omitempty"`
	// Needs are the rules of the world the task cannot be done without
	// meeting; Lessons, for each, a pattern that an entry of memory holding
	// it matches.
	Needs   []string          `json:"needs,omitempty"`
	Lessons map[string]string `json:"lessons,omitempty"`
	Checks  []Check           `json:"checks"`
	// Stale are checks that pass when an old rule was followed after it changed.
	Stale []Check `json:"stale,omitempty"`
}

// Check is one thing that must hold when a task has ended: a fact of the
// world equal to a value, or what the task attached under a name matching
// (or, with Not, not matching) a pattern.
type Check struct {
	Fact     string `json:"fact,omitempty"`
	Equals   string `json:"equals,omitempty"`
	Attached string `json:"attached,omitempty"`
	Matches  string `json:"matches,omitempty"`
	Not      bool   `json:"not,omitempty"`
}

func (c Check) holds(task string, s State) (bool, error) {
	if c.Fact != "" {
		facts := s.Facts
		if after, ok := s.After[task]; ok {
			facts = after
		}
		return facts[c.Fact] == c.Equals != c.Not, nil
	}
	re, err := regexp.Compile(c.Matches)
	if err != nil {
		return false, fmt.Errorf("task %s: %w", task, err)
	}
	return re.MatchString(s.Attached[task+"/"+c.Attached]) != c.Not, nil
}

// Result is how a task went, by the world's account.
type Result struct {
	Task       string `json:"task"`
	Day        int    `json:"day"`
	Family     string `json:"family,omitempty"`
	Role       string `json:"role"`
	Pass       bool   `json:"pass"`
	Calls      int    `json:"calls"`
	WrongCalls int    `json:"wrong_calls"`
	Stale      bool   `json:"stale,omitempty"`
	// Refused is how often each rule refused a call of the task; Needs,
	// the rules the task turns on; Met, those of them no earlier task of
	// the stream turned on (the lesson's first meeting).
	Refused map[string]int `json:"refused,omitempty"`
	Needs   []string       `json:"needs,omitempty"`
	Met     []string       `json:"met,omitempty"`
	// FirstRefusal is, for each rule, the place among the task's calls of
	// the first one it refused.
	FirstRefusal map[string]int `json:"first_refusal,omitempty"`
	// Failed names the checks that did not hold: the fact, or what was attached.
	Failed []string `json:"failed,omitempty"`
}

// Score is each task's result, in the stream's order. A call that asks for
// help is not a wrong call.
func Score(tasks []Task, s State) ([]Result, error) {
	var out []Result
	seen := map[string]bool{}
	for _, t := range tasks {
		r := Result{Task: t.ID, Day: t.Day, Family: t.Family, Role: t.Role, Pass: len(t.Checks) > 0, Needs: t.Needs}
		for _, rule := range t.Needs {
			if !seen[rule] {
				r.Met = append(r.Met, rule)
				seen[rule] = true
			}
		}
		for _, c := range t.Checks {
			ok, err := c.holds(t.ID, s)
			if err != nil {
				return nil, err
			}
			r.Pass = r.Pass && ok
			if !ok {
				r.Failed = append(r.Failed, or(c.Fact, c.Attached))
			}
		}
		for _, c := range t.Stale {
			if ok, err := c.holds(t.ID, s); err != nil {
				return nil, err
			} else if ok {
				r.Stale = true
			}
		}
		for _, c := range s.Calls {
			if c.Task != t.ID {
				continue
			}
			r.Calls++
			if c.Code != 0 && !(len(c.Argv) > 0 && c.Argv[0] == "help") {
				r.WrongCalls++
			}
			if c.Rule != "" {
				if r.Refused == nil {
					r.Refused, r.FirstRefusal = map[string]int{}, map[string]int{}
				}
				if r.Refused[c.Rule] == 0 {
					r.FirstRefusal[c.Rule] = r.Calls
				}
				r.Refused[c.Rule]++
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
