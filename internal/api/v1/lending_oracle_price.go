package v1

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingOracleLake is the lake half of the pool-oracle read.
type lendingOracleLake interface {
	ContractWasmHash(ctx context.Context, contractID string) (string, error)
	ContractInstanceStorage(ctx context.Context, contract string) ([]scval.ScMapEntry, bool, error)
	ContractPersistentEntry(ctx context.Context, contract string, key scval.ScVal) (scval.ScVal, bool, error)
	CloseTimeForLedger(ctx context.Context, seq uint32) (time.Time, bool, error)
}

// lendingOracleFeeds is the served-tier half: the feed's rounds and its projector cursor.
type lendingOracleFeeds interface {
	OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error)
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

var (
	_ lendingOracleLake  = (*clickhouse.ExplorerReader)(nil)
	_ lendingOracleFeeds = (*timescale.Store)(nil)
	_ lendingOracleFeeds = (*CachedLendingReader)(nil)
)

// OracleFeedRounds is a pass-through.
func (c *CachedLendingReader) OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error) {
	if up, ok := c.upstream.(lendingOracleFeeds); ok {
		return up.OracleFeedRounds(ctx, contract, assetKeys, from, to, maxLedger)
	}
	return timescale.OracleFeedWindow{}, errLendingOracleUnwired
}

// GetCursor is a pass-through.
func (c *CachedLendingReader) GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error) {
	if up, ok := c.upstream.(lendingOracleFeeds); ok {
		return up.GetCursor(ctx, source, sub)
	}
	return timescale.Cursor{}, errLendingOracleUnwired
}

var errLendingOracleUnwired = errors.New("pool-oracle reader not wired")

// lendingOracleV1Window bounds a v1 adapter's feed read: its lastprice has no age limit on Reflector v7.1, but the
// pool refuses a price older than 24 h (pool.rs::load_price), so an older round cannot price the pool either.
const lendingOracleV1Window = 24 * time.Hour

// lendingOraclePool is the pool whose reserves are priced: its PoolConfig oracle and its reserve assets.
type lendingOraclePool struct {
	ID, Oracle string
	Reserves   []string
}

// lendingOraclePrice is one reserve's reproduced lastprice, or the verdict that replaces it (never a substitute).
type lendingOraclePrice struct {
	Price   blend.OraclePrice
	Quote   canonical.Asset // the adapter's base, verbatim: fiat USD or the USDC SAC, never normalised
	Verdict oracleagg.Verdict
	Detail  string
}

// lendingOracleBasis is what the prices were reproduced against. Ledger is the lake watermark, or the feed's
// projector cursor when that is behind it; CloseTime is that ledger's close time.
type lendingOracleBasis struct {
	Oracle    string
	WasmHash  string
	Program   oracleagg.Program
	Ledger    uint32
	CloseTime time.Time
}

// poolOraclePrices reproduces the pool oracle's lastprice for every reserve. A reserve is priced only when the oracle
// runs an audited adapter build and every rule of its lastprice was reproduced; otherwise it carries a verdict.
func (s *Server) poolOraclePrices(ctx context.Context, pool lendingOraclePool) (map[string]lendingOraclePrice, lendingOracleBasis) {
	basis := lendingOracleBasis{Oracle: pool.Oracle}
	out := make(map[string]lendingOraclePrice, len(pool.Reserves))
	fail := func(v oracleagg.Verdict, detail string) (map[string]lendingOraclePrice, lendingOracleBasis) {
		for _, r := range pool.Reserves {
			out[r] = lendingOraclePrice{Verdict: v, Detail: detail}
		}
		countOracleVerdicts(pool.ID, out)
		return out, basis
	}
	lake, lok := s.Explorer.(lendingOracleLake)
	feeds, fok := s.Lending.(lendingOracleFeeds)
	if !lok || !fok {
		return fail(oracleagg.VerdictReadFailed, errLendingOracleUnwired.Error())
	}
	ledger, _, ok := s.lakeWatermark(ctx)
	if !ok {
		return fail(oracleagg.VerdictReadFailed, "no lake watermark")
	}
	adapter, err := s.lendingOracleAdapter(ctx, lake, pool.Oracle, ledger)
	basis.WasmHash, basis.Program = adapter.hash, adapter.cfg.Program
	if err != nil {
		var v verdictErr
		if errors.As(err, &v) {
			return fail(v.verdict, v.Error())
		}
		return fail(oracleagg.VerdictReadFailed, err.Error())
	}
	if basis.Ledger, basis.CloseTime, err = evaluationPoint(ctx, lake, feeds, adapter, ledger); err != nil {
		return fail(oracleagg.VerdictReadFailed, err.Error())
	}
	t := uint64(basis.CloseTime.Unix())
	for _, r := range pool.Reserves {
		out[r] = priceReserve(ctx, feeds, adapter, oracleagg.Stellar(r), t, basis.Ledger)
	}
	countOracleVerdicts(pool.ID, out)
	return out, basis
}

