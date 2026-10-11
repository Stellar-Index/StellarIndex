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
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ProtocolTVLView is the additive per-protocol TVL summary on GET /v1/protocols rows and
// /v1/protocols/{name}. Phoenix and Comet events carry flow deltas, not post-state reserves, so
// their figures come from the pools' STORAGE entries in the lake; a window net-flow is NOT TVL.
// Absent for protocols with no absolute reserve source (SDEX is an order book) and on cold start.
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
	// AsOfLedger is the highest ledger at which any contributing pool's reserves changed. A pool
	// untouched since an earlier ledger is EQUALLY current: the reserve readers' contract is
	// "current as of this ledger and unchanged since". 0 (omitted) when no pool carried a ledger.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// AsOf is when the snapshot was computed (RFC3339).
	AsOf string `json:"as_of"`
	// Basis is a one-line provenance statement of what was measured
	// and how it was valued.
	Basis string `json:"basis"`
}

// DEXTVLRefreshInterval is the cadence at which the API's background goroutine calls Refresh.
// The reads are three batched lake lookups, one served-tier query and a few prices_1m lookups;
// TVL moves slowly enough that anything faster adds no signal.
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

// PhoenixTVLReserveReader reads current Phoenix pool reserves and token identities from the
// pools' persistent storage in the lake (production: *clickhouse.ExplorerReader). The second
// return lists pools whose storage did not decode to the recognised shape: counted, never
// guessed. Pools absent from BOTH returns are "unavailable", NEVER zero TVL.
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

// TVLUSDPricer resolves an on-chain asset's USD price at a point in time. Production wiring is
// *timescale.VWAPUSDFXResolver, the same tier system that stamps trades.usd_volume, so TVL and
// volume share one methodology. The rate is a RAW prices_1m ratio anchored at the 7-decimal
// classic scale; see tvlLegUSD.
type TVLUSDPricer interface {
	USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error)
}

// TVLUSDPegInfo reports whether an asset is an operator-declared USD-pegged classic (or its SAC
// wrapper) and at what decimal scale. Nil means no peg shortcut: pegged tokens then price
// through the resolver like any other asset, or count unpriced.
type TVLUSDPegInfo interface {
	QuoteUSDPegInfo(asset canonical.Asset) (decimals int, ok bool)
}

// TVLValueGate is the serving-side TRUST gate on a reserve leg: may this
// platform publish a USD valuation of this asset at all?
//
// Without it `rateFor` consults only the resolver, whose sole floor is one cent of quote
// notional, so a scam-flagged issuer's token with one self-traded $0.01 minute would be valued
// into a pool's TVL and the protocol headline. An attacker-authored number is not a lower bound.
//
// Production routes to cmd/stellarindex-api's priceWithheld chokepoint (substance gate OR scam
// gate, never consulted separately). Nil is a valid allow-everything gate; buildDEXTVLValueGate
// must return a nil INTERFACE when neither guard is wired, since an interface holding a
// non-pointer struct is never == nil.
type TVLValueGate interface {
	ValueWithheld(ctx context.Context, asset canonical.Asset) bool

	// Screens names the withholding screens this gate ACTUALLY applies, in the Basis prose of the
	// [TVLScreenScamDirectory] / [TVLScreenSubstanceFloor] constants. Empty means Basis claims
	// nothing. It is required, not an optional interface: only the gate knows which sub-guards
	// the operator left wired, and one that cannot answer must not claim to have screened.
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
	// serving trust guards refuse to price. Optional; nil values every
	// leg ungated, and says so in Basis.
	Gate TVLValueGate
	// Verified is the hand-vetted verified-currency catalogue: the only
	// non-native, non-peg assets whose reserves are valued. Nil
	// fails closed — only native XLM and declared pegs are then valued.
	Verified *currency.Catalogue
	// Logger for refresh warnings. Optional.
	Logger Logger
}

