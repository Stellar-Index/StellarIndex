package v1

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingOracleLake is the lake half of the pool-oracle read: exact-key current entries with their last-modified ledger.
type lendingOracleLake interface {
	ContractInstance(ctx context.Context, contract string) (clickhouse.ContractInstance, bool, error)
	ContractPersistentEntry(ctx context.Context, contract string, key scval.ScVal) (scval.ScVal, uint32, bool, error)
	CloseTimeForLedger(ctx context.Context, seq uint32) (time.Time, bool, error)
}

// lendingOracleFeeds is the served-tier half: the feed's stored rounds.
type lendingOracleFeeds interface {
	OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, lastFrom, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error)
}

var (
	_ lendingOracleLake  = (*clickhouse.ExplorerReader)(nil)
	_ lendingOracleFeeds = (*timescale.Store)(nil)
	_ lendingOracleFeeds = (*CachedLendingReader)(nil)
)

// OracleFeedRounds is a pass-through.
func (c *CachedLendingReader) OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, lastFrom, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error) {
	if up, ok := c.upstream.(lendingOracleFeeds); ok {
		return up.OracleFeedRounds(ctx, contract, assetKeys, lastFrom, from, to, maxLedger)
	}
	return timescale.OracleFeedWindow{}, errLendingOracleUnwired
}

var errLendingOracleUnwired = errors.New("pool-oracle reader not wired")

// LendingOraclePool is the pool whose reserves are priced: its PoolConfig oracle and its reserve assets.
type LendingOraclePool struct {
	ID, Oracle string
	Reserves   []string
}

// LendingOraclePrice is one reserve's reproduced lastprice, or the verdict that replaces it (never a substitute). The
// pool's own 24 h age bound is not applied here: blend's health math applies pool.rs::load_price to Price.Timestamp.
type LendingOraclePrice struct {
	Price   blend.OraclePrice
	Quote   canonical.Asset // the adapter's base, verbatim: fiat USD or the USDC SAC, never normalised
	Verdict oracleagg.Verdict
	Detail  string
}

// LendingOracleBasis is what the prices were reproduced against: the lake watermark ledger and its close time. Every
// entry read is the state at Ledger (its last modification is at or before it) or the reserve carries a verdict.
type LendingOracleBasis struct {
	Oracle    string
	WasmHash  string
	Program   oracleagg.Program
	Ledger    uint32
	CloseTime time.Time
}

// PoolOraclePrices reproduces the pool oracle's lastprice for every reserve at the lake watermark. A reserve is priced
// only when the oracle runs an audited adapter build, every entry read is the state at that ledger, our stored rounds
// agree with the feed's own state, and every rule of lastprice was reproduced; otherwise it carries a verdict.
func (s *Server) PoolOraclePrices(ctx context.Context, pool LendingOraclePool) (map[string]LendingOraclePrice, LendingOracleBasis) {
	basis := LendingOracleBasis{Oracle: pool.Oracle}
	out := make(map[string]LendingOraclePrice, len(pool.Reserves))
	fail := func(err error) (map[string]LendingOraclePrice, LendingOracleBasis) {
		v, detail := verdictOf(err)
		for _, r := range pool.Reserves {
			out[r] = LendingOraclePrice{Verdict: v, Detail: detail}
		}
		countOracleVerdicts(pool.ID, out)
		return out, basis
	}
	lake, lok := s.Explorer.(lendingOracleLake)
	feeds, fok := s.Lending.(lendingOracleFeeds)
	if !lok || !fok {
		return fail(errLendingOracleUnwired)
	}
	ledger, _, ok := s.lakeWatermark(ctx)
	if !ok {
		return fail(errors.New("no lake watermark"))
	}
	basis.Ledger = ledger
	closeAt, err := closeTime(ctx, lake, ledger)
	if err != nil {
		return fail(err)
	}
	basis.CloseTime = closeAt
	adapter, err := s.lendingOracleAdapter(ctx, lake, pool.Oracle, ledger)
	basis.WasmHash, basis.Program = adapter.hash, adapter.cfg.Program
	if err != nil {
		return fail(err)
	}
	t := uint64(closeAt.Unix())
	for _, r := range pool.Reserves {
		out[r] = priceReserve(ctx, feeds, adapter, oracleagg.Stellar(r), t, ledger)
	}
	countOracleVerdicts(pool.ID, out)
	return out, basis
}

func countOracleVerdicts(pool string, prices map[string]LendingOraclePrice) {
	for _, p := range prices {
		if p.Verdict != "" {
			obs.LendingOracleVerdictsTotal.WithLabelValues(pool, string(p.Verdict)).Inc()
		}
	}
}

// verdictErr is a failure that maps to a named verdict rather than oracle_read_failed.
type verdictErr struct {
	verdict oracleagg.Verdict
	detail  string
}

func (e verdictErr) Error() string { return e.detail }

func verdictOf(err error) (oracleagg.Verdict, string) {
	if v := (verdictErr{}); errors.As(err, &v) {
		return v.verdict, v.detail
	}
	return oracleagg.ErrorVerdict(err), err.Error()
}

