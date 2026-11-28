// Package budget keeps what the model's usage is for (ADR
// new-the-plan-of-the-models-budget): the owner's priorities, each a name,
// a share of the usage and a list of what he wants done under it, in
// order. They are events in the dispatcher's log; the plan divides the
// room it finds by the shares.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

const (
	// TypePriority: a priority is set, changed or removed. Its subject is
	// the priority's name.
	TypePriority = "budget.priority"
	// TypeItem: something to be done under a priority is added or
	// changed. Its subject is the item's id.
	TypeItem = "budget.item"
)

// The states of an item.
const (
	Waiting = "waiting"
	Started = "started"
	Done    = "done"
	Dropped = "dropped"
)

var (
	ErrForbidden = errors.New("not allowed")
	ErrInvalid   = dispatch.ErrInvalid
	ErrNotFound  = dispatch.ErrNotFound
)

// Priority is one stream of work and its share of the usage. Shares are
// in any unit; a priority's part is its share of their sum.
type Priority struct {
	Name    string  `json:"name"`
	Share   float64 `json:"share"`
	Default bool    `json:"default,omitempty"` // work that names no priority counts here
	Note    string  `json:"note,omitempty"`    // the owner's words on it
	Removed bool    `json:"removed,omitempty"`
	Who     string  `json:"who,omitempty"`
}

// Change adds an item (no ID) or changes one.
type Change struct {
	ID       string `json:"id,omitempty"`
	Priority string `json:"priority,omitempty"`
	Title    string `json:"title,omitempty"`
	// Hard: it is for the strongest model. Otherwise the cheapest tier
	// with a record of such work is tried first.
	Hard  *bool  `json:"hard,omitempty"`
	Model string `json:"model,omitempty"` // the model it must run on, when the owner says
	State string `json:"state,omitempty"`
	First bool   `json:"first,omitempty"` // before the others of its priority
	Note  string `json:"note,omitempty"`
	Who   string `json:"who,omitempty"`
}

// Item is something to be done under a priority.
type Item struct {
	ID       string    `json:"id"`
	Priority string    `json:"priority"`
	Title    string    `json:"title"`
	Hard     bool      `json:"hard,omitempty"`
	Model    string    `json:"model,omitempty"`
	State    string    `json:"state"`
	Note     string    `json:"note,omitempty"`
	By       string    `json:"by,omitempty"` // who last changed its state
	Added    time.Time `json:"added"`
	Changed  time.Time `json:"changed"`
	order    int64
}

// Stream is a priority as it stands: its part of the usage, and its
// items that are not done or dropped, in the order they are to be taken.
type Stream struct {
	Priority
	Part  float64 `json:"part"` // of the usage, 0 to 1
	Items []Item  `json:"items"`
	Done  int     `json:"done"`
}

// View is every priority, the largest share first.
type View struct {
	Streams []Stream `json:"priorities"`
}

// Default is the priority that work naming none counts against: the one
// marked, else the first.
func (v View) Default() string {
	for _, s := range v.Streams {
		if s.Default {
			return s.Name
		}
	}
	if len(v.Streams) > 0 {
		return v.Streams[0].Name
	}
	return ""
}

// Store is what this needs from the log.
type Store interface {
	Claim(ctx context.Context, e dispatch.Event) (bool, error)
	EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error)
}

// Service keeps the priorities.
type Service struct {
	Store Store
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)

func who(c message.Caller) string {
	switch {
	case c.Owner:
		return message.Owner
	case c.Host:
		return "place/" + c.Agent
	}
	return "agent/" + c.Agent
}

// owns says whether the caller speaks for the owner: the owner, or a
// session on the owner's own machine, which is where he says in plain
// words what the usage is for.
func owns(c message.Caller) bool { return c.Owner || c.Host }

func known(c message.Caller) bool { return owns(c) || (c.Agent != "" && c.Pod != "") }

type state struct {
	streams map[string]*Priority
	items   map[string]*Item
	count   map[string]int // items ever added, by priority
	seq     int64
}

