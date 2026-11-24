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
