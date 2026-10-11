package aggregate

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PriceAuthority labels what produced a global-view price, served as /v1/assets/{slug}.price_authority.
type PriceAuthority string

const (
	// AuthorityVWAPNative is our own VWAP across exchange-class trades, the gold standard.
	AuthorityVWAPNative PriceAuthority = "vwap_native"

	// AuthorityAggregatorAvg is a simple average across Class:Aggregator sources, which never feed VWAP (already aggregated upstream; mixing double-counts).
	AuthorityAggregatorAvg PriceAuthority = "aggregator_avg"

	// AuthorityTriangulated is a price derived via a bridge currency, the last-resort tier.
	AuthorityTriangulated PriceAuthority = "triangulated"

	// AuthorityReferenceRate is a published FX reference rate (the daily ECB series in fx_quotes), not a traded VWAP.
	AuthorityReferenceRate PriceAuthority = "reference_rate"

	// AuthorityIdentity is a price true by definition (USD in USD).
	AuthorityIdentity PriceAuthority = "identity"

	// AuthorityOnChainListing is the per-asset listing price served as a Stellar-only token's headline, with no trade-count floor or observation time.
	AuthorityOnChainListing PriceAuthority = "onchain_listing"
)

// GlobalPriceResult is the output of [ComputeGlobalPrice]: the served price, its tier and the contributing sources.
type GlobalPriceResult struct {
	Price     string         // decimal string, exactly as Postgres serialises NUMERIC
	Authority PriceAuthority // which tier won
	Sources   []string       // contributors — venue names (VWAP), aggregator names (avg), bridge-currency-anchor source (triangulated, optional)
	AsOf      time.Time      // observation timestamp the price corresponds to
	// TradeCount is the VWAP tier's trade count; zero for the aggregator and triangulated tiers.
	TradeCount int64
}

// GlobalPriceReader is the storage seam [ComputeGlobalPrice] reads, one method per fallback tier.
type GlobalPriceReader interface {
	// LatestVWAP returns the latest closed VWAP bucket; vwap is the RAW prices_1m ratio, so whoever serves it must apply [AdjustPrice].
	LatestVWAP(ctx context.Context, base, quote canonical.Asset) (vwap string, asOf time.Time, tradeCount int64, sources []string, ok bool, err error)

	// LatestAggregatorPrices returns the latest observation per source across the given aggregator-class sources.
	LatestAggregatorPrices(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error)

	// LookupTriangulated returns the bridge-implied price for (base, quote); ok is false when no path was published.
	LookupTriangulated(ctx context.Context, base, quote canonical.Asset, window time.Duration) (price string, asOf time.Time, ok bool, err error)
}

// GlobalPriceOptions tunes the fallback-chain policy.
type GlobalPriceOptions struct {
	// VWAPMinTradeCount is the trade-count floor below which the VWAP tier falls through to the aggregator tier; zero accepts any non-empty bucket.
	VWAPMinTradeCount int64

	// AggregatorSources is the source list for the aggregator tier; empty disables it.
	AggregatorSources []string

	// TriangulationWindow is the window the published implied VWAP was computed over, used to pick the cache key.
	TriangulationWindow time.Duration

	// MaxAggregatorAge caps aggregator observation age (default 10 minutes); zero disables the freshness check.
	MaxAggregatorAge time.Duration

	// MaxVWAPAge is the VWAP tier's freshness ceiling: an older VWAP yields to a fresh lower tier and is served only if none exists; zero disables it.
	MaxVWAPAge time.Duration
}

// DefaultGlobalPriceOptions returns the defaults: 5-trade floor, 5-minute triangulation window, 10-minute freshness ceiling on the VWAP and aggregator tiers.
func DefaultGlobalPriceOptions() GlobalPriceOptions {
	return GlobalPriceOptions{
		VWAPMinTradeCount:   5,
		TriangulationWindow: 5 * time.Minute,
		MaxAggregatorAge:    10 * time.Minute,
		MaxVWAPAge:          10 * time.Minute,
	}
}

// ErrNoPrice indicates none of the three tiers produced a price.
var ErrNoPrice = errors.New("aggregate: no global price available")

