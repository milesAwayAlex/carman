package pgxdispatch

import (
	"context"
	"time"
)

const (
	pruneTasks  = 100  // finished tasks removed per statement
	pruneEvents = 1000 // events removed per statement
)

// pruneEvery is how often the leader looks for what retention removes.
func (o Options) pruneEvery() time.Duration {
	shortest := o.Retention
	for _, p := range o.RetentionByType {
		if p > 0 && (shortest == 0 || p < shortest) {
			shortest = p
		}
	}
	return min(max(shortest/4, time.Second), time.Minute)
}

func (o Options) prunes() bool {
	if o.Retention > 0 {
		return true
	}
	for _, p := range o.RetentionByType {
		if p > 0 {
			return true
		}
	}
	return false
}

// prune removes what the retention periods let go. Only the instance that
// leads (the live one with the lowest id) does it, in short statements
// that do not take the write lock, so the loops are not held up. An event
// the dispatcher has not yet acted on (above meta.cursor) is never removed.
func (d *Dispatcher) prune(ctx context.Context) error {
	if d.schema < 2 {
		return nil
	}
	var leads bool
	err := d.pool.QueryRow(ctx, `SELECT coalesce(min(id) = $1, false) FROM instances
		i WHERE `+liveInstance(d.schema), d.id).Scan(&leads)
	if err != nil || !leads {
		return err
	}
	if d.opts.Retention > 0 {
		// Tasks a build before 0002 finished have no end time yet.
		_, err := d.pool.Exec(ctx, `UPDATE tasks SET ended_at = clock_timestamp()
			WHERE state IN ('done', 'failed') AND ended_at IS NULL`)
		if err != nil {
			return err
		}
		for {
			var n int
			err := d.pool.QueryRow(ctx, `WITH gone AS (
					DELETE FROM tasks WHERE id IN (SELECT id FROM tasks
						WHERE state IN ('done', 'failed') AND ended_at < clock_timestamp() - $1 * interval '1 millisecond'
						ORDER BY ended_at LIMIT $2)
					RETURNING id),
				evs AS (DELETE FROM events WHERE subject IN (SELECT id FROM gone)
					AND seq <= (SELECT cursor FROM meta))
				SELECT count(*) FROM gone`, d.opts.Retention.Milliseconds(), pruneTasks).Scan(&n)
			if err != nil || n < pruneTasks {
				if err != nil {
					return err
				}
				break
			}
		}
	}
	// Events that belong to no task: a type of their own, or the default.
	var listed []string
	for typ, p := range d.opts.RetentionByType {
		listed = append(listed, typ)
		if p > 0 {
			if err := d.pruneEvents(ctx, p, `type = $3`, typ); err != nil {
				return err
			}
		}
	}
	if d.opts.Retention > 0 {
		return d.pruneEvents(ctx, d.opts.Retention, `type <> ALL($3)`, listed)
	}
	return nil
}

func (d *Dispatcher) pruneEvents(ctx context.Context, period time.Duration, filter string, arg any) error {
	for {
		tag, err := d.pool.Exec(ctx, `DELETE FROM events WHERE seq IN (SELECT seq FROM events e
				WHERE added < clock_timestamp() - $1 * interval '1 millisecond' AND `+filter+`
				  AND seq <= (SELECT cursor FROM meta)
				  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = e.subject)
				ORDER BY added LIMIT $2)`, period.Milliseconds(), pruneEvents, arg)
		if err != nil {
			return err
		}
		if tag.RowsAffected() < pruneEvents || ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
