package pgxdispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch"
)

func versions(t *testing.T, d db) []int {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT version FROM `+d.schema()+`.schema_versions ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	var vs []int
	for rows.Next() {
		var v int
		rows.Scan(&v)
		vs = append(vs, v)
	}
	return vs
}

func wantAllVersions(t *testing.T, d db) {
	t.Helper()
	vs := versions(t, d)
	if len(vs) != latest || vs[len(vs)-1] != latest {
		t.Fatalf("schema versions %v, want 1 to %d once each", vs, latest)
	}
}

// TestR6MigrationsOnce: instances starting together apply each migration once.
func TestR6MigrationsOnce(t *testing.T) {
	d := factory(t).(db)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst, err := Open(context.Background(), d.url, dispatch.Config{Starter: nopStarter{}, MaxRunning: 1}, "b1")
			if err != nil {
				t.Error(err)
				return
			}
			inst.Kill()
		}()
	}
	wg.Wait()
	wantAllVersions(t, d)
}

// TestR6SchemaNewer: an instance that finds a schema newer than it knows
// does not start, and one already running stops taking work.
func TestR6SchemaNewer(t *testing.T) {
	if latest < 2 {
		t.Skip("needs two schema versions")
	}
	d := factory(t).(db)
	ctx := context.Background()
	cfg := dispatch.Config{Starter: nopStarter{}, MaxRunning: 1}
	old, err := OpenWith(ctx, d.url, cfg, "old", Options{schemaVersion: latest - 1, beat: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Kill()
	cur, err := Open(ctx, d.url, cfg, "new")
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(old.Err(), ErrSchemaNewer) {
		if time.Now().After(deadline) {
			t.Fatalf("the old instance did not stop: %v", old.Err())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log(old.Err())
	_, err = OpenWith(ctx, d.url, cfg, "old", Options{schemaVersion: latest - 1})
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("opening an old build on a newer schema: %v, want ErrSchemaNewer", err)
	}
	t.Log(err)
}

// TestR6SchemaFromBeforeMigrations: a database made by the code before
// migrations existed (the first migration's tables, no versions table, and
// work in it) is upgraded and its work carried on.
func TestR6SchemaFromBeforeMigrations(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	s := newFake()
	s.hold("held")
	a, err := OpenWith(ctx, d.url, s.config(4), "old", Options{schemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	submit(t, a, "held", 0)
	submit(t, a, "run", 0)
	waitEvent(t, a, dispatch.TransitionID(dispatch.TypeStarted, "run", "run#1"))
	a.Kill()
	if _, err := admin.Exec(ctx, `DROP TABLE `+d.schema()+`.schema_versions`); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, d.url, s.config(4), "new")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Kill()
	wantAllVersions(t, d)
	finish(t, b, "run#1", dispatch.OutcomeDone, nil)
	s.release("held")
	waitEvent(t, b, dispatch.TransitionID(dispatch.TypeStarted, "held", "held#1"))
	finish(t, b, "held#1", dispatch.OutcomeDone, nil)
	waitEvent(t, b, dispatch.TransitionID(dispatch.TypeDone, "held", "held#1"))
	waitEvent(t, b, dispatch.TransitionID(dispatch.TypeDone, "run", "run#1"))
	noTransitionTwice(t, d)
}

// TestR6UpgradeWithWorkInFlight: work in every state at the previous schema
// version goes on as it would have after an instance at the newest one
// takes over, and no transition is recorded twice.
func TestR6UpgradeWithWorkInFlight(t *testing.T) {
	if latest < 2 {
		t.Skip("needs two schema versions")
	}
	d := factory(t).(db)
	ctx := context.Background()
	tr := dispatch.TransitionID
	s := newFake()
	a, err := OpenWith(ctx, d.url, s.config(2), "old", Options{schemaVersion: latest - 1})
	if err != nil {
		t.Fatal(err)
	}
	s.hold("held")
	submit(t, a, "held", 9)
	eventually(t, "held to be refused", func() bool { return reason(t, a, "held") == dispatch.ReasonAdmission })
	submit(t, a, "waiter", 8)
	waitEvent(t, a, tr(dispatch.TypeStarted, "waiter", "waiter#1"))
	finish(t, a, "waiter#1", dispatch.OutcomeWaiting,
		&dispatch.Wait{Type: "x", Subject: "y", Deadline: time.Now().Add(time.Hour)})
	waitEvent(t, a, tr(dispatch.TypeWaiting, "waiter", "waiter#1"))
	submit(t, a, "run1", 7)
	submit(t, a, "run2", 7)
	waitEvent(t, a, tr(dispatch.TypeStarted, "run1", "run1#1"))
	waitEvent(t, a, tr(dispatch.TypeStarted, "run2", "run2#1"))
	submit(t, a, "pending", 6)
	eventually(t, "pending to wait for a place", func() bool { return reason(t, a, "pending") == dispatch.ReasonLimit })
	sched := dispatch.Schedule{Name: "s", Interval: time.Second,
		Template: dispatch.TaskSpec{Class: "c", Priority: -1, AttemptTimeout: time.Hour}}
	if err := a.Declare(ctx, sched); err != nil {
		t.Fatal(err)
	}
	a.Kill()
	killed := time.Now()

	b, err := Open(ctx, d.url, s.config(2), "new")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Kill()
	wantAllVersions(t, d)
	finish(t, b, "run1#1", dispatch.OutcomeDone, nil)
	waitEvent(t, b, tr(dispatch.TypeStarted, "pending", "pending#1"))
	emit(t, b, dispatch.Event{ID: "wake", Type: "x", Subject: "y", Source: "test"})
	finish(t, b, "run2#1", dispatch.OutcomeDone, nil)
	waitEvent(t, b, tr(dispatch.TypeStarted, "waiter", "waiter#2"))
	for _, at := range []string{"pending#1", "waiter#2"} {
		finish(t, b, at, dispatch.OutcomeDone, nil)
	}
	s.release("held")
	waitEvent(t, b, tr(dispatch.TypeStarted, "held", "held#1"))
	finish(t, b, "held#1", dispatch.OutcomeDone, nil)
	for _, id := range []string{"run1", "run2", "pending", "waiter", "held"} {
		eventually(t, id+" to be done", func() bool {
			st, err := b.Explain(ctx, id)
			return err == nil && st.State == dispatch.Done
		})
	}
	// The schedule goes on firing under the new build.
	tick := time.Unix(killed.Unix()+2, 0)
	eventually(t, "the schedule to fire after the upgrade", func() bool {
		var n int
		b.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id LIKE 's@%' AND id >= $1`,
			dispatch.ScheduledTaskID("s", tick)).Scan(&n)
		return n > 0
	})
	if n := s.startsOf("waiter#1") + s.startsOf("run1#1") + s.startsOf("run2#1"); n != 3 {
		t.Errorf("attempts begun before the upgrade were started %d times in all, want 3", n)
	}
	noTransitionTwice(t, d)
}
