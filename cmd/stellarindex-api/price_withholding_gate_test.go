package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPriceServingSeamsAreGated is a guard-coverage test for the price
// WITHHOLDING decision (wave-D MSP cluster).
//
// The product rule is fail-closed on trust: when the substance gate judges a
// market too thin to aggregate, or the scam gate finds the issuer
// directory-flagged, we do not publish a price. That rule was implemented at
// ONE reader seam (storePriceReader, behind /v1/price) and leaked at every
// other seam reading the same closed VWAP buckets — /v1/price/at and
// /v1/price/changes re-served the exact number /v1/price had just withheld, so
// one extra path segment defeated both gates (MSP-01, reproduced against real
// Postgres by the wave-D skeptic).
//
// Fixing those two seams by hand is not the deliverable; the leak happened
// BECAUSE the decision lived at a seam instead of a chokepoint, so the same
// thing recurs the next time someone adds a reader. This test derives the
// price-serving seam set from the source and fails when one of them does not
// route through priceWithheld() — i.e. it fails for the seam that does not
// exist yet.
//
// Why an AST guard rather than a behavioural test: a behavioural test can only
// cover the endpoints someone remembered to write a case for, which is the same
// weakness that produced the leak.
//
// The subject set is derived by finding every method that CALLS a closed-VWAP
// store read, not by listing seam names. The first version of this test did
// list them — two literal entries — which meant a brand-new ungated reader
// passed silently, and the property it advertised ("a new read seam cannot
// forget it") was not true. Matching on the store call is what makes it true:
// a reader has to call one of those methods to serve a price at all.
//
// Proven red: deleting the priceWithheld() call from storePriceAtReader.PriceAt
// (i.e. restoring the pre-fix state) fails this test naming that method.
//
// SCOPE — stated because it was read as wider than it is. This scan parses
// main.go and nothing else, so its subject set is the READER seams wired in
// this binary. Every HTTP handler lives in internal/api/v1, which this scan
// structurally cannot see: that is how `/v1/price?window=N` shipped serving
// a directory-flagged issuer's aggregated price straight out of the VWAP
// cache while a guard named "price serving seams are gated" passed (T669).
// The handler package's own cache seams are covered by
// [TestV1VWAPCacheSeamsAreGated] below.
func TestPriceServingSeamsAreGated(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Methods that legitimately read a closed bucket WITHOUT consulting
	// the chokepoint, each with the reason it is safe. An exemption is a
	// deliberate, reviewed decision — not a way to silence this test.
	exempt := map[string]string{
		"storePriceReader.RecentClosedVWAP1mExists": "existence probe: returns a bool, never a price or a bucket value",
		"storeChange24hReader.USDPrice24hAgo": "gated upstream — populateChange24h early-returns unless the GATED " +
			"lookupUSDPrice succeeds first, and scam suppression nulls the change pills regardless",
	}

	var ungated []string
	seams := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		if !readsClosedVWAP(fn) {
			return true
		}
		seams++
		name := receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
		if _, ok := exempt[name]; ok {
			return true
		}
		if !callsPriceWithheld(fn) {
			ungated = append(ungated, name)
		}
		return true
	})

	// A guard whose subject set is empty passes forever. If the scan
	// stops finding seams, the scan is broken — not the code clean.
	if seams == 0 {
		t.Fatal("found no closed-VWAP read seams in main.go — the scan is broken, " +
			"and a guard that checks nothing passes forever")
	}
	for _, name := range ungated {
		t.Errorf("price-serving seam %s reads a closed VWAP bucket without calling "+
			"priceWithheld() — whatever route reaches it would publish a price the "+
			"gate refuses (a directory-flagged issuer, or a market too thin to "+
			"aggregate). Route it through the chokepoint, or add it to `exempt` "+
			"with the reason it cannot serve a gated value.", name)
	}
}

// readsClosedVWAP reports whether fn calls one of the store reads that
// return a closed 1m VWAP bucket — the value the withholding decision
// governs. Matching on the STORE CALL rather than a hand-listed set of
// method names is what makes this guard cover a seam that does not exist
// yet: a new reader has to call one of these to serve a price at all.
func readsClosedVWAP(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// r.s.LatestClosedVWAP1mForPair(...), g.s.ClosedVWAPAtOrBefore(...), …
		if inner, ok := sel.X.(*ast.SelectorExpr); !ok || inner.Sel.Name != "s" {
			return true
		}
		if strings.Contains(sel.Sel.Name, "ClosedVWAP") {
			found = true
			return false
		}
		return true
	})
	return found
}

