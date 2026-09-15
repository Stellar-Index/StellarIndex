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

// The measured defect this file guards (r1, 2026-09-15).
//
// The listing's "precise" supply arm read supply_1d — a DAILY roll-up of the
// ADR-0011 supply observer — took max(bucket) with no vintage bound of any
// kind, and that reading unconditionally outranked the live lake figure. The
// roll-up's newest bucket is always a COMPLETED PREVIOUS day (its refresh
// policy's end_offset means the current day's bucket is never fully covered
// and never materialises), so the served figure was between 18 and 42 hours
// old in normal healthy operation while the envelope reported it fresh.
//
// On USDC — this index's single largest served market cap — that published
// 354,858,863.57 against 375,766,247.91 actually outstanding: 5.57% low, about
// $21M of market cap, from a number that was correct when it was taken.
//
// The obvious repair is the wrong one, and these tests are what pins that
// shut. Re-measured against Horizon's all-domain totals on the same day, the
// lake arm cannot be promoted above the observer:
//
//	asset  observer          lake              Horizon           lake err
//	BLND     114,879,423.92    128,119,614.53    114,854,773.04   +11.53%
//	PHO       77,882,787.15    199,999,999.31     77,882,787.15  +156.79%
//	USDC     376,302,129.55    375,876,536.51    375,766,247.91    +0.03%
//
// The lake over-counts BLND and PHO because its flow history carries replayed
// historical mints whose matching burns are missing, and its own completeness
// check fires only on the opposite asymmetry (a NEGATIVE net). So the arm
// order stands; what changed is that the observation is read live and bounded.
//
// observedSupply builds the observation map the serving path now receives, all
// entries stamped fresh. The store is what enforces vintage — an observation
// older than the bound is not in the map at all — so "stale" in these tests is
// expressed the way the serving path actually sees it: an absent entry.
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

// TestClassicSupplyReading_StaleObservationYieldsToLake is the load-bearing
// one: a supply observation that is too old to be served must not outrank a
// live lake reading that disagrees with it.
//
// It is expressed at the seam the serving path sees. The vintage bound lives
// in the store query, so an observation past [preciseSupplyMaxAge] never
// reaches this map — and the row must then be answered by the lake, with the
// basis saying so, rather than falling through to nothing or to the
// trustline-only floor.
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

// TestClassicSupplyReading_LiveObservationOutranksDisagreeingLake is the other
// half of the same decision, and the reason the arm order was NOT inverted to
// fix USDC. PHO's lake total is 2.57x its real supply; BLND's is 11.5% over.
// A fresh observation must win both, however loudly the lake disagrees.
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

// TestClassicSupplyReading_TrustlineFloorHoldsWithoutObservation keeps the
// pre-existing floor guard honest once the observation arm is absent: a lake
// total BELOW the trustline sum is incomplete seeding, not a smaller truth, so
// the floor wins and says it is a floor.
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

// TestClassicSupplyReading_EmptyObservationFallsThrough — an observation row
// present but carrying no value must not swallow the row. The map is keyed on
// presence, so an entry with an empty reading would otherwise shadow both lake
// arms and publish nothing.
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

// TestLatestPreciseSupply_AsksForABoundedRead is the guard against the defect
// returning by omission. The reading is only as fresh as the bound the serving
// path asks for, and the arm it replaced asked for none at all — so an
// unbounded call here is the bug, not a detail. A test that only checked the
// returned map would pass against exactly the code that shipped the $21M
// understatement.
func TestLatestPreciseSupply_AsksForABoundedRead(t *testing.T) {
	stub := &preciseSupplyStub{obs: observedSupply(map[string]string{"native": "1"})}
	s := &Server{assetsReader: stub}

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
	// The bound has to be short enough to exclude the daily roll-up this arm
	// used to read, whose newest value is never less than 18 hours old.
	if stub.gotMaxAge >= 18*time.Hour {
		t.Errorf("freshness bound = %v — too loose to exclude a day-old reading", stub.gotMaxAge)
	}
	if len(got) != 1 {
		t.Errorf("observations = %v, want the stub's single row", got)
	}
}

// TestFillRowMarketCap_PublishesTheBasisItUsed — whichever arm answered, the
// row must say which one did. A market cap whose multiplicand has no named
// provenance is a total with no traceable source.
func TestFillRowMarketCap_PublishesTheBasisItUsed(t *testing.T) {
	const asset = "FOO-GAFOO"
	price := "1.00"
	cases := []struct {
		name      string
		precise   map[string]timescale.SupplyObservation
		lake      map[string]string
		broad     map[string]string
		wantBasis supply.Basis
	}{
		{"observation", observedSupply(map[string]string{asset: "10000000"}), nil, nil, supply.BasisIssuerExclusion},
		{"lake flows", nil, map[string]string{asset: "10000000"}, nil, supply.BasisClassicLakeFlows},
		{"trustline floor", nil, nil, map[string]string{asset: "10000000"}, supply.BasisClassicTrustlineSum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{minMarketCapVolumeUSD: 1000}
			row := AssetDetail{AssetID: asset, Code: "FOO", Decimals: 7, PriceUSD: &price}
			s.fillRowMarketCap(&row, tc.precise, tc.lake, tc.broad, map[string]int{asset: 5})
			if row.CirculatingSupply == nil {
				t.Fatal("circulating_supply not published")
			}
			if row.SupplyBasis == nil {
				t.Fatal("supply_basis not published — the served figure must say which arm produced it")
			}
			if *row.SupplyBasis != string(tc.wantBasis) {
				t.Errorf("supply_basis = %q, want %q", *row.SupplyBasis, tc.wantBasis)
			}
		})
	}
}
