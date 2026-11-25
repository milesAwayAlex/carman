// Package pgxdispatch is a dispatcher on hand-written Postgres tables,
// through pgx. State is data: a task's place in its life, its deadlines and
// what it waits for are columns, and each instance runs loops that poll
// them. Every transition is one transaction that also appends its events.
// See NOTES.md.
package pgxdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carman/internal/dispatch"
)

const (
	idlePoll = 5 * time.Second        // the fallback for a lost notification
	retry    = time.Second            // admission and failed starts are asked again
	recheck  = 500 * time.Millisecond // another instance's claim, which may have died
	lease    = 15 * time.Second       // how long an instance counts as alive without a heartbeat
	beat     = 5 * time.Second        // how often an instance renews its lease
	batch    = 200                    // events acted on per transaction
)

// liveInstance is true for instance i while its lease runs and, from schema
// 3, its listening session exists: a process that dies drops it at once.
func liveInstance(schema int) string {
	if schema < 3 {
		return `i.alive_until > clock_timestamp()`
	}
	return `i.alive_until > clock_timestamp() AND (i.pid IS NULL OR EXISTS (SELECT 1 FROM pg_stat_activity a
		WHERE a.pid = i.pid AND a.backend_start = i.backend_start))`
}

// live is true when task t's start claim belongs to a live instance.
func live(schema int) string {
	return `(t.starting_by IS NOT NULL AND EXISTS (SELECT 1 FROM instances i
	WHERE i.id = t.starting_by AND ` + liveInstance(schema) + `))`
}

// Dispatcher is one instance. It owns its connection pool.
type Dispatcher struct {
	pool   *pgxpool.Pool
	cfg    dispatch.Config
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	opts   Options
	wg     sync.WaitGroup
	kick   chan struct{} // a hint that there is something to do
	schema int           // the schema version it works on

	mu      sync.Mutex
	stopped error // why it stopped taking work, if it did
}

// Options are the settings beyond the contract's Config. The zero value is
// the default.
type Options struct {
	// Retention is how long the log keeps a finished (done or failed) task,
	// with its events, counted from its end, and an event that belongs to
	// no task (its subject is no task's ID), counted from its appending.
	// Zero keeps everything. What belongs to a task that is not finished
	// is kept however old. A task or event submitted again with an ID that
	// retention removed is new: "one ID, one effect" holds within the period.
	Retention time.Duration
	// RetentionUnowned, when positive, replaces Retention for events that
	// belong to no task and whose type has no period in RetentionByType:
	// the record of what ran is worth keeping longer than task history.
	RetentionUnowned time.Duration
	// RetentionByType replaces Retention for events of a type that belong
	// to no task; zero keeps them for ever.
	RetentionByType map[string]time.Duration
	// CallTimeout limits each call to the Starter and to Admission; 30
	// seconds when zero. A call that passes it counts as failed for this
	// round (the start failed, or admission refused) and is made again
	// later; the loop goes on with other tasks. A call that ignores its
	// context goes on in the background, and its answer is dropped.
	CallTimeout time.Duration
	// schemaVersion, for tests only, stops the migrations at a version.
	schemaVersion int
	// beat, for tests only, replaces how often the lease is renewed.
	beat time.Duration
}

var _ dispatch.Dispatcher = (*Dispatcher)(nil)

// Open connects to the database at url, creates the tables if they are
// missing, and starts an instance. build is recorded and otherwise unused:
// any build carries on any other's work.
func Open(ctx context.Context, url string, cfg dispatch.Config, build string) (*Dispatcher, error) {
	return OpenWith(ctx, url, cfg, build, Options{})
}

// OpenWith is Open with options. It first brings the schema up to the
// newest version this build knows, and fails with ErrSchemaNewer when the
// database's is newer.
func OpenWith(ctx context.Context, url string, cfg dispatch.Config, build string, opts Options) (*Dispatcher, error) {
	if cfg.Starter == nil || cfg.MaxRunning <= 0 {
		return nil, fmt.Errorf("%w: config needs a starter and a positive MaxRunning", dispatch.ErrInvalid)
	}
	pool, err := connect(ctx, url)
	if err != nil {
		return nil, err
	}
	upTo := latest
	if opts.schemaVersion > 0 {
		upTo = opts.schemaVersion
	}
	version, err := migrate(ctx, pool, upTo)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 30 * time.Second
	}
	d := &Dispatcher{pool: pool, cfg: cfg, opts: opts, kick: make(chan struct{}, 1), schema: version}
	err = pool.QueryRow(ctx, `INSERT INTO instances (build, alive_until)
		VALUES ($1, clock_timestamp() + $2 * interval '1 millisecond') RETURNING id`,
		build, lease.Milliseconds()).Scan(&d.id)
	if err != nil {
		pool.Close()
		return nil, err
	}
	listener, err := d.listen(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.wg.Add(3)
	b := beat
	if opts.beat > 0 {
		b = opts.beat
	}
	go d.every(b, d.heartbeat)
	go d.notifications(listener)
	go d.run()
	if opts.prunes() {
		d.wg.Add(1)
		go d.every(opts.pruneEvery(), d.prune)
	}
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
	return write(ctx, pool, 0, func(w *wtx) error { return w.append(e) })
}

func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, url)
}

// Err says why the instance stopped taking work: nil while it works,
// ErrSchemaNewer when a newer build migrated the schema under it. It still
// has to be killed.
func (d *Dispatcher) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

