//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestDeriveGenerationGuard_CorrectiveReDerive is the proven-red test for
// the INV-3 re-derive trap (audit-2026-07-16 M1 / migration 0109). Before
// the fix the served-tier writers used `ON CONFLICT (...) DO NOTHING` with
// the derived value OUTSIDE the conflict key, so a corrected re-derive of
// a wrong money value silently no-op'd — the ONLY way to fix it was a
// destructive DELETE + full re-backfill.
//
// The fix is a generation-guarded idempotent-corrective upsert. This test
// exercises the real seam ([Store.SetDeriveGeneration] + the writers) and
// asserts, for trades (single + batch) and supply:
//
//   - a re-derive at a HIGHER generation (N>0) UPDATEs the wrong value in
//     place — the correction lands. This assertion FAILS on the unfixed
//     DO-NOTHING writers (they keep the original V1).
//   - a subsequent LOWER-generation write (a live gen-0 replay carrying a
//     different value) can NEVER revert the correction — the guard
//     (`derive_generation <= EXCLUDED.derive_generation`) preserves it.
//
// To reproduce the red state: revert only the writers' SQL to
// `ON CONFLICT ... DO NOTHING` (keep migration 0109 + SetDeriveGeneration)
// and the "correction lands" assertions go red.
func TestDeriveGenerationGuard_CorrectiveReDerive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := c.NewPair(xlm, usd)

	// One fixed instant so every (source, ledger, tx_hash, op_index, ts)
	// across V1/V2/V3 is the SAME trades PK — a re-derive of one row, not
	// a new row.
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// The wrong original value (V1), the corrected value (V2), and a
	// different value carried by a stale live replay (V3). Binance +
	// fiat:USD auto-populates usd_volume = quote_amount / 1e8, so the
	// derived money column tracks the quote correction too.
	const (
		v1 = 12_000_000 // wrong original       → usd_volume 0.12
		v2 = 99_000_000 // corrected re-derive   → usd_volume 0.99
		v3 = 55_000_000 // stale live gen-0 value → must be ignored
	)

	readTrade := func(source string, ledger uint32) (quote string, usdVol sql.NullString) {
		const q = `SELECT quote_amount::text, usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
		if err := store.DB().QueryRowContext(ctx, q, source, ledger).Scan(&quote, &usdVol); err != nil {
			t.Fatalf("read trade (%s, %d): %v", source, ledger, err)
		}
		return quote, usdVol
	}
	wantUSD := func(t *testing.T, uv sql.NullString) {
		t.Helper()
		if !uv.Valid || (uv.String != "0.99000000" && uv.String != "0.99") {
			t.Errorf("usd_volume = %v, want 0.99 (the corrected money value)", uv)
		}
	}

	t.Run("InsertTrade", func(t *testing.T) {
		// gen 1 — the original (wrong) value lands.
		store.SetDeriveGeneration(1)
		tr := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v1)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade V1: %v", err)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in
		// place. Unfixed DO-NOTHING writers keep V1 → this goes red.
		store.SetDeriveGeneration(2)
		trV2 := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v2)
		if err := store.InsertTrade(ctx, trV2); err != nil {
			t.Fatalf("InsertTrade V2: %v", err)
		}
		if q, uv := readTrade("binance", trV2.Ledger); q != "99000000" {
			t.Errorf("after gen-2 corrective re-derive: quote_amount = %s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", q)
		} else {
			wantUSD(t, uv)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not
		// revert the gen-2 correction.
		store.SetDeriveGeneration(0)
		trV3 := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v3)
		if err := store.InsertTrade(ctx, trV3); err != nil {
			t.Fatalf("InsertTrade V3 (gen 0 replay): %v", err)
		}
		if q, uv := readTrade("binance", trV3.Ledger); q != "99000000" {
			t.Errorf("after gen-0 replay: quote_amount = %s, want 99000000 "+
				"(the generation guard must preserve the correction)", q)
		} else {
			wantUSD(t, uv)
		}
	})

	t.Run("BatchInsertTrades", func(t *testing.T) {
		store.SetDeriveGeneration(1)
		b1 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v1)
		if err := store.BatchInsertTrades(ctx, []c.Trade{b1}); err != nil {
			t.Fatalf("BatchInsertTrades V1: %v", err)
		}

		store.SetDeriveGeneration(2)
		b2 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v2)
		if err := store.BatchInsertTrades(ctx, []c.Trade{b2}); err != nil {
			t.Fatalf("BatchInsertTrades V2: %v", err)
		}
		if q, uv := readTrade("binance", b2.Ledger); q != "99000000" {
			t.Errorf("after gen-2 batch corrective re-derive: quote_amount = %s, want 99000000", q)
		} else {
			wantUSD(t, uv)
		}

		store.SetDeriveGeneration(0)
		b3 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v3)
		if err := store.BatchInsertTrades(ctx, []c.Trade{b3}); err != nil {
			t.Fatalf("BatchInsertTrades V3 (gen 0 replay): %v", err)
		}
		if q, _ := readTrade("binance", b3.Ledger); q != "99000000" {
			t.Errorf("after gen-0 batch replay: quote_amount = %s, want 99000000 "+
				"(guard must preserve the correction)", q)
		}

		// The DO UPDATE upsert rejects an intra-statement duplicate
		// conflict key ("cannot affect row a second time"), which the old
		// DO NOTHING tolerated. A batch carrying the SAME PK twice (CEX WS
		// redelivery) must be deduped in-writer, not error — and the last
		// copy must win.
		store.SetDeriveGeneration(3)
		dupA := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, 77_000_000)
		dupB := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, 88_000_000)
		if err := store.BatchInsertTrades(ctx, []c.Trade{dupA, dupB}); err != nil {
			t.Fatalf("batch with an intra-batch duplicate PK must dedupe, not error: %v", err)
		}
		if q, _ := readTrade("binance", dupB.Ledger); q != "88000000" {
			t.Errorf("intra-batch duplicate PK: quote_amount = %s, want 88000000 (last copy wins)", q)
		}
	})

	t.Run("InsertSupply", func(t *testing.T) {
		const assetKey = "XLM"
		const ledgerSeq = 60_000_000
		obs := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		mkSupply := func(total, circ int64) supply.Supply {
			return supply.Supply{
				AssetKey:          assetKey,
				TotalSupply:       big.NewInt(total),
				CirculatingSupply: big.NewInt(circ),
				MaxSupply:         big.NewInt(total),
				Basis:             supply.BasisXLMSDFReserveExclusion,
				LedgerSequence:    ledgerSeq,
				ObservedAt:        obs,
			}
		}
		readSupply := func() (total, circ string) {
			const q = `SELECT total_supply::text, circulating_supply::text
			             FROM asset_supply_history WHERE asset_key = $1 AND ledger_sequence = $2`
			if err := store.DB().QueryRowContext(ctx, q, assetKey, int64(ledgerSeq)).Scan(&total, &circ); err != nil {
				t.Fatalf("read supply: %v", err)
			}
			return total, circ
		}

		// gen 1 — original (wrong) supply.
		store.SetDeriveGeneration(1)
		if err := store.InsertSupply(ctx, mkSupply(1000, 900)); err != nil {
			t.Fatalf("InsertSupply V1: %v", err)
		}

		// gen 2 — corrected re-derive of the SAME (asset, ledger, time)
		// must UPDATE both value columns. Unfixed DO NOTHING keeps V1.
		store.SetDeriveGeneration(2)
		if err := store.InsertSupply(ctx, mkSupply(2000, 1800)); err != nil {
			t.Fatalf("InsertSupply V2: %v", err)
		}
		if total, circ := readSupply(); total != "2000" || circ != "1800" {
			t.Errorf("after gen-2 corrective re-derive: total=%s circulating=%s, want 2000/1800 "+
				"(INV-3: the old DO NOTHING keeps 1000/900)", total, circ)
		}

		// gen 0 — stale live replay with different values must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertSupply(ctx, mkSupply(3000, 2700)); err != nil {
			t.Fatalf("InsertSupply V3 (gen 0 replay): %v", err)
		}
		if total, circ := readSupply(); total != "2000" || circ != "1800" {
			t.Errorf("after gen-0 replay: total=%s circulating=%s, want 2000/1800 "+
				"(guard must preserve the correction)", total, circ)
		}
	})
}
