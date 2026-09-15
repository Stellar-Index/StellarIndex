//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestLatestSupplyObservations_BoundsVintage is the storage-side proof of the
// defect fixed on 2026-09-15 and the one test that could have caught it.
//
// The listing's authoritative supply arm read supply_1d — a DAILY roll-up of
// asset_supply_history — with `bucket = max(bucket)` and no vintage bound at
// all. supply_1d's refresh policy carries an end_offset, so the current day's
// bucket is never fully covered by a refresh window and never materialises;
// the newest bucket is therefore always a COMPLETED PREVIOUS day, so the value
// in it is the last observation of the PREVIOUS UTC day — on r1's 6-hourly
// refresh, between about 2.9 and about 26.9 hours old, and 17 h 47 m old at
// the moment of measurement. On r1 that served USDC at 354,858,863.57 against
// 375,766,247.91 outstanding — 5.57% low, roughly $21M of market cap — while
// the response envelope reported the figure fresh.
//
// Two properties are asserted together because either alone is satisfied by
// code that still has the bug: the read returns the LATEST observation (not a
// day-old roll-up of it), and it returns NOTHING for an asset whose newest
// observation is older than the bound.
func TestLatestSupplyObservations_BoundsVintage(t *testing.T) {
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
		usdcKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcID  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		phoKey  = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		phoID   = "PHO-GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		bound   = 6 * time.Hour
	)
	now := time.Now().UTC()

	// USDC: the day-old reading that used to be served, then the live one.
	// Both are inside the bound, so the newest must win — this is the half
	// of the contract the old max(bucket) read satisfied for the WRONG day.
	mustInsert(t, ctx, store, usdcKey, "3548588635712599", now.Add(-26*time.Hour), 64_400_000)
	mustInsert(t, ctx, store, usdcKey, "3763021295452262", now.Add(-4*time.Minute), 64_443_400)

	// XLM exercises the `XLM` -> `native` key translation the listing joins on.
	mustInsert(t, ctx, store, "XLM", "348508791955885292", now.Add(-5*time.Minute), 64_443_401)

	// PHO's observer has stopped: its newest reading is outside the bound and
	// must not be offered at all, so the caller falls through to its next arm
	// instead of publishing a figure nobody is computing.
	mustInsert(t, ctx, store, phoKey, "778827871496573", now.Add(-30*time.Hour), 64_390_000)

	got, err := store.LatestSupplyObservations(ctx, bound)
	if err != nil {
		t.Fatalf("LatestSupplyObservations: %v", err)
	}

	obs, ok := got[usdcID]
	if !ok {
		t.Fatalf("USDC missing from %v — a fresh observation must be served", keysOf(got))
	}
	if obs.CirculatingSupply != "3763021295452262" {
		t.Errorf("USDC circulating = %q, want the newest observation 3763021295452262 (a stale "+
			"reading outranking a live one is the defect this test exists for)", obs.CirculatingSupply)
	}
	if obs.Basis != string(supply.BasisIssuerExclusion) {
		t.Errorf("USDC basis = %q, want %q — the arm must publish the basis the observer recorded",
			obs.Basis, supply.BasisIssuerExclusion)
	}
	if age := time.Since(obs.ObservedAt); age <= 0 || age > bound {
		t.Errorf("USDC ObservedAt = %v (age %v), want a vintage inside the bound", obs.ObservedAt, age)
	}

	if _, ok := got["native"]; !ok {
		t.Errorf("native missing from %v — XLM must translate to the listing's asset_id", keysOf(got))
	}

	if stale, ok := got[phoID]; ok {
		t.Errorf("PHO served at %q from an observation %v old, past the %v bound — "+
			"an out-of-bound reading must not be offered",
			stale.CirculatingSupply, time.Since(stale.ObservedAt).Round(time.Hour), bound)
	}

	// A non-positive bound admits nothing. That is the safe direction: the
	// caller falls back to its next arm rather than publishing an observation
	// of unknown vintage.
	if none, err := store.LatestSupplyObservations(ctx, 0); err != nil {
		t.Fatalf("LatestSupplyObservations(0): %v", err)
	} else if len(none) != 0 {
		t.Errorf("LatestSupplyObservations(0) returned %d rows, want none", len(none))
	}
}

func mustInsert(
	t *testing.T, ctx context.Context, store *timescale.Store,
	assetKey, circ string, at time.Time, ledger uint32,
) {
	t.Helper()
	n, ok := new(big.Int).SetString(circ, 10)
	if !ok {
		t.Fatalf("bad fixture supply %q", circ)
	}
	basis := supply.BasisIssuerExclusion
	if assetKey == "XLM" {
		basis = supply.BasisXLMSDFReserveExclusion
	}
	if err := store.InsertSupply(ctx, supply.Supply{
		AssetKey:          assetKey,
		TotalSupply:       n,
		CirculatingSupply: n,
		Basis:             basis,
		LedgerSequence:    ledger,
		ObservedAt:        at,
	}); err != nil {
		t.Fatalf("InsertSupply %s at %v: %v", assetKey, at, err)
	}
}

func keysOf(m map[string]timescale.SupplyObservation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
