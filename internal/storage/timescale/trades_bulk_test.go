package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The bulk backfill writer's two pure pieces are the bounding box behind the emptiness proof and the
// partition split for parallel COPY. A box missing a row's PK space allows an unexpected conflict;
// overlapping partitions let two writers present one key.

func bulkTestTrade(source string, ledger uint32, ts time.Time) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      "00000000000000000000000000000000000000000000000000000000000000ff",
		OpIndex:     0,
		Timestamp:   ts,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2)),
	}
}

func TestBulkExtents_CoversEveryRowPerSource(t *testing.T) {
	t0 := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	trades := []canonical.Trade{
		bulkTestTrade("sdex", 500, t0.Add(20*time.Minute)),
		bulkTestTrade("sdex", 100, t0.Add(90*time.Minute)), // min ledger, max ts
		bulkTestTrade("sdex", 900, t0),                     // max ledger, min ts
		bulkTestTrade("soroswap", 42, t0.Add(time.Hour)),
	}
	got := bulkExtents(trades)
	if len(got) != 2 {
		t.Fatalf("extents for %d sources, want 2", len(got))
	}
	sdex := got["sdex"]
	if sdex.minLedger != 100 || sdex.maxLedger != 900 {
		t.Fatalf("sdex ledger extent = [%d,%d], want [100,900]", sdex.minLedger, sdex.maxLedger)
	}
	if !sdex.minTS.Equal(t0) || !sdex.maxTS.Equal(t0.Add(90*time.Minute)) {
		t.Fatalf("sdex ts extent = [%s,%s], want [%s,%s]",
			sdex.minTS, sdex.maxTS, t0, t0.Add(90*time.Minute))
	}
	// The box must CONTAIN every row of its source — that containment is the
	// whole proof: a row that could collide on the PK shares source, ledger
	// and ts with one of ours, so it lies inside this box.
	for _, tr := range trades {
		e := got[tr.Source]
		if tr.Ledger < e.minLedger || tr.Ledger > e.maxLedger {
			t.Fatalf("ledger %d escapes its source's extent [%d,%d]", tr.Ledger, e.minLedger, e.maxLedger)
		}
		ts := tr.Timestamp.UTC()
		if ts.Before(e.minTS) || ts.After(e.maxTS) {
			t.Fatalf("ts %s escapes its source's extent [%s,%s]", ts, e.minTS, e.maxTS)
		}
	}
	// A single-row source degenerates to a point box, not an empty one.
	sor := got["soroswap"]
	if sor.minLedger != 42 || sor.maxLedger != 42 || !sor.minTS.Equal(sor.maxTS) {
		t.Fatalf("single-row extent = %+v, want a point box at ledger 42", sor)
	}
}

func TestBulkExtents_NormalisesTimestampsToUTC(t *testing.T) {
	// The probe binds ::timestamptz, and the COPY writes .UTC(); a non-UTC
	// input must not produce a box in a different zone from the rows.
	loc := time.FixedZone("UTC+9", 9*3600)
	ts := time.Date(2024, 3, 5, 21, 0, 0, 0, loc)
	got := bulkExtents([]canonical.Trade{bulkTestTrade("sdex", 1, ts)})["sdex"]
	if got.minTS.Location() != time.UTC || !got.minTS.Equal(ts) {
		t.Fatalf("extent ts = %s (%v), want the same instant in UTC", got.minTS, got.minTS.Location())
	}
}

func TestBulkPartitions_AreContiguousDisjointAndComplete(t *testing.T) {
	for _, tc := range []struct{ n, writers int }{
		{0, 4},
		{1, 4},
		{999, 4},
		{2_000, 4},
		{40_000, 4},
		{40_001, 7},
		{1_000_000, 8},
		{5_000, 0}, // 0 writers = the package default
	} {
		parts := bulkPartitions(tc.n, tc.writers)
		prev := 0
		for _, p := range parts {
			if p[0] != prev {
				t.Fatalf("n=%d w=%d: partition starts at %d, previous ended at %d — a gap or an "+
					"overlap here would drop rows or let two writers present one key",
					tc.n, tc.writers, p[0], prev)
			}
			if p[1] <= p[0] {
				t.Fatalf("n=%d w=%d: empty partition %v", tc.n, tc.writers, p)
			}
			prev = p[1]
		}
		if prev != tc.n {
			t.Fatalf("n=%d w=%d: partitions cover %d rows, want %d", tc.n, tc.writers, prev, tc.n)
		}
		if tc.n > 0 && len(parts) == 0 {
			t.Fatalf("n=%d w=%d: no partition at all", tc.n, tc.writers)
		}
	}
}

