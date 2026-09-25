// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"slices"
	"testing"
)

// RLT-337 F3 — the catalogue row is priced on its own, so the twin's
// scam suppression has to be CARRIED, not merely performed.
//
// A catalogue row carries no issuer (projectCatalogueRow sets none, and
// `type: "global"` rows are documented as issuer-less), so
// fillIssuerDirectoryTags skips it and the suppression that call
// performs never reaches it. That would be harmless if the row's only
// money came from its twin — but fillCataloguePricesForPage runs BEFORE
// fillCatalogueStatsForPage and fills price_usd / market_cap_usd from
// buildGlobalAssetView's global tier, and mergeTwinStats fills only what
// is nil. So a flagged issuer's verified currency served a price and a
// market cap on the catalogue phase of /v1/assets, on
// /v1/assets?asset_class=…, and on /v1/external/assets, while the
// classic row that suppressCatalogueTwins throws away in its favour —
// and its own detail page — served null.
func TestMergeTwinStats_CarriesTheScamSuppressionOntoTheCatalogueRow(t *testing.T) {
	t.Parallel()

	ownPrice, ownCap, ownFDV := "1.07", "109504500.00", "200000000.00"
	change := "4.2"
	// The catalogue row as fillCataloguePricesForPage leaves it: priced
	// from the global tier, with no issuer and no directory verdict.
	dst := AssetDetail{
		Type:         assetTypeGlobal,
		AssetID:      "some-currency",
		Slug:         "some-currency",
		PriceUSD:     &ownPrice,
		MarketCapUSD: &ownCap,
		FDVUSD:       &ownFDV,
		Change24hPct: &change,
	}
	// The twin, after fillIssuerDirectoryTags stamped it and
	// suppressScamIssuerPricing emptied it.
	twin := AssetDetail{
		IssuerDirectoryTags:   []string{"malicious", "unsafe"},
		IssuerDirectoryDomain: "audrev-stellar.com",
		IssuerDirectoryName:   "AUD Revolution",
		IssuerScamReason:      "wash-inflated volume; issuer impersonates a regulated anchor",
	}

	mergeTwinStats(&dst, twin)

	if dst.PriceUSD != nil {
		t.Errorf("price_usd = %q, want withheld — the issuer carries a scam-class directory tag",
			*dst.PriceUSD)
	}
	if dst.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want withheld — the same tag nulled it on the classic row "+
			"this catalogue row replaces", *dst.MarketCapUSD)
	}
	if dst.FDVUSD != nil {
		t.Errorf("fdv_usd = %q, want withheld", *dst.FDVUSD)
	}
	if dst.Change24hPct != nil {
		t.Errorf("change_24h_pct = %q, want withheld — it is the price over time", *dst.Change24hPct)
	}
	// The row goes silent WITH its reason, never without it: a null price
	// and no warning reads as "no data" rather than "refused".
	if !slices.Contains(dst.IssuerDirectoryTags, "malicious") {
		t.Errorf("issuer_directory_tags = %v, want the twin's verdict carried across",
			dst.IssuerDirectoryTags)
	}
	if dst.IssuerDirectoryDomain != twin.IssuerDirectoryDomain {
		t.Errorf("issuer_directory_domain = %q, want %q",
			dst.IssuerDirectoryDomain, twin.IssuerDirectoryDomain)
	}
	if dst.IssuerScamReason != twin.IssuerScamReason {
		t.Errorf("issuer_scam_reason = %q, want %q", dst.IssuerScamReason, twin.IssuerScamReason)
	}
}

// The other half of the same rule: an issuer the directory merely
// LABELS keeps every figure. The suppression is scoped to the
// scam-class tags, and a fix that quietly nulled an anchor's price
// would be a worse defect than the one it replaced.
func TestMergeTwinStats_AnUnflaggedIssuerKeepsItsFigures(t *testing.T) {
	t.Parallel()

	ownPrice, ownCap := "1.00", "5000000.00"
	dst := AssetDetail{
		Type:         assetTypeGlobal,
		Slug:         "some-currency",
		PriceUSD:     &ownPrice,
		MarketCapUSD: &ownCap,
	}
	twin := AssetDetail{
		IssuerDirectoryTags:   []string{"anchor", "issuer"},
		IssuerDirectoryDomain: "circle.com",
		IssuerDirectoryName:   "Circle",
	}

	mergeTwinStats(&dst, twin)

	if dst.PriceUSD == nil || *dst.PriceUSD != ownPrice {
		t.Errorf("price_usd = %v, want %q kept", dst.PriceUSD, ownPrice)
	}
	if dst.MarketCapUSD == nil || *dst.MarketCapUSD != ownCap {
		t.Errorf("market_cap_usd = %v, want %q kept", dst.MarketCapUSD, ownCap)
	}
	if !slices.Contains(dst.IssuerDirectoryTags, "anchor") {
		t.Errorf("issuer_directory_tags = %v, want the twin's labels", dst.IssuerDirectoryTags)
	}
}