// DEXTVLCache is a read-mostly per-protocol TVL snapshot refreshed by a background goroutine,
// because the explorer read budget cannot afford per-request aggregation over reserve tables.
// Cold start serves an empty snapshot: handlers omit the tvl field, never 503.
type DEXTVLCache struct {
	mu       sync.RWMutex
	snapshot map[string]ProtocolTVLView
	pools    map[string][]DEXTVLPoolView
	carried  map[string]bool
	// unavailable names each derived protocol that published NO figure
	// on the latest refresh, with the reason.
	unavailable map[string]string
	total       *DEXTVLTotalView
	fetchedAt   time.Time
	src         DEXTVLSources
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

// SnapshotAndTotal returns the per-protocol snapshot with the headline total from the SAME
// refresh cycle. Reading Snapshot() and Total() separately can straddle a Refresh() and pair one
// cycle's figures with another's total, so callers publishing both (GET /v1/protocols) use this.
func (c *DEXTVLCache) SnapshotAndTotal() (map[string]ProtocolTVLView, *DEXTVLTotalView, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.total, c.fetchedAt
}

// Protocol returns one protocol's published view with the per-pool breakdown it was summed from
// and whether it is carried forward from an earlier cycle. ok=false when the protocol has no
// entry. Callers must treat the pools slice as read-only.
func (c *DEXTVLCache) Protocol(name string) (DEXTVLProtocolSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	view, ok := c.snapshot[name]
	if !ok {
		return DEXTVLProtocolSnapshot{}, false
	}
	// The entry's own computed time: a carried-forward figure is older
	// than the cycle that carried it.
	at, err := time.Parse(time.RFC3339, view.AsOf)
	if err != nil {
		at = c.fetchedAt
	}
	return DEXTVLProtocolSnapshot{
		TVL:            view,
		Pools:          c.pools[name],
		CarriedForward: c.carried[name],
		FetchedAt:      at,
	}, true
}

// Unavailable reports why a protocol whose TVL IS derived on this
// deployment published no figure on the latest refresh; ok=false when it
// published one, or is not derived here at all.
func (c *DEXTVLCache) Unavailable(name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	reason, ok := c.unavailable[name]
	return reason, ok
}

// tvlProtocolResult is one protocol's refreshed figure plus the pools it
// was built from; the two are published together or carried together.
type tvlProtocolResult struct {
	view  ProtocolTVLView
	pools []DEXTVLPoolView
}

// dexTVLDerivation names one protocol this cache can compute a figure
// for, paired with the refresh that computes it.
type dexTVLDerivation struct {
	name    string
	refresh func(context.Context, *tvlValuer, time.Time) (*tvlProtocolResult, error)
}

// derivations is the CLOSED set of protocols the snapshot derives a TVL figure for. It is a
// method so the scope guard (TestDEXTVLScope_PooledProtocolIsDerivedOrExplicitlyExcluded) can
// read the list without a second copy. A pooled protocol not in here must name itself in
// dexTVLScopeExclusions: the headline may omit a protocol, never silently.
func (c *DEXTVLCache) derivations() []dexTVLDerivation {
	return []dexTVLDerivation{
		{"soroswap", c.refreshSoroswap},
		{"aquarius", c.refreshAquarius},
		{"phoenix", c.refreshPhoenix},
		{"comet", c.refreshComet},
	}
}

// Refresh recomputes the snapshot. A per-protocol failure keeps that protocol's previous entry,
// so a transient read error does not blank a healthy figure, and is joined into the returned
// error; protocols that compute successfully swap in atomically.
func (c *DEXTVLCache) Refresh(ctx context.Context) error {
	now := time.Now().UTC()
	valuer := newTVLValuer(c.src.Pricer, c.src.PegInfo, c.src.Gate, now)
	valuer.verified = tvlVerifiedIdentities(c.src.Verified)
	next := make(map[string]ProtocolTVLView, 4)
	nextPools := make(map[string][]DEXTVLPoolView, 4)
	prev, _ := c.Snapshot()
	var errs []error
	// carried names protocols serving a PREVIOUS cycle's figure after a failed read. It is tracked
	// explicitly, not inferred from as_of: RFC3339 is second-resolution, so two refreshes can
	// share a stamp and a stale value would pass an equality test.
	var carried []string
	carriedSet := map[string]bool{}
	// unavailable names each derived protocol publishing no figure this cycle, so the headline
	// never drops one unnamed.
	var unavailable []DEXTVLExclusion

	for _, p := range c.derivations() {
		// A price-read failure is judged per protocol — see beginPass.
		valuer.beginPass()
		res, err := p.refresh(ctx, valuer, now)
		switch {
		case errors.Is(err, errTVLNoObservedPools):
			// Not a read failure: the reader answered, with nothing.
			unavailable = append(unavailable, DEXTVLExclusion{Subject: p.name, Reason: dexTVLUnavailableNoPools})
		case err != nil:
			errs = append(errs, fmt.Errorf("%s tvl: %w", p.name, err))
			if !carryPrev(next, prev, p.name) {
				unavailable = append(unavailable, DEXTVLExclusion{Subject: p.name, Reason: dexTVLUnavailableReadFailed})
				continue
			}
			carried = append(carried, p.name)
			carriedSet[p.name] = true
			nextPools[p.name] = c.prevPools(p.name)
		case res != nil:
			next[p.name] = res.view
			nextPools[p.name] = res.pools
		}
	}

	// Reconcile the headline from the figures about to be published, not the rationals they were
	// rounded from (see reconcileDEXTVLTotal). Once per refresh, so the divergence verdict matches
	// the instant that produced the parts.
	total := reconcileDEXTVLTotal(next, now, carried, unavailable)
	unavailableSet := make(map[string]string, len(unavailable))
	for _, u := range unavailable {
		unavailableSet[u.Subject] = u.Reason
	}

	c.mu.Lock()
	c.snapshot = next
	c.pools = nextPools
	c.carried = carriedSet
	c.unavailable = unavailableSet
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

// tvlBasisUnpricedTail ends every protocol's Basis with the lower-bound contract: an unpriced leg
// contributes exactly 0 and its pool counts in UnpricedPools.
const tvlBasisUnpricedTail = "; unpriced legs contribute 0"

// tvlBasisIdentityClause states the identity screen every leg passes before any price is
// consulted. It has no off switch; a nil catalogue narrows it rather than lifting it.
const tvlBasisIdentityClause = "; only native XLM, a declared USD peg or an asset in the " +
	"verified currency catalogue is valued, and any other token's leg is counted unpriced"

// TVLScreenScamDirectory / TVLScreenSubstanceFloor are the Basis phrases naming each screen a
// [TVLValueGate] can apply. They live beside the sentence they are spliced into so the adapter
// that knows which screens are wired names them by constant, not a second copy of the prose.
const (
	TVLScreenScamDirectory  = "directory-flagged issuer"
	TVLScreenSubstanceFloor = "a market below the substance floor"
)

// basisTail returns the lower-bound clause for THIS cache's wiring, naming only the screens the
// gate says it ran. A fixed sentence naming both whenever a gate is non-nil would misreport an
// operator running [pricing_guard] disable_substance_gate = true (buildSubstanceGate returns
// nil), claiming every leg was screened against the substance floor.
func (c *DEXTVLCache) basisTail() string {
	if c.src.Gate == nil {
		return tvlBasisIdentityClause + tvlBasisUnpricedTail
	}
	screens := c.src.Gate.Screens()
	if len(screens) == 0 {
		return tvlBasisIdentityClause + tvlBasisUnpricedTail
	}
	return tvlBasisIdentityClause + tvlBasisUnpricedTail +
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

// prevPools returns the pools published for name on the previous cycle, so a carried-forward
// figure keeps the breakdown it was summed from, not an empty list beside a non-zero figure.
func (c *DEXTVLCache) prevPools(name string) []DEXTVLPoolView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pools[name]
}

// refreshSoroswap computes Soroswap TVL from CURRENT pair reserves in the lake, scoped to the
// soroswap_pairs registry. Pairs absent from the lake read are excluded: absence is the
// reader's honest signal, not a zero. Returns (nil, nil) when the readers aren't wired.
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
	return finishTVLProtocol(acc)
}

// errTVLNoObservedPools is finishTVLProtocol's refusal of a pass whose
// reader returned no pool: Refresh publishes no figure and names the
// protocol unavailable, without counting a refresh failure.
var errTVLNoObservedPools = errors.New("reserve read returned no pools")

// finishTVLProtocol renders an accumulator into the result Refresh publishes, or refuses when a
// price read ERRORED: such a figure is short by every leg of the unreadable token and says so
// nowhere, so it becomes the refresh error and Refresh carries the previous figure forward. It is
// the one place a result is built, so no protocol can publish around it. A pass that observed no
// pool is refused with errTVLNoObservedPools: absence is "unavailable", never a fabricated "0.00".
func finishTVLProtocol(acc *tvlProtocolAccumulator) (*tvlProtocolResult, error) {
	if err := acc.valuer.passFailure(); err != nil {
		return nil, err
	}
	if acc.view.PoolsTotal == 0 {
		return nil, errTVLNoObservedPools
	}
	view, pools := acc.finish()
	return &tvlProtocolResult{view: view, pools: pools}, nil
}

// refreshAquarius computes Aquarius TVL from the latest per-pool POST-STATE reserve snapshot
// within the trailing window. A leg whose token address never resolved (update_reserves carries
// positions, not addresses) is unpriceable and counts its pool in UnpricedPools. Returns
// (nil, nil) when the reader isn't wired.
func (c *DEXTVLCache) refreshAquarius(ctx context.Context, valuer *tvlValuer, now time.Time) (*tvlProtocolResult, error) {
	if c.src.AquariusReserves == nil {
		return nil, nil
	}
	pools, err := c.src.AquariusReserves.LatestAquariusReserves(ctx, aquariusTVLWindowDays)
	if err != nil {
		return nil, fmt.Errorf("reserve snapshots: %w", err)
	}

	acc := newTVLProtocolAccumulator(valuer, now.Format(time.RFC3339),
		fmt.Sprintf("sum of each pool's latest post-state reserve snapshot (aquarius_reserves, trailing %dd; "+
			"token identities recovered by position from the pool's deposit/withdraw events, and a pool "+
			"touched by two transactions at the same operation and event index in its latest ledger "+
			"reads either one's post-state), valued through the served USD price tiers%s",
			aquariusTVLWindowDays, c.basisTail()))
	for _, p := range pools {
		legs := make([]tvlLegInput, 0, len(p.Legs))
		for _, leg := range p.Legs {
			// An empty Token is a position whose address never resolved;
			// the accumulator files it as unresolved_token.
			legs = append(legs, tvlLegInput{token: leg.Token, raw: leg.Reserve.BigInt()})
		}
		acc.addPool(ctx, p.ContractID, p.Ledger, legs)
	}
	return finishTVLProtocol(acc)
}

// refreshPhoenix computes Phoenix TVL from CURRENT pool reserves in the lake, scoped to the
// curated pool set. Pools absent from the lake read are excluded: absence is not a zero. Pools
// with an unrecognised storage shape contribute 0 and are counted (see countUndecodablePools).
// Returns (nil, nil) when the readers aren't wired.
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
	return finishTVLProtocol(acc)
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
	return finishTVLProtocol(acc)
}

