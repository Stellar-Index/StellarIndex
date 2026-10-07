//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSweepOracleRederive pins the sweep's invariant on compressed chunks:
// it deletes an older-generation row only when a row at the run's own
// generation shares its identity with another ts, so it never deletes the
// only row of an identity nor a row whose identity has no run-generation
// twin. The decrement keeps source_entry_counts equal to the row count.
func TestSweepOracleRederive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mustAsset := func(a c.Asset, err error) c.Asset {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	xlm, btc, eth := mustAsset(c.NewCryptoAsset("XLM")), mustAsset(c.NewCryptoAsset("BTC")), mustAsset(c.NewCryptoAsset("ETH"))
	usd := mustAsset(c.NewFiatAsset("USD"))
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const gen = int64(100)

	row := func(source string, ledger uint32, tx string, op uint32, ts time.Time, asset c.Asset) c.OracleUpdate {
		return c.OracleUpdate{
			Source: source, Ledger: ledger, TxHash: strings.Repeat(tx, 32), OpIndex: op,
			Timestamp: ts, Asset: asset, Quote: usd,
			Price: c.NewAmount(big.NewInt(12_345)), Decimals: 7,
		}
	}
	insert := func(g int64, rows ...c.OracleUpdate) {
		t.Helper()
		store.SetDeriveGeneration(g)
		for _, u := range rows {
			if err := store.InsertOracleUpdate(ctx, u); err != nil {
				t.Fatalf("insert %+v: %v", u, err)
			}
		}
	}

	// Live generation-0 rows, compressed as they would be on r1.
	insert(0,
		row("reflector-dex", 1000, "a1", 5, t0, xlm),         // stale: gen-G twin below
		row("reflector-dex", 1001, "b1", 6, t0, xlm),         // no twin at all
		row("reflector-dex", 1002, "c1", 7, t0, xlm),         // op_index shifted at gen G
		row("reflector-dex", 5000, "d1", 1, t0, xlm),         // twin, but outside the range
		row("reflector-dex", 1003, "e1", 8, t0, xlm),         // twin only at gen 50
		row("band", 2000, "f1", 0, t0.Add(2*time.Hour), btc), // stale, future-dated
		// A nested relay sharing op_index 0. Its ts differs: an equal ts would
		// collide on the primary key.
		row("band", 2000, "f1", 0, t0.Add(3*time.Hour), eth),
	)
	var compressed int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT compress_chunk(ch) FROM show_chunks('oracle_updates') ch) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no oracle_updates chunk compressed; the compressed-DML path would go untested")
	}
	insert(50, row("reflector-dex", 1003, "e1", 8, t0.Add(time.Minute), xlm))
	insert(gen,
		row("reflector-dex", 1000, "a1", 5, t0.Add(time.Minute), xlm),
		row("reflector-dex", 1002, "c1", 70, t0, xlm),
		row("reflector-dex", 5000, "d1", 1, t0.Add(time.Minute), xlm),
		row("band", 2000, "f1", 0, t0, btc),
		row("band", 2001, "a2", 0, t0, btc), // a gen-G multi-update tx
		row("band", 2001, "a2", 0, t0.Add(30*time.Second), eth),
	)

	type key struct {
		ledger, op int
		ts         time.Time
		asset      string
	}
	rows := func(source string) []key {
		t.Helper()
		r, err := store.DB().QueryContext(ctx,
			`SELECT ledger, op_index, ts, asset FROM oracle_updates WHERE source = $1`, source)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var out []key
		for r.Next() {
			var k key
			if err := r.Scan(&k.ledger, &k.op, &k.ts, &k.asset); err != nil {
				t.Fatal(err)
			}
			k.ts = k.ts.UTC()
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.ledger != b.ledger {
				return a.ledger < b.ledger
			}
			if a.op != b.op {
				return a.op < b.op
			}
			if !a.ts.Equal(b.ts) {
				return a.ts.Before(b.ts)
			}
			return a.asset < b.asset
		})
		return out
	}
	tally := func(source string) int64 {
		t.Helper()
		var n int64
		if err := store.DB().QueryRowContext(ctx,
			`SELECT entry_count FROM source_entry_counts WHERE source = $1`, source).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sweep := func(sw timescale.OracleRederiveSweep) timescale.OracleRederiveSweepResult {
		t.Helper()
		res, err := store.SweepOracleRederive(ctx, sw)
		if err != nil {
			t.Fatalf("sweep %+v: %v", sw, err)
		}
		return res
	}

	reflBefore, bandBefore := rows("reflector-dex"), rows("band")
	if len(reflBefore) != 9 || len(bandBefore) != 5 {
		t.Fatalf("seed: %d reflector-dex / %d band rows, want 9 / 5", len(reflBefore), len(bandBefore))
	}

	t.Run("dry run deletes nothing", func(t *testing.T) {
		res := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, DryRun: true})
		if res.Deleted != 2 || res.OpIndexShifted != 1 {
			t.Errorf("dry run: would delete %d, op_index-shifted %d; want 2 (rows at 1000 and 1003, twins at any newer gen), 1", res.Deleted, res.OpIndexShifted)
		}
		if got := rows("reflector-dex"); len(got) != len(reflBefore) {
			t.Errorf("dry run deleted rows: %d left, want %d", len(got), len(reflBefore))
		}
		if _, err := store.SweepOracleRederive(ctx, timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100}); err == nil {
			t.Error("a deleting sweep without the run's generation must refuse")
		}
	})

	t.Run("deletes exactly the stale twin", func(t *testing.T) {
		res := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, Generation: gen})
		if res.Deleted != 1 || res.OpIndexShifted != 1 || res.FutureTS != 0 {
			t.Errorf("sweep = %+v, want 1 deleted, 1 op_index-shifted, 0 future-dated", res)
		}
		want := []key{
			{1000, 5, t0.Add(time.Minute), xlm.String()},
			{1001, 6, t0, xlm.String()},
			{1002, 7, t0, xlm.String()},
			{1002, 70, t0, xlm.String()},
			{1003, 8, t0, xlm.String()},
			{1003, 8, t0.Add(time.Minute), xlm.String()},
			{5000, 1, t0, xlm.String()},
			{5000, 1, t0.Add(time.Minute), xlm.String()},
		}
		assertKeys(t, rows("reflector-dex"), want)
		if got := tally("reflector-dex"); got != 8 {
			t.Errorf("source_entry_counts reflector-dex = %d, want 8 (9 inserted - 1 swept)", got)
		}
		if again := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, Generation: gen}); again.Deleted != 0 {
			t.Errorf("second sweep deleted %d, want 0", again.Deleted)
		}
	})

	t.Run("band identity includes asset", func(t *testing.T) {
		if loose := sweep(timescale.OracleRederiveSweep{Source: "band", From: 1900, To: 2100, DryRun: true}); loose.Deleted != 2 {
			t.Fatalf("precondition: a 4-column band identity would delete %d rows, want 2 (the ETH relay too)", loose.Deleted)
		}
		res := sweep(timescale.OracleRederiveSweep{Source: "band", From: 1900, To: 2100, Generation: gen, AssetScoped: true})
		if res.Deleted != 1 || res.FutureTS != 1 {
			t.Errorf("sweep = %+v, want 1 deleted, 1 future-dated", res)
		}
		want := []key{
			{2000, 0, t0, btc.String()},
			{2000, 0, t0.Add(3 * time.Hour), eth.String()},
			{2001, 0, t0, btc.String()},
			{2001, 0, t0.Add(30 * time.Second), eth.String()},
		}
		assertKeys(t, rows("band"), want)
		if got := tally("band"); got != 4 {
			t.Errorf("source_entry_counts band = %d, want 4", got)
		}
	})
}

func assertKeys[K comparable](t *testing.T, got, want []K) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}
