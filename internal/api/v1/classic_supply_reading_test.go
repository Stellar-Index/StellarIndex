// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// What this file guards: the listing's "precise" supply arm reads the supply_1d roll-up, whose
// newest bucket is always a COMPLETED PREVIOUS day, so an unbounded read serves a figure hours
// old while the envelope reports it fresh, and it must not unconditionally outrank the live
// lake figure.
//
// The obvious repair, promoting the lake arm above the observer, is wrong: the lake over-counts
// some assets because its flow history carries replayed mints whose burns are missing, and its
// completeness check fires only on the opposite asymmetry (a NEGATIVE net). So the arm order
// stands and the observation is read live and bounded.
//
// observedSupply builds the observation map the serving path receives, all entries stamped
// fresh. The store enforces vintage, so "stale" here is an absent entry.
func observedSupply(circ map[string]string) map[string]timescale.SupplyObservation {
	out := make(map[string]timescale.SupplyObservation, len(circ))
	for assetID, v := range circ {
		out[assetID] = timescale.SupplyObservation{
			CirculatingSupply: v,
			Basis:             string(supply.BasisIssuerExclusion),
			ObservedAt:        time.Now(),
		}
	}
	return out
}

// TestClassicSupplyReading_StaleObservationYieldsToLake: an observation too old to serve is absent from the map (the vintage
// bound is in the store query), so the row must be answered by the lake with the basis saying so, not by nothing or the
// trustline floor.
func TestClassicSupplyReading_StaleObservationYieldsToLake(t *testing.T) {
	const (
		usdc      = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		lake      = "3758765365125485" // 375,876,536.51 — within 0.03% of Horizon
		trustline = "3186442274209846" // 318,644,227.42 — trustlines only
		stale     = "3548588635712599" // 354,858,863.57 — the day-old figure that shipped
	)
	circ, basis := classicSupplyReading(usdc,
		map[string]timescale.SupplyObservation{}, // aged out of the bounded read
		map[string]string{usdc: lake},
		map[string]string{usdc: trustline},
	)
	if circ == stale {
		t.Fatal("served the aged-out observation — a stale reading must not outrank a live one")
	}
	if circ != lake {
		t.Errorf("circulating supply = %q, want the live lake reading %q", circ, lake)
	}
	if basis != supply.BasisClassicLakeFlows {
		t.Errorf("basis = %q, want %q", basis, supply.BasisClassicLakeFlows)
	}
}

// TestClassicSupplyReading_LiveObservationOutranksDisagreeingLake: the arm order is not inverted because the lake over-counts
// some assets (PHO 2.57x, BLND 11.5%); a fresh observation must win however loudly the lake disagrees.
func TestClassicSupplyReading_LiveObservationOutranksDisagreeingLake(t *testing.T) {
	cases := []struct {
		name                       string
		assetID                    string
		observed, lake, trustline  string
		wantCirculatingSupplyIsObs bool
	}{
		{
			name:     "PHO: lake is 2.57x the real supply",
			assetID:  "PHO-GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO",
			observed: "778827871496573",  // 77,882,787.15 — matches Horizon to the stroop
			lake:     "1999999993050205", // 199,999,999.31 — one replayed mint, no matching burns
			// the trustline floor is itself above nothing here; it must not win either
			trustline:                  "766002451766903",
			wantCirculatingSupplyIsObs: true,
		},
		{
			name:                       "BLND: lake is 11.5% over",
			assetID:                    "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY",
			observed:                   "1148794239246518", // 114,879,423.92
			lake:                       "1281196145286920", // 128,119,614.53
			trustline:                  "1077820838484513",
			wantCirculatingSupplyIsObs: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			circ, basis := classicSupplyReading(tc.assetID,
				observedSupply(map[string]string{tc.assetID: tc.observed}),
				map[string]string{tc.assetID: tc.lake},
				map[string]string{tc.assetID: tc.trustline},
			)
			if circ != tc.observed {
				t.Errorf("circulating supply = %q, want the observation %q", circ, tc.observed)
			}
			// The observation publishes the observer's OWN basis, the same
			// one /v1/assets/{asset_id} publishes for this asset — not a
			// listing-only synonym for it.
			if basis != supply.BasisIssuerExclusion {
				t.Errorf("basis = %q, want the observer's own %q", basis, supply.BasisIssuerExclusion)
			}
		})
	}
}

// TestClassicSupplyReading_TrustlineFloorHoldsWithoutObservation: a lake total below the trustline sum is incomplete seeding, so
// with no observation the floor wins and says it is a floor.
func TestClassicSupplyReading_TrustlineFloorHoldsWithoutObservation(t *testing.T) {
	const asset = "FOO-GAFOO"
	circ, basis := classicSupplyReading(asset,
		nil,
		map[string]string{asset: "3"},
		map[string]string{asset: "1000000000"},
	)
	if circ != "1000000000" {
		t.Errorf("circulating supply = %q, want the trustline floor", circ)
	}
	if basis != supply.BasisClassicTrustlineSum {
		t.Errorf("basis = %q, want %q — a floor that does not say so reads as a total",
			basis, supply.BasisClassicTrustlineSum)
	}
}