// warnUndecodablePools warns once for pools whose storage shape the reader refused to decode
// (already counted as 0 in PoolsTotal and UnpricedPools), so a contract upgrade that changes the
// storage layout is operator-visible rather than a silent TVL shrink.
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
	verified map[string]bool     // canonical asset id or SAC strkey → catalogue-verified; nil = none
	// failed is NOT memo's nil: memo[token] == nil says nobody prices the token (published as
	// no_served_price), failed[token] says the price READ errored, a fact about this refresh only.
	// See [tvlValuer.rateFor].
	failed map[string]error
	// passErr is the first price-read failure met since [tvlValuer.beginPass]:
	// the protocol being valued touched a token whose price could not be
	// read, so its figure must not be published ([finishTVLProtocol]).
	passErr error
}

func newTVLValuer(pricer TVLUSDPricer, pegInfo TVLUSDPegInfo, gate TVLValueGate, at time.Time) *tvlValuer {
	return &tvlValuer{
		pricer:   pricer,
		pegInfo:  pegInfo,
		gate:     gate,
		at:       at,
		memo:     map[string]*big.Rat{},
		gateMemo: map[string]bool{},
		failed:   map[string]error{},
	}
}

// beginPass opens one protocol's valuation. Memos are shared across the refresh, but whether a
// price read failed is a per-protocol verdict: a protocol that never held the failing token still
// publishes this cycle's figure.
func (v *tvlValuer) beginPass() { v.passErr = nil }

