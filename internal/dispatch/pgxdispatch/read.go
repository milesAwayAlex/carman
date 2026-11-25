package pgxdispatch

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"carman/internal/dispatch"
)

// These reads are not in the contract: they are what the service shows its
// owner. They take no lock and change nothing.

// Tasks returns up to limit tasks in state, with the reason each is not
// running. Open tasks come in the order they would start (priority, then
// submission); finished ones latest first.
func (d *Dispatcher) Tasks(ctx context.Context, state dispatch.State, limit int) ([]dispatch.TaskStatus, error) {
	order := `priority DESC, ord`
	switch state {
	case dispatch.Pending, dispatch.Running, dispatch.Waiting:
	case dispatch.Done, dispatch.Failed:
		order = `ord DESC`
		if d.schema >= 2 {
			order = `ended_at DESC NULLS FIRST, ord DESC`
		}
	default:
		return nil, fmt.Errorf("%w: no state %q", dispatch.ErrInvalid, state)
	}
	rows, err := d.pool.Query(ctx, `SELECT id FROM tasks WHERE state = $1 ORDER BY `+order+` LIMIT $2`, string(state), limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.TaskStatus, 0, len(ids))
	for _, id := range ids {
		st, err := d.Explain(ctx, id)
		if err == dispatch.ErrNotFound {
			continue // removed by retention since the list was read
		}
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// EventsOf is Events restricted to one type, with the same read order.
func (d *Dispatcher) EventsOf(ctx context.Context, after int64, typ string, limit int) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE seq > $1 AND type = $2 ORDER BY seq LIMIT $3`, after, typ, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

// Last returns the last n records of the log, of type typ when it is not
// empty, in log order.
func (d *Dispatcher) Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT * FROM (SELECT seq, id, type, source, subject, time, data
		FROM events WHERE $1 = '' OR type = $1 ORDER BY seq DESC LIMIT $2) last ORDER BY seq`, typ, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}
