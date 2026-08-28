package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestProxyQuoteLists_Lockstep pins the four places that decide "what is
// a USD/XLM proxy" to ONE set, and pins the XLM leg to being read in
// BOTH stored directions everywhere the catalogue prices it — the 8
// headline-price CTEs AND the 4 price-history series CTEs.
//
// The lists live in: the coverage tripwire (coverageQuoteProxies), the
// transitive resolver (usdProxyQuotes + xlmQuotes), and the two
// catalogue queries' literal IN-lists (listAssetsBaseSelect +
// getAssetBySlugSQL). An asset "priced" by one and "priceless" by
// another is exactly how the popular-priceless alert fires forever on
// an asset the catalogue could price — or, as on r1 2026-08-28, how an
// asset the volume path valued at $730k/7d served no price at all.
func TestProxyQuoteLists_Lockstep(t *testing.T) {
	t.Parallel()

	if nativeXLMSAC != canonical.XLMSacContractID {
		t.Fatalf("nativeXLMSAC = %s, want canonical.XLMSacContractID %s", nativeXLMSAC, canonical.XLMSacContractID)
	}

	xlmList := "'native', '" + canonical.XLMSacContractID + "'"
	if xlmQuotes != xlmList {
		t.Errorf("xlmQuotes = %q, want %q", xlmQuotes, xlmList)
	}
	for _, member := range []string{
		"'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN'",
		"'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75'",
		"'fiat:USD'",
	} {
		if !strings.Contains(usdProxyQuotes, member) {
			t.Errorf("usdProxyQuotes lacks %s", member)
		}
		if !strings.Contains(coverageQuoteProxies, member) {
			t.Errorf("coverageQuoteProxies lacks %s", member)
		}
		// Every catalogue direct_usd* CTE (4 per query) lists it.
		for name, sql := range map[string]string{"listing": listAssetsBaseSelect, "detail": getAssetBySlugSQL} {
			if got := strings.Count(sql, member); got < 4 {
				t.Errorf("%s SQL mentions %s %d times, want >= 4 (one per direct_usd* CTE)", name, member, got)
			}
		}
	}
	if !strings.Contains(coverageQuoteProxies, xlmList) {
		t.Errorf("coverageQuoteProxies lacks the XLM forms %s", xlmList)
	}

	// The XLM leg in both directions, in all 8 catalogue CTEs: the
	// base-side arm (quote_asset IN xlm) AND the inverted arm
	// (base_asset IN xlm). Counting the INVERTED arm is what makes this
	// test red against the pre-fix catalogue: it had 4 base-side arms
	// per query and zero inverted ones.
	for name, sql := range map[string]string{"listing": listAssetsBaseSelect, "detail": getAssetBySlugSQL} {
		// The detail query column-aligns its predicates ("base_asset  IN");
		// fold runs of spaces so the count is about the predicate, not
		// the indentation.
		sql = strings.Join(strings.Fields(sql), " ")
		if got := strings.Count(sql, "quote_asset IN ("+xlmList+")"); got != 4 {
			t.Errorf("%s SQL: base-side XLM arms = %d, want 4 (asset_vs_xlm, _1h, _24h, _7d)", name, got)
		}
		if got := strings.Count(sql, "base_asset IN ("+xlmList+")"); got != 4 {
			t.Errorf("%s SQL: inverted XLM arms (base_asset IN xlm) = %d, want 4 — "+
				"a market stored as (XLM-SAC, asset) is invisible without them", name, got)
		}
	}

	// The four price-history SERIES queries (24h/7d, single + batch)
	// carry one XLM-leg CTE each (asset_xlm_per_hour / _per_day). Each
	// must read both directions too: with only the base-side arm an
	// asset whose XLM market is SAC-as-base had a headline price (the 8
	// arms above) but an EMPTY sparkline. Same red-maker as above —
	// pre-fix these had 1 base-side arm and 0 inverted arms each.
	for name, sql := range map[string]string{
		"history24h":      getAssetPriceHistory24hSQL,
		"history7d":       getAssetPriceHistory7dSQL,
		"history24hBatch": getAssetsPriceHistory24hBatchSQL,
		"history7dBatch":  getAssetsPriceHistory7dBatchSQL,
	} {
		sql = strings.Join(strings.Fields(sql), " ")
		if got := strings.Count(sql, "quote_asset IN ("+xlmList+")"); got != 1 {
			t.Errorf("%s SQL: base-side XLM arms = %d, want 1 (asset_xlm_per_*)", name, got)
		}
		if got := strings.Count(sql, "base_asset IN ("+xlmList+") AND quote_asset = ANY($1)"); got != 1 {
			t.Errorf("%s SQL: inverted XLM arms (base_asset IN xlm AND quote_asset = ANY($1)) = %d, want 1 — "+
				"a market stored as (XLM-SAC, asset) yields an empty sparkline without it", name, got)
		}
		// Base-side preferred per bucket: the inverted flag must be the
		// FIRST key of the per-bucket pick after any partition columns,
		// i.e. ahead of bucket DESC — otherwise a fresher inverted row
		// would silently replace a base-side point (byte-identity).
		if !strings.Contains(sql, "inverted, prio, bucket DESC") && !strings.Contains(sql, "inverted, bucket DESC") {
			t.Errorf("%s SQL: per-bucket pick does not order inverted ahead of bucket DESC", name)
		}
	}

	// The tripwire's priced_direct must seed the proxies themselves (so
	// one_hop can route THROUGH the XLM SAC) and read the inverted arm.
	if !strings.Contains(popularPricelessCandidatesSQL, "SELECT unnest(ARRAY["+coverageQuoteProxies+"])") {
		t.Error("priced_direct does not seed the proxy assets — one_hop cannot route through the XLM SAC")
	}
	if !strings.Contains(popularPricelessCandidatesSQL, "AND base_asset IN ("+xlmList+")") {
		t.Error("priced_direct lacks the inverted XLM arm")
	}
}
