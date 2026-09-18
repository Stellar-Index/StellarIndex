package main

// change_24h_pct divides a current USD price by the bucket 24h before
// it. The current leg is decimals-normalised in internal/api/v1
// (lookupUSDPrice, and the batch row); the anchor comes from
// storeChange24hReader here, and used to come back RAW. For a confirmed
// 9-decimals token that put the two legs of one percentage a factor of
// 100 apart, so a flat market served about +9900%.
//
// storeChange24hReader.s is a concrete *timescale.Store with an
// unexported db field (see price_at_guard_wiring_test.go), so the read
// itself cannot be scripted from this package. The correction is
// therefore proven on normalizeChange24hAnchor directly, and two
// source-level tripwires pin that the reader cannot return a bucket
// without going through it and that main() actually hands it the table.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fixedDecimals map[string]int

func (f fixedDecimals) Lookup(assetID string) (int, bool) {
	d, ok := f[assetID]
	return d, ok
}

func TestNormalizeChange24hAnchor(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	tkn, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	peg, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		lookup      fixedDecimals
		vwap        string
		base, quote canonical.Asset
		want        string // exact string when wantSame, else compared as a rational
		wantSame    bool
		wantErr     error
	}{
		{
			name:   "9dp base against fiat:USD — scaled up by 10^2",
			lookup: fixedDecimals{sorobanContract: 9}, vwap: "41.32", base: tkn, quote: usdQuoteAsset,
			want: "4132",
		},
		{
			name:   "9dp base against the peg it was read from — same factor",
			lookup: fixedDecimals{sorobanContract: 9}, vwap: "41.32", base: tkn, quote: peg,
			want: "4132",
		},
		{
			name:   "6dp base — scaled down by 10^1, small ratio not rounded away",
			lookup: fixedDecimals{sorobanContract: 6}, vwap: "0.000000000123", base: tkn, quote: peg,
			want: "0.0000000000123",
		},
		{
			name:   "9dp QUOTE leg — the factor inverts",
			lookup: fixedDecimals{sorobanContract: 9}, vwap: "41.32", base: peg, quote: tkn,
			want: "0.4132",
		},
		{
			name:   "no confirmed row — byte-identical, no reformat",
			lookup: fixedDecimals{}, vwap: "0.20114638079663692765", base: tkn, quote: peg,
			want: "0.20114638079663692765", wantSame: true,
		},
		{
			name:   "nil table — byte-identical",
			lookup: nil, vwap: "41.32", base: tkn, quote: peg,
			want: "41.32", wantSame: true,
		},
		{
			name:   "no confirmed row — even unparseable text passes through untouched",
			lookup: fixedDecimals{}, vwap: "garbage", base: tkn, quote: peg,
			want: "garbage", wantSame: true,
		},
		{
			name:   "flagged and unparseable — no anchor, never a raw one",
			lookup: fixedDecimals{sorobanContract: 9}, vwap: "garbage", base: tkn, quote: peg,
			wantErr: v1.ErrChange24hUnavailable,
		},
		{
			name:   "flagged and zero — no anchor",
			lookup: fixedDecimals{sorobanContract: 9}, vwap: "0", base: tkn, quote: peg,
			wantErr: v1.ErrChange24hUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A nil map must reach the function as a nil INTERFACE, the way
			// an unwired reader would hold it.
			var got string
			var err error
			if tc.lookup == nil {
				got, err = normalizeChange24hAnchor(nil, tc.vwap, tc.base, tc.quote)
			} else {
				got, err = normalizeChange24hAnchor(tc.lookup, tc.vwap, tc.base, tc.quote)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v (got %q)", err, tc.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSame {
				if got != tc.want {
					t.Errorf("anchor = %q, want %q byte-identical", got, tc.want)
				}
				return
			}
			gotRat, ok := new(big.Rat).SetString(got)
			if !ok {
				t.Fatalf("anchor %q does not parse", got)
			}
			wantRat, _ := new(big.Rat).SetString(tc.want)
			if gotRat.Cmp(wantRat) != 0 {
				t.Errorf("anchor = %q, want a value equal to %s", got, tc.want)
			}
		})
	}
}

// The property the finding is about, stated as the percentage: a flat
// market must read 0%, not +9900%.
func TestNormalizeChange24hAnchor_FlatMarketIsFlat(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	tkn, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	// What lookupUSDPrice serves now for a raw 41.32 bucket on a 9dp token.
	current, _ := new(big.Rat).SetString("4132.0000000000")

	anchor, err := normalizeChange24hAnchor(fixedDecimals{sorobanContract: 9}, "41.32", tkn, usdQuoteAsset)
	if err != nil {
		t.Fatal(err)
	}
	then, ok := new(big.Rat).SetString(anchor)
	if !ok {
		t.Fatalf("anchor %q does not parse", anchor)
	}
	pct := new(big.Rat).Sub(current, then)
	pct.Quo(pct, then).Mul(pct, big.NewRat(100, 1))
	if pct.Sign() != 0 {
		t.Errorf("flat market change = %s%%, want 0 (current and anchor are on different scales)", pct.FloatString(2))
	}
}

// Every bucket USDPrice24hAgo hands back must have gone through the
// normaliser: a `return row.VWAP, nil` is the defect.
func TestChange24hReaderNeverReturnsARawBucket(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	fn := findMethod(f, "storeChange24hReader", "USDPrice24hAgo")
	if fn == nil {
		t.Fatal("could not locate storeChange24hReader.USDPrice24hAgo in main.go — " +
			"update this guard to follow the refactor rather than deleting it")
	}
	normalised, reads := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "normalizeChange24hAnchor" {
				normalised++
			}
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ClosedVWAP1mAtOrBefore" {
				reads++
			}
		case *ast.ReturnStmt:
			if len(v.Results) == 0 {
				return true
			}
			if sel, ok := v.Results[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "VWAP" {
				t.Errorf("USDPrice24hAgo returns a raw %s.VWAP at %s — the anchor must be "+
					"decimals-normalised like the current price it is divided into",
					exprName(sel.X), fset.Position(v.Pos()))
			}
		}
		return true
	})
	if reads == 0 {
		t.Fatal("found no ClosedVWAP1mAtOrBefore read in USDPrice24hAgo — the scan is broken")
	}
	if normalised < reads {
		t.Errorf("USDPrice24hAgo makes %d bucket reads but normalises only %d — "+
			"every read path (the peg fallback included) must normalise against the pair it read",
			reads, normalised)
	}
}

// A normaliser handed a nil table is a byte-identical no-op, so the
// wiring is what makes the fix live: main() must pass the cache.
func TestChange24hReaderIsWiredWithDecimalsTable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	literals := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "storeChange24hReader" {
			return true
		}
		literals++
		wired := false
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "decimals" {
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name != "nil" {
					wired = true
				}
			}
		}
		if !wired {
			t.Errorf("storeChange24hReader constructed at %s without a decimals table — "+
				"the anchor normalisation is inert and change_24h_pct mixes scales again",
				fset.Position(lit.Pos()))
		}
		return true
	})
	if literals == 0 {
		t.Fatal("found no storeChange24hReader literal in main.go — the scan is broken")
	}
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<expr>"
}