// receiverTypeName unwraps *T / T to the bare type name.
func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// callsPriceWithheld reports whether fn's body contains a call to the
// withholding chokepoint.
func callsPriceWithheld(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "priceWithheld" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestPriceWithheldChokepointHonoursBothGates pins the chokepoint's own
// semantics: withhold when the substance gate refuses OR the scam gate flags.
// Nil gates mean "operator disabled [pricing_guard]" and must allow — the
// gates are nil-receiver safe and this test keeps that contract explicit, so a
// future refactor cannot turn a disabled gate into a deny-everything outage.
func TestPriceWithheldChokepointHonoursBothGates(t *testing.T) {
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	// Nil gates: allow (disabled guard keeps prior behaviour).
	if priceWithheld(context.Background(), nil, nil, canonical.NativeAsset(), usd, "price_read") {
		t.Error("nil gates must allow — a disabled [pricing_guard] must not withhold every price")
	}
}

// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint pins the SECOND
// property of the MSP cluster: drift WITHIN a seam.
//
// TestPriceServingSeamsAreGated above answers "does every serving seam
// consult the gates AT ALL". It cannot answer "does each seam consult
// BOTH gates on EVERY branch", because a method with two arms satisfies
// it as soon as ONE arm calls priceWithheld().
//
// That is exactly the MSP-07 shape. storePriceReader.LatestPrice has a
// closed-VWAP arm and a last-trade arm; the last-trade arm originally
// spelled out `!r.substance.Allowed(...)` and consulted the SCAM gate
// not at all. An operator setting disable_substance_gate=true to
// diagnose a coverage complaint would then silently publish a
// directory-flagged issuer's last trade as its price — reversing an
// owner-level trust decision they never touched.
//
// This guard existed, and I DELETED it in the review sweep that
// rewrote its sibling — while that same commit's message said "the
// MSP-07 half (drift WITHIN a seam) was always real". The coverage
// half was genuinely weak and was rightly replaced; removing this half
// alongside it was a regression, and the planted MSP-07 mutation
// passed CI until this was restored. main.go still cited this test by
// name the whole time.
//
// The rule: the gate METHODS (.Allowed / .Withheld on a gate receiver)
// may be named in exactly one place — priceWithheld(). Every other
// call site is a second spelling that can drift out of step.
func TestWithholdingGatesAreSpelledOnlyAtTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Method names that ARE the withholding decision. Called anywhere but the
	// chokepoint, they are a second spelling that can drift.
	gateMethods := map[string]string{
		"Allowed":  "substance gate",
		"Withheld": "scam gate",
	}

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Name.Name == "priceWithheld" {
			return false // the one legitimate spelling
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			which, isGate := gateMethods[sel.Sel.Name]
			if !isGate || !gateReceiver(sel.X) {
				return true
			}
			t.Errorf("%s: calls the %s directly (%s) at %s — route it through "+
				"priceWithheld() instead. A hand-written call site can consult one "+
				"gate and forget the other, which is exactly how the last-trade arm "+
				"came to honour the thin-market floor but not the scam decision.",
				enclosingName(fn), which, exprString(sel), fset.Position(call.Pos()))
			return true
		})
		return false
	})
}

func gateReceiver(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == "substance" || t.Sel.Name == "scam"
	case *ast.Ident:
		return t.Name == "substance" || t.Name == "scam"
	}
	return false
}

func enclosingName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func exprString(sel *ast.SelectorExpr) string {
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return exprString(x) + "." + sel.Sel.Name
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}

// flaggingScamDirectory flags exactly the listed G-addresses and records
// which address each lookup asked about — the SAC spelling carries no
// G-address of its own, so what the directory is ASKED is the proof that
// the wrapper was resolved to its classic issuance.
type flaggingScamDirectory struct {
	flagged map[string]bool
	asked   []string
}

