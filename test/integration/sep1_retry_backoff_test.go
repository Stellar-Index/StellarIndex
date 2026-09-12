//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SEP-1 retry-ladder round-trip against a real Postgres (migration 0159).
//
// Every claim here needs the database. The ladder is computed inside the
// UPDATE (`$2::interval * POWER(2, …)` clamped by `$3::interval`) so that the
// new failure count and the deferral derived from it cannot disagree, and the
// queue's new arm is a column predicate — neither exists in Go to unit-test.
// An untyped bind parameter beside an interval operator also compiles and
// reviews perfectly while raising 42883 on every call at runtime, so the only
// honest test of this SQL is one that EXECUTES it.
//
// Production opens the store with timescale.Open (internal/ops/ingest/
// sep1_refresh.go, `store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)`)
// and this test uses that same constructor — not OpenServing/OpenBackground,
// which wrap it with a statement timeout the cron does not set.
func TestSep1RetryBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Real r1 residue (2026-09-12). coinonstellar.com is NXDOMAIN;
	// centre.io is the Circle toml the USDC issuer resolves through;
	// litemint.store fronts a large family of parked subdomains.
	const (
		dead      = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
		healthy   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		recovered = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: dead, homeDomain: "coinonstellar.com"},
		{g: healthy, homeDomain: "centre.io"},
		{g: recovered, homeDomain: "litemint.store"},
	})

	t.Run("the ladder doubles and then caps", func(t *testing.T) {
		// 1d, 2d, 4d, 8d, 16d, then the 30d cap — NOT 32d. The first step
		// is deliberately the cadence a healthy domain already gets, so a
		// domain's first failure costs it nothing.
		want := []time.Duration{
			24 * time.Hour,
			2 * 24 * time.Hour,
			4 * 24 * time.Hour,
			8 * 24 * time.Hour,
			16 * 24 * time.Hour,
			30 * 24 * time.Hour,
			30 * 24 * time.Hour,
		}
		for i, wantDefer := range want {
			streak, ferr := store.MarkIssuerSep1Failed(ctx, dead)
			if ferr != nil {
				t.Fatalf("MarkIssuerSep1Failed #%d: %v", i+1, ferr)
			}
			if streak != i+1 {
				t.Fatalf("failure #%d returned streak %d, want %d", i+1, streak, i+1)
			}
			gotDefer := sep1Deferral(t, ctx, store, dead)
			if gotDefer != wantDefer {
				t.Errorf("failure #%d deferred by %v, want %v", i+1, gotDefer, wantDefer)
			}
		}
	})

	t.Run("a repeatedly-failing domain is not selected while a healthy one is", func(t *testing.T) {
		// This is the defect. Both rows are equally stale — 40 days since
		// the last attempt, well past `-older-than 24h` — and before the
		// ladder both were selected, with the dead one AHEAD of the healthy
		// one whenever its sep1_resolved_at was the older of the two. The
		// only thing that may keep it out now is its own deferral.
		ageSep1ResolvedAt(t, ctx, store, dead, 40*24*time.Hour)
		ageSep1ResolvedAt(t, ctx, store, healthy, 40*24*time.Hour)
		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)

		got := candidateSet(t, ctx, store, 24*time.Hour, 100)
		if got[dead] {
			t.Errorf("the 7x-failed domain is still a candidate — the deferral is not being applied")
		}
		if !got[healthy] {
			t.Errorf("the never-failed domain is NOT a candidate; want it selected every cadence")
		}
	})

	t.Run("a success clears the ladder and restores the fast cadence", func(t *testing.T) {
		// Three failures first: enough to earn a 4-day deferral, so the
		// recovery has something real to undo.
		for i := 0; i < 3; i++ {
			if _, ferr := store.MarkIssuerSep1Failed(ctx, recovered); ferr != nil {
				t.Fatalf("MarkIssuerSep1Failed: %v", ferr)
			}
		}
		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)
		if candidateSet(t, ctx, store, 24*time.Hour, 100)[recovered] {
			t.Fatalf("a 3x-failed domain is still a candidate; the rest of this case proves nothing")
		}

		if err := store.SetIssuerSep1Payload(ctx, recovered,
			[]byte(`{"OrgName":"Litemint","OrgVerified":false}`)); err != nil {
			t.Fatalf("SetIssuerSep1Payload: %v", err)
		}
		failures, next := sep1Ladder(t, ctx, store, recovered)
		if !failures.Valid || failures.Int64 != 0 {
			t.Errorf("after a success sep1_consecutive_failures = %v, want 0", failures)
		}
		if next.Valid {
			t.Errorf("after a success sep1_next_attempt_after = %v, want NULL — a recovering domain "+
				"must not serve out the deferral it earned while it was dead", next.Time)
		}

		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)
		if !candidateSet(t, ctx, store, 24*time.Hour, 100)[recovered] {
			t.Errorf("a recovered domain is not back in the queue")
		}
	})

	t.Run("unwinding a run takes back exactly one step", func(t *testing.T) {
		// The systemic-outage escape hatch. Everything the run failed gets
		// its ladder step back and its deferral lifted, so a night when our
		// DNS was broken leaves no trace on the schedule.
		if _, ferr := store.MarkIssuerSep1Failed(ctx, healthy); ferr != nil {
			t.Fatalf("MarkIssuerSep1Failed: %v", ferr)
		}
		ageSep1ResolvedAt(t, ctx, store, healthy, 40*24*time.Hour)
		if candidateSet(t, ctx, store, 24*time.Hour, 100)[healthy] {
			t.Fatalf("a once-failed domain is still a candidate; the rest of this case proves nothing")
		}

		n, uerr := store.UnwindIssuerSep1Backoff(ctx, []string{healthy})
		if uerr != nil {
			t.Fatalf("UnwindIssuerSep1Backoff: %v", uerr)
		}
		if n != 1 {
			t.Errorf("unwound %d row(s), want 1", n)
		}
		failures, next := sep1Ladder(t, ctx, store, healthy)
		if !failures.Valid || failures.Int64 != 0 {
			t.Errorf("sep1_consecutive_failures = %v, want the pre-run 0", failures)
		}
		if next.Valid {
			t.Errorf("sep1_next_attempt_after = %v, want NULL", next.Time)
		}
		if !candidateSet(t, ctx, store, 24*time.Hour, 100)[healthy] {
			t.Errorf("an unwound domain is not back in the queue")
		}

		// Unwinding a row that never failed must floor at zero rather than
		// trip issuers_sep1_consecutive_failures_check.
		if _, uerr = store.UnwindIssuerSep1Backoff(ctx, []string{healthy}); uerr != nil {
			t.Errorf("unwinding an already-zero row: %v, want it to floor at 0", uerr)
		}
		// And an empty batch must not build `= ANY('{}')` for nothing.
		if n, uerr = store.UnwindIssuerSep1Backoff(ctx, nil); uerr != nil || n != 0 {
			t.Errorf("UnwindIssuerSep1Backoff(nil) = %d, %v; want 0, nil", n, uerr)
		}
	})

	t.Run("an over-range limit clamps to the ceiling, not to 100", func(t *testing.T) {
		// The old code reset ANY out-of-range limit to 100, so an operator
		// raising LIMIT past the cap silently got a fifth of the previous
		// budget. It reads as "the job is slow", never as "your setting was
		// rejected" — precisely the shape of bug this ladder exists to
		// remove elsewhere.
		const extra = 150
		rows := make([]seedIssuer, 0, extra)
		for i := 0; i < extra; i++ {
			rows = append(rows, seedIssuer{
				g:          fmt.Sprintf("GBULK%051d", i),
				homeDomain: fmt.Sprintf("bulk-%03d.example", i),
			})
		}
		seedIssuers(t, ctx, store, rows)

		got, qerr := store.IssuersNeedingSep1Refresh(ctx, 0, 999_999)
		if qerr != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", qerr)
		}
		if len(got) <= 100 {
			t.Errorf("an over-range limit returned %d row(s) out of %d eligible — it is still being "+
				"snapped down to the old 100 default", len(got), extra)
		}
	})
}