func countOracleVerdicts(pool string, prices map[string]lendingOraclePrice) {
	for _, p := range prices {
		if p.Verdict != "" {
			obs.LendingOracleVerdictsTotal.WithLabelValues(pool, string(p.Verdict)).Inc()
		}
	}
}

// verdictErr is a config-stage failure that maps to a named verdict rather than oracle_read_failed.
type verdictErr struct {
	verdict oracleagg.Verdict
	detail  string
}

func (e verdictErr) Error() string { return e.detail }

// lendingOracleAdapter is an adapter's identity, decoded config and its feeds' builds, as of one lake watermark.
type lendingOracleAdapter struct {
	ledger uint32
	hash   string
	cfg    oracleagg.Config
	feeds  map[string]oracleagg.FeedProgram
}

type lendingOracleCacheKey struct {
	srv    *Server
	oracle string
}

// lendingOracleCache holds one entry per (server, oracle), valid for the lake watermark it was read at; the server
// struct takes no new fields, so the cache is keyed by it instead. Read failures are not cached.
var lendingOracleCache = struct {
	sync.Mutex
	m map[lendingOracleCacheKey]lendingOracleAdapter
}{m: map[lendingOracleCacheKey]lendingOracleAdapter{}}

// lendingOracleAdapter gates the oracle on its CURRENT WASM hash and decodes its config, both cached per watermark.
func (s *Server) lendingOracleAdapter(ctx context.Context, lake lendingOracleLake, oracle string, ledger uint32) (lendingOracleAdapter, error) {
	key := lendingOracleCacheKey{srv: s, oracle: oracle}
	lendingOracleCache.Lock()
	hit, ok := lendingOracleCache.m[key]
	lendingOracleCache.Unlock()
	if ok && hit.ledger == ledger {
		return hit, nil
	}
	a := lendingOracleAdapter{ledger: ledger}
	var err error
	if a.hash, err = lake.ContractWasmHash(ctx, oracle); err != nil {
		if errors.Is(err, clickhouse.ErrContractIsSAC) {
			return a, verdictErr{oracleagg.VerdictUnsupported, "oracle " + oracle + " is a Stellar Asset Contract"}
		}
		return a, fmt.Errorf("oracle %s wasm hash: %w", oracle, err)
	}
	program := oracleagg.ProgramForHash(a.hash)
	if program == oracleagg.ProgramUnknown {
		return a, verdictErr{oracleagg.VerdictUnsupported, "oracle " + oracle + " runs unaudited wasm " + a.hash}
	}
	if a.cfg, err = readAdapterConfig(ctx, lake, oracle, program); err != nil {
		return a, err
	}
	if a.feeds, err = feedPrograms(ctx, lake, a.cfg); err != nil {
		return a, err
	}
	lendingOracleCache.Lock()
	lendingOracleCache.m[key] = a
	lendingOracleCache.Unlock()
	return a, nil
}

func readAdapterConfig(ctx context.Context, lake lendingOracleLake, oracle string, program oracleagg.Program) (oracleagg.Config, error) {
	storage, ok, err := lake.ContractInstanceStorage(ctx, oracle)
	if err != nil {
		return oracleagg.Config{}, fmt.Errorf("oracle %s instance: %w", oracle, err)
	}
	if !ok {
		return oracleagg.Config{}, fmt.Errorf("oracle %s instance entry not in the lake", oracle)
	}
	if program == oracleagg.ProgramV2 {
		return oracleagg.DecodeV2Instance(storage)
	}
	return oracleagg.DecodeV1(storage, func(k scval.ScVal) (scval.ScVal, bool, error) {
		return lake.ContractPersistentEntry(ctx, oracle, k)
	})
}

// feedPrograms reads the current build of every feed the adapter calls; an unmodelled build prices nothing.
func feedPrograms(ctx context.Context, lake lendingOracleLake, cfg oracleagg.Config) (map[string]oracleagg.FeedProgram, error) {
	out := map[string]oracleagg.FeedProgram{}
	for _, a := range cfg.SortedAssets() {
		feed := cfg.Assets[a].Feed
		if _, done := out[feed]; done {
			continue
		}
		hash, err := lake.ContractWasmHash(ctx, feed)
		if err != nil && !errors.Is(err, clickhouse.ErrContractIsSAC) {
			return nil, fmt.Errorf("feed %s wasm hash: %w", feed, err)
		}
		out[feed] = oracleagg.FeedProgramForHash(hash)
	}
	return out, nil
}

