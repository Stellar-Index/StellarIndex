package oracleagg

import (
	"fmt"
	"math/big"
)

// Verdict names why an adapter's lastprice cannot be served. Each None or panic the contract can return has its own.
type Verdict string

// The verdict vocabulary.
const (
	VerdictUnsupported           Verdict = "oracle_unsupported"            // program, feed or asset shape not reproduced
	VerdictReadFailed            Verdict = "oracle_read_failed"            // config or feed rows could not be read
	VerdictAssetMissing          Verdict = "oracle_asset_missing"          // lastprice panics AssetNotFound
	VerdictAssetBlocked          Verdict = "oracle_asset_blocked"          // v1: lastprice panics AssetBlocked
	VerdictPriceStale            Verdict = "oracle_price_stale"            // no feed round inside the program's window
	VerdictPriceAbsent           Verdict = "oracle_price_absent"           // v1: the feed's last round has no price for the asset
	VerdictDeviation             Verdict = "oracle_deviation"              // v2: price moved more than max_dev since the earlier period
	VerdictDeviationUnverifiable Verdict = "oracle_deviation_unverifiable" // v2: no earlier period to check max_dev against
)

// Feed is the underlying Reflector feed as of the evaluation point, for one feed asset.
type Feed struct {
	Program FeedProgram
	// ResolutionMs is the feed's own live resolution (Reflector settings), which its price() normalises by.
	ResolutionMs uint64
	// LastRoundMs is the feed's last_timestamp: its newest round applied at or before the evaluation point; 0 = none.
	LastRoundMs uint64
	// Prices is the asset's raw price per round timestamp (ms); a round without the asset is absent.
	Prices map[uint64]*big.Int
	// AssetIsBase reports that the feed asset is the feed's own base, which Reflector v7.1 quotes as 1 (not modelled).
	AssetIsBase bool
}

// Price is a reproduced lastprice: the raw integer at Decimals, the PriceData timestamp (seconds) and the quote.
type Price struct {
	Price     *big.Int
	Decimals  uint32
	Timestamp uint64
	Quote     Asset
}

// Result is a Price or the Verdict that replaces it; Detail says which rule fired.
type Result struct {
	Price   Price
	Verdict Verdict
	Detail  string
}

func verdict(v Verdict, format string, args ...any) Result {
	return Result{Verdict: v, Detail: fmt.Sprintf(format, args...)}
}

// LastPrice reproduces cfg's lastprice(asset) and decimals() at ledger close time t (seconds) from feed.
func LastPrice(cfg Config, asset Asset, t uint64, feed Feed) Result {
	switch cfg.Program {
	case ProgramV2:
		return lastPriceV2(cfg, asset, t, feed)
	case ProgramV1:
		return lastPriceV1(cfg, asset, t, feed)
	case ProgramUnknown:
	}
	return verdict(VerdictUnsupported, "adapter program %s", cfg.Program)
}

// lastPriceV2 is v2.0.0 contract.rs::lastprice and price_data.rs::get_price.
func lastPriceV2(cfg Config, asset Asset, t uint64, feed Feed) Result {
	// contract.rs:87-93: Base and BaseAssets price at exactly one Base, stamped with the ledger time.
	if asset == cfg.Base || containsAsset(cfg.BaseAssets, asset) {
		return Result{Price: Price{Price: pow10(cfg.Decimals), Decimals: cfg.Decimals, Timestamp: t, Quote: cfg.Base}}
	}
	rule, ok := cfg.Assets[asset]
	if !ok {
		return verdict(VerdictAssetMissing, "%s is not configured on the adapter", asset)
	}
	if r := feedSupported(feed, rule); r != nil {
		return *r
	}
	res := uint64(rule.ResolutionSeconds)
	if cfg.MaxAge > t {
		return verdict(VerdictReadFailed, "max_age %d exceeds ledger time %d", cfg.MaxAge, t)
	}
	// price_data.rs:12-21: walk back from the feed's last round by the cached resolution while the period is
	// not older than max_age; the first period carrying the asset wins.
	oldest := t - cfg.MaxAge
	next := feed.LastRoundMs / 1000
	var p *big.Int
	var ts uint64
	found := false
	for !found && next >= oldest {
		p, ts, found = feed.priceAt(next)
		if next < res {
			return verdict(VerdictReadFailed, "period walk underflows at %d", next)
		}
		next -= res
	}
	if !found {
		return verdict(VerdictPriceStale, "no %s round for %s at or after %d (ledger time %d − max_age %d)",
			rule.Feed, rule.FeedAsset, oldest, t, cfg.MaxAge)
	}
	// price_data.rs:26-50: 0 < max_dev < 100 compares with the first earlier period found within max_age/resolution
	// further lookups (not bounded by oldest), on the feed's raw integers.
	if rule.MaxDev > 0 && rule.MaxDev < 100 {
		if r := deviationCheck(feed, rule, cfg.MaxAge/res, next, p); r != nil {
			return *r
		}
	}
	// price_data.rs:53-56: rescale to the adapter's decimals; the timestamp is the period's.
	out := Price{Price: normalize(p, cfg.Decimals, rule.FeedDecimals), Decimals: cfg.Decimals, Timestamp: ts, Quote: cfg.Base}
	if out.Timestamp < oldest {
		return verdict(VerdictPriceStale, "period %d older than %d", out.Timestamp, oldest)
	}
	return Result{Price: out}
}

