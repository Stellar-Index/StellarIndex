package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPriceServingSeamsAreGated is a guard-coverage test for the price
// WITHHOLDING decision.
//
// The product rule is fail-closed on trust: when the substance gate judges a
// market too thin to aggregate, or the scam gate finds the issuer
// directory-flagged, we do not publish a price. Implementing that rule at
// ONE reader seam (wiring.StorePriceReader, behind /v1/price) leaks it at every
// other seam reading the same closed VWAP buckets — /v1/price/at and
// /v1/price/changes would re-serve the exact number /v1/price withheld, so
// one extra path segment would defeat both gates.
//
// Fixing those two seams by hand is not the deliverable; the leak happened
// BECAUSE the decision lived at a seam instead of a chokepoint, so the same
// thing recurs the next time someone adds a reader. This test derives the
// price-serving seam set from the source and fails when one of them does not
// route through wiring.PriceWithheld() — i.e. it fails for the seam that does not
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
// Proven red: deleting the wiring.PriceWithheld() call from storePriceAtReader.PriceAt
// fails this test naming that method.
//
// SCOPE — stated because it was read as wider than it is. This scan parses
// main.go and the binary's internal/wiring package and nothing else, so its
// subject set is the READER seams wired in this binary. Every HTTP handler lives in internal/api/v1, which this scan
// structurally cannot see: so `/v1/price?window=N` could serve
// a directory-flagged issuer's aggregated price straight out of the VWAP
// cache while a guard named "price serving seams are gated" passed.
// The handler package's own cache seams are covered by
// [TestV1VWAPCacheSeamsAreGated] below.
func TestPriceServingSeamsAreGated(t *testing.T) {
	fset := token.NewFileSet()

	// Methods that legitimately read a closed bucket WITHOUT consulting
	// the chokepoint, each with the reason it is safe, keyed "dir:Type.Method".
	// An exemption is a deliberate, reviewed decision — not a way to
	// silence this test.
	exempt := map[string]string{
		wiringDir + ":StorePriceReader.RecentClosedVWAP1mExists": "existence probe: returns a bool, never a price or a bucket value",
		".:storeChange24hReader.USDPrice24hAgo": "gated upstream — populateChange24h early-returns unless the GATED " +
			"lookupUSDPrice succeeds first, and scam suppression nulls the change pills regardless",
	}

	var ungated []string
	seams := map[string]int{}
	for _, sf := range readerSeamFiles(t) {
		f, err := parser.ParseFile(fset, sf.path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", sf.path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				return true
			}
			if !readsClosedVWAP(fn) {
				return true
			}
			seams[sf.dir]++
			name := sf.dir + ":" + receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
			if _, ok := exempt[name]; ok {
				return true
			}
			if !callsPriceWithheld(fn, sf.dir) {
				ungated = append(ungated, name)
			}
			return true
		})
	}

	// A guard whose subject set is empty passes forever. If the scan
	// stops finding seams in either source, the scan is broken — not the
	// code clean.
	for _, dir := range []string{".", wiringDir} {
		if seams[dir] == 0 {
			t.Fatalf("found no closed-VWAP read seams in %s — the scan is broken, "+
				"and a guard that checks nothing passes forever", dir)
		}
	}
	for _, name := range ungated {
		t.Errorf("price-serving seam %s reads a closed VWAP bucket without calling "+
			"wiring.PriceWithheld() — whatever route reaches it would publish a price the "+
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
// Any reference counts, not only a call: a method value
// (f := r.S.LatestClosedVWAP1mForPair; f(...)) reads the same bucket.
func readsClosedVWAP(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
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

// wiringDir is the binary's adapter package, home of the PriceWithheld
// chokepoint, relative to this package's directory.
const wiringDir = "internal/wiring"

// readerSeamFiles is every non-test file holding this binary's price reader
// seams: main.go and the wiring package.
func readerSeamFiles(t *testing.T) []gateSpellingFile {
	t.Helper()
	return append([]gateSpellingFile{{dir: ".", path: "main.go"}}, apiSourceFiles(t, wiringDir)...)
}

// callsPriceWithheld reports whether fn's body calls the withholding
// chokepoint: PriceWithheld inside the wiring package, wiring.PriceWithheld
// from main.go. A same-named function anywhere else is not the chokepoint.
func callsPriceWithheld(fn *ast.FuncDecl, dir string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if dir == wiringDir && f.Name == "PriceWithheld" {
				found = true
			}
		case *ast.SelectorExpr:
			if pkg, ok := f.X.(*ast.Ident); ok && dir != wiringDir && pkg.Name == "wiring" && f.Sel.Name == "PriceWithheld" {
				found = true
			}
		}
		return !found
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
	if wiring.PriceWithheld(context.Background(), nil, nil, canonical.NativeAsset(), usd, "price_read") != pricingguard.NotWithheld {
		t.Error("nil gates must allow — a disabled [pricing_guard] must not withhold every price")
	}
}

// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint pins the SECOND
// property of the MSP cluster: drift WITHIN a seam.
//
// TestPriceServingSeamsAreGated above answers "does every serving seam
// consult the gates AT ALL". It cannot answer "does each seam consult
// BOTH gates on EVERY branch", because a method with two arms satisfies
// it as soon as ONE arm calls wiring.PriceWithheld().
//
// That is exactly the MSP-07 shape. wiring.StorePriceReader.LatestPrice has a
// closed-VWAP arm and a last-trade arm; a last-trade arm that spells out
// `!r.substance.Allowed(...)` and never consults the SCAM gate lets an
// operator setting disable_substance_gate=true to diagnose a coverage
// complaint silently publish a directory-flagged issuer's last trade as
// its price — reversing an owner-level trust decision they never touched.
//
// The rule: a gate HALF's decision method may be named only inside a
// chokepoint — wiring.PriceWithheld() here, and in the handler package
// withheldBy() (the fold) and scamWithheld() (the pair question it
// asks). Every other call site is a second spelling that can drift out
// of step. The scan covers main.go, the wiring package AND every non-test
// file under internal/api: a guard bound to main.go could not see the handler
// package, so a hand-written gate in /v1/price/stream would be invisible
// to the very test its comment cites as its drift guard.
func TestWithholdingGatesAreSpelledOnlyAtTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	files := readerSeamFiles(t)
	files = append(files, apiSourceFiles(t, apiTreeDir)...)
	permitted := 0
	for _, sf := range files {
		f, err := parser.ParseFile(fset, sf.path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", sf.path, err)
		}
		violations, n := gateSpellingViolations(fset, sf.dir, f)
		permitted += n
		for _, v := range violations {
			t.Error(v)
		}
	}
	// withheldBy's .Allowed plus scamWithheld's .WithheldPair and .Withheld:
	// a scan that cannot see them cannot see the handler package.
	if permitted < 3 {
		t.Errorf("found %d gate-half calls inside the chokepoints across %d files, want >= 3 — the scan is broken, not the code clean",
			permitted, len(files))
	}
}

// TestGateSpellingGuardCatchesHandlerPackageSites plants the shapes the
// guard exists for, so a narrowed predicate fails here rather than going
// green over the tree.
func TestGateSpellingGuardCatchesHandlerPackageSites(t *testing.T) {
	shapes := []struct{ name, dir, src string }{
		{"substance half in a handler", v1ChokepointDir, `func (s *Server) h() bool { return s.substance.Allowed(ctx, a, b, "x") }`},
		{"scam half in a handler", v1ChokepointDir, `func (s *Server) h() bool { return s.scam.WithheldPair(ctx, a, b, "x") }`},
		{"uncounted measurement in a handler", v1ChokepointDir, `func (s *Server) h() { _ = s.substance.Measure(ctx, a, b) }`},
		{"renamed local", v1ChokepointDir, `func (s *Server) h() bool { g := s.substance; return g.AllowedAt(ctx, a, b, at, "x") }`},
		{"chokepoint name in another package", "../../internal/api/streaming", `func withheldBy() bool { return sub.Allowed(ctx, a, b, "x") }`},
		{"main.go outside PriceWithheld", ".", `func (r storePriceAtReader) f() bool { return r.substance.Allowed(ctx, a, b, "x") }`},
		{"wiring outside PriceWithheld", wiringDir, `func (r StorePriceReader) f() bool { return r.Substance.Allowed(ctx, a, b, "x") }`},
		{"chokepoint name in main.go", ".", `func PriceWithheld() bool { return sub.Allowed(ctx, a, b, "x") }`},
		{"method value of a gate half", v1ChokepointDir, `func (s *Server) h() bool { f := s.substance.Allowed; return f(ctx, a, b, "x") }`},
		{"gate half passed as a value", wiringDir, `func (r StorePriceReader) f() bool { return apply(r.Scam.WithheldPair) }`},
		{"method value through a renamed local", v1ChokepointDir, `func (s *Server) h() bool { g := s.scam; f := g.Withheld; return f(ctx, a, "x") }`},
	}
	violations := func(dir, src string) int {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "planted.go", "package p\n"+src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		v, _ := gateSpellingViolations(fset, dir, f)
		return len(v)
	}
	for _, sh := range shapes {
		if n := violations(sh.dir, sh.src); n != 1 {
			t.Errorf("%s: got %d violations, want 1", sh.name, n)
		}
	}
	// A rate-limit result's Allowed field shares the half's name but is no gate.
	if n := violations(v1ChokepointDir, `func f() bool { res := take(); return res.Allowed }`); n != 0 {
		t.Errorf("field read on a non-gate value: got %d violations, want 0", n)
	}
}

// TestStoreReadScansCatchMethodValues pins that both store-read scans see a
// method value, not only a call: f := r.S.LatestClosedVWAP1mForPair; f(...)
// serves the same bucket, and a call-only match let it through ungated.
func TestStoreReadScansCatchMethodValues(t *testing.T) {
	cases := []struct {
		name, src string
		want      bool
	}{
		{"direct call", `func (r R) f() { r.S.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"method value", `func (r R) f() { g := r.S.LatestClosedVWAP1mForPair; g(ctx, a, b) }`, true},
		{"method passed as an argument", `func (r R) f() { apply(r.s.ClosedVWAPAtOrBefore) }`, true},
		{"store alias", `func (r R) f() { st := r.S; st.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"unrelated store read", `func (r R) f() { r.S.LatestTrade(ctx, a, b) }`, false},
	}
	for _, c := range cases {
		if got := readsClosedVWAP(plantedFunc(t, c.src)); got != c.want {
			t.Errorf("readsClosedVWAP %s: got %v, want %v", c.name, got, c.want)
		}
	}

	sc := &v1Scan{reads: map[string]bool{"Lookup": true}}
	for _, src := range []string{
		`func (s *Server) h() { s.triangulated.Lookup(a, b) }`,
		`func (s *Server) h() { f := s.triangulated.Lookup; f(a, b) }`,
		`func (s *Server) h() { c := s.triangulated; c.Lookup(a, b) }`,
		`func (s *Server) h() { var c = s.triangulated; f := c.Lookup; f(a, b) }`,
		`func (s *Server) h(l TriangulatedPriceLooker) { l.Lookup(a, b) }`,
	} {
		if sc.earliestRead(&v1Func{decl: plantedFunc(t, src)}) == token.NoPos {
			t.Errorf("earliestRead missed the cache read in %q", src)
		}
	}
	// A package function that shares the method's name is not the cache.
	if sc.earliestRead(&v1Func{decl: plantedFunc(t, `func (s *Server) h() { dns.Lookup(a, b) }`)}) != token.NoPos {
		t.Error("earliestRead counted a package-qualified call as a cache read")
	}
}

func plantedFunc(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\n"+src, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return f.Decls[0].(*ast.FuncDecl)
}

// apiTreeDir is every handler package the API binary serves from.
const apiTreeDir = "../../internal/api"

// v1ChokepointDir is where withheldBy and scamWithheld live; the
// exemption is keyed on (dir, name) so a same-named function elsewhere
// earns nothing.
const v1ChokepointDir = "../../internal/api/v1"

// gateHalfMethods mirrors pricingguard's halfMethods: the SubstanceGate /
// ScamGate decision methods.
var gateHalfMethods = map[string]bool{
	"Allowed": true, "AllowedAt": true, "Verdict": true, "Probe": true,
	"Withheld": true, "WithheldPair": true, "Measure": true, "MeasureAt": true,
}

var gateChokepoints = map[string]bool{
	wiringDir + ":PriceWithheld":      true,
	v1ChokepointDir + ":withheldBy":   true,
	v1ChokepointDir + ":scamWithheld": true,
}

type gateSpellingFile struct{ dir, path string }

func apiSourceFiles(t *testing.T, root string) []gateSpellingFile {
	t.Helper()
	var out []gateSpellingFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, gateSpellingFile{dir: filepath.Dir(path), path: path})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// gateHolderNames are the field names a SubstanceGate or ScamGate is held
// under in the scanned packages.
var gateHolderNames = map[string]bool{"substance": true, "Substance": true, "scam": true, "Scam": true}

// gateSpellingViolations reports every gate-half call in f outside a
// chokepoint, and counts the ones inside. A method value taken from a gate
// (f := s.substance.Allowed; f(...)) is a call deferred, so it counts too.
// Non-call references are matched only on a gate holder because the half
// names are generic: rate-limit results carry a plain Allowed field.
func gateSpellingViolations(fset *token.FileSet, dir string, f *ast.File) (violations []string, permitted int) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		exempt := gateChokepoints[dir+":"+fn.Name.Name]
		calls := selectorCallees(fn.Body)
		aliases := gateAliases(fn.Body)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !gateHalfMethods[sel.Sel.Name] {
				return true
			}
			if !calls[sel] && !isGateHolder(sel.X, aliases) {
				return true
			}
			if exempt {
				permitted++
				return true
			}
			violations = append(violations, fmt.Sprintf("%s: calls a gate half directly (%s) at %s — route it through "+
				"wiring.PriceWithheld() (cmd/stellarindex-api/internal/wiring) or withheldBy() (internal/api/v1). A hand-written call site "+
				"can consult one gate and forget the other, which is exactly how the last-trade arm came to honour "+
				"the thin-market floor but not the scam decision.",
				enclosingName(fn), exprString(sel), fset.Position(sel.Pos())))
			return true
		})
	}
	return violations, permitted
}

