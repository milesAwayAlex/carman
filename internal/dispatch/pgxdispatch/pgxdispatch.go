// Package pgxdispatch is a dispatcher on hand-written Postgres tables,
// through pgx. State is data: a task's place in its life, its deadlines and
// what it waits for are columns, and each instance runs loops that poll
// them. Every transition is one transaction that also appends its events.
// See NOTES.md.
package pgxdispatch

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carman/internal/dispatch"
)

//go:embed schema.sql
var schema string

const (
	poll  = 100 * time.Millisecond // how often the loops look at the tables
	lease = 2 * time.Second        // how long a start claim outlives its instance
	beat  = 400 * time.Millisecond // how often an instance renews its lease
	batch = 200                    // events acted on per transaction
)

// live is true when task t's start claim belongs to an instance whose
// lease has not run out.
const live = `(t.starting_by IS NOT NULL AND EXISTS (SELECT 1 FROM instances i
	WHERE i.id = t.starting_by AND i.alive_until > clock_timestamp()))`

// Dispatcher is one instance. It owns its connection pool.
type Dispatcher struct {
	pool   *pgxpool.Pool
	cfg    dispatch.Config
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   chan struct{} // a hint that there is something to do
}

var _ dispatch.Dispatcher = (*Dispatcher)(nil)

// Open connects to the database at url, creates the tables if they are
// missing, and starts an instance. build is recorded and otherwise unused:
// any build carries on any other's work.
func Open(ctx context.Context, url string, cfg dispatch.Config, build string) (*Dispatcher, error) {
	if cfg.Starter == nil || cfg.MaxRunning <= 0 {
		return nil, fmt.Errorf("%w: config needs a starter and a positive MaxRunning", dispatch.ErrInvalid)
	}
	pool, err := connect(ctx, url)
	if err != nil {
		return nil, err
	}
	d := &Dispatcher{pool: pool, cfg: cfg, kick: make(chan struct{}, 1)}
	err = pool.QueryRow(ctx, `INSERT INTO instances (build, alive_until)
		VALUES ($1, clock_timestamp() + $2 * interval '1 millisecond') RETURNING id`,
		build, lease.Milliseconds()).Scan(&d.id)
	if err != nil {
		pool.Close()
		return nil, err
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.wg.Add(3)
	go d.every(beat, d.heartbeat)
	go d.every(poll, d.react)
	go d.every(poll, d.startWhatCan)
	return d, nil
}

// Emit appends e to the log at url without an instance, as a watcher does.
func Emit(ctx context.Context, url string, e dispatch.Event) error {
	if err := validEvent(e); err != nil {
		return err
	}
	pool, err := connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	return write(ctx, pool, func(w *wtx) error { return w.append(e) })
}

func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Two instances creating the tables at once would collide.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pgxdispatch schema'))`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgxdispatch: create tables: %w", err)
	}
	return pool, nil
}

// Kill stops the instance as a dying process would: in-flight queries are
// cancelled (their transactions roll back), nothing is marked as stopped,
// and its lease simply runs out.
func (d *Dispatcher) Kill() {
	d.cancel()
	d.wg.Wait()
	d.pool.Close()
}

// every runs f every interval, or sooner when kicked, until the instance
// is killed.
func (d *Dispatcher) every(interval time.Duration, f func(context.Context) error) {
	defer d.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f(d.ctx) // an error is retried at the next round
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
	}
}

func (d *Dispatcher) poke() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) heartbeat(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `UPDATE instances SET alive_until = clock_timestamp() + $2 * interval '1 millisecond'
		WHERE id = $1`, d.id, lease.Milliseconds())
	if err != nil {
		return err
	}
	// A claim whose instance is gone reads as not live, as an expired one does.
	_, err = d.pool.Exec(ctx, `DELETE FROM instances WHERE alive_until < clock_timestamp() - interval '1 hour'`)
	return err
}

// wtx is a write transaction. It holds the lock on meta, so it is the only
// writer, and its appends are numbered from meta.seq.
type wtx struct {
	tx     pgx.Tx
	ctx    context.Context
	now    time.Time // the database's clock when the lock was taken
	seq    int64
	cursor int64
}

// write runs fn in a write transaction.
func write(ctx context.Context, pool *pgxpool.Pool, fn func(w *wtx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		w := &wtx{tx: tx, ctx: ctx}
		err := tx.QueryRow(ctx, `SELECT seq, cursor, clock_timestamp() FROM meta FOR UPDATE`).
			Scan(&w.seq, &w.cursor, &w.now)
		if err != nil {
			return err
		}
		seq, cursor := w.seq, w.cursor
		if err := fn(w); err != nil {
			return err
		}
		if w.seq != seq || w.cursor != cursor {
			_, err = tx.Exec(ctx, `UPDATE meta SET seq = $1, cursor = $2`, w.seq, w.cursor)
		}
		return err
	})
}

func (w *wtx) exec(sql string, args ...any) (int64, error) {
	tag, err := w.tx.Exec(w.ctx, sql, args...)
	return tag.RowsAffected(), err
}

