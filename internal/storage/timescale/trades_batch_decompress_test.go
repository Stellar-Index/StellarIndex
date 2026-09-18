package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

// The batch upsert is the fallback writer for ranges whose trades chunks are
// compressed; a compressed-chunk upsert decompresses whole segments per
// conflict and trips the server's per-transaction cap (SQLSTATE 53400), which
// dropped a 40k-ledger SDEX re-derive to one INSERT per row on 2026-09-18.
// The statement must therefore run inside ONE transaction that lifts the cap
// with SET LOCAL first — scoped to that transaction, never the pooled
// connection — and commit once.
func TestScanBatchTradeOutcome_LiftsTheDecompressionCapInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	returning := scriptedResult{
		cols: []string{"source", "ledger", "ts", "base_asset", "quote_asset", "unit_ratio"},
		rows: [][]driver.Value{
			{"sdex", int64(61249957), ts, "native", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false},
			{"sdex", int64(61249958), ts, "native", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", true},
		},
	}
	store, conn := newScriptedStore(t, scriptedResult{}, returning)

	perSourceNew, perSourceUnitRatio, seen, err := store.scanBatchTradeOutcome(
		context.Background(), "WITH ins AS (INSERT INTO trades ...) SELECT ...", nil)
	if err != nil {
		t.Fatalf("scanBatchTradeOutcome: %v", err)
	}
	got := conn.statements()
	if len(got) != 2 {
		t.Fatalf("issued %d statements, want 2 (SET LOCAL, then the batch):\n%s", len(got), strings.Join(got, "\n"))
	}
	if got[0] != batchTradeDecompressionCapSQL || !strings.Contains(got[0], "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0") {
		t.Errorf("statement 0 = %q, want the lifted decompression cap as a SET LOCAL", got[0])
	}
	if !strings.Contains(got[1], "INSERT INTO trades") {
		t.Errorf("statement 1 = %q, want the batch upsert", got[1])
	}
	if conn.commits != 1 {
		t.Errorf("commits = %d, want exactly 1 — the cap and the upsert share one transaction", conn.commits)
	}
	if perSourceNew["sdex"] != 2 || perSourceUnitRatio["sdex"] != 1 {
		t.Errorf("tallies = new %v unit-ratio %v, want sdex: 2 new, 1 unit-ratio", perSourceNew, perSourceUnitRatio)
	}
	if obs, ok := seen["native"]; !ok || obs.ledger != 61249958 {
		t.Errorf("seen[native] = %+v, want the highest-ledger observation 61249958", obs)
	}
}
