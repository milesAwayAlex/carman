package pgxdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"carman/internal/dispatch"
)

// Mark reads a value the service keeps under name; false when there is none.
func (d *Dispatcher) Mark(ctx context.Context, name string, v any) (bool, error) {
	var raw []byte
	err := d.pool.QueryRow(ctx, `SELECT value FROM marks WHERE name = $1`, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// SetMark keeps v under name.
func (d *Dispatcher) SetMark(ctx context.Context, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.pool.Exec(ctx, `INSERT INTO marks (name, value) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, string(b))
	return err
}

// JobsEnded lists the Jobs whose end the log records for what ran (a
// job.ended event's subject), among events from since on.
func (d *Dispatcher) JobsEnded(ctx context.Context, subject string, since time.Time) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT coalesce(data->>'job', '') FROM events
		WHERE type = 'job.ended' AND subject = $1 AND time >= $2`, subject, since)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out, err
}

// EventsOfTypes lists up to limit records of the given types that arrived after
// seq after, in log order: Events, for the readers that want only some.
func (d *Dispatcher) EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE seq > $1 AND type = ANY($2) ORDER BY seq LIMIT $3`, after, types, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}