func TestBulkPartitions_SmallBuffersStaySingleStream(t *testing.T) {
	// Below bulkMinPartition there is nothing to gain from a second
	// connection, and opening one per few hundred rows would cost more than
	// the parallelism returns.
	for _, n := range []int{1, 500, bulkMinPartition - 1} {
		if got := len(bulkPartitions(n, 8)); got != 1 {
			t.Fatalf("n=%d split into %d partitions, want 1", n, got)
		}
	}
	if got := len(bulkPartitions(8*bulkMinPartition, 8)); got != 8 {
		t.Fatalf("a buffer with room for 8 partitions split into %d", got)
	}
}

// TestTradeBulkColumns_MatchTheRowWriters pins the COPY column list against the INSERT statements; a column
// added to InsertTrade/BatchInsertTrades but not here makes the bulk path write a DEFAULT, wrong for derived
// columns.
func TestTradeBulkColumns_MatchTheRowWriters(t *testing.T) {
	want := []string{
		"source", "ledger", "tx_hash", "op_index", "ts",
		"base_asset", "quote_asset",
		"base_amount", "quote_amount", "usd_volume",
		"maker", "taker", "derive_generation", "usd_basis",
	}
	if len(tradeBulkColumns) != len(want) {
		t.Fatalf("tradeBulkColumns has %d columns, want %d", len(tradeBulkColumns), len(want))
	}
	for i := range want {
		if tradeBulkColumns[i] != want[i] {
			t.Fatalf("tradeBulkColumns[%d] = %q, want %q", i, tradeBulkColumns[i], want[i])
		}
	}
	// The count is also the batch writer's bind-parameter width; if they
	// differ, one of the two paths has grown a column the other has not.
	if len(tradeBulkColumns) != tradeColsPerRow {
		t.Fatalf("tradeBulkColumns=%d but tradeBatchValues binds %d params/row",
			len(tradeBulkColumns), tradeColsPerRow)
	}
}

// TestBulkTradeValues_MirrorsTheInsertBindings proves the COPY tuple carries the same values as
// the INSERT placeholders: an absent maker/taker becomes SQL NULL rather than empty text, amounts
// go over as decimal strings, and derive_generation is stamped per row rather than defaulted.
func TestBulkTradeValues_MirrorsTheInsertBindings(t *testing.T) {
	s := &Store{deriveGeneration: 4242}
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("asset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	tr := bulkTestTrade("sdex", 7, ts)
	tr.Pair = pair
	tr.BaseAmount = canonical.NewAmount(big.NewInt(1_234_567))
	tr.QuoteAmount = canonical.NewAmount(big.NewInt(89))
	tr.Maker = "" // must land as NULL, not ''
	tr.Taker = "GTAKER"

	got := s.bulkTradeValues(
		[]canonical.Trade{tr},
		[]tradeUSDValuation{{
			volume: sql.NullString{String: "12.34", Valid: true},
			basis:  sql.NullString{String: string(BasisQuotePegged), Valid: true},
		}},
	)
	if len(got) != 1 {
		t.Fatalf("bulkTradeValues returned %d tuples, want 1", len(got))
	}
	row := got[0]
	if len(row) != len(tradeBulkColumns) {
		t.Fatalf("tuple has %d values for %d columns", len(row), len(tradeBulkColumns))
	}
	want := []any{
		"sdex", int64(7), tr.TxHash, int64(0), ts,
		"native", usdc.String(),
		"1234567", "89",
		sql.NullString{String: "12.34", Valid: true},
		sql.NullString{},
		sql.NullString{String: "GTAKER", Valid: true},
		int64(4242),
		sql.NullString{String: string(BasisQuotePegged), Valid: true},
	}
	for i := range want {
		if !reflect.DeepEqual(row[i], want[i]) {
			t.Fatalf("column %q = %#v, want %#v", tradeBulkColumns[i], row[i], want[i])
		}
	}
}

func TestBulkTradeValues_NullUSDVolume(t *testing.T) {
	s := &Store{deriveGeneration: 0}
	tr := bulkTestTrade("sdex", 1, time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC))
	row := s.bulkTradeValues([]canonical.Trade{tr}, []tradeUSDValuation{{}})[0]
	usd, ok := row[9].(sql.NullString)
	if !ok || usd.Valid {
		t.Fatalf("usd_volume = %#v, want an invalid sql.NullString (SQL NULL) — an unpriceable "+
			"trade must store NULL, exactly as the row writers store it", row[9])
	}
}

