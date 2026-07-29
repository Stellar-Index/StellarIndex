package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ProtocolTVLView is the additive per-protocol TVL summary attached to
// GET /v1/protocols rows and /v1/protocols/{name} when the DEX TVL
// snapshot cache is wired and has data for the protocol. Absent for
// protocols without an absolute reserve source (phoenix/comet emit
// flow deltas, not post-state reserves — a window net-flow is NOT TVL
// and is deliberately not dressed up as one) and on cold start before
// the first background refresh completes.
type ProtocolTVLView struct {
	// TVLUSD is the summed USD value of every PRICED reserve leg across
	// the protocol's pools, as a decimal string (ADR-0003). Unpriced
	// legs contribute 0, so this is a LOWER BOUND whenever
	// UnpricedPools > 0.
	TVLUSD string `json:"tvl_usd"`
	// PoolsTotal is the number of pools with a current reserve
	// observation contributing to this snapshot.
	PoolsTotal int `json:"pools_total"`
	// PoolsPriced is the number of pools whose every reserve leg
	// resolved to a USD price.
	PoolsPriced int `json:"pools_priced"`
	// UnpricedPools is the number of pools with at least one reserve
	// leg that could not be priced in USD; their priceable legs still
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
// underlying reads are one batched lake lookup (soroswap pair
// instance entries) + one served-tier query (aquarius reserve
// snapshots) + a handful of prices_1m lookups — cheap enough for 10
// minutes, slow-moving enough that anything faster adds no signal.
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
	next := make(map[string]ProtocolTVLView, 2)
	prev, _ := c.Snapshot()
	var errs []error

	if view, err := c.refreshSoroswap(ctx, valuer, now); err != nil {
		errs = append(errs, fmt.Errorf("soroswap tvl: %w", err))
		carryPrev(next, prev, "soroswap")
	} else if view != nil {
		next["soroswap"] = *view
	}

	if view, err := c.refreshAquarius(ctx, valuer, now); err != nil {
		errs = append(errs, fmt.Errorf("aquarius tvl: %w", err))
		carryPrev(next, prev, "aquarius")
	} else if view != nil {
		next["aquarius"] = *view
	}

	c.mu.Lock()
	c.snapshot = next
	c.fetchedAt = now
	c.mu.Unlock()

	err := errors.Join(errs...)
	if err != nil && c.src.Logger != nil {
		c.src.Logger.Warn("dex tvl cache refresh", "err", err)
	}
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
