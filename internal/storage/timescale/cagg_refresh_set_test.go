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

// ─── The backfill refresh set must cover every SERVED rung ───────────
//
// [CAGGsLiveForever] is the list the backfill tool iterates after each
// chunk. A granularity the API serves but the list omits is not a
// missing optimisation — it is a permanent hole at that resolution in
// every backfilled range: the trades land in the hypertable, the
// continuous-aggregate policies only roll forward, and nothing else
// materialises a historical bucket. The rows exist and no read can
// reach them.
//
// prices_1m and prices_15m were omitted for that reason from 2026-05
// to 2026-09 on a premise that expired in between (a 30-day retention
// migration 0031 removed), while /v1/ohlc, /v1/chart and
// /v1/history/since-inception went on serving both grains over
// caller-chosen windows.
//
// This file exists so the set cannot fall behind the enum again, and
// it is derived rather than transcribed. Three links make that hold:
//
//  1. [AllHistoryGranularities] is the only declaration of the served
//     set. [HistoryGranularity.Validate] ranges over it, and the 400
//     bodies in chart.go / history.go render it through
//     [HistoryGranularityList] — so nothing is accepted, and nothing
//     is advertised, that is not in the slice.
//  2. TestDeclaredGranularityConstsAreAllServed reads the package's
//     own source for HistoryGranularity constants, so a const added
//     but left out of the slice is caught rather than left as a rung
//     that half-exists.
//  3. TestCAGGsLiveForeverCoversEveryServedGranularity ranges over
//     the same slice, so the refresh set has to grow with it.
//
// The chain means an eighth granularity cannot reach the API without
// joining the refresh set — it has to be in the slice to be accepted,
// and being in the slice fails (3) until prices_<g> is refreshed.
// Nothing here reads prose, so rewording a message breaks nothing and
// rewording a message hides nothing.

// TestDeclaredGranularityConstsAreAllServed discovers every
// package-level `const … HistoryGranularity = "…"` from this package's
// own syntax and requires each to appear in [AllHistoryGranularities].
// A constant that skips the slice is not merely unused: Validate
// rejects it, so a caller passing the new grain gets a 400 from a
// granularity the code plainly declares — a rung that exists in one
// half of the package and not the other.
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

// declaredGranularityConsts parses this package's non-test sources and
// returns {const name: wire value} for every package-level constant of
// type HistoryGranularity. Syntactic on purpose — it needs no build of
// the whole repo, and the shape it reads is the shape the constants
// are actually written in. A grouped `const (…)` block carries the
// type forward exactly as the language does — see
// [collectGranularityConsts] — so a rung added without repeating the
// type is still found, and an unrelated constant sharing the group is
// not mistaken for one.
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

// collectGranularityConsts walks one file's declarations, adding every
// package-level HistoryGranularity string constant to out.
//
// The carrying rule is the language's, not a looser approximation of
// it. Within a parenthesized const declaration only a spec that omits
// its expression list inherits anything, and what it inherits is the
// preceding non-empty list TOGETHER WITH that list's type. A spec that
// supplies its own value inherits nothing: it is typed by its own
// `Type` if it has one and untyped otherwise, however the spec above
// it was written.
//
// Both halves matter here. Carrying a type onto a spec that has its
// own value makes any ordinary constant sharing the group read as a
// granularity, and this collector's callers report what they find as a
// rung the API declares but does not serve — so the misreading
// surfaces as a confident, wrong failure naming a constant with
// nothing to do with granularities. Carrying the type WITHOUT the
// expression list, meanwhile, finds a repeated spec's type and then
// has no value to record for it, so the rung it was supposed to catch
// falls out silently.
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

// TestCollectGranularityConstsFollowsTheLanguagesTypeCarrying pins
// [collectGranularityConsts] against synthetic source rather than
// against the package's current layout, because the package's layout
// is what it is meant to survive a change to.
//
// The middle case is the one that bit: an ordinary untyped constant
// written into the same `const (…)` group as the granularities was
// read as a granularity, and the guard above reported it as a rung
// declared but not served — a failure naming a constant that has
// nothing to do with granularities, on a file whose author did
// nothing wrong. Go carries a type forward only into a spec that
// omits its expression list too, so a spec with its own value starts
// fresh; the first case pins that the carrying itself still works, so
// the fix cannot be "stop carrying" and lose a rung written without
// its type.
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
		// Own value, no type → an untyped string, whatever the spec
		// above it says. A real rung written this way goes unseen here,
		// and TestDeclaredGranularityConstsAreAllServed's reverse check
		// catches it instead: the slice would hold a value no typed
		// constant declares.
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

// TestCAGGsLiveForeverRefreshesPrices1mFirst pins the one ordering
// convention in the set. Nothing in the set reads prices_1m today —
// the other six read `trades` directly and twap_1h / twap_1d are not
// in the list — so this is defensive rather than a live dependency:
// prices_1m is the only view another aggregate is built on (migrations
// 0081 / 0126 / 0147), so an operator or a future rung that does read
// it finds it current. Same order, same reason, as
// chops.xlmBaseRestampCAGGs.
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

// TestCAGGsLiveForeverMinWindowsClearTimescaleMinimum checks each entry
// against the rule that produced them: refresh_continuous_aggregate
// rejects a window narrower than 2x the bucket width with SQLSTATE
// 22023, and a rejected call means the operator ships a partially-stale
// surface rather than a loud failure.
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

// TestPadRefreshWindowCoversTheChunkAtEveryRung proves the added fine
// rungs are refreshed over the SAME range as the coarse ones rather
// than a narrower one: for a chunk shorter than any minimum, every
// entry's padded window must still contain the chunk end to end.
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

// TestHistoryGranularityListMatchesTheServedSet pins the generated
// enumeration to the slice. The 400 bodies for /v1/chart and
// /v1/history/since-inception render this string, so a caller shown a
// list that omits a served grain (or advertises one Validate rejects)
// is being told the wrong contract.
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