func (d *flaggingScamDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	d.asked = append(d.asked, address)
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

// TestPriceWithheldChokepointResolvesSACSpelling pins the /v1/price
// family (plus /v1/price/batch, /v1/price/at, the SEP-40 oracle and the
// asset headline, which all route through this one function) for the
// SAC-spelling bypass.
//
// The scam directory is keyed by the issuer G-address only a CLASSIC
// asset carries, and every caller hands the chokepoint the RAW requested
// base. A Stellar Asset Contract wrapper is the same asset as the
// classic issuance it wraps, so a request naming the wrapper reached a
// gate that had already decided it had nothing to say — and the flagged
// issuer's price was served (R8). The gate now resolves the base to its
// canonical family form first, so both spellings reach the same verdict.
//
// NOT parallel: the alias registry is process-global.
func TestPriceWithheldChokepointResolvesSACSpelling(t *testing.T) {
	const (
		code   = "RIO"
		issuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	)
	classic, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sacID, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(sacID)
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(map[string]string{sacID: code + ":" + issuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	ctx := context.Background()

	dir := &flaggingScamDirectory{flagged: map[string]bool{issuer: true}}
	gate := pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})

	if !priceWithheld(ctx, nil, gate, sac, usd, "price_read") {
		t.Error("the SAC spelling of a flagged classic issuance must be withheld on " +
			"/v1/price — the wrapper is the same asset, so the contract id must not " +
			"be a second, ungated way to ask for the price")
	}
	if len(dir.asked) == 0 || dir.asked[0] != issuer {
		t.Errorf("directory asked about %v, want the classic issuance's issuer %q — "+
			"the resolution, not the status, is what is being pinned", dir.asked, issuer)
	}
	// The classic spelling is unchanged.
	if !priceWithheld(ctx, nil, gate, classic, usd, "price_read") {
		t.Error("the classic spelling must still be withheld")
	}

	// Blast radius: an unflagged wrapped asset keeps serving.
	cleanDir := &flaggingScamDirectory{flagged: map[string]bool{}}
	cleanGate := pricingguard.NewScamGate(cleanDir, pricingguard.ScamGateOptions{})
	if priceWithheld(ctx, nil, cleanGate, sac, usd, "price_read") {
		t.Error("a wrapped asset the directory has not flagged must keep serving")
	}
}

// v1PackageDir is the API handler package, relative to this package's
// directory (a `go test` binary runs with its own package dir as cwd).
const v1PackageDir = "../../internal/api/v1"

// v1LookerInterface is the handler package's declared seam onto the
// aggregator's published VWAP cache. Its methods are derived from the
// source rather than hand-listed: a second method added to it is a
// second ungated cache read, and this guard covers it the day it
// appears.
const v1LookerInterface = "TriangulatedPriceLooker"

// TestV1VWAPCacheSeamsAreGated is the missing half of the chokepoint
// guard above: it covers internal/api/v1, where the HTTP handlers live.
//
// Why a second guard rather than a wider first one. The withholding
// decision is spelled once per package because the two packages hold
// different halves of it: cmd/stellarindex-api owns the READER
// chokepoint (priceWithheld, consulted inside the store readers), and
// internal/api/v1 owns the HANDLER chokepoint (scamWithheld, plus the
// ErrPriceWithheld verdict those readers propagate). A handler that
// answers out of the aggregator's VWAP cache passes through neither
// reader — the production looker reads Redis unconditionally — so that
// cache is the one price source with no gate underneath it, and the
// handler has to ask for itself.
//
// Which is precisely what `?window=300|3600|86400` did not do: it read
// vwap:<base>:<quote>:<window> and published a directory-flagged
// issuer's aggregated price at 200, unauthenticated, while the default
// route on the same pair 404'd (RLT-350/T669).
//
// The rule enforced here:
//
//   - every function reading the cache must consult the withholding
//     decision BEFORE the read — position-checked, because the original
//     bypass was a caller that consulted AFTER dispatching;
//   - an HTTP handler must consult it itself, with no credit from its
//     callers: handlePrice consults the decision and still dispatched to
//     the ungated windowed handler first, so "my caller checks" is
//     exactly the reasoning that shipped the leak;
//   - a non-handler helper may inherit the decision from its callers,
//     checked transitively and position-wise, or from a caller that
//     discards the price — an existence probe serving a bool is not a
//     price surface, the same exemption the main.go scan grants
//     storePriceReader.RecentClosedVWAP1mExists, except PROVEN here from
//     the call site's blank assignment instead of asserted in a list.
//
// What this guard does NOT prove, said plainly so it is not read as
// wider than it is: it is a structural reachability check, not a proof
// that the consultation it found was a verdict about THIS pair. A caller
// that honours ErrPriceWithheld on its primary read and then falls
// through to the cache on not-found earns credit here, so the fallback
// chain's own entry gate is pinned behaviourally instead — see
// TestPriceFallbackWithholdsScamFlaggedBase and
// TestCachedVWAPSurfacesWithholdScamFlaggedMarket in the handler
// package. What this guard owns is the class those cannot: the seam
// nobody remembered to write a case for.
//
// Proven red three ways, each by reconstructing the state and running
// this test alone: deleting the scamWithheld() call from
// Server.handlePriceWindowed (the pre-fix state) names that handler;
// MOVING that call below the cache read names it too; and making
// Server.observationsHaveTriangulatedPrice keep the price it currently
// discards names the shared helper with the whole caller chain.
func TestV1VWAPCacheSeamsAreGated(t *testing.T) {
	sc := loadV1Scan(t)
	seams := sc.cacheSeams()
	// A guard whose subject set is empty passes forever.
	if len(seams) == 0 {
		t.Fatalf("found no %s reads in %s — the scan is broken, not the code clean",
			v1LookerInterface, v1PackageDir)
	}
	for _, fn := range seams {
		why, ok := sc.covered(fn, fn.read, map[*v1Func]bool{})
		if ok {
			t.Logf("gated cache seam %s (%s): %s", fn.label, fn.file, why)
			continue
		}
		t.Errorf("price-serving seam %s (%s) reads the aggregator's published VWAP "+
			"without the withholding decision being asked first: %s. That cache is "+
			"written with no directory consultation, so nothing upstream filters it "+
			"— call scamWithheld(ctx, s.scam, base, quote, surface) before the read "+
			"(or honour ErrPriceWithheld), otherwise this route publishes the price "+
			"a flagged issuer's market had already been refused.", fn.label, fn.file, why)
	}
}

// v1Func is one function declaration in the handler package. `name` is
// the bare identifier an in-package call spells (`s.name(...)`); `label`
// carries the receiver for reporting.
type v1Func struct {
	name  string
	label string
	file  string
	decl  *ast.FuncDecl
	read  token.Pos // earliest cache read in the body; token.NoPos if none
}

// v1Call is one in-package call site. `discard` records that the call's
// first result — the price — was assigned to the blank identifier.
type v1Call struct {
	caller  *v1Func
	pos     token.Pos
	discard bool
}

type v1Scan struct {
	fset  *token.FileSet
	funcs []*v1Func
	calls map[string][]v1Call
	reads map[string]bool
}

// loadV1Scan parses every non-test file of the handler package.
func loadV1Scan(t *testing.T) *v1Scan {
	t.Helper()
	entries, err := os.ReadDir(v1PackageDir)
	if err != nil {
		t.Fatalf("read %s: %v — the guard cannot see the handler package", v1PackageDir, err)
	}
	sc := &v1Scan{
		fset:  token.NewFileSet(),
		calls: map[string][]v1Call{},
		reads: map[string]bool{},
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(sc.fset, filepath.Join(v1PackageDir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		sc.addFile(name, f)
	}
	if len(sc.reads) == 0 {
		t.Fatalf("interface %s not found in %s — the scan is broken, not the code clean",
			v1LookerInterface, v1PackageDir)
	}
	for _, fn := range sc.funcs {
		sc.recordCalls(fn)
		fn.read = sc.earliestRead(fn)
	}
	return sc
}

func (sc *v1Scan) addFile(file string, f *ast.File) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			sc.addLookerMethods(d)
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			sc.funcs = append(sc.funcs, &v1Func{
				name: d.Name.Name, label: enclosingName(d), file: file, decl: d,
			})
		}
	}
}