// TestCataloguePricesArePaidBeforeTheTwinMerge — the ORDER the
// suppression above depends on, guarded at source in the style of
// TestDirectoryTagsPrecedeTheListingValuationArm and for the same
// reason: each function is correct read alone.
//
// fillCataloguePricesForPage is what gives a catalogue row a price of
// its own, and mergeTwinStats (inside fillCatalogueStatsForPage) is
// where the twin's verdict reaches it. Running the price fill AFTER the
// stats merge would republish the exact figure the tag just withheld,
// and nothing behavioural would see it.
func TestCataloguePricesArePaidBeforeTheTwinMerge(t *testing.T) {
	const (
		prices = "fillCataloguePricesForPage"
		stats  = "fillCatalogueStatsForPage"
	)
	checked := 0
	for _, file := range packageGoFiles(t) {
		parsed := parseGoFile(t, file)
		for _, fn := range funcNamesIn(parsed) {
			calls := serverCallsInFile(parsed, fn)
			priceAt := slices.Index(calls, prices)
			statsAt := slices.Index(calls, stats)
			if priceAt < 0 || statsAt < 0 {
				continue
			}
			checked++
			if priceAt > statsAt {
				t.Errorf("%s:%s calls %s at step %d but %s only at step %d — the catalogue row's "+
					"own price would be filled after the twin's scam suppression carried across, "+
					"republishing the figure the tag withheld.\ncalls: %v",
					file, fn, prices, priceAt, stats, statsAt, calls)
			}
		}
	}
	// Both serving paths — writeCataloguePage (the class-scoped listings
	// and /v1/external/assets) and serveCatalogueUnifiedPage (the unified
	// catalogue phase) — make both calls through fillAndRankCatalogueRows,
	// which TestCatalogueRankFollowsTheFill pins them to. A guard that
	// matched nothing would pass over an empty set forever.
	if checked < 1 {
		t.Fatalf("the guard examined %d function(s) that make both calls; "+
			"fillAndRankCatalogueRows is known to. Either it lost its price fill or this "+
			"guard stopped reading the package", checked)
	}
}

// TestCatalogueRankFollowsTheFill — the catalogue's market-cap rank is
// only meaningful over caps that have been written. Stellar-issued rows
// get theirs from the price fill and the twin merge, so a rank taken
// before both ordered every such row on nil — seed order — and a slice
// taken before the fill made a late-seeded large asset unreachable.
//
// fillAndRankCatalogueRows is the one place the rank may run, after both
// fills, and each catalogue page writer must go through it rather than
// sorting or filling on its own.
func TestCatalogueRankFollowsTheFill(t *testing.T) {
	const (
		chokepoint = "fillAndRankCatalogueRows"
		sortFn     = "sortAssetDetailsByMarketCapDesc"
	)
	writers := map[string]bool{"writeCataloguePage": false, "serveCatalogueUnifiedPage": false}
	for _, file := range packageGoFiles(t) {
		parsed := parseGoFile(t, file)
		for _, fn := range funcNamesIn(parsed) {
			calls := plainCallsInFile(parsed, fn)
			if fn == chokepoint {
				sortAt := slices.Index(calls, sortFn)
				if sortAt < 0 {
					t.Errorf("%s no longer ranks the rows; calls: %v", chokepoint, calls)
				}
				for _, fill := range []string{"fillCataloguePricesForPage", "fillCatalogueStatsForPage"} {
					if at := slices.Index(calls, fill); at < 0 || at > sortAt {
						t.Errorf("%s ranks at step %d but calls %s at step %d; calls: %v",
							chokepoint, sortAt, fill, at, calls)
					}
				}
				continue
			}
			if slices.Contains(calls, sortFn) {
				t.Errorf("%s:%s ranks catalogue rows itself; rank through %s so the rank "+
					"follows the fill", file, fn, chokepoint)
			}
			if _, ok := writers[fn]; ok {
				writers[fn] = slices.Contains(calls, chokepoint)
			}
		}
	}
	for fn, ok := range writers {
		if !ok {
			t.Errorf("%s does not rank through %s", fn, chokepoint)
		}
	}
}

// plainCallsInFile returns, in source order, the names of both the
// `s.<name>(…)` method calls and the bare `<name>(…)` function calls made
// in one function of one parsed file.
func plainCallsInFile(parsed *ast.File, fn string) []string {
	var out []string
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				out = append(out, f.Name)
			case *ast.SelectorExpr:
				if x, ok := f.X.(*ast.Ident); ok && x.Name == "s" {
					out = append(out, f.Sel.Name)
				}
			}
			return true
		})
	}
	return out
}

// TestMarketCapRankTreatsZeroAsAbsent — computeMarketCapUSD emits "0.00"
// for a zero-supply asset; ranked as a real figure it put a fully burned
// asset above every asset whose cap is merely unknown.
func TestMarketCapRankTreatsZeroAsAbsent(t *testing.T) {
	zero, sized := "0.00", "5.00"
	rows := []AssetDetail{
		{Slug: "unknown-a"},
		{Slug: "burned", MarketCapUSD: &zero},
		{Slug: "unknown-b"},
		{Slug: "sized", MarketCapUSD: &sized},
	}
	sortAssetDetailsByMarketCapDesc(rows)
	got := make([]string, len(rows))
	for i := range rows {
		got[i] = rows[i].Slug
	}
	want := []string{"sized", "unknown-a", "burned", "unknown-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("rank = %v, want %v — a zero cap must keep its seed position among the "+
			"absent caps, not outrank them", got, want)
	}
}
