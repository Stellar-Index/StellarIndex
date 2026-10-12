package timescale

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Volume-character rollup: per-asset account-structure signals over a trailing window, read
// from the `trades` hypertable (which carries maker/taker; the ClickHouse lake does not), and
// a derived `volume_character` label separating honest market activity from wash-painting and
// issuer wrap corridors. DISPLAY / ANALYTICS metadata only; it never re-ranks anything.

const (
	// volumeCharacterWindow is the trailing window: long enough to survive a quiet day, short
	// enough that a corridor that has since gone honest recovers.
	volumeCharacterWindow = 14 * 24 * time.Hour

	// VolumeCharacterMarket, ...Operational and ...Concentrated are the wire values.
	VolumeCharacterMarket       = "market"
	VolumeCharacterOperational  = "operational"
	VolumeCharacterConcentrated = "concentrated"

	// volumeCharacterMinVolumeUSD is the floor below which an asset stays the neutral `market`:
	// too little priced volume to tell a quiet asset from a pattern. It sits below the known
	// dust-bot volume so real low-value wash is still caught.
	volumeCharacterMinVolumeUSD = 1000

	// volumeCharacterConcentrationThreshold — over 90% of window volume in a SINGLE (unordered)
	// account pair on a market-styled pair is the volume-painting / ping-pong / dust signature.
	volumeCharacterConcentrationThreshold = 0.90

	// volumeCharacterMarketStyledThreshold — "market-styled" means at least half the priced
	// volume trades against a real price surface (native / USDC / fiat), not a wrap counterpart.
	volumeCharacterMarketStyledThreshold = 0.50

	// volumeCharacterIssuerSideThreshold — the issuer-side share that, on a NON-market-styled
	// (wrap) pair, marks an operational mint/redeem corridor (USDC to USDCAllow, AUDD to AUDR).
	volumeCharacterIssuerSideThreshold = 0.90
)

// AssetVolumeCharacter is the signal set for one asset over the trailing window plus the derived character.
type AssetVolumeCharacter struct {
	WindowDays int
	// VolumeUSD is the priced window volume the shares are computed against: the exact NUMERIC
	// sum's decimal text, never a float (ADR-0003).
	VolumeUSD              string
	DistinctMakers         int64
	DistinctTakers         int64
	TopAccountPairVolShare float64
	SelfCrossShare         float64
	IssuerSideShare        float64
	MarketStyledShare      float64
	IsMarketStyled         bool
	Character              string
}

// trades.maker is either the resting-offer account (G-strkey) or the hex pool id on a classic
// liquidity-pool fill. A pool is not an account, so both queries read maker through these shared
// fragments: maker is kept only when it is an account, so a pool never counts as a distinct maker,
// self-cross or issuer-side leg. A row with one known account is keyed on that account, so one
// account round-tripping through pools is one concentrated actor; rows with no account stay out of
// the numerator. taker may be a contract (C...) on a Soroban swap, so distinct_takers counts
// addresses.
const (
	volumeCharacterMakerSQL = `CASE WHEN maker ~ '^G[A-Z2-7]{55}$' THEN maker END AS maker,
    (maker IS NOT NULL AND maker !~ '^G[A-Z2-7]{55}$') AS pool_fill`
	volumeCharacterPairFilterSQL = `(maker IS NOT NULL OR taker IS NOT NULL)`
	volumeCharacterPairKeySQL    = `LEAST(COALESCE(maker, taker), taker), GREATEST(COALESCE(maker, taker), taker)`
)

