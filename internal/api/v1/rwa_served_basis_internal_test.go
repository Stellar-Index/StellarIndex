// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func rwaBothBasesRow(status, marketCap, reference string) RWAAsset {
	var a RWAAsset
	a.Valuation.MarketCapUSD = &marketCap
	a.ReferenceValuation.ValueUSD = &reference
	a.Premium.Status = status
	return a
}

// TestRWABothBases_CountsOnlyPublishedPremiums: a row whose premium was refused (listing price,
// outside band) carries both figures, but its gap is a comparison the methodology forbids; adding
// it would serve an aggregate discount the published premiums do not show.
func TestRWABothBases_CountsOnlyPublishedPremiums(t *testing.T) {
	got := rwaBothBases([]RWAAsset{
		rwaBothBasesRow(RWAPremiumPublished, "100000", "100000"),
		rwaBothBasesRow(RWAPremiumReferenceNotOracle, "50000", "80000"),
		rwaBothBasesRow(RWAPremiumMarketOutsideBand, "200000", "100000"),
	})
	if got.Assets != 1 {
		t.Fatalf("both_bases.assets = %d, want 1 (only the published comparison)", got.Assets)
	}
	if m, r := optStr(got.MarketCapUSD), optStr(got.ReferenceValueUSD); m != "100000.00" || r != "100000.00" {
		t.Errorf("both_bases = %s / %s, want 100000.00 / 100000.00", m, r)
	}
	if got.LowerBound {
		t.Error("both_bases.lower_bound = true, want false: no summed row has a floor supply")
	}
}

// TestRWABothBases_FloorSupplyMakesBothTotalsFloors: both figures scale with the row's supply, so a
// floor supply makes both sums floors; a floor on a row left out of the sum must not.
func TestRWABothBases_FloorSupplyMakesBothTotalsFloors(t *testing.T) {
	floored := rwaBothBasesRow(RWAPremiumPublished, "40000000", "41000000")
	floored.CirculatingSupplyLowerBound = true
	excluded := rwaBothBasesRow(RWAPremiumReferenceNotOracle, "1", "2")
	excluded.CirculatingSupplyLowerBound = true
	if got := rwaBothBases([]RWAAsset{rwaBothBasesRow(RWAPremiumPublished, "100", "100"), floored}); !got.LowerBound {
		t.Errorf("both_bases = %+v, want lower_bound=true over a floor-supply row", got)
	}
	if got := rwaBothBases([]RWAAsset{rwaBothBasesRow(RWAPremiumPublished, "100", "100"), excluded}); got.LowerBound {
		t.Errorf("both_bases = %+v, want lower_bound=false: the floor row is not summed", got)
	}
}

// TestRWAHistory_StatesTheMembershipItWasBuiltOn: a fresh series assembled on an old set whose rebuilds keep failing must
// date the set at its own build and carry its degraded state, as /v1/rwa/assets does for the same set.
func TestRWAHistory_StatesTheMembershipItWasBuiltOn(t *testing.T) {
	decided := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	s := &Server{AssetsReader: struct{ AssetsReader }{}, OracleHistory: struct{ RWAOracleHistoryReader }{}}
	s.rwaCache = &rwaMembership{available: true, builtAt: decided, refusals: map[string]int{}}
	s.rwaAt = time.Now()
	s.rwaFailedAt = time.Now().Add(-time.Minute)

	h := s.buildRWAValueHistory(context.Background())
	if !h.available {
		t.Fatal("history unavailable; the fixture no longer reaches the served path")
	}
	s.rwaHistCache, s.rwaHistAt = &h, time.Now()

	rec := httptest.NewRecorder()
	s.handleRWAHistory(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/history", nil))
	var body struct {
		Data struct {
			MembershipAsOf time.Time `json:"membership_as_of"`
		} `json:"data"`
		Flags Flags `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d, body %s: %v", rec.Code, rec.Body.String(), err)
	}
	if !body.Data.MembershipAsOf.Equal(decided) {
		t.Errorf("membership_as_of = %s, want the set's own build %s", body.Data.MembershipAsOf, decided)
	}
	// Degraded never reaches the body; it is served as no-store.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store while the membership's rebuild is failing", cc)
	}
}

// TestRWAPremiumDay_ServesEveryDigitOfBothLegs: a 14-dp oracle close and a VWAP with more than 10 fractional digits must
// reconcile digit for digit with the feed and the market leg they came from.
func TestRWAPremiumDay_ServesEveryDigitOfBothLegs(t *testing.T) {
	ref, _ := new(big.Rat).SetString("1.00012345678912")
	day, ok := rwaPremiumDayOf(timescale.MarketDay{VWAP: "1.000987654321098", VolumeUSD: "5000"}, ref)
	if !ok {
		t.Fatal("day refused")
	}
	if day.reference != "1.00012345678912" || day.market != "1.000987654321098" {
		t.Errorf("reference %s, market %s; want 1.00012345678912 and 1.000987654321098", day.reference, day.market)
	}
	if got := rwaExactDecimal(big.NewRat(1, 3)); got != "0.3333333333" {
		t.Errorf("non-terminating value = %s, want 0.3333333333", got)
	}
}

// rwaAgedListing answers every listing page from a cache entry of a fixed age, flagged stale as the catalogue does past its TTL.
type rwaAgedListing struct {
	AssetsReader
	age time.Duration
}

func (r rwaAgedListing) ListAssetsExtAt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, time.Time, bool, error) {
	return nil, time.Now().Add(-r.age), true, nil
}

// TestRWAListingRows_ExpiredPageIsNotAFailedRead: the prewarm reads pages older than the catalogue TTL on every rebuild; only a
// page that has outlived the view is a read that did not answer.
func TestRWAListingRows_ExpiredPageIsNotAFailedRead(t *testing.T) {
	m := rwaMembership{members: []rwaMember{{issuer: "GISSUER", code: "CETES"}}}
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{5 * time.Minute, false}, {rwaAssetsViewTTL + time.Minute, true}} {
		s := &Server{AssetsReader: rwaAgedListing{age: tc.age}}
		_, join, err := s.rwaListingRows(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		if join.unanswered != tc.want {
			t.Errorf("page aged %s: unanswered = %v, want %v", tc.age, join.unanswered, tc.want)
		}
	}
}

func optStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
