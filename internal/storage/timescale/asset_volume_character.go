package timescale

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Volume-character rollup (wash-and-scam-signals design §2). Computes the
// per-asset account-structure signals over a trailing window from the
// `trades` hypertable (which carries maker/taker — the ClickHouse lake
// does not) and derives a `volume_character` label distinguishing honest
// market activity from wash-painting and issuer wrap corridors.
//
// This is DISPLAY / ANALYTICS metadata. It does NOT re-rank anything
// (that is design §4, operator-policy-gated) — it only exposes the signals
// §4 would sort on, plus the derived character.

const (
	// volumeCharacterWindow is the trailing window the signals roll over.
	// 14 days matches the forensic window that proved the reported scam
	// AUD wash (design origin note: 108/109 of its 14-day XLM/AUD trades
	// were one wallet pair). Long enough to be robust to a quiet day,
	// short enough that a corridor that has since gone honest recovers.
	volumeCharacterWindow = 14 * 24 * time.Hour

	// VolumeCharacterMarket / …Operational / …Concentrated are the wire
	// values of the derived character.
	VolumeCharacterMarket       = "market"
	VolumeCharacterOperational  = "operational"
	VolumeCharacterConcentrated = "concentrated"

	// volumeCharacterMinVolumeUSD is the floor below which we do not
	// editorialize: too little priced volume to tell a quiet legitimate
	// asset from a pattern, so it stays the neutral default `market`.
	// Mirrors the explorer's market-cap confidence floor ($1k) and stays
	// below the census dust-bot example ($5.5k) so real low-value wash is
	// still caught.
	volumeCharacterMinVolumeUSD = 1000.0

	// volumeCharacterConcentrationThreshold — >90% of window volume in a
	// SINGLE (unordered) account pair on a market-styled pair is the
	// volume-painting / ping-pong / dust signature (design §1 census).
	volumeCharacterConcentrationThreshold = 0.90

	// volumeCharacterMarketStyledThreshold — an asset is "market-styled"
	// when at least half its priced volume trades against a real price
	// surface (native / USDC / fiat) rather than a wrap counterpart.
	volumeCharacterMarketStyledThreshold = 0.50

	// volumeCharacterIssuerSideThreshold — the issuer-side share that,
	// on a NON-market-styled (wrap) pair, marks an operational
	// mint/redeem corridor (USDC↔USDCAllow, AUDD↔AUDR).
	volumeCharacterIssuerSideThreshold = 0.90
)

// AssetVolumeCharacter is the §2 signal set for one asset over the
// trailing window, plus the derived character. Shares are fractions in
// [0,1] rounded to 4 dp.
type AssetVolumeCharacter struct {
	WindowDays int
	// VolumeUSD is the priced (usd_volume non-null) window volume the
	// shares below are computed against.
	VolumeUSD              float64
	DistinctMakers         int64
	DistinctTakers         int64
	TopAccountPairVolShare float64
	SelfCrossShare         float64
	IssuerSideShare        float64
	MarketStyledShare      float64
	IsMarketStyled         bool
	Character              string
}

// assetVolumeCharacterSQL rolls the trailing-window signals in one query.
//   - $1 = alias array of the asset's canonical forms (native/SAC/…),
//     matched on either side (alias-complete, like the sibling stats reads).
//   - $2 = the window as an interval string.
//   - $3 = the asset's issuer G-address (empty for native/soroban/fiat,
//     which collapses the issuer-side predicate to a constant-false no-op).
//
// The (maker,taker) account pair is UNORDERED — LEAST/GREATEST folds the
// two directions of a round-trip into one pair so third-party ping-pong
// (A→B and B→A) reads as the single concentrated pair it economically is,
// not two 50% halves. usd_volume rows only: an unpriced trade contributes
// no volume and can't move a share.
const assetVolumeCharacterSQL = `
WITH w AS (
  SELECT
    maker, taker,
    CASE WHEN base_asset = ANY($1) THEN quote_asset ELSE base_asset END AS counterpart,
    usd_volume::double precision AS v
  FROM trades
  WHERE ts >= now() - $2::interval
    AND (base_asset = ANY($1) OR quote_asset = ANY($1))
    AND usd_volume IS NOT NULL
),
pairs AS (
  SELECT SUM(v) AS pv
  FROM w
  WHERE maker IS NOT NULL AND taker IS NOT NULL
  GROUP BY LEAST(maker, taker), GREATEST(maker, taker)
)
SELECT
  COALESCE((SELECT SUM(v) FROM w), 0)                                              AS total_vol,
  (SELECT COUNT(DISTINCT maker) FROM w WHERE maker IS NOT NULL)                    AS distinct_makers,
  (SELECT COUNT(DISTINCT taker) FROM w WHERE taker IS NOT NULL)                    AS distinct_takers,
  COALESCE((SELECT MAX(pv) FROM pairs), 0)                                         AS top_pair_vol,
  COALESCE((SELECT SUM(v) FROM w WHERE maker IS NOT NULL AND maker = taker), 0)    AS self_cross_vol,
  COALESCE((SELECT SUM(v) FROM w WHERE $3 <> '' AND (maker = $3 OR taker = $3)), 0) AS issuer_side_vol,
  COALESCE((SELECT SUM(v) FROM w
              WHERE counterpart = 'native'
                 OR counterpart LIKE 'fiat:%'
                 OR counterpart LIKE 'USDC-%'), 0)                                 AS market_styled_vol
`

