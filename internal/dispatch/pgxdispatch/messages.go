package pgxdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

// What messaging (ADR 0045) needs from the log, beyond the contract: a
// message is an event, and what became of it is events whose IDs say which
// message they are about, so that the log's refusal of an ID twice is what
// makes a delivery happen once.

// Claim appends e unless the log has an event with its ID, and says whether
// this call appended it.
func (d *Dispatcher) Claim(ctx context.Context, e dispatch.Event) (bool, error) {
	if err := validEvent(e); err != nil {
		return false, err
	}
	var added bool
	err := d.write(ctx, func(w *wtx) error {
		before := w.seq
		if err := w.append(e); err != nil {
			return err
		}
		added = w.seq > before
		return nil
	})
	d.poke()
	return added, err
}

// Unmarked are the messages sent to any of the addresses (any at all when
// none is given) that have no event marked KIND (delivered, read) yet, in
// the order they were sent. A message whose delivery was found to have
// reached nobody (message.undelivered, ADR 0053) is undelivered until it
// is delivered again.
func (d *Dispatcher) Unmarked(ctx context.Context, to []string, kind string, limit int) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data FROM events m
		WHERE m.type = 'message.sent' AND ($1::text[] IS NULL OR (m.data->>'to') = ANY($1))
		  AND (NOT EXISTS (SELECT 1 FROM events x WHERE x.id = $2 || m.subject)
		    OR ($2 = 'message.delivered/'
		      AND EXISTS (SELECT 1 FROM events x WHERE x.id = 'message.undelivered/' || m.subject)
		      AND NOT EXISTS (SELECT 1 FROM events x WHERE x.id = 'message.delivered/' || m.subject || '/again')))
		ORDER BY seq LIMIT $3`, to, "message."+kind+"/", limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

// About is every event whose subject is the one given, in order.
func (d *Dispatcher) About(ctx context.Context, subject string) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE subject = $1 ORDER BY seq`, subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

// Answers are the messages that answer the one given.
func (d *Dispatcher) Answers(ctx context.Context, id string) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE type = 'message.sent' AND (data->>'answers') = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

// OfTypeAbout are the events of one type about one subject.
func (d *Dispatcher) OfTypeAbout(ctx context.Context, typ string, subject string) ([]dispatch.Record, error) {
	rows, err := d.pool.Query(ctx, `SELECT seq, id, type, source, subject, time, data
		FROM events WHERE type = $1 AND subject = $2 ORDER BY seq`, typ, subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

// Sessions are kept in the marks table, one mark each (session/ID), so that
// messaging needs no migration: a build from before it runs on the same
// database, and going back to one is possible.

const sessionMark = "session/"

var errNoSessions = errors.New("pgxdispatch: this schema has no marks table yet")

func scanSession(row pgx.CollectableRow) (message.Session, error) {
	var raw []byte
	var s message.Session
	if err := row.Scan(&raw); err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

// PutSession records where a session is and what it is doing, and gives
// what was recorded before, if anything was.
func (d *Dispatcher) PutSession(ctx context.Context, s message.Session) (message.Session, bool, error) {
	if d.schema < 4 {
		return message.Session{}, false, errNoSessions
	}
	if s.ID == "" || s.Agent == "" || s.State == "" {
		return message.Session{}, false, fmt.Errorf("%w: a session needs an id, an agent and a state", dispatch.ErrInvalid)
	}
	var prev message.Session
	found := false
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT value FROM marks WHERE name = $1 FOR UPDATE`, sessionMark+s.ID)
		if err != nil {
			return err
		}
		old, err := pgx.CollectRows(rows, scanSession)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		s.Seen = now
		if found = len(old) == 1; found {
			prev = old[0]
			if prev.PodUID == s.PodUID && prev.State != message.Ended && !prev.Started.IsZero() {
				s.Started = prev.Started
			}
		}
		if s.Started.IsZero() {
			s.Started = now
		}
		b, _ := json.Marshal(s)
		_, err = tx.Exec(ctx, `INSERT INTO marks (name, value) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET value = excluded.value`, sessionMark+s.ID, string(b))
		return err
	})
	return prev, found, err
}

// GetSession is one session, by its whole id.
func (d *Dispatcher) GetSession(ctx context.Context, id string) (message.Session, bool, error) {
	if d.schema < 4 {
		return message.Session{}, false, errNoSessions
	}
	rows, err := d.pool.Query(ctx, `SELECT value FROM marks WHERE name = $1`, sessionMark+id)
	if err != nil {
		return message.Session{}, false, err
	}
	ss, err := pgx.CollectRows(rows, scanSession)
	if err != nil || len(ss) == 0 {
		return message.Session{}, false, err
	}
	return ss[0], true, nil
}

// Sessions are the sessions seen since a time, the latest first; of one
// agent when one is named.
func (d *Dispatcher) Sessions(ctx context.Context, agent string, since time.Time) ([]message.Session, error) {
	if d.schema < 4 {
		return nil, errNoSessions
	}
	rows, err := d.pool.Query(ctx, `SELECT value FROM marks WHERE name LIKE 'session/%'
		AND ($1 = '' OR value->>'agent' = $1) AND (value->>'seen')::timestamptz >= $2
		ORDER BY (value->>'seen')::timestamptz DESC LIMIT 500`, agent, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSession)
}

// Correspondents are the session addresses that sent or were sent a
// message since a time.
func (d *Dispatcher) Correspondents(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT DISTINCT a FROM events,
		LATERAL (VALUES (data->>'to'), (data->>'from')) v(a)
		WHERE type = 'message.sent' AND time >= $1 AND a LIKE 'session/%'`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ForgetSessions removes sessions last seen before a time.
func (d *Dispatcher) ForgetSessions(ctx context.Context, before time.Time) error {
	if d.schema < 4 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `DELETE FROM marks WHERE name LIKE 'session/%' AND (value->>'seen')::timestamptz < $1`, before)
	return err
}