func deviationCheck(feed Feed, rule AssetRule, steps, next uint64, p *big.Int) *Result {
	res := uint64(rule.ResolutionSeconds)
	var old *big.Int
	found := false
	for range steps {
		if old, _, found = feed.priceAt(next); found {
			break
		}
		if next < res {
			r := verdict(VerdictReadFailed, "deviation walk underflows at %d", next)
			return &r
		}
		next -= res
	}
	if !found {
		r := verdict(VerdictDeviationUnverifiable, "no earlier %s period within %d lookups", rule.FeedAsset, steps)
		return &r
	}
	diff := new(big.Int).Abs(new(big.Int).Sub(p, old))
	// Quo truncates toward zero, as Rust's i128 division does.
	limit := new(big.Int).Quo(new(big.Int).Mul(old, big.NewInt(int64(rule.MaxDev))), big.NewInt(100))
	if diff.Cmp(limit) > 0 {
		r := verdict(VerdictDeviation, "|%s − %s| = %s exceeds %d%% of the earlier price (%s)", p, old, diff, rule.MaxDev, limit)
		return &r
	}
	return nil
}

// lastPriceV1 is v1.0.0 contract.rs::lastprice over Reflector lastprice (prices.rs::obtain_last_record_timestamp).
func lastPriceV1(cfg Config, asset Asset, t uint64, feed Feed) Result {
	rule, ok := cfg.Assets[asset]
	if !ok {
		return verdict(VerdictAssetMissing, "%s is not configured on the adapter", asset)
	}
	if rule.Blocked {
		return verdict(VerdictAssetBlocked, "%s is blocked on the adapter", asset)
	}
	if r := feedSupported(feed, rule); r != nil {
		return *r
	}
	last, now := feed.LastRoundMs, t*1000
	switch {
	case last == 0:
		return verdict(VerdictPriceStale, "%s has no round", rule.Feed)
	case last > now:
		return verdict(VerdictPriceStale, "%s last round %d ms is after ledger time %d ms", rule.Feed, last, now)
	case feed.Program == FeedReflectorV6 && now-last >= 2*feed.ResolutionMs:
		return verdict(VerdictPriceStale, "%s last round %d ms is two resolutions before ledger time %d ms", rule.Feed, last, now)
	}
	p, ok := feed.Prices[last]
	if !ok {
		return verdict(VerdictPriceAbsent, "%s round %d ms carries no %s price", rule.Feed, last, rule.FeedAsset)
	}
	return Result{Price: Price{Price: normalize(p, cfg.Decimals, rule.FeedDecimals), Decimals: cfg.Decimals, Timestamp: last / 1000, Quote: cfg.Base}}
}

func feedSupported(feed Feed, rule AssetRule) *Result {
	var r Result
	switch {
	case feed.Program == FeedUnknown:
		r = verdict(VerdictUnsupported, "feed %s runs an unmodelled program", rule.Feed)
	case feed.ResolutionMs == 0 || rule.ResolutionSeconds == 0:
		r = verdict(VerdictUnsupported, "feed %s has no resolution", rule.Feed)
	case feed.AssetIsBase && feed.Program == FeedReflectorV7:
		r = verdict(VerdictUnsupported, "%s is feed %s's own base asset", rule.FeedAsset, rule.Feed)
	default:
		return nil
	}
	return &r
}

// priceAt is Reflector price(asset, timestamp) (price_oracle.rs::price, prices.rs::retrieve_asset_price_data): the
// timestamp is normalised down to the resolution, and a period after the last round or more than 255 periods
// before it has no price. It returns the price and the period in seconds.
func (f Feed) priceAt(seconds uint64) (*big.Int, uint64, bool) {
	ts := seconds * 1000 / f.ResolutionMs * f.ResolutionMs
	if ts == 0 || f.LastRoundMs < ts || (f.LastRoundMs-ts)/f.ResolutionMs > 255 {
		return nil, 0, false
	}
	p, ok := f.Prices[ts]
	return p, ts / 1000, ok
}

// normalize is price_data.rs::normalize_price: truncating division when the feed has more decimals.
func normalize(p *big.Int, decimals, feedDecimals uint32) *big.Int {
	switch {
	case feedDecimals > decimals:
		return new(big.Int).Quo(p, pow10(feedDecimals-decimals))
	case feedDecimals < decimals:
		return new(big.Int).Mul(p, pow10(decimals-feedDecimals))
	}
	return new(big.Int).Set(p)
}

func pow10(n uint32) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

func containsAsset(list []Asset, a Asset) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}