// evaluationPoint is the ledger the prices are reproduced at: the lake watermark, held back to the slowest feed's
// projector cursor, since a round the projector has not written would make the recipe read an older one.
func evaluationPoint(ctx context.Context, lake lendingOracleLake, feeds lendingOracleFeeds, a lendingOracleAdapter, ledger uint32) (uint32, time.Time, error) {
	closeAt, err := closeTime(ctx, lake, ledger)
	if err != nil {
		return 0, time.Time{}, err
	}
	for feed := range a.feeds {
		w, err := feeds.OracleFeedRounds(ctx, feed, nil, closeAt.Add(-lendingOracleV1Window), closeAt, ledger)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("feed %s: %w", feed, err)
		}
		if w.Source == "" {
			continue // no round in the window: the recipe reports stale
		}
		c, err := feeds.GetCursor(ctx, "projector", w.Source)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("feed %s projector cursor: %w", feed, err)
		}
		ledger = min(ledger, c.LastLedger)
	}
	if closeAt, err = closeTime(ctx, lake, ledger); err != nil {
		return 0, time.Time{}, err
	}
	return ledger, closeAt, nil
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

// priceReserve runs the recipe for one reserve over the feed rows it needs.
func priceReserve(ctx context.Context, feeds lendingOracleFeeds, a lendingOracleAdapter, asset oracleagg.Asset, t uint64, ledger uint32) lendingOraclePrice {
	var feed oracleagg.Feed
	if rule, ok := a.cfg.Assets[asset]; ok && !rule.Blocked {
		var v *lendingOraclePrice
		if feed, v = readFeed(ctx, feeds, a, rule, t, ledger); v != nil {
			return *v
		}
	}
	res := oracleagg.LastPrice(a.cfg, asset, t, feed)
	if res.Verdict != "" {
		return lendingOraclePrice{Verdict: res.Verdict, Detail: res.Detail}
	}
	quote, err := oracleagg.FeedAssetKey(res.Price.Quote)
	if err != nil {
		return lendingOraclePrice{Verdict: oracleagg.VerdictUnsupported, Detail: "adapter base " + res.Price.Quote.String() + ": " + err.Error()}
	}
	return lendingOraclePrice{
		Price: blend.OraclePrice{Price: res.Price.Price, Decimals: res.Price.Decimals, Timestamp: res.Price.Timestamp},
		Quote: quote,
	}
}

// readFeed loads the feed's rounds for one rule's asset as the recipe's Feed, keyed on the feed CONTRACT.
func readFeed(ctx context.Context, feeds lendingOracleFeeds, a lendingOracleAdapter, rule oracleagg.AssetRule, t uint64, ledger uint32) (oracleagg.Feed, *lendingOraclePrice) {
	key, err := oracleagg.FeedAssetKey(rule.FeedAsset)
	if err != nil {
		return oracleagg.Feed{}, &lendingOraclePrice{Verdict: oracleagg.VerdictUnsupported, Detail: "feed asset " + rule.FeedAsset.String() + ": " + err.Error()}
	}
	to := time.Unix(int64(t), 0).UTC()
	from := to.Add(-lendingOracleV1Window)
	if a.cfg.Program == oracleagg.ProgramV2 {
		// The walk reaches t − max_age; the deviation search up to max_age/resolution periods below that.
		res := time.Duration(rule.ResolutionSeconds) * time.Second
		from = to.Add(-time.Duration(a.cfg.MaxAge)*time.Second - time.Duration(a.cfg.MaxAge)*time.Second - res)
	}
	// Every stored identity of the asset (canonical.AssetAliases); the window refuses two prices in one round.
	w, err := feeds.OracleFeedRounds(ctx, rule.Feed, canonical.AssetAliasStrings(key), from, to, ledger)
	if err != nil {
		return oracleagg.Feed{}, &lendingOraclePrice{Verdict: oracleagg.VerdictReadFailed, Detail: fmt.Sprintf("feed %s: %v", rule.Feed, err)}
	}
	return oracleagg.FeedFromRounds(a.feeds[rule.Feed], reflector.DefaultResolutionSeconds, w.LastRound, w.Quote, key, w.Prices), nil
}
