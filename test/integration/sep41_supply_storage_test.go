//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
	"github.com/StellarIndex/stellar-index/internal/supply"
)

// sep41StoreAdapter projects *timescale.Store onto supply.SEP41SupplyStore
// (the supply package defines its own SEP41KindTotals to avoid a cyclic
// import). Mirrors cmd/stellarindex-aggregator's supplyAggregatorSEP41Store so
// the integration test exercises the exact production reader → computer path.
type sep41StoreAdapter struct{ s *timescale.Store }

func (a sep41StoreAdapter) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (supply.SEP41KindTotals, error) {
	t, err := a.s.SEP41KindTotalsAtOrBefore(ctx, contractID, asOfLedger)
	if err != nil {
		return supply.SEP41KindTotals{}, err
	}
	return supply.SEP41KindTotals{Mint: t.Mint, Burn: t.Burn, Clawback: t.Clawback}, nil
}

func (a sep41StoreAdapter) SACBalanceForContractAtOrBefore(ctx context.Context, holder, assetKey string, asOfLedger uint32) (*big.Int, error) {
	return a.s.SACBalanceForContractAtOrBefore(ctx, holder, assetKey, asOfLedger)
}

func (a sep41StoreAdapter) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	return a.s.MinSEP41ComponentLedger(ctx, contractID, asOfLedger)
}

func (a sep41StoreAdapter) SEP41GenesisBaselineSeeded(ctx context.Context, contractID string) (bool, error) {
	return a.s.SEP41GenesisBaselineSeeded(ctx, contractID)
}

// TestSEP41SupplyEventsRoundTrip exercises the
// InsertSEP41SupplyEvent → SEP41NetMintAtOrBefore →
// SEP41KindTotalsAtOrBefore paths through real TimescaleDB.
// Per ADR-0023 + ADR-0011 Algorithm 3, the running net mint
// (mint - burn - clawback) IS the SEP-41 total supply; if the
// SQL CASE-WHEN sign-flip or DISTINCT ON / FILTER aggregations
// regress, total supply silently goes wrong. The unit tests
// (#309) cover defensive guards but can't validate the SQL —
// this test does.
func TestSEP41SupplyEventsRoundTrip(t *testing.T) {
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
	const otherContract = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// ─── Empty state: net mint = 0; kind totals all zero ─────────
	got, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 1)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (empty): %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("empty net mint = %s, want 0", got)
	}
	totals, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, 1)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore (empty): %v", err)
	}
	if totals.Mint.Sign() != 0 || totals.Burn.Sign() != 0 || totals.Clawback.Sign() != 0 {
		t.Errorf("empty totals: mint=%s burn=%s clawback=%s, want all 0",
			totals.Mint, totals.Burn, totals.Clawback)
	}

	// ─── Insert a mint event at ledger 1000 ──────────────────────
	mintEvent := timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       1000,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000001",
		OpIndex:      0,
		ObservedAt:   t0,
		Kind:         timescale.SEP41EventMint,
		Amount:       big.NewInt(1_000_000),
		Counterparty: "GA1",
	}
	if err := store.InsertSEP41SupplyEvent(ctx, mintEvent); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (mint): %v", err)
	}

	// Idempotent re-insert — same PK is a no-op.
	if err := store.InsertSEP41SupplyEvent(ctx, mintEvent); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (mint dup): %v", err)
	}

	// ─── Insert a burn at ledger 2000 ────────────────────────────
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       2000,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000002",
		OpIndex:      0,
		ObservedAt:   t0.Add(time.Hour),
		Kind:         timescale.SEP41EventBurn,
		Amount:       big.NewInt(300_000),
		Counterparty: "GA1",
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (burn): %v", err)
	}

	// ─── Insert a clawback at ledger 2500 ────────────────────────
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       2500,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000003",
		OpIndex:      0,
		ObservedAt:   t0.Add(2 * time.Hour),
		Kind:         timescale.SEP41EventClawback,
		Amount:       big.NewInt(100_000),
		Counterparty: "GA2",
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (clawback): %v", err)
	}

	// ─── Net mint = 1_000_000 − 300_000 − 100_000 = 600_000 ──────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 3000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	if got.Cmp(big.NewInt(600_000)) != 0 {
		t.Errorf("net mint at ledger 3000 = %s, want 600000", got)
	}

	// ─── Kind totals split out cleanly ───────────────────────────
	totals, err = store.SEP41KindTotalsAtOrBefore(ctx, contractID, 3000)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore: %v", err)
	}
	if totals.Mint.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("Mint = %s, want 1000000", totals.Mint)
	}
	if totals.Burn.Cmp(big.NewInt(300_000)) != 0 {
		t.Errorf("Burn = %s, want 300000", totals.Burn)
	}
	if totals.Clawback.Cmp(big.NewInt(100_000)) != 0 {
		t.Errorf("Clawback = %s, want 100000", totals.Clawback)
	}

	// ─── At-or-before ledger 1500: only the mint counts ──────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 1500)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (1500): %v", err)
	}
	if got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("net mint at ledger 1500 = %s, want 1000000 (burn+clawback excluded)", got)
	}

	// ─── At-or-before ledger 2000: mint + burn ───────────────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 2000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (2000): %v", err)
	}
	if got.Cmp(big.NewInt(700_000)) != 0 {
		t.Errorf("net mint at ledger 2000 = %s, want 700000 (1M − 300K, clawback at 2500 excluded)", got)
	}

	// ─── Other contract is isolated — its totals stay 0 ──────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, otherContract, 5000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (otherContract): %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("isolated contract net mint = %s, want 0 — contract_id filter is broken",
			got)
	}
}

