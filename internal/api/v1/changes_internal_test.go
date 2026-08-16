package v1

import (
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestChangeSummaryCoinCandidates pins the slug→canonical expansion
// the /v1/changes/coin/{id} handler relies on. The change-summary
// worker writes rows under the canonical asset_id form (`native`,
// `crypto:XLM`, `USDC-GA5Z…`); a caller passing the friendly slug
// "XLM" would otherwise 404 against a strict-equality lookup. The
// candidate set is documented behavior — locking it in protects
// against silent regressions.
func TestChangeSummaryCoinCandidates(t *testing.T) {
	tests := []struct {
		name       string
		entityType string
		entityID   string
		want       []string
	}{
		{
			name:       "non-coin entity returns input verbatim",
			entityType: "pair",
			entityID:   "native/USDC-GA5Z…",
			want:       []string{"native/USDC-GA5Z…"},
		},
		{
			name:       "non-coin entity returns input verbatim — protocol",
			entityType: "protocol",
			entityID:   "soroswap",
			want:       []string{"soroswap"},
		},
		{
			name:       "XLM expands to native + crypto:XLM + SAC (last)",
			entityType: "coin",
			entityID:   "XLM",
			want:       []string{"XLM", "native", "crypto:XLM", canonical.XLMSacContractID},
		},
		{
			name:       "lowercase xlm uppercases for the alternate forms",
			entityType: "coin",
			entityID:   "xlm",
			want:       []string{"xlm", "native", "crypto:XLM", canonical.XLMSacContractID},
		},
		{
			name:       "native expands to crypto:XLM + SAC (last)",
			entityType: "coin",
			entityID:   "native",
			want:       []string{"native", "crypto:XLM", canonical.XLMSacContractID},
		},
		{
			name:       "bare classic code expands to crypto:CODE",
			entityType: "coin",
			entityID:   "USDC",
			want:       []string{"USDC", "crypto:USDC"},
		},
		{
			name:       "EURC expands like USDC",
			entityType: "coin",
			entityID:   "EURC",
			want:       []string{"EURC", "crypto:EURC"},
		},
		{
			name:       "full asset_id with G-strkey returns input + parsed canonical form (which is identical)",
			entityType: "coin",
			entityID:   "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			// canonical.ParseAsset round-trips this verbatim so the
			// dedup keeps the list to a single entry.
			want: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		},
		{
			name:       "crypto:XLM does not re-add itself; folds in native + SAC (last)",
			entityType: "coin",
			entityID:   "crypto:XLM",
			want:       []string{"crypto:XLM", "native", canonical.XLMSacContractID},
		},
		{
			name:       "XLM SAC C-address falls back to native + crypto:XLM",
			entityType: "coin",
			entityID:   canonical.XLMSacContractID,
			want:       []string{canonical.XLMSacContractID, "native", "crypto:XLM"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := changeSummaryCoinCandidates(tc.entityType, tc.entityID)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("changeSummaryCoinCandidates(%q, %q) = %v, want %v",
					tc.entityType, tc.entityID, got, tc.want)
			}
		})
	}
}
