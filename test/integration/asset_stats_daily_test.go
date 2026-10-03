//go:build integration

package integration_test

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestAssetStatsDaily_ExecutesAgainstServer runs the real ch-holders-rollup
// cycle twice against a real ClickHouse server and reads the day's snapshot
// back. It proves the trustline count includes zero-balance lines while
// holders do not, the balance sums stay exact past 2^63, the Gini matches a
// hand-computed value, and a second cycle on the same day replaces the day's
// rows rather than adding to them.
func TestAssetStatsDaily_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
			"asset_stats_daily", "asset_stats_daily_staging",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const spreadAsset, wideAsset = "A3AS-GISSUERA3A", "A3AW-GISSUERA3A"
	seed := func(entryType, asset, holder string, balance int64) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: 73_000_001, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "a3a", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "a3a-" + entryType + "-" + holder + asset, EntryXDR: "a3a", AccountID: holder,
			Asset: asset, Balance: balance,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s %s): %v", entryType, asset, holder, err)
		}
	}
	// The cycle's accounts_stats arm needs at least one funded account.
	seed("account", "", "GHOLDERA3A1", 42)
	seed("trustline", spreadAsset, "GHOLDERA3A1", 10)
	seed("trustline", spreadAsset, "GHOLDERA3A2", 20)
	seed("trustline", spreadAsset, "GHOLDERA3A3", 30)
	seed("trustline", spreadAsset, "GHOLDERA3A4", 0)
	seed("trustline", wideAsset, "GHOLDERA3A1", math.MaxInt64)
	seed("trustline", wideAsset, "GHOLDERA3A2", math.MaxInt64)

	for range 2 {
		if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
			t.Fatalf("RunHoldersRollup: %v", err)
		}
	}

	type snapshot struct {
		day                  time.Time
		holders, trustlines  int64
		total, top10, top100 string
		gini                 *float64
	}
	read := func(asset string) snapshot {
		t.Helper()
		rows, err := conn.Query(ctx, `
			SELECT day, holders, trustlines, toString(balance_total), toString(top10_balance),
			       toString(top100_balance), gini
			FROM stellar.asset_stats_daily WHERE asset = ?`, asset)
		if err != nil {
			t.Fatalf("read snapshot %s: %v", asset, err)
		}
		defer func() { _ = rows.Close() }()
		var out []snapshot
		for rows.Next() {
			var s snapshot
			if err := rows.Scan(&s.day, &s.holders, &s.trustlines, &s.total, &s.top10, &s.top100, &s.gini); err != nil {
				t.Fatalf("scan snapshot %s: %v", asset, err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows %s: %v", asset, err)
		}
		if len(out) != 1 {
			t.Fatalf("%s has %d snapshot row(s) after two same-day cycles, want exactly 1", asset, len(out))
		}
		return out[0]
	}

	today := time.Now().UTC().Format("2006-01-02")
	spread := read(spreadAsset)
	if got := spread.day.Format("2006-01-02"); got != today {
		t.Errorf("snapshot day = %s, want the cycle's UTC day %s", got, today)
	}
	if spread.holders != 3 || spread.trustlines != 4 {
		t.Errorf("holders/trustlines = %d/%d, want 3/4 (the zero-balance line counts as a trustline only)", spread.holders, spread.trustlines)
	}
	if spread.total != "60" || spread.top10 != "60" || spread.top100 != "60" {
		t.Errorf("total/top10/top100 = %s/%s/%s, want 60/60/60", spread.total, spread.top10, spread.top100)
	}
	// Mean absolute difference over 10, 20, 30: 80 / (2 · 3² · 20) = 2/9.
	if spread.gini == nil || math.Abs(*spread.gini-2.0/9.0) > 1e-12 {
		t.Errorf("gini = %s, want 2/9", giniString(spread.gini))
	}

	wide := read(wideAsset)
	if wide.total != "18446744073709551614" || wide.top10 != "18446744073709551614" {
		t.Errorf("total/top10 = %s/%s, want 18446744073709551614 (2·(2^63−1), exact past Int64)", wide.total, wide.top10)
	}
	if wide.gini == nil || *wide.gini != 0 {
		t.Errorf("gini of two equal holders = %s, want 0", giniString(wide.gini))
	}

	native := read("native")
	if native.holders < 1 || native.trustlines < native.holders {
		t.Errorf("native holders/trustlines = %d/%d, want ≥1 and trustlines ≥ holders", native.holders, native.trustlines)
	}
}

func giniString(g *float64) string {
	if g == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*g, 'g', -1, 64)
}
