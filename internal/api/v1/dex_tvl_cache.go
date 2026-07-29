package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ProtocolTVLView is the additive per-protocol TVL summary attached to
// GET /v1/protocols rows and /v1/protocols/{name} when the DEX TVL
// snapshot cache is wired and has data for the protocol. Phoenix and
// Comet events carry flow deltas, not post-state reserves, so their
// figures come from the pools' STORAGE entries in the lake (absolute
// current state — a window net-flow is NOT TVL and is deliberately
// not dressed up as one). Absent for protocols without any absolute
// reserve source (SDEX is an order book) and on cold start before
// the first background refresh completes.
type ProtocolTVLView struct {
	// TVLUSD is the summed USD value of every PRICED reserve leg across
	// the protocol's pools, as a decimal string (ADR-0003). Unpriced
	// legs contribute 0, so this is a LOWER BOUND whenever
	// UnpricedPools > 0.
	TVLUSD string `json:"tvl_usd"`
	// PoolsTotal is the number of pools with a current reserve
	// observation contributing to this snapshot — including pools
	// whose captured storage did NOT decode to a recognised shape
	// (they contribute 0 and count in UnpricedPools, never a guess).
	PoolsTotal int `json:"pools_total"`
	// PoolsPriced is the number of pools whose every reserve leg
	// resolved to a USD price.
	PoolsPriced int `json:"pools_priced"`
	// UnpricedPools is the number of pools with at least one reserve
	// leg that could not be priced in USD — or whose current storage
	// shape was unrecognised; their priceable legs (if any) still
	// contribute to TVLUSD (honest lower bound, never silent).
	UnpricedPools int `json:"unpriced_pools"`
	// AsOf is when the snapshot was computed (RFC3339).
	AsOf string `json:"as_of"`
	// Basis is a one-line provenance statement of what was measured
	// and how it was valued.
	Basis string `json:"basis"`
}

// DEXTVLRefreshInterval is the cadence the background goroutine in
// cmd/stellarindex-api/main.go calls DEXTVLCache.Refresh at. The
// underlying reads are three batched lake lookups (soroswap pair
// instance entries; phoenix pool persistent entries; comet record
// entries) + one served-tier query (aquarius reserve snapshots) + a
// handful of prices_1m lookups — cheap enough for 10 minutes,
// slow-moving enough that anything faster adds no signal.
const DEXTVLRefreshInterval = 10 * time.Minute

// aquariusTVLWindowDays bounds the aquarius_reserves recency scan —
// same 90d window the protocol bespoke analytics use, so the two
// surfaces describe the same pool set.
const aquariusTVLWindowDays = 90

// SoroswapTVLReserveReader reads current Soroswap pair reserves from
// the certified lake. Production wiring is *clickhouse.ExplorerReader.
// Pairs absent from the result have no current reserve state (archived
// or uncaptured) — absence is honest "unavailable", NEVER zero TVL.
type SoroswapTVLReserveReader interface {
	SoroswapPairReserves(ctx context.Context, pairs []string) (map[string]clickhouse.SoroswapPairState, error)
}

// AquariusTVLReserveReader reads the latest post-state reserve
// snapshot per Aquarius pool. Production wiring is timescale.Store.
type AquariusTVLReserveReader interface {
	LatestAquariusReserves(ctx context.Context, windowDays int) ([]timescale.AquariusPoolReserve, error)
}

// PhoenixTVLReserveReader reads current Phoenix pool reserves +
// token identities from the pools' persistent storage in the
// certified lake. Production wiring is *clickhouse.ExplorerReader.
// The second return lists pools whose captured storage did not
// decode to the recognised shape — counted, never guessed; pools
// absent from BOTH returns have no current state (archived or
// uncaptured), which is honest "unavailable", NEVER zero TVL.
type PhoenixTVLReserveReader interface {
	PhoenixPoolReserves(ctx context.Context, pools []string) (map[string]clickhouse.PhoenixPoolState, []string, error)
}

// CometTVLReserveReader reads current Comet per-token pool balance
// records from the lake. Production wiring is
// *clickhouse.ExplorerReader; same absence + undecodable contract as
// PhoenixTVLReserveReader.
type CometTVLReserveReader interface {
	CometPoolReserves(ctx context.Context, pools []string) (map[string]clickhouse.CometPoolState, []string, error)
}

