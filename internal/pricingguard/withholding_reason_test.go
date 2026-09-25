package pricingguard

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A flagged issuer's market is usually thin as well. The verdict must
// name the flag, because the thin-market wording tells the reader to go
// and price the market from its raw trades themselves.
func TestPriceWithholdingFlaggedIssuerBeatsThinMarket(t *testing.T) {
	flagged, _, native := pairLegAssets(t)
	ctx := context.Background()
	thin := newTimedGate(&timedSubstanceReader{
		live: dustSubstance,
		at:   func(time.Time) timescale.MarketSubstance { return dustSubstance },
	})
	scam := NewScamGate(&pairLegDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}}, ScamGateOptions{})
	clean := NewScamGate(&pairLegDirectory{flagged: map[string]bool{}}, ScamGateOptions{})

	if got := (Gate{Substance: thin, Scam: scam}).PriceWithholding(ctx, native, flagged, "test"); got != WithheldFlaggedIssuer {
		t.Errorf("thin AND flagged: PriceWithholding = %q, want %q", got, WithheldFlaggedIssuer)
	}
	if got := (Gate{Substance: thin, Scam: scam}).PriceWithholdingAt(ctx, native, flagged, gateNow, "test"); got != WithheldFlaggedIssuer {
		t.Errorf("thin AND flagged: PriceWithholdingAt = %q, want %q", got, WithheldFlaggedIssuer)
	}
	if got := (Gate{Substance: thin, Scam: clean}).PriceWithholding(ctx, native, flagged, "test"); got != WithheldThinMarket {
		t.Errorf("thin only: PriceWithholding = %q, want %q", got, WithheldThinMarket)
	}
	if got := (Gate{Scam: scam}).PriceWithholding(ctx, native, flagged, "test"); got != WithheldFlaggedIssuer {
		t.Errorf("flagged only: PriceWithholding = %q, want %q", got, WithheldFlaggedIssuer)
	}
	if got := (Gate{Scam: clean}).PriceWithholding(ctx, native, flagged, "test"); got != NotWithheld {
		t.Errorf("neither: PriceWithholding = %q, want NotWithheld", got)
	}
	if !(Gate{Substance: thin, Scam: clean}).PriceWithheld(ctx, native, flagged, "test") || (Gate{Scam: clean}).PriceWithheld(ctx, native, flagged, "test") {
		t.Error("PriceWithheld must agree with PriceWithholding != NotWithheld")
	}
}
