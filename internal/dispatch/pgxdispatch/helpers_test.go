package pgxdispatch

import (
	"context"
	"encoding/json"

	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// fake is a starter and admission for this package's own tests.
type fake struct {
	mu     sync.Mutex
	held   map[string]bool
	hang   map[string]chan struct{}
	starts map[string]int
}

func newFake() *fake {
	return &fake{held: map[string]bool{}, hang: map[string]chan struct{}{}, starts: map[string]int{}}
}

func (f *fake) Start(ctx context.Context, a dispatch.Attempt) error {
	f.mu.Lock()
	f.starts[a.Name]++
	ch := f.hang[a.Task.ID]
	f.mu.Unlock()
	if ch != nil {
		<-ch // ignores ctx, as a stuck client would
	}
	return nil
}

func (f *fake) admit(_ context.Context, t dispatch.TaskSpec, _ []dispatch.TaskSpec) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[t.ID] {
		return false, "held"
	}
	return true, ""
}

func (f *fake) hold(id string)    { f.mu.Lock(); f.held[id] = true; f.mu.Unlock() }
func (f *fake) release(id string) { f.mu.Lock(); delete(f.held, id); f.mu.Unlock() }

func (f *fake) startsOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[name]
}

func (f *fake) config(max int) dispatch.Config {
	return dispatch.Config{Starter: f, Admission: f.admit, MaxRunning: max}
}

func submit(t *testing.T, d *Dispatcher, id string, priority int) {
	t.Helper()
	err := d.Submit(context.Background(), dispatch.TaskSpec{ID: id, Class: "c", Priority: priority, AttemptTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
}

func emit(t *testing.T, d *Dispatcher, e dispatch.Event) {
	t.Helper()
	if err := d.Emit(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func finish(t *testing.T, d *Dispatcher, attempt string, o dispatch.Outcome, w *dispatch.Wait) {
	t.Helper()
	body, _ := json.Marshal(dispatch.Finished{Attempt: attempt, Outcome: o, Wait: w})
	task := attempt[:strings.LastIndex(attempt, "#")]
	emit(t, d, dispatch.Event{ID: "finished-" + attempt, Type: dispatch.TypeFinished, Source: "test", Subject: task, Data: body})
}

func hasEvent(t *testing.T, d *Dispatcher, id string) bool {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func waitEvent(t *testing.T, d *Dispatcher, id string) {
	t.Helper()
	eventually(t, "event "+id, func() bool { return hasEvent(t, d, id) })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func reason(t *testing.T, d *Dispatcher, id string) dispatch.ReasonKind {
	t.Helper()
	st, err := d.Explain(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return st.Reason.Kind
}

// noTransitionTwice fails when the dispatcher recorded one transition
// twice, under any ID.
func noTransitionTwice(t *testing.T, d db) {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT type, subject, coalesce(data::jsonb->>'attempt', ''), count(*)
		FROM `+d.schema()+`.events WHERE source = 'dispatcher' AND type <> 'schedule.missed'
		GROUP BY 1, 2, 3 HAVING count(*) > 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, subject, attempt string
		var n int
		rows.Scan(&typ, &subject, &attempt, &n)
		t.Errorf("%s for %s %s recorded %d times", typ, subject, attempt, n)
	}
}