// TVLUSDPricer resolves an on-chain asset's USD price at a point in
// time. Production wiring is *timescale.VWAPUSDFXResolver — the same
// tier system (peg → direct VWAP → XLM bridge) that stamps
// trades.usd_volume, so TVL and volume on the same page share one
// pricing methodology. The returned rate is a RAW prices_1m ratio
// whose scale is anchored at the 7-decimal classic scale; see
// tvlLegUSD for the exact identity.
type TVLUSDPricer interface {
	USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error)
}

// TVLUSDPegInfo reports whether an asset is an operator-declared
// USD-pegged classic (or its SAC wrapper) and at what decimal scale.
// Production wiring is *timescale.USDVolumeQuoteSpec. Nil-safe: no
// spec means no peg shortcut — pegged tokens then price through the
// resolver like any other asset (or count unpriced).
type TVLUSDPegInfo interface {
	QuoteUSDPegInfo(asset canonical.Asset) (decimals int, ok bool)
}

// DEXTVLSources are the read seams the TVL snapshot is computed from.
// Every field is optional; a protocol whose readers are missing is
// simply absent from the snapshot (same degradation contract as the
// other protocol joins).
type DEXTVLSources struct {
	// SoroswapPairs is the soroswap_pairs registry (the pair id set the
	// lake reserve lookup is scoped to).
	SoroswapPairs SoroswapPairsReader
	// SoroswapReserves is the lake current-reserves reader.
	SoroswapReserves SoroswapTVLReserveReader
	// AquariusReserves is the served-tier latest-reserve-snapshot reader.
	AquariusReserves AquariusTVLReserveReader
	// PhoenixPools is the ADR-0035 curated Phoenix pool set the lake
	// reserve lookup is scoped to (phoenix.MainnetPools +
	// MainnetMapPools; stake contracts are deliberately excluded —
	// they hold LP shares, which would double-count the underlying).
	PhoenixPools []string
	// PhoenixReserves is the lake current-pool-state reader.
	PhoenixReserves PhoenixTVLReserveReader
	// CometPools is the curated Comet pool allowlist
	// (comet.MainnetGatedSet — today exactly Blend's BLND/USDC
	// backstop; a new pool must be operator-admitted first).
	CometPools []string
	// CometReserves is the lake current-record-state reader.
	CometReserves CometTVLReserveReader
	// Pricer resolves reserve legs to USD. Required for any TVL to be
	// priced; nil means every leg is unpriced (and the snapshot says so).
	Pricer TVLUSDPricer
	// PegInfo short-circuits operator-declared USD pegs to $1 at their
	// real decimals. Optional.
	PegInfo TVLUSDPegInfo
	// Logger for refresh warnings. Optional.
	Logger Logger
}

// DEXTVLCache is a read-mostly snapshot of per-protocol TVL, refreshed
// by a background goroutine (CoverageCache pattern — the explorer read
// budget cannot afford per-request aggregation over reserve tables).
// Cold start (no refresh yet) serves an empty snapshot: handlers omit
// the tvl field, never 503.
type DEXTVLCache struct {
	mu        sync.RWMutex
	snapshot  map[string]ProtocolTVLView
	fetchedAt time.Time
	src       DEXTVLSources
}

// NewDEXTVLCache constructs an empty cache. The production wiring in
// cmd/stellarindex-api/main.go calls Refresh once at startup and then
// on the DEXTVLRefreshInterval schedule.
func NewDEXTVLCache(src DEXTVLSources) *DEXTVLCache {
	return &DEXTVLCache{src: src}
}

// Snapshot returns the most recent successful per-protocol TVL map +
// its timestamp. Nil map / zero time before the first refresh
// completes. Callers must treat the map as read-only.
func (c *DEXTVLCache) Snapshot() (map[string]ProtocolTVLView, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.fetchedAt
}