// TestBulkCopyFallsBack pins which COPY errors replay the buffer through the idempotent upsert instead of
// failing the batch: a unique violation, and 53400 (TimescaleDB's tuple-decompression cap hit by the PK
// check on compressed segments).
func TestBulkCopyFallsBack(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unique violation", &pgconn.PgError{Code: "23505"}, true},
		{"decompression limit", &pgconn.PgError{Code: "53400"}, true},
		{"wrapped decompression limit", fmt.Errorf("copy: %w", &pgconn.PgError{Code: "53400"}), true},
		{"check violation", &pgconn.PgError{Code: "23514"}, false},
		{"plain error", errors.New("53400"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := bulkCopyFallsBack(tc.err); got != tc.want {
			t.Errorf("%s: bulkCopyFallsBack = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// bulkPanicTrades builds n distinct on-chain DEX trades whose quote leg has no USD peg, so tradeUSDVolume
// falls through tiers 1/2/2b to the FX resolver, the per-row call that can fail and so the real panic source.
func bulkPanicTrades(t *testing.T, n int) []canonical.Trade {
	t.Helper()
	xlm := canonical.NativeAsset()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset AQUA: %v", err)
	}
	rows := make([]canonical.Trade, 0, n)
	for i := range n {
		tr := mkClassicDEXTrade(t, "soroswap", aqua, xlm, 50_000_000_000)
		tr.Ledger = uint32(i + 1)
		rows = append(rows, tr)
	}
	return rows
}

// A panic in the bulk backfill writer's usd_volume fan-out must fail the whole call and return no volumes.
// Otherwise unreached rows keep a NULL usd_volume and a silently under-valued batch is COPYed into `trades`.
func TestResolveBulkUSDVolumes_PanickingResolverFailsTheCallNotTheProcess(t *testing.T) {
	// The literal, not the constant: this label is the alert's contract, so a
	// rename of bulkResolverWorkerName must fail here rather than silently move
	// the series the page rule reads.
	const workerName = "timescale-bulk-backfill-usd-volume-resolver"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	s := &Store{usdVolumeFXResolver: panicFXResolver{}}
	rows := bulkPanicTrades(t, 64)

	type outcome struct {
		out []tradeUSDValuation
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := s.resolveBulkUSDVolumes(context.Background(), rows, 4)
		done <- outcome{out, err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolveBulkUSDVolumes never returned: a worker that contained its panic " +
			"stopped receiving, stranding the index feeder on a send nobody will take")
	}

	if got.err == nil {
		t.Fatalf("a fan-out in which every resolver panicked returned err=nil with %d "+
			"volume slot(s): the caller would COPY those rows with NULL usd_volume and "+
			"tally them as landed", len(got.out))
	}
	if !strings.Contains(got.err.Error(), "panicked") {
		t.Errorf("err = %v, want the contained panic surfaced as the call's failure", got.err)
	}
	if got.out != nil {
		t.Errorf("out is non-nil (%d slots) — a partially-resolved volume set must never "+
			"reach the writer", len(got.out))
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after <= before {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} did not move (%v → %v) — a "+
			"recover that does not move the counter turns a loud crash into a silent "+
			"under-valued backfill", workerName, before, after)
	}
}

// The COPY fan-out's release is errs[i]: copyTradePartitions treats errs[i] == nil as committed, so a
// contained panic must become that partition's error, or source_entry_counts is credited for rows never
// written.
func TestCopyTradePartitions_PanickingPartitionIsNotReportedAsLanded(t *testing.T) {
	const workerName = "timescale-bulk-backfill-copy-partition" // literal: see above
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	s := &Store{} // no *sql.DB — copyTradeRange's s.db.Conn dereferences nil
	rows := bulkPanicTrades(t, 16)
	usd := make([]tradeUSDValuation, len(rows))

	landed, copied, err := s.copyTradePartitions(context.Background(), rows, usd, BulkBackfillOptions{})

	if err == nil {
		t.Fatalf("a partition that panicked returned err=nil (landed=%v copied=%d)", landed, copied)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want the contained panic surfaced as that partition's failure", err)
	}
	if len(landed) != 0 {
		t.Errorf("landed = %v, want empty — a panicked partition copied nothing, so "+
			"crediting source_entry_counts for its rows would corrupt the tally", landed)
	}
	if copied != 0 {
		t.Errorf("copied = %d, want 0", copied)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after <= before {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} did not move (%v → %v)",
			workerName, before, after)
	}
}

// TestResolveBulkUSDVolumes_HealthyFanOutUnchanged is the companion to the destructive case: the guard must
// not change an ordinary fan-out's result, with exact expected volumes on every row, not merely "no error".
func TestResolveBulkUSDVolumes_HealthyFanOutUnchanged(t *testing.T) {
	s := &Store{usdVolumeFXResolver: stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.10",
	}}}

	rows := bulkPanicTrades(t, 32)
	out, err := s.resolveBulkUSDVolumes(context.Background(), rows, 4)
	if err != nil {
		t.Fatalf("resolveBulkUSDVolumes: %v", err)
	}
	if len(out) != len(rows) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(rows))
	}
	// Quote leg is XLM at $0.10: 50_000_000_000 stroops / 1e7 = 5_000 XLM = $500.
	for i := range out {
		if !out[i].volume.Valid || out[i].volume.String != "500.00000000" || out[i].basis.String != string(BasisXLMQuote) {
			t.Fatalf("out[%d] = %+v, want 500.00000000 on %s", i, out[i], BasisXLMQuote)
		}
	}
}

const subBatchUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// subBatchTrades returns n storable native/USDC trades on source, one per
// ledger starting at 1, so conflict-key order is ledger order and rows
// [0, tradeInsertMaxRows) form the first sub-batch.
func subBatchTrades(t *testing.T, source string, n int) []canonical.Trade {
	t.Helper()
	pair := osztPair(t)
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	out := make([]canonical.Trade, n)
	for i := range out {
		out[i] = canonical.Trade{
			Source: source, Ledger: uint32(i + 1), TxHash: fmt.Sprintf("%064x", i+1), //nolint:gosec // G115: small test index
			Timestamp: ts, Pair: pair, BaseAmount: osztAmt(100), QuoteAmount: osztAmt(25),
		}
	}
	return out
}

// landedRows is a sub-batch's RETURNING set: one landed native/USDC row per
// ledger on source.
func landedRows(source string, ledgers ...int64) scriptedResult {
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	res := scriptedResult{cols: []string{"source", "ledger", "ts", "base_asset", "quote_asset", "unit_ratio"}}
	for _, l := range ledgers {
		res.rows = append(res.rows, []driver.Value{source, l, ts, "native", subBatchUSDC, false})
	}
	return res
}

// registeredUSDCLedger returns the ledger the classic_assets upsert bound
// for USDC, failing when the registry hook never wrote it.
func registeredUSDCLedger(t *testing.T, conn *scriptedConn) int64 {
	t.Helper()
	for _, st := range conn.stmts {
		if strings.Contains(st.sql, "INSERT INTO classic_assets") {
			l, ok := st.arg(t, 7).(int)
			if !ok {
				t.Fatalf("classic_assets $7 is %T, want int", st.arg(t, 7))
			}
			return int64(l)
		}
	}
	t.Fatalf("no classic_assets upsert issued; statements:\n%s", strings.Join(conn.statements(), "\n---\n"))
	return 0
}

// A failed later sub-batch must not erase the outcome of the sub-batches
// that already committed: a replay sees those rows as updates (xmax <> 0)
// and can never count or register them, so this call is the only chance.
func TestBatchInsertTrades_FailedSubBatchKeepsCommittedOutcome(t *testing.T) {
	t.Parallel()
	const source = "subbatch_partial"
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	store, conn := newScriptedStore(t,
		landedRows(source, 7, 4999),        // sub-batch 1: 2 landed rows
		scriptedResult{err: deadlock},      // sub-batch 2: the fault
		scriptedResult{}, scriptedResult{}, // registry: issuer, classic_assets
	)
	newBefore := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new"))
	dupBefore := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "duplicate"))

	err := store.BatchInsertTrades(context.Background(), subBatchTrades(t, source, tradeInsertMaxRows+1000))

	var sbErr *TradeSubBatchError
	switch {
	case !errors.As(err, &sbErr):
		t.Errorf("err = %v, want *TradeSubBatchError", err)
	case sbErr.Start != tradeInsertMaxRows || sbErr.End != tradeInsertMaxRows+1000 || sbErr.Total != tradeInsertMaxRows+1000:
		t.Errorf("failed range = [%d,%d) of %d, want [%d,%d) of %d",
			sbErr.Start, sbErr.End, sbErr.Total, tradeInsertMaxRows, tradeInsertMaxRows+1000, tradeInsertMaxRows+1000)
	case sbErr.FirstLedger != tradeInsertMaxRows+1 || sbErr.LastLedger != tradeInsertMaxRows+1000:
		t.Errorf("failed ledgers = %d-%d, want %d-%d", sbErr.FirstLedger, sbErr.LastLedger, tradeInsertMaxRows+1, tradeInsertMaxRows+1000)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		t.Errorf("cause not reachable via errors.As: %v", err)
	}
	if got := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new")) - newBefore; got != 2 {
		t.Errorf("outcome=new delta = %v, want 2 (the committed sub-batch's landed rows)", got)
	}
	if got := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "duplicate")) - dupBefore; got != tradeInsertMaxRows-2 {
		t.Errorf("outcome=duplicate delta = %v, want %d (committed sub-batch sent minus landed)", got, tradeInsertMaxRows-2)
	}
	if got := registeredUSDCLedger(t, conn); got != 4999 {
		t.Errorf("registered USDC at ledger %d, want 4999 from the committed sub-batch", got)
	}
	if conn.commits != 1 {
		t.Errorf("commits = %d, want 1 (only the first sub-batch)", conn.commits)
	}
}

