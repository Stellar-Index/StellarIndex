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

	if got, _ := (Gate{Substance: thin, Scam: scam}).PushWithholding(ctx, native, flagged, "test"); got != WithheldFlaggedIssuer {
		t.Errorf("thin AND flagged: PushWithholding = %q, want %q", got, WithheldFlaggedIssuer)
	}
	at := Query{PointInTime: true, At: gateNow}
	if got := (Gate{Substance: thin, Scam: scam}).Judge(ctx, native, flagged, "test", at).Withholding; got != WithheldFlaggedIssuer {
		t.Errorf("thin AND flagged: point-in-time Judge = %q, want %q", got, WithheldFlaggedIssuer)
	}
	if got, _ := (Gate{Substance: thin, Scam: clean}).PushWithholding(ctx, native, flagged, "test"); got != WithheldThinMarket {
		t.Errorf("thin only: PushWithholding = %q, want %q", got, WithheldThinMarket)
	}
	if got, _ := (Gate{Scam: scam}).PushWithholding(ctx, native, flagged, "test"); got != WithheldFlaggedIssuer {
		t.Errorf("flagged only: PushWithholding = %q, want %q", got, WithheldFlaggedIssuer)
	}
	if got, err := (Gate{Scam: clean}).PushWithholding(ctx, native, flagged, "test"); got != NotWithheld || err != nil {
		t.Errorf("neither: PushWithholding = (%q, %v), want NotWithheld", got, err)
	}
}