// Refresh recomputes the snapshot. Per-protocol failures keep that
// protocol's previous entry (a transient read hiccup shouldn't blank
// a healthy figure) and are joined into the returned error for the
// refresher goroutine to log; protocols that compute successfully
// swap in atomically.
func (c *DEXTVLCache) Refresh(ctx context.Context) error {
	now := time.Now().UTC()
	valuer := newTVLValuer(c.src.Pricer, c.src.PegInfo, now)
	next := make(map[string]ProtocolTVLView, 4)
	prev, _ := c.Snapshot()
	var errs []error

	for _, p := range []struct {
		name    string
		refresh func(context.Context, *tvlValuer, time.Time) (*ProtocolTVLView, error)
	}{
		{"soroswap", c.refreshSoroswap},
		{"aquarius", c.refreshAquarius},
		{"phoenix", c.refreshPhoenix},
		{"comet", c.refreshComet},
	} {
		if view, err := p.refresh(ctx, valuer, now); err != nil {
			errs = append(errs, fmt.Errorf("%s tvl: %w", p.name, err))
			carryPrev(next, prev, p.name)
		} else if view != nil {
			next[p.name] = *view
		}
	}

	c.mu.Lock()
	c.snapshot = next
	c.fetchedAt = now
	c.mu.Unlock()

	err := errors.Join(errs...)
	if err != nil && c.src.Logger != nil {
		c.src.Logger.Warn("dex tvl cache refresh", "err", err)
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	obs.DEXTVLRefreshTotal.WithLabelValues(outcome).Inc()
	obs.DEXTVLRefreshDurationSeconds.WithLabelValues(outcome).Observe(time.Since(now).Seconds())
	return err
}

// carryPrev keeps a protocol's previous snapshot entry across a failed
// per-protocol refresh.
func carryPrev(next, prev map[string]ProtocolTVLView, name string) {
	if v, ok := prev[name]; ok {
		next[name] = v
	}
}

// refreshSoroswap computes Soroswap TVL from CURRENT pair reserves in
// the certified lake (pair instance storage), scoped to the
// soroswap_pairs registry. Pairs absent from the lake read (archived
// pairs, uncaptured entries) are excluded entirely — that absence is
// the reader's honest signal, not a zero. Returns (nil, nil) when the
// readers aren't wired.
func (c *DEXTVLCache) refreshSoroswap(ctx context.Context, valuer *tvlValuer, now time.Time) (*ProtocolTVLView, error) {
	if c.src.SoroswapPairs == nil || c.src.SoroswapReserves == nil {
		return nil, nil
	}
	pairs, err := c.src.SoroswapPairs.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("pair registry: %w", err)
	}
	ids := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ids = append(ids, p.PairStrkey)
	}
	states, err := c.src.SoroswapReserves.SoroswapPairReserves(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("pair reserves: %w", err)
	}

	total := new(big.Rat)
	view := ProtocolTVLView{
		AsOf: now.Format(time.RFC3339),
		Basis: "sum of current pair reserves (lake instance storage; archived pairs excluded), " +
			"valued through the served USD price tiers; unpriced legs contribute 0",
	}
	for _, st := range states {
		view.PoolsTotal++
		priced := true
		for _, leg := range []struct {
			token string
			raw   *big.Int
		}{{st.Token0, st.Reserve0}, {st.Token1, st.Reserve1}} {
			usd, ok := valuer.legUSD(ctx, leg.token, leg.raw)
			if !ok {
				priced = false
				continue
			}
			total.Add(total, usd)
		}
		if priced {
			view.PoolsPriced++
		} else {
			view.UnpricedPools++
		}
	}
	view.TVLUSD = total.FloatString(2)
	return &view, nil
}

// refreshAquarius computes Aquarius TVL from the latest per-pool
// POST-STATE reserve snapshot (aquarius_reserves) within the trailing
// window. A leg whose token address never resolved (update_reserves
// carries positions, not addresses — migration 0089) is unpriceable
// and counts its pool in UnpricedPools. Returns (nil, nil) when the
// reader isn't wired.
func (c *DEXTVLCache) refreshAquarius(ctx context.Context, valuer *tvlValuer, now time.Time) (*ProtocolTVLView, error) {
	if c.src.AquariusReserves == nil {
		return nil, nil
	}
	pools, err := c.src.AquariusReserves.LatestAquariusReserves(ctx, aquariusTVLWindowDays)
	if err != nil {
		return nil, fmt.Errorf("reserve snapshots: %w", err)
	}

	total := new(big.Rat)
	view := ProtocolTVLView{
		AsOf: now.Format(time.RFC3339),
		Basis: fmt.Sprintf("sum of each pool's latest post-state reserve snapshot (aquarius_reserves, trailing %dd), "+
			"valued through the served USD price tiers; unpriced legs contribute 0", aquariusTVLWindowDays),
	}
	for _, p := range pools {
		view.PoolsTotal++
		priced := true
		for _, leg := range p.Legs {
			if leg.Token == "" {
				priced = false
				continue
			}
			usd, ok := valuer.legUSD(ctx, leg.Token, leg.Reserve.BigInt())
			if !ok {
				priced = false
				continue
			}
			total.Add(total, usd)
		}
		if priced {
			view.PoolsPriced++
		} else {
			view.UnpricedPools++
		}
	}
	view.TVLUSD = total.FloatString(2)
	return &view, nil
}

