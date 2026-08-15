//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCopyMergeSEP41_GenerationGuard is the proven-red DB-backed test for
// TV-1/TV-3 (audit-2026-08-14): the BULK COPY+merge writers used by
// ch_rebuild (CopyMergeSEP41SupplyEvents / CopyMergeSEP41Transfers) must
// carry the SAME INV-3 generation-guarded corrective-upsert semantics as
// their per-row siblings, not the old generation-0 `ON CONFLICT DO NOTHING`.
//
// Before the fix the bulk path (a) omitted derive_generation from the COPY
// column list, so every bulk row defaulted to the migration-0110 DEFAULT 0,
// and (b) merged DO NOTHING, so a corrected re-derive of a wrong money value
// silently no-op'd against an existing PK. On the unfixed code:
//   - "corrective re-derive lands" goes RED (DO NOTHING keeps the wrong V1).
//   - "gen-0 replay cannot revert" would trivially pass on DO NOTHING but is
//     kept as the companion assertion that pins the guard direction once the
//     merge is DO UPDATE.
//
// TV-3 is the same defect observed from ch_rebuild: with the primary bulk
// path at DO NOTHING and the per-row fallback at gen-guarded DO UPDATE, a
// COPY batch error silently flipped additive-recovery into an overwrite.
// Unifying the bulk path on the guarded DO UPDATE makes the two paths
// semantically identical, which this test's use of the real bulk writer pins.
//
// To reproduce red: revert copyMergeUpsertSQL (sep41_copy.go) to
// `... ON CONFLICT ... DO NOTHING` (keep migration 0110 + SetDeriveGeneration)
// and the "corrective re-derive lands" assertion goes red.
func TestCopyMergeSEP41_GenerationGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		v1 = 12_000_000 // wrong original
		v2 = 99_000_000 // corrected re-derive
		v3 = 55_000_000 // stale gen-0 replay (must be ignored)
	)

	// ── sep41_supply_events via CopyMergeSEP41SupplyEvents ─────────────────
	t.Run("SupplyEvents", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			ledger     = uint32(70_200_001)
		)
		obs := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
		txHash := pad64("3", 9)
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
		read := func() (amount string, gen int64) {
			const q = `SELECT amount::text, derive_generation FROM sep41_supply_events WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&amount, &gen); err != nil {
				t.Fatalf("read sep41_supply_events: %v", err)
			}
			return
		}

		// gen 1 — the wrong original value lands via the bulk path.
		store.SetDeriveGeneration(1)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v1)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V1: %v", err)
		}
		if amt, gen := read(); amt != "12000000" || gen != 1 {
			// The generation must NOT be the DEFAULT 0 (TV-1): if derive_generation
			// is omitted from the COPY list, gen reads 0 here and this fails.
			t.Fatalf("after gen-1 bulk load: amount=%s gen=%d, want 12000000 / gen 1 "+
				"(TV-1: omitting derive_generation from COPY makes gen default to 0)", amt, gen)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in place.
		// The unfixed DO-NOTHING bulk merge keeps V1 → this goes red.
		store.SetDeriveGeneration(2)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v2)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V2: %v", err)
		}
		if amt, gen := read(); amt != "99000000" || gen != 2 {
			t.Errorf("after gen-2 corrective bulk re-derive: amount=%s gen=%d, want 99000000 / gen 2 "+
				"(TV-1/TV-3: the old DO NOTHING keeps 12000000)", amt, gen)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not revert.
		store.SetDeriveGeneration(0)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v3)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V3 (gen 0 replay): %v", err)
		}
		if amt, _ := read(); amt != "99000000" {
			t.Errorf("after gen-0 bulk replay: amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", amt)
		}
	})

	// ── sep41_transfers via CopyMergeSEP41Transfers ────────────────────────
	t.Run("Transfers", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			from       = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
			to         = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
			ledger     = uint32(70_300_001)
		)
		obs := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
		txHash := pad64("4", 11)
		mk := func(amt int64) timescale.SEP41TransferRow {
			return timescale.SEP41TransferRow{
				ContractID: contractID,
				Ledger:     ledger,
				TxHash:     txHash,
				OpIndex:    0,
				EventIndex: 0,
				ObservedAt: obs,
				Kind:       timescale.SEP41Transfer,
				FromAddr:   from,
				ToAddr:     to,
				Amount:     big.NewInt(amt),
			}
		}
		read := func() (amount string, gen int64) {
			const q = `SELECT amount::text, derive_generation FROM sep41_transfers WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&amount, &gen); err != nil {
				t.Fatalf("read sep41_transfers: %v", err)
			}
			return
		}

		store.SetDeriveGeneration(1)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v1)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V1: %v", err)
		}
		if amt, gen := read(); amt != "12000000" || gen != 1 {
			t.Fatalf("after gen-1 bulk load: amount=%s gen=%d, want 12000000 / gen 1", amt, gen)
		}

		store.SetDeriveGeneration(2)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v2)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V2: %v", err)
		}
		if amt, gen := read(); amt != "99000000" || gen != 2 {
			t.Errorf("after gen-2 corrective bulk re-derive: amount=%s gen=%d, want 99000000 / gen 2 "+
				"(TV-1/TV-3: the old DO NOTHING keeps 12000000)", amt, gen)
		}

		store.SetDeriveGeneration(0)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v3)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V3 (gen 0 replay): %v", err)
		}
		if amt, _ := read(); amt != "99000000" {
			t.Errorf("after gen-0 bulk replay: amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", amt)
		}
	})
}
