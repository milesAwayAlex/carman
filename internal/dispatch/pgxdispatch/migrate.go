package pgxdispatch

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema's migrations, numbered NNNN_name.sql. Each is applied once, in
// one transaction. They are never edited once released: a change is a new
// file. See the README's "Changing the schema".
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

var migrations = loadMigrations()

// latest is the newest schema version this build knows.
var latest = migrations[len(migrations)-1].version

func loadMigrations() []migration {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		panic(err)
	}
	var ms []migration
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			panic("pgxdispatch: migration without a number: " + base)
		}
		body, err := migrationFiles.ReadFile(n)
		if err != nil {
			panic(err)
		}
		ms = append(ms, migration{version: v, name: base, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			panic(fmt.Sprintf("pgxdispatch: migration %s is out of sequence", m.name))
		}
	}
	return ms
}

// ErrSchemaNewer is returned when the database's schema is newer than this
// build knows: a newer build has migrated it, and this one must not work on it.
var ErrSchemaNewer = errors.New("pgxdispatch: the database's schema is newer than this build knows")

// migrate brings the schema up to version upTo and returns the version it
// is at. Two instances starting together take turns on an advisory lock, so
// each migration is applied once.
func migrate(ctx context.Context, pool *pgxpool.Pool, upTo int) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	// Advisory locks are the database's, not the schema's.
	const key = `hashtext('pgxdispatch schema ' || current_schema())`
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+key+`)`); err != nil {
		return 0, err
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(`+key+`)`); err != nil {
			conn.Conn().Close(context.Background()) // the lock goes with the session
		}
	}()
	_, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_versions (
		version integer PRIMARY KEY,
		name    text NOT NULL,
		applied timestamptz NOT NULL DEFAULT clock_timestamp())`)
	if err != nil {
		return 0, err
	}
	var at int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_versions`).Scan(&at); err != nil {
		return 0, err
	}
	if at > upTo {
		return at, fmt.Errorf("%w: it is at version %d, this build knows up to %d; it must not take work",
			ErrSchemaNewer, at, upTo)
	}
	for _, m := range migrations {
		if m.version <= at || m.version > upTo {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_versions (version, name) VALUES ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return at, fmt.Errorf("pgxdispatch: migration %s: %w", m.name, err)
		}
		at = m.version
	}
	return at, nil
}
