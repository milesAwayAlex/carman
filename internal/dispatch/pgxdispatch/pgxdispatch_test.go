package pgxdispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

func (d db) schema() string {
	u, _ := url.Parse(d.url)
	return u.Query().Get("search_path")
}

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

// TestR2LogReadOrder: with many concurrent appenders, a reader that tails
// the log never skips a record it later finds below its position.
func TestR2LogReadOrder(t *testing.T) {
	d := factory(t).(db)
	ctx := context.Background()
	inst, err := Open(ctx, d.url, dispatch.Config{Starter: nopStarter{}, MaxRunning: 1}, "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				e := dispatch.Event{ID: fmt.Sprintf("w%d-%d", w, i), Type: "x", Source: "test"}
				if err := inst.Emit(ctx, e); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	seen := map[string]bool{}
	var after int64
	tail := func() {
		rs, err := inst.Events(ctx, after, 37)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			seen[r.ID] = true
			after = r.Seq
		}
	}
	for {
		select {
		case <-done:
			for range writers * each {
				tail()
			}
			if len(seen) != writers*each {
				t.Fatalf("the tailing reader saw %d of %d events", len(seen), writers*each)
			}
			return
		default:
			tail()
		}
	}
}

type nopStarter struct{}

func (nopStarter) Start(context.Context, dispatch.Attempt) error { return nil }
