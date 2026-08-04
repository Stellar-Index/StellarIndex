package claimable_balances

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// This file pins the supply-drift regression from audit-2026-07-23
// DAT-10: a claimed claimable balance must stop counting toward the
// served supply aggregate. Before the removal fix the observer emitted
// nothing for a ClaimClaimableBalance, so the created amount stayed in
// Σ claimable forever and total/circulating supply (and market cap /
// FDV downstream) only ever grew.

// makeCBChangeOfType builds a claimable-balance change of the given
// variant. Body-carrying variants (Created/Updated/State/Restored)
// carry the full entry; Removed carries only the LedgerKey — which is
// precisely why the observer needs the paired State pre-image.
func makeCBChangeOfType(t *testing.T, ct xdr.LedgerEntryChangeType, balanceIDByte byte, code, issuer string, amount int64) xdr.LedgerEntryChange {
	t.Helper()
	body := makeCBChange(t, balanceIDByte, code, issuer, amount) // Created variant
	entry := body.Created
	switch ct {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return body
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return xdr.LedgerEntryChange{Type: ct, Updated: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return xdr.LedgerEntryChange{Type: ct, State: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return xdr.LedgerEntryChange{Type: ct, Restored: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		var hash xdr.Hash
		hash[0] = balanceIDByte
		return xdr.LedgerEntryChange{
			Type: ct,
			Removed: &xdr.LedgerKey{
				Type: xdr.LedgerEntryTypeClaimableBalance,
				ClaimableBalance: &xdr.LedgerKeyClaimableBalance{
					BalanceId: xdr.ClaimableBalanceId{
						Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0,
						V0:   &hash,
					},
				},
			},
		}
	}
	t.Fatalf("makeCBChangeOfType: unsupported change type %d", ct)
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

// servedClaimableAggregate mirrors the SQL contract of
// Store.SumClaimableBalancesAtOrBefore — the query that feeds
// ClassicSupplyComponents.Claimable and therefore total_supply:
//
//	SELECT sum(balance_stroops) FROM (
//	  SELECT DISTINCT ON (claimable_id) balance_stroops, is_removal
//	    FROM claimable_observations
//	   WHERE asset_key = $1 AND ledger <= $2
//	   ORDER BY claimable_id, ledger DESC) latest
//	 WHERE NOT is_removal
//
// i.e. filter to the asset, keep the LATEST observation per
// claimable_id (ties inside a ledger resolved by intra_ledger_seq, the
// writer's upsert guard), then sum the ones that are not removals.
func servedClaimableAggregate(obs []Observation, assetKey string) *big.Int {
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
		cur, seen := latest[o.ClaimableID]
		if seen && (cur.ledger > o.Ledger || (cur.ledger == o.Ledger && cur.seq > o.IntraLedgerSeq)) {
			continue
		}
		latest[o.ClaimableID] = row{o.Ledger, o.IntraLedgerSeq, o.Balance, o.IsRemoval}
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

// TestServedAggregate_ReturnsToZeroAfterClaim is the DAT-10 money
// regression: create a claimable balance, then claim it, and the
// served Σ claimable must return to its pre-create value (zero) rather
// than keep counting the claimed amount.
func TestServedAggregate_ReturnsToZeroAfterClaim(t *testing.T) {
	const assetKey = "USDC:" + gIssuer
	const amount = int64(1_000_000)

	o, err := NewObserver([]string{assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	var all []Observation

	// Ledger 100 — the balance is created.
	seq := uint32(0)
	all = append(all, routeLedger(t, disp, 100, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 0xab, "USDC", gIssuer, amount),
	)...)
	if got := servedClaimableAggregate(all, assetKey); got.Int64() != amount {
		t.Fatalf("after create: served claimable aggregate = %s, want %d", got, amount)
	}

	// Ledger 200 — ClaimClaimableBalance. stellar-core emits the
	// pre-image STATE immediately before the REMOVED for the entry.
	seq = 0
	all = append(all, routeLedger(t, disp, 200, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 0xab, "USDC", gIssuer, amount),
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0xab, "USDC", gIssuer, amount),
	)...)

	got := servedClaimableAggregate(all, assetKey)
	if got.Sign() != 0 {
		t.Errorf("after claim: served claimable aggregate = %s, want 0 "+
			"(claimed balance still counted → total/circulating supply over-reports)", got)
	}
}

// TestRemovalObservationShape pins the row the writer persists for a
// claim: the asset resolved from the pre-image, a zeroed balance and
// is_removal=true — the combination SumClaimableBalancesAtOrBefore
// excludes.
func TestRemovalObservationShape(t *testing.T) {
	const assetKey = "USDC:" + gIssuer

	o, _ := NewObserver([]string{assetKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(7)
	obs := routeLedger(t, disp, 300, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 0x0c, "USDC", gIssuer, 42),
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0x0c, "USDC", gIssuer, 42),
	)
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1 (State emits nothing, Removed emits the removal)", len(obs))
	}
	rm := obs[0]
	if !rm.IsRemoval {
		t.Errorf("IsRemoval=false on a claim, want true")
	}
	if rm.AssetKey != assetKey {
		t.Errorf("AssetKey=%q, want %q (resolved from the pre-image STATE)", rm.AssetKey, assetKey)
	}
	if rm.Balance == nil || rm.Balance.Sign() != 0 {
		t.Errorf("Balance=%v, want 0", rm.Balance)
	}
	if rm.ClaimableID[:2] != "0c" {
		t.Errorf("ClaimableID prefix=%q, want 0c", rm.ClaimableID[:2])
	}
	// The removal must sit AFTER the create in intra-ledger order — the
	// writer's `intra_ledger_seq <= EXCLUDED` upsert guard is what makes
	// a same-ledger create+claim collapse to the removal.
	if rm.IntraLedgerSeq != 8 {
		t.Errorf("IntraLedgerSeq=%d, want 8 (position of the Removed change)", rm.IntraLedgerSeq)
	}
}

// TestServedAggregate_SameLedgerCreateThenClaim — a balance created
// and claimed inside one ledger nets to zero, and the removal carries
// the higher intra-ledger position so the writer's last-writer-wins
// upsert keeps the removal.
func TestServedAggregate_SameLedgerCreateThenClaim(t *testing.T) {
	const assetKey = "USDC:" + gIssuer

	o, _ := NewObserver([]string{assetKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	obs := routeLedger(t, disp, 400, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 0x5a, "USDC", gIssuer, 750_000),
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 0x5a, "USDC", gIssuer, 750_000),
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0x5a, "USDC", gIssuer, 750_000),
	)
	if got := servedClaimableAggregate(obs, assetKey); got.Sign() != 0 {
		t.Errorf("same-ledger create+claim: aggregate = %s, want 0", got)
	}
}

// TestServedAggregate_ReplayIsIdempotent — re-ingesting the same
// ledgers must not double-remove (the removal is an absorbing state,
// not a decrement) and must not resurrect the balance.
func TestServedAggregate_ReplayIsIdempotent(t *testing.T) {
	const assetKey = "USDC:" + gIssuer
	const amount = int64(2_500_000)

	o, _ := NewObserver([]string{assetKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	var all []Observation
	for pass := range 2 {
		seq := uint32(0)
		all = append(all, routeLedger(t, disp, 100, &seq,
			makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 0x77, "USDC", gIssuer, amount),
		)...)
		seq = 0
		all = append(all, routeLedger(t, disp, 200, &seq,
			makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 0x77, "USDC", gIssuer, amount),
			makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0x77, "USDC", gIssuer, amount),
		)...)
		if got := servedClaimableAggregate(all, assetKey); got.Sign() != 0 {
			t.Errorf("pass %d: aggregate = %s, want 0", pass, got)
		}
	}
}

// TestUnwatchedRemovalStaysUnmatched — a claim on an asset the
// operator does not watch is still skipped: no pre-image is memoized
// for it, so the removal is unattributable and must not produce a row.
func TestUnwatchedRemovalStaysUnmatched(t *testing.T) {
	o, _ := NewObserver([]string{"USDC:" + gIssuer})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	obs := routeLedger(t, disp, 500, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryState, 0x11, "EURO", gIssuer, 900),
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0x11, "EURO", gIssuer, 900),
	)
	if len(obs) != 0 {
		t.Errorf("got %d observations for an unwatched-asset claim, want 0", len(obs))
	}
}

