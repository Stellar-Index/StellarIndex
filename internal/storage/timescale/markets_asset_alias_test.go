package timescale

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The ?asset= filter on /v1/markets must match every alias form the asset is keyed under, so $5 must bind the
// full alias text[] with ANY-membership on each leg. bindArrayValue renders the bound arg; a scalar fails it.
func bindArrayValue(t *testing.T, arg any) string {
	t.Helper()
	forms, ok := arg.([]string)
	if !ok {
		t.Fatalf("$5 asset arg is %T (%v), want a bound text[] ([]string) — "+
			"a scalar bind matches only the literal spelling and omits the alias forms", arg, arg)
	}
	return "{" + strings.Join(forms, ",") + "}"
}

func TestBuildDistinctPairsQuery_AssetFilterUsesAnyMembership(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildDistinctPairsQuery(since, "", "native", "", 100, order)
			// Each leg must be an ANY-membership test, not a scalar `= $5`.
			for _, want := range []string{
				"p.base_asset = ANY($5)",
				"p.quote_asset = ANY($5)",
			} {
				if !strings.Contains(q, want) {
					t.Errorf("query missing %q — an aliased asset can't match its other forms:\n%s", want, q)
				}
			}
			if strings.Contains(q, "p.base_asset = $5") {
				t.Errorf("query still binds the scalar `= $5` asset predicate (pre-fix shape)")
			}
		})
	}
}

func TestBuildDistinctPairsQuery_AssetFilterBindsFullAliasSet(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	wantForms := canonical.AssetAliasStrings(native)
	if len(wantForms) < 3 {
		t.Fatalf("expected XLM to expand to >=3 alias forms, got %v", wantForms)
	}

	_, args := buildDistinctPairsQuery(since, "", "native", "", 100, MarketsOrderVolume24hDesc)
	if len(args) != 8 {
		t.Fatalf("args = %d, want 8 ($1..$5 + the $6/$7 alias fold + the $8 native SAC)", len(args))
	}
	bound := bindArrayValue(t, args[4])
	// Every alias form (native, crypto:XLM, the SAC C-address) must be
	// bound so ANY-membership can reach the rows keyed under each.
	for _, form := range wantForms {
		if !strings.Contains(bound, form) {
			t.Errorf("bound $5 %q missing alias form %q — that form's markets stay invisible", bound, form)
		}
	}
}

func TestBuildDistinctPairsQuery_NoAssetBindsEmptyArray(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	// DistinctPairsExt / SourceMarkets pass asset="" — $5 must bind an
	// EMPTY array so the cardinality($5)=0 short-circuit disables the
	// filter (an unfiltered directory scan, unchanged).
	_, args := buildDistinctPairsQuery(since, "", "", "", 100, MarketsOrderPair)
	bound := bindArrayValue(t, args[4])
	if strings.ContainsAny(bound, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("empty-asset $5 = %q, want an empty array {}", bound)
	}
}

// The fold maps only SAC spellings: crypto:XLM keys the CEX population
// and is served as its own market row.
func TestSacAliasFoldBind_FoldsSACOnly(t *testing.T) {
	t.Parallel()
	forms, canons := sacAliasFoldBind()
	if !slices.Contains(forms, canonical.XLMSacContractID) {
		t.Fatalf("forms %v omit the XLM SAC", forms)
	}
	if slices.Contains(forms, "crypto:XLM") {
		t.Errorf("forms %v fold crypto:XLM; the CEX ticker must stay its own row", forms)
	}
	if len(forms) != len(canons) {
		t.Fatalf("forms/canons length %d != %d", len(forms), len(canons))
	}
	for i, f := range forms {
		if f == canonical.XLMSacContractID && canons[i] != "native" {
			t.Errorf("XLM SAC folds to %q, want native", canons[i])
		}
	}
}

var placeholderRE = regexp.MustCompile(`\$(\d+)`)