// TestClassicSupplyReading_NoArmAnswers — no reading at all yields no value
// AND no basis. A basis on an empty value would name provenance for a number
// that was never published.
func TestClassicSupplyReading_NoArmAnswers(t *testing.T) {
	circ, basis := classicSupplyReading("FOO-GAFOO", nil, nil, nil)
	if circ != "" || basis != "" {
		t.Errorf("classicSupplyReading = (%q, %q), want empty", circ, basis)
	}
}

// TestClassicSupplyReading_EmptyObservationFallsThrough: the map is keyed on presence, so an entry with an empty reading must
// not shadow both lake arms and publish nothing.
func TestClassicSupplyReading_EmptyObservationFallsThrough(t *testing.T) {
	const asset = "FOO-GAFOO"
	circ, basis := classicSupplyReading(asset,
		map[string]timescale.SupplyObservation{asset: {Basis: "issuer_exclusion", ObservedAt: time.Now()}},
		map[string]string{asset: "1366050000"},
		nil,
	)
	if circ != "1366050000" || basis != supply.BasisClassicLakeFlows {
		t.Errorf("classicSupplyReading = (%q, %q), want the lake reading", circ, basis)
	}
}

// preciseSupplyStub records the freshness bound it was asked for.
type preciseSupplyStub struct {
	AssetsReader // nil embedded — only the supply seam is exercised
	gotMaxAge    time.Duration
	calls        int
	obs          map[string]timescale.SupplyObservation
}

func (p *preciseSupplyStub) LatestSupplyObservations(
	_ context.Context, maxAge time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	p.gotMaxAge = maxAge
	p.calls++
	return p.obs, nil
}

// TestLatestPreciseSupply_AsksForABoundedRead guards against the defect returning by omission.
// The reading is only as fresh as the bound the serving path asks for, so an unbounded call is
// the bug; a test that only checked the returned map would pass against the unbounded code.
func TestLatestPreciseSupply_AsksForABoundedRead(t *testing.T) {
	stub := &preciseSupplyStub{obs: observedSupply(map[string]string{"native": "1"})}
	s := &Server{Options: Options{AssetsReader: stub}}

	got := s.latestPreciseSupply(context.Background())

	if stub.calls != 1 {
		t.Fatalf("supply seam called %d times, want 1 — the arm resolved to a different reader", stub.calls)
	}
	if stub.gotMaxAge <= 0 {
		t.Errorf("freshness bound = %v, want a positive bound", stub.gotMaxAge)
	}
	if stub.gotMaxAge != preciseSupplyMaxAge {
		t.Errorf("freshness bound = %v, want preciseSupplyMaxAge (%v)", stub.gotMaxAge, preciseSupplyMaxAge)
	}
	// The bound has to be short enough to exclude the daily roll-up, which this arm
	// must not read. That roll-up's newest value is the last observation of the
	// previous UTC day, so it spends most of its serving life past twelve
	// hours old and reaches about twenty-seven.
	if stub.gotMaxAge >= 12*time.Hour {
		t.Errorf("freshness bound = %v — too loose to exclude a day-old reading", stub.gotMaxAge)
	}
	if len(got) != 1 {
		t.Errorf("observations = %v, want the stub's single row", got)
	}
}

// TestStampCirculatingSupplyNeverOverwrites covers the guard: dust-suppressed and ticker-collision paths attach a supply
// deliberately first, and a later arm must not replace it. The basis is asserted too, since value and name travel together.
func TestStampCirculatingSupplyNeverOverwrites(t *testing.T) {
	existing := "42"
	row := AssetDetail{CirculatingSupply: &existing}

	stampCirculatingSupply(&row, "999", supply.BasisClassicTrustlineSum)

	if *row.CirculatingSupply != "42" {
		t.Errorf("circulating_supply = %q, want the value already attached", *row.CirculatingSupply)
	}
	if row.SupplyBasis != nil {
		t.Errorf("supply_basis = %q, want absent — no value was stamped", *row.SupplyBasis)
	}
}

// TestStampCirculatingSupplyRefusesAnUnnamedArm: an empty basis is not a claim. The value is published either way; the name is
// omitted rather than written as "".
func TestStampCirculatingSupplyRefusesAnUnnamedArm(t *testing.T) {
	var row AssetDetail

	stampCirculatingSupply(&row, "1000000000", "")

	if row.CirculatingSupply == nil || *row.CirculatingSupply != "1000000000" {
		t.Fatalf("circulating_supply = %v, want the value published", row.CirculatingSupply)
	}
	if row.SupplyBasis != nil {
		t.Errorf("supply_basis = %q, want absent", *row.SupplyBasis)
	}
}
