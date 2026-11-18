package v1

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// assetParamNames are the parameter names through which a route takes an asset or a pair.
var assetParamNames = map[string]bool{
	"asset_id": true, "asset": true, "base": true, "quote": true, "pair": true, "assets": true, "pairs": true,
}

// aliasSpellingTests maps each asset-taking route to a test proving alias spellings read the same stored rows: XLM's spellings for
// most routes, a SAC folding to its wrapped classic for the movements and holders routes. The named test must exist in this package
// or in explorer/.
var aliasSpellingTests = map[string]string{
	"/price":                         "TestPrice_XLMQuoteAlias",
	"/divergence/series":             "TestDivergenceSeries_EveryXLMSpellingReachesTheStoredPair",
	"/accounts/{g_strkey}/movements": "TestAccountMovements_SACAssetFilterFoldsToTheWrappedClassic",
	"/assets/{asset_id}/holders":     "TestAssetHolders_SACFoldsToTheWrappedAssetsBoard",
	"/price/tip":                     "TestPriceTip_AliasResolvesXLM",
	"/price/batch":                   "TestPriceBatch_AliasResolvesXLM",
	"/price/stream":                  "TestPriceStream_AliasSubscriptionReceivesCryptoXLMPublishes",
}

// aliasSpellingDebt lists asset-taking routes with no spelling-equivalence test yet. Debt: shrink it by adding a test above; a
// new route belongs above, not here. aliasSpellingDebtCeiling is its current size: lower it as rows leave, never raise it.
const aliasSpellingDebtCeiling = 30

var aliasSpellingDebt = map[string]bool{
	"/assets/{asset_id}":                             true,
	"/assets/{asset_id}/entry-changes":               true,
	"/assets/{asset_id}/metadata":                    true,
	"/assets/{asset_id}/movements":                   true,
	"/assets/{asset_id}/supply":                      true,
	"/assets/{asset_id}/supply/flows":                true,
	"/assets/{asset_id}/trustline-history":           true,
	"/chart":                                         true,
	"/history":                                       true,
	"/history/since-inception":                       true,
	"/lending/pools/{pool}/reserves/{asset}/history": true,
	"/lending/rates":                                 true,
	"/liquidity-pools":                               true,
	"/markets":                                       true,
	"/markets/sources":                               true,
	"/observations":                                  true,
	"/observations/stream":                           true,
	"/ohlc":                                          true,
	"/oracle/lastprice":                              true,
	"/oracle/latest":                                 true,
	"/oracle/prices":                                 true,
	"/oracle/x_last_price":                           true,
	"/pairs":                                         true,
	"/pools":                                         true,
	"/pools/reserves":                                true,
	"/price/at":                                      true,
	"/price/changes":                                 true,
	"/price/tip/stream":                              true,
	"/twap":                                          true,
	"/vwap":                                          true,
}

// TestAssetRoutes_DeclareAliasSpellingCoverage walks the OpenAPI spec: every route taking an asset or pair parameter must name a
// spelling-equivalence test or sit on the debt list, so a new asset route cannot ship unexamined.
func TestAssetRoutes_DeclareAliasSpellingCoverage(t *testing.T) {
	spec := loadOpenAPISpec(t)
	tests := declaredTestNames(t)
	if n := len(aliasSpellingDebt); n > aliasSpellingDebtCeiling {
		t.Errorf("aliasSpellingDebt has %d rows, ceiling %d: the debt list only shrinks; cover the route in aliasSpellingTests instead",
			n, aliasSpellingDebtCeiling)
	}

	seen := map[string]bool{}
	var missing []string
	for path, ops := range spec.Paths {
		if !takesAsset(spec, ops) {
			continue
		}
		seen[path] = true
		name, covered := aliasSpellingTests[path]
		switch {
		case covered && !tests[name]:
			t.Errorf("%s names %s, which is not a test in internal/api/v1 or explorer/", path, name)
		case !covered && !aliasSpellingDebt[path]:
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("route %s takes an asset or pair: add a test that every XLM spelling (and, where the route takes a classic "+
			"asset, its SAC) reads the same rows, and name it in aliasSpellingTests", p)
	}
	for _, table := range []map[string]bool{keysOf(aliasSpellingTests), aliasSpellingDebt} {
		for p := range table {
			if !seen[p] {
				t.Errorf("%s is listed but the spec has no asset-taking route by that path: delete the row", p)
			}
		}
	}
}

func takesAsset(spec openAPISpec, ops map[string]*openAPIOperation) bool {
	for method, op := range ops {
		if op == nil || !strings.EqualFold(method, "get") {
			continue
		}
		for _, p := range op.Parameters {
			if p.Ref != "" {
				p = spec.Components.Parameters[p.Ref[strings.LastIndex(p.Ref, "/")+1:]]
			}
			if assetParamNames[p.Name] {
				return true
			}
		}
	}
	return false
}

func keysOf(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// declaredTestNames collects the Test functions declared in this package's and explorer/'s test files.
func declaredTestNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, glob := range []string{"*_test.go", filepath.Join("explorer", "*_test.go")} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(body), "\n") {
				if rest, ok := strings.CutPrefix(line, "func Test"); ok {
					out["Test"+rest[:strings.IndexAny(rest, "(")]] = true
				}
			}
		}
	}
	return out
}