// ComputeGlobalPrice walks vwap_native, aggregator_avg, triangulated, then a stale VWAP, returning [ErrNoPrice] if all are empty; reader errors other than not-found propagate.
func ComputeGlobalPrice(
	ctx context.Context,
	base, quote canonical.Asset,
	reader GlobalPriceReader,
	opts GlobalPriceOptions,
) (GlobalPriceResult, error) {
	if reader == nil {
		return GlobalPriceResult{}, fmt.Errorf("aggregate: nil GlobalPriceReader")
	}

	// Tier 1 — direct VWAP from prices_1m.
	res, stale, err := tryVWAPTier(ctx, base, quote, reader, opts)
	if err != nil {
		return GlobalPriceResult{}, err
	}
	if res != nil {
		return *res, nil
	}

	// Tier 2 — average across aggregator-class sources.
	if res, hit, err := tryAggregatorTier(ctx, base, quote, reader, opts); err != nil {
		return GlobalPriceResult{}, err
	} else if hit {
		return res, nil
	}

	// Tier 3 — triangulated via bridge currency.
	if res, hit, err := tryTriangulatedTier(ctx, base, quote, reader, opts); err != nil {
		return GlobalPriceResult{}, err
	} else if hit {
		return res, nil
	}

	if stale != nil {
		return *stale, nil
	}
	return GlobalPriceResult{}, ErrNoPrice
}

// tryVWAPTier returns the first alias's VWAP that clears the trade floor
// and MaxVWAPAge, else nil plus the first that cleared only the floor.
func tryVWAPTier(
	ctx context.Context,
	base, quote canonical.Asset,
	reader GlobalPriceReader,
	opts GlobalPriceOptions,
) (fresh, stale *GlobalPriceResult, err error) {
	// Loop the base's canonical aliases (XLM: native, crypto:XLM, SAC); the pair set may be configured under any form.
	// The FIRST alias whose VWAP clears the trade-count floor wins.
	for _, b := range assetAliases(base) {
		vwap, asOf, tradeCount, sources, ok, err := reader.LatestVWAP(ctx, b, quote)
		if err != nil {
			return nil, nil, fmt.Errorf("aggregate: VWAP lookup: %w", err)
		}
		if !ok || tradeCount < opts.VWAPMinTradeCount {
			continue
		}
		res := &GlobalPriceResult{
			Price:      vwap,
			Authority:  AuthorityVWAPNative,
			Sources:    sources,
			AsOf:       asOf,
			TradeCount: tradeCount,
		}
		if opts.MaxVWAPAge <= 0 || asOf.After(time.Now().Add(-opts.MaxVWAPAge)) {
			return res, nil, nil
		}
		if stale == nil {
			stale = res
		}
	}
	return nil, stale, nil
}

// assetAliases delegates to [canonical.AssetAliases] so the aggregator and api/v1 cannot drift on XLM's identities.
// The SAC form is ordered last: tryVWAPTier takes the FIRST alias clearing VWAPMinTradeCount.
func assetAliases(asset canonical.Asset) []canonical.Asset {
	return canonical.AssetAliases(asset)
}

func tryAggregatorTier(
	ctx context.Context,
	base, quote canonical.Asset,
	reader GlobalPriceReader,
	opts GlobalPriceOptions,
) (GlobalPriceResult, bool, error) {
	if len(opts.AggregatorSources) == 0 {
		return GlobalPriceResult{}, false, nil
	}
	// Same alias loop as tryVWAPTier; the FIRST fresh average clearing the tier wins and the SAC form is last,
	// so a thin Soroban pool can never shadow the deep classic/CEX feeds.
	for _, b := range assetAliases(base) {
		rows, err := reader.LatestAggregatorPrices(ctx, b, quote, opts.AggregatorSources)
		if err != nil {
			return GlobalPriceResult{}, false, fmt.Errorf("aggregate: aggregator-prices lookup: %w", err)
		}
		if len(rows) == 0 {
			continue
		}
		fresh := filterFreshAggregatorRows(rows, opts.MaxAggregatorAge)
		if len(fresh) == 0 {
			continue
		}
		// M8: drop any source grossly divergent from the consensus BEFORE
		// averaging, so a single bad print can't drag the plain-mean
		// headline. Both the served price and the reported Sources reflect
		// only the survivors.
		kept := rejectAggregatorOutliers(fresh)
		avgPrice, latest, ok := averageAggregatorPrices(kept)
		if !ok {
			continue
		}
		sources := make([]string, 0, len(kept))
		for _, u := range kept {
			sources = append(sources, u.Source)
		}
		return GlobalPriceResult{
			Price:     avgPrice,
			Authority: AuthorityAggregatorAvg,
			Sources:   sources,
			AsOf:      latest,
		}, true, nil
	}
	return GlobalPriceResult{}, false, nil
}

