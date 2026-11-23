package pgxdispatch

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"carman/internal/dispatch"
)

// react acts on the log from meta.cursor on, then on the clock. Any
// instance may run it; the lock on meta makes them take turns.
func (d *Dispatcher) react(ctx context.Context) error {
	for {
		more := false
		err := write(ctx, d.pool, func(w *wtx) error {
			rows, err := w.tx.Query(ctx, `SELECT seq, id, type, source, subject, time, data
				FROM events WHERE seq > $1 ORDER BY seq LIMIT $2`, w.cursor, batch)
			if err != nil {
				return err
			}
			rs, err := pgx.CollectRows(rows, scanRecord)
			if err != nil {
				return err
			}
			more = len(rs) == batch
			for _, r := range rs {
				if err := w.actOn(r); err != nil {
					return err
				}
				w.cursor = r.Seq
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return write(ctx, d.pool, func(w *wtx) error { return w.actOnTime() })
}

func (w *wtx) actOn(r dispatch.Record) error {
	if r.Type == dispatch.TypeFinished {
		if err := w.finished(r); err != nil {
			return err
		}
	}
	rows, err := w.tx.Query(w.ctx, `SELECT id, attempt, wait FROM tasks
		WHERE state = 'waiting' AND wait_type = $1 AND wait_subject = $2 AND wait_after < $3`,
		r.Type, r.Subject, r.Seq)
	if err != nil {
		return err
	}
	ws, err := pgx.CollectRows(rows, scanWaiter)
	if err != nil {
		return err
	}
	for _, t := range ws {
		ev := r.Event
		if err := w.wake(t, dispatch.Wake{Wait: t.wait, Event: &ev}); err != nil {
			return err
		}
	}
	return nil
}

type waiter struct {
	id      string
	attempt int
	wait    dispatch.Wait
}

func scanWaiter(row pgx.CollectableRow) (waiter, error) {
	var t waiter
	var wait []byte
	if err := row.Scan(&t.id, &t.attempt, &wait); err != nil {
		return t, err
	}
	return t, json.Unmarshal(wait, &t.wait)
}

// finished acts on a task.finished event for the task's current attempt,
// recording its start first when the task was still pending.
func (w *wtx) finished(r dispatch.Record) error {
	var f dispatch.Finished
	if json.Unmarshal(r.Data, &f) != nil {
		return nil
	}
	var state dispatch.State
	var attempt int
	err := w.tx.QueryRow(w.ctx, `SELECT state, attempt FROM tasks WHERE id = $1`, r.Subject).Scan(&state, &attempt)
	if err == pgx.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	id := r.Subject
	attempt = max(attempt, 1)
	name := dispatch.AttemptName(id, attempt)
	if f.Attempt != name || (state != dispatch.Running && state != dispatch.Pending) {
		return nil
	}
	if state == dispatch.Pending {
		if err := w.transition(dispatch.TypeStarted, id, name, dispatch.Transition{Attempt: name}); err != nil {
			return err
		}
	}
	tr := dispatch.Transition{Attempt: name, Event: r.ID}
	switch {
	case f.Outcome == dispatch.OutcomeDone:
		_, err = w.exec(`UPDATE tasks SET state = 'done', attempt = $2, starting_by = NULL, wake = NULL,
			reason = '', reason_detail = '' WHERE id = $1`, id, attempt)
		if err == nil {
			err = w.transition(dispatch.TypeDone, id, name, tr)
		}
	case f.Outcome == dispatch.OutcomeWaiting && f.Wait != nil:
		tr.Wait = f.Wait
		_, err = w.exec(`UPDATE tasks SET state = 'waiting', attempt = $2, starting_by = NULL, wake = NULL,
			wait = $3, wait_type = $4, wait_subject = $5, wait_deadline = $6, wait_after = $7,
			reason = '', reason_detail = '' WHERE id = $1`,
			id, attempt, mustJSON(f.Wait), f.Wait.Type, f.Wait.Subject, f.Wait.Deadline, r.Seq)
		if err == nil {
			err = w.transition(dispatch.TypeWaiting, id, name, tr)
		}
	default: // failed, or a malformed outcome
		_, err = w.exec(`UPDATE tasks SET state = 'failed', attempt = $2, starting_by = NULL, wake = NULL,
			reason = '', reason_detail = '' WHERE id = $1`, id, attempt)
		if err == nil {
			err = w.transition(dispatch.TypeFailed, id, name, tr)
		}
	}
	return err
}

// wake makes a waiting task pending for its next attempt.
func (w *wtx) wake(t waiter, wk dispatch.Wake) error {
	name := dispatch.AttemptName(t.id, t.attempt)
	_, err := w.exec(`UPDATE tasks SET state = 'pending', attempt = attempt + 1, wake = $2,
		wait = NULL, wait_type = NULL, wait_subject = NULL, wait_deadline = NULL, wait_after = NULL,
		reason = '', reason_detail = '' WHERE id = $1`, t.id, mustJSON(wk))
	if err != nil {
		return err
	}
	return w.transition(dispatch.TypeWoken, t.id, name, dispatch.Transition{Attempt: name, Wake: &wk})
}

// actOnTime handles overruns, wait deadlines, late starts and schedules.
func (w *wtx) actOnTime() error {
	rows, err := w.tx.Query(w.ctx, `SELECT id, attempt FROM tasks
		WHERE state = 'running' AND $1 > started_at + timeout_us * interval '1 microsecond'`, w.now)
	if err != nil {
		return err
	}
	type over struct {
		id      string
		attempt int
	}
	overs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (over, error) {
		var o over
		return o, row.Scan(&o.id, &o.attempt)
	})
	if err != nil {
		return err
	}
	for _, o := range overs {
		name := dispatch.AttemptName(o.id, o.attempt)
		if _, err := w.exec(`UPDATE tasks SET state = 'failed' WHERE id = $1`, o.id); err != nil {
			return err
		}
		if err := w.transition(dispatch.TypeOverran, o.id, name, dispatch.Transition{Attempt: name}); err != nil {
			return err
		}
		if err := w.transition(dispatch.TypeFailed, o.id, name, dispatch.Transition{Attempt: name, Overran: true}); err != nil {
			return err
		}
	}

	rows, err = w.tx.Query(w.ctx, `SELECT id, attempt, wait FROM tasks
		WHERE state = 'waiting' AND wait_deadline <= $1`, w.now)
	if err != nil {
		return err
	}
	ws, err := pgx.CollectRows(rows, scanWaiter)
	if err != nil {
		return err
	}
	for _, t := range ws {
		if err := w.wake(t, dispatch.Wake{Wait: t.wait}); err != nil {
			return err
		}
	}

	// A pending task at attempt 0 or 1 has never started.
	rows, err = w.tx.Query(w.ctx, `SELECT id, start_by FROM tasks t
		WHERE state = 'pending' AND attempt <= 1 AND NOT not_started AND start_by < $1 AND NOT `+live, w.now)
	if err != nil {
		return err
	}
	type late struct {
		id      string
		startBy time.Time
	}
	lates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (late, error) {
		var l late
		return l, row.Scan(&l.id, &l.startBy)
	})
	if err != nil {
		return err
	}
	for _, l := range lates {
		if _, err := w.exec(`UPDATE tasks SET not_started = true WHERE id = $1`, l.id); err != nil {
			return err
		}
		if err := w.transition(dispatch.TypeNotStarted, l.id, "", dispatch.Transition{StartBy: &l.startBy}); err != nil {
			return err
		}
	}
	return w.fireSchedules()
}

func (w *wtx) fireSchedules() error {
	rows, err := w.tx.Query(w.ctx, `SELECT spec, interval_s, last_tick FROM schedules`)
	if err != nil {
		return err
	}
	type sched struct {
		spec dispatch.Schedule
		iv   int64
		last time.Time
	}
	ss, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sched, error) {
		var s sched
		var spec []byte
		if err := row.Scan(&spec, &s.iv, &s.last); err != nil {
			return s, err
		}
		return s, json.Unmarshal(spec, &s.spec)
	})
	if err != nil {
		return err
	}
	for _, s := range ss {
		latest := tickAt(w.now, s.iv)
		if !latest.After(s.last) {
			continue
		}
		skipped := int(latest.Sub(s.last)/(time.Duration(s.iv)*time.Second)) - 1
		if _, err := w.exec(`UPDATE schedules SET last_tick = $2 WHERE name = $1`, s.spec.Name, latest); err != nil {
			return err
		}
		t := s.spec.Template
		t.ID = dispatch.ScheduledTaskID(s.spec.Name, latest)
		t.StartBy = time.Time{}
		if s.spec.StartWithin > 0 {
			t.StartBy = latest.Add(s.spec.StartWithin)
		}
		if err := w.submit(t); err != nil {
			return err
		}
		if skipped > 0 {
			err := w.transition(dispatch.TypeMissed, s.spec.Name, latest.Format(time.RFC3339),
				dispatch.Transition{Tick: &latest, Skipped: skipped})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type candidate struct {
	spec    dispatch.TaskSpec
	attempt int
	reason  dispatch.ReasonKind
}

// startWhatCan offers pending tasks, in order, to the limit, admission and
// the starter. Admission and the starter are called outside any
// transaction; a start is guarded by a claim on the task's row.
func (d *Dispatcher) startWhatCan(ctx context.Context) error {
	// This instance starts one task at a time, so a claim of its own found
	// now is left from a start whose outcome was not recorded: not live.
	rows, err := d.pool.Query(ctx, `SELECT spec, payload, attempt, reason,
			state = 'running' OR (t.starting_by <> $1 AND `+live+`)
		FROM tasks t WHERE state IN ('pending', 'running')
		ORDER BY priority DESC, ord`, d.id)
	if err != nil {
		return err
	}
	var cands []candidate
	var running []dispatch.TaskSpec
	for rows.Next() {
		var spec, payload []byte
		var c candidate
		var busy bool
		if err := rows.Scan(&spec, &payload, &c.attempt, &c.reason, &busy); err != nil {
			rows.Close()
			return err
		}
		if c.spec, err = decodeSpec(spec, payload); err != nil {
			rows.Close()
			return err
		}
		if busy {
			running = append(running, c.spec)
		} else {
			cands = append(cands, c)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i, c := range cands {
		if len(running) >= d.cfg.MaxRunning {
			var ids []string
			for _, c := range cands[i:] {
				// Admission was not asked again; what it said last stands.
				if c.reason != dispatch.ReasonAdmission && c.reason != dispatch.ReasonLimit {
					ids = append(ids, c.spec.ID)
				}
			}
			if len(ids) == 0 {
				return nil
			}
			return write(ctx, d.pool, func(w *wtx) error {
				_, err := w.exec(`UPDATE tasks SET reason = 'limit', reason_detail = ''
					WHERE id = ANY($1) AND state = 'pending'`, ids)
				return err
			})
		}
		if d.cfg.Admission != nil {
			if ok, why := d.cfg.Admission(ctx, c.spec, running); !ok {
				if err := d.setReason(ctx, c, dispatch.ReasonAdmission, why); err != nil {
					return err
				}
				continue
			}
		}
		a, ok, err := d.claim(ctx, c)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		serr := d.cfg.Starter.Start(ctx, a)
		if ctx.Err() != nil {
			return ctx.Err() // killed: the claim runs out with the lease
		}
		if err := d.recordStart(ctx, a, serr); err != nil {
			return err
		}
		if serr == nil {
			running = append(running, c.spec)
		}
	}
	return nil
}

func (d *Dispatcher) setReason(ctx context.Context, c candidate, kind dispatch.ReasonKind, detail string) error {
	return write(ctx, d.pool, func(w *wtx) error {
		_, err := w.exec(`UPDATE tasks SET reason = $3, reason_detail = $4
			WHERE id = $1 AND state = 'pending' AND attempt = $2`, c.spec.ID, c.attempt, string(kind), detail)
		return err
	})
}

// claim marks c as being started by this instance, if it is still pending
// at the same attempt, nobody else is starting it, the limit allows, and no
// task.finished for the attempt is in the log.
func (d *Dispatcher) claim(ctx context.Context, c candidate) (dispatch.Attempt, bool, error) {
	n := max(c.attempt, 1)
	a := dispatch.Attempt{Number: n, Name: dispatch.AttemptName(c.spec.ID, n)}
	var spec, payload, wake []byte
	found := false
	err := write(ctx, d.pool, func(w *wtx) error {
		err := w.tx.QueryRow(ctx, `UPDATE tasks t SET starting_by = $3, attempt = $4
			WHERE id = $1 AND state = 'pending' AND attempt = $2
			  AND (t.starting_by IS NULL OR t.starting_by = $3 OR NOT `+live+`)
			  AND (SELECT count(*) FROM tasks t WHERE state = 'running'
			         OR (state = 'pending' AND t.starting_by <> $3 AND `+live+`)) < $5
			  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.type = 'task.finished' AND e.subject = $1
			         AND CASE WHEN json_typeof(e.data) = 'object' THEN e.data->>'attempt' = $6 END)
			RETURNING spec, payload, wake`,
			c.spec.ID, c.attempt, d.id, n, d.cfg.MaxRunning, a.Name).Scan(&spec, &payload, &wake)
		if err == pgx.ErrNoRows {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found {
		return a, false, err
	}
	if a.Task, err = decodeSpec(spec, payload); err != nil {
		return a, false, err
	}
	if len(wake) > 0 {
		a.Wake = new(dispatch.Wake)
		if err := json.Unmarshal(wake, a.Wake); err != nil {
			return a, false, err
		}
	}
	return a, true, nil
}

// recordStart records how a start call went, if this instance's claim
// still stands: the task may have finished meanwhile, or another instance
// may have taken over a claim whose lease ran out.
func (d *Dispatcher) recordStart(ctx context.Context, a dispatch.Attempt, serr error) error {
	return write(ctx, d.pool, func(w *wtx) error {
		if serr != nil {
			_, err := w.exec(`UPDATE tasks SET starting_by = NULL, reason = 'start-failed', reason_detail = $4
				WHERE id = $1 AND attempt = $2 AND state = 'pending' AND starting_by = $3`,
				a.Task.ID, a.Number, d.id, serr.Error())
			return err
		}
		n, err := w.exec(`UPDATE tasks SET state = 'running', starting_by = NULL, started_at = $4, wake = NULL,
				reason = '', reason_detail = ''
			WHERE id = $1 AND attempt = $2 AND state = 'pending' AND starting_by = $3`,
			a.Task.ID, a.Number, d.id, w.now)
		if err != nil || n == 0 {
			return err
		}
		return w.transition(dispatch.TypeStarted, a.Task.ID, a.Name, dispatch.Transition{Attempt: a.Name})
	})
}