// TestSEP41SupplyEvents_LargeI128 verifies the SQL preserves
// values that exceed int64. SEP-41 amounts are i128 in the wire
// protocol; Algorithm 3's running sum must not silently truncate.
func TestSEP41SupplyEvents_LargeI128(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)

	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: contractID,
		Ledger:     1,
		TxHash:     "2200000000000000000000000000000000000000000000000000000000000001",
		OpIndex:    0,
		ObservedAt: time.Now().UTC(),
		Kind:       timescale.SEP41EventMint,
		Amount:     huge,
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (huge): %v", err)
	}

	got, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 100)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	if got.Cmp(huge) != 0 {
		t.Errorf("got %s, want %s — i128 / NUMERIC round-trip lost precision", got, huge)
	}
}

// TestSEP41SupplyRollup_AdvanceDeltaAndFallback exercises the
// migration-0085 rollup path end-to-end against real TimescaleDB
// (incident 2026-07-06). It pins that:
//
//   - the reader returns the FULL correct totals via the fallback
//     full-sum when no checkpoint exists yet;
//   - AdvanceSEP41SupplyRollup folds only SETTLED ledgers — the current
//     tip ledger is deferred (the `< max(ledger)` watermark guard) so a
//     mid-write ledger is never half-folded;
//   - after an advance the reader returns the SAME totals via
//     rollup ⊕ live delta as the full sum would (the core correctness
//     invariant the fast path relies on);
//   - a historical read below the checkpoint falls back to the full sum;
//   - re-advancing with nothing newly settled is a monotonic no-op.
func TestSEP41SupplyRollup_AdvanceDeltaAndFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	const otherContract = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(ledger uint32, kind timescale.SEP41EventKind, amount int64, at time.Time, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: at, Kind: kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s@%d: %v", kind, ledger, err)
		}
	}

	assertTotals := func(label string, asOf uint32, mint, burn, claw int64) {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, asOf)
		if err != nil {
			t.Fatalf("%s: SEP41KindTotalsAtOrBefore: %v", label, err)
		}
		if got.Mint.Cmp(big.NewInt(mint)) != 0 || got.Burn.Cmp(big.NewInt(burn)) != 0 || got.Clawback.Cmp(big.NewInt(claw)) != 0 {
			t.Errorf("%s @%d = mint=%s burn=%s clawback=%s; want %d/%d/%d",
				label, asOf, got.Mint, got.Burn, got.Clawback, mint, burn, claw)
		}
	}

	insert(1000, timescale.SEP41EventMint, 1_000_000, t0, 1)
	insert(2000, timescale.SEP41EventBurn, 300_000, t0.Add(time.Hour), 2)
	insert(2500, timescale.SEP41EventClawback, 100_000, t0.Add(2*time.Hour), 3)

	// ─── No checkpoint yet — fallback full-sum path ──────────────
	assertTotals("fallback", 3000, 1_000_000, 300_000, 100_000)

	// ─── First advance: tip (2500) deferred, last_ledger = 2000 ──
	adv, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if adv.ToLedger != 2000 {
		t.Errorf("advance 1 ToLedger = %d; want 2000 (tip 2500 deferred by the < max guard)", adv.ToLedger)
	}
	if !adv.Advanced {
		t.Errorf("advance 1 should report Advanced=true")
	}

	// ─── Reader now uses rollup(≤2000) + delta(2000,asOf] ─────────
	assertTotals("rollup+delta", 3000, 1_000_000, 300_000, 100_000)    // delta covers deferred 2500
	assertTotals("at-checkpoint", 2000, 1_000_000, 300_000, 0)         // empty delta, pure rollup
	assertTotals("historical-below-checkpoint", 1500, 1_000_000, 0, 0) // fallback full-sum ≤1500

	// ─── Idempotent re-advance: nothing new settled → no-op ──────
	adv2, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if adv2.Advanced || adv2.ToLedger != 2000 {
		t.Errorf("advance 2 should be a no-op at 2000; got Advanced=%v To=%d", adv2.Advanced, adv2.ToLedger)
	}

	// ─── New settled data: 2500 now settles, 3000 becomes the tip ─
	insert(3000, timescale.SEP41EventMint, 500_000, t0.Add(3*time.Hour), 4)
	adv3, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if adv3.ToLedger != 2500 {
		t.Errorf("advance 3 ToLedger = %d; want 2500 (tip 3000 deferred)", adv3.ToLedger)
	}
	assertTotals("rollup+delta-after-3", 4000, 1_500_000, 300_000, 100_000)

	// ─── Isolation: a different contract stays zero + advances clean
	oth, err := store.SEP41KindTotalsAtOrBefore(ctx, otherContract, 5000)
	if err != nil {
		t.Fatalf("other contract read: %v", err)
	}
	if oth.Mint.Sign() != 0 || oth.Burn.Sign() != 0 || oth.Clawback.Sign() != 0 {
		t.Errorf("other contract totals nonzero: mint=%s burn=%s clawback=%s", oth.Mint, oth.Burn, oth.Clawback)
	}
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, otherContract); err != nil {
		t.Fatalf("advance eventless contract: %v", err)
	}
}