// assetVolumeCharacterSQL rolls the trailing-window signals in one query. $1 is the asset's alias
// array (matched on either side), $2 the window interval, $3 the issuer G-address (empty for
// native/soroban/fiat, which makes the issuer-side predicate constant-false).
//
// The (maker,taker) pair is UNORDERED: LEAST/GREATEST folds both directions of a round-trip
// into one pair, so A-B and B-A ping-pong reads as one concentrated pair, not two 50% halves.
// Only usd_volume rows count; an unpriced trade cannot move a share.
const assetVolumeCharacterSQL = `
WITH w AS (
  SELECT
    ` + volumeCharacterMakerSQL + `,
    taker,
    CASE WHEN base_asset = ANY($1) THEN quote_asset ELSE base_asset END AS counterpart,
    usd_volume::double precision AS v,
    usd_volume AS v_num
  FROM trades
  WHERE ts >= now() - $2::interval
    AND (base_asset = ANY($1) OR quote_asset = ANY($1))
    AND usd_volume IS NOT NULL
),
pairs AS (
  SELECT SUM(v) AS pv
  FROM w
  WHERE ` + volumeCharacterPairFilterSQL + `
  GROUP BY ` + volumeCharacterPairKeySQL + `
)
SELECT
  COALESCE((SELECT SUM(v) FROM w), 0)                                              AS total_vol,
  COALESCE((SELECT SUM(v_num) FROM w), 0)::text                                    AS total_vol_num,
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

// AssetVolumeCharacter computes the signals and character for one asset; an asset with no priced trades in the window is the neutral `market` with zeroed signals.
func (s *Store) AssetVolumeCharacter(ctx context.Context, assetID string) (AssetVolumeCharacter, error) {
	issuer := ""
	if a, err := canonical.ParseAsset(assetID); err == nil {
		issuer = a.Issuer
	}
	window := fmt.Sprintf("%d hours", int(volumeCharacterWindow.Hours()))

	var (
		total, topPair, selfCross, issuerSide, marketStyled float64
		totalNum                                            string
		makers, takers                                      int64
	)
	err := s.db.QueryRowContext(ctx, assetVolumeCharacterSQL,
		assetAliasArray(assetID), window, issuer).
		Scan(&total, &totalNum, &makers, &takers, &topPair, &selfCross, &issuerSide, &marketStyled)
	if err != nil {
		return AssetVolumeCharacter{}, fmt.Errorf("timescale: AssetVolumeCharacter %s: %w", assetID, err)
	}

	// Shared with RefreshAssetVolumeCharacter so the all-asset rollup cannot drift from this
	// per-request read.
	return volumeCharacterFromSums(totalNum, total, topPair, selfCross, issuerSide, marketStyled, makers, takers), nil
}

// deriveVolumeCharacter maps the signals to a character. Pure; the taxonomy is its test oracle:
//
//	species                | signature                          | character
//	-----------------------|------------------------------------|-------------
//	Operational corridor   | issuer-side, wrap/redeem pair       | operational
//	Volume-painting wash   | market-styled, issuer sole counter. | concentrated
//	Third-party ping-pong  | two non-issuer wallets round-trip   | concentrated
//	Dust-bot               | one account pair, micro trades      | concentrated
//
// High issuer-side share ALONE is not wash: a stablecoin mint/redeem corridor is 100%
// issuer-side by nature. Wash is single-counterparty AND market-styled. In order:
//   - below the volume floor: `market` (never badge a quiet asset).
//   - market-styled AND one account pair owns >90% of volume: `concentrated`.
//   - not market-styled (wrap pair) AND overwhelmingly issuer-side: `operational`.
//   - one pair owns >90% but the issuer is not the counterparty: `concentrated`.
//   - otherwise `market`.
func deriveVolumeCharacter(s AssetVolumeCharacter) string {
	if belowVolumeCharacterFloor(s.VolumeUSD) {
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

// belowVolumeCharacterFloor compares the exact decimal volume to the floor in big.Rat;
// unparseable text is below it (no readable signal, no badge).
func belowVolumeCharacterFloor(volumeUSD string) bool {
	v, ok := new(big.Rat).SetString(volumeUSD)
	return !ok || v.Cmp(big.NewRat(volumeCharacterMinVolumeUSD, 1)) < 0
}

// round4 rounds a share to 4 decimal places for a stable wire value.
func round4(f float64) float64 {
	return math.Round(f*1e4) / 1e4
}
