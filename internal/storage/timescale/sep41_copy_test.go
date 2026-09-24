package timescale

import (
	"strings"
	"testing"
)

// TestCopyMergeUpsertSQL_GenerationGuarded is the proven-red guard for
// TV-1/TV-3: the bulk COPY+merge path used by ch_rebuild must carry the SAME
// INV-3 generation-guarded corrective-upsert semantics as the per-row writers
// (sep41_transfers.go InsertSEP41TransferBatch / sep41_supply_events.go
// InsertSEP41SupplyEvent), NOT the old generation-0 `ON CONFLICT DO NOTHING`.
//
// Before the fix, copyMerge emitted `... ON CONFLICT (...) DO NOTHING` and the
// COPY column list omitted derive_generation, so a re-derive's corrected value
// (a) defaulted to generation 0 and (b) never landed on an existing PK. Worse,
// on a COPY batch error ch_rebuild's per-row fallback (gen-guarded DO UPDATE)
// DID overwrite, so the rebuild's write semantics silently diverged on whether
// COPY happened to error (TV-3). This asserts the corrected, unified semantic:
// derive_generation is present and the merge is the guarded DO UPDATE.
//
// To reproduce red: revert copyMergeUpsertSQL to `... DO NOTHING` (or drop
// derive_generation from the column/update lists) and this fails.
func TestCopyMergeUpsertSQL_GenerationGuarded(t *testing.T) {
	// Mirror the real call for sep41_supply_events.
	const (
		target   = "sep41_supply_events"
		tmp      = "copy_merge_sep41_supply_events"
		colList  = "contract_id, ledger, tx_hash, op_index, event_index, observed_at, event_kind, amount, counterparty, derive_generation"
		conflict = "(contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index)"
	)
	updateCols := []string{"amount", "counterparty", "derive_generation"}

	got := copyMergeUpsertSQL(target, tmp, colList, conflict, updateCols)

	// It must NOT be the old additive DO NOTHING — that is the exact defect.
	if strings.Contains(got, "DO NOTHING") {
		t.Fatalf("copyMergeUpsertSQL still emits DO NOTHING (TV-1/TV-3 regression):\n%s", got)
	}
	if !strings.Contains(got, "DO UPDATE SET") {
		t.Fatalf("want a gen-guarded DO UPDATE merge, got:\n%s", got)
	}

	// The COPY/SELECT column list must carry derive_generation so bulk-loaded
	// rows keep the stamped generation instead of the DEFAULT 0.
	if !strings.Contains(got, "derive_generation") {
		t.Fatalf("merge SQL never mentions derive_generation:\n%s", got)
	}

	// The corrective SET must update every value column IN PLACE, including
	// the guard column itself (so a higher generation advances the marker).
	for _, c := range updateCols {
		want := c + " = EXCLUDED." + c
		if !strings.Contains(got, want) {
			t.Errorf("merge SQL missing corrective assignment %q:\n%s", want, got)
		}
	}

	// The generation guard is the load-bearing security property: a lower
	// (stale) generation can never win the upsert.
	wantGuard := target + ".derive_generation <= EXCLUDED.derive_generation"
	if !strings.Contains(got, wantGuard) {
		t.Fatalf("merge SQL missing the generation guard %q:\n%s", wantGuard, got)
	}
}