// TestSEP41GenesisBaseline_LifetimeSupplyEndToEnd proves the migration-0088
// fix through the REAL store → reader → computer path against TimescaleDB
// (incident 2026-07-06):
//
//   - A SAC-wrapper with pre-Soroban issuance (seeded genesis mint) + a large
//     Soroban-era burn computes a POSITIVE LIFETIME total and does NOT trip the
//     negative-total guard — the pre-fix failure mode for VELO/AQUA/yXLM/… .
//   - A Soroban-only token (no pre-genesis flows) is UNCHANGED whether or not a
//     baseline row exists, and seeding a ZERO baseline does not double-count.
//   - The baseline is gated on asOfLedger ≥ genesis_baseline_ledger, so a read
//     below the boundary omits it (aggregator always reads at tip).
//   - The genesis-seeded flag flips false → true across the seed.
func TestSEP41GenesisBaseline_LifetimeSupplyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Real production reader → computer over the store.
	reader := supply.NewStorageSEP41SupplyReader(sep41StoreAdapter{s: store})
	computer, err := supply.NewSEP41Computer(supply.Policy{}, reader)
	if err != nil {
		t.Fatalf("NewSEP41Computer: %v", err)
	}

	// Known-valid C-strkeys (canonical validates strkey CRC on construction).
	const (
		sacContract     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // pubnet native-XLM SAC
		sorobanContract = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	)
	const boundary = 50457424 // clickhouse.SorobanGenesisLedger
	const tip = 62000000
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	computeTotal := func(contractID string, asOf uint32) (*big.Int, error) {
		asset, aerr := canonical.NewSorobanAsset(contractID)
		if aerr != nil {
			t.Fatalf("NewSorobanAsset(%s): %v", contractID, aerr)
		}
		snap, cerr := computer.Compute(ctx, asset, asOf, t0)
		if cerr != nil {
			return nil, cerr
		}
		return snap.TotalSupply, nil
	}

	// ─── SAC-wrapper: Soroban-era burn dwarfs Soroban-era mint ───────────
	// Mimics VELO — nearly all issuance predates Soroban, so the Soroban-era
	// window alone reads Σburn ≫ Σmint.
	sorobanMint := big.NewInt(2) // negligible Soroban-era mint
	sorobanBurn, _ := new(big.Int).SetString("2180000000000", 10)
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sacContract, Ledger: 60000000, TxHash: txh(1), OpIndex: 0,
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: sorobanMint,
	}); err != nil {
		t.Fatalf("insert sac soroban mint: %v", err)
	}
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sacContract, Ledger: 60000001, TxHash: txh(2), OpIndex: 0,
		ObservedAt: t0.Add(time.Hour), Kind: timescale.SEP41EventBurn, Amount: sorobanBurn,
	}); err != nil {
		t.Fatalf("insert sac soroban burn: %v", err)
	}

	// Pre-fix state: no baseline seeded → negative total → benign
	// missing-baseline sentinel (NOT a paging compute_error).
	if seeded, err := store.SEP41GenesisBaselineSeeded(ctx, sacContract); err != nil || seeded {
		t.Fatalf("pre-seed SEP41GenesisBaselineSeeded = %v, %v; want false, nil", seeded, err)
	}
	if _, err := computeTotal(sacContract, tip); !errors.Is(err, supply.ErrNegativeTotalMissingBaseline) {
		t.Fatalf("pre-seed compute err = %v; want ErrNegativeTotalMissingBaseline", err)
	}

	// Seed the pre-Soroban opening balance (lifetime mint lived below the
	// boundary — synthesized from the CH lake in production).
	genesisMint, _ := new(big.Int).SetString("2400000000000", 10)
	if err := store.UpsertSEP41GenesisBaseline(ctx, sacContract,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("UpsertSEP41GenesisBaseline (sac): %v", err)
	}
	if seeded, err := store.SEP41GenesisBaselineSeeded(ctx, sacContract); err != nil || !seeded {
		t.Fatalf("post-seed SEP41GenesisBaselineSeeded = %v, %v; want true, nil", seeded, err)
	}

	// Post-fix: lifetime total = (2 + 2.4e12) − 2.18e12 = 220000000002, positive,
	// guard not tripped.
	got, err := computeTotal(sacContract, tip)
	if err != nil {
		t.Fatalf("post-seed compute (sac): %v", err)
	}
	wantSac := new(big.Int).Sub(new(big.Int).Add(sorobanMint, genesisMint), sorobanBurn)
	if got.Cmp(wantSac) != 0 {
		t.Errorf("sac lifetime total = %s, want %s (positive, incl. pre-Soroban baseline)", got, wantSac)
	}
	if got.Sign() <= 0 {
		t.Errorf("sac lifetime total = %s, want > 0", got)
	}

	// Baseline gate: a read BELOW the boundary omits the genesis baseline (the
	// Soroban-era events are also above it, so the answer is 0 there).
	belowTotals, err := store.SEP41KindTotalsAtOrBefore(ctx, sacContract, boundary-1)
	if err != nil {
		t.Fatalf("kind totals below boundary: %v", err)
	}
	if belowTotals.Mint.Sign() != 0 || belowTotals.Burn.Sign() != 0 {
		t.Errorf("below-boundary totals = mint=%s burn=%s; want 0/0 (genesis not added below boundary)",
			belowTotals.Mint, belowTotals.Burn)
	}

	// Idempotent re-seed does not double-count.
	if err := store.UpsertSEP41GenesisBaseline(ctx, sacContract,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("re-seed (sac): %v", err)
	}
	if got2, err := computeTotal(sacContract, tip); err != nil || got2.Cmp(wantSac) != 0 {
		t.Errorf("re-seed changed total: got %s, %v; want %s (idempotent SET, no double-count)", got2, err, wantSac)
	}

	// The rollup worker + a seeded baseline coexist on the same row: advancing
	// the Soroban-era checkpoint must not disturb the genesis columns.
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sacContract); err != nil {
		t.Fatalf("advance after seed: %v", err)
	}
	if got3, err := computeTotal(sacContract, tip); err != nil || got3.Cmp(wantSac) != 0 {
		t.Errorf("advance disturbed total: got %s, %v; want %s", got3, err, wantSac)
	}

	// ─── Soroban-only token: already correct, must stay UNCHANGED ────────
	sorOnlyMint := big.NewInt(1_000_000_000)
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sorobanContract, Ledger: 60000000, TxHash: txh(10), OpIndex: 0,
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: sorOnlyMint,
	}); err != nil {
		t.Fatalf("insert soroban-only mint: %v", err)
	}
	// Unseeded: total == Soroban-era mint.
	if got, err := computeTotal(sorobanContract, tip); err != nil || got.Cmp(sorOnlyMint) != 0 {
		t.Errorf("soroban-only unseeded total = %s, %v; want %s (unchanged)", got, err, sorOnlyMint)
	}
	// Seeding a ZERO baseline (the production seed for a token with no
	// pre-genesis flows) leaves the served number identical — no double-count.
	if err := store.UpsertSEP41GenesisBaseline(ctx, sorobanContract,
		timescale.SEP41KindTotals{Mint: big.NewInt(0), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("zero-seed (soroban-only): %v", err)
	}
	if got, err := computeTotal(sorobanContract, tip); err != nil || got.Cmp(sorOnlyMint) != 0 {
		t.Errorf("soroban-only zero-seeded total = %s, %v; want %s (still unchanged — no double-count)", got, err, sorOnlyMint)
	}
}

