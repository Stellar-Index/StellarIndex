package oracleagg

import (
	"errors"
	"fmt"
	"math/big"
)

// Verdict names why an adapter's lastprice cannot be served. Each None or panic the contract can return has its own,
// and so does each way our copy of the chain can fall short of it.
type Verdict string

// The verdict vocabulary.
const (
	// What the contract itself would answer instead of a price.
	VerdictAssetMissing          Verdict = "oracle_asset_missing"          // lastprice panics AssetNotFound
	VerdictAssetBlocked          Verdict = "oracle_asset_blocked"          // v1: lastprice panics AssetBlocked
	VerdictContractPanics        Verdict = "oracle_contract_panics"        // the call traps (arithmetic underflow)
	VerdictFeedAssetUnknown      Verdict = "oracle_feed_asset_unknown"     // the feed does not quote the feed asset: None
	VerdictPriceStale            Verdict = "oracle_price_stale"            // no feed round inside the program's window
	VerdictPriceAbsent           Verdict = "oracle_price_absent"           // v1: the feed's last round has no price for the asset
	VerdictDeviation             Verdict = "oracle_deviation"              // v2: price moved more than max_dev since the earlier period
	VerdictDeviationUnverifiable Verdict = "oracle_deviation_unverifiable" // v2: no earlier period to check max_dev against

	// Where we cannot reproduce the answer.
	VerdictUnsupported    Verdict = "oracle_unsupported"     // program, feed build or asset shape not modelled
	VerdictReadFailed     Verdict = "oracle_read_failed"     // our read of config, feed state or rows failed
	VerdictFeedIncomplete Verdict = "oracle_feed_incomplete" // our stored rounds disagree with the feed's own state
	VerdictStateNewer     Verdict = "oracle_state_newer"     // a config or feed entry changed after the evaluation ledger
	VerdictStateArchived  Verdict = "oracle_state_archived"  // a config or feed entry is TTL-archived (or undecodable) in the lake
	VerdictFeedConflict   Verdict = "oracle_feed_conflict"   // our stored rows for one round disagree (two prices, sources or quotes)
)

// ErrEntryNewer marks an entry last modified after the evaluation ledger; its value is not the state at that ledger.
var ErrEntryNewer = errors.New("oracleagg: entry modified after the evaluation ledger")

// ErrEntryArchived marks an entry the lake holds archived or undecodable: chain state we cannot read, not our failure.
var ErrEntryArchived = errors.New("oracleagg: entry archived")

// ErrorVerdict names a config or feed-state failure: an entry newer than the evaluation ledger, an archived entry,
// a layout we do not decode (ErrConfig), or else our own read failing.
func ErrorVerdict(err error) Verdict {
	switch {
	case errors.Is(err, ErrEntryNewer):
		return VerdictStateNewer
	case errors.Is(err, ErrEntryArchived):
		return VerdictStateArchived
	case errors.Is(err, ErrConfig):
		return VerdictUnsupported
	}
	return VerdictReadFailed
}