// Every /v1/markets and /v1/pools listing must group on the alias-folded
// spelling, with the fold arrays bound at the placeholders the SQL reads.
func TestMarketQueries_GroupOnFoldedSpelling(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	forms, _ := sacAliasFoldBind()
	for _, order := range []MarketsOrder{MarketsOrderVolume24hDesc, MarketsOrderPair} {
		for name, build := range map[string]func() (string, []any){
			"pools":          func() (string, []any) { return buildPoolsQuery(since, PoolsFilter{}, "", 100, order) },
			"source markets": func() (string, []any) { return buildSourceMarketsQuery(since, "sdex", "", 100, order) },
			"distinct pairs": func() (string, []any) { return buildDistinctPairsQuery(since, "", "native", "", 100, order) },
		} {
			q, args := build()
			label := fmt.Sprintf("%s (order %v)", name, order)
			if n := strings.Count(q, "LEFT JOIN alias_fold"); n != 2 {
				t.Errorf("%s: %d alias_fold joins, want 2 (base and quote)", label, n)
			}
			// native vs its SAC folds to a self-pair that canonical.NewPair refuses.
			if !regexp.MustCompile(`WHERE COALESCE\(bf\.canon, \w+\.base_asset\) <> COALESCE\(qf\.canon, \w+\.quote_asset\)`).MatchString(q) {
				t.Errorf("%s: self-folded pairs are not excluded before grouping", label)
			}
			m := regexp.MustCompile(`unnest\(\$(\d+)::text\[\], \$(\d+)::text\[\]\)`).FindStringSubmatch(q)
			if m == nil {
				t.Fatalf("%s: no alias_fold unnest", label)
			}
			fi, _ := strconv.Atoi(m[1])
			if got, ok := args[fi-1].([]string); !ok || !slices.Equal(got, forms) {
				t.Errorf("%s: $%d = %#v, want the SAC fold forms %v", label, fi, args[fi-1], forms)
			}
			maxIdx := 0
			for _, p := range placeholderRE.FindAllStringSubmatch(q, -1) {
				i, _ := strconv.Atoi(p[1])
				maxIdx = max(maxIdx, i)
			}
			if maxIdx != len(args) {
				t.Errorf("%s: SQL reads up to $%d but binds %d args", label, maxIdx, len(args))
			}
		}
	}
}

func TestExpandSACSpellings_ReadsFoldedForms(t *testing.T) {
	fold, spellings := sacFoldMaps()
	if got := foldSpelling(fold, canonical.XLMSacContractID); got != "native" {
		t.Fatalf("fold(XLM SAC) = %q, want native", got)
	}
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	keys, bases, quotes := expandSACSpellings([][2]string{{"native", usdc}}, spellings)
	var sawSAC bool
	for i := range keys {
		if keys[i] != "native|"+usdc {
			t.Errorf("row %d keyed %q, want the requested pair", i, keys[i])
		}
		if bases[i] == canonical.XLMSacContractID && quotes[i] == usdc {
			sawSAC = true
		}
	}
	if !sawSAC || !slices.Contains(bases, "native") {
		t.Errorf("expansion of native|USDC = %v × %v, want both native and the XLM SAC", bases, quotes)
	}
}

// ValidateMarketsCursor pins per-shape rejection: unvalidated MarketsOrderPair garbage falls to a
// collation-dependent lexicographic skip, and MarketsOrderVolume24hDesc garbage 500s on a `numeric` cast.

func TestValidateMarketsCursor_emptyAlwaysOK(t *testing.T) {
	for _, order := range []MarketsOrder{
		MarketsOrderPair,
		MarketsOrderVolume24hDesc,
	} {
		if err := ValidateMarketsCursor("", order); err != nil {
			t.Errorf("empty cursor must be valid for order %v, got %v", order, err)
		}
	}
}

