package timescale

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	foldTestUSDCSAC   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	foldTestUSDCClass = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// A market traded on SDEX (native / USDC-GA5Z…) and on a Soroban venue
// (the XLM SAC / the USDC SAC) is ONE market. Every listing query that
// groups by canonOrientSQL's key must group the alias-FOLDED legs, and
// must bind the registry's fold map at the placeholder the fold reads —
// otherwise the two spellings are two directory rows, each with a slice
// of the 24h volume (GH-1098).
func TestMarketsQueriesGroupAliasFoldedLegs(t *testing.T) {
	reg, err := canonical.NewAliasRegistry(map[string]string{
		foldTestUSDCSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	type built struct {
		q    string
		args []any
	}
	cases := map[string]func() built{}
	for name, order := range map[string]MarketsOrder{"volume": MarketsOrderVolume24hDesc, "pair": MarketsOrderPair} {
		cases["pools/"+name] = func() built {
			q, a := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)
			return built{q, a}
		}
		cases["source markets/"+name] = func() built {
			q, a := buildSourceMarketsQuery(since, "soroswap", "", 100, order)
			return built{q, a}
		}
		cases["distinct pairs/"+name] = func() built {
			q, a := buildDistinctPairsQuery(since, "", "native", "", 100, order)
			return built{q, a}
		}
	}
	cases["network stats"] = func() built { return built{networkStatsQuery(), []any{aliasFoldArg()}} }

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			b := build()
			idx := len(b.args)
			fold := "COALESCE($" + strconv.Itoa(idx) + "::text::jsonb ->> base_asset, base_asset) AS base_asset"
			if !strings.Contains(b.q, fold) {
				t.Fatalf("query does not group the alias-folded base leg bound at $%d (%q) — "+
					"an SDEX row and its Soroban twin stay two markets:\n%s", idx, fold, b.q)
			}
			if !strings.Contains(b.q, strings.Replace(fold, "base_asset", "quote_asset", 3)) {
				t.Fatalf("query does not group the alias-folded quote leg bound at $%d", idx)
			}
			raw, ok := b.args[idx-1].(string)
			if !ok {
				t.Fatalf("$%d = %T, want the JSON fold map", idx, b.args[idx-1])
			}
			var forms map[string]string
			if err := json.Unmarshal([]byte(raw), &forms); err != nil {
				t.Fatalf("$%d is not a JSON object: %v", idx, err)
			}
			for form, want := range map[string]string{
				nativeXLMSAC:    "native",
				"crypto:XLM":    "native",
				foldTestUSDCSAC: foldTestUSDCClass,
			} {
				if forms[form] != want {
					t.Errorf("fold map[%s] = %q, want %q (bound map: %s)", form, forms[form], want, raw)
				}
			}
		})
	}
}

// A folded listing row (native|USDC-GA5Z…) must still find the volume
// its Soroban venues recorded under the SAC spellings, or the fold turns
// a pure-Soroban market's sparkline and first-trade date into blanks.
func TestAliasSpellingKeysCoverEverySpelling(t *testing.T) {
	reg, err := canonical.NewAliasRegistry(map[string]string{
		foldTestUSDCSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	got := aliasSpellingKeys([2]string{"native", foldTestUSDCClass})
	for _, want := range []string{
		"native|" + foldTestUSDCClass,
		nativeXLMSAC + "|" + foldTestUSDCSAC,
		"crypto:XLM|" + foldTestUSDCSAC,
	} {
		if !strings.Contains(strings.Join(got, ","), want) {
			t.Errorf("aliasSpellingKeys(native|USDC) = %v, missing %q", got, want)
		}
	}
	if len(got) != 6 {
		t.Errorf("aliasSpellingKeys(native|USDC) = %d keys, want 6 (3 XLM forms x 2 USDC forms)", len(got))
	}
	if !strings.Contains(pairsVolumeHistory24hQuery, "p.fold_key = w.fold_key") {
		t.Errorf("sparkline query must join rows to requests on the alias-folded key")
	}
}

// canonOrientUnfoldedAllowed names the functions that may orient without
// alias-folding, each with the reason it cannot double-count a market.
var canonOrientUnfoldedAllowed = map[string]string{
	// Per-source grain over raw trades: one venue writes one spelling of
	// an asset (SDEX classic, Soroban contract ids, CEX crypto:), so the
	// per-source market count has no twin to merge.
	"sourceStatsQuery": "per-source grain",
}

// Any function that builds a canonOrientSQL group key must also fold the
// legs onto their canonical alias form — a new listing that orients
// without folding reintroduces the SDEX/Soroban twin rows (GH-1098).
func TestCanonOrientCallersFoldAliases(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "canonOrientSQL" {
				continue
			}
			calls := calledIdents(fn.Body)
			if !calls["canonOrientSQL"] {
				continue
			}
			checked++
			if calls["aliasFoldedSelect"] || calls["aliasFoldSQL"] {
				continue
			}
			if _, ok := canonOrientUnfoldedAllowed[fn.Name.Name]; ok {
				continue
			}
			t.Errorf("%s: %s orients with canonOrientSQL but never alias-folds its legs (aliasFoldedSelect) — "+
				"a market's SDEX and Soroban spellings become two rows", f, fn.Name.Name)
		}
	}
	if checked < 4 {
		t.Fatalf("checked %d canonOrientSQL callers, want >= 4 — the scan is not seeing the package", checked)
	}
}

func calledIdents(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}
