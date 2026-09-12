package timescale

import (
	"database/sql"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The two pure pieces of the bulk backfill writer are the ones a DB-backed
// test would only exercise incidentally: the bounding box the emptiness proof
// is built from, and the partition split that makes parallel COPY safe. Both
// are load-bearing for correctness, not just for speed — a bounding box that
// misses a row's PK space would let a COPY run into a conflict it was told
// could not exist, and a partition split that overlapped would let two writers
// present the same key.

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

// TestTradeBulkColumns_MatchTheRowWriters pins the COPY column list against
// the INSERT statements. If someone adds a column to InsertTrade /
// BatchInsertTrades and not here, the bulk path silently writes a DEFAULT for
// it — which for a derived column is a wrong value, not a missing one.
func TestTradeBulkColumns_MatchTheRowWriters(t *testing.T) {
	want := []string{
		"source", "ledger", "tx_hash", "op_index", "ts",
		"base_asset", "quote_asset",
		"base_amount", "quote_amount", "usd_volume",
		"maker", "taker", "derive_generation",
	}
	if len(tradeBulkColumns) != len(want) {
		t.Fatalf("tradeBulkColumns has %d columns, want %d", len(tradeBulkColumns), len(want))
	}
	for i := range want {
		if tradeBulkColumns[i] != want[i] {
			t.Fatalf("tradeBulkColumns[%d] = %q, want %q", i, tradeBulkColumns[i], want[i])
		}
	}
	// The count is also the batch writer's bind-parameter width; if that
	// constant moves, one of the two paths has grown a column the other
	// has not.
	const batchColsPerRow = 13
	if len(tradeBulkColumns) != batchColsPerRow {
		t.Fatalf("tradeBulkColumns=%d but tradeBatchValues binds %d params/row",
			len(tradeBulkColumns), batchColsPerRow)
	}
}

// TestBulkTradeValues_MirrorsTheInsertBindings proves the COPY tuple carries
// the same values the INSERT statement's placeholders carry — in particular
// that an absent maker/taker becomes SQL NULL rather than a zero-length text
// value (both statements wrap those placeholders in a NULLIF against the
// empty string), that amounts go over as their decimal strings, and that the
// store's derive_generation is stamped per row rather than left to a default.
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
		[]sql.NullString{{String: "12.34", Valid: true}},
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
	row := s.bulkTradeValues([]canonical.Trade{tr}, []sql.NullString{{}})[0]
	usd, ok := row[9].(sql.NullString)
	if !ok || usd.Valid {
		t.Fatalf("usd_volume = %#v, want an invalid sql.NullString (SQL NULL) — an unpriceable "+
			"trade must store NULL, exactly as the row writers store it", row[9])
	}
}
