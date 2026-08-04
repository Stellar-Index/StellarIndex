package liquidity_pools

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// This file pins the supply-drift regression from audit-2026-07-23
// DAT-10: reserves of a pool whose last liquidity is withdrawn (entry
// deleted) must stop counting toward the served supply aggregate.
// Before the removal fix the observer emitted nothing for the deletion,
// so the last-seen reserves stayed in Σ lp_reserve forever and
// total/circulating supply (and market cap / FDV downstream) only ever
// grew.

// makeLPChangeOfType builds a liquidity-pool change of the given
// variant. Body-carrying variants carry the full entry; Removed carries
// only the LedgerKey — which is precisely why the observer needs the
// paired State pre-image.
func makeLPChangeOfType(t *testing.T, ct xdr.LedgerEntryChangeType, poolByte byte, assetA, assetB xdr.Asset, reserveA, reserveB int64) xdr.LedgerEntryChange {
	t.Helper()
	body := makeLPChange(t, poolByte, assetA, assetB, reserveA, reserveB) // Updated variant
	entry := body.Updated
	switch ct {
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return body
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return xdr.LedgerEntryChange{Type: ct, Created: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return xdr.LedgerEntryChange{Type: ct, State: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return xdr.LedgerEntryChange{Type: ct, Restored: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		var pid xdr.PoolId
		pid[0] = poolByte
		return xdr.LedgerEntryChange{
			Type: ct,
			Removed: &xdr.LedgerKey{
				Type:          xdr.LedgerEntryTypeLiquidityPool,
				LiquidityPool: &xdr.LedgerKeyLiquidityPool{LiquidityPoolId: pid},
			},
		}
	}
	t.Fatalf("makeLPChangeOfType: unsupported change type %d", ct)
	return xdr.LedgerEntryChange{}
}

// routeLedger walks one ledger's entry-changes through the dispatcher
// exactly as [dispatcher.Dispatcher.ProcessLedger] does — in order,
// with a monotonic IntraLedgerSeq — and returns the Observations the
// observer emitted. Going through the dispatcher (rather than calling
// Decode directly) means Matches is exercised too.
func routeLedger(t *testing.T, disp *dispatcher.Dispatcher, ledger uint32, seq *uint32, changes ...xdr.LedgerEntryChange) []Observation {
	t.Helper()
	var out []Observation
	for _, ch := range changes {
		evs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
			Ledger:         ledger,
			ClosedAt:       time.Unix(1_770_000_000+int64(ledger), 0).UTC(),
			IntraLedgerSeq: *seq,
			Change:         ch,
		})
		*seq++
		if err != nil {
			t.Fatalf("RouteEntryChange(ledger=%d): %v", ledger, err)
		}
		for _, ev := range evs {
			obs, ok := ev.(Observation)
			if !ok {
				t.Fatalf("unexpected event type %T", ev)
			}
			out = append(out, obs)
		}
	}
	return out
}

// servedLPAggregate mirrors the SQL contract of
// Store.SumLPReservesAtOrBefore — the query that feeds
// ClassicSupplyComponents.LPReserve and therefore total_supply:
//
//	SELECT sum(balance_stroops) FROM (
//	  SELECT DISTINCT ON (pool_id) balance_stroops, is_removal
//	    FROM lp_reserve_observations
//	   WHERE asset_key = $1 AND ledger <= $2
//	   ORDER BY pool_id, ledger DESC) latest
//	 WHERE NOT is_removal
//
// i.e. filter to the asset, keep the LATEST observation per pool_id
// (ties inside a ledger resolved by intra_ledger_seq, the writer's
// upsert guard), then sum the ones that are not removals.
func servedLPAggregate(obs []Observation, assetKey string) *big.Int {
	type row struct {
		ledger    uint32
		seq       uint32
		balance   *big.Int
		isRemoval bool
	}
	latest := map[string]row{}
	for _, o := range obs {
		if o.AssetKey != assetKey {
			continue
		}
		cur, seen := latest[o.PoolID]
		if seen && (cur.ledger > o.Ledger || (cur.ledger == o.Ledger && cur.seq > o.IntraLedgerSeq)) {
			continue
		}
		latest[o.PoolID] = row{o.Ledger, o.IntraLedgerSeq, o.Balance, o.IsRemoval}
	}
	total := new(big.Int)
	for _, r := range latest {
		if r.isRemoval {
			continue
		}
		total.Add(total, r.balance)
	}
	return total
}

// TestServedAggregate_ReturnsToZeroAfterFullWithdraw is the DAT-10
// money regression: deposit into a pool, withdraw part of it, then
// withdraw the rest (pool entry deleted). The served Σ lp_reserve must
// return to its pre-deposit value (zero) rather than keep counting the
// withdrawn reserves.
func TestServedAggregate_ReturnsToZeroAfterFullWithdraw(t *testing.T) {
	const assetKey = "USDC:" + gIssuerA
	usdc := makeAsset(t, "USDC", gIssuerA)
	euro := makeAsset(t, "EURO", gIssuerB)

	o, err := NewObserver([]string{assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	var all []Observation

	// Ledger 100 — pool created with 1_000_000 USDC on side A.
	seq := uint32(0)
	all = append(all, routeLedger(t, disp, 100, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 3, usdc, euro, 1_000_000, 5_000_000),
	)...)
	if got := servedLPAggregate(all, assetKey); got.Int64() != 1_000_000 {
		t.Fatalf("after deposit: served LP aggregate = %s, want 1000000", got)
	}

	// Ledger 150 — partial withdraw. Already handled pre-fix; anchors
	// that the fold tracks real values rather than always returning 0.
	seq = 0
	all = append(all, routeLedger(t, disp, 150, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 3, usdc, euro, 1_000_000, 5_000_000),
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryUpdated, 3, usdc, euro, 400_000, 2_000_000),
	)...)
	if got := servedLPAggregate(all, assetKey); got.Int64() != 400_000 {
		t.Fatalf("after partial withdraw: served LP aggregate = %s, want 400000", got)
	}

	// Ledger 200 — last liquidity withdrawn, pool entry deleted.
	seq = 0
	all = append(all, routeLedger(t, disp, 200, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 3, usdc, euro, 400_000, 2_000_000),
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 3, usdc, euro, 400_000, 2_000_000),
	)...)

	got := servedLPAggregate(all, assetKey)
	if got.Sign() != 0 {
		t.Errorf("after full withdraw: served LP aggregate = %s, want 0 "+
			"(vanished reserves still counted → total/circulating supply over-reports)", got)
	}
}