// append adds e to the log unless its ID is there.
func (w *wtx) append(e dispatch.Event) error {
	if e.Time.IsZero() {
		e.Time = w.now
	}
	n, err := w.exec(`INSERT INTO events (seq, id, type, source, subject, time, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		w.seq+1, e.ID, e.Type, e.Source, e.Subject, e.Time, jsonArg(e.Data))
	if n == 1 {
		w.seq++
	}
	return err
}

func (w *wtx) transition(typ, subject, key string, tr dispatch.Transition) error {
	body, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	return w.append(dispatch.Event{ID: dispatch.TransitionID(typ, subject, key), Type: typ,
		Source: dispatch.Source, Subject: subject, Data: body})
}

// jsonArg passes JSON as text, so that Postgres keeps it as it is, and
// empty as NULL.
func jsonArg(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func validEvent(e dispatch.Event) error {
	if e.ID == "" || e.Type == "" || e.Source == "" {
		return fmt.Errorf("%w: event needs id, type and source", dispatch.ErrInvalid)
	}
	if len(e.Data) > 0 && !json.Valid(e.Data) {
		return fmt.Errorf("%w: event data is not JSON", dispatch.ErrInvalid)
	}
	return nil
}

func (d *Dispatcher) Emit(ctx context.Context, e dispatch.Event) error {
	if err := validEvent(e); err != nil {
		return err
	}
	err := write(ctx, d.pool, func(w *wtx) error { return w.append(e) })
	d.poke()
	return err
}

func (d *Dispatcher) Events(ctx context.Context, after int64, limit int) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE seq > $1 ORDER BY seq LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

func scanRecord(row pgx.CollectableRow) (dispatch.Record, error) {
	var r dispatch.Record
	var data []byte
	err := row.Scan(&r.Seq, &r.ID, &r.Type, &r.Source, &r.Subject, &r.Time, &data)
	if len(data) > 0 {
		r.Data = data
	}
	return r, err
}

func (d *Dispatcher) Submit(ctx context.Context, t dispatch.TaskSpec) error {
	if t.ID == "" || t.Class == "" || t.AttemptTimeout <= 0 {
		return fmt.Errorf("%w: task needs id, class and a positive attempt timeout", dispatch.ErrInvalid)
	}
	err := write(ctx, d.pool, func(w *wtx) error { return w.submit(t) })
	d.poke()
	return err
}

func (w *wtx) submit(t dispatch.TaskSpec) error {
	bare := t
	bare.Payload = nil
	var startBy *time.Time
	if !t.StartBy.IsZero() {
		startBy = &t.StartBy
	}
	n, err := w.exec(`INSERT INTO tasks (id, spec, payload, priority, ord, timeout_us, start_by, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending') ON CONFLICT (id) DO NOTHING`,
		t.ID, mustJSON(bare), jsonArg(t.Payload), t.Priority, w.seq+1, t.AttemptTimeout.Microseconds(), startBy)
	if err != nil || n == 0 {
		return err
	}
	return w.transition(dispatch.TypeSubmitted, t.ID, "", dispatch.Transition{Spec: &t})
}

func (d *Dispatcher) Declare(ctx context.Context, s dispatch.Schedule) error {
	if s.Name == "" || s.Interval < time.Second || s.Interval%time.Second != 0 {
		return fmt.Errorf("%w: schedule needs a name and an interval of whole seconds", dispatch.ErrInvalid)
	}
	if s.Template.Class == "" || s.Template.AttemptTimeout <= 0 {
		return fmt.Errorf("%w: schedule template needs a class and a positive attempt timeout", dispatch.ErrInvalid)
	}
	iv := int64(s.Interval / time.Second)
	return write(ctx, d.pool, func(w *wtx) error {
		_, err := w.exec(`INSERT INTO schedules (name, spec, interval_s, last_tick) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET spec = excluded.spec, interval_s = excluded.interval_s`,
			s.Name, mustJSON(s), iv, tickAt(w.now, iv))
		return err
	})
}

// tickAt is the latest multiple of iv seconds since the Unix epoch at or
// before now.
func tickAt(now time.Time, iv int64) time.Time {
	sec := now.Unix()
	return time.Unix(sec-sec%iv, 0).UTC()
}

func (d *Dispatcher) Explain(ctx context.Context, id string) (dispatch.TaskStatus, error) {
	var st dispatch.TaskStatus
	var spec, payload, wait []byte
	var kind, detail string
	var inFlight bool
	err := d.pool.QueryRow(ctx, `SELECT spec, payload, state, attempt, wait, reason, reason_detail, `+live+`
		FROM tasks t WHERE id = $1`, id).Scan(&spec, &payload, &st.State, &st.Attempt, &wait, &kind, &detail, &inFlight)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, dispatch.ErrNotFound
	}
	if err != nil {
		return st, err
	}
	if st.Spec, err = decodeSpec(spec, payload); err != nil {
		return st, err
	}
	switch {
	case st.State == dispatch.Waiting:
		var w dispatch.Wait
		if err := json.Unmarshal(wait, &w); err != nil {
			return st, err
		}
		st.Reason = dispatch.Reason{Kind: dispatch.ReasonWaiting, Wait: &w}
	case st.State == dispatch.Pending && !inFlight:
		st.Reason = dispatch.Reason{Kind: dispatch.ReasonKind(kind), Detail: detail}
	}
	return st, nil
}

func decodeSpec(spec, payload []byte) (dispatch.TaskSpec, error) {
	var s dispatch.TaskSpec
	err := json.Unmarshal(spec, &s)
	if len(payload) > 0 {
		s.Payload = payload
	}
	return s, err
}