// TestPreImageMemoIsLedgerScoped — the memo must not let a removal in
// a LATER ledger borrow a pre-image from an earlier one; every real
// removal is preceded by its own STATE change in the same ledger.
func TestPreImageMemoIsLedgerScoped(t *testing.T) {
	o, _ := NewObserver([]string{"USDC:" + gIssuer})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	seq := uint32(0)
	routeLedger(t, disp, 600, &seq,
		makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryCreated, 0x22, "USDC", gIssuer, 10),
	)
	// The removal must emit nothing AND must not do so silently. Matches
	// consults hasPreImage, which does not apply lookupPreImage's ledger
	// guard, so this is the one shape that reaches Decode unattributable
	// — and a dropped removal over-reports supply forever, because the
	// removal is the only thing that stops the claimed balance being
	// counted. It used to return (nil, nil) (cold audit 2026-08-04).
	evs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
		Ledger:         601,
		ClosedAt:       time.Unix(1_770_000_601, 0).UTC(),
		IntraLedgerSeq: 0,
		Change:         makeCBChangeOfType(t, xdr.LedgerEntryChangeTypeLedgerEntryRemoved, 0x22, "USDC", gIssuer, 10),
	})
	if len(evs) != 0 {
		t.Errorf("got %d observations for a removal with no same-ledger pre-image, want 0", len(evs))
	}
	if err == nil {
		t.Error("RouteEntryChange returned nil error — an unattributable removal of a WATCHED asset must be counted, not silently dropped")
	}
	if !errors.Is(err, ErrNotClaimable) {
		t.Errorf("err = %v, want it to wrap ErrNotClaimable", err)
	}
}
