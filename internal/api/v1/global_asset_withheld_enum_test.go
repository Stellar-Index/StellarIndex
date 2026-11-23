package v1

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// TestGlobalAssetView_WithheldReasonSpecEnumCoversEveryVerdict: GlobalAssetView names a refusal with withheldReasonFor, so the
// spec enum must hold every reason any gate verdict maps to; a value outside it breaks clients generated with strict enums.
func TestGlobalAssetView_WithheldReasonSpecEnumCoversEveryVerdict(t *testing.T) {
	enum := specPropertyEnum(t, "GlobalAssetView", "price_withheld_reason")
	for _, w := range []pricingguard.Withholding{
		pricingguard.WithheldThinMarket, pricingguard.WithheldFlaggedIssuer,
		pricingguard.WithheldSourceExcluded, pricingguard.WithheldExclusionsUnread,
	} {
		if r := withheldReasonFor(w); !slices.Contains(enum, string(r)) {
			t.Errorf("verdict %q is served as price_withheld_reason %q, which the GlobalAssetView enum %v omits", w, r, enum)
		}
	}
}