// lendingOracleAdapter is an adapter's identity, decoded config and its feeds' state, as of one lake watermark.
type lendingOracleAdapter struct {
	ledger uint32
	hash   string
	cfg    oracleagg.Config
	feeds  map[string]lendingOracleFeed
}

// lendingOracleFeed is one feed's build and decoded instance state at the watermark, or why it could not be read.
type lendingOracleFeed struct {
	program oracleagg.FeedProgram
	state   oracleagg.FeedState
	err     error
}

type lendingOracleCacheKey struct {
	srv    *Server
	oracle string
}

// lendingOracleCache holds one entry per (server, oracle), valid for the lake watermark it was read at; the server
// struct takes no new fields, so the cache is keyed by it instead. Failures are not cached; concurrent misses for one
// (server, oracle, watermark) share one read.
var (
	lendingOracleCache = struct {
		sync.Mutex
		m map[lendingOracleCacheKey]lendingOracleAdapter
	}{m: map[lendingOracleCacheKey]lendingOracleAdapter{}}
	lendingOracleFlight singleflight.Group
)

// lendingOracleAdapter gates the oracle on the WASM hash of its instance entry and decodes that entry's config and its
// feeds' instance state, all as of ledger.
func (s *Server) lendingOracleAdapter(ctx context.Context, lake lendingOracleLake, oracle string, ledger uint32) (lendingOracleAdapter, error) {
	key := lendingOracleCacheKey{srv: s, oracle: oracle}
	lendingOracleCache.Lock()
	hit, ok := lendingOracleCache.m[key]
	lendingOracleCache.Unlock()
	if ok && hit.ledger == ledger {
		return hit, nil
	}
	v, err, _ := lendingOracleFlight.Do(fmt.Sprintf("%p|%s|%d", s, oracle, ledger), func() (any, error) {
		a, err := readAdapter(ctx, lake, oracle, ledger)
		if err == nil && a.cacheable() {
			lendingOracleCache.Lock()
			lendingOracleCache.m[key] = a
			lendingOracleCache.Unlock()
		}
		return a, err
	})
	a, _ := v.(lendingOracleAdapter)
	return a, err
}

func readAdapter(ctx context.Context, lake lendingOracleLake, oracle string, ledger uint32) (lendingOracleAdapter, error) {
	a := lendingOracleAdapter{ledger: ledger}
	inst, err := readInstance(ctx, lake, "oracle", oracle, ledger)
	a.hash = inst.WasmHash
	if err != nil {
		return a, err
	}
	if a.hash == "" {
		return a, verdictErr{oracleagg.VerdictUnsupported, "oracle " + oracle + " is a Stellar Asset Contract"}
	}
	program := oracleagg.ProgramForHash(a.hash)
	if program == oracleagg.ProgramUnknown {
		return a, verdictErr{oracleagg.VerdictUnsupported, "oracle " + oracle + " runs unaudited wasm " + a.hash}
	}
	if program == oracleagg.ProgramV2 {
		a.cfg, err = oracleagg.DecodeV2Instance(inst.Storage)
	} else {
		a.cfg, err = oracleagg.DecodeV1(inst.Storage, func(k scval.ScVal) (scval.ScVal, bool, error) {
			v, modified, ok, err := lake.ContractPersistentEntry(ctx, oracle, k)
			if err == nil && modified > ledger {
				err = fmt.Errorf("%w: %s %s at L%d, evaluating L%d", oracleagg.ErrEntryNewer, oracle, scval.Display(k), modified, ledger)
			}
			return v, ok, err
		})
	}
	if err != nil {
		return a, err
	}
	a.feeds = map[string]lendingOracleFeed{}
	for _, asset := range a.cfg.SortedAssets() {
		feed := a.cfg.Assets[asset].Feed
		if _, done := a.feeds[feed]; !done {
			a.feeds[feed] = readFeedState(ctx, lake, feed, ledger)
		}
	}
	return a, nil
}

// cacheable is false when a feed read failed on our side: that is retried, not held for the watermark.
func (a lendingOracleAdapter) cacheable() bool {
	for _, f := range a.feeds {
		if f.err != nil && oracleagg.ErrorVerdict(f.err) == oracleagg.VerdictReadFailed {
			return false
		}
	}
	return true
}

// readInstance reads a contract's instance entry and refuses it when it was modified after ledger.
func readInstance(ctx context.Context, lake lendingOracleLake, role, contract string, ledger uint32) (clickhouse.ContractInstance, error) {
	inst, ok, err := lake.ContractInstance(ctx, contract)
	if err != nil {
		return inst, fmt.Errorf("%s %s instance: %w", role, contract, err)
	}
	if !ok {
		return inst, fmt.Errorf("%s %s instance entry not in the lake", role, contract)
	}
	if inst.Ledger > ledger {
		return inst, fmt.Errorf("%w: %s %s instance at L%d, evaluating L%d", oracleagg.ErrEntryNewer, role, contract, inst.Ledger, ledger)
	}
	return inst, nil
}

