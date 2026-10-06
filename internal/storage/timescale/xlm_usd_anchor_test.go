// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestXLMUSDAnchor_PopulationIsAliasAndDirectionComplete pins the anchor's
// population to XLM's alias forms in canonical priority order (SAC last, the
// order the precedence pick depends on) against every USD proxy, read in both
// stored directions and weighted by volume_usd.
func TestXLMUSDAnchor_PopulationIsAliasAndDirectionComplete(t *testing.T) {
	t.Parallel()
	const sac = "'" + canonical.XLMSacContractID + "'"
	aliases := canonical.AssetAliasStrings(canonical.NativeAsset())
	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = "'" + a + "'"
	}
	if got, want := xlmUSDAnchorForms(sac), strings.Join(quoted, ", "); got != want {
		t.Errorf("xlmUSDAnchorForms = %s, want canonical.AssetAliases order %s", got, want)
	}
	if grid := xlmUSDAnchorGridCTE("g", "now()", sac); !strings.Contains(grid, "(VALUES (1), (2), (3))") || len(aliases) != 3 {
		t.Errorf("anchor grid enumerates a form_rank per alias form; aliases = %d", len(aliases))
	}

	pop := strings.Join(strings.Fields(xlmUSDAnchorMinutes("now()", "now()", sac)), " ")
	forms, usd := xlmUSDAnchorForms(sac), strings.Join(strings.Fields(usdProxyQuotes), " ")
	for _, want := range []string{
		"WHERE base_asset IN (" + forms + ") AND quote_asset IN (" + usd + ")",
		"WHERE quote_asset IN (" + forms + ") AND base_asset IN (" + usd + ")",
		"bucket, 1 / vwap, volume_usd",
		"sum(px * volume_usd) / sum(volume_usd)",
		"AND bucket <= now() - INTERVAL '1 minute'",
		"volume_usd >= " + xlmUSDAnchorMinUSD,
	} {
		if !strings.Contains(pop, want) {
			t.Errorf("anchor population lacks %q", want)
		}
	}
	if strings.Contains(pop, "volume_priced") {
		t.Error("anchor weights by leg sums; CEX (1e8) and on-chain (1e7) legs are not comparable — weight by volume_usd")
	}
	// Sargable: nothing wraps bucket in the population's WHERE.
	if loc := regexp.MustCompile(`\w\(\s*bucket\b`).FindStringIndex(pop); loc != nil {
		t.Errorf("anchor population applies a function to bucket at byte %d", loc[0])
	}
}

// TestXLMUSDAnchor_ShapesShareOnePopulation pins every renderer — the pick,
// the instant, the grid and the per-period CTE — to the same population and
// the same precedence (form first, then newest minute).
func TestXLMUSDAnchor_ShapesShareOnePopulation(t *testing.T) {
	t.Parallel()
	const sac = "$2::text"
	pop := xlmUSDAnchorMinutes("LO", "HI", sac)
	popBody := pop[:strings.Index(pop, "bucket >= LO")]
	for name, q := range map[string]string{
		"pick":      xlmUSDAnchorPick("LO", "HI", sac),
		"at":        xlmUSDAnchorAt("TS", sac),
		"grid":      xlmUSDAnchorGridCTE("g", "LO", sac),
		"perPeriod": xlmUSDAnchorPerPeriodCTE("p", "h", "hour", "LO", sac),
	} {
		if !strings.Contains(q, popBody) {
			t.Errorf("%s does not read the shared anchor population", name)
		}
	}
	if at := xlmUSDAnchorAt("TS", sac); !strings.Contains(at, "bucket >= (TS) - INTERVAL '"+xlmUSDAnchorMaxAge+"'") ||
		!strings.Contains(at, "ORDER BY xm.form_rank, xm.bucket DESC") {
		t.Error("xlmUSDAnchorAt must take the first form with a minute within xlmUSDAnchorMaxAge, newest first")
	}
	grid := xlmUSDAnchorGridCTE("g", "LO", sac)
	for _, want := range []string{
		"bucket >= (LO) - INTERVAL '" + xlmUSDAnchorMaxAge + "'",
		"WHERE bucket >= minute - INTERVAL '" + xlmUSDAnchorMaxAge + "'",
		"ORDER BY minute, form_rank",
	} {
		if !strings.Contains(grid, want) {
			t.Errorf("grid lacks %q", want)
		}
	}
	if !strings.Contains(xlmUSDAnchorPerPeriodCTE("p", "h", "hour", "LO", sac), "ORDER BY 1, xm.form_rank, xm.bucket DESC") {
		t.Error("per-period anchor must prefer form over recency")
	}
}

// TestXLMUSDAnchor_RollupGridReachesOldestArm pins the rollup's grid to start
// at the oldest asset_vs_xlm* window (the 7d arm's lower bound), so an anchor
// minute up to xlmUSDAnchorMaxAge before that bound still seeds the fill.
func TestXLMUSDAnchor_RollupGridReachesOldestArm(t *testing.T) {
	t.Parallel()
	if !strings.Contains(assetPriceCTEs, xlmUSDAnchorGridCTE("xlm_usd_grid", priceWindow7dLo, "'"+nativeXLMSAC+"'")) {
		t.Error("rollup does not build its anchor grid from the 7d arm's lower bound")
	}
	if !strings.Contains(refreshAssetPriceSnapshotUpsert, priceArmJoins(xlmUSDGridJoin)) {
		t.Error("rollup does not join the anchor grid at each XLM arm's own minute")
	}
}

// TestNoBespokeUSDProxyList fails on any IN-list of USD-proxy literals that
// is not exactly usdProxyQuotes. A hand-copied list is how the XLM/USD anchor
// lost the USDC SAC (and fiat:USD/USDC drifted) in five places.
func TestNoBespokeUSDProxyList(t *testing.T) {
	t.Parallel()
	want := strings.Join(strings.Fields(usdProxyQuotes), " ")
	inList := regexp.MustCompile(`(?s)(?:quote_asset|base_asset)\s+IN\s*\(\s*('[^)]*)\)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	api, err := filepath.Glob("../../api/v1/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(files, api...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		// The volume-only scalar moves to trade-time usd_volume with the
		// volume read paths; it is not a price anchor.
		skipFrom, skipTo := -1, -1
		if i := strings.Index(src, "const xlmUSDVolumeSelect = `"); i >= 0 {
			skipFrom = i
			skipTo = i + len("const xlmUSDVolumeSelect = `") + strings.Index(src[i+len("const xlmUSDVolumeSelect = `"):], "`")
		}
		for _, m := range inList.FindAllStringSubmatchIndex(src, -1) {
			list := strings.Join(strings.Fields(src[m[2]:m[3]]), " ")
			if !strings.Contains(list, "USDC-GA5Z") && !strings.Contains(list, "'fiat:USD'") && !strings.Contains(list, "CCW67TSZ") {
				continue
			}
			if m[0] >= skipFrom && m[0] < skipTo {
				continue
			}
			if list != want {
				t.Errorf("%s:%d: USD-proxy IN-list %s is not usdProxyQuotes; splice the constant",
					f, strings.Count(src[:m[0]], "\n")+1, list)
			}
		}
	}
}