func (d *Dispatcher) stop(err error) {
	d.mu.Lock()
	if d.stopped == nil {
		d.stopped = err
	}
	d.mu.Unlock()
	d.cancel()
}

// Kill stops the instance as a dying process would: in-flight queries are
// cancelled (their transactions roll back), nothing is marked as stopped,
// and its lease simply runs out.
func (d *Dispatcher) Kill() {
	d.cancel()
	d.wg.Wait()
	d.pool.Close()
}

// every runs f every interval until the instance is killed.
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
		}
	}
}

// channel is where a write that appends to the log notifies; one per schema.
const channel = `'pgxdispatch.' || current_schema()`

// listen opens the session that receives notifications and, from schema 3,
// makes it the instance's sign of life.
func (d *Dispatcher) listen(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, d.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	var ch string
	err = conn.QueryRow(ctx, `SELECT `+channel).Scan(&ch)
	if err == nil {
		_, err = conn.Exec(ctx, `LISTEN `+pgx.Identifier{ch}.Sanitize())
	}
	if err == nil && d.schema >= 3 {
		_, err = conn.Exec(ctx, `UPDATE instances SET pid = pg_backend_pid(),
			backend_start = (SELECT backend_start FROM pg_stat_activity WHERE pid = pg_backend_pid())
			WHERE id = $1`, d.id)
	}
	if err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	return conn, nil
}

// notifications wakes the loop on every notification, and reconnects when
// the session is lost; the slow poll covers the gap.
func (d *Dispatcher) notifications(conn *pgx.Conn) {
	defer d.wg.Done()
	for {
		_, err := conn.WaitForNotification(d.ctx)
		if d.ctx.Err() != nil {
			conn.Close(context.Background())
			return
		}
		d.poke()
		for err != nil {
			conn.Close(context.Background())
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(retry):
			}
			if conn, err = d.listen(d.ctx); err == nil {
				d.poke()
			}
		}
	}
}

// run is the instance's loop: a round of work, then sleep until the next
// thing is due, a notification, or the slow poll, whichever is first.
func (d *Dispatcher) run() {
	defer d.wg.Done()
	var due time.Time // when the clock next has something to do
	for {
		t := time.NewTimer(d.round(&due))
		select {
		case <-d.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-d.kick:
			t.Stop()
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
	// One statement: renew the lease, forget instances long gone (a claim
	// whose instance is gone reads as not live), and read the schema version.
	var at int
	err := d.pool.QueryRow(ctx, `WITH gone AS (DELETE FROM instances
			WHERE alive_until < clock_timestamp() - interval '1 hour')
		UPDATE instances SET alive_until = clock_timestamp() + $2 * interval '1 millisecond'
		WHERE id = $1 RETURNING (SELECT max(version) FROM schema_versions)`, d.id, lease.Milliseconds()).Scan(&at)
	if err != nil {
		return err
	}
	if at > d.schema {
		d.stop(fmt.Errorf("%w: it is at version %d, this instance works on %d; it stopped taking work",
			ErrSchemaNewer, at, d.schema))
		return nil
	}
	return nil
}

// wtx is a write transaction. It holds the lock on meta, so it is the only
// writer, and its appends are numbered from meta.seq.
type wtx struct {
	tx     pgx.Tx
	ctx    context.Context
	now    time.Time // the database's clock when the lock was taken
	seq    int64
	cursor int64
	schema int  // the schema version, where the code depends on it
	notify bool // wake the other instances at commit, though the log did not grow
}

// ended is the assignment that records a task's end, at schema 2 and later.
func (w *wtx) ended() string {
	if w.schema >= 2 {
		return ", ended_at = clock_timestamp()"
	}
	return ""
}

func (d *Dispatcher) write(ctx context.Context, fn func(w *wtx) error) error {
	return write(ctx, d.pool, d.schema, fn)
}

// write runs fn in a write transaction.
func write(ctx context.Context, pool *pgxpool.Pool, schema int, fn func(w *wtx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		w := &wtx{tx: tx, ctx: ctx, schema: schema}
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
			if _, err = tx.Exec(ctx, `UPDATE meta SET seq = $1, cursor = $2`, w.seq, w.cursor); err != nil {
				return err
			}
		}
		if w.seq != seq || w.notify {
			_, err = tx.Exec(ctx, `SELECT pg_notify(`+channel+`, '')`)
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
	err := d.write(ctx, func(w *wtx) error { return w.append(e) })
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
	err := d.write(ctx, func(w *wtx) error { return w.submit(t) })
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
	err := d.write(ctx, func(w *wtx) error {
		w.notify = true
		_, err := w.exec(`INSERT INTO schedules (name, spec, interval_s, last_tick) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET spec = excluded.spec, interval_s = excluded.interval_s`,
			s.Name, mustJSON(s), iv, tickAt(w.now, iv))
		return err
	})
	d.poke()
	return err
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
	err := d.pool.QueryRow(ctx, `SELECT spec, payload, state, attempt, wait, reason, reason_detail, `+live(d.schema)+`
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

// errTimedOut is a call out that did not return within CallTimeout.
var errTimedOut = errors.New("did not return in time")

// call runs f with a deadline. When the deadline passes first, it returns
// errTimedOut and leaves f running; f's answer is then dropped.
func (d *Dispatcher) call(ctx context.Context, f func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if d.ctx.Err() != nil {
			return d.ctx.Err()
		}
		return fmt.Errorf("%w (%s)", errTimedOut, d.opts.CallTimeout)
	}
}
