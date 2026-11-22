package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// TestWalkWithheldReason_SourceExcludedPassesThrough: an exclusion on the peg pair is the asset's own market refused by the
// operator, so the walk reports it as such (outranking every other reason) rather than as a refused upstream leg.
func TestWalkWithheldReason_SourceExcludedPassesThrough(t *testing.T) {
	excluded := PriceWithheldError(pricingguard.WithheldSourceExcluded)
	for _, have := range []PriceWithheldReason{"", PriceWithheldUnattributed, PriceWithheldUpstreamLeg, PriceWithheldSubstance} {
		if got := walkWithheldReason(have, excluded); got != PriceWithheldSourceExcluded {
			t.Errorf("walkWithheldReason(%q, source_excluded) = %q, want source_excluded", have, got)
		}
	}
	if got := walkWithheldReason(PriceWithheldSourceExcluded, PriceWithheldError(pricingguard.WithheldThinMarket)); got != PriceWithheldSourceExcluded {
		t.Errorf("a later substance refusal displaced source_excluded: %q", got)
	}
	if got := walkWithheldReason("", PriceWithheldError(pricingguard.WithheldFlaggedIssuer)); got != PriceWithheldUpstreamLeg {
		t.Errorf("a flagged peg issuer = %q, want upstream_leg", got)
	}
}