// sep1Ladder reads the two migration-0159 columns straight from the row.
func sep1Ladder(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) (sql.NullInt64, sql.NullTime) {
	t.Helper()
	var (
		failures sql.NullInt64
		next     sql.NullTime
	)
	err := store.DB().QueryRowContext(ctx,
		`SELECT sep1_consecutive_failures, sep1_next_attempt_after FROM issuers WHERE g_strkey = $1`,
		gStrkey,
	).Scan(&failures, &next)
	if err != nil {
		t.Fatalf("read ladder for %s: %v", gStrkey, err)
	}
	return failures, next
}

// sep1Deferral returns how far past the attempt the next one was pushed.
// Both columns are set to NOW() in the same statement, so their difference
// is exactly the interval the ladder produced — no clock skew to tolerate.
func sep1Deferral(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) time.Duration {
	t.Helper()
	var secs float64
	err := store.DB().QueryRowContext(ctx, `
        SELECT extract(epoch FROM sep1_next_attempt_after - sep1_resolved_at)
          FROM issuers WHERE g_strkey = $1`, gStrkey).Scan(&secs)
	if err != nil {
		t.Fatalf("read deferral for %s: %v", gStrkey, err)
	}
	return time.Duration(secs) * time.Second
}

// ageSep1ResolvedAt backdates the last-attempt stamp so the `-older-than`
// arm of the queue is satisfied and the DEFERRAL arm is the only thing left
// deciding. Without this every row the test just wrote is < 24h old and
// nothing would be selected for reasons that have nothing to do with 0159.
func ageSep1ResolvedAt(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string, age time.Duration) {
	t.Helper()
	_, err := store.DB().ExecContext(ctx,
		`UPDATE issuers SET sep1_resolved_at = NOW() - $2::interval WHERE g_strkey = $1`,
		gStrkey, fmt.Sprintf("%d seconds", int64(age/time.Second)))
	if err != nil {
		t.Fatalf("backdate %s: %v", gStrkey, err)
	}
}

func candidateSet(t *testing.T, ctx context.Context, store *timescale.Store, staleness time.Duration, limit int) map[string]bool {
	t.Helper()
	got, err := store.IssuersNeedingSep1Refresh(ctx, staleness, limit)
	if err != nil {
		t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
	}
	out := make(map[string]bool, len(got))
	for _, c := range got {
		out[c.GStrkey] = true
	}
	return out
}
