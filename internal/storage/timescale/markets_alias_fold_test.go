package timescale

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

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