// readFeedState reads a feed's build and, for a modelled build, its instance state; an unmodelled build prices nothing.
func readFeedState(ctx context.Context, lake lendingOracleLake, feed string, ledger uint32) lendingOracleFeed {
	inst, err := readInstance(ctx, lake, "feed", feed, ledger)
	if err != nil {
		return lendingOracleFeed{err: err}
	}
	f := lendingOracleFeed{program: oracleagg.FeedProgramForHash(inst.WasmHash)}
	if f.program == oracleagg.FeedUnknown {
		return f
	}
	if f.state, err = oracleagg.DecodeFeedState(inst.Storage); err != nil {
		f.err = fmt.Errorf("feed %s state: %w", feed, err)
	}
	return f
}

func closeTime(ctx context.Context, lake lendingOracleLake, ledger uint32) (time.Time, error) {
	at, ok, err := lake.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		return time.Time{}, fmt.Errorf("close time of L%d: %w", ledger, err)
	}
	if !ok {
		return time.Time{}, fmt.Errorf("close time of L%d not in the lake", ledger)
	}
	return at.UTC(), nil
}

// priceReserve runs the recipe for one reserve over the feed state and stored rounds it needs.
func priceReserve(ctx context.Context, feeds lendingOracleFeeds, a lendingOracleAdapter, asset oracleagg.Asset, t uint64, ledger uint32) LendingOraclePrice {
	var feed oracleagg.Feed
	if rule, ok := a.cfg.Assets[asset]; ok && !rule.Blocked && !a.cfg.IsBase(asset) {
		var v *LendingOraclePrice
		if feed, v = readFeed(ctx, feeds, a, rule, t, ledger); v != nil {
			return *v
		}
	}
	res := oracleagg.LastPrice(a.cfg, asset, t, feed)
	if res.Verdict != "" {
		return LendingOraclePrice{Verdict: res.Verdict, Detail: res.Detail}
	}
	quote, err := oracleagg.FeedAssetKey(res.Price.Quote)
	if err != nil {
		return LendingOraclePrice{Verdict: oracleagg.VerdictUnsupported, Detail: "adapter base " + res.Price.Quote.String() + ": " + err.Error()}
	}
	return LendingOraclePrice{
		Price: blend.OraclePrice{Price: res.Price.Price, Decimals: res.Price.Decimals, Timestamp: res.Price.Timestamp},
		Quote: quote,
	}
}

// readFeed loads one rule's feed as the recipe's Feed: the feed's state at ledger, and our stored rounds of it, keyed on
// the feed CONTRACT, over exactly the rounds the program's lastprice can touch.
func readFeed(ctx context.Context, feeds lendingOracleFeeds, a lendingOracleAdapter, rule oracleagg.AssetRule, t uint64, ledger uint32) (oracleagg.Feed, *LendingOraclePrice) {
	fs := a.feeds[rule.Feed]
	if fs.err != nil {
		v, detail := verdictOf(fs.err)
		return oracleagg.Feed{}, &LendingOraclePrice{Verdict: v, Detail: detail}
	}
	if fs.program == oracleagg.FeedUnknown {
		return oracleagg.Feed{Program: fs.program}, nil
	}
	key, err := oracleagg.FeedAssetKey(rule.FeedAsset)
	if err != nil {
		return oracleagg.Feed{}, &LendingOraclePrice{Verdict: oracleagg.VerdictUnsupported, Detail: "feed asset " + rule.FeedAsset.String() + ": " + err.Error()}
	}
	to := time.Unix(int64(t), 0).UTC()
	last := time.UnixMilli(int64(fs.state.LastTimestampMs)).UTC()
	from := last // v1 reads the last round only
	if a.cfg.Program == oracleagg.ProgramV2 {
		from = to.Add(-v2Reach(a.cfg.MaxAge, uint64(rule.ResolutionSeconds), fs.state.ResolutionMs))
	}
	lastFrom := last
	if lastFrom.Before(from) {
		lastFrom = from // the recipe answers stale before it compares an older last round
	}
	// Every stored identity of the asset (canonical.AssetAliases); the window refuses two prices in one round.
	w, err := feeds.OracleFeedRounds(ctx, rule.Feed, canonical.AssetAliasStrings(key), lastFrom, from, to, ledger)
	if err != nil {
		return oracleagg.Feed{}, &LendingOraclePrice{Verdict: oracleagg.VerdictReadFailed, Detail: fmt.Sprintf("feed %s: %v", rule.Feed, err)}
	}
	return oracleagg.FeedFromRounds(fs.program, fs.state, w.LastRound, w.Prices), nil
}

// v2Reach is how far below the ledger time v2's lastprice can look: the walk to t − max_age, then max_age/resolution
// deviation lookups a resolution apart, each normalised down by up to one feed resolution.
func v2Reach(maxAge, resSeconds, feedResMs uint64) time.Duration {
	reach := maxAge + resSeconds
	if resSeconds > 0 {
		reach += maxAge / resSeconds * resSeconds
	}
	return time.Duration(reach)*time.Second + time.Duration(feedResMs)*time.Millisecond
}