func tryTriangulatedTier(
	ctx context.Context,
	base, quote canonical.Asset,
	reader GlobalPriceReader,
	opts GlobalPriceOptions,
) (GlobalPriceResult, bool, error) {
	// Same alias loop as the other tiers: the worker publishes each implied VWAP under one canonical form.
	// The FIRST alias with a published path wins; the SAC form is tried last.
	for _, b := range assetAliases(base) {
		price, asOf, ok, err := reader.LookupTriangulated(ctx, b, quote, opts.TriangulationWindow)
		if err != nil {
			return GlobalPriceResult{}, false, fmt.Errorf("aggregate: triangulation lookup: %w", err)
		}
		if !ok {
			continue
		}
		return GlobalPriceResult{
			Price:     price,
			Authority: AuthorityTriangulated,
			AsOf:      asOf,
		}, true, nil
	}
	return GlobalPriceResult{}, false, nil
}

// filterFreshAggregatorRows drops observations older than
// `maxAge`. Zero `maxAge` is "no filter" — every observation
// passes through. The result is a fresh slice; the input's backing
// array is left untouched.
func filterFreshAggregatorRows(rows []canonical.OracleUpdate, maxAge time.Duration) []canonical.OracleUpdate {
	if maxAge <= 0 {
		return rows
	}
	cutoff := time.Now().Add(-maxAge)
	out := make([]canonical.OracleUpdate, 0, len(rows))
	for _, u := range rows {
		if u.Timestamp.After(cutoff) {
			out = append(out, u)
		}
	}
	return out
}

// aggregatorCommonDecimals is the shared fixed scale every aggregator
// price is projected onto before it is compared or averaged — matches
// the /v1/oracle/lastprice surface and exceeds every wired
// aggregator's native precision (CoinGecko=8, CMC=8).
const aggregatorCommonDecimals = 14

// aggregatorMinForOutlierReject is the fewest sources at which an outlier is identifiable; with 1-2 sources there is no majority, so they are averaged unchanged.
const aggregatorMinForOutlierReject = 3

// aggregatorMADFactor is K=5 sigma-equivalents (ADR-0019), the half-width of the aggregator tier's acceptance band
// around the median; a business-value tolerance knob.
var aggregatorMADFactor = big.NewRat(5, 1)

// rejectAggregatorOutliers drops sources beyond aggregatorMADFactor*(1.4826*MAD) of the median, in ratio space ([symmetricDev])
// so a downward or zero print is rejected too; exact *big.Rat (ADR-0003). Fewer than [aggregatorMinForOutlierReject]
// usable sources returns the input unchanged (never fails closed).
func rejectAggregatorOutliers(rows []canonical.OracleUpdate) []canonical.OracleUpdate {
	if len(rows) < aggregatorMinForOutlierReject {
		return rows
	}
	vals := make([]*big.Rat, 0, len(rows))
	idx := make([]int, 0, len(rows))
	for i := range rows {
		v, ok := scaledAggregatorRat(&rows[i])
		if !ok {
			continue // non-positive price — no usable value to judge
		}
		vals = append(vals, v)
		idx = append(idx, i)
	}
	if len(vals) < aggregatorMinForOutlierReject {
		return rows // too few usable prices to judge a consensus
	}

	centre, scale := robustCentreScale(vals)
	half := new(big.Rat).Mul(aggregatorMADFactor, scale)

	kept := make([]canonical.OracleUpdate, 0, len(rows))
	for k, v := range vals {
		dev := symmetricDev(v, centre)
		if dev == nil || dev.Cmp(half) > 0 {
			continue // divergent print — drop
		}
		kept = append(kept, rows[idx[k]])
	}
	if len(kept) == 0 {
		// Unreachable: the median centre keeps >= half the sources.
		// Never fail closed — serve the unfiltered set rather than no
		// price when sources merely disagree.
		return rows
	}
	return kept
}

