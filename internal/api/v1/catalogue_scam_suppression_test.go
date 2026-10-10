// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

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
	checked := 0
	for _, file := range packageGoFiles(t) {
		parsed := parseGoFile(t, file)
		for _, fn := range funcNamesIn(parsed) {
			steps := serverStepsInFile(parsed, fn)
			priceAt, statsAt, ok := cataloguePriceFillOrder(steps)
			if !ok {
				continue
			}
			checked++
			if priceAt < 0 || priceAt > statsAt {
				t.Errorf("%s:%s calls %s at step %d but %s only at step %d — the catalogue row's "+
					"own price would be filled after the twin's scam suppression carried across, "+
					"republishing the figure the tag withheld.\nsteps: %v",
					file, fn, cataloguePriceFill, priceAt, catalogueStatsFill, statsAt, steps)
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

const (
	cataloguePriceFill = "fillCataloguePricesForPage"
	catalogueStatsFill = "fillCatalogueStatsForPage"
)

// cataloguePriceFillOrder returns the step of the first price-fill CALL and
// of the first stats-merge reference; ok is false unless steps reach both.
// The price fill counts only as a call: a method value taken early and
// called after the merge (p := s.fill…; s.stats(); p()) still republishes.
func cataloguePriceFillOrder(steps []serverStep) (priceAt, statsAt int, ok bool) {
	names := stepNames(steps, false)
	if !slices.Contains(names, cataloguePriceFill) || !slices.Contains(names, catalogueStatsFill) {
		return -1, -1, false
	}
	return slices.Index(steps, serverStep{name: cataloguePriceFill, call: true}), slices.Index(names, catalogueStatsFill), true
}

// TestCataloguePriceFillOrderCountsCallsOnly: an early method value of the
// price fill must not stand in for the call that runs after the merge.
func TestCataloguePriceFillOrderCountsCallsOnly(t *testing.T) {
	cases := []struct {
		name, src string
		wantOK    bool
	}{
		{"fill before merge", `func (s *Server) f() { s.fillCataloguePricesForPage(ctx); s.fillCatalogueStatsForPage(ctx) }`, true},
		{"early method value, call after merge", `func (s *Server) f() { p := s.fillCataloguePricesForPage; s.fillCatalogueStatsForPage(ctx); s.fillCataloguePricesForPage(ctx) }`, false},
		{"method value only, called after merge", `func (s *Server) f() { p := s.fillCataloguePricesForPage; s.fillCatalogueStatsForPage(ctx); p(ctx) }`, false},
	}
	for _, c := range cases {
		parsed, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package v1\n"+c.src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		priceAt, statsAt, ok := cataloguePriceFillOrder(serverStepsInFile(parsed, "f"))
		if !ok {
			t.Fatalf("%s: the scan did not see both steps", c.name)
		}
		if got := priceAt >= 0 && priceAt < statsAt; got != c.wantOK {
			t.Errorf("%s: priceAt=%d statsAt=%d ordered=%v, want %v", c.name, priceAt, statsAt, got, c.wantOK)
		}
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