// AssetVolumeCharacter computes the §2 signals + derived character for one
// asset. A newly-observed asset with no priced trades in the window comes
// back as the neutral default `market` with zeroed signals.
func (s *Store) AssetVolumeCharacter(ctx context.Context, assetID string) (AssetVolumeCharacter, error) {
	issuer := ""
	if a, err := canonical.ParseAsset(assetID); err == nil {
		issuer = a.Issuer
	}
	window := fmt.Sprintf("%d hours", int(volumeCharacterWindow.Hours()))

	var (
		total, topPair, selfCross, issuerSide, marketStyled float64
		makers, takers                                      int64
	)
	err := s.db.QueryRowContext(ctx, assetVolumeCharacterSQL,
		assetAliasArray(assetID), window, issuer).
		Scan(&total, &makers, &takers, &topPair, &selfCross, &issuerSide, &marketStyled)
	if err != nil {
		return AssetVolumeCharacter{}, fmt.Errorf("timescale: AssetVolumeCharacter %s: %w", assetID, err)
	}

	out := AssetVolumeCharacter{
		WindowDays:     int(volumeCharacterWindow.Hours()) / 24,
		VolumeUSD:      total,
		DistinctMakers: makers,
		DistinctTakers: takers,
	}
	if total > 0 {
		out.TopAccountPairVolShare = round4(topPair / total)
		out.SelfCrossShare = round4(selfCross / total)
		out.IssuerSideShare = round4(issuerSide / total)
		out.MarketStyledShare = round4(marketStyled / total)
	}
	out.IsMarketStyled = out.MarketStyledShare >= volumeCharacterMarketStyledThreshold
	out.Character = deriveVolumeCharacter(out)
	return out, nil
}

// deriveVolumeCharacter maps the signals to a character. Pure — the design
// §1 taxonomy table is its test oracle, and it classifies every row of it:
//
//	species                | signature                          | character
//	-----------------------|------------------------------------|-------------
//	Operational corridor   | issuer-side, wrap/redeem pair       | operational
//	Volume-painting wash   | market-styled, issuer sole counter. | concentrated
//	Third-party ping-pong  | two non-issuer wallets round-trip   | concentrated
//	Dust-bot               | one account pair, micro trades      | concentrated
//
// The design's discriminator: high issuer-side share ALONE is not wash — a
// stablecoin mint/redeem corridor is 100% issuer-side by nature. Wash is
// issuer-side (or not) AND single-counterparty AND a market-styled pair.
//
//   - Below the volume floor → `market` (insufficient signal; never badge a
//     quiet asset).
//   - market-styled AND one account pair owns >90% of volume →
//     `concentrated` (volume-painting wash, dust-bot on native/USDC/fiat).
//   - NOT market-styled (wrap pair) AND overwhelmingly issuer-side →
//     `operational` (mint/redeem corridor: USDC↔USDCAllow, AUDD↔AUDR).
//   - one account pair owns >90% but the issuer is NOT the counterparty →
//     `concentrated` (third-party ping-pong on a wrap-styled pair — one
//     pair paints the volume, but it isn't an issuer corridor, so it's
//     fabricated, not operational).
//   - otherwise → `market`.
func deriveVolumeCharacter(s AssetVolumeCharacter) string {
	if s.VolumeUSD < volumeCharacterMinVolumeUSD {
		return VolumeCharacterMarket
	}
	concentrated := s.TopAccountPairVolShare >= volumeCharacterConcentrationThreshold
	issuerCorridor := s.IssuerSideShare >= volumeCharacterIssuerSideThreshold
	if s.IsMarketStyled && concentrated {
		return VolumeCharacterConcentrated
	}
	if !s.IsMarketStyled && issuerCorridor {
		return VolumeCharacterOperational
	}
	if concentrated && !issuerCorridor {
		return VolumeCharacterConcentrated
	}
	return VolumeCharacterMarket
}

// round4 rounds a share to 4 decimal places for a stable wire value.
func round4(f float64) float64 {
	return math.Round(f*1e4) / 1e4
}
