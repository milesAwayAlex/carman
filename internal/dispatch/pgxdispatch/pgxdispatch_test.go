package pgxdispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"carman/internal/dispatch"
	"carman/internal/dispatch/dispatchtest"
)

var (
	adminOnce sync.Once
	admin     *pgxpool.Pool
	adminErr  error
)

// db is one test's schema.
type db struct{ url string }

func (d db) Open(cfg dispatch.Config, build string) (dispatchtest.Instance, error) {
	return Open(context.Background(), d.url, cfg, build)
}

func (d db) Emit(ctx context.Context, e dispatch.Event) error { return Emit(ctx, d.url, e) }

// factory gives each test a schema of its own, dropped when it ends.
func factory(t *testing.T) dispatchtest.Database {
	base := dispatchtest.DatabaseURL(t)
	adminOnce.Do(func() { admin, adminErr = pgxpool.New(context.Background(), base) })
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "dispatch_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", name)
	q.Set("pool_max_conns", "2")
	u.RawQuery = q.Encode()
	return db{u.String()}
}

// TestAcceptance runs the dispatcher's acceptance suite (R2, R6, R7).
func TestAcceptance(t *testing.T) { dispatchtest.Run(t, factory) }
