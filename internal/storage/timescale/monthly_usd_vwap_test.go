package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The monthly fold keys every spelling of an asset onto ONE canonical
// id: all three XLM forms land under 'native' (the row an XLM-quoted
// asset will be priced through), a classic id is its own key, and a
// spelling that is not an asset id at all folds with nothing.
func TestMonthlyUSDVWAPAsset_FoldsEverySpellingOntoTheCanonicalForm(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for raw, want := range map[string]string{
		"native":                   "native",
		"crypto:XLM":               "native",
		canonical.XLMSacContractID: "native",
		usdc:                       usdc,
		"not an asset id":          "not an asset id",
	} {
		if got := monthlyUSDVWAPAsset(raw); got != want {
			t.Errorf("monthlyUSDVWAPAsset(%q) = %q, want %q", raw, got, want)
		}
	}
}