// TestSEP41GenesisBaseline_BoundaryPartitionDisjoint pins the EXACT ledger seam
// between the two supply slices the migration-0088 reader stitches together: the
// seeded pre-Soroban genesis baseline (ledger < boundary, from the CH lake) and
// the Soroban-era totals (ledger >= boundary, from PG). The invariant is that
// they are a DISJOINT partition — every ledger belongs to exactly one — so
// folding them cannot double-count.
//
// It places a Soroban-era mint at EXACTLY the boundary ledger and asserts:
//
//   - a read one ledger BELOW the boundary sees NEITHER slice (genesis gated off
//     by asOf < genesis_baseline_ledger; the boundary event is above asOf) → 0;
//   - a read AT the boundary sees the genesis baseline PLUS the boundary event,
//     each counted exactly once (genesis + soroban), never twice;
//   - a read at the chain tip is identical.
//
// The `< boundary` (genesis) vs `>= boundary` (Soroban observer / the reader's
// `asOf >= genesis_baseline_ledger` gate) seam is what makes the fold sound; the
// operator-facing half of the same invariant — refusing a seed boundary above
// the true Soroban genesis — is enforced by validateGenesisLedgerBoundary in
// cmd/stellarindex-ops (its own unit test).
func TestSEP41GenesisBaseline_BoundaryPartitionDisjoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const boundary uint32 = 50457424 // clickhouse.SorobanGenesisLedger
	const tip uint32 = 62000000
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	genesisMint := big.NewInt(900)  // pre-Soroban opening balance (ledger < boundary)
	boundaryMint := big.NewInt(100) // a Soroban-era mint at EXACTLY the boundary ledger

	// Soroban-era event at ledger == boundary — the seam case: it must land in
	// the PG (Soroban) slice, never in the CH (genesis) slice.
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: contractID, Ledger: boundary, OpIndex: 0,
		TxHash:     fmt.Sprintf("%064x", 1),
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: boundaryMint,
	}); err != nil {
		t.Fatalf("insert boundary mint: %v", err)
	}

	// Seed the pre-Soroban genesis baseline with the boundary as its exclusive
	// upper bound (production passes clickhouse.SorobanGenesisLedger).
	if err := store.UpsertSEP41GenesisBaseline(ctx, contractID,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("UpsertSEP41GenesisBaseline: %v", err)
	}

	mintAt := func(asOf uint32) *big.Int {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, asOf)
		if err != nil {
			t.Fatalf("SEP41KindTotalsAtOrBefore @%d: %v", asOf, err)
		}
		return got.Mint
	}

	// Below the boundary: genesis gated off, boundary event above asOf → 0.
	if got := mintAt(boundary - 1); got.Sign() != 0 {
		t.Errorf("mint @boundary-1 = %s, want 0 (neither slice contributes below the seam)", got)
	}
	// At the boundary: genesis (900) + the boundary event (100), each once.
	wantLifetime := new(big.Int).Add(genesisMint, boundaryMint) // 1000
	if got := mintAt(boundary); got.Cmp(wantLifetime) != 0 {
		t.Errorf("mint @boundary = %s, want %s (genesis + boundary event, each counted once)", got, wantLifetime)
	}
	// At tip: identical — no further events, and the fold does not re-add.
	if got := mintAt(tip); got.Cmp(wantLifetime) != 0 {
		t.Errorf("mint @tip = %s, want %s (disjoint fold, no double-count)", got, wantLifetime)
	}
}

