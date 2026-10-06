//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOpen_CtxCancelStopsBackend guards that pgx keeps sending a CancelRequest on ctx deadline (pgconn asyncClose), so a pgx upgrade that drops it fails here.
func TestOpen_CtxCancelStopsBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	cases := []struct {
		name   string
		marker string
		run    func(context.Context, string) error
	}{
		{"exec", "ctx_cancel_backend_exec", func(qctx context.Context, q string) error {
			_, err := db.ExecContext(qctx, q)
			return err
		}},
		{"query", "ctx_cancel_backend_query", func(qctx context.Context, q string) error {
			var v string
			return db.QueryRowContext(qctx, q).Scan(&v)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := "SELECT pg_sleep(30)::text /* " + tc.marker + " */"

			// Control: with no deadline the probe must see the backend active.
			cctx, ccancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- tc.run(cctx, "SELECT pg_sleep(30)::text /* "+tc.marker+"_ctl */") }()
			seen := false
			for i := 0; i < 40 && !seen; i++ {
				seen = backendsRunning(t, ctx, db, tc.marker+"_ctl") > 0
				if !seen {
					time.Sleep(50 * time.Millisecond)
				}
			}
			ccancel()
			<-done
			if !seen {
				t.Fatalf("control: probe never saw the running backend; the test cannot detect a leak")
			}

			qctx, qcancel := context.WithTimeout(ctx, 300*time.Millisecond)
			err := tc.run(qctx, q)
			qcancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
			}

			gaveUp := time.Now()
			for {
				n := backendsRunning(t, ctx, db, tc.marker)
				if n == 0 {
					t.Logf("backend gone %s after the client gave up", time.Since(gaveUp).Round(time.Millisecond))
					return
				}
				if time.Since(gaveUp) > 2*time.Second {
					t.Fatalf("%d backend(s) still running pg_sleep %s after the ctx deadline", n, time.Since(gaveUp).Round(time.Millisecond))
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

func backendsRunning(t *testing.T, ctx context.Context, db *sql.DB, marker string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE pid <> pg_backend_pid() AND state = 'active'
		    AND strpos(query, 'pg_sleep') > 0 AND strpos(query, $1) > 0`,
		marker).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}
