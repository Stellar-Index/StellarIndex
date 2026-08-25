package v1

import (
	"context"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// volumeCharacterTimeout caps the trailing-window account-structure roll
// on /v1/assets/{id}. One bounded trades scan; the detail response cache
// (120s) amortises it across repeat requests.
const volumeCharacterTimeout = 4 * time.Second

// VolumeCharacterReader computes the per-asset trailing-window
// account-structure signals + derived volume_character (design §2).
// Production impl is *timescale.Store (the maker/taker trades live in
// Timescale, not the ClickHouse lake). Nil omits the fields.
type VolumeCharacterReader interface {
	AssetVolumeCharacter(ctx context.Context, assetID string) (timescale.AssetVolumeCharacter, error)
}

// AssetVolumeCharacterSignals is the wire form of the §2 signals. Shares
// are fractions in [0,1] (4 dp). volume_usd is the priced window volume
// the shares are computed against, as a decimal string.
type AssetVolumeCharacterSignals struct {
	WindowDays             int     `json:"window_days"`
	VolumeUSD              string  `json:"volume_usd"`
	DistinctMakers         int64   `json:"distinct_makers"`
	DistinctTakers         int64   `json:"distinct_takers"`
	TopAccountPairVolShare float64 `json:"top_account_pair_vol_share"`
	SelfCrossShare         float64 `json:"self_cross_share"`
	IssuerSideShare        float64 `json:"issuer_side_share"`
	MarketStyledShare      float64 `json:"market_styled_share"`
	IsMarketStyled         bool    `json:"is_market_styled"`
}

// applyVolumeCharacter stamps volume_character + its signals onto the
// detail. Best-effort: a nil reader, a fiat/native-only asset with no
// trades, or a lookup failure leaves the fields omitted and never fails
// the asset response. ANALYTICS-only — it reads nothing from and writes
// nothing to the price/verification/gate surfaces.
func (s *Server) applyVolumeCharacter(ctx context.Context, detail *AssetDetail, asset canonical.Asset) {
	if s.volumeCharacter == nil {
		return
	}
	// fiat:* assets are off-chain reference rows with no trades-table
	// presence — the roll would always be empty. Skip cleanly.
	if asset.Type == canonical.AssetFiat {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, volumeCharacterTimeout)
	defer cancel()

	vc, err := s.volumeCharacter.AssetVolumeCharacter(cctx, asset.String())
	if err != nil {
		s.logger.Debug("volume character lookup failed", "asset_id", asset.String(), "err", err)
		return
	}
	detail.VolumeCharacter = vc.Character
	detail.VolumeCharacterSignals = &AssetVolumeCharacterSignals{
		WindowDays:             vc.WindowDays,
		VolumeUSD:              strconv.FormatFloat(vc.VolumeUSD, 'f', 2, 64),
		DistinctMakers:         vc.DistinctMakers,
		DistinctTakers:         vc.DistinctTakers,
		TopAccountPairVolShare: vc.TopAccountPairVolShare,
		SelfCrossShare:         vc.SelfCrossShare,
		IssuerSideShare:        vc.IssuerSideShare,
		MarketStyledShare:      vc.MarketStyledShare,
		IsMarketStyled:         vc.IsMarketStyled,
	}
}