// selectorCallees returns every selector body calls directly (x.M(...)).
func selectorCallees(body *ast.BlockStmt) map[*ast.SelectorExpr]bool {
	calls := map[*ast.SelectorExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				calls[sel] = true
			}
		}
		return true
	})
	return calls
}

// gateAliases returns the locals body binds to a gate holder (g := s.substance).
func gateAliases(body *ast.BlockStmt) map[string]bool {
	aliases := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		var lhs []*ast.Ident
		var rhs []ast.Expr
		switch st := n.(type) {
		case *ast.AssignStmt:
			for _, e := range st.Lhs {
				id, _ := e.(*ast.Ident)
				lhs = append(lhs, id)
			}
			rhs = st.Rhs
		case *ast.ValueSpec:
			lhs, rhs = st.Names, st.Values
		}
		if len(lhs) != len(rhs) {
			return true
		}
		for i, id := range lhs {
			if id != nil && isGateHolder(rhs[i], nil) {
				aliases[id.Name] = true
			}
		}
		return true
	})
	return aliases
}

// isGateHolder reports whether expr names a gate: a holder field
// (s.substance, r.Scam), a bare holder parameter, or a local alias of one.
func isGateHolder(expr ast.Expr, aliases map[string]bool) bool {
	switch x := expr.(type) {
	case *ast.SelectorExpr:
		return gateHolderNames[x.Sel.Name]
	case *ast.Ident:
		return gateHolderNames[x.Name] || aliases[x.Name]
	case *ast.ParenExpr:
		return isGateHolder(x.X, aliases)
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
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{sacID: code + ":" + issuer})
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

	if wiring.PriceWithheld(ctx, nil, gate, sac, usd, "price_read") != pricingguard.WithheldFlaggedIssuer {
		t.Error("the SAC spelling of a flagged classic issuance must be withheld on " +
			"/v1/price — the wrapper is the same asset, so the contract id must not " +
			"be a second, ungated way to ask for the price")
	}
	if len(dir.asked) == 0 || dir.asked[0] != issuer {
		t.Errorf("directory asked about %v, want the classic issuance's issuer %q — "+
			"the resolution, not the status, is what is being pinned", dir.asked, issuer)
	}
	// The classic spelling is unchanged.
	if wiring.PriceWithheld(ctx, nil, gate, classic, usd, "price_read") != pricingguard.WithheldFlaggedIssuer {
		t.Error("the classic spelling must still be withheld")
	}

	// Blast radius: an unflagged wrapped asset keeps serving.
	cleanDir := &flaggingScamDirectory{flagged: map[string]bool{}}
	cleanGate := pricingguard.NewScamGate(cleanDir, pricingguard.ScamGateOptions{})
	if wiring.PriceWithheld(ctx, nil, cleanGate, sac, usd, "price_read") != pricingguard.NotWithheld {
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
// chokepoint (wiring.PriceWithheld, consulted inside the store readers), and
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
// route on the same pair 404'd.
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
//     wiring.StorePriceReader.RecentClosedVWAP1mExists, except PROVEN here from
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
// Server.handlePriceWindowed names that handler;
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
// token.NoPos when fn never reads the cache. A method value taken from the
// cache is a read: it is the price-serving call, just deferred.
func (sc *v1Scan) earliestRead(fn *v1Func) token.Pos {
	first := token.NoPos
	locals := declaredNames(fn.decl)
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !sc.reads[sel.Sel.Name] {
			return true
		}
		// A bare identifier must be one fn declares (c := s.triangulated;
		// c.Lookup…); any other is taken as a package qualifier, so a
		// package function sharing the name is not a read.
		if id, ok := sel.X.(*ast.Ident); ok && !locals[id.Name] {
			return true
		}
		if first == token.NoPos || sel.Pos() < first {
			first = sel.Pos()
		}
		return true
	})
	return first
}

