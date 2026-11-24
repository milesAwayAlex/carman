package pgxdispatch

import (
	"context"
	"os"
	"testing"
	"time"

	"carman/internal/dispatch"
)

// TestIdleCost measures what one idle instance does to the database in 20
// seconds. It runs only when DISPATCH_MEASURE_IDLE is set, alone: the commit
// count is the whole database's.
func TestIdleCost(t *testing.T) {
	if os.Getenv("DISPATCH_MEASURE_IDLE") == "" {
		t.Skip("set DISPATCH_MEASURE_IDLE to measure")
	}
	d := factory(t).(db)
	ctx := context.Background()
	inst, err := Open(ctx, d.url, dispatch.Config{Starter: nopStarter{}, MaxRunning: 1}, "b1")
	if err != nil {
		t.Fatal(err)
	}
	type snap struct{ commits, ins, upd, wal int64 }
	read := func() snap {
		var s snap
		err := admin.QueryRow(ctx, `SELECT (SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()),
			(SELECT coalesce(sum(n_tup_ins), 0)::bigint FROM pg_stat_user_tables WHERE schemaname = $1),
			(SELECT coalesce(sum(n_tup_upd), 0)::bigint FROM pg_stat_user_tables WHERE schemaname = $1),
			pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint`,
			d.schema()).Scan(&s.commits, &s.ins, &s.upd, &s.wal)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// Backends flush their statistics when idle for ten seconds or at exit.
	time.Sleep(12 * time.Second)
	before := read()
	time.Sleep(20 * time.Second)
	inst.Kill()
	time.Sleep(2 * time.Second)
	after := read()
	t.Logf("idle 20 s: %d commits, %d rows inserted, %d rows updated, %d bytes of WAL (the whole server's)",
		after.commits-before.commits, after.ins-before.ins, after.upd-before.upd, after.wal-before.wal)
}

// TestIdleTakesNoLock: an idle instance does not take the write lock (each
// time it does, the meta row's xmax changes), and a finished event from
// another connection is still acted on at once, through a notification.
func TestIdleTakesNoLock(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	inst, err := Open(ctx, d.url, dispatch.Config{Starter: nopStarter{}, MaxRunning: 1}, "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	submit(t, inst, "t", 0)
	waitEvent(t, inst, dispatch.TransitionID(dispatch.TypeStarted, "t", "t#1"))
	time.Sleep(500 * time.Millisecond)
	xmax := func() (x string) {
		if err := inst.pool.QueryRow(ctx, `SELECT xmax::text FROM meta`).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before := xmax()
	time.Sleep(3 * time.Second)
	if after := xmax(); after != before {
		t.Errorf("an idle instance locked meta (xmax %s, then %s)", before, after)
	}
	body := []byte(`{"attempt":"t#1","outcome":"done"}`)
	sent := time.Now()
	err = Emit(ctx, d.url, dispatch.Event{ID: "f", Type: dispatch.TypeFinished, Source: "test", Subject: "t", Data: body})
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, inst, dispatch.TransitionID(dispatch.TypeDone, "t", "t#1"))
	if took := time.Since(sent); took > time.Second {
		t.Errorf("an event from another connection took %s to be acted on", took)
	}
}