// refreshPhoenix computes Phoenix TVL from CURRENT pool reserves in
// the certified lake (persistent ReserveA/ReserveB entries + the
// CONFIG entry's token identities), scoped to the curated ADR-0035
// pool set. Pools absent from the lake read (archived pools,
// uncaptured entries) are excluded entirely — that absence is the
// reader's honest signal, not a zero. Pools whose captured storage
// shape was unrecognised contribute 0 and are counted (see
// countUndecodablePools). Returns (nil, nil) when the readers aren't
// wired.
func (c *DEXTVLCache) refreshPhoenix(ctx context.Context, valuer *tvlValuer, now time.Time) (*ProtocolTVLView, error) {
	if c.src.PhoenixReserves == nil || len(c.src.PhoenixPools) == 0 {
		return nil, nil
	}
	states, undecodable, err := c.src.PhoenixReserves.PhoenixPoolReserves(ctx, c.src.PhoenixPools)
	if err != nil {
		return nil, fmt.Errorf("pool reserves: %w", err)
	}

	total := new(big.Rat)
	view := ProtocolTVLView{
		AsOf: now.Format(time.RFC3339),
		Basis: "sum of current pool reserves (lake persistent storage; archived pools excluded, " +
			"unrecognised storage shapes counted unpriced), valued through the served USD price tiers; " +
			"unpriced legs contribute 0",
	}
	for _, st := range states {
		view.PoolsTotal++
		priced := true
		for _, leg := range []struct {
			token string
			raw   *big.Int
		}{{st.TokenA, st.ReserveA}, {st.TokenB, st.ReserveB}} {
			usd, ok := valuer.legUSD(ctx, leg.token, leg.raw)
			if !ok {
				priced = false
				continue
			}
			total.Add(total, usd)
		}
		if priced {
			view.PoolsPriced++
		} else {
			view.UnpricedPools++
		}
	}
	c.countUndecodablePools(&view, "phoenix", undecodable)
	view.TVLUSD = total.FloatString(2)
	return &view, nil
}

// refreshComet computes Comet TVL from the CURRENT per-token balance
// records in the certified lake (the AllRecordData entry), scoped to
// the curated allowlist. Same absence / undecodable semantics as
// refreshPhoenix. Returns (nil, nil) when the readers aren't wired.
func (c *DEXTVLCache) refreshComet(ctx context.Context, valuer *tvlValuer, now time.Time) (*ProtocolTVLView, error) {
	if c.src.CometReserves == nil || len(c.src.CometPools) == 0 {
		return nil, nil
	}
	states, undecodable, err := c.src.CometReserves.CometPoolReserves(ctx, c.src.CometPools)
	if err != nil {
		return nil, fmt.Errorf("pool reserves: %w", err)
	}

	total := new(big.Rat)
	view := ProtocolTVLView{
		AsOf: now.Format(time.RFC3339),
		Basis: "sum of current per-token pool balance records (lake persistent storage; archived pools " +
			"excluded, unrecognised storage shapes counted unpriced), valued through the served USD " +
			"price tiers; unpriced legs contribute 0",
	}
	for _, st := range states {
		view.PoolsTotal++
		priced := true
		for _, leg := range st.Legs {
			usd, ok := valuer.legUSD(ctx, leg.Token, leg.Balance)
			if !ok {
				priced = false
				continue
			}
			total.Add(total, usd)
		}
		if priced {
			view.PoolsPriced++
		} else {
			view.UnpricedPools++
		}
	}
	c.countUndecodablePools(&view, "comet", undecodable)
	view.TVLUSD = total.FloatString(2)
	return &view, nil
}

