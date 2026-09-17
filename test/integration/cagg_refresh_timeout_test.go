//go:build integration

package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRefreshContinuousAggregate_PerCallBound is the W8-19 proof on a
// real TimescaleDB: a refresh_continuous_aggregate CALL made through
// the store runs under a per-CALL statement_timeout, and when that
// bound fires the caller gets a typed error naming the view and the
// window, the BACKEND (not the client socket) cancelled the refresh,
// and the pooled connection it ran on is returned healthy with its
// previous statement_timeout restored.
//
// The pool is pinned to ONE connection for the whole test so every
// statement after the timed-out CALL demonstrably rides the same
// connection the CALL was cancelled on — the "does not poison the
// pool" claim is about that connection, and a fresh dial would prove
// nothing.
func TestRefreshContinuousAggregate_PerCallBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.DB().SetMaxOpenConns(1)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc)

	// Two disjoint windows of three one-minute buckets each, well in the
	// past so no policy refresh can race the test's own CALLs.
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	windowA := base
	windowB := base.Add(2 * time.Hour)
	nonce := 0
	for _, start := range []time.Time{windowA, windowB} {
		for i := range 3 {
			nonce++
			tr := mkAPITrade(nonce, start.Add(time.Duration(i)*time.Minute), xlmUSDC, 1_000_000, 500_000)
			if err := store.InsertTrade(ctx, tr); err != nil {
				t.Fatalf("InsertTrade: %v", err)
			}
		}
	}
	// A window must span >= 2 buckets; [start, start+3m] does for prices_1m.
	fromA, toA := timescale.PadRefreshWindow(windowA, windowA.Add(3*time.Minute), 2*time.Minute)
	fromB, toB := timescale.PadRefreshWindow(windowB, windowB.Add(3*time.Minute), 2*time.Minute)

	bucketsIn := func(from, to time.Time) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM prices_1m WHERE bucket >= $1::timestamptz AND bucket < $2::timestamptz`,
			from, to).Scan(&n); err != nil {
			t.Fatalf("count prices_1m: %v", err)
		}
		return n
	}
	sessionTimeout := func() string {
		t.Helper()
		var v string
		if err := store.DB().QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&v); err != nil {
			t.Fatalf("current_setting: %v", err)
		}
		return v
	}

	// ── generous bound: the refresh lands ─────────────────────────────
	if err := store.RefreshContinuousAggregateWithTimeout(ctx, "prices_1m", fromA, toA, time.Minute); err != nil {
		t.Fatalf("refresh under a generous bound: %v", err)
	}
	if got := bucketsIn(fromA, toA); got != 3 {
		t.Fatalf("window A: %d prices_1m buckets after refresh, want 3", got)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after a successful bounded refresh the connection's statement_timeout = %q, want \"0\" (restored)", got)
	}

	// ── 1 ms bound: the typed timeout error, from the backend ─────────
	err = store.RefreshContinuousAggregateWithTimeout(ctx, "prices_1m", fromB, toB, time.Millisecond)
	var tErr *timescale.CAGGRefreshTimeoutError
	if !errors.As(err, &tErr) {
		t.Fatalf("refresh under a 1ms bound: err = %v, want *timescale.CAGGRefreshTimeoutError", err)
	}
	if tErr.View != "prices_1m" || !tErr.From.Equal(fromB) || !tErr.To.Equal(toB) || tErr.Timeout != time.Millisecond {
		t.Errorf("typed error = {View:%s From:%s To:%s Timeout:%s}, want {prices_1m %s %s 1ms}",
			tErr.View, tErr.From, tErr.To, tErr.Timeout, fromB, toB)
	}
	// A PgError carrying the statement_timeout message on the chain
	// proves the SERVER cancelled the statement — the refresh lock is
	// released and the connection is intact. A Go-side deadline
	// (context.DeadlineExceeded) would mean the socket was closed under
	// a still-running backend. The code is asserted as observed: the
	// procedure re-throws the cancellation as XX000 (internal_error),
	// not 57014 — the first run of this test against
	// timescale/timescaledb:2.26.4-pg15 is how that was learned, and a
	// Timescale release that starts surfacing 57014 should fail here so
	// the classifier's comment is corrected with it.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "canceling statement due to statement timeout") {
		t.Fatalf("timeout must be the backend's statement_timeout cancellation; got %v", err)
	}
	if pgErr.Code != "XX000" {
		t.Fatalf("refresh_continuous_aggregate surfaced the cancellation as SQLSTATE %s, not the XX000 the classifier documents — update isStatementTimeoutErr's comment", pgErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must not have come from the Go-side deadline: %v", err)
	}

	// ── the same connection still serves, with its timeout restored ───
	var one int
	if err := store.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 on the pool after the timed-out CALL: %d, %v", one, err)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after the timed-out refresh the connection's statement_timeout = %q, want \"0\" — the CALL's bound leaked onto the pooled connection", got)
	}
	// And the abandoned window is refreshable again: nothing is left
	// holding the view's refresh lock, and the default (window-derived)
	// bound is the path the backfill takes.
	if err := store.RefreshContinuousAggregate(ctx, "prices_1m", fromB, toB); err != nil {
		t.Fatalf("re-refresh of the interrupted window: %v", err)
	}
	if got := bucketsIn(fromB, toB); got != 3 {
		t.Fatalf("window B: %d prices_1m buckets after the re-refresh, want 3", got)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after the default-bound refresh the connection's statement_timeout = %q, want \"0\"", got)
	}
}