// addLookerMethods records the cache interface's method set.
func (sc *v1Scan) addLookerMethods(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok || ts.Name.Name != v1LookerInterface {
			continue
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			continue
		}
		for _, m := range it.Methods.List {
			for _, n := range m.Names {
				sc.reads[n.Name] = true
			}
		}
	}
}

// recordCalls indexes fn's in-package calls by callee name. The package
// names its server receiver `s` throughout, so `s.helper(...)` is the
// one spelling an intra-package method call takes.
func (sc *v1Scan) recordCalls(fn *v1Func) {
	discarded := map[token.Pos]bool{}
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		if call, ok := as.Rhs[0].(*ast.CallExpr); ok && isBlankIdent(as.Lhs[0]) {
			discarded[call.Pos()] = true
		}
		return true
	})
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := v1LocalCallee(call); ok {
			sc.calls[name] = append(sc.calls[name], v1Call{
				caller: fn, pos: call.Pos(), discard: discarded[call.Pos()],
			})
		}
		return true
	})
}

// earliestRead returns the position of fn's first cache read, or
// token.NoPos when fn never reads the cache.
func (sc *v1Scan) earliestRead(fn *v1Func) token.Pos {
	first := token.NoPos
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !sc.reads[sel.Sel.Name] {
			return true
		}
		// A field read (s.triangulated.Lookup…), never a package
		// function that happens to share the name.
		if _, ok := sel.X.(*ast.SelectorExpr); !ok {
			return true
		}
		if first == token.NoPos || call.Pos() < first {
			first = call.Pos()
		}
		return true
	})
	return first
}

