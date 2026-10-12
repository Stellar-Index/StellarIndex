// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The backfill refresh set must cover every SERVED rung: a granularity missing from [CAGGsLiveForever] leaves
// a permanent hole in backfilled ranges. The tests derive the served set from [AllHistoryGranularities] and
// source consts, so a new granularity cannot reach the API without joining the refresh set.

// TestDeclaredGranularityConstsAreAllServed requires every package-level `const ... HistoryGranularity` to
// appear in [AllHistoryGranularities]; one that skips the slice fails Validate, so callers get a 400 for a
// declared granularity.
func TestDeclaredGranularityConstsAreAllServed(t *testing.T) {
	t.Parallel()
	declared := declaredGranularityConsts(t)
	if len(declared) == 0 {
		t.Fatal("found zero HistoryGranularity constants in this package's source — " +
			"the declaration shape changed under this guard's feet, and it is now " +
			"asserting nothing")
	}
	for name, value := range declared {
		if err := HistoryGranularity(value).Validate(); err != nil {
			t.Errorf("const %s = %q is declared but absent from AllHistoryGranularities, "+
				"so Validate rejects it (%v) — add it to the slice, and to CAGGsLiveForever "+
				"with it, or delete the constant", name, value, err)
		}
	}
	// And the reverse: nothing in the slice may be a bare string with
	// no constant behind it, which is how a typo'd rung gets served.
	byValue := map[string]bool{}
	for _, v := range declared {
		byValue[v] = true
	}
	for _, g := range AllHistoryGranularities {
		if !byValue[string(g)] {
			t.Errorf("AllHistoryGranularities holds %q, which no HistoryGranularity constant "+
				"in this package declares", g)
		}
	}
}

// declaredGranularityConsts parses this package's non-test sources and returns {const name: wire value} for
// every package-level HistoryGranularity constant, syntactically. A grouped const block carries the type
// forward as the language does.
func declaredGranularityConsts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the timescale package directory: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		collectGranularityConsts(file, out)
	}
	return out
}

// collectGranularityConsts adds every package-level HistoryGranularity string const in a file to out. It
// follows Go's implicit repetition: a spec with no expression list inherits the previous list and its type; a
// spec with its own value inherits nothing.
func collectGranularityConsts(file *ast.File, out map[string]string) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// The last non-empty expression list in this group and the type
		// declared alongside it — what a spec omitting its own list
		// repeats.
		var (
			carriedType   string
			carriedValues []ast.Expr
		)
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			values := vs.Values
			if len(vs.Values) > 0 {
				carriedType = ""
				if id, isIdent := vs.Type.(*ast.Ident); isIdent {
					carriedType = id.Name
				}
				carriedValues = vs.Values
			} else {
				values = carriedValues
			}
			if carriedType != "HistoryGranularity" {
				continue
			}
			for i, ident := range vs.Names {
				if i >= len(values) {
					continue
				}
				lit, isLit := values[i].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out[ident.Name] = value
			}
		}
	}
}

