package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

type log struct {
	mu   sync.Mutex
	recs []dispatch.Record
}

func (l *log) Claim(_ context.Context, e dispatch.Event) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.ID == e.ID {
			return false, nil
		}
	}
	l.recs = append(l.recs, dispatch.Record{Seq: int64(len(l.recs) + 1), Event: e})
	return true, nil
}

func (l *log) EventsOfTypes(_ context.Context, after int64, types []string, limit int) ([]dispatch.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []dispatch.Record
	for _, r := range l.recs {
		for _, t := range types {
			if r.Seq > after && r.Type == t && len(out) < limit {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

var (
	owner = message.Caller{Owner: true}
	place = message.Caller{Host: true, Agent: "laptop"}
	agent = message.Caller{Agent: "desk", Pod: "desk-0"}
)

func service() *Service {
	return &Service{Store: &log{}, Now: func() time.Time { return time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC) }}
}

// The owner names priorities and gives each a share; a priority's part
// is its share of the sum, and one is the default.
func TestPrioritiesAndTheirShares(t *testing.T) {
	ctx := context.Background()
	s := service()
	for _, p := range []Priority{{Name: "platform", Share: 50, Default: true}, {Name: "benchmark", Share: 30}, {Name: "errands", Share: 20, Note: "small things"}} {
		if _, err := s.Set(ctx, owner, p); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.View(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Streams) != 3 || v.Streams[0].Name != "platform" || v.Streams[0].Part != 0.5 || v.Streams[2].Part != 0.2 || v.Default() != "platform" {
		t.Fatalf("priorities: %+v", v.Streams)
	}
	// A share is changed from the owner's machine; the note stays, and
	// the parts are of the new sum.
	if v, err = s.Set(ctx, place, Priority{Name: "errands", Share: 70}); err != nil {
		t.Fatal(err)
	}
	if v.Streams[0].Name != "errands" || v.Streams[0].Note != "small things" || v.Streams[0].Part != 70.0/150 || v.Default() != "platform" {
		t.Fatalf("after a change: %+v", v.Streams)
	}
	// The default moves when another is marked.
	if v, err = s.Set(ctx, owner, Priority{Name: "benchmark", Share: 30, Default: true}); err != nil || v.Default() != "benchmark" {
		t.Fatalf("default: %q %v", v.Default(), err)
	}
	// An agent in a pod sets nothing.
	if _, err := s.Set(ctx, agent, Priority{Name: "mine", Share: 90}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent set a priority: %v", err)
	}
	if _, err := s.Set(ctx, owner, Priority{Name: "Bad Name", Share: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad name: %v", err)
	}
	if _, err := s.View(ctx, message.Caller{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an unknown caller read the priorities: %v", err)
	}
}

// Under a priority, what the owner wants done, in order; an agent says
// what it started and finished and nothing else.
func TestItemsInOrder(t *testing.T) {
	ctx := context.Background()
	s := service()
	if _, err := s.Set(ctx, owner, Priority{Name: "platform", Share: 1}); err != nil {
		t.Fatal(err)
	}
	hard := true
	a, err := s.Change(ctx, owner, Change{Priority: "platform", Title: "the planner"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Change(ctx, place, Change{Priority: "platform", Title: "a second harness", Hard: &hard})
	c, _ := s.Change(ctx, owner, Change{Priority: "platform", Title: "first of all", First: true, Model: "cheap"})
	if a.ID != "platform-1" || b.ID != "platform-2" || !b.Hard || c.Model != "cheap" || a.State != Waiting {
		t.Fatalf("items: %+v %+v %+v", a, b, c)
	}
	v, _ := s.View(ctx, agent)
	if got := v.Streams[0].Items; len(got) != 3 || got[0].ID != c.ID || got[1].ID != a.ID || got[2].ID != b.ID {
		t.Fatalf("order: %+v", got)
	}
	if _, err := s.Change(ctx, agent, Change{Priority: "platform", Title: "work of my own"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent added an item: %v", err)
	}
	if _, err := s.Change(ctx, agent, Change{ID: a.ID, State: Dropped}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent dropped an item: %v", err)
	}
	if _, err := s.Change(ctx, agent, Change{ID: a.ID, First: true, State: Started}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent moved an item: %v", err)
	}
	it, err := s.Change(ctx, agent, Change{ID: c.ID, State: Started})
	if err != nil || it.State != Started || it.By != "agent/desk" {
		t.Fatalf("started: %+v %v", it, err)
	}
	if it, err = s.Change(ctx, agent, Change{ID: c.ID, State: Done, Note: "merged"}); err != nil || it.State != Done {
		t.Fatalf("done: %+v %v", it, err)
	}
	v, _ = s.View(ctx, owner)
	if x := v.Streams[0]; len(x.Items) != 2 || x.Done != 1 || x.Items[0].ID != a.ID {
		t.Fatalf("after one is done: %+v", x)
	}
	// A priority with work waiting is not removed from under it.
	if _, err := s.Set(ctx, owner, Priority{Name: "platform", Removed: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("removed with items waiting: %v", err)
	}
	if _, err := s.Change(ctx, owner, Change{Priority: "nothing", Title: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an item under no priority: %v", err)
	}
}