// Feed is the underlying Reflector feed as of the evaluation ledger, for one feed asset: the feed's own on-chain state
// and our stored rounds, which every lookup the recipe makes is cross-checked against.
type Feed struct {
	Program FeedProgram
	State   FeedState
	// Prices is our stored raw price per round timestamp (ms) for the asset.
	Prices map[uint64]*big.Int
	// StoredLastRoundMs is our newest stored round of the feed (any asset) between State.LastTimestampMs and the
	// ledger time; 0 = none.
	StoredLastRoundMs uint64
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

func verdictPtr(v Verdict, format string, args ...any) *Result {
	r := verdict(v, format, args...)
	return &r
}

// LastPrice reproduces cfg's lastprice(asset) and decimals() at ledger close time t (seconds) from feed.
func LastPrice(cfg Config, asset Asset, t uint64, feed Feed) Result {
	if err, bad := cfg.Unreadable[asset]; bad {
		return verdict(ErrorVerdict(err), "%s config: %v", asset, err)
	}
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
	if cfg.IsBase(asset) {
		return Result{Price: Price{Price: pow10(cfg.Decimals), Decimals: cfg.Decimals, Timestamp: t, Quote: cfg.Base}}
	}
	rule, ok := cfg.Assets[asset]
	if !ok {
		return verdict(VerdictAssetMissing, "%s is not configured on the adapter", asset)
	}
	q, r := newFeedQuery(feed, rule, t)
	if r != nil {
		return *r
	}
	res := uint64(rule.ResolutionSeconds)
	if cfg.MaxAge > t {
		return verdict(VerdictContractPanics, "ledger time %d − max_age %d underflows", t, cfg.MaxAge)
	}
	// price_data.rs:12-21: walk back from the feed's last_timestamp() by the adapter's cached resolution while the
	// period is not older than max_age; the first period carrying the asset wins.
	oldest := t - cfg.MaxAge
	next := feed.State.LastTimestampMs / 1000
	if next < oldest {
		return verdict(VerdictPriceStale, "%s last round %d is before %d (ledger time %d − max_age %d)",
			rule.Feed, next, oldest, t, cfg.MaxAge)
	}
	if r := checkLastRound(feed, rule); r != nil {
		return *r
	}
	var p *big.Int
	var ts uint64
	found := false
	for !found && next >= oldest {
		if p, ts, found, r = q.priceAt(next); r != nil {
			return *r
		}
		if next < res {
			return verdict(VerdictContractPanics, "period walk underflows at %d", next)
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
		if r := q.deviationCheck(rule, cfg.MaxAge/res, next, p); r != nil {
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

func (q feedQuery) deviationCheck(rule AssetRule, steps, next uint64, p *big.Int) *Result {
	res := uint64(rule.ResolutionSeconds)
	var old *big.Int
	found := false
	for range steps {
		var r *Result
		if old, _, found, r = q.priceAt(next); r != nil {
			return r
		}
		if found {
			break
		}
		if next < res {
			return verdictPtr(VerdictContractPanics, "deviation walk underflows at %d", next)
		}
		next -= res
	}
	if !found {
		return verdictPtr(VerdictDeviationUnverifiable, "no earlier %s period within %d lookups", rule.FeedAsset, steps)
	}
	diff := new(big.Int).Abs(new(big.Int).Sub(p, old))
	// Quo truncates toward zero, as Rust's i128 division does.
	limit := new(big.Int).Quo(new(big.Int).Mul(old, big.NewInt(int64(rule.MaxDev))), big.NewInt(100))
	if diff.Cmp(limit) > 0 {
		return verdictPtr(VerdictDeviation, "|%s − %s| = %s exceeds %d%% of the earlier price (%s)", p, old, diff, rule.MaxDev, limit)
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
	q, r := newFeedQuery(feed, rule, t)
	if r != nil {
		return *r
	}
	last, now := feed.State.LastTimestampMs, t*1000
	switch {
	case last == 0:
		return verdict(VerdictPriceStale, "%s has no round", rule.Feed)
	case last > now:
		return verdict(VerdictPriceStale, "%s last round %d ms is after ledger time %d ms", rule.Feed, last, now)
	case feed.Program == FeedReflectorV6 && now-last >= 2*feed.State.ResolutionMs:
		return verdict(VerdictPriceStale, "%s last round %d ms is two resolutions before ledger time %d ms", rule.Feed, last, now)
	}
	if r := checkLastRound(feed, rule); r != nil {
		return *r
	}
	p, found, r := q.roundPrice(last)
	if r != nil {
		return *r
	}
	if !found {
		return verdict(VerdictPriceAbsent, "%s round %d ms carries no %s price", rule.Feed, last, rule.FeedAsset)
	}
	return Result{Price: Price{Price: normalize(p, cfg.Decimals, rule.FeedDecimals), Decimals: cfg.Decimals, Timestamp: last / 1000, Quote: cfg.Base}}
}

// feedQuery is one feed asset resolved against the feed's state, after the checks every lookup depends on.
type feedQuery struct {
	feed  Feed
	asset Asset
	index uint32
	nowMs uint64 // the ledger time
}

func newFeedQuery(feed Feed, rule AssetRule, t uint64) (feedQuery, *Result) {
	st := feed.State
	switch {
	case feed.Program == FeedUnknown:
		return feedQuery{}, verdictPtr(VerdictUnsupported, "feed %s runs an unmodelled program", rule.Feed)
	case st.Protocol != ReflectorProtocol:
		return feedQuery{}, verdictPtr(VerdictUnsupported, "feed %s storage protocol %d", rule.Feed, st.Protocol)
	case st.ResolutionMs == 0 || rule.ResolutionSeconds == 0:
		return feedQuery{}, verdictPtr(VerdictUnsupported, "feed %s has no resolution", rule.Feed)
	case feed.Program == FeedReflectorV7 && rule.FeedAsset == st.Base:
		// price_oracle.rs:212-214: v7.1 quotes its base as 1 at the last round; not modelled.
		return feedQuery{}, verdictPtr(VerdictUnsupported, "%s is feed %s's own base asset", rule.FeedAsset, rule.Feed)
	}
	idx, ok := st.AssetIndex(rule.FeedAsset)
	if !ok {
		return feedQuery{}, verdictPtr(VerdictFeedAssetUnknown, "feed %s does not quote %s", rule.Feed, rule.FeedAsset)
	}
	return feedQuery{feed: feed, asset: rule.FeedAsset, index: idx, nowMs: t * 1000}, nil
}

// checkLastRound requires our newest stored round to be the feed's last_timestamp, unless the chain says that round
// priced nothing (an all-zero round writes no event, so no row).
func checkLastRound(feed Feed, rule AssetRule) *Result {
	stored, last, res := feed.StoredLastRoundMs, feed.State.LastTimestampMs, feed.State.ResolutionMs
	switch {
	case stored != 0 && stored%res != 0:
		return verdictPtr(VerdictFeedIncomplete, "stored round %d ms of %s is off the %d ms boundary", stored, rule.Feed, res)
	case stored > last:
		return verdictPtr(VerdictFeedIncomplete, "stored round %d ms of %s is after its last_timestamp %d ms", stored, rule.Feed, last)
	case stored < last && feed.State.RoundPriced(0):
		return verdictPtr(VerdictFeedIncomplete, "%s round %d ms is on chain but not stored (newest stored %d ms)", rule.Feed, last, stored)
	}
	return nil
}

// priceAt is Reflector price(asset, timestamp) (price_oracle.rs:211-220, prices.rs:32-58): the timestamp is
// normalised down to the feed's resolution; no price after the last round or more than 255 periods before it.
func (q feedQuery) priceAt(seconds uint64) (*big.Int, uint64, bool, *Result) {
	res := q.feed.State.ResolutionMs
	ts := seconds * 1000 / res * res
	p, found, r := q.roundPrice(ts)
	return p, ts / 1000, found, r
}

// roundPrice is retrieve_asset_price_data at round ts (ms): the chain's history mask decides whether the asset was
// priced, and our stored row must agree with it (and with the chain's cached price where the round is cached).
func (q feedQuery) roundPrice(ts uint64) (*big.Int, bool, *Result) {
	st := q.feed.State
	last := st.LastTimestampMs
	if ts == 0 || last < ts || (last-ts)/st.ResolutionMs > 255 {
		return nil, false, nil
	}
	onChain := st.HasPrice(q.index, uint32((last-ts)/st.ResolutionMs))
	row, stored := q.feed.Prices[ts]
	if onChain != stored {
		return nil, false, verdictPtr(VerdictFeedIncomplete, "%s at round %d ms: priced on chain %v, stored %v", q.asset, ts, onChain, stored)
	}
	if !onChain {
		return nil, false, nil
	}
	cp, cached := st.CachedPrice(q.index, ts)
	if cached && cp.Cmp(row) != 0 {
		return nil, false, verdictPtr(VerdictFeedIncomplete, "%s at round %d ms: stored %s, chain cache %s", q.asset, ts, row, cp)
	}
	// load_history_record (prices.rs:136-147) falls back from the cache to the round's temporary entry, which
	// lives only as long as store_prices extended it.
	if !cached && q.nowMs > ts && q.nowMs-ts >= st.EntryLifeMs() {
		return nil, false, verdictPtr(VerdictUnsupported, "%s round %d ms is not cached and %d ms old, past its entry life %d ms",
			q.asset, ts, q.nowMs-ts, st.EntryLifeMs())
	}
	return row, true, nil
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