// passFailure reports the first price-read failure the current pass met,
// memoised or fresh.
func (v *tvlValuer) passFailure() error { return v.passErr }

// classicScaleDecimals is the 7-decimal Stellar classic scale every
// raw-ratio rate is anchored against (see tvlLegUSD).
const classicScaleDecimals = 7

// tvlLegPriceReadFailed marks a leg whose price read ERRORED. It is never published: a pass that
// produced one is refused whole by [finishTVLProtocol]. It exists so a failed read cannot be
// reported as DEXTVLLegNoServedPrice.
const tvlLegPriceReadFailed = "price_read_failed"

// legUSD values `raw` base units of `token` (a C-strkey) in USD.
// ok=false when the leg cannot be priced honestly.
//
// The math mirrors the trades.usd_volume insert path (timescale.usdVolumeViaXLMBaseAnchor).
// USDPriceAt returns a RAW prices_1m ratio chain anchored against a 7-decimal asset, so for A raw
// units at raw rate R
//
//	usd = (A / 1e7) × R
//
// holds for ANY token: its own decimals cancel through the raw ratio and the 1e7 belongs to the
// anchor. A declared USD peg is the exception, valued at $1 × (A / 10^declaredDecimals) via
// PegInfo, whose scope is deliberately classic + SAC (7-decimal invariant).
func (v *tvlValuer) legUSD(ctx context.Context, token string, raw *big.Int) (*big.Rat, bool) {
	val := v.value(ctx, token, raw)
	return val.usd, val.usd != nil
}