// TestCollectGranularityConstsFollowsTheLanguagesTypeCarrying pins [collectGranularityConsts] on synthetic
// source. Go carries a type only into a spec that omits its expression list, so an untyped constant with its
// own value in the same group must not read as a granularity; the first case ensures carrying still works.
func TestCollectGranularityConstsFollowsTheLanguagesTypeCarrying(t *testing.T) {
	t.Parallel()
	const src = `package timescale

const (
	Granularity1m  HistoryGranularity = "1m"
	Granularity15m                    = "15m"
	granularityDocsURL                = "https://example.invalid/granularity"
	maxGranularityLabelLen            = 3
	Granularity1h  HistoryGranularity = "1h"
	Granularity1hAlias
	someOtherWireValue OtherEnum = "1d"
)

const loneUntypedConst = "5m"
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	got := map[string]string{}
	collectGranularityConsts(file, got)

	want := map[string]string{
		// Declares the type outright.
		"Granularity1m": "1m",
		"Granularity1h": "1h",
		// Omits BOTH type and value, so it repeats the spec above it —
		// type included. This is the shape the collector exists to
		// follow, and dropping it would silently lose a rung.
		"Granularity1hAlias": "1h",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("collected %s = %q, want %q — a granularity the language types is missing "+
				"from the guard's view, so it could be added without joining the refresh set",
				name, got[name], value)
		}
	}
	// Everything else in that group carries its own value, so the
	// language types none of them from the spec above.
	for _, name := range []string{
		// Own value, no type: an untyped string whatever the spec above says. A real rung written
		// this way goes unseen here; TestDeclaredGranularityConstsAreAllServed's reverse check
		// catches it because the slice would hold a value no typed constant declares.
		"Granularity15m",
		"granularityDocsURL",     // the reproduction: unrelated, same group
		"maxGranularityLabelLen", // unrelated and not even a string
		"someOtherWireValue",     // a different named type
		"loneUntypedConst",       // a different declaration entirely
	} {
		if value, found := got[name]; found {
			t.Errorf("collected %s = %q, but nothing declares it a HistoryGranularity — the guard "+
				"would report an ordinary constant as a served rung that is missing from "+
				"AllHistoryGranularities", name, value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("collected %d constants, want exactly %d: %v", len(got), len(want), got)
	}
}

// TestCAGGsLiveForeverCoversEveryServedGranularity is the assertion
// that matters: every granularity the API serves must be refreshed by
// the backfill, or that grain is permanently empty over the range.
func TestCAGGsLiveForeverCoversEveryServedGranularity(t *testing.T) {
	t.Parallel()
	inSet := map[string]CAGGSpec{}
	for _, spec := range CAGGsLiveForever {
		if _, dup := inSet[spec.Name]; dup {
			t.Errorf("CAGGsLiveForever lists %s twice", spec.Name)
		}
		inSet[spec.Name] = spec
	}
	for _, g := range AllHistoryGranularities {
		view := "prices_" + string(g)
		if _, ok := inSet[view]; !ok {
			t.Errorf("granularity %q is served (OHLCSeries / HistoryPointsInRange read %s) but the "+
				"backfill never refreshes %s — every backfilled range keeps a permanent hole at that "+
				"resolution", g, view, view)
		}
	}
	// The reverse direction: a view in the set that no read serves
	// would be work with no reader, and a name RefreshContinuousAggregate
	// would reject at runtime rather than at build time.
	for _, spec := range CAGGsLiveForever {
		if !allowedCAGGViews[spec.Name] {
			t.Errorf("CAGGsLiveForever lists %s, which RefreshContinuousAggregate's allow-list rejects — "+
				"every chunk's refresh of it would fail", spec.Name)
		}
		g := HistoryGranularity(strings.TrimPrefix(spec.Name, "prices_"))
		if err := g.Validate(); err != nil {
			t.Errorf("CAGGsLiveForever refreshes %s, which maps to no served granularity: %v", spec.Name, err)
		}
	}
}

// TestCAGGsLiveForeverRefreshesPrices1mFirst pins the ordering convention. No member reads prices_1m today,
// so it is defensive: it is the only view another aggregate builds on, so a future rung reading it finds it
// current (as chops.xlmBaseRestampCAGGs).
func TestCAGGsLiveForeverRefreshesPrices1mFirst(t *testing.T) {
	t.Parallel()
	if len(CAGGsLiveForever) == 0 {
		t.Fatal("CAGGsLiveForever is empty")
	}
	if got := CAGGsLiveForever[0].Name; got != "prices_1m" {
		t.Errorf("first refreshed view is %s, want prices_1m — it is the view the hierarchical TWAP "+
			"aggregates are built on, so refreshing it late would rebuild them from stale input", got)
	}
}

// TestCAGGsLiveForeverMinWindowsClearTimescaleMinimum checks each entry against the rule that
// produced it: refresh_continuous_aggregate rejects a window narrower than 2x the bucket width
// (SQLSTATE 22023), which would ship a partially-stale surface rather than fail loudly.
func TestCAGGsLiveForeverMinWindowsClearTimescaleMinimum(t *testing.T) {
	t.Parallel()
	for _, spec := range CAGGsLiveForever {
		g := HistoryGranularity(strings.TrimPrefix(spec.Name, "prices_"))
		bucket := g.BucketDuration()
		if bucket == 0 {
			t.Errorf("%s maps to no bucket duration", spec.Name)
			continue
		}
		if spec.MinWindow < 2*bucket {
			t.Errorf("%s MinWindow %v is under Timescale's 2-bucket minimum (%v) — the refresh call "+
				"would be rejected with SQLSTATE 22023", spec.Name, spec.MinWindow, 2*bucket)
		}
	}
}

// TestPadRefreshWindowCoversTheChunkAtEveryRung proves the fine rungs are refreshed over the same range as
// the coarse ones: for a chunk shorter than any minimum, every entry's padded window must still contain the
// chunk end to end.
func TestPadRefreshWindowCoversTheChunkAtEveryRung(t *testing.T) {
	t.Parallel()
	from := time.Date(2024, 3, 12, 9, 0, 0, 0, time.UTC)
	to := from.Add(90 * time.Second) // shorter than every MinWindow
	for _, spec := range CAGGsLiveForever {
		padFrom, padTo := PadRefreshWindow(from, to, spec.MinWindow)
		if padFrom.After(from) || padTo.Before(to) {
			t.Errorf("%s padded window [%s, %s] does not cover the chunk [%s, %s]",
				spec.Name, padFrom, padTo, from, to)
		}
		if got := padTo.Sub(padFrom); got < spec.MinWindow {
			t.Errorf("%s padded to %v, want >= its MinWindow %v", spec.Name, got, spec.MinWindow)
		}
	}
}

// TestHistoryGranularityListMatchesTheServedSet pins the generated enumeration to the slice. The
// 400 bodies for /v1/chart and /v1/history/since-inception render it, so a list that omits a
// served grain (or advertises one Validate rejects) tells callers the wrong contract.
func TestHistoryGranularityListMatchesTheServedSet(t *testing.T) {
	t.Parallel()
	listed := strings.Split(HistoryGranularityList(), historyGranularitySep)
	if len(listed) != len(AllHistoryGranularities) {
		t.Fatalf("HistoryGranularityList() renders %d entries, AllHistoryGranularities holds %d",
			len(listed), len(AllHistoryGranularities))
	}
	for i, raw := range listed {
		if HistoryGranularity(raw) != AllHistoryGranularities[i] {
			t.Errorf("entry %d is %q, want %q", i, raw, AllHistoryGranularities[i])
		}
	}
	if err := HistoryGranularity("definitely-not-a-granularity").Validate(); err == nil {
		t.Error("Validate accepted a bogus granularity")
	} else if !strings.Contains(err.Error(), HistoryGranularityList()) {
		t.Errorf("Validate's rejection no longer carries the generated enumeration: %v", err)
	}
}
