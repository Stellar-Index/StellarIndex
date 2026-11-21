package v1

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestApplyAssetRowToDetail_ReaderVerdictIsFinal — the catalogue row's price derives through the leg the gated reader refused,
// so an allowed own-pair verdict must neither fill price_usd nor clear the withheld reason, and the pills go with it. A
// substance verdict is the one the own-pair gate re-judges (TestIncludeThinCatalogueBeatsThinF2).
func TestApplyAssetRowToDetail_ReaderVerdictIsFinal(t *testing.T) {
	asset, err := canonical.ParseAsset("TKN-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []PriceWithheldReason{PriceWithheldUpstreamLeg, PriceWithheldUnattributed, PriceWithheldScamIssuer} {
		t.Run(string(reason), func(t *testing.T) {
			price, pct := "1.25", "3.0"
			row := timescale.AssetRow{PriceUSD: &price, Change1hPct: &pct, Change7dPct: &pct}
			detail := AssetDetail{AssetID: asset.String(), PriceWithheldReason: reason}
			s := &Server{logger: slog.New(slog.DiscardHandler)}
			s.applyAssetRowToDetail(context.Background(), &detail, asset, row, nil, asset.String())
			if detail.PriceUSD != nil || detail.PriceWithheldReason != reason {
				t.Errorf("price_usd = %q reason = %q, want withheld as %q", strOrEmpty(detail.PriceUSD), detail.PriceWithheldReason, reason)
			}
			if detail.Change1hPct != nil || detail.Change7dPct != nil {
				t.Errorf("change pills derive from the withheld price and must not serve: 1h=%v 7d=%v", detail.Change1hPct, detail.Change7dPct)
			}
		})
	}
	t.Run("no verdict fills", func(t *testing.T) {
		price := "1.25"
		detail := AssetDetail{AssetID: asset.String()}
		s := &Server{logger: slog.New(slog.DiscardHandler)}
		s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{PriceUSD: &price}, nil, asset.String())
		if detail.PriceUSD == nil {
			t.Error("an unwithheld detail must still take the row price")
		}
	})
}
