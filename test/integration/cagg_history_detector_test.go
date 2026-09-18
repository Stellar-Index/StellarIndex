//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCAGGHistoryDetector executes the data-freshness watchdog's
// emptied-continuous-aggregate SQL — the SHIPPED bytes, read out of
// configs/ansible/roles/archival-node/files/data-freshness.sh — against
// a real TimescaleDB carrying the real migrations.
//
// The state under test is the one migrations 0115 and 0147 leave behind
// and the one a fresh database applying them from zero starts in: the
// nine price/TWAP views recreated WITH NO DATA, with each view's own
// refresh policy re-filling only its trailing start_offset sliver. Every
// newest-bar signal reads green there — last refresh, bar age, the
// ADR-0033 verdict — while the API serves no OHLC, no chart and no
// since-inception history for the whole back-history.
//
// Two properties are pinned:
//
//  1. every one of the nine views is judged, not just the two TWAPs;
//  2. the judgement's reference survives the migration that causes the
//     defect. Deriving it from prices_1m (the previous shape) made the
//     detector self-muting: 0147 empties prices_1m too, so the
//     reference collapses to the same sliver as the subject and the
//     gauge publishes a healthy 0 for a database with no history at all.
func TestCAGGHistoryDetector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	// Ten days of trades — the lake the nine views aggregate.
	now := time.Now().UTC()
	for i := range 10 {
		seedDetectorTrade(t, ctx, db, 100+i, now.AddDate(0, 0, -9+i))
	}

	// The post-migration state: the coarse views hold nothing at all,
	// and prices_1m holds only what a trailing-window policy tick would
	// have re-filled since the recreate.
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', $1::timestamptz, $2::timestamptz)`,
		now.Add(-2*time.Hour), now,
	); err != nil {
		t.Fatalf("refresh prices_1m sliver: %v", err)
	}

	t.Run("every emptied view is flagged", func(t *testing.T) {
		got := runFreshnessCAGGDetector(t, ctx, db)
		for _, view := range []string{
			"prices_1m", "prices_15m", "prices_1h", "prices_4h",
			"prices_1d", "prices_1w", "prices_1mo", "twap_1h", "twap_1d",
		} {
			v, ok := got[view]
			if !ok {
				t.Errorf("no history-missing sample for %q — the watchdog does not judge this view, so an emptied %s is invisible", view, view)
				continue
			}
			if v != "1" {
				t.Errorf("history-missing{%s} = %s, want 1 — the view holds none of the 10 days of trades the lake holds", view, v)
			}
		}
	})

	t.Run("a view whose armed retention explains its floor is not flagged", func(t *testing.T) {
		// An ARMED retention policy makes a short history correct, and
		// the detector has to know the difference between "trimmed by
		// policy" and "never re-materialized". Migration 0156 ships one
		// of these disabled on prices_1m; an operator may arm it.
		if _, err := db.ExecContext(ctx,
			`SELECT add_retention_policy('prices_15m', drop_after => INTERVAL '2 days', if_not_exists => true)`,
		); err != nil {
			t.Fatalf("add retention policy: %v", err)
		}
		// Armed, but parked a day out so the job cannot race the test.
		if _, err := db.ExecContext(ctx, `
			SELECT alter_job(job_id, scheduled => true, next_start => now() + INTERVAL '1 day')
			  FROM timescaledb_information.jobs
			 WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_15m'`,
		); err != nil {
			t.Fatalf("arm retention policy: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`CALL refresh_continuous_aggregate('prices_15m', $1::timestamptz, $2::timestamptz)`,
			now.Add(-48*time.Hour), now,
		); err != nil {
			t.Fatalf("refresh prices_15m over its retained window: %v", err)
		}

		got := runFreshnessCAGGDetector(t, ctx, db)
		if v, ok := got["prices_15m"]; !ok || v != "0" {
			t.Errorf("history-missing{prices_15m} = %q (present=%v), want 0 — the view holds everything its armed 2-day retention lets it hold", v, ok)
		}
		if v, ok := got["prices_1h"]; !ok || v != "1" {
			t.Errorf("history-missing{prices_1h} = %q (present=%v), want 1 — no retention explains ITS empty state", v, ok)
		}
	})

	t.Run("a fully materialized view reads healthy", func(t *testing.T) {
		for _, view := range []string{
			"prices_1m", "prices_1h", "prices_4h",
			"prices_1d", "prices_1w", "prices_1mo", "twap_1h", "twap_1d",
		} {
			if _, err := db.ExecContext(ctx,
				fmt.Sprintf(`CALL refresh_continuous_aggregate('%s', NULL, NULL)`, view),
			); err != nil {
				t.Fatalf("refresh %s: %v", view, err)
			}
		}
		got := runFreshnessCAGGDetector(t, ctx, db)
		for view, v := range got {
			if view == "prices_15m" {
				continue // retention-trimmed on purpose by the sub-test above
			}
			if v != "0" {
				t.Errorf("history-missing{%s} = %s, want 0 — every view now holds the whole lake", view, v)
			}
		}
	})
}

// runFreshnessCAGGDetector executes every emptied-CAGG detector block
// in the shipped watchdog and returns view → value. Reading the SQL out
// of the script rather than restating it here is the point: a detector
// that is correct in a test file and absent from the script protects
// nobody.
func runFreshnessCAGGDetector(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, stmt := range freshnessSQLBlocks(t, "_history_missing") {
		rows, err := db.QueryContext(ctx, stmt)
		if err != nil {
			t.Fatalf("execute watchdog block: %v\n--- SQL ---\n%s", err, stmt)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan exposition line: %v", err)
			}
			view, value := parseExpositionSample(t, line)
			out[view] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("watchdog block rows: %v", err)
		}
		_ = rows.Close()
	}
	return out
}

// freshnessSQLBlocks returns the psql heredocs of the shipped
// data-freshness watchdog whose body mentions `needle`.
func freshnessSQLBlocks(t *testing.T, needle string) []string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"configs", "ansible", "roles", "archival-node", "files", "data-freshness.sh")
	b, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read watchdog: %v", err)
	}
	var blocks []string
	var cur []string
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case !inBlock && strings.HasSuffix(line, "<<'SQL'"):
			inBlock = true
			cur = nil
		case inBlock && line == "SQL":
			inBlock = false
			block := strings.Join(cur, "\n")
			if strings.Contains(block, needle) {
				blocks = append(blocks, block)
			}
		case inBlock:
			cur = append(cur, line)
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("no SQL block in data-freshness.sh emits %q — nothing detects an emptied continuous aggregate", needle)
	}
	return blocks
}

// parseExpositionSample splits `name{view="x"} 1` into ("x", "1").
func parseExpositionSample(t *testing.T, line string) (string, string) {
	t.Helper()
	open := strings.Index(line, `{view="`)
	closeIdx := strings.Index(line, `"}`)
	space := strings.LastIndex(line, " ")
	if open < 0 || closeIdx < 0 || space < 0 || space < closeIdx {
		t.Fatalf("un-parseable exposition line %q", line)
	}
	return line[open+len(`{view="`) : closeIdx], strings.TrimSpace(line[space+1:])
}

func seedDetectorTrade(t *testing.T, ctx context.Context, db *sql.DB, nonce int, ts time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ('sdex', $1, $2, 0, $3, 'native', 'fiat:USD', 1::numeric, 1::numeric, 1::numeric)`,
		80_000_000+nonce, fmt.Sprintf("%064x", 800_000+nonce), ts,
	); err != nil {
		t.Fatalf("seed detector trade %d: %v", nonce, err)
	}
}