// TestRemovalEmitsBothWatchedSides — a deleted pool with both sides
// watched must zero BOTH asset aggregates; the served query is keyed
// per (asset_key, pool_id), so one removal row per side is required.
func TestRemovalEmitsBothWatchedSides(t *testing.T) {
	keyUSDC := "USDC:" + gIssuerA
	keyAQUA := "AQUA:" + gIssuerB
	usdc := makeAsset(t, "USDC", gIssuerA)
	aqua := makeAsset(t, "AQUA", gIssuerB)

	o, _ := NewObserver([]string{keyUSDC, keyAQUA})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	var all []Observation
	seq := uint32(0)
	all = append(all, routeLedger(t, disp, 100, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 9, usdc, aqua, 1_000_000, 2_000_000),
	)...)
	if got := servedLPAggregate(all, keyUSDC); got.Int64() != 1_000_000 {
		t.Fatalf("USDC after deposit = %s, want 1000000", got)
	}
	if got := servedLPAggregate(all, keyAQUA); got.Int64() != 2_000_000 {
		t.Fatalf("AQUA after deposit = %s, want 2000000", got)
	}

	seq = 0
	removals := routeLedger(t, disp, 200, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 9, usdc, aqua, 1_000_000, 2_000_000),
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 9, usdc, aqua, 1_000_000, 2_000_000),
	)
	if len(removals) != 2 {
		t.Fatalf("got %d observations for the deletion, want 2 (one per watched side)", len(removals))
	}
	for _, rm := range removals {
		if !rm.IsRemoval {
			t.Errorf("%s: IsRemoval=false on a pool deletion, want true", rm.AssetKey)
		}
		if rm.Balance == nil || rm.Balance.Sign() != 0 {
			t.Errorf("%s: Balance=%v, want 0", rm.AssetKey, rm.Balance)
		}
		// The removal must sit AFTER the State change in intra-ledger
		// order — the writer's `intra_ledger_seq <= EXCLUDED` upsert
		// guard depends on it.
		if rm.IntraLedgerSeq != 1 {
			t.Errorf("%s: IntraLedgerSeq=%d, want 1", rm.AssetKey, rm.IntraLedgerSeq)
		}
	}

	all = append(all, removals...)
	if got := servedLPAggregate(all, keyUSDC); got.Sign() != 0 {
		t.Errorf("USDC after pool deletion = %s, want 0", got)
	}
	if got := servedLPAggregate(all, keyAQUA); got.Sign() != 0 {
		t.Errorf("AQUA after pool deletion = %s, want 0", got)
	}
}