// An asset landing in several sub-batches is registered at its HIGHEST
// ledger, the same rule the per-row fold applies within one sub-batch.
func TestBatchInsertTrades_RegistryKeepsHighestLedgerAcrossSubBatches(t *testing.T) {
	t.Parallel()
	const source = "subbatch_merge"
	store, conn := newScriptedStore(t,
		landedRows(source, 10),
		landedRows(source, 5500),
		scriptedResult{}, scriptedResult{},
	)
	if err := store.BatchInsertTrades(context.Background(), subBatchTrades(t, source, tradeInsertMaxRows+1000)); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}
	if got := registeredUSDCLedger(t, conn); got != 5500 {
		t.Errorf("registered USDC at ledger %d, want 5500 (highest across sub-batches)", got)
	}
}

// Callers replay a failed batch in their own order (the external retry ring
// trims its oldest rows by position), so the insert must not reorder it.
func TestBatchInsertTrades_LeavesCallerOrderOnFailure(t *testing.T) {
	t.Parallel()
	const source = "subbatch_caller_order"
	store, _ := newScriptedStore(t,
		scriptedResult{err: &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}},
	)
	trades := subBatchTrades(t, source, 5)
	slices.Reverse(trades)
	want := make([]uint32, len(trades))
	for i := range trades {
		want[i] = trades[i].Ledger
	}

	if err := store.BatchInsertTrades(context.Background(), trades); err == nil {
		t.Fatal("BatchInsertTrades: want the scripted deadlock, got nil")
	}
	for i := range trades {
		if trades[i].Ledger != want[i] {
			t.Fatalf("caller slice reordered: ledger[%d] = %d, want %d", i, trades[i].Ledger, want[i])
		}
	}
}

func batchOutcomeReturning() scriptedResult {
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	return scriptedResult{
		cols: []string{"source", "ledger", "ts", "base_asset", "quote_asset", "unit_ratio"},
		rows: [][]driver.Value{
			{"sdex", int64(61249957), ts, "native", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false},
			{"sdex", int64(61249958), ts, "native", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", true},
		},
	}
}

// A re-derive's batch upsert into compressed chunks decompresses whole segments per conflict and trips the
// per-transaction cap (SQLSTATE 53400). The statement must run in one transaction that lifts the cap with
// SET LOCAL first, scoped to that transaction and never the pooled connection, and commit once.
func TestScanBatchTradeOutcome_LiftsTheDecompressionCapInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, scriptedResult{}, batchOutcomeReturning())
	store.SetDeriveGeneration(1)

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
	if obs, ok := seen["native"]; !ok || obs.maxLedger != 61249958 {
		t.Errorf("seen[native] = %+v, want the highest-ledger observation 61249958", obs)
	}
}

// Live ingest (generation 0) runs up to eight persist workers concurrently;
// it must keep the server's decompression cap so a runaway decompression
// fails as a retryable 53400 rather than proceeding unbounded.
func TestScanBatchTradeOutcome_LiveIngestKeepsTheServerDecompressionCap(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, batchOutcomeReturning())

	perSourceNew, _, _, err := store.scanBatchTradeOutcome(
		context.Background(), "WITH ins AS (INSERT INTO trades ...) SELECT ...", nil)
	if err != nil {
		t.Fatalf("scanBatchTradeOutcome: %v", err)
	}
	got := conn.statements()
	for _, stmt := range got {
		if strings.Contains(stmt, "max_tuples_decompressed_per_dml_transaction") {
			t.Errorf("live ingest issued %q, want the server's decompression cap left in force", stmt)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "INSERT INTO trades") {
		t.Errorf("statements = %q, want only the batch upsert", got)
	}
	if conn.commits != 1 {
		t.Errorf("commits = %d, want exactly 1", conn.commits)
	}
	if perSourceNew["sdex"] != 2 {
		t.Errorf("perSourceNew = %v, want sdex: 2", perSourceNew)
	}
}
