// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A trade with NULL usd_volume must never be valued at today's XLM price:
// a historical per-source figure would then move with XLM spot. Unpriced
// trades are excluded and reported so the API can mark a lower bound.
func TestSourceStatsQueryDoesNotValueAtSpot(t *testing.T) {
	q := sourceStatsQuery()
	for _, bad := range []string{"xlm_usd", "vwap", "base_amount /", "quote_amount /"} {
		if strings.Contains(q, bad) {
			t.Errorf("sourceStatsQuery must not value unpriced trades at spot; found %q:\n%s", bad, q)
		}
	}
	if !strings.Contains(q, "unpriced_trades") {
		t.Errorf("sourceStatsQuery must report the excluded unpriced trade count:\n%s", q)
	}
	assertDEXNumericSafe(t, "source stats", q)
}

func TestSourceVolumeHistoryQueryDoesNotValueAtSpot(t *testing.T) {
	q := sourceVolumeHistoryQuery()
	for _, bad := range []string{"xlm_usd", "vwap", "10000000"} {
		if strings.Contains(q, bad) {
			t.Errorf("sourceVolumeHistoryQuery must not value XLM legs at spot; found %q:\n%s", bad, q)
		}
	}
	for _, want := range []string{"sum_usd_priced", "sum_xlm_base", "sum_xlm_quote", "xlm_unpriced"} {
		if !strings.Contains(q, want) {
			t.Errorf("sourceVolumeHistoryQuery must read %q:\n%s", want, q)
		}
	}
}

// The per-pair and per-asset breakdowns follow GetSourceStats: trade-time
// usd_volume only, with the excluded unpriced trades counted.
func TestPairAssetSourceStatsQueriesDoNotValueAtSpot(t *testing.T) {
	for name, q := range map[string]string{
		"pairSourceStatsQuery":  pairSourceStatsQuery,
		"assetSourceStatsQuery": assetSourceStatsQuery,
	} {
		for _, bad := range []string{"xlm_usd", "vwap", "base_amount", "quote_amount"} {
			if strings.Contains(q, bad) {
				t.Errorf("%s must not value unpriced trades at spot; found %q:\n%s", name, bad, q)
			}
		}
		if !strings.Contains(q, "COUNT(*) FILTER (WHERE usd_volume IS NULL)::bigint AS unpriced_trades_24h") {
			t.Errorf("%s must report the excluded unpriced trade count:\n%s", name, q)
		}
	}
}

// TestSourceStatsQueries_SACLiteralMatchesCanonical: per-source breakdowns reach every XLM form, the SAC
// included, through the bound `= ANY($n)` set built from [canonical.AssetAliases], never a hard-coded
// literal.
func TestSourceStatsQueries_SACLiteralMatchesCanonical(t *testing.T) {
	for name, q := range map[string]string{
		"pairSourceStatsQuery":  pairSourceStatsQuery,
		"assetSourceStatsQuery": assetSourceStatsQuery,
	} {
		if strings.Contains(q, canonical.XLMSacContractID) {
			t.Errorf("%s hard-codes the pubnet XLM SAC", name)
		}
		// The other forms of the family must be reachable through
		// the bound array, not hard-coded — the filter is `= ANY($n)`.
		if !strings.Contains(q, "= ANY($1)") {
			t.Errorf("%s no longer binds the form set with `= ANY($1)`; the alias expansion "+
				"is how `native`/`crypto:XLM`/SAC all reach the same aggregate", name)
		}
	}

	// The alias family the handler binds must contain the native SAC.
	forms := canonical.AssetAliasStrings(canonical.NativeAsset())
	found := false
	for _, f := range forms {
		if f == canonical.NativeSACContractID() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AssetAliasStrings(native) = %v, missing the bound native SAC %s — "+
			"Soroban XLM volume would never be selected", forms, canonical.NativeSACContractID())
	}
}