// TestRemovalEmitsOnlyWatchedSide — a deleted pool with one watched
// side emits exactly one removal, for that side.
func TestRemovalEmitsOnlyWatchedSide(t *testing.T) {
	const assetKey = "USDC:" + gIssuerA
	usdc := makeAsset(t, "USDC", gIssuerA)
	euro := makeAsset(t, "EURO", gIssuerB)

	o, _ := NewObserver([]string{assetKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	obs := routeLedger(t, disp, 100, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 4, usdc, euro, 1_000_000, 5_000_000),
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 4, usdc, euro, 1_000_000, 5_000_000),
	)
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1 (only the watched side)", len(obs))
	}
	if obs[0].AssetKey != assetKey {
		t.Errorf("AssetKey=%q, want %q", obs[0].AssetKey, assetKey)
	}
}

// TestServedAggregate_ReplayIsIdempotent — re-ingesting the same
// ledgers must not double-remove (the removal is an absorbing state,
// not a decrement) and must not resurrect the reserves.
func TestServedAggregate_ReplayIsIdempotent(t *testing.T) {
	const assetKey = "USDC:" + gIssuerA
	usdc := makeAsset(t, "USDC", gIssuerA)
	euro := makeAsset(t, "EURO", gIssuerB)

	o, _ := NewObserver([]string{assetKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	var all []Observation
	for pass := range 2 {
		seq := uint32(0)
		all = append(all, routeLedger(t, disp, 100, &seq,
			makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 5, usdc, euro, 3_000_000, 1_000),
		)...)
		seq = 0
		all = append(all, routeLedger(t, disp, 200, &seq,
			makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 5, usdc, euro, 3_000_000, 1_000),
			makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 5, usdc, euro, 3_000_000, 1_000),
		)...)
		if got := servedLPAggregate(all, assetKey); got.Sign() != 0 {
			t.Errorf("pass %d: aggregate = %s, want 0", pass, got)
		}
	}
}

// TestUnwatchedPoolRemovalStaysUnmatched — deleting a pool with no
// watched side is still skipped: no pre-image is memoized for it, so
// the removal is unattributable and must not produce a row.
func TestUnwatchedPoolRemovalStaysUnmatched(t *testing.T) {
	euro := makeAsset(t, "EURO", gIssuerB)
	gbp := makeAsset(t, "GBP", gIssuerB)

	o, _ := NewObserver([]string{"USDC:" + gIssuerA})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	obs := routeLedger(t, disp, 100, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 6, euro, gbp, 1_000, 2_000),
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 6, euro, gbp, 1_000, 2_000),
	)
	if len(obs) != 0 {
		t.Errorf("got %d observations for an unwatched-pool deletion, want 0", len(obs))
	}
}

// TestPreImageMemoIsLedgerScoped — the memo must not let a removal in
// a LATER ledger borrow a pre-image from an earlier one; every real
// removal is preceded by its own STATE change in the same ledger.
func TestPreImageMemoIsLedgerScoped(t *testing.T) {
	usdc := makeAsset(t, "USDC", gIssuerA)
	euro := makeAsset(t, "EURO", gIssuerB)

	o, _ := NewObserver([]string{"USDC:" + gIssuerA})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	routeLedger(t, disp, 600, &seq,
		makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 7, usdc, euro, 10, 20),
	)
	// The removal must emit nothing AND must not do so silently. Matches
	// consults hasPreImage, which does not apply lookupPreImage's ledger
	// guard, so this is the one shape that reaches Decode unattributable
	// — and a dropped removal over-reports supply forever, because the
	// removal is absorbing, not a delta. It used to return (nil, nil)
	// (cold audit 2026-08-04).
	evs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
		Ledger:         601,
		ClosedAt:       time.Unix(1_770_000_601, 0).UTC(),
		IntraLedgerSeq: 0,
		Change:         makeLPChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 7, usdc, euro, 10, 20),
	})
	if len(evs) != 0 {
		t.Errorf("got %d observations for a removal with no same-ledger pre-image, want 0", len(evs))
	}
	if err == nil {
		t.Error("RouteEntryChange returned nil error — an unattributable removal of a WATCHED pool must be counted, not silently dropped")
	}
	if !errors.Is(err, ErrUnsupportedLPType) {
		t.Errorf("err = %v, want it to wrap ErrUnsupportedLPType", err)
	}
}