// tvlLegValue is one leg's valuation verdict: usd is nil exactly when excluded is set. asset is
// the canonical identity the price path was asked about (empty if none), basis how usd was derived.
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
	// Trust gates run BEFORE the declared-peg shortcut, as on the asset detail path
	// (suppressScamIssuerPricing after fillDeclaredPegPrice, so a re-fill cannot resurrect a
	// withheld value): a declared 1:1-USD token may have an issuer the directory flagged later.
	if v.withheld(ctx, token, asset) {
		return tvlLegValue{asset: id, excluded: DEXTVLLegWithheld}
	}
	decimals, pegged := v.pegDecimals(asset)
	if !pegged && !v.identified(token, asset, id) {
		return tvlLegValue{asset: id, excluded: DEXTVLLegUnverifiedAsset}
	}
	// Operator-declared USD peg: exactly $1 per whole unit at the
	// peg's real decimals.
	if pegged {
		return tvlLegValue{
			usd:   new(big.Rat).SetFrac(raw, pow10(uint32(decimals))),
			asset: id,
			basis: DEXTVLBasisDeclaredUSDPeg,
		}
	}
	rate, ok, err := v.rateFor(ctx, asset, token)
	if err != nil {
		return tvlLegValue{asset: id, excluded: tvlLegPriceReadFailed}
	}
	if !ok {
		return tvlLegValue{asset: id, excluded: DEXTVLLegNoServedPrice}
	}
	usd := new(big.Rat).SetFrac(raw, pow10(classicScaleDecimals))
	return tvlLegValue{usd: usd.Mul(usd, rate), asset: id, basis: DEXTVLBasisServedUSDPrice}
}

// pegDecimals reports whether asset is an operator-declared USD peg and
// at what decimals.
func (v *tvlValuer) pegDecimals(asset canonical.Asset) (int, bool) {
	if v.pegInfo == nil {
		return 0, false
	}
	decimals, pegged := v.pegInfo.QuoteUSDPegInfo(asset)
	return decimals, pegged && decimals >= 0
}