func (sc *v1Scan) cacheSeams() []*v1Func {
	var out []*v1Func
	for _, fn := range sc.funcs {
		if fn.read != token.NoPos {
			out = append(out, fn)
		}
	}
	return out
}

// covered reports whether the withholding decision is reached before
// `before` on every route into fn, and why.
func (sc *v1Scan) covered(fn *v1Func, before token.Pos, seen map[*v1Func]bool) (string, bool) {
	if pos, ok := sc.consultBefore(fn, before); ok {
		return "consults the withholding decision at " + sc.fset.Position(pos).String(), true
	}
	if isHTTPHandler(fn.decl) {
		return fn.label + " is an HTTP handler and asks nothing before " +
			sc.fset.Position(before).String(), false
	}
	if seen[fn] {
		return fn.label + " is reachable only through itself", false
	}
	seen[fn] = true
	defer delete(seen, fn)
	sites := sc.calls[fn.name]
	if len(sites) == 0 {
		return fn.label + " has no in-package caller to inherit the decision from", false
	}
	for _, site := range sites {
		if site.discard {
			continue // the price is thrown away: an existence probe, not a price surface
		}
		if why, ok := sc.covered(site.caller, site.pos, seen); !ok {
			return site.caller.label + " calls " + fn.label + " at " +
				sc.fset.Position(site.pos).String() + " and " + why, false
		}
	}
	return "every caller reaches the decision before calling it", true
}

// consultBefore finds a withholding consultation positioned before
// `before`. The package spells the decision two ways: scamWithheld()
// (the handler chokepoint) and the ErrPriceWithheld verdict the store
// readers propagate. A hand-rolled s.scam.Withheld() is neither, and the
// handler package's own TestScamGateIsAskedThePairQuestion fails on it.
func (sc *v1Scan) consultBefore(fn *v1Func, before token.Pos) (token.Pos, bool) {
	found := token.NoPos
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Pos() >= before {
			return true
		}
		if id.Name != "scamWithheld" && id.Name != "ErrPriceWithheld" {
			return true
		}
		if found == token.NoPos || id.Pos() < found {
			found = id.Pos()
		}
		return true
	})
	return found, found != token.NoPos
}

// v1LocalCallee returns the bare name of an in-package call: a method on
// the server receiver (s.foo) or a package-level function (foo).
func v1LocalCallee(call *ast.CallExpr) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name, true
	case *ast.SelectorExpr:
		if recv, ok := fun.X.(*ast.Ident); ok && recv.Name == "s" {
			return fun.Sel.Name, true
		}
	}
	return "", false
}

func isBlankIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "_"
}

// isHTTPHandler reports whether fn takes an http.ResponseWriter — it can
// write a response itself, so it is the last place the decision can be
// made.
func isHTTPHandler(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		sel, ok := p.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" && sel.Sel.Name == "ResponseWriter" {
			return true
		}
	}
	return false
}
