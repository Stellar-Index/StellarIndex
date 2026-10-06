// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSourceStatsQueries_SACLiteralMatchesCanonical: the per-source
// breakdowns reach every XLM form, the SAC included, through the bound
// `= ANY($n)` form set built from [canonical.AssetAliases], never a
// hard-coded literal.
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