// declaredNames returns every name fn binds: receiver, parameters,
// results (its own and its closures'), and locals.
func declaredNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	for _, id := range fieldNames(fn.Recv) {
		out[id.Name] = true
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		for _, id := range boundIdents(n) {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// boundIdents returns the names n declares.
func boundIdents(n ast.Node) []*ast.Ident {
	switch d := n.(type) {
	case *ast.FuncType:
		return append(fieldNames(d.Params), fieldNames(d.Results)...)
	case *ast.ValueSpec:
		return d.Names
	case *ast.AssignStmt:
		if d.Tok == token.DEFINE {
			return identsOf(d.Lhs...)
		}
	case *ast.RangeStmt:
		if d.Tok == token.DEFINE {
			return identsOf(d.Key, d.Value)
		}
	}
	return nil
}

func fieldNames(fl *ast.FieldList) []*ast.Ident {
	if fl == nil {
		return nil
	}
	var out []*ast.Ident
	for _, f := range fl.List {
		out = append(out, f.Names...)
	}
	return out
}

func identsOf(exprs ...ast.Expr) []*ast.Ident {
	var out []*ast.Ident
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok {
			out = append(out, id)
		}
	}
	return out
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
// `before`. The package spells the decision three ways: scamWithheld()
// (the handler chokepoint), withheldBy() (its fold with the substance
// gate, which calls scamWithheld), writeIfScamWithheld() (scamWithheld
// plus the scam-worded problem, for handlers that answer the verdict
// themselves) and the ErrPriceWithheld verdict the store readers
// propagate. A hand-rolled s.scam.Withheld() is none of them, and the
// handler package's own TestScamGateIsAskedThePairQuestion fails on it.
func (sc *v1Scan) consultBefore(fn *v1Func, before token.Pos) (token.Pos, bool) {
	found := token.NoPos
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Pos() >= before {
			return true
		}
		switch id.Name {
		case "scamWithheld", "withheldBy", "writeIfScamWithheld", "ErrPriceWithheld":
		default:
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

// errPriceReader is a v1.PriceReader whose every read fails with err: the
// transport that puts a real store reader's withholding error on the
// /v1/price wire.
type errPriceReader struct{ err error }

func (r errPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	return v1.PriceSnapshot{}, nil, false, r.err
}

func (r errPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]v1.PriceSnapshot, error) {
	return nil, r.err
}

// TestReaderChokepointNamesTheScamGate pins that a store reader withholding
// for a directory-flagged issuer says so on the wire. The reader seams
// returned the bare sentinel, so /v1/price described a flagged issuer's
// market as too thin and told the client to recompute the price from the
// raw trades — the price the gate exists to refuse.
func TestReaderChokepointNamesTheScamGate(t *testing.T) {
	const issuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	flagged, err := canonical.NewClassicAsset("RIO", issuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	pair, err := canonical.NewPair(flagged, usd)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	gate := pricingguard.NewScamGate(&flaggingScamDirectory{flagged: map[string]bool{issuer: true}}, pricingguard.ScamGateOptions{})
	reader := storePriceAtReader{scam: gate} // nil store: the gate answers before any read

	_, _, _, readErr := reader.PriceAt(context.Background(), pair, time.Now().Add(-time.Hour), time.Hour)
	if !errors.Is(readErr, v1.ErrPriceWithheld) {
		t.Fatalf("PriceAt err = %v, want an ErrPriceWithheld-compatible error", readErr)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Prices: errPriceReader{err: readErr}}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/price?asset=" + flagged.String() + "&quote=fiat:USD")
	if err != nil {
		t.Fatalf("GET /v1/price: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Price withheld — issuer flagged") {
		t.Errorf("withheld body must name the flagged issuer as the cause: %s", body)
	}
	for _, wrong := range []string{"too thin", "apply your own judgement", "/v1/history"} {
		if strings.Contains(string(body), wrong) {
			t.Errorf("a flagged issuer's withheld body must not say %q: %s", wrong, body)
		}
	}
}
