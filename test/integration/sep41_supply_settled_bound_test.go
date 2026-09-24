//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// settleSEP41Cursor marks the sep41_supply projection DURABLE through
// `ledger` by writing the projector's ingestion cursor on the exact
// (source, sub) pair internal/projector commits to — `"projector"` /
// `src.Name` — so the pairing the storage layer hard-codes
// (sep41SupplyCursorSource / sep41SupplyCursorSub) is pinned end-to-end
// rather than restated.
//
// The pair is the production shape; the CALL is not, deliberately. The
// projector's only cursor write is `AdvanceCursorFrom` — a compare-and-swap
// against the position the cycle read, since F159 (`7f2a32655`), so that an
// in-flight cycle cannot clobber a `projector-replay` rewind. Seeding a
// starting position has nothing to compare against, so this uses the
// unconditional `UpsertCursor`. A helper that claimed to seed "through the
// exact call the projector makes" would send a reader to a call the projector
// no longer has.
//
// AdvanceSEP41SupplyRollup folds no further than this watermark (F118),
// so a rollup test that wants "everything below the tip has settled"
// must say so explicitly.
func settleSEP41Cursor(t *testing.T, ctx context.Context, store *timescale.Store, ledger uint32) { //nolint:revive // t-first matches the file's other helpers (startTimescale).
	t.Helper()
	if err := store.UpsertCursor(ctx, "projector", sep41supply.SourceName, ledger); err != nil {
		t.Fatalf("UpsertCursor(projector, %s, %d): %v", sep41supply.SourceName, ledger, err)
	}
}

// sep41SettledTestCursorLedger is far above every ledger the rollup tests
// use, so seeding it leaves `< max(ledger)` as the binding half of the
// F118 settled bound and those tests keep pinning the tip-deferral guard,
// not the durability guard.
const sep41SettledTestCursorLedger = 200_000_000

// TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor pins F118 (audit
// 2026-09-02): AdvanceSEP41SupplyRollup must not fold past the ledger the
// projector is still holding for retry, or the retried row lands
// permanently between the two halves of the served read and the token's
// supply is silently under-counted.
//
// The scenario is the production one. A supply-affecting event at ledger L
// hits a transient sink fault (deadlock / statement_timeout). The projector
// does NOT abort the cycle: it writes the rest of the window (L+1 …) and
// caps its cursor at L-1 so L is re-read next cycle. Meanwhile the 5-minute
// rollup pass runs. Bounded by max(ledger) alone it folded L+1 … into the
// checkpoint and pushed last_ledger ABOVE L; when the retry finally wrote
// L, no later fold could see it (folds look only above last_ledger) and the
// reader's live delta could not see it either (it adds only
// `ledger > last_ledger`) — the amount vanished from served supply with the
// pass reporting a normal advance.
//
// Also pins the fail-closed default: with no projector cursor at all there
// is no evidence of settlement, so the pass folds NOTHING and the reader
// still answers exactly (from the full-sum path, at a higher query cost).
func TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" // synthetic
	const uncursored = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6" // synthetic
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(contract string, ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contract, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: t0.Add(time.Duration(ledger) * time.Second),
			Kind:       kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s %s@%d: %v", contract, kind, ledger, err)
		}
	}
	totals := func(label, contract string, asOf uint32) timescale.SEP41KindTotals {
		t.Helper()
		got, terr := store.SEP41KindTotalsAtOrBefore(ctx, contract, asOf)
		if terr != nil {
			t.Fatalf("%s: SEP41KindTotalsAtOrBefore(%s@%d): %v", label, contract, asOf, terr)
		}
		return got
	}
	assertTotals := func(label, contract string, asOf uint32, mint, burn int64) {
		t.Helper()
		got := totals(label, contract, asOf)
		if got.Mint.Cmp(big.NewInt(mint)) != 0 || got.Burn.Cmp(big.NewInt(burn)) != 0 {
			t.Errorf("%s: served totals @%d = mint=%s burn=%s; want mint=%d burn=%d",
				label, asOf, got.Mint, got.Burn, mint, burn)
		}
	}

	// ─── Fail-closed: no projector cursor yet → nothing is provably
	//     settled, so the pass folds nothing and the reader stays exact.
	insert(uncursored, 100, timescale.SEP41EventMint, 5, 90)
	insert(uncursored, 200, timescale.SEP41EventMint, 7, 91)
	advNoCursor, err := store.AdvanceSEP41SupplyRollup(ctx, uncursored)
	if err != nil {
		t.Fatalf("advance (no cursor): %v", err)
	}
	if advNoCursor.Advanced || advNoCursor.ToLedger != 0 {
		t.Errorf("advance with NO projector cursor = {Advanced:%v To:%d}; want {false 0} — nothing has provably settled",
			advNoCursor.Advanced, advNoCursor.ToLedger)
	}
	if !advNoCursor.CursorAbsent {
		t.Error("advance with NO projector cursor reported CursorAbsent=false; the pinned fold must be distinguishable from a steady-state no-op")
	}
	assertTotals("no-cursor reader", uncursored, 1000, 12, 0)

	// ─── Clean cycles: the projector has committed through ledger 999. ──
	insert(contractID, 900, timescale.SEP41EventMint, 700_000, 1)
	insert(contractID, 950, timescale.SEP41EventBurn, 100_000, 2)
	settleSEP41Cursor(t, ctx, store, 999)

	adv1, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if adv1.ToLedger != 900 {
		t.Errorf("advance 1 ToLedger = %d; want 900 (tip 950 deferred by the < max(ledger) guard)", adv1.ToLedger)
	}
	if adv1.CursorAbsent {
		t.Error("advance 1 reported CursorAbsent=true with the projector cursor committed through 999")
	}
	assertTotals("after clean fold", contractID, 5000, 700_000, 100_000)

	// ─── The fault. The mint at ledger 1000 fails its sink write with a
	//     transient fault, so it is NOT written; the projector keeps
	//     writing the rest of the window and holds its cursor at 999.
	insert(contractID, 1001, timescale.SEP41EventMint, 2_000_000, 3)
	insert(contractID, 1002, timescale.SEP41EventMint, 3_000_000, 4)
	insert(contractID, 1003, timescale.SEP41EventMint, 5_000_000, 5)

	// The rollup pass fires in that gap. It must stop BELOW the held
	// ledger — max(ledger) says 1003 is settled, the cursor says only
	// ledgers ≤ 999 are.
	adv2, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if adv2.ToLedger != 950 {
		t.Errorf("advance 2 ToLedger = %d; want 950 — the fold must not pass ledger 1000, which the projector is holding for retry (cursor=999)", adv2.ToLedger)
	}
	// Whatever the checkpoint, the reader is still whole at this point:
	// everything above it is covered by the live delta.
	assertTotals("during hold", contractID, 5000, 10_700_000, 100_000)

	// ─── The retry succeeds: the ledger-1000 mint lands, and the
	//     projector's next clean cycle commits through 1003.
	insert(contractID, 1000, timescale.SEP41EventMint, 1_000_000, 6)
	settleSEP41Cursor(t, ctx, store, 1003)

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance 3: %v", err)
	}

	// The money assertion: the retried mint is part of served supply.
	// Folding past it (the pre-fix bound) loses exactly its 1,000,000.
	const wantMint = 700_000 + 1_000_000 + 2_000_000 + 3_000_000 + 5_000_000
	assertTotals("after retry landed", contractID, 5000, wantMint, 100_000)

	// Independent oracle: SEP41NetMintAtOrBefore never consults the
	// rollup, so the fast path and the authoritative full aggregate must
	// agree — the invariant the whole checkpoint design rests on.
	full, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 5000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	got := totals("oracle", contractID, 5000)
	net := new(big.Int).Sub(got.Mint, new(big.Int).Add(got.Burn, got.Clawback))
	if net.Cmp(full) != 0 {
		t.Errorf("rollup fast path net = %s, authoritative full sum = %s — the checkpoint dropped %s",
			net, full, new(big.Int).Sub(full, net))
	}
	if full.Cmp(big.NewInt(wantMint-100_000)) != 0 {
		t.Errorf("full-sum net mint = %s; want %d", full, wantMint-100_000)
	}
}