func (s *Service) fold(ctx context.Context) (*state, error) {
	st := &state{streams: map[string]*Priority{}, items: map[string]*Item{}, count: map[string]int{}}
	var after int64
	for {
		recs, err := s.Store.EventsOfTypes(ctx, after, []string{TypePriority, TypeItem}, 1000)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			after, st.seq = r.Seq, r.Seq
			switch r.Type {
			case TypePriority:
				var p Priority
				if json.Unmarshal(r.Data, &p) != nil || p.Name == "" {
					continue
				}
				if p.Removed {
					delete(st.streams, p.Name)
					continue
				}
				if p.Default {
					for _, o := range st.streams {
						o.Default = false
					}
				}
				st.streams[p.Name] = &p
			case TypeItem:
				var c Change
				if json.Unmarshal(r.Data, &c) != nil || c.ID == "" {
					continue
				}
				it, ok := st.items[c.ID]
				if !ok {
					it = &Item{ID: c.ID, Priority: c.Priority, State: Waiting, Added: r.Time, order: r.Seq}
					st.items[c.ID] = it
					st.count[c.Priority]++
				}
				it.Changed = r.Time
				if c.Title != "" {
					it.Title = c.Title
				}
				if ok && c.Priority != "" {
					it.Priority = c.Priority
				}
				if c.Hard != nil {
					it.Hard = *c.Hard
				}
				if c.Model != "" {
					it.Model = strings.TrimPrefix(c.Model, "-")
				}
				if c.Note != "" {
					it.Note = c.Note
				}
				if c.State != "" {
					it.State, it.By = c.State, c.Who
				}
				if c.First {
					it.order = -r.Seq
				}
			}
		}
		if len(recs) < 1000 {
			return st, nil
		}
	}
}

// View is the priorities as they stand. Any caller the dispatcher knows
// may read them: a session is told what its work counts against.
func (s *Service) View(ctx context.Context, c message.Caller) (View, error) {
	if !known(c) {
		return View{}, fmt.Errorf("%w: the priorities are read by the owner, the owner's machine and agents in their pods", ErrForbidden)
	}
	st, err := s.fold(ctx)
	if err != nil {
		return View{}, err
	}
	return st.view(), nil
}

func (st *state) view() View {
	var v View
	var sum float64
	for _, p := range st.streams {
		sum += p.Share
	}
	for _, p := range st.streams {
		x := Stream{Priority: *p, Items: []Item{}}
		if sum > 0 {
			x.Part = p.Share / sum
		}
		for _, it := range st.items {
			if it.Priority != p.Name {
				continue
			}
			switch it.State {
			case Done:
				x.Done++
			case Dropped:
			default:
				x.Items = append(x.Items, *it)
			}
		}
		// What was put first latest comes first; the rest as added.
		sort.Slice(x.Items, func(i, j int) bool {
			a, b := x.Items[i].order, x.Items[j].order
			if (a < 0) != (b < 0) {
				return a < 0
			}
			return a < b
		})
		v.Streams = append(v.Streams, x)
	}
	sort.Slice(v.Streams, func(i, j int) bool {
		if v.Streams[i].Share != v.Streams[j].Share {
			return v.Streams[i].Share > v.Streams[j].Share
		}
		return v.Streams[i].Name < v.Streams[j].Name
	})
	return v
}

