package v1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// A scam-gate withhold and a substance-gate withhold share one problem
// type, but only the substance wording may call the market thin or send
// the client to the raw trades: for a flagged issuer those trades are
// the market the platform refused to price (#732).

var scamWithheldForbidden = []string{
	"too thin",
	"trailing market activity",
	"apply your own judgement",
	"/v1/history",
}

func assertScamWithheldBody(t *testing.T, route string, resp *http.Response) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%s: status = %d, want 404: %s", route, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Price withheld — issuer flagged") {
		t.Errorf("%s: withheld body must name the flagged issuer as the cause: %s", route, body)
	}
	for _, wrong := range scamWithheldForbidden {
		if strings.Contains(string(body), wrong) {
			t.Errorf("%s: a flagged issuer's withheld body must not say %q: %s", route, wrong, body)
		}
	}
}

// TestScamWithheldHandlersNameTheFlag drives every handler that answers
// the scam verdict itself.
func TestScamWithheldHandlersNameTheFlag(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native := canonical.NativeAsset()
	for _, route := range []string{
		"/v1/vwap?base=" + base.String() + "&quote=native",
		"/v1/twap?base=" + base.String() + "&quote=native",
		"/v1/chart?base=" + base.String() + "&quote=native&timeframe=24h",
	} {
		t.Run(route, func(t *testing.T) {
			reader := &pairAwareHistoryReader{
				tradesByPair: map[string][]canonical.Trade{
					base.String() + "/native": {scamTestTrade(t, base, native)},
				},
			}
			gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
			ts := httpTestServer(t, v1.New(v1.Options{History: reader, Scam: gate}))
			assertScamWithheldBody(t, route, mustGet(t, ts.URL+route))
		})
	}
}

// TestReaderScamWithheldNamesTheFlag covers the reader seam: a reader
// that withholds via PriceWithheldError keeps the verdict's reason on
// /v1/price.
func TestReaderScamWithheldNamesTheFlag(t *testing.T) {
	reader := &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer)}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())
	route := "/v1/price?asset=" + flaggedIssuerAsset + "&quote=fiat:USD"
	assertScamWithheldBody(t, route, mustGet(t, ts.URL+route))
}

// TestSubstanceWithheldKeepsRawMarketGuidance is the other side: a thin
// market's raw trades ARE the honest fallback, so its wording keeps it.
func TestSubstanceWithheldKeepsRawMarketGuidance(t *testing.T) {
	reader := &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldThinMarket)}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())
	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "market too thin to aggregate") ||
		!strings.Contains(string(body), "/v1/observations") {
		t.Errorf("substance-withheld body must keep the thin-market title and raw-market guidance: %s", body)
	}
}

// TestScamVerdictIsWrittenWithTheScamReason guards the defect's shape: a
// function that asks scamWithheld and then writes the withheld problem
// must pass PriceWithheldScamIssuer — the reason-free call is exactly
// how /v1/vwap, /v1/twap and /v1/chart came to call a flagged issuer's
// market thin.
func TestScamVerdictIsWrittenWithTheScamReason(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	askers := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !callsIdent(fn.Body, "scamWithheld") {
				continue
			}
			askers++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "writePriceWithheldProblem" {
					return true
				}
				reason, ok := call.Args[len(call.Args)-1].(*ast.Ident)
				if !ok || reason.Name != "PriceWithheldScamIssuer" {
					t.Errorf("%s: %s asks scamWithheld but writes the withheld problem with "+
						"a reason other than PriceWithheldScamIssuer — use writeIfScamWithheld",
						fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	// A guard whose subject set is empty passes forever.
	if askers == 0 {
		t.Fatal("found no function calling scamWithheld — the guard is broken, not the code clean")
	}
}

func callsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
