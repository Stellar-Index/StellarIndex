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
	// AsOfLedger is the highest ledger at which any contributing pool's
	// reserves changed — this protocol's chain high-water, the same
	// convention /v1/liquidity-pools stamps per pool and SDEXOrderBook
	// stamps for its book. A pool untouched since an earlier ledger is
	// EQUALLY current: every reserve reader's contract is "current as of
	// this ledger and unchanged since". 0 (field omitted) when no
	// contributing pool carried a ledger — never a guess.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
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

// TVLValueGate is the serving-side TRUST gate on a reserve leg: may this
// platform publish a USD valuation of this asset at all? (#338)
//
// It is the same question — and, in production, literally the same
// decision function — every other served price surface asks. The TVL
// path used to ask nobody: `rateFor` consulted only the resolver, whose
// sole floor is one cent of quote notional, so a directory-scam-flagged
// issuer's token with a single self-traded $0.01 minute was valued into
// a pool's TVL at its own VWAP and summed into the protocol headline.
// A number an attacker authors is not a lower bound, it is a lie with a
// "≥" in front of it.
//
// Production wiring routes to cmd/stellarindex-api's priceWithheld
// chokepoint (substance gate OR scam gate — the MSP-cluster invariant
// that the two are never consulted separately). Nil is a valid
// allow-everything gate, so a deployment with [pricing_guard] disabled
// keeps today's figures — and the production builder
// (cmd/stellarindex-api's buildDEXTVLValueGate) returns a nil INTERFACE
// when neither guard is wired, because an interface holding a
// non-pointer struct is never == nil however empty the struct is.
//
// Two things belong to that production adapter rather than here,
// because that is where the gates and the operator's peg list already
// live: the quote set a "does this asset have a publishable USD price"
// question expands to (vs XLM / vs fiat:USD / vs a declared peg — the
// same three [Server.listingPriceAllowed] tries), and the
// low-cardinality metric label the guards count the verdict under
// (obs.PriceServe{Scam,Substance}WithheldTotal).
type TVLValueGate interface {
	ValueWithheld(ctx context.Context, asset canonical.Asset) bool

	// Screens names the withholding screens this gate ACTUALLY applies,
	// in the Basis prose the [TVLScreenScamDirectory] /
	// [TVLScreenSubstanceFloor] constants spell. Empty means the gate
	// withholds nothing, and Basis then claims nothing.
	//
	// It is a required method, not an optional companion interface, on
	// purpose: the gate is the only thing that knows which of its
	// sub-guards the operator left wired, and a gate that cannot answer
	// must not be describable as having screened anything.
	Screens() []string
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
	// Gate withholds the USD valuation of a reserve leg whose asset the
	// serving trust guards refuse to price (#338). Optional; nil values
	// every leg exactly as before, and says so in Basis.
	Gate TVLValueGate
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
	pools     map[string][]DEXTVLPoolView
	carried   map[string]bool
	total     *DEXTVLTotalView
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

// Protocol returns one protocol's published view together with the
// per-pool breakdown it was summed from, and whether the entry is a
// carried-forward figure from an earlier cycle. ok=false when the
// protocol has no entry (no derivation wired, or cold start). Callers
// must treat the pools slice as read-only.
func (c *DEXTVLCache) Protocol(name string) (DEXTVLProtocolSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	view, ok := c.snapshot[name]
	if !ok {
		return DEXTVLProtocolSnapshot{}, false
	}
	return DEXTVLProtocolSnapshot{
		TVL:            view,
		Pools:          c.pools[name],
		CarriedForward: c.carried[name],
	}, true
}

// tvlProtocolResult is one protocol's refreshed figure plus the pools it
// was built from; the two are published together or carried together.
type tvlProtocolResult struct {
	view  ProtocolTVLView
	pools []DEXTVLPoolView
}

// Refresh recomputes the snapshot. Per-protocol failures keep that
// protocol's previous entry (a transient read hiccup shouldn't blank
// a healthy figure) and are joined into the returned error for the
// refresher goroutine to log; protocols that compute successfully
// swap in atomically.
func (c *DEXTVLCache) Refresh(ctx context.Context) error {
	now := time.Now().UTC()
	valuer := newTVLValuer(c.src.Pricer, c.src.PegInfo, c.src.Gate, now)
	next := make(map[string]ProtocolTVLView, 4)
	nextPools := make(map[string][]DEXTVLPoolView, 4)
	prev, _ := c.Snapshot()
	var errs []error
	// carried names the protocols serving a PREVIOUS cycle's figure
	// because this cycle's read failed. Tracked explicitly rather than
	// inferred from the entry's as_of: RFC3339 is second-resolution and
	// two refreshes can share a stamp, so a value that merely LOOKS
	// current would silently pass an equality test.
	var carried []string
	carriedSet := map[string]bool{}

	for _, p := range []struct {
		name    string
		refresh func(context.Context, *tvlValuer, time.Time) (*tvlProtocolResult, error)
	}{
		{"soroswap", c.refreshSoroswap},
		{"aquarius", c.refreshAquarius},
		{"phoenix", c.refreshPhoenix},
		{"comet", c.refreshComet},
	} {
		if res, err := p.refresh(ctx, valuer, now); err != nil {
			errs = append(errs, fmt.Errorf("%s tvl: %w", p.name, err))
			if carryPrev(next, prev, p.name) {
				carried = append(carried, p.name)
				carriedSet[p.name] = true
				nextPools[p.name] = c.prevPools(p.name)
			}
		} else if res != nil {
			next[p.name] = res.view
			nextPools[p.name] = res.pools
		}
	}

	// Reconcile the headline total from the figures we are about to
	// publish, not from the rationals they were rounded out of — see
	// reconcileDEXTVLTotal. It runs here, once per refresh, so the
	// divergence verdict is computed against the refresh instant that
	// produced the parts rather than re-derived per request.
	total := reconcileDEXTVLTotal(next, now, carried)

	c.mu.Lock()
	c.snapshot = next
	c.pools = nextPools
	c.carried = carriedSet
	c.total = total
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

// tvlBasisUnpricedTail is the clause every protocol's Basis ends with:
// the honest lower-bound contract. An unpriced leg contributes exactly
// 0 and its pool counts in UnpricedPools, which is what the explorer's
// "≥" prefix and hatched bar tail render.
const tvlBasisUnpricedTail = "; unpriced legs contribute 0"

// TVLScreenScamDirectory / TVLScreenSubstanceFloor are the Basis
// phrases naming each withholding screen a [TVLValueGate] can apply.
// They live here, beside the sentence they are spliced into, so the
// production adapter that knows WHICH screens are wired
// (cmd/stellarindex-api's dexTVLValueGate) names them by constant
// instead of re-typing the prose into a second copy.
const (
	TVLScreenScamDirectory  = "directory-flagged issuer"
	TVLScreenSubstanceFloor = "a market below the substance floor"
)

// basisTail returns the lower-bound clause matching THIS cache's wiring
// — naming the screens the gate says it actually ran, never the full
// set. Written from what ships, not from intent.
//
// Until 2026-09-03 this returned one fixed sentence naming BOTH screens
// whenever a gate was non-nil, and the API binary wired the gate
// unconditionally as a non-pointer struct (so `Gate == nil` was never
// true in production). An operator running [pricing_guard]
// disable_substance_gate = true — which makes buildSubstanceGate return
// nil — was therefore told by every /v1/protocols response, and by the
// explorer tooltip that renders Basis verbatim, that each leg had been
// screened against the substance floor. It had not been.
func (c *DEXTVLCache) basisTail() string {
	if c.src.Gate == nil {
		return tvlBasisUnpricedTail
	}
	screens := c.src.Gate.Screens()
	if len(screens) == 0 {
		return tvlBasisUnpricedTail
	}
	return tvlBasisUnpricedTail +
		", and a leg whose asset the serving trust gates withhold (" +
		strings.Join(screens, ", or ") +
		") is counted unpriced rather than valued"
}

// carryPrev keeps a protocol's previous snapshot entry across a failed
// per-protocol refresh, reporting whether it actually carried one (a
// first-cycle failure has no previous entry to keep, and the protocol
// is simply absent).
func carryPrev(next, prev map[string]ProtocolTVLView, name string) bool {
	v, ok := prev[name]
	if ok {
		next[name] = v
	}
	return ok
}

// prevPools returns the pools published for name on the previous cycle,
// so a carried-forward figure travels with the breakdown it was summed
// from — the drill-down for a carried protocol shows the pools that
// produced the carried number, not an empty list beside a non-zero
// figure.
func (c *DEXTVLCache) prevPools(name string) []DEXTVLPoolView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pools[name]
}

// refreshSoroswap computes Soroswap TVL from CURRENT pair reserves in
// the certified lake (pair instance storage), scoped to the
// soroswap_pairs registry. Pairs absent from the lake read (archived
// pairs, uncaptured entries) are excluded entirely — that absence is
// the reader's honest signal, not a zero. Returns (nil, nil) when the
// readers aren't wired.
func (c *DEXTVLCache) refreshSoroswap(ctx context.Context, valuer *tvlValuer, now time.Time) (*tvlProtocolResult, error) {
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

	acc := newTVLProtocolAccumulator(valuer, now.Format(time.RFC3339),
		"sum of current pair reserves (lake instance storage; archived pairs excluded), "+
			"valued through the served USD price tiers"+c.basisTail())
	for _, st := range states {
		acc.addPool(ctx, st.Pair, st.Ledger, []tvlLegInput{
			{token: st.Token0, raw: st.Reserve0},
			{token: st.Token1, raw: st.Reserve1},
		})
	}
	return finishTVLProtocol(acc), nil
}

// finishTVLProtocol renders an accumulator into the result Refresh
// publishes.
func finishTVLProtocol(acc *tvlProtocolAccumulator) *tvlProtocolResult {
	view, pools := acc.finish()
	return &tvlProtocolResult{view: view, pools: pools}
}

// refreshAquarius computes Aquarius TVL from the latest per-pool
// POST-STATE reserve snapshot (aquarius_reserves) within the trailing
// window. A leg whose token address never resolved (update_reserves
// carries positions, not addresses — migration 0089) is unpriceable
// and counts its pool in UnpricedPools. Returns (nil, nil) when the
// reader isn't wired.
func (c *DEXTVLCache) refreshAquarius(ctx context.Context, valuer *tvlValuer, now time.Time) (*tvlProtocolResult, error) {
	if c.src.AquariusReserves == nil {
		return nil, nil
	}
	pools, err := c.src.AquariusReserves.LatestAquariusReserves(ctx, aquariusTVLWindowDays)
	if err != nil {
		return nil, fmt.Errorf("reserve snapshots: %w", err)
	}

	acc := newTVLProtocolAccumulator(valuer, now.Format(time.RFC3339),
		fmt.Sprintf("sum of each pool's latest post-state reserve snapshot (aquarius_reserves, trailing %dd), "+
			"valued through the served USD price tiers%s", aquariusTVLWindowDays, c.basisTail()))
	for _, p := range pools {
		legs := make([]tvlLegInput, 0, len(p.Legs))
		for _, leg := range p.Legs {
			// An empty Token is a position whose address never resolved;
			// the accumulator files it as unresolved_token.
			legs = append(legs, tvlLegInput{token: leg.Token, raw: leg.Reserve.BigInt()})
		}
		acc.addPool(ctx, p.ContractID, p.Ledger, legs)
	}
	return finishTVLProtocol(acc), nil
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
func (c *DEXTVLCache) refreshPhoenix(ctx context.Context, valuer *tvlValuer, now time.Time) (*tvlProtocolResult, error) {
	if c.src.PhoenixReserves == nil || len(c.src.PhoenixPools) == 0 {
		return nil, nil
	}
	states, undecodable, err := c.src.PhoenixReserves.PhoenixPoolReserves(ctx, c.src.PhoenixPools)
	if err != nil {
		return nil, fmt.Errorf("pool reserves: %w", err)
	}

	acc := newTVLProtocolAccumulator(valuer, now.Format(time.RFC3339),
		"sum of current pool reserves (lake persistent storage; archived pools excluded, "+
			"unrecognised storage shapes counted unpriced), valued through the served USD price tiers"+
			c.basisTail())
	for _, st := range states {
		acc.addPool(ctx, st.Pool, st.Ledger, []tvlLegInput{
			{token: st.TokenA, raw: st.ReserveA},
			{token: st.TokenB, raw: st.ReserveB},
		})
	}
	acc.addUndecodable(undecodable)
	c.warnUndecodablePools("phoenix", undecodable)
	return finishTVLProtocol(acc), nil
}

// refreshComet computes Comet TVL from the CURRENT per-token balance
// records in the certified lake (the AllRecordData entry), scoped to
// the curated allowlist. Same absence / undecodable semantics as
// refreshPhoenix. Returns (nil, nil) when the readers aren't wired.
func (c *DEXTVLCache) refreshComet(ctx context.Context, valuer *tvlValuer, now time.Time) (*tvlProtocolResult, error) {
	if c.src.CometReserves == nil || len(c.src.CometPools) == 0 {
		return nil, nil
	}
	states, undecodable, err := c.src.CometReserves.CometPoolReserves(ctx, c.src.CometPools)
	if err != nil {
		return nil, fmt.Errorf("pool reserves: %w", err)
	}

	acc := newTVLProtocolAccumulator(valuer, now.Format(time.RFC3339),
		"sum of current per-token pool balance records (lake persistent storage; archived pools "+
			"excluded, unrecognised storage shapes counted unpriced), valued through the served USD "+
			"price tiers"+c.basisTail())
	for _, st := range states {
		legs := make([]tvlLegInput, 0, len(st.Legs))
		for _, leg := range st.Legs {
			legs = append(legs, tvlLegInput{token: leg.Token, raw: leg.Balance})
		}
		acc.addPool(ctx, st.Pool, st.Ledger, legs)
	}
	acc.addUndecodable(undecodable)
	c.warnUndecodablePools("comet", undecodable)
	return finishTVLProtocol(acc), nil
}

// warnUndecodablePools emits one metric-friendly warn line for pools
// whose captured storage shape the reader refused to decode (the
// accumulator has already counted them: contributing 0, in both
// PoolsTotal and UnpricedPools, published with the pool-level
// exclusion) so a contract upgrade that changes the storage layout is
// operator-visible rather than a silent TVL shrink.
func (c *DEXTVLCache) warnUndecodablePools(protocol string, pools []string) {
	if len(pools) == 0 || c.src.Logger == nil {
		return
	}
	c.src.Logger.Warn("dex tvl: unrecognised pool storage shape; counted unpriced",
		"protocol", protocol, "pools", strings.Join(pools, ","), "count", len(pools))
}

// tvlValuer prices raw on-chain reserve legs in USD, memoising one
// resolver hit — and one trust verdict — per token per refresh.
type tvlValuer struct {
	pricer   TVLUSDPricer
	pegInfo  TVLUSDPegInfo
	gate     TVLValueGate
	at       time.Time
	memo     map[string]*big.Rat // token strkey → raw-ratio USD rate; nil = unpriceable
	gateMemo map[string]bool     // token strkey → "the trust gates withhold this asset"
}

func newTVLValuer(pricer TVLUSDPricer, pegInfo TVLUSDPegInfo, gate TVLValueGate, at time.Time) *tvlValuer {
	return &tvlValuer{
		pricer:   pricer,
		pegInfo:  pegInfo,
		gate:     gate,
		at:       at,
		memo:     map[string]*big.Rat{},
		gateMemo: map[string]bool{},
	}
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
	val := v.value(ctx, token, raw)
	return val.usd, val.usd != nil
}

// tvlLegValue is one leg's valuation verdict: usd is nil exactly when
// excluded is set. asset is the canonical identity the served price
// path was asked about (empty when the token resolved to no asset),
// and basis says how usd was derived.
type tvlLegValue struct {
	usd      *big.Rat
	asset    string
	basis    string
	excluded string
}

// value is legUSD with its reasons: the same decision, in the same
// order, but every "no" names the rule that said it and every "yes"
// names the identity and the basis. The drill-down publishes these;
// the protocol figure only needs the amount.
func (v *tvlValuer) value(ctx context.Context, token string, raw *big.Int) tvlLegValue {
	if raw == nil || raw.Sign() < 0 {
		return tvlLegValue{excluded: DEXTVLLegInvalidReserve}
	}
	asset, ok := tvlAssetForToken(token)
	var id string
	if ok {
		id = canonical.CanonicalAsset(asset).String()
	}
	if raw.Sign() == 0 {
		// Nothing to value: worth exactly $0 whatever the price, so no
		// gate and no price tier is consulted — a pool with an empty
		// side is not "unpriced", it is empty.
		return tvlLegValue{usd: new(big.Rat), asset: id, basis: DEXTVLBasisEmptyReserve}
	}
	if !ok {
		return tvlLegValue{excluded: DEXTVLLegMalformedToken}
	}
	// Serving trust gates FIRST — before the declared-peg shortcut, not
	// after it (#338). Same ordering the asset detail path fixed on
	// 2026-08-25: suppressScamIssuerPricing runs AFTER fillDeclaredPegPrice
	// precisely so a re-fill cannot resurrect a withheld value. A token
	// an operator declared 1:1-USD is still a token whose issuer the
	// curated directory may since have flagged, and the flag is the
	// later, narrower, owner-level decision.
	if v.withheld(ctx, token, asset) {
		return tvlLegValue{asset: id, excluded: DEXTVLLegWithheld}
	}
	// Operator-declared USD peg: exactly $1 per whole unit at the
	// peg's real decimals.
	if v.pegInfo != nil {
		if decimals, pegged := v.pegInfo.QuoteUSDPegInfo(asset); pegged && decimals >= 0 {
			return tvlLegValue{
				usd:   new(big.Rat).SetFrac(raw, pow10(uint32(decimals))),
				asset: id,
				basis: DEXTVLBasisDeclaredUSDPeg,
			}
		}
	}
	rate, ok := v.rateFor(ctx, asset, token)
	if !ok {
		return tvlLegValue{asset: id, excluded: DEXTVLLegNoServedPrice}
	}
	usd := new(big.Rat).SetFrac(raw, pow10(classicScaleDecimals))
	return tvlLegValue{usd: usd.Mul(usd, rate), asset: id, basis: DEXTVLBasisServedUSDPrice}
}

// withheld memoises the trust verdict per token per refresh.
//
// The gate is asked about the token's CANONICAL identity, not its raw
// pool form. Pool legs are C-strkeys by construction (tvlAssetForToken
// yields a Soroban asset for everything but the XLM SAC), and the scam
// directory is keyed by the ISSUER G-address that only a classic asset
// carries — so a configured classic↔SAC wrapper must be collapsed to
// its classic twin first or the gate answers about the wrong identity.
// canonical.CanonicalAsset is the same [supply].sac_wrappers-fed
// collapse /v1/assets/{sac} applies (assets.go, "Configured classic↔SAC
// wrappers"); a SAC the operator has NOT declared stays Soroban, which
// the substance gate still measures (SubstanceGated covers
// AssetSoroban) but the scam gate cannot speak about.
func (v *tvlValuer) withheld(ctx context.Context, token string, asset canonical.Asset) bool {
	if v.gate == nil {
		return false
	}
	if cached, seen := v.gateMemo[token]; seen {
		return cached
	}
	out := v.gate.ValueWithheld(ctx, canonical.CanonicalAsset(asset))
	v.gateMemo[token] = out
	return out
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
