package v1

import (
	"context"
	"strconv"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// VolumeCharacterReader reads the pre-computed per-asset trailing-window
// account-structure signals + derived volume_character (design §2) from the
// asset_volume_character rollup (migration 0149) — a keyed-on-PK lookup,
// folded to the asset's canonical form. Production impl is *timescale.Store.
//
// It REPLACES the pre-rollup per-request trades roll, which measured 4.09s
// on the USDC detail and tripped a 4s timeout, returning null: the rollup
// worker moved that compute off the request path. A nil reader, a
// fiat/native-only asset, a rollup miss (found=false), or a lookup error
// all leave the fields omitted and never fail the asset response.
type VolumeCharacterReader interface {
	AssetVolumeCharacterRollup(ctx context.Context, assetID string) (timescale.AssetVolumeCharacter, bool, error)
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
	// presence — they never carry a rolled row. Skip cleanly.
	if asset.Type == canonical.AssetFiat {
		return
	}
	// Keyed-on-PK rollup lookup (~instant) — the pre-rollup 4s per-request
	// trades roll (and its timeout) is gone.
	vc, found, err := s.volumeCharacter.AssetVolumeCharacterRollup(ctx, asset.String())
	if err != nil {
		s.logger.Debug("volume character rollup lookup failed", "asset_id", asset.String(), "err", err)
		return
	}
	if !found {
		// No rolled row (no priced trades in the window, or the worker
		// hasn't run yet) — omit the fields, same best-effort posture as a
		// lookup miss.
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