// TestSEP41SupplyRollupFoldReset proves the incident-2026-07-06 re-derive
// footgun fix: ResetSEP41SupplyRollupFold (which `ch-rebuild -sep41 -write`
// calls automatically) clears the worker-owned fold columns so the aggregator
// re-folds a re-derived history correctly, WITHOUT wiping the migration-0088
// genesis baseline. It exercises both variants the ch-rebuild wiring drives:
//
//   - SCOPED reset (-contracts) zeroes ONLY the listed contract's fold row,
//     leaving other watched contracts' checkpoints intact;
//   - FULL reset (nil scope) zeroes EVERY contract's fold row (the whole-table
//     TRUNCATE-equivalent);
//   - both PRESERVE the seeded genesis columns (genesis_mint_total +
//     genesis_baseline_ledger), asserted directly against the row;
//   - and after a reset + re-advance the worker re-folds a below-checkpoint
//     recovery it would otherwise never see (the served-undercount half of the
//     bug that a bare re-derive leaves behind).
func TestSEP41SupplyRollupFoldReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Raw connection for DIRECT column assertions — the exported reader hides
	// last_ledger and the fold-vs-genesis column split, so this is the literal
	// proof that the reset zeroes the fold columns and spares the genesis ones.
	rawdb, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contractA = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		contractB = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"

		boundary  uint32 = 50457424 // clickhouse.SorobanGenesisLedger
		lSettled1 uint32 = 50457500 // folded into the checkpoint
		lSettled2 uint32 = 50458000 // folded into the checkpoint (becomes last_ledger)
		lRecover  uint32 = 50457600 // a below-checkpoint recovery row (< last_ledger)
		lTip      uint32 = 50460000 // deferred by the < max(ledger) watermark guard
		readAt    uint32 = 50460000 // >= boundary, so the genesis baseline is added
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(c string, ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: c, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: t0, Kind: kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s %s@%d: %v", c, kind, ledger, err)
		}
	}

	type rollupRow struct {
		mint, genesisMint string
		lastLedger        int64
		genesisSeeded     bool
	}
	readRow := func(c string) rollupRow {
		t.Helper()
		var r rollupRow
		var genesisLedger sql.NullInt64
		if err := rawdb.QueryRowContext(ctx, `
			SELECT mint_total::text, last_ledger, genesis_mint_total::text, genesis_baseline_ledger
			  FROM sep41_supply_rollup WHERE contract_id = $1`, c).
			Scan(&r.mint, &r.lastLedger, &r.genesisMint, &genesisLedger); err != nil {
			t.Fatalf("read rollup %s: %v", c, err)
		}
		r.genesisSeeded = genesisLedger.Valid
		return r
	}
	mintAt := func(c string, asOf uint32) int64 {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, c, asOf)
		if err != nil {
			t.Fatalf("SEP41KindTotalsAtOrBefore %s@%d: %v", c, asOf, err)
		}
		return got.Mint.Int64()
	}

	// ─── Seed both contracts: settled history + a seeded genesis baseline ─
	genesisMint := big.NewInt(4_000_000)
	for _, c := range []string{contractA, contractB} {
		insert(c, lSettled1, timescale.SEP41EventMint, 1_000_000, 1)
		insert(c, lSettled2, timescale.SEP41EventMint, 500_000, 2)
		insert(c, lTip, timescale.SEP41EventMint, 1, 3) // tip — deferred by the < max guard
		if err := store.UpsertSEP41GenesisBaseline(ctx, c,
			timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
			boundary); err != nil {
			t.Fatalf("seed genesis %s: %v", c, err)
		}
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance %s: %v", c, err)
		}
	}
	for _, c := range []string{contractA, contractB} {
		if r := readRow(c); r.mint != "1500000" || r.lastLedger != int64(lSettled2) {
			t.Fatalf("%s pre-reset fold = mint %s last_ledger %d; want 1500000 / %d", c, r.mint, r.lastLedger, lSettled2)
		}
	}

	// ─── Recover a below-checkpoint row on BOTH (the re-derive's effect) ──
	// A previously-missing mint at ledger lRecover (< last_ledger). The worker's
	// `> last_ledger` fold can never see it, so a bare re-advance UNDERCOUNTS.
	for _, c := range []string{contractA, contractB} {
		insert(c, lRecover, timescale.SEP41EventMint, 700_000, 4)
	}
	// Lifetime mint = genesis(4M) + 1M + 0.5M + tip(1) [+ recovered 0.7M when folded].
	const wantUndercount = 5_500_001 // recovered row invisible to the incremental worker
	const wantFixed = 6_200_001      // recovered row folded after a reset
	for _, c := range []string{contractA, contractB} {
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance-no-reset %s: %v", c, err)
		}
		if got := mintAt(c, readAt); got != wantUndercount {
			t.Fatalf("%s without reset = %d; want %d (below-checkpoint row invisible to the worker)", c, got, wantUndercount)
		}
	}

	// ─── SCOPED reset: only contractA ─────────────────────────────────────
	n, err := store.ResetSEP41SupplyRollupFold(ctx, []string{contractA})
	if err != nil {
		t.Fatalf("scoped reset: %v", err)
	}
	if n != 1 {
		t.Errorf("scoped reset touched %d rows; want 1 (only contractA)", n)
	}
	ra := readRow(contractA)
	if ra.mint != "0" || ra.lastLedger != 0 {
		t.Errorf("A post-scoped-reset fold = mint %s last_ledger %d; want 0 / 0", ra.mint, ra.lastLedger)
	}
	if ra.genesisMint != "4000000" || !ra.genesisSeeded {
		t.Errorf("A post-scoped-reset genesis = %s seeded %v; want 4000000 / true (baseline must survive the reset)", ra.genesisMint, ra.genesisSeeded)
	}
	if rb := readRow(contractB); rb.lastLedger != int64(lSettled2) {
		t.Errorf("B fold last_ledger = %d; want %d (scoped reset must not touch B)", rb.lastLedger, lSettled2)
	}

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractA); err != nil {
		t.Fatalf("re-advance A: %v", err)
	}
	if got := mintAt(contractA, readAt); got != wantFixed {
		t.Errorf("A after scoped reset+advance = %d; want %d (recovered row now folded)", got, wantFixed)
	}
	if got := mintAt(contractB, readAt); got != wantUndercount {
		t.Errorf("B still = %d; want %d (unaffected by the A-scoped reset)", got, wantUndercount)
	}

	// ─── FULL reset: nil scope zeroes EVERY remaining folded row ──────────
	n, err = store.ResetSEP41SupplyRollupFold(ctx, nil)
	if err != nil {
		t.Fatalf("full reset: %v", err)
	}
	if n != 2 {
		t.Errorf("full reset touched %d rows; want 2 (all watched contracts)", n)
	}
	for _, c := range []string{contractA, contractB} {
		r := readRow(c)
		if r.mint != "0" || r.lastLedger != 0 {
			t.Errorf("%s post-full-reset fold = mint %s last_ledger %d; want 0 / 0", c, r.mint, r.lastLedger)
		}
		if r.genesisMint != "4000000" || !r.genesisSeeded {
			t.Errorf("%s post-full-reset genesis = %s seeded %v; want 4000000 / true (baseline preserved)", c, r.genesisMint, r.genesisSeeded)
		}
	}
	for _, c := range []string{contractA, contractB} {
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("final advance %s: %v", c, err)
		}
		if got := mintAt(c, readAt); got != wantFixed {
			t.Errorf("%s after full reset+advance = %d; want %d", c, got, wantFixed)
		}
	}
}

// TestSEP41SupplyRollup_LargeI128 verifies the rollup checkpoint + delta
// preserve values exceeding int64 — Σmint alone can exceed i128, so the
// running NUMERIC totals must never truncate (ADR-0003).
func TestSEP41SupplyRollup_LargeI128(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	tail := big.NewInt(7)
	want := new(big.Int).Add(huge, tail)

	// huge at ledger 1 (folded into the checkpoint), tail at ledger 2
	// (the deferred tip, served from the live delta) — so the read sum
	// spans BOTH the rollup and the delta.
	for i, ev := range []struct {
		ledger uint32
		amt    *big.Int
	}{{1, huge}, {2, tail}} {
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ev.ledger,
			TxHash:  fmt.Sprintf("%064x", i+1),
			OpIndex: 0, ObservedAt: time.Now().UTC(),
			Kind: timescale.SEP41EventMint, Amount: ev.amt,
		}); err != nil {
			t.Fatalf("insert huge[%d]: %v", i, err)
		}
	}

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, 100)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore: %v", err)
	}
	if got.Mint.Cmp(want) != 0 {
		t.Errorf("rollup+delta mint = %s, want %s — i128 truncated across the checkpoint boundary", got.Mint, want)
	}
}
