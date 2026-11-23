// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// premPegServer is rwaPremiumServer with USDC declared as a dollar peg (its crypto:USDC/fiat:USD market observable) and an
// optional source-exclusion table.
func premPegServer(t *testing.T, market *stubMarketHistory, excl []timescale.SourceExclusion) *v1.Server {
	t.Helper()
	bound, dir, rows := oneBoundMember()
	usdc := installPegAliasRegistry(t)
	opts := v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
		OracleHistory: &stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 2, 107000000),
		}},
		MarketHistory:      market,
		USDPeggedClassics:  []canonical.Asset{usdc},
		VerifiedCurrencies: newTestCatalogue(t),
	}
	if excl != nil {
		opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: excl}, pricingguard.SourceExclusionGateOptions{})
	}
	return v1.New(opts)
}

func getRWAPremiumEnvelope(t *testing.T, srv *v1.Server) (v1.RWAPremiumHistoryView, string) {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/rwa/premium?timeframe=all")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data  v1.RWAPremiumHistoryView `json:"data"`
		Flags struct {
			FillsWithheldReason string `json:"fills_withheld_reason"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data, env.Flags.FillsWithheldReason
}

// pegDay is USDC's own observed dollar day.
func pegDay(n int, vwap string) timescale.MarketDay {
	return premMarket("crypto:USDC", n, vwap)
}

// TestRWAPremium_ADepeggedDollarDayIsWithheldNotPriced: on day 2 USDC traded at $0.88, so the token's 1.2159 "dollars" were
// USDC, about $1.07 — par, not a 13.6% premium. The day is absent and counted, never priced against a $1 peg.
func TestRWAPremium_ADepeggedDollarDayIsWithheldNotPriced(t *testing.T) {
	id := premAssetID("USTRY", rwaGoodIssuer)
	market := &stubMarketHistory{rows: []timescale.MarketDay{
		premMarket(id, 1, "1.08"),
		premMarket(id, 2, "1.2159"),
		pegDay(1, "1.0001"),
		pegDay(2, "0.88"),
	}}
	v, _ := getRWAPremiumEnvelope(t, premPegServer(t, market, nil))
	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	s := v.Series[0]
	if len(s.Points) != 1 || !s.Points[0].T.Time().Equal(histTime(1)) {
		t.Fatalf("points = %+v, want only day 1: day 2's dollar was a depegged USDC", s.Points)
	}
	if s.PegOutsideBandDays != 1 || s.MarketWithheldDays != 0 || s.ReferenceOnlyDays != 0 {
		t.Errorf("peg/withheld/refOnly = %d/%d/%d, want 1/0/0", s.PegOutsideBandDays, s.MarketWithheldDays, s.ReferenceOnlyDays)
	}

	// Every cleared day depegged: no series, and the reason says so rather than "no overlap".
	market.rows = []timescale.MarketDay{premMarket(id, 2, "1.2159"), pegDay(2, "0.88")}
	v, _ = getRWAPremiumEnvelope(t, premPegServer(t, market, nil))
	if len(v.Series) != 0 || premiumExcluded(v, v1.RWAPremiumExcludedPegOutsideBand) != 1 {
		t.Errorf("series = %+v, excluded = %+v; want none and peg_outside_band", v.Series, v.Excluded)
	}
}

// TestRWAPremium_ADayWithNoUsablePegObservationIsWithheldNotAssumedAtPar: once USDC's own dollar market has been observed, a
// day it is silent, thin, or excluded on cannot show the token's USDC price was a dollar price, so the day is withheld and
// counted, never priced as if USDC were $1 and never reported as a depeg.
func TestRWAPremium_ADayWithNoUsablePegObservationIsWithheldNotAssumedAtPar(t *testing.T) {
	id := premAssetID("USTRY", rwaGoodIssuer)
	thin := pegDay(2, "0.5")
	thin.Trades = 0
	for name, tc := range map[string]struct {
		peg  []timescale.MarketDay
		excl []timescale.SourceExclusion
	}{
		"silent": {},
		"thin":   {peg: []timescale.MarketDay{thin}},
		"excluded": {
			peg:  []timescale.MarketDay{pegDay(2, "0.88")},
			excl: []timescale.SourceExclusion{{ID: 1, Source: "coinbase", Base: "crypto:USDC", Quote: "fiat:USD", From: histTime(2), Until: histTime(3)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			rows := append([]timescale.MarketDay{premMarket(id, 1, "1.08"), premMarket(id, 2, "1.06"), pegDay(1, "1.0001")}, tc.peg...)
			v, _ := getRWAPremiumEnvelope(t, premPegServer(t, &stubMarketHistory{rows: rows}, tc.excl))
			if len(v.Series) != 1 || len(v.Series[0].Points) != 1 || !v.Series[0].Points[0].T.Time().Equal(histTime(1)) {
				t.Fatalf("series = %+v, want one series holding only day 1", v.Series)
			}
			s := v.Series[0]
			if s.PegUnobservedDays != 1 || s.PegOutsideBandDays != 0 || s.MarketWithheldDays != 0 {
				t.Errorf("peg_unobserved/peg_outside_band/withheld = %d/%d/%d, want 1/0/0",
					s.PegUnobservedDays, s.PegOutsideBandDays, s.MarketWithheldDays)
			}
		})
	}

	// Before USDC's first observed day there was no feed to judge with: the day is not withheld on its account.
	market := &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08"), premMarket(id, 2, "1.06"), pegDay(2, "1.0")}}
	v, _ := getRWAPremiumEnvelope(t, premPegServer(t, market, nil))
	if len(v.Series) != 1 || len(v.Series[0].Points) != 2 || v.Series[0].PegUnobservedDays != 0 {
		t.Errorf("series = %+v, want both days published and none unobserved", v.Series)
	}

	// Every cleared day unobserved: no series, and the reason names it.
	market.rows = []timescale.MarketDay{premMarket(id, 2, "1.06"), pegDay(1, "1.0")}
	v, _ = getRWAPremiumEnvelope(t, premPegServer(t, market, nil))
	if len(v.Series) != 0 || premiumExcluded(v, v1.RWAPremiumExcludedPegUnobserved) != 1 {
		t.Errorf("series = %+v, excluded = %+v; want none and peg_unobserved", v.Series, v.Excluded)
	}
}

// TestRWAPremium_ARefusedDayWithNoSameDayReferenceIsNoOverlap: day 3 has a depegged dollar but no oracle value, so it could
// never have carried a premium; the member's reason is the missing overlap, not the peg.
func TestRWAPremium_ARefusedDayWithNoSameDayReferenceIsNoOverlap(t *testing.T) {
	id := premAssetID("USTRY", rwaGoodIssuer)
	market := &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 3, "1.2159"), pegDay(3, "0.88")}}
	v, _ := getRWAPremiumEnvelope(t, premPegServer(t, market, nil))
	if len(v.Series) != 0 || premiumExcluded(v, v1.RWAPremiumExcludedNoOverlap) != 1 ||
		premiumExcluded(v, v1.RWAPremiumExcludedPegOutsideBand) != 0 {
		t.Errorf("series = %+v, excluded = %+v; want none and no_same_day_observation", v.Series, v.Excluded)
	}
}

// TestRWAPremium_AnUnreadableExclusionSetServesTheLastBodyStale: the build is refused while the set is unreadable; a cached
// body keeps serving marked stale, and with none the route is a 503 with Retry-After, never an unjudged series.
func TestRWAPremium_AnUnreadableExclusionSetServesTheLastBodyStale(t *testing.T) {
	id := premAssetID("USTRY", rwaGoodIssuer)
	unread := pricingguard.NewSourceExclusionGate(exclTable{err: errors.New("exclusions unreadable")}, pricingguard.SourceExclusionGateOptions{})
	get := func(srv *v1.Server) (int, string, bool) {
		t.Helper()
		resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/rwa/premium?timeframe=all")
		defer func() { _ = resp.Body.Close() }()
		var env struct {
			Flags struct {
				Stale bool `json:"stale"`
			} `json:"flags"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp.StatusCode, resp.Header.Get("Retry-After"), env.Flags.Stale
	}

	srv := premPegServer(t, &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08")}}, nil)
	if code, _, stale := get(srv); code != http.StatusOK || stale {
		t.Fatalf("first build: status = %d stale = %v, want 200 fresh", code, stale)
	}
	srv.Exclusions = unread
	srv.AgeRWASeriesCachesForTest(time.Hour)
	if code, _, stale := get(srv); code != http.StatusOK || !stale {
		t.Errorf("unreadable set over a cached body: status = %d stale = %v, want 200 stale", code, stale)
	}

	cold := premPegServer(t, &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08")}}, nil)
	cold.Exclusions = unread
	if code, retry, _ := get(cold); code != http.StatusServiceUnavailable || retry == "" {
		t.Errorf("unreadable set with no cached body: status = %d Retry-After = %q, want 503 with Retry-After", code, retry)
	}
}

// TestRWAPremium_ASourceExcludedDayIsWithheldAndFlagged: an operator exclusion over day 2, on the token's market or on the
// oracle series it is compared with, removes that day and marks the envelope.
func TestRWAPremium_ASourceExcludedDayIsWithheldAndFlagged(t *testing.T) {
	id := premAssetID("USTRY", rwaGoodIssuer)
	for name, ex := range map[string]timescale.SourceExclusion{
		"market":              {ID: 1, Source: "sdex", Base: id, Quote: "crypto:USDC"},
		"market classic usdc": {ID: 1, Source: "sdex", Base: id, Quote: "USDC-" + pegAliasUSDCIssuer},
		"reference":           {ID: 1, Source: "redstone", Base: "rwa:USTRY", Quote: "fiat:USD"},
	} {
		t.Run(name, func(t *testing.T) {
			ex.From, ex.Until = histTime(2), histTime(3)
			market := &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08"), premMarket(id, 2, "1.06")}}
			v, reason := getRWAPremiumEnvelope(t, premPegServer(t, market, []timescale.SourceExclusion{ex}))
			if len(v.Series) != 1 || len(v.Series[0].Points) != 1 || !v.Series[0].Points[0].T.Time().Equal(histTime(1)) {
				t.Fatalf("series = %+v, want one series holding only day 1", v.Series)
			}
			if v.Series[0].SourceExcludedDays != 1 {
				t.Errorf("source_excluded_days = %d, want 1", v.Series[0].SourceExcludedDays)
			}
			if reason != string(v1.PriceWithheldSourceExcluded) {
				t.Errorf("flags.fills_withheld_reason = %q, want %q", reason, v1.PriceWithheldSourceExcluded)
			}
		})
	}
}