// scaledAggregatorRat projects u.Price (at u.Decimals) onto
// [aggregatorCommonDecimals] and returns it as an exact *big.Rat for
// cross-source comparison. ok=false for a non-positive price.
func scaledAggregatorRat(u *canonical.OracleUpdate) (*big.Rat, bool) {
	bi := u.Price.BigInt()
	if bi == nil || bi.Sign() <= 0 {
		return nil, false
	}
	scaled := new(big.Int).Set(bi)
	diff := int(aggregatorCommonDecimals) - int(u.Decimals)
	if diff > 0 {
		scaled.Mul(scaled, pow10(diff))
	} else if diff < 0 {
		scaled.Quo(scaled, pow10(-diff))
	}
	return new(big.Rat).SetInt(scaled), true
}

// pow10 returns 10^n as a *big.Int (n >= 0).
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// averageAggregatorPrices returns the arithmetic mean of positive prices scaled to 14 dp, rounded half-up once, plus the latest
// observation time. ok=false when no row has a positive price or the mean rounds to zero ("0.00000000000000" is not a price).
func averageAggregatorPrices(rows []canonical.OracleUpdate) (string, time.Time, bool) {
	if len(rows) == 0 {
		return "", time.Time{}, false
	}
	const commonDecimals = 14
	target := new(big.Int).Exp(big.NewInt(10), big.NewInt(commonDecimals), nil)

	// Exact rational sum of the real prices: a row published above 14 dp
	// is not truncated before it is averaged, so the one rounding below
	// is the only one.
	sum := new(big.Rat)
	var latest time.Time
	contributed := 0
	for i := range rows {
		u := &rows[i]
		if u.Price.BigInt() == nil || u.Price.BigInt().Sign() <= 0 {
			continue
		}
		sum.Add(sum, new(big.Rat).SetFrac(u.Price.BigInt(), pow10(int(u.Decimals))))
		contributed++
		if u.Timestamp.After(latest) {
			latest = u.Timestamp
		}
	}
	if contributed == 0 {
		return "", time.Time{}, false
	}

	// avg_scaled = round_half_up(sum · 10^14 / contributed)
	//            = ⌊(2·num + den) / (2·den)⌋ (all operands positive).
	sum.Mul(sum, new(big.Rat).SetFrac(target, big.NewInt(int64(contributed))))
	twoNum := new(big.Int).Lsh(sum.Num(), 1)
	twoNum.Add(twoNum, sum.Denom())
	avgScaled := twoNum.Quo(twoNum, new(big.Int).Lsh(sum.Denom(), 1))
	if avgScaled.Sign() <= 0 {
		return "", time.Time{}, false
	}
	return formatScaledDecimal(avgScaled, target, commonDecimals), latest, true
}

// formatScaledDecimal renders avg (scaled by `pow10`) as a fixed-
// fractional decimal string with `decimals` fractional digits.
// e.g. avg = 100_010_000_000_000, pow10 = 10^14, decimals = 14
// → "1.00010000000000".
func formatScaledDecimal(avg, pow10 *big.Int, decimals int) string {
	// Integer and fractional parts via big.Int division.
	intPart := new(big.Int).Quo(avg, pow10)
	frac := new(big.Int).Rem(avg, pow10)
	if frac.Sign() < 0 {
		frac.Neg(frac)
	}

	fracStr := frac.String()
	// Pad with leading zeros to `decimals` digits.
	for len(fracStr) < decimals {
		fracStr = "0" + fracStr
	}
	return intPart.String() + "." + fracStr
}
