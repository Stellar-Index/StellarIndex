package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type headlineExclTable struct {
	rows []timescale.SourceExclusion
	err  error
}

func (e headlineExclTable) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e.rows, e.err
}

// TestGlobalHeadlineTriangulatedTierFailsClosedOnExclusions: tier 3 is what the headline reaches after tiers 1 and 2 decline an
// excluded or unjudged market, and its value carries no source list, so an in-force exclusion touching the pair, a source-wide
// one, or an unreadable set is no data rather than a fall-through.
func TestGlobalHeadlineTriangulatedTierFailsClosedOnExclusions(t *testing.T) {
	base, quote := headlinePair(t)
	const window = 5 * time.Minute
	from := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		table headlineExclTable
		serve bool
	}{
		{name: "no exclusions", serve: true},
		{name: "another pair excluded", serve: true, table: headlineExclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", Base: "native", Quote: "fiat:EUR", From: from,
		}}}},
		{name: "source-wide exclusion", table: headlineExclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: from}}}},
		{name: "pair exclusion", table: headlineExclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", Base: base.String(), Quote: quote.String(), From: from,
		}}}},
		{name: "exclusions unreadable", table: headlineExclTable{err: errors.New("db down")}},
	} {
		reader, _ := headlineCacheFixture(t, base, quote, window, "0.00723", map[string]bool{})
		reader.Exclusions = pricingguard.NewSourceExclusionGate(tc.table, pricingguard.SourceExclusionGateOptions{})
		price, _, ok, err := reader.LookupTriangulated(context.Background(), base, quote, window)
		if err != nil {
			t.Fatalf("%s: LookupTriangulated: %v", tc.name, err)
		}
		if tc.serve != (ok && price == "0.00723") || (!tc.serve && price != "") {
			t.Errorf("%s: tier 3 returned (%q, ok=%v), want served=%v", tc.name, price, ok, tc.serve)
		}
	}
}