// Set makes or changes a priority, or removes it. Only the owner's: what
// the usage is for is his to say.
func (s *Service) Set(ctx context.Context, c message.Caller, p Priority) (View, error) {
	if !owns(c) {
		return View{}, fmt.Errorf("%w: a priority and its share are set by the owner or a session on the owner's machine; %s is neither", ErrForbidden, who(c))
	}
	if !nameRe.MatchString(p.Name) {
		return View{}, fmt.Errorf("%w: a priority's name is lower-case letters, digits and dashes, 2 to 41 of them", ErrInvalid)
	}
	st, err := s.fold(ctx)
	if err != nil {
		return View{}, err
	}
	old, exists := st.streams[p.Name]
	switch {
	case p.Removed && !exists:
		return View{}, fmt.Errorf("%w: no priority %s", ErrNotFound, p.Name)
	case p.Removed:
		for _, it := range st.items {
			if it.Priority == p.Name && (it.State == Waiting || it.State == Started) {
				return View{}, fmt.Errorf("%w: %s has items that are not done; move or drop them first (%s)", ErrInvalid, p.Name, it.ID)
			}
		}
	case p.Share < 0 || p.Share > 1000:
		return View{}, fmt.Errorf("%w: a share is a number from 0 to 1000; a priority's part is its share of the sum", ErrInvalid)
	case exists:
		if p.Note == "" {
			p.Note = old.Note
		}
		p.Default = p.Default || old.Default
	}
	p.Who = who(c)
	if err := s.record(ctx, TypePriority, p.Name, st.seq, p); err != nil {
		return View{}, err
	}
	return s.View(ctx, c)
}

// Change adds an item or changes one. Adding, moving and dropping are
// the owner's; an agent in its pod may say it started or finished one.
func (s *Service) Change(ctx context.Context, c message.Caller, ch Change) (Item, error) {
	if !known(c) {
		return Item{}, fmt.Errorf("%w: %s is not known to the dispatcher", ErrForbidden, who(c))
	}
	st, err := s.fold(ctx)
	if err != nil {
		return Item{}, err
	}
	ch.Who = who(c)
	switch ch.State {
	case "", Waiting, Started, Done, Dropped:
	default:
		return Item{}, fmt.Errorf("%w: an item is %s, %s, %s or %s", ErrInvalid, Waiting, Started, Done, Dropped)
	}
	if ch.Priority != "" {
		if _, ok := st.streams[ch.Priority]; !ok {
			return Item{}, fmt.Errorf("%w: no priority %s", ErrNotFound, ch.Priority)
		}
	}
	if ch.ID == "" {
		if !owns(c) {
			return Item{}, fmt.Errorf("%w: what is to be done under a priority is said by the owner or a session on the owner's machine; %s is neither", ErrForbidden, ch.Who)
		}
		if ch.Priority == "" || strings.TrimSpace(ch.Title) == "" {
			return Item{}, fmt.Errorf("%w: a new item needs a priority and a title", ErrInvalid)
		}
		if len(ch.Title) > 2000 {
			return Item{}, fmt.Errorf("%w: a title is at most 2000 bytes; put more where the work is described and name that", ErrInvalid)
		}
		ch.ID = fmt.Sprintf("%s-%d", ch.Priority, st.count[ch.Priority]+1)
		ch.State = ""
	} else {
		it, ok := st.items[ch.ID]
		if !ok {
			return Item{}, fmt.Errorf("%w: no item %s", ErrNotFound, ch.ID)
		}
		onlyState := ch.Priority == "" && ch.Title == "" && ch.Hard == nil && ch.Model == "" && !ch.First && (ch.State == Started || ch.State == Done)
		if !owns(c) && !onlyState {
			return Item{}, fmt.Errorf("%w: an agent may say it started or finished an item; the rest is the owner's. %s is an agent", ErrForbidden, ch.Who)
		}
		if ch.State == it.State {
			ch.State = ""
		}
	}
	if err := s.record(ctx, TypeItem, ch.ID, st.seq, ch); err != nil {
		return Item{}, err
	}
	st, err = s.fold(ctx)
	if err != nil {
		return Item{}, err
	}
	return *st.items[ch.ID], nil
}

// record appends one event after the log's seq as it was read: two
// changes made of the same reading are one, and the second is told.
func (s *Service) record(ctx context.Context, typ, subject string, seq int64, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	ok, err := s.Store.Claim(ctx, dispatch.Event{ID: fmt.Sprintf("%s/%s/%d", typ, subject, seq), Type: typ, Source: "dispatcher", Subject: subject, Time: s.now(), Data: b})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s was changed by another at the same moment; look and say again", ErrInvalid, subject)
	}
	return nil
}
