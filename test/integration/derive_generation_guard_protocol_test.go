//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDeriveGenerationGuardProtocol_CorrectiveReDerive is the proven-red test
// for the INV-3 re-derive trap extended to the PROTOCOL projector tables
// (audit-2026-07-16 wave 2 / migration 0110). Before the fix these writers used
// `ON CONFLICT (natural key) DO NOTHING` with the derived value (an i128 amount
// scaled by token decimals, a reserve/supply/shares figure, …) OUTSIDE the
// conflict key, so a corrected re-derive of a wrong money value silently
// no-op'd — the ONLY way to fix it was a destructive DELETE + full re-backfill.
//
// The fix is the same generation-guarded idempotent-corrective upsert 0109
// shipped for the core tables. This test exercises the real seam
// ([Store.SetDeriveGeneration] + the writers) for two REPRESENTATIVE targeted
// tables — blend_positions (single-row) and sep41_supply_events (batch) — and
// asserts:
//
//   - a re-derive at a HIGHER generation (N>0) UPDATEs the wrong value in place
//     — the correction lands. This assertion FAILS on the unfixed DO-NOTHING
//     writers (they keep the original V1).
//   - a subsequent LOWER-generation write (a live gen-0 replay carrying a
//     different value) can NEVER revert the correction — the guard
//     (`derive_generation <= EXCLUDED.derive_generation`) preserves it.
//   - the batch writer dedupes an intra-batch duplicate conflict key (last
//     wins) rather than erroring on Postgres's "cannot affect row a second
//     time" (which the old DO NOTHING absorbed).
//
// To reproduce the red state: revert one writer's SQL to
// `ON CONFLICT ... DO NOTHING` (keep migration 0110 + SetDeriveGeneration) and
// that table's "correction lands" assertion goes red.
func TestDeriveGenerationGuardProtocol_CorrectiveReDerive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The wrong original value (V1), the corrected re-derive (V2), and a
	// different value carried by a stale live gen-0 replay (V3, must be ignored).
	const (
		v1 = 12_000_000
		v2 = 99_000_000
		v3 = 55_000_000
	)

	// ── blend_positions: single-row generation-guarded upsert ──────────────
	t.Run("BlendPositions", func(t *testing.T) {
		const (
			pool   = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			asset  = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
			user   = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
			ledger = uint32(60_100_001)
		)
		ts := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
		txHash := pad64("b", 1)
		// One fixed PK (pool, ledger, tx_hash, op_index, event_kind,
		// event_index, ledger_close_time) so V1/V2/V3 re-derive ONE row.
		mk := func(tokenAmt int64) domain.BlendPositionEvent {
			return domain.BlendPositionEvent{
				Pool:        pool,
				Kind:        domain.BlendEventSupply,
				Asset:       asset,
				User:        user,
				TokenAmount: big.NewInt(tokenAmt),
				BOrDAmount:  big.NewInt(tokenAmt - 1_000),
				Ledger:      ledger,
				TxHash:      txHash,
				OpIndex:     0,
				EventIndex:  0,
				Timestamp:   ts,
			}
		}
		read := func() string {
			var got string
			const q = `SELECT token_amount::text FROM blend_positions WHERE pool = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, pool, int(ledger)).Scan(&got); err != nil {
				t.Fatalf("read blend_positions: %v", err)
			}
			return got
		}

		// gen 1 — the original (wrong) value lands.
		store.SetDeriveGeneration(1)
		if err := store.InsertBlendPositionEvent(ctx, mk(v1)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V1: %v", err)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in place.
		// The unfixed DO-NOTHING writer keeps V1 → this goes red.
		store.SetDeriveGeneration(2)
		if err := store.InsertBlendPositionEvent(ctx, mk(v2)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V2: %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-2 corrective re-derive: token_amount=%s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", got)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertBlendPositionEvent(ctx, mk(v3)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V3 (gen 0 replay): %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-0 replay: token_amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", got)
		}
	})

	// ── sep41_supply_events: BATCH generation-guarded upsert + dedup ───────
	t.Run("SEP41SupplyEventBatch", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			ledger     = uint32(70_100_001)
		)
		obs := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
		txHash := pad64("2", 7)
		// One fixed PK (contract_id, ledger, tx_hash, op_index, observed_at,
		// event_kind, event_index) so V1/V2/V3 re-derive ONE row.
		mk := func(amt int64) timescale.SEP41SupplyEvent {
			return timescale.SEP41SupplyEvent{
				ContractID: contractID,
				Ledger:     ledger,
				TxHash:     txHash,
				OpIndex:    0,
				EventIndex: 0,
				ObservedAt: obs,
				Kind:       timescale.SEP41EventMint,
				Amount:     big.NewInt(amt),
			}
		}
		read := func() string {
			var got string
			const q = `SELECT amount::text FROM sep41_supply_events WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&got); err != nil {
				t.Fatalf("read sep41_supply_events: %v", err)
			}
			return got
		}

		// gen 1 — original (wrong) value.
		store.SetDeriveGeneration(1)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v1)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V1: %v", err)
		}

		// gen 2 — corrected re-derive of the SAME PK must UPDATE in place.
		store.SetDeriveGeneration(2)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v2)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V2: %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-2 batch corrective re-derive: amount=%s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", got)
		}

		// gen 0 — stale live replay must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v3)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V3 (gen 0 replay): %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-0 batch replay: amount=%s, want 99000000 "+
				"(guard must preserve the correction)", got)
		}

		// The DO UPDATE upsert rejects an intra-statement duplicate conflict
		// key ("cannot affect row a second time"), which the old DO NOTHING
		// tolerated. A batch carrying the SAME PK twice must be deduped
		// in-writer, not error — and the last copy must win.
		store.SetDeriveGeneration(3)
		dupA := mk(77_000_000)
		dupB := mk(88_000_000)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{dupA, dupB}); err != nil {
			t.Fatalf("batch with an intra-batch duplicate PK must dedupe, not error: %v", err)
		}
		if got := read(); got != "88000000" {
			t.Errorf("intra-batch duplicate PK: amount=%s, want 88000000 (last copy wins)", got)
		}
	})
}