// identified reports whether a pool leg's asset is one this platform vouches for: native XLM, or
// a verified-catalogue asset reached by canonical id or SAC. Any other token is permissionless
// to deploy and pair, so its reserve and VWAP are creator-authored; the one-cent and substance
// floors are a few dollars of self-trading away.
func (v *tvlValuer) identified(token string, asset canonical.Asset, id string) bool {
	if asset.Type == canonical.AssetNative {
		return true
	}
	return v.verified[id] || v.verified[token]
}

// tvlVerifiedIdentities indexes the catalogue's Stellar entries by canonical asset id AND SAC
// contract id: pool legs carry C-strkeys, and a SAC not declared in [supply].sac_wrappers stays
// a Soroban asset under canonical.CanonicalAsset while still being the verified asset. A nil
// catalogue yields an empty set (fail closed).
func tvlVerifiedIdentities(cat *currency.Catalogue) map[string]bool {
	out := map[string]bool{}
	for _, vc := range cat.StellarIssued() {
		se := vc.StellarEntry()
		if se.AssetID != "" {
			out[se.AssetID] = true
		}
		if se.Contract != "" {
			out[se.Contract] = true
		}
		asset, err := canonical.ParseAsset(se.AssetID)
		if err != nil {
			continue
		}
		if sac, err := asset.SacContractID(); err == nil {
			out[sac] = true
		}
	}
	return out
}

// withheld memoises the trust verdict per token per refresh.
//
// The gate is asked about the token's CANONICAL identity, not its raw pool form. Pool legs are
// C-strkeys, but the scam directory is keyed by the ISSUER G-address only a classic asset
// carries, so a configured classic-SAC wrapper must collapse to its classic twin first
// (canonical.CanonicalAsset, as /v1/assets/{sac} does). An undeclared SAC stays Soroban: the
// substance gate still measures it, the scam gate cannot speak about it.
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
//
// It has THREE outcomes. Folding an ERRORED read into "unpriceable" would publish the leg as
// no_served_price with no error, so carry-forward (which fires only on a refresh error) could not
// run: one transient Postgres error on the XLM rate would drop every XLM leg and publish the
// shrunken total as fresh. An error is its own outcome, kept in v.failed rather than v.memo so
// the failing store is asked once per refresh, and recorded against the current pass so
// [finishTVLProtocol] refuses to publish the figure.
func (v *tvlValuer) rateFor(ctx context.Context, asset canonical.Asset, token string) (*big.Rat, bool, error) {
	if err, bad := v.failed[token]; bad {
		return nil, false, v.notePassFailure(err)
	}
	if cached, seen := v.memo[token]; seen {
		return cached, cached != nil, nil
	}
	var out *big.Rat
	if v.pricer != nil {
		rateStr, ok, err := v.pricer.USDPriceAt(ctx, asset, v.at)
		if err != nil {
			err = fmt.Errorf("usd price read for %s: %w", token, err)
			v.failed[token] = err
			return nil, false, v.notePassFailure(err)
		}
		if ok && rateStr != "" {
			if r, parsed := new(big.Rat).SetString(rateStr); parsed && r.Sign() > 0 {
				out = r
			}
		}
	}
	v.memo[token] = out
	return out, out != nil, nil
}

// notePassFailure records err against the current pass, keeping the
// first, and hands it back.
func (v *tvlValuer) notePassFailure(err error) error {
	if v.passErr == nil {
		v.passErr = err
	}
	return err
}

// tvlAssetForToken maps a pool token strkey to its canonical pricing
// asset: the XLM SAC resolves as `native` (that's where the XLM/USD
// markets live — same rule as the trade-insert path), everything else
// as a Soroban contract asset.
func tvlAssetForToken(token string) (canonical.Asset, bool) {
	if token == canonical.NativeSACContractID() {
		return canonical.NativeAsset(), true
	}
	a, err := canonical.NewSorobanAsset(token)
	if err != nil {
		return canonical.Asset{}, false
	}
	return a, true
}