// countUndecodablePools folds pools whose captured storage shape the
// reader refused to decode into the view — contributing 0, counted in
// both PoolsTotal and UnpricedPools (an honest lower bound, never a
// fabricated figure) — and emits one metric-friendly warn line so a
// contract upgrade that changes the storage layout is operator-visible
// rather than a silent TVL shrink.
func (c *DEXTVLCache) countUndecodablePools(view *ProtocolTVLView, protocol string, pools []string) {
	if len(pools) == 0 {
		return
	}
	view.PoolsTotal += len(pools)
	view.UnpricedPools += len(pools)
	if c.src.Logger != nil {
		c.src.Logger.Warn("dex tvl: unrecognised pool storage shape; counted unpriced",
			"protocol", protocol, "pools", strings.Join(pools, ","), "count", len(pools))
	}
}

// tvlValuer prices raw on-chain reserve legs in USD, memoising one
// resolver hit per token per refresh.
type tvlValuer struct {
	pricer  TVLUSDPricer
	pegInfo TVLUSDPegInfo
	at      time.Time
	memo    map[string]*big.Rat // token strkey → raw-ratio USD rate; nil = unpriceable
}

func newTVLValuer(pricer TVLUSDPricer, pegInfo TVLUSDPegInfo, at time.Time) *tvlValuer {
	return &tvlValuer{pricer: pricer, pegInfo: pegInfo, at: at, memo: map[string]*big.Rat{}}
}

// classicScaleDecimals is the 7-decimal Stellar classic scale every
// raw-ratio rate is anchored against (see tvlLegUSD).
const classicScaleDecimals = 7

// legUSD values `raw` base units of `token` (a C-strkey) in USD.
// ok=false when the leg cannot be priced honestly.
//
// The math mirrors the trades.usd_volume insert path exactly
// (timescale.tradeUSDVolumeViaXLMBaseAnchor / baseAnchorEligible):
// USDPriceAt returns a RAW prices_1m ratio chain anchored against a
// 7-decimal asset (native XLM or a classic USD peg), so for A raw
// units at raw rate R the identity
//
//	usd = (A / 1e7) × R
//
// holds for ANY token — the token's own decimals cancel through the
// raw ratio; the 1e7 belongs to the anchor. The one boundary that
// does need real decimals is a per-WHOLE-UNIT price: a declared USD
// peg is valued at $1 × (A / 10^declaredDecimals) via PegInfo, whose
// scope is deliberately classic + SAC (7-decimal invariant).
func (v *tvlValuer) legUSD(ctx context.Context, token string, raw *big.Int) (*big.Rat, bool) {
	if raw == nil || raw.Sign() < 0 {
		return nil, false
	}
	if raw.Sign() == 0 {
		return new(big.Rat), true
	}
	asset, ok := tvlAssetForToken(token)
	if !ok {
		return nil, false
	}
	// Operator-declared USD peg: exactly $1 per whole unit at the
	// peg's real decimals.
	if v.pegInfo != nil {
		if decimals, pegged := v.pegInfo.QuoteUSDPegInfo(asset); pegged && decimals >= 0 {
			return new(big.Rat).SetFrac(raw, pow10(uint32(decimals))), true
		}
	}
	rate, ok := v.rateFor(ctx, asset, token)
	if !ok {
		return nil, false
	}
	usd := new(big.Rat).SetFrac(raw, pow10(classicScaleDecimals))
	return usd.Mul(usd, rate), true
}

// rateFor memoises the resolver lookup per token per refresh.
func (v *tvlValuer) rateFor(ctx context.Context, asset canonical.Asset, token string) (*big.Rat, bool) {
	if cached, seen := v.memo[token]; seen {
		return cached, cached != nil
	}
	var out *big.Rat
	if v.pricer != nil {
		rateStr, ok, err := v.pricer.USDPriceAt(ctx, asset, v.at)
		if err == nil && ok && rateStr != "" {
			if r, parsed := new(big.Rat).SetString(rateStr); parsed && r.Sign() > 0 {
				out = r
			}
		}
	}
	v.memo[token] = out
	return out, out != nil
}

// tvlAssetForToken maps a pool token strkey to its canonical pricing
// asset: the XLM SAC resolves as `native` (that's where the XLM/USD
// markets live — same rule as the trade-insert path), everything else
// as a Soroban contract asset.
func tvlAssetForToken(token string) (canonical.Asset, bool) {
	if token == canonical.XLMSacContractID {
		return canonical.NativeAsset(), true
	}
	a, err := canonical.NewSorobanAsset(token)
	if err != nil {
		return canonical.Asset{}, false
	}
	return a, true
}
