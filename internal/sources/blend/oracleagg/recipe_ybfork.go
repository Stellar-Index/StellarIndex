package oracleagg

import (
	"math/big"
	"time"
)

// ybForkRecords is the record count the fork asks the feed for (fork price_data.rs:16, prices(&asset, &4)).
const ybForkRecords = 4

// reflectorHistoryPeriods bounds Reflector's prices() walk (prices.rs:219, back < 256).
const reflectorHistoryPeriods = 256

// YBForkReach is how far below the feed's last round the fork's max_dev path can read: prices(asset, 4) may walk
// the feed's whole 256-period history to find its records.
func YBForkReach(feedResMs uint64) time.Duration {
	return time.Duration((reflectorHistoryPeriods-1)*feedResMs) * time.Millisecond
}

// lastPriceYBFork is the YieldBlox fork's contract.rs::lastprice (v2.0.0's, unchanged) and its price_data.rs::get_price
// (yieldblox/oracle-aggregator tag v2.0.0_oracle-aggregator_cli22.0.1, commit 2307a9b2).
func lastPriceYBFork(cfg Config, asset Asset, t uint64, feed Feed) Result {
	// contract.rs:85-93: Base and BaseAssets price at exactly one Base, stamped with the ledger time.
	if cfg.IsBase(asset) {
		return Result{Price: Price{Price: pow10(cfg.Decimals), Decimals: cfg.Decimals, Timestamp: t, Quote: cfg.Base}}
	}
	rule, ok := cfg.Assets[asset]
	if !ok {
		return verdict(VerdictAssetMissing, "%s is not configured on the adapter", asset)
	}
	// price_data.rs:12: ledger time − max_age, before any feed call.
	if cfg.MaxAge > t {
		return verdict(VerdictContractPanics, "ledger time %d − max_age %d underflows", t, cfg.MaxAge)
	}
	oldest := t - cfg.MaxAge
	q, r := newFeedQuery(feed, rule, t)
	if r != nil {
		return *r
	}
	var p *big.Int
	var ts uint64
	if rule.MaxDev > 0 && rule.MaxDev < 100 {
		p, ts, r = q.newestTwoCheck(rule)
	} else {
		p, ts, r = q.lastRoundPrice(rule, oldest)
	}
	if r != nil {
		return *r
	}
	// price_data.rs:35-41: rescale to the adapter's decimals, then the record's own timestamp must be within max_age.
	out := Price{Price: normalize(p, cfg.Decimals, rule.FeedDecimals), Decimals: cfg.Decimals, Timestamp: ts, Quote: cfg.Base}
	if out.Timestamp < oldest {
		return verdict(VerdictPriceStale, "%s record %d is before %d (ledger time %d − max_age %d)", rule.FeedAsset, ts, oldest, t, cfg.MaxAge)
	}
	return Result{Price: out}
}

// lastRoundPrice is price_data.rs:28-33 (max_dev 0 or >= 100): feed last_timestamp(), which Reflector returns with no
// future check (price_oracle.rs:73-75); if it is not before t − max_age, price(asset, last_timestamp), else None.
func (q feedQuery) lastRoundPrice(rule AssetRule, oldest uint64) (*big.Int, uint64, *Result) {
	last := q.feed.State.LastTimestampMs / 1000
	if last < oldest {
		return nil, 0, verdictPtr(VerdictPriceStale, "%s last round %d is before %d", rule.Feed, last, oldest)
	}
	if r := checkLastRound(q.feed, rule); r != nil {
		return nil, 0, r
	}
	p, ts, found, r := q.priceAt(last)
	if r != nil {
		return nil, 0, r
	}
	if !found {
		return nil, 0, verdictPtr(VerdictPriceAbsent, "%s round %d carries no %s price", rule.Feed, last, rule.FeedAsset)
	}
	return p, ts, nil
}

// newestTwoCheck is price_data.rs:15-27 (0 < max_dev < 100): the feed's prices(asset, 4), newest first; the newest
// record is taken only if |p0 − p1| < p1·max_dev/100, strictly, on the feed's raw integers. Nothing older than p1 is
// consulted, and p1 is whatever record the feed walk found next, however many periods back.
func (q feedQuery) newestTwoCheck(rule AssetRule) (*big.Int, uint64, *Result) {
	if r := checkLastRound(q.feed, rule); r != nil {
		return nil, 0, r
	}
	recs, r := q.newestRecords(2)
	if r != nil {
		return nil, 0, r
	}
	switch len(recs) {
	case 0:
		return nil, 0, verdictPtr(VerdictPriceAbsent, "%s prices(%s, %d) is None", rule.Feed, rule.FeedAsset, ybForkRecords)
	case 1:
		return nil, 0, verdictPtr(VerdictDeviationUnverifiable, "%s prices(%s, %d) holds one record", rule.Feed, rule.FeedAsset, ybForkRecords)
	}
	p0, p1 := recs[0], recs[1]
	diff := new(big.Int).Abs(new(big.Int).Sub(p0.price, p1.price))
	// Quo truncates toward zero, as Rust's i128 division does (price_data.rs:22).
	limit := new(big.Int).Quo(new(big.Int).Mul(p1.price, big.NewInt(int64(rule.MaxDev))), big.NewInt(100))
	if diff.Cmp(limit) >= 0 { // price_data.rs:23: `if diff < max_dev`, so a move of exactly the limit is refused
		return nil, 0, verdictPtr(VerdictDeviation, "|%s − %s| = %s is not below %d%% of the previous record (%s)",
			p0.price, p1.price, diff, rule.MaxDev, limit)
	}
	return p0.price, p0.ts, nil
}

type feedRecord struct {
	price *big.Int
	ts    uint64 // seconds
}

// newestRecords is the first n records of Reflector v7.1.0 prices(asset, 4) (price_oracle.rs:258-264,
// prices.rs:196-238): from obtain_last_record_timestamp (None when there is no round or it is after the ledger time),
// walk back a resolution at a time, keeping each priced round, until a record at or below last − 4·resolution is
// kept or 256 periods are walked. Later records cannot change the first n, so the walk stops once it has them.
func (q feedQuery) newestRecords(n int) ([]feedRecord, *Result) {
	st := q.feed.State
	last, res := st.LastTimestampMs, st.ResolutionMs
	if last == 0 || last > q.nowMs { // prices.rs:19-29, 198-200
		return nil, verdictPtr(VerdictPriceStale, "feed last round %d ms is absent or after ledger time %d ms", last, q.nowMs)
	}
	if last < res*ybForkRecords {
		return nil, verdictPtr(VerdictContractPanics, "last round %d ms − %d resolutions underflows", last, ybForkRecords)
	}
	lower := last - res*ybForkRecords // prices.rs:211
	var out []feedRecord
	ts, included := last, last
	for back := 0; included > lower && back < reflectorHistoryPeriods; back++ {
		p, found, r := q.roundPrice(ts)
		if r != nil {
			return nil, r
		}
		if found {
			out = append(out, feedRecord{price: p, ts: ts / 1000})
			included = ts
			if len(out) == n {
				break
			}
		}
		ts -= res
		if ts < res {
			break
		}
	}
	return out, nil
}