func TestValidateMarketsCursor_pairOrder(t *testing.T) {
	cases := []struct {
		in        string
		wantErr   bool
		wantTagIn string
	}{
		{"native|USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false, ""},
		{"fiat:USD|fiat:EUR", false, ""},
		{"crypto:BTC|native", false, ""},
		// Bare contract-strkey base/quote are valid asset IDs.
		{"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC|native", false, ""},
		{"missing-pipe", true, "separator in pair"},
		{"|native", true, "missing base or quote"},
		{"native|", true, "missing base or quote"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateMarketsCursor(tc.in, MarketsOrderPair)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantTagIn) {
					t.Errorf("expected error containing %q, got %v", tc.wantTagIn, err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateMarketsCursor_volumeOrder(t *testing.T) {
	cases := []struct {
		in        string
		wantErr   bool
		wantTagIn string
	}{
		{"123.45:native|USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false, ""},
		// Empty volume prefix is what encodeMarketsCursor emits when
		// the last row had a null vol_usd.
		{":native|USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false, ""},
		// fiat:USD pair with multiple colons — first ':' is
		// vol/pair sep, suffix retains its colons.
		{"100:fiat:USD|fiat:EUR", false, ""},
		{"missing-colon", true, "':' separator"},
		{"100:", true, "missing pair suffix"},
		{"100:nopipehere", true, "separator in pair"},
		{"abc:native|USDC-G", true, "non-numeric volume prefix"},
		{"1.2.3:native|USDC-G", true, "non-numeric volume prefix"},
		{"-5.0:native|USDC-G", true, "non-numeric volume prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateMarketsCursor(tc.in, MarketsOrderVolume24hDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantTagIn) {
					t.Errorf("expected error containing %q, got %v", tc.wantTagIn, err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// rawPairCountRE matches a distinct-market count keyed on raw (base_asset, quote_asset) columns. prices_1m
// and trades store each market in the venue's observed direction, so such a count counts a two-sided market
// twice.
var rawPairCountRE = regexp.MustCompile(
	`(?i)(count\s*\(\s*distinct\s*\(\s*base_asset\s*,\s*quote_asset\s*\)` +
		`|select\s+distinct\s+base_asset\s*,\s*quote_asset\b)`)

// TestNoRawOrientationMarketCount guards against counting a market once per stored orientation: no non-test
// file may count distinct markets on the raw columns; use marketKeySQL or canonOrientSQL. Comment lines are
// skipped.
func TestNoRawOrientationMarketCount(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue
			}
			if rawPairCountRE.MatchString(line) {
				t.Errorf("%s:%d counts markets on the raw stored orientation (use marketKeySQL / canonOrientSQL): %s",
					name, i+1, trimmed)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files — the guard did not run")
	}
}

// TestPairSourceStatsMatchesBothOrientations pins /v1/markets/sources?
// base=&quote= to both stored directions of the market, as
// pairMarketQuery reads it.
func TestPairSourceStatsMatchesBothOrientations(t *testing.T) {
	for _, want := range []string{
		"AND base_asset = ANY($1) AND quote_asset = ANY($2)\n\t\t    UNION ALL",
		"AND base_asset = ANY($2) AND quote_asset = ANY($1)",
	} {
		if !strings.Contains(pairSourceStatsQuery, want) {
			t.Errorf("pairSourceStatsQuery must match %s:\n%s", want, pairSourceStatsQuery)
		}
	}
	for name, q := range map[string]string{
		"pairSourceStatsQuery":  pairSourceStatsQuery,
		"assetSourceStatsQuery": assetSourceStatsQuery,
	} {
		if !strings.Contains(q, "COUNT(DISTINCT ("+marketKeySQL+"))") {
			t.Errorf("%s markets_24h must count marketKeySQL's unordered pair:\n%s", name, q)
		}
	}
}

// TestAssetMarketQueriesFoldOrientation pins the asset-detail market
// readers to canonOrientSQL's folded pair: the count DISTINCTs on it and
// the top-markets preview groups volume and trade count by it.
func TestAssetMarketQueriesFoldOrientation(t *testing.T) {
	canonBase, canonQuote, _ := canonOrientSQL(2)
	if q := assetMarketsCountQuery(); !strings.Contains(q, "SELECT DISTINCT "+canonBase+", "+canonQuote) {
		t.Errorf("GetAssetMarketsCount must DISTINCT on the canonical pair:\n%s", q)
	}
	canonBase, canonQuote, _ = canonOrientSQL(3)
	q := assetTopMarketsQuery()
	folded := "SELECT " + canonBase + " AS base_asset, " + canonQuote + " AS quote_asset"
	if got := strings.Count(q, folded); got != 2 {
		t.Errorf("GetAssetTopMarkets must fold both per_pair_24h and per_pair_count (%d of 2):\n%s", got, q)
	}
	if strings.Contains(q, "GROUP BY base_asset, quote_asset") {
		t.Errorf("GetAssetTopMarkets must not group on the raw stored pair:\n%s", q)
	}
}
