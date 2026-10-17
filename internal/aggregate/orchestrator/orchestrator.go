// Package orchestrator drives the aggregation pre-compute cycle: each tick it computes VWAP per (pair, window) over the last closed minute (ADR-0015) and caches it in Redis.
// Around that core: class filter, stablecoin proxy, outlier filter, triangulation, divergence refresh, confidence and the ADR-0019 freeze. Ticks are serialised so a slow Timescale is not piled on.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Store is the subset of timescale.Store the orchestrator needs, as an interface so tests can mock it.
type Store interface {
	TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error)
}

// FXStore is the subset of timescale.Store the forex-snap path needs; nil keeps FX legs on cached VWAP, and [timescale.ErrNoFXQuote] falls back to it and counts [obs.AggregatorFXSnapFallbackTotal].
type FXStore interface {
	FXQuoteAtOrBefore(ctx context.Context, pair canonical.Pair, cutoff time.Time, fxSources []string) (*big.Rat, time.Time, string, error)
}

// Cache is the subset of redis.UniversalClient we need, as an interface for test replacement; Get returns redis.Nil for an absent key (an empty-window leg), and triangulation skips that chain for the tick.
type Cache interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	// TxPipelined writes a served value with its provenance and observed-at stamp (and composite meta) in one MULTI/EXEC so no reader sees one without the other; the freeze path extends TTLs through it too.
	TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// FreezeMarker records an ActionFreeze decision (production: freeze.Writer); all methods MUST be idempotent on (asset, quote). frozenValue is the last-known-good VWAP (formatRatFixed(prev, 12), empty with no prior bucket) and reaches freeze_events via the EventSink, not the Redis marker.
type FreezeMarker interface {
	// Mark writes a marker with the default TTL and no lifecycle state, for the triangulated-composite
	// refusal: a per-tick decision about a DERIVED price with no freeze lifecycle of its own.
	Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error

	// MarkHold writes a marker carrying the ADR-0019 lifecycle state with ttl = remaining hold +
	// silence grace; the expiry is a liveness backstop, not the freeze policy.
	MarkHold(ctx context.Context, asset, quote canonical.Asset, frozenValue string,
		decision anomaly.Decision, state freeze.State, ttl time.Duration) error

	// LoadState reads back the state a previous MarkHold stamped. (State{}, false, nil) means no
	// marker: "never frozen" on a cold key, an operator force-unfreeze on a key believed frozen.
	LoadState(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error)

	// Clear deletes the marker, ending the freeze on the serving path immediately.
	Clear(ctx context.Context, asset, quote canonical.Asset) error
}

// WindowedFreezeMarker is the window-aware half of [FreezeMarker]: the marker is pair-wide but windows freeze independently, so a pair-level ladder let a cold window adopt a sibling's FiredAt, HoldUntil, ExtensionsUsed and Escalated. Optional: a writer that cannot scope keeps pair-wide behaviour, which errs towards holding.
type WindowedFreezeMarker interface {
	// MarkHoldForWindow is [FreezeMarker.MarkHold] recording `window` as the owner of `state`,
	// merged with the ladders the marker holds for the pair's other windows.
	MarkHoldForWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration,
		frozenValue string, decision anomaly.Decision, state freeze.State, ttl time.Duration) error

	// LoadStateForWindow is [FreezeMarker.LoadState] scoped to `window`: presence stays pair-wide
	// (so the operator override reads identically) but the state is the one `window` owns, or zero.
	LoadStateForWindow(ctx context.Context, asset, quote canonical.Asset,
		window time.Duration) (freeze.State, bool, error)

	// RetireWindowLadder drops `window`'s ladder but keeps the marker, for releasing one window of
	// a pair whose other windows are still frozen.
	RetireWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration) error

	// ReleaseWindow ends `window`'s freeze when this process knows of no other frozen window: it clears the
	// marker UNLESS the marker still records a sibling's ladder, then retires only `window`'s (kept=true). The
	// marker is asked because this process can't see windows it hasn't evaluated (under MinUSDVolume, or after a
	// restart), and deleting on that blind spot ended a sibling's freeze.
	ReleaseWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (kept bool, err error)
}

// FreezeOverrideReader tells an operator's force-unfreeze (a tombstone written by `stellarindex-ops freeze-unfreeze`) from a marker that lapsed on its own; optional like [WindowedFreezeMarker].
type FreezeOverrideReader interface {
	OverrideRecorded(ctx context.Context, asset, quote canonical.Asset) (bool, error)
}

// Config controls the orchestrator's behaviour; the orchestrator itself doesn't know about TOML.
type Config struct {
	// Pairs is the set to pre-compute VWAP for; empty makes the orchestrator a no-op.
	Pairs []canonical.Pair

	// Windows are the rolling windows to compute over; empty defaults to [5m, 1h, 24h].
	Windows []time.Duration

	// Interval is the gap between ticks; the 30s default leaves headroom inside the 60s Redis
	// `price:` TTL.
	Interval time.Duration

	// TickTimeout bounds one [Tick] and every store, cache and reference call in it, so one wedged query (a
	// half-open connection, a lock wait) can't stall every price. It is a WEDGE GUARD, not a budget: refreshOrder
	// is fixed, so a bound a healthy tick can exceed would starve the same tail pairs every tick. Zero defaults to
	// DefaultTickTimeoutIntervals × Interval.
	TickTimeout time.Duration

	// MaxTradesPerWindow caps per-query rows so an unexpectedly active pair cannot run away with
	// Timescale. Defaults to 10_000.
	MaxTradesPerWindow int

	// EnableStablecoinFiatProxy expands each fiat target into the direct pair plus one stablecoin-backed pair per peg and rewrites those trades onto the target via aggregate.ProxyPair before VWAP.
	// Off by default (N+1 TradesInRange calls per pair/window). Mapping at the aggregator, not the decoder, keeps a depeg visible in raw trades.
	EnableStablecoinFiatProxy bool

	// USDPeggedClassicAssets lists classic credits declared USD-pegged; the abstract stablecoin map excludes issuer-bearing credits, so without it every XLM/fiat:USD VWAP is empty on mainnet.
	// Shared with the indexer's usd_volume via cfg.Trades.USDPeggedClassicAssets; used only with EnableStablecoinFiatProxy and a fiat:USD target.
	USDPeggedClassicAssets []canonical.Asset

	// USDPeggedSorobanAssets is the SAC wrappers inheriting a USD peg from USDPeggedClassicAssets (derived by the binary, no TOML knob; same 7 decimals).
	// [usdQuoteDecimals] uses it to apply MinUSDVolume to SAC-USDC-quoted pairs, which otherwise served unguarded VWAP.
	USDPeggedSorobanAssets []canonical.Asset

	// MinUSDVolume, when > 0, requires a window's post-class, post-outlier USD volume to meet it before the VWAP publishes, so a dust trade cannot set the price. 0 = off.
	// Covers every quote [usdQuoteDecimals] can value; proxy-expanded trades are valued against their SOURCE quote; non-USD fiat converts at the [Config.FXStore] snap and fails closed without an admissible rate (dropForMinUSDVolume).
	MinUSDVolume float64

	// OutlierSigmaThreshold, when > 0, drops trades more than sigma robust scales (1.4826*MAD) from BOTH the window median and its time-local neighbourhood (aggregate.FilterOutliersLocal), so a lone wild print goes but a regime shift survives.
	// Runs after class filtering and the stablecoin rewrite; windows with < 3 prices pass. 0 = off.
	OutlierSigmaThreshold float64

	// Anomaly, when non-nil, evaluates each fresh VWAP against the previous bucket (ADR-0019): Allow publishes,
	// Warn publishes, Freeze does NOT publish and keeps serving the last-known-good value while FreezeWriter's
	// marker sets flags.frozen. Nil = every VWAP publishes; production wires it at the binary boundary.
	Anomaly *anomaly.Checker

	// Triangulations is the operator's chain set: after the per-pair loop each chain prices its target through the graph router (aggregate.BuildEdges, CombineRoutes/CompositeRate) into its own cache key.
	// Explicit rather than every fiat x stablecoin combination because cardinality is len(Windows) keys per chain per tick.
	Triangulations []TriangulationChain

	// MaxHops bounds the router's cross-rate route length in LEGS (base-hub-quote is 2) for triangulation targets. 0 = [DefaultMaxHops]; values outside [2,4] are clamped in [New].
	MaxHops int

	// MinRouteConfidence is the weakest-link confidence floor a router route must clear to count as CONFIDENT (aggregate.CombineRoutes), so a dust edge can't set a confident cross.
	// If no route clears it the composite is flagged low-confidence and NOT published over the direct price. 0 disables the floor.
	MinRouteConfidence float64

	// FreezeWriter writes the Redis freeze marker the API's freeze.Looker reads to set flags.frozen. Nil = the
	// freeze is logged and counted but unmarked, so a Phase 2 refusal serves its last value with flags.frozen
	// ABSENT: a stale price presented as fresh. The Phase 2 lifecycle runs regardless of Anomaly, so the
	// aggregator binary builds this unconditionally.
	FreezeWriter FreezeMarker

	// DisableClassFilter, when true, lets every source class contribute to VWAP. The default filter is ON:
	// aggregator-class sources re-report other venues (double-counting) and oracles publish already-aggregated
	// prices (rationale in internal/sources/external/registry.go). Phrased as Disable-X because a Go bool can't
	// tell unset from false, so the safe default must be the zero value.
	DisableClassFilter bool

	// ExcludedSources drops stored trades from the named sources at read time, before the class
	// filter (and even with DisableClassFilter), so a misbehaving source leaves VWAP without a purge.
	ExcludedSources []string

	// Phase2Thresholds tunes the ADR-0019 Phase 2 freeze (3-signal AND on confidence, z, source count); zero fields fall back to the [Default*] constants.
	Phase2Thresholds Phase2Thresholds

	// Baselines, when non-nil, feeds the per-tick confidence score (ADR-0019 §"Multi-factor confidence score"):
	// the fresh VWAP plus the cached MultiBaseline, written to `confidence:<base>:<quote>:<window>`. Nil skips the
	// step; production adapts *timescale.Store.LatestBaseline. The first tick after startup always skips, having
	// no previous VWAP to score against.
	Baselines BaselineSource

	// FXStore, when non-nil, enables the forex snap: an FX leg (fiat Base AND Quote) reads the latest FX quote
	// at-or-before the bucket end instead of its cached VWAP, so every region serving the same closed bucket gets
	// the same rate (ADR-0018). Nil = cached-VWAP FX legs, near-equivalent but not ADR-0018-strict across
	// partitions.
	FXStore FXStore

	// CompositeReference gates composite-reference corroboration of the phase-2 freeze for
	// structurally single-venue targets (composite_reference.go). Zero value is off.
	CompositeReference CompositeReferenceConfig

	// DivergenceRefresher, when non-nil, refreshes the `div:<base>/<quote>` cache once per pair per [Tick] so
	// flags.divergence_warning has a producer (ADR-0019); production is internal/divergence.Service, nil leaves
	// the flag always false. It uses the SHORTEST window's VWAP to stay ~Interval-fresh without hitting external
	// references per window.
	DivergenceRefresher DivergenceRefresher

	// DivergenceMinInterval skips the divergence pass until this long after the last successful one (zero = every tick); the CMC free tier is 10,000 calls/MONTH, so every 30s x 12 pairs is too much and 5 minutes fits.
	// The div:<base>/<quote> TTL covers this cadence plus a worst-case pass (divergence.ServiceOptions.PairCount).
	DivergenceMinInterval time.Duration

	// DivergenceMinSources is the SuccessCount floor before the confidence step's cross-oracle factor trusts a
	// cached divergence result. It must equal `cfg.Divergence.MinSourcesForWarning`, the quorum the divergence
	// worker and the API's divergence_checked use; separate copies would let freeze corroboration trust a quorum
	// the API stopped publishing. <= 0 takes [defaultDivergenceMinSources], matching divergence.NewService.
	DivergenceMinSources int

	// StreamPublisher fans each successful closed-bucket VWAP out to `/v1/price/stream` subscribers (production:
	// internal/api/streaming/redispub). Best-effort: errors log and count but never block the tick, since the
	// cache write is the source of truth. Nil = no producer, and the stream answers 503.
	StreamPublisher StreamPublisher

	// ContributionSink receives the per-source breakdown of every VWAP compute (production:
	// timescale.PriceSourceContributionSink) so the explorer's source-contribution donut reads stored history.
	// Best-effort. See docs/architecture/explorer-data-inventory.md §11.3.
	ContributionSink ContributionSink

	// DecimalsLookup corrects a window's raw VWAP for a non-7-decimal leg (aggregate.AdjustPrice). The VWAP feeds paths that bypass internal/api/v1's declineIfNonstandardDecimals, so this is the one place to stop a wrong price leaking.
	// Nil resolves both legs to StandardDecimals (factor 1).
	DecimalsLookup aggregate.DecimalsLookup

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// ContributionSink is the optional durable mirror of per-source contributions, called once per (pair, window) on every successful VWAP compute.
type ContributionSink interface {
	RecordContributions(ctx context.Context, rec ContributionRecord) error
}

// ContributionRecord is the per-(pair, window, tick) shape passed to ContributionSink, decoupled from the storage row shape.
type ContributionRecord struct {
	Pair          canonical.Pair
	Window        time.Duration
	ComputedAt    time.Time
	Contributions []aggregate.SourceContribution

	// SourceUSDVolume is the exact per-source USD volume of the POST-filter trades, keyed like Contributions.
	// Splitting a pre-filter total by post-filter weights would over-attribute dollars when outliers drop, so the
	// sink persists this and volume_usd matches what VWAP saw.
	SourceUSDVolume map[string]*big.Rat
}

// DivergenceRefresher keeps `div:<base>/<quote>` populated (production: [internal/divergence.Service]); ourPrice is the pair's shortest-window VWAP. RefreshPinnedPair serves a frozen pair, whose cached VWAP is the pinned last-known-good: references refresh but no verdict is reached against it.
type DivergenceRefresher interface {
	RefreshPair(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time) error
	RefreshPinnedPair(ctx context.Context, pair canonical.Pair, pinnedPrice float64, observedAt time.Time) error
}

// StreamPublisher fans closed-bucket events out (production: redispub.Publisher), best-effort since the VWAP cache is durable; coverage is nil when unknown. PublishFrozenBucket is called for a bucket a freeze refused so a frozen series is not silent; frozenSince is its first refused bucket's end.
type StreamPublisher interface {
	PublishClosedBucket(ctx context.Context, pair canonical.Pair, window time.Duration, valueDecimal string, observedAt time.Time, coverage *cachekeys.WindowCoverage) error
	PublishFrozenBucket(ctx context.Context, pair canonical.Pair, window time.Duration, observedAt, frozenSince time.Time) error
}

// DefaultWindows is the built-in window set: hot (5m), warm (1h), cold (24h).
var DefaultWindows = []time.Duration{
	5 * time.Minute,
	1 * time.Hour,
	24 * time.Hour,
}

// normalizeWindows returns the positive windows ascending and de-duplicated in a fresh slice;
// refreshDivergenceAll reads Windows[0] as the shortest.
func normalizeWindows(ws []time.Duration) []time.Duration {
	out := make([]time.Duration, 0, len(ws))
	for _, w := range ws {
		if w > 0 {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// effectiveWindows is the normalized set [New] runs, or [DefaultWindows] when none is positive.
func effectiveWindows(ws []time.Duration) []time.Duration {
	if out := normalizeWindows(ws); len(out) > 0 {
		return out
	}
	return DefaultWindows
}

// LongestWindow is the longest window an orchestrator built from ws runs.
func LongestWindow(ws []time.Duration) time.Duration {
	eff := effectiveWindows(ws)
	return eff[len(eff)-1]
}

// ShortestWindow is the window the divergence refresh compares for a window set (Windows[0]); the divergence service stamps it on every cached verdict.
func ShortestWindow(ws []time.Duration) time.Duration {
	return effectiveWindows(ws)[0]
}

// closedBucket is the closed-bucket granularity: every window ends at the last closed boundary of this size, never an in-progress one (ADR-0015).
// It is also the SSE observed_at cadence and the return granularity ADR-0019 baselines are calibrated at.
const closedBucket = time.Minute

// decidedBucket is the outcome of the first evaluation of one closed bucket for one (pair,
// window); see [Orchestrator.refreshPairWindow].
type decidedBucket struct {
	end time.Time
	// published is nil when the bucket was held (frozen, empty, below the volume floor).
	published *publishedBucket
	// frozen keeps a replaying tick's leg out of triangulation.
	frozen       bool
	compositeRef *compositeReference
}

// publishedBucket is what a published bucket wrote, so a replaying tick republishes the scored
// value rather than re-fetching.
type publishedBucket struct {
	value    string
	coverage cachekeys.WindowCoverage
	score    confidence.Score
	confOK   bool
	edge     aggregate.Quote
	leg      legRef
}

// DefaultInterval is the built-in tick cadence; 30s leaves headroom for missed ticks inside the 60s Redis price-key TTL.
const DefaultInterval = 30 * time.Second

// DefaultTickTimeoutIntervals sizes the default [Config.TickTimeout]: 4 x 30s = 120s, the `stellarindex_api_price_stale` threshold, so cutting a tick then costs nothing new.
const DefaultTickTimeoutIntervals = 4

// DefaultMaxTradesPerWindow bounds one refresh's Timescale scan; a liquid pair can exceed it in 1h/24h, TradesInRange then returns the NEWEST 10,000 and AggregatorWindowTruncatedTotal fires.
const DefaultMaxTradesPerWindow = 10_000

// DefaultMaxHops is the route-length cap when [Config.MaxHops] is 0: three legs reach every crypto-USD-fiat cross plus one pivot; [maxRouterHops] clamps to [2,4].
const DefaultMaxHops = 3

// maxRouterHops is the hard cap on [Config.MaxHops]: four legs is the obscure x obscure worst
// case (aggregate.FindRoutes); beyond it search cost grows without adding needed reachability.
const maxRouterHops = 4

// Orchestrator holds the wired dependencies and runs the tick loop.
type Orchestrator struct {
	store  Store
	cache  Cache
	cfg    Config
	logger *slog.Logger

	// prevVWAPs is the last published VWAP per (pair, window), the anomaly comparator; it doesn't advance during a freeze.
	// WARNING: LOCK-FREE, safe ONLY because one Tick runs at a time with a sequential per-pair loop. The sibling per-Tick maps (frozenThisTick, tickEdgeQuotes, lastComposites, freezeStates) share this invariant; parallelising REQUIRES guarding them with o.mu.
	prevVWAPs map[string]*big.Rat

	// prevVWAPAt is when each prevVWAPs entry was set; an unstamped entry is never aged.
	prevVWAPAt map[string]time.Time

	// prevVWAPBucketEnd is the bucket end each entry was published for: the observed-at stamp a
	// re-seeded held value must carry.
	prevVWAPBucketEnd map[string]time.Time
	// prevVWAPCoverage lets a freeze reseed restore the held value with its own coverage.
	prevVWAPCoverage map[string]cachekeys.WindowCoverage

	// frozenPrevVWAPs is the shadow comparator for pairs whose bucket the freeze REFUSED. Against the pinned prevVWAPs, z measures drift since the freeze, so auto-unfreeze is reachable only if the market returns to the freeze-time price.
	// The shadow advances with each refused bucket's VWAP, scoring "is the market calm NOW"; cleared on publish/release.
	frozenPrevVWAPs map[string]*big.Rat

	// scoredMinutes is the newest closed minute each (pair, window) decision scored, where the next
	// scoring starts (see marginalBuckets). Same single-Tick invariant as prevVWAPs.
	scoredMinutes map[string]minuteVWAP

	// lastWriteAt: last VWAP write per FULL pair (one quote can't vouch for another), set only via recordPairWrite and read by emitStalenessGauges
	// for `stellarindex_api_price_stale`. Bounded by len(cfg.Pairs) + len(cfg.Triangulations); the gauge iterates
	// cfg.Pairs only, so cardinality is unchanged. Same single-Tick invariant as prevVWAPs.
	lastWriteAt map[string]time.Time

	// lastDivergenceRefreshAt is the time of the last successful refreshDivergenceAll; touched only in [Tick] (single-runner, no lock). Zero = never, so the first tick runs the pass.
	lastDivergenceRefreshAt time.Time

	// frozenThisTick holds the `<pair>:<window>` keys this tick refused to publish, so triangulation doesn't re-publish a frozen leg's last-known-good as fresh ([Orchestrator.legPriceFromCache]). Rebuilt each [Tick].
	// WARNING: lock-free only because Tick is sequential; parallelising without o.mu could launder a frozen leg. See prevVWAPs.
	frozenThisTick map[string]struct{}

	// tickNow is the CURRENT tick's injected `now`, read once at the top of [Tick], so the composite writer, the direct writer and emitStalenessGauges judge one instant. Zero outside a Tick; same invariant as frozenThisTick.
	tickNow time.Time

	// decidedBuckets holds the last decided closed bucket per `pair:window`, so a later tick in the
	// same bucket replays it instead of scoring twice. Lock-free as prevVWAPs.
	decidedBuckets map[string]decidedBucket

	// heldDirect holds, per `pair:window`, a triangulation target's direct value until the triangulation pass decides whether it or the composite is served, so the shared served key has one writer per tick. Rebuilt every Tick.
	heldDirect map[string]heldDirect

	// streamedBuckets is the closed bucket last streamed per `pair:window`, so each bucket streams
	// once whichever writer served it. Lock-free as decidedBuckets.
	streamedBuckets map[string]time.Time

	// tickEdgeQuotes collects, per window, this tick's published pair VWAPs as router edges; frozen, dropped and empty windows add none, which is how min_usd_volume keeps a dust pair from setting a confident cross.
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	tickEdgeQuotes map[time.Duration][]aggregate.Quote

	// lastComposites holds the latest composite per (target pair, window) so the NEXT tick's confidence step compares a direct VWAP against an independent opinion (see triangulate_corroborate.go).
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	lastComposites map[string]compositeSample

	// tickLegRefs holds, per window, this tick's confidently published VWAP and venue count per pair, the legs the composite-reference evaluator multiplies; refreshOrder puts legs first.
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	tickLegRefs map[time.Duration]map[string]legRef

	// tickCompositeRefs holds this tick's composite-reference reading per `<pair>:<window>`; written BEFORE the confidence and freeze steps so both see the SAME sample, and read by triangulation for composite_meta.
	// Rebuilt every Tick; same lock-free invariant as prevVWAPs.
	tickCompositeRefs map[string]compositeReference

	// lastCompositeUnavailable is the last logged unavailable cause per `<pair>:<window>`, so the cause is logged at Info when it changes
	// rather than every tick. Persists across ticks; same lock-free invariant as prevVWAPs.
	lastCompositeUnavailable map[string]string

	// refreshOrder is cfg.Pairs with composite-reference legs ahead of their targets
	// (composite_reference.go); identical to cfg.Pairs when that is off. Computed once in New.
	refreshOrder []canonical.Pair

	// freezeStates holds the ADR-0019 lifecycle per `<pair>:<window>` (hold, extension ladder, escalation, unfreeze streak). A present-but-inactive entry marks a key evaluated, so the Redis hydrate doesn't rerun each tick.
	// In-memory is the working copy; the first evaluation after a restart re-hydrates from the marker so a deploy doesn't restart the escalation clock. Lock-free as prevVWAPs.
	freezeStates map[string]freeze.State

	// bootstrapReleased is each pair's last bootstrap-cap gate state (true once its score cleared the cap), the prior state for the hysteresis band ([confidence.BootstrapReengageDensityDays]).
	// In-memory only, so a restart falls back to the stricter upper gate; same invariant as prevVWAPs.
	bootstrapReleased map[string]bool

	// windowedFreeze and overrideReader are [Config.FreezeWriter] when it implements
	// [WindowedFreezeMarker] / [FreezeOverrideReader], else nil; resolved once in [New].
	windowedFreeze WindowedFreezeMarker
	overrideReader FreezeOverrideReader

	// clock is injectable so the freeze ladder, measured in tens of minutes, is testable without
	// sleeping. Defaults to time.Now.
	clock func() time.Time

	// Stats exposed for metrics / test assertions. Zero-copy.
	mu             sync.Mutex
	lastTickAt     time.Time
	ticksTotal     int64
	vwapWrites     int64
	emptyWindows   int64
	errors         int64
	freezesEngaged int64
}

// New constructs an Orchestrator with defaults applied.
func New(store Store, cache Cache, cfg Config) *Orchestrator {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = DefaultTickTimeoutIntervals * cfg.Interval
	}
	cfg.Windows = effectiveWindows(cfg.Windows)
	if cfg.MaxTradesPerWindow <= 0 {
		cfg.MaxTradesPerWindow = DefaultMaxTradesPerWindow
	}
	// Hop budget: 0 means default; anything outside [2,4] is clamped.
	if cfg.MaxHops == 0 {
		cfg.MaxHops = DefaultMaxHops
	}
	if cfg.MaxHops < 2 {
		cfg.MaxHops = 2
	}
	if cfg.MaxHops > maxRouterHops {
		cfg.MaxHops = maxRouterHops
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	o := &Orchestrator{
		store:             store,
		cache:             cache,
		cfg:               cfg,
		logger:            logger,
		prevVWAPs:         make(map[string]*big.Rat, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		prevVWAPAt:        make(map[string]time.Time, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		prevVWAPBucketEnd: make(map[string]time.Time, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		prevVWAPCoverage:  make(map[string]cachekeys.WindowCoverage, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		frozenPrevVWAPs:   make(map[string]*big.Rat),
		lastWriteAt:       make(map[string]time.Time, len(cfg.Pairs)),
		lastComposites:    make(map[string]compositeSample, len(cfg.Triangulations)*max(len(cfg.Windows), 1)),
		freezeStates:      make(map[string]freeze.State, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		bootstrapReleased: make(map[string]bool, len(cfg.Pairs)),
		decidedBuckets:    make(map[string]decidedBucket, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		refreshOrder:      refreshOrder(cfg),
		clock:             time.Now,
	}
	// A window-aware writer lets each window rehydrate ITS OWN freeze on a cold key, not a sibling's.
	if windowed, ok := cfg.FreezeWriter.(WindowedFreezeMarker); ok {
		o.windowedFreeze = windowed
	}
	if reader, ok := cfg.FreezeWriter.(FreezeOverrideReader); ok {
		o.overrideReader = reader
	}
	return o
}

// Run blocks until ctx is cancelled, invoking [Tick] every [Config.Interval]; the first tick fires immediately so Redis is warm before the API's first query.
func (o *Orchestrator) Run(ctx context.Context) error {
	if len(o.cfg.Pairs) == 0 {
		o.logger.Warn("orchestrator: no pairs configured — running as no-op")
	}

	// Kick off an immediate first tick.
	if err := o.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		o.logger.Warn("initial tick failed", "err", err)
	}

	t := time.NewTicker(o.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := o.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				o.logger.Warn("tick failed", "err", err)
			}
		}
	}
}

// Tick runs one aggregation cycle over every (pair, window); exported so tests can drive deterministic cycles.
func (o *Orchestrator) Tick(ctx context.Context) error {
	now := o.clock().UTC()
	o.mu.Lock()
	o.lastTickAt = now
	o.ticksTotal++
	o.mu.Unlock()

	// This tick's clock for every pair-level write stamp — see
	// [Orchestrator.tickNow].
	o.tickNow = now

	// Fresh per-tick freeze set — see [Orchestrator.frozenThisTick].
	o.frozenThisTick = make(map[string]struct{})

	// Fresh per-tick router edge inputs — see [Orchestrator.tickEdgeQuotes].
	// The per-pair loop below fills it; triangulateAll reads it.
	o.tickEdgeQuotes = make(map[time.Duration][]aggregate.Quote, len(o.cfg.Windows))

	// Fresh per-tick composite-reference inputs/outputs — see
	// [Orchestrator.tickLegRefs] / [Orchestrator.tickCompositeRefs].
	o.tickLegRefs = make(map[time.Duration]map[string]legRef, len(o.cfg.Windows))
	o.tickCompositeRefs = make(map[string]compositeReference)
	o.heldDirect = make(map[string]heldDirect)

	// Every store and cache call below runs on tickCtx so none outlives the wedge guard
	// ([Config.TickTimeout]).
	tickCtx, cancel := context.WithTimeout(ctx, o.cfg.TickTimeout)
	defer cancel()

	tickHadError, loopErr := o.refreshAllPairs(tickCtx, now)
	if loopErr != nil && ctx.Err() != nil {
		// The CALLER's context ended (shutdown): stop here; only the tick's own deadline finishes
		// the accounting.
		return loopErr
	}

	// Both passes are skipped once the tick's deadline has fired, whether
	// it cut the loop short (loopErr) or landed inside its last refresh.
	if tickCtx.Err() == nil {
		// Triangulation runs AFTER the per-pair refresh so legs read fresh VWAPs; failures are logged and counted, never abort the tick.
		// Skipped on a cut tick: a composite must not be routed over a partial edge set.
		o.triangulateAll(tickCtx)

		// Divergence refresh runs AFTER the per-pair VWAPs are cached so RefreshPair has a fresh price; best-effort per pair, never aborts the tick.
		o.refreshDivergenceAll(tickCtx, now)
	}
	o.flushHeldDirect(tickCtx)

	// The tick's own deadline fired (caller's context still live): the tick is an error but still does end-of-tick accounting, so staleness gauges keep climbing for pairs the cut tick didn't reach.
	timedOut := ctx.Err() == nil && tickCtx.Err() != nil
	if timedOut {
		o.mu.Lock()
		o.errors++
		o.mu.Unlock()
	}

	outcome := "ok"
	if tickHadError || timedOut {
		outcome = "error"
	}
	obs.AggregatorTicksTotal.WithLabelValues(outcome).Inc()

	// Emit per-asset staleness at end-of-Tick whether or not any window wrote, so pairs with no fresh trades climb past the `stellarindex_api_price_stale` threshold.
	o.emitStalenessGauges(now)

	// A gauge of freezes HELD now, unlike the engaged counter, tells "one pair frozen for an hour"
	// from "sixty pairs frozen for one tick" (obs.AnomalyFreezeActive).
	obs.AnomalyFreezeActive.Set(float64(o.activeFreezeCount()))

	if timedOut {
		return fmt.Errorf("orchestrator: tick cut by its %s wedge guard: %w",
			o.cfg.TickTimeout, context.DeadlineExceeded)
	}
	return nil
}

// refreshAllPairs runs the per-(pair, window) refresh in refreshOrder. hadError reports a skipped failed refresh; err is non-nil only when ctx ended early.
func (o *Orchestrator) refreshAllPairs(ctx context.Context, now time.Time) (hadError bool, err error) {
	for _, pair := range o.refreshOrder {
		for _, window := range o.cfg.Windows {
			if err := ctx.Err(); err != nil {
				return hadError, err
			}
			if err := o.refreshPairWindow(ctx, pair, window, now); err != nil {
				hadError = true
				o.mu.Lock()
				o.errors++
				o.mu.Unlock()
				o.logger.Warn("refresh failed",
					"pair", pair.String(),
					"window", window,
					"err", err)
				continue
			}
		}
	}
	return hadError, nil
}

// activeFreezeCount counts keys holding a freeze; released keys keep an inactive entry (so
// hydration stays once), so it walks the map rather than taking len.
func (o *Orchestrator) activeFreezeCount() int {
	n := 0
	for _, st := range o.freezeStates {
		if st.Active() {
			n++
		}
	}
	return n
}

// The two canonical forms of XLM, as they appear in the `asset` label
// of `stellarindex_price_staleness_seconds`.
const (
	stalenessXLMNative = "native"
	stalenessXLMTicker = "crypto:XLM"
)

// recordPairWrite stamps a successful VWAP publish for the FULL pair (a base-only key let XLM/USD keep a dead XLM/GBP reading fresh); the
// only writer of lastWriteAt.
// A zero `at` (outside a Tick) records nothing: an unstamped gauge keeps climbing, the fail-safe direction.
func (o *Orchestrator) recordPairWrite(pair canonical.Pair, at time.Time) {
	if at.IsZero() {
		return
	}
	o.lastWriteAt[pair.String()] = at
}

// pairLastWrite is the freshest write that can answer a lookup for `pair`: its own stamp, except
// XLM, whose two interchangeable base forms count for the SAME quote only.
func (o *Orchestrator) pairLastWrite(pair canonical.Pair) time.Time {
	last := o.lastWriteAt[pair.String()]
	var sibling string
	switch pair.Base.String() {
	case stalenessXLMNative:
		sibling = stalenessXLMTicker
	case stalenessXLMTicker:
		sibling = stalenessXLMNative
	default:
		return last
	}
	if other, ok := o.lastWriteAt[sibling+"/"+pair.Quote.String()]; ok && other.After(last) {
		return other
	}
	return last
}

// emitStalenessGauges sets `stellarindex_price_staleness_seconds` per (base, quote) pair to `now - lastWriteAt[pair]` (never-written falls back to `now`).
// Labels must match the asset_id customers query (`native` in /v1/price vs `crypto:XLM` here), so a known alias pair is emitted under both forms.
func (o *Orchestrator) emitStalenessGauges(now time.Time) {
	// First sighting counts as "just observed" so the metric exists without paging. Seeded for
	// every pair BEFORE any is read, so the dual-form merge is order-independent.
	for _, pair := range o.cfg.Pairs {
		if _, ok := o.lastWriteAt[pair.String()]; !ok {
			o.recordPairWrite(pair, now)
		}
	}

	// XLM has two forms: customers query `native`, oracles publish `crypto:XLM`. A quote's freshness is the
	// fresher of the two (pairLastWrite), which is symmetric, so both labels get the same value whatever the
	// cfg.Pairs order.
	for _, pair := range o.cfg.Pairs {
		stale := now.Sub(o.pairLastWrite(pair)).Seconds()
		asset, quote := pair.Base.String(), pair.Quote.String()
		obs.PriceStalenessSeconds.WithLabelValues(asset, quote).Set(stale)
		switch asset {
		case stalenessXLMNative:
			obs.PriceStalenessSeconds.WithLabelValues(stalenessXLMTicker, quote).Set(stale)
		case stalenessXLMTicker:
			obs.PriceStalenessSeconds.WithLabelValues(stalenessXLMNative, quote).Set(stale)
		}
	}
}

// refreshPairWindow computes VWAP for one (pair, window) over the last CLOSED bucket (ADR-0015) and writes it to Redis. The first tick to see a bucket closed decides it (score, freeze step, publish or hold); later ticks in the same bucket replay that decision.
// ErrNoTrades is a normal outcome, not propagated.
func (o *Orchestrator) refreshPairWindow(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	now time.Time,
) error {
	bucketEnd := now.Truncate(closedBucket)
	stateKey := pair.String() + ":" + window.String()
	if prior, ok := o.decidedBuckets[stateKey]; ok && prior.end.Equal(bucketEnd) {
		return o.replayDecidedBucket(ctx, pair, window, prior, now)
	}
	o.ageComparator(stateKey, now)
	pub, err := o.decideBucket(ctx, pair, window, bucketEnd, now)
	if err != nil {
		// Undecided: the next tick retries this bucket from scratch.
		return err
	}
	_, frozen := o.frozenThisTick[frozenTickKey(pair, window)]
	if frozen && pub == nil {
		o.streamFrozenOnce(ctx, pair, window, bucketEnd, o.freezeStates[stateKey].FiredAt)
	}
	d := decidedBucket{end: bucketEnd, published: pub, frozen: frozen}
	if ref, ok := o.currentCompositeReference(pair, window); ok {
		d.compositeRef = &ref
	}
	if o.decidedBuckets == nil {
		o.decidedBuckets = make(map[string]decidedBucket)
	}
	o.decidedBuckets[stateKey] = d
	return nil
}

// noteEmptyWindow counts one window that produced no price.
func (o *Orchestrator) noteEmptyWindow() {
	o.mu.Lock()
	o.emptyWindows++
	o.mu.Unlock()
	obs.AggregatorEmptyWindowsTotal.Inc()
}

// decideBucket is the first evaluation of one closed bucket: fetch, filter, VWAP, Phase 1 and 2 freeze steps and, if cleared, the publish. Returns the published bucket, or nil when held (frozen, empty, below the volume floor).
func (o *Orchestrator) decideBucket(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	bucketEnd time.Time,
	now time.Time,
) (*publishedBucket, error) {
	from := bucketEnd.Add(-window)
	trades, tradeUSD, proxied, weights, coverage, err := o.fetchForTarget(ctx, pair, from, bucketEnd)
	if err != nil {
		return nil, fmt.Errorf("fetch %s %v: %w", pair.String(), window, err)
	}
	trades = dropExcludedSources(pair, trades, o.cfg.ExcludedSources)
	preFilter := tradeCount(trades, weights)
	if !o.cfg.DisableClassFilter {
		trades = filterForVWAP(trades)
		if dropped := preFilter - tradeCount(trades, weights); dropped > 0 {
			// `pair` is a configured target (bounded cardinality); per-pair drops let outlier_storm
			// attribute a single-issuer SDEX token farm.
			obs.AggregatorDroppedTradesTotal.WithLabelValues("class", pair.String()).Add(float64(dropped))
		}
	}
	trades = dropUnpriceable(pair, trades)
	// The outlier_storm alert reads per-venue DISAGREEMENT from this view, not the trim re-count.
	recordWindowStage(pair, window, "fetched", preFilter)
	recordWindowStageVolume(pair, window, "class", trades, weights)
	o.recordVenueVWAPs(pair, window, trades)
	if o.cfg.OutlierSigmaThreshold > 0 {
		preOutlier := tradeCount(trades, weights)
		// Time-local trimming: a print is dropped only when it disagrees with the whole window AND its neighbourhood, so a regime shift survives (aggregate.FilterOutliersLocal).
		// A trim that would leave less base volume than it removes withholds the window, compared at computeNormalizedVWAP's common scale.
		trades = aggregate.FilterOutliersLocal(trades, aggregate.LocalOutlierOptions{
			Sigma:               o.cfg.OutlierSigmaThreshold,
			AmountScaleDecimals: amountScaleDecimalsFor,
		})
		if dropped := preOutlier - tradeCount(trades, weights); dropped > 0 {
			obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String()).Add(float64(dropped))
		}
	}
	recordWindowStageVolume(pair, window, "outlier", trades, weights)
	if len(trades) == 0 {
		o.noteEmptyWindow()
		return o.unpricedBucket(ctx, pair, window, now)
	}

	// Sum USD across the SURVIVOR slice, not the pre-filter total from fetchForTarget, or a window gutted by class/outlier filters could publish above MinUSDVolume on volume that never reached the VWAP.
	survivorUSD := survivorUSDVolume(trades, tradeUSD)
	if o.dropForMinUSDVolume(ctx, pair, trades, survivorUSD, now) {
		return o.unpricedBucket(ctx, pair, window, now)
	}

	vwap, err := o.computeNormalizedVWAP(trades, pair)
	if err != nil {
		if errors.Is(err, aggregate.ErrNoTrades) {
			o.noteEmptyWindow()
			return o.unpricedBucket(ctx, pair, window, now)
		}
		return nil, fmt.Errorf("vwap %s %v: %w", pair.String(), window, err)
	}

	o.flushContributions(ctx, pair, window, trades, tradeUSD, weights)

	// Phase 1 anomaly evaluation BEFORE the cache write (class deviation + source count). On freeze the previous value stays in cache and a marker is emitted so flags.frozen=true.
	// evaluateAndMaybeFreeze stands down while THIS window holds an active freeze, leaving the Phase 2 lifecycle the sole release authority.
	stateKey := pair.String() + ":" + window.String()
	mb := o.scoreMinutes(trades, pair, stateKey, vwap)
	if action, ok := o.evaluateAndMaybeFreeze(ctx, pair, window, mb, trades, stateKey, now); !ok {
		_ = action
		// Frozen: the LKG key's TTL is already refreshed; skip the write so the prior value serves.
		return nil, nil
	}

	// Phase 2 (ADR-0019): compute confidence, then advance the freeze lifecycle, both BEFORE the cache write so a
	// freeze leaves the prior value in cache. The step runs unconditionally, not only when the 3-signal AND fires:
	// a held pair stays frozen through quiet buckets and is released only by the auto-unfreeze condition (two
	// healthy buckets after the initial hold), and an unscored bucket must not release a live freeze.
	prevForConfidence := o.prevVWAPs[stateKey]
	if shadow, frozen := o.frozenPrevVWAPs[stateKey]; frozen {
		// Mid-freeze, score against the PREVIOUS refused bucket's VWAP, not the pinned baseline
		// (see frozenPrevVWAPs).
		prevForConfidence = shadow
	}
	// Composite-reference corroboration for an allow-listed single-venue target: build the chain composite on the CURRENT bucket and read it against this fresh VWAP.
	// Evaluated BEFORE the confidence step so confidence and the freeze verdict see the SAME sample; never for multi-venue buckets.
	var compositeRef compositeReference
	if o.compositeReferenceEligible(pair, trades) {
		compositeRef = o.evaluateCompositeReference(ctx, pair, window, now, vwap)
	} else {
		// Retire the previous tick's verdict rather than leave it standing (clearCompositeReference).
		o.clearCompositeReference(pair, window)
	}
	conf, confOK := o.computeConfidence(ctx, pair, window, vwap, mb.returns(prevForConfidence), trades, now)
	// source_count counts VENUES only; router routes are never a second venue (ADR-0019 amendment
	// §2). The composite reference changes only the VERDICT, never the count.
	if o.stepPhase2Freeze(ctx, pair, window, stateKey, now,
		conf, confOK, distinctSourceCount(trades), vwap, compositeRef) {
		// Refused: advance the shadow comparator so the NEXT frozen bucket scores a per-tick return
		// (and a post-restart frozen pair is scorable from its second bucket).
		o.frozenPrevVWAPs[stateKey] = vwap
		return nil, nil
	}
	// Published (or released this tick): the shadow's job is done.
	delete(o.frozenPrevVWAPs, stateKey)

	// Money stays big.Rat until this decimal string; float encoding is prohibited (ADR-0003).
	pub := &publishedBucket{
		value:    formatRatFixed(vwap, 12),
		coverage: coverage,
		score:    conf.Score,
		confOK:   confOK,
		// Only a published bucket becomes an edge or leg, which keeps a dust pair out of the graph.
		edge: newEdgeQuote(pair, vwap, conf, confOK, trades),
		leg:  o.newLegRef(pair, vwap, trades, proxied),
	}
	if err := o.publishDirect(ctx, pair, window, pub, bucketEnd, now); err != nil {
		return nil, err
	}

	// Update the prev-VWAP comparator ONLY on successful publish. Frozen buckets advance frozenPrevVWAPs instead; keeping the pinned value as the sole comparator was the auto-unfreeze ratchet (see frozenPrevVWAPs).
	o.setComparator(stateKey, vwap, now, bucketEnd)
	o.prevVWAPCoverage[stateKey] = coverage
	return pub, nil
}

// setComparator records a published bucket as the window's prev-VWAP
// comparator with the times its aging and re-seeding read.
func (o *Orchestrator) setComparator(stateKey string, vwap *big.Rat, now, bucketEnd time.Time) {
	o.prevVWAPs[stateKey] = vwap
	o.prevVWAPAt[stateKey] = now
	o.prevVWAPBucketEnd[stateKey] = bucketEnd
}

// ageComparator drops a prevVWAPs entry the window has not refreshed within [Orchestrator.vwapMaxAge]: with no cached last-known-good beside it a freeze would refuse the bucket with nothing to serve. A live freeze keeps its comparator.
func (o *Orchestrator) ageComparator(stateKey string, now time.Time) {
	at, stamped := o.prevVWAPAt[stateKey]
	if !stamped || o.freezeStates[stateKey].Active() || now.Sub(at) <= o.vwapMaxAge() {
		return
	}
	delete(o.prevVWAPs, stateKey)
	delete(o.prevVWAPAt, stateKey)
	delete(o.prevVWAPBucketEnd, stateKey)
	delete(o.prevVWAPCoverage, stateKey)
}

// heldDirect is a triangulation target's direct bucket awaiting the triangulation decision.
type heldDirect struct {
	pair      canonical.Pair
	window    time.Duration
	pub       *publishedBucket
	bucketEnd time.Time
	now       time.Time
}

// publishDirect records a decided bucket's router edge and reference leg and serves its direct VWAP; shared by the deciding and replaying ticks so the replay republishes exactly what was scored.
// A triangulation target's value is held for [Orchestrator.settleDirect], else a replay would flip the key back to the direct print under a composite every tick.
func (o *Orchestrator) publishDirect(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	pub *publishedBucket,
	bucketEnd, now time.Time,
) error {
	if o.isTriangulationTarget(pair) {
		if o.heldDirect == nil {
			o.heldDirect = make(map[string]heldDirect)
		}
		o.heldDirect[pair.String()+":"+window.String()] = heldDirect{
			pair: pair, window: window, pub: pub, bucketEnd: bucketEnd, now: now,
		}
	} else if err := o.serveDirect(ctx, pair, window, pub, bucketEnd, now); err != nil {
		return err
	}
	o.appendTickEdgeQuote(window, pub.edge)
	o.setTickLegRef(pair, window, pub.leg)
	return nil
}

// vwapMaxAge returns the silence grace: 10 missed ticks at the OWN configured cadence, floored at [cachekeys.VWAPMaxAge].
// Deriving it from cfg.Interval stops a raised interval or a slow tick cycle flapping long windows between 200 and 404.
func (o *Orchestrator) vwapMaxAge() time.Duration {
	if derived := 10 * o.cfg.Interval; derived > cachekeys.VWAPMaxAge {
		return derived
	}
	return cachekeys.VWAPMaxAge
}

// vwapTTL is [cachekeys.VWAPTTL] derived from this orchestrator's own tick cadence.
func (o *Orchestrator) vwapTTL(window time.Duration) time.Duration {
	return cachekeys.VWAPTTLWithMaxAge(window, o.vwapMaxAge())
}

// serveDirect writes a direct VWAP to the served key stamped with its closed bucket and clears any "triangulated" provenance in one MULTI/EXEC: the API serves the Redis fallback only under that marker, so a direct (possibly thin) value must never sit beneath it. Then streams the bucket once.
func (o *Orchestrator) serveDirect(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	pub *publishedBucket,
	bucketEnd, now time.Time,
) error {
	key := cachekeys.VWAP(pair.Base, pair.Quote, window)
	provKey := cachekeys.VWAPProvenance(pair.Base, pair.Quote, window)
	atKey := cachekeys.VWAPObservedAt(pair.Base, pair.Quote, window)
	covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window)
	ttl := o.vwapTTL(window)
	if _, err := o.cache.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, provKey.String())
		p.Set(ctx, atKey.String(), cachekeys.FormatVWAPObservedAt(bucketEnd), ttl)
		p.Set(ctx, covKey.String(), cachekeys.FormatVWAPCoverage(pub.coverage), ttl)
		p.Set(ctx, key.String(), pub.value, ttl)
		return nil
	}); err != nil {
		// Alerted on rate(...vwap_cache_write_errors_total[5m]) > 0: a Redis refusing every write
		// (BGSAVE MISCONF) is otherwise invisible until /v1/price 404s.
		obs.AggregatorVWAPCacheWriteErrorsTotal.Inc()
		return fmt.Errorf("redis publish %s: %w", key, err)
	}

	// Only on successful publish, so frozen buckets carry no stale score. Best-effort enrichment.
	if pub.confOK {
		o.cacheConfidence(ctx, pair, window, pub.score)
	}

	o.mu.Lock()
	o.vwapWrites++
	o.mu.Unlock()
	obs.AggregatorVWAPWritesTotal.Inc()

	// Pair-level write clock for `stellarindex_price_staleness_seconds`.
	o.recordPairWrite(pair, now)
	coverage := pub.coverage
	o.streamBucketOnce(ctx, pair, window, pub.value, bucketEnd, &coverage)
	return nil
}

// isTriangulationTarget reports whether pair's served key has a second, composite writer.
func (o *Orchestrator) isTriangulationTarget(pair canonical.Pair) bool {
	for i := range o.cfg.Triangulations {
		t := o.cfg.Triangulations[i].Target
		if t.Base.Equal(pair.Base) && t.Quote.Equal(pair.Quote) {
			return true
		}
	}
	return false
}

// settleDirect resolves a target's held direct value once triangulation has decided: published when the composite was not served, otherwise dropped keeping only the direct market's confidence score. Idempotent.
func (o *Orchestrator) settleDirect(ctx context.Context, pair canonical.Pair, window time.Duration, compositeServed bool) {
	k := pair.String() + ":" + window.String()
	h, ok := o.heldDirect[k]
	if !ok {
		return
	}
	delete(o.heldDirect, k)
	if compositeServed {
		if h.pub.confOK {
			o.cacheConfidence(ctx, pair, window, h.pub.score)
		}
		return
	}
	if err := o.serveDirect(ctx, h.pair, h.window, h.pub, h.bucketEnd, h.now); err != nil {
		o.mu.Lock()
		o.errors++
		o.mu.Unlock()
		o.logger.Warn("aggregator: direct publish of a triangulation target failed",
			"pair", pair.String(), "window", window.String(), "err", err)
	}
}

// flushHeldDirect serves direct values the triangulation pass left undecided, or drops them when
// the tick's context has ended (a write cannot land; the next tick replays the bucket).
func (o *Orchestrator) flushHeldDirect(ctx context.Context) {
	for _, h := range o.heldDirect {
		if ctx.Err() != nil {
			delete(o.heldDirect, h.pair.String()+":"+h.window.String())
			continue
		}
		o.settleDirect(ctx, h.pair, h.window, false)
	}
}

// replayDecidedBucket re-applies a decided bucket on a later tick inside it: the value is republished and per-tick state restored, but nothing is re-scored; freeze lifecycle, release streak and comparators advance once per closed bucket.
func (o *Orchestrator) replayDecidedBucket(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	d decidedBucket,
	now time.Time,
) error {
	if d.frozen {
		o.markFrozenThisTick(pair, window)
	}
	if d.compositeRef != nil {
		if o.tickCompositeRefs == nil {
			o.tickCompositeRefs = make(map[string]compositeReference)
		}
		o.tickCompositeRefs[pair.String()+":"+window.String()] = *d.compositeRef
	}
	if d.published == nil {
		return nil
	}
	return o.publishDirect(ctx, pair, window, d.published, d.end, now)
}

// computeNormalizedVWAP computes VWAP plus the non-standard-decimals normalization in one step, since aggregate.VWAP is right only when
// both legs share a scale
// (aggregate.AdjustPrice, docs/operations/runbooks/dex.md); a nil DecimalsLookup is an exact no-op, and one call site keeps every
// consumer's number identical.
func (o *Orchestrator) computeNormalizedVWAP(trades []canonical.Trade, pair canonical.Pair) (*big.Rat, error) {
	// Scale-normalize BEFORE the weighted sum: a window can mix on-chain (7dp) and CEX (8dp) trades, and the raw
	// Σquote/Σbase over-weights the finer scale ~10× per decimal (aggregate.NormalizeAmountScale, as price_tip.go
	// and ohlc_fiat_combine.go do). A single-scale window is returned byte-identical.
	vwap, err := aggregate.VWAP(aggregate.NormalizeAmountScale(trades, amountScaleDecimalsFor))
	if err != nil {
		return nil, err
	}
	return aggregate.AdjustPrice(vwap,
		aggregate.ResolveDecimals(o.cfg.DecimalsLookup, pair.Base),
		aggregate.ResolveDecimals(o.cfg.DecimalsLookup, pair.Quote)), nil
}

// amountScaleDecimalsFor resolves a source's smallest-unit scale from the external registry, mirroring internal/api/v1's helper; taken as a parameter by aggregate because internal/sources/external imports aggregate (cycle).
func amountScaleDecimalsFor(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// markFrozenThisTick records a freeze refusal so [Orchestrator.legPriceFromCache] won't feed the last-known-good into a chain; called from
// the freeze paths themselves
// so a new freeze path can't reopen the laundering route. Tolerates a nil map for tests calling refreshPairWindow directly.
func (o *Orchestrator) markFrozenThisTick(pair canonical.Pair, window time.Duration) {
	if o.frozenThisTick == nil {
		o.frozenThisTick = make(map[string]struct{})
	}
	o.frozenThisTick[frozenTickKey(pair, window)] = struct{}{}
}

// frozenLeg reports whether (pair, window) is frozen for triangulation: refused this tick, OR inside a freeze this process holds. The second arm matters because a bucket whose fetch errors never reaches the freeze step, yet its LKG stays in Redis and would launder into a derived price.
// Bounded by hold + marker grace, how long the LKG lives.
func (o *Orchestrator) frozenLeg(pair canonical.Pair, window time.Duration) bool {
	if _, ok := o.frozenThisTick[frozenTickKey(pair, window)]; ok {
		return true
	}
	// Same `pair:window` shape refreshPairWindow keys o.freezeStates by.
	st := o.freezeStates[pair.String()+":"+window.String()]
	grace := o.cfg.Phase2Thresholds.Lifecycle.WithDefaults().MarkerGrace
	return freeze.LadderStillLive(st, grace, o.clock())
}

// frozenTickKey is the [Orchestrator.frozenThisTick] key, deliberately a separate key space from
// refreshPairWindow's stateKey even though the shape matches.
func frozenTickKey(pair canonical.Pair, window time.Duration) string {
	return pair.String() + ":" + window.String()
}

// keepFrozenVWAPAlive extends the TTL of the last-known-good VWAP key and every qualifier beside it (observed-at, coverage, triangulated marker, composite meta, confidence) to the marker's, since a freeze skips the write and the LKG would expire mid-freeze; a stray qualifier would relabel a frozen composite as direct.
// The value isn't rewritten, so observed_at stays honest. ttl MUST be the marker's own; <= 0 falls back to FreezeTTL. Best-effort.
func (o *Orchestrator) keepFrozenVWAPAlive(ctx context.Context, pair canonical.Pair, window, ttl time.Duration) {
	if ttl <= 0 {
		ttl = cachekeys.FreezeTTL
	}
	key := cachekeys.VWAP(pair.Base, pair.Quote, window)
	atKey := cachekeys.VWAPObservedAt(pair.Base, pair.Quote, window)
	provKey := cachekeys.VWAPProvenance(pair.Base, pair.Quote, window)
	metaKey := cachekeys.VWAPCompositeMeta(pair.Base, pair.Quote, window)
	covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window)
	// The cached score describes the LKG, so it lives as long as the held value (ADR-0019); the
	// refused bucket's score is never written.
	confKey := cachekeys.Confidence(pair.Base, pair.Quote, window)
	if _, err := o.cache.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Expire(ctx, key.String(), ttl)
		p.Expire(ctx, atKey.String(), ttl)
		p.Expire(ctx, provKey.String(), ttl)
		p.Expire(ctx, metaKey.String(), ttl)
		p.Expire(ctx, covKey.String(), ttl)
		p.Expire(ctx, confKey.String(), ttl)
		return nil
	}); err != nil {
		o.logger.Debug("freeze: LKG VWAP TTL refresh failed",
			"pair", pair.String(), "window", window, "key", key, "err", err)
	}
}

// evaluateAndMaybeFreeze runs the anomaly check on a fresh VWAP and writes a freeze marker when told to. ok=false (Freeze): the caller
// skips the write so the previous value keeps serving; nil Anomaly returns Allow.
func (o *Orchestrator) evaluateAndMaybeFreeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	mb marginalBuckets,
	trades []canonical.Trade,
	stateKey string,
	now time.Time,
) (anomaly.Action, bool) {
	if o.cfg.Anomaly == nil {
		return anomaly.ActionAllow, true
	}

	// Once THIS window holds an active freeze, Phase 1 stands down and the ADR-0019 lifecycle is the SOLE release authority: Phase 1 measures against the fixed prevVWAPs, so each fire would short-circuit the Phase 2 step that earns the unfreeze streak and pin the LKG to escalation.
	// The lifecycle still keeps the bucket refused while Active.
	if o.freezeStates[stateKey].Active() {
		return anomaly.ActionAllow, true
	}

	prev := o.prevVWAPs[stateKey]
	step := mb.worstStep(prev)
	decision := o.cfg.Anomaly.Evaluate(anomaly.Observation{
		Pair:     pair,
		PrevVWAP: step.prev,
		CurrVWAP: step.curr,
		// Venues only, as in Phase 2: router corroboration is not a second source.
		SourceCount: distinctSourceCount(trades),
	})
	if !decision.IsFrozen() {
		if decision.IsWarn() {
			// Count the warn here, since the caller discards the Action on the non-freeze path. It is NOT folded into
			// flags.divergence_warning: that flag belongs to the cross-reference service and means something only with
			// divergence_checked, which an anomaly warn never sets.
			obs.AnomalyWarnTotal.WithLabelValues(string(decision.Class)).Inc()
			o.logger.Warn("anomaly warn threshold crossed (published, not frozen)",
				"pair", pair.String(),
				"window", window.String(),
				"class", string(decision.Class),
				"deviation_pct", decision.DeviationPct,
				"reason", decision.Reason)
		}
		return decision.Action, true
	}

	o.logger.Warn("anomaly freeze engaged",
		"pair", pair.String(),
		"window", window,
		"class", decision.Class,
		"deviation_pct", decision.DeviationPct,
		"reason", decision.Reason)

	// Phase 1 shares the ADR-0019 lifecycle with Phase 2: one owner per `freeze:<asset>:<quote>` key, or a Phase 1 fire would truncate a Phase 2 hold's TTL.
	// Scored=false and Corroborated=false: no streak and the shorter first hold. A persisting anomaly re-freezes next tick unless the operator cleared the marker.
	if !o.stepFreezeLifecycle(ctx, pair, window, stateKey,
		freeze.Signal{Now: now, Fires: true}, decision) {
		return decision.Action, true
	}
	return decision.Action, false
}

// distinctSourceCount returns how many distinct trade.Source values contributed; zero when empty.
func distinctSourceCount(trades []canonical.Trade) int {
	if len(trades) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, 8)
	for i := range trades {
		seen[trades[i].Source] = struct{}{}
	}
	return len(seen)
}

// fetchForTarget pulls one target pair's window trades: a single TradesInRange, or with the stablecoin proxy each aggregate.ExpandTargetPair backer fetched and rewritten onto the target (a failing backer is skipped).
// tradeUSD (by Trade.ID()) is valued BEFORE the rewrite blurs the quote scale and feeds [survivorUSDVolume]. proxied lists rewritten trade IDs (nil with proxy off); coverage is truncated if any read stayed capped.
// weights maps a minute-sum row's ID to the trades it stands for ([tradeCount]).
func (o *Orchestrator) fetchForTarget(
	ctx context.Context,
	target canonical.Pair,
	from, to time.Time,
) (trades []canonical.Trade, tradeUSD map[string]*big.Rat, proxied map[string]struct{}, weights map[string]int64, coverage cachekeys.WindowCoverage, err error) {
	if !o.cfg.EnableStablecoinFiatProxy {
		t, w, coveredFrom, err := o.fetchTradesDetectTruncation(ctx, target, target, from, to)
		if err != nil {
			return nil, nil, nil, nil, coverage, err
		}
		usd := usdVolumeForPairPerTrade(target, t, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets)
		return t, usd, nil, w, widenCoverage(coverage, coveredFrom), nil
	}

	sources, err := aggregate.ExpandTargetPairWithClassicPegs(target, o.cfg.USDPeggedClassicAssets)
	if err != nil {
		return nil, nil, nil, nil, coverage, fmt.Errorf("expand target %s: %w", target.String(), err)
	}

	var merged []canonical.Trade
	var fetchErrs []error
	tradeUSD = map[string]*big.Rat{}
	proxied = map[string]struct{}{}
	for _, src := range sources {
		batch, w, coveredFrom, ferr := o.fetchTradesDetectTruncation(ctx, target, src, from, to)
		if ferr != nil {
			o.logger.Warn("stablecoin-expansion fetch failed",
				"target", target.String(),
				"source_pair", src.String(),
				"err", ferr,
			)
			fetchErrs = append(fetchErrs, fmt.Errorf("%s: %w", src.String(), ferr))
			continue
		}
		coverage = widenCoverage(coverage, coveredFrom)
		for id, n := range w {
			if weights == nil {
				weights = make(map[string]int64, len(w))
			}
			weights[id] = n
		}
		// Per-trade USD value against the SOURCE pair's quote-decimal
		// convention — captured BEFORE the rewrite below blurs the
		// original 7-vs-8 decimal.
		for id, v := range usdVolumeForPairPerTrade(src, batch, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets) {
			tradeUSD[id] = v
		}
		if src.Equal(target) {
			merged = append(merged, batch...)
			continue
		}
		for i := range batch {
			batch[i].Pair = target
			proxied[batch[i].ID()] = struct{}{}
			merged = append(merged, batch[i])
		}
	}
	// One failing leg is tolerated; every leg failing means nothing was
	// read, and reporting that as an empty window hides a store outage.
	if len(fetchErrs) == len(sources) {
		return nil, nil, nil, nil, coverage, fmt.Errorf("all %d source pairs failed: %w", len(sources), errors.Join(fetchErrs...))
	}
	return merged, tradeUSD, proxied, weights, coverage, nil
}

// usdVolumeForPairPerTrade returns exact USD value per Trade.ID(), keyed before fetchForTarget rewrites Pair; nil when the quote isn't a
// USD surface (volume_usd NULL).
// Scale comes from [usdQuoteDecimals], shared with [dropForMinUSDVolume] so the two can't disagree.
func usdVolumeForPairPerTrade(pair canonical.Pair, batch []canonical.Trade, classicUSDPegs, sorobanUSDPegs []canonical.Asset) map[string]*big.Rat {
	if len(batch) == 0 {
		return nil
	}
	if _, ok := usdQuoteDecimals(pair.Quote, classicUSDPegs, sorobanUSDPegs); !ok {
		return nil
	}
	// One scale per DISTINCT decimals value, not per trade — a window
	// carries hundreds of trades across at most three decimal classes.
	scales := make(map[int]*big.Int, 3)
	perTrade := make(map[string]*big.Rat, len(batch))
	for i := range batch {
		amt := batch[i].QuoteAmount.BigInt()
		if amt == nil || amt.Sign() == 0 {
			continue
		}
		decimals, ok := usdQuoteDecimalsForTrade(pair.Quote, batch[i].Source, classicUSDPegs, sorobanUSDPegs)
		if !ok {
			continue
		}
		scale, hit := scales[decimals]
		if !hit {
			scale = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
			scales[decimals] = scale
		}
		perTrade[batch[i].ID()] = new(big.Rat).SetFrac(amt, scale)
	}
	return perTrade
}

// usdQuoteDecimalsForTrade resolves ONE trade's QuoteAmount scale: the [usdQuoteDecimals] tiers, but off-chain tiers read the source's declared scale (CEX 8, FX 6) as [approxUSDVolume] does; valuing 6dp at 1e8 understates 100x.
// On-chain tiers keep the protocol 7, since the registry default of 8 would understate an unregistered venue 10x.
func usdQuoteDecimalsForTrade(quote canonical.Asset, source string, classicUSDPegs, sorobanUSDPegs []canonical.Asset) (decimals int, ok bool) {
	switch {
	case quote.Type == canonical.AssetFiat && quote.Code == "USD",
		aggregate.IsFiatProxyFor(quote, "USD"):
		return external.Lookup(source).AmountScaleDecimals(), true
	default:
		return usdQuoteDecimals(quote, classicUSDPegs, sorobanUSDPegs)
	}
}

// usdQuoteDecimals gives a quote's decimal scale if it is a USD surface, without a live price: fiat:USD (8); abstract USD stablecoin ticker (8, what the proxy expansion fetches under); classic credit on classicUSDPegs (7); SAC on sorobanUSDPegs (7).
// ok=false covers non-USD fiat and stablecoins (need FX), unpegged on-chain quotes and the rest. Valuation and the MinUSDVolume floor both call this so they can't drift.
func usdQuoteDecimals(quote canonical.Asset, classicUSDPegs, sorobanUSDPegs []canonical.Asset) (decimals int, ok bool) {
	switch {
	case quote.Type == canonical.AssetFiat && quote.Code == "USD":
		return 8, true
	case aggregate.IsFiatProxyFor(quote, "USD"):
		return 8, true
	case quote.Type == canonical.AssetClassic && isUSDPeggedClassic(quote, classicUSDPegs):
		return 7, true
	case quote.Type == canonical.AssetSoroban && isUSDPeggedSoroban(quote, sorobanUSDPegs):
		return 7, true
	default:
		return 0, false
	}
}

// isUSDPeggedClassic reports whether `asset` is an operator-declared classic USD-pegged credit, by exact (code, issuer) equality.
func isUSDPeggedClassic(asset canonical.Asset, pegs []canonical.Asset) bool {
	for _, p := range pegs {
		if p.Type != canonical.AssetClassic {
			continue
		}
		if p.Code == asset.Code && p.Issuer == asset.Issuer {
			return true
		}
	}
	return false
}

// isUSDPeggedSoroban reports whether `asset` is an operator-resolved USD-pegged SAC wrapper ([Config.USDPeggedSorobanAssets]), by exact
// ContractID; the Soroban twin of [isUSDPeggedClassic].
func isUSDPeggedSoroban(asset canonical.Asset, pegs []canonical.Asset) bool {
	for _, p := range pegs {
		if p.Type != canonical.AssetSoroban {
			continue
		}
		if p.ContractID == asset.ContractID {
			return true
		}
	}
	return false
}

// survivorUSDVolume sums USD volume of the post-filter survivors from the per-trade map captured before the rewrite: the MinUSDVolume gate
// is post-class, post-outlier,
// else thin windows clear the floor on discarded volume. A missing key (quote not a USD surface) contributes zero.
func survivorUSDVolume(trades []canonical.Trade, tradeUSD map[string]*big.Rat) *big.Rat {
	sum := new(big.Rat)
	for i := range trades {
		if v, ok := tradeUSD[trades[i].ID()]; ok {
			sum.Add(sum, v)
		}
	}
	return sum
}

// minUSDVolumeRat is the configured floor as the exact decimal the operator wrote (shortest float64 round-trip, not its binary expansion);
// nil for NaN/Inf.
func minUSDVolumeRat(floor float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(floor, 'g', -1, 64))
	if !ok {
		return nil
	}
	return r
}

// dropForMinUSDVolume reports (and counts) a post-filter window failing the USD-volume floor; the caller treats it as empty. Applicability follows [usdQuoteDecimals]:
// USD-valuable quote: floor applies. On-chain but unrecognised peg: FAILS CLOSED (what a mint-and-dust attacker produces; add it to usd_pegged_classic_assets / sac_wrappers to re-admit). Non-USD fiat: converted at the [Config.FXStore] snap ([fiatWindowUSDVolume]); no admissible rate drops it.
func (o *Orchestrator) dropForMinUSDVolume(ctx context.Context, pair canonical.Pair, trades []canonical.Trade, usdVolume *big.Rat, now time.Time) bool {
	if o.cfg.MinUSDVolume <= 0 {
		return false
	}
	if _, valuable := usdQuoteDecimals(pair.Quote, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets); !valuable {
		switch pair.Quote.Type {
		case canonical.AssetClassic, canonical.AssetSoroban:
			o.logger.Warn("min_usd_volume floor unverifiable: on-chain quote asset has no recognised USD peg — window DROPPED (fail-closed; add the peg to usd_pegged_classic_assets / sac_wrappers to publish this pair)",
				"pair", pair.String())
			return o.dropMinUSDVolumeUnvaluable(pair)
		case canonical.AssetFiat:
			v, reason := o.fiatWindowUSDVolume(ctx, pair.Quote, trades, now)
			if reason != "" {
				o.logger.Warn("min_usd_volume floor unverifiable: no admissible FX rate for the fiat quote — window DROPPED (fail-closed)",
					"pair", pair.String(), "reason", reason)
				return o.dropMinUSDVolumeUnvaluable(pair)
			}
			usdVolume = v
		default:
			return false
		}
	}
	// A NaN/+Inf floor can never be met, so it drops (fail-closed).
	if floor := minUSDVolumeRat(o.cfg.MinUSDVolume); floor != nil && usdVolume.Cmp(floor) >= 0 {
		return false
	}
	obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume").Inc()
	o.mu.Lock()
	o.emptyWindows++
	o.mu.Unlock()
	obs.AggregatorEmptyWindowsTotal.Inc()
	return true
}

func (o *Orchestrator) dropMinUSDVolumeUnvaluable(pair canonical.Pair) bool {
	obs.AggregatorMinUSDVolumeUnvaluableTotal.WithLabelValues(pair.String()).Inc()
	obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume_unvaluable").Inc()
	o.mu.Lock()
	o.emptyWindows++
	o.mu.Unlock()
	obs.AggregatorEmptyWindowsTotal.Inc()
	return true
}

// fiatWindowUSDVolume values a non-USD fiat-quoted window in USD: quote volume at each source's off-chain scale times the quote-to-USD rate from the same FX snap and admission rule triangulation uses. Returns a refusal reason when no admissible rate exists.
func (o *Orchestrator) fiatWindowUSDVolume(ctx context.Context, quote canonical.Asset, trades []canonical.Trade, now time.Time) (*big.Rat, string) {
	if o.cfg.FXStore == nil {
		return nil, "fx_store_unwired"
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil, "fx_pair_invalid"
	}
	leg, err := canonical.NewPair(quote, usd)
	if err != nil {
		return nil, "fx_pair_invalid"
	}
	rate, observedAt, source, err := o.cfg.FXStore.FXQuoteAtOrBefore(ctx, leg, now, external.FXSources())
	switch {
	case errors.Is(err, timescale.ErrNoFXQuote):
		return nil, "fx_missing"
	case err != nil:
		return nil, "fx_error: " + err.Error()
	case rate == nil || rate.Sign() <= 0:
		return nil, "fx_non_positive"
	}
	if reason := fxSnapRejection(now, observedAt, source, o.cfg.CompositeReference.withDefaults().FXMaxAge); reason != "" {
		return nil, reason
	}
	sum := new(big.Rat)
	for i := range trades {
		amt := trades[i].QuoteAmount.BigInt()
		if amt == nil || amt.Sign() == 0 {
			continue
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(external.Lookup(trades[i].Source).AmountScaleDecimals())), nil)
		sum.Add(sum, new(big.Rat).SetFrac(amt, scale))
	}
	return sum.Mul(sum, rate), ""
}

// dropUnpriceable removes stored trades with a zero leg before venue VWAPs and outlier statistics: they must neither count as outliers nor
// weigh a VWAP with price-less volume.
func dropUnpriceable(pair canonical.Pair, trades []canonical.Trade) []canonical.Trade {
	out := make([]canonical.Trade, 0, len(trades))
	for _, t := range trades {
		if t.BaseAmount.BigInt().Sign() > 0 && t.QuoteAmount.BigInt().Sign() > 0 {
			out = append(out, t)
		}
	}
	if dropped := len(trades) - len(out); dropped > 0 {
		obs.AggregatorDroppedTradesTotal.WithLabelValues("unpriceable", pair.String()).Add(float64(dropped))
	}
	return out
}

// dropExcludedSources returns trades minus those from any source in
// excluded, preserving order in a fresh slice.
func dropExcludedSources(pair canonical.Pair, trades []canonical.Trade, excluded []string) []canonical.Trade {
	if len(excluded) == 0 {
		return trades
	}
	skip := make(map[string]struct{}, len(excluded))
	for _, s := range excluded {
		skip[s] = struct{}{}
	}
	out := make([]canonical.Trade, 0, len(trades))
	for _, t := range trades {
		if _, drop := skip[t.Source]; !drop {
			out = append(out, t)
		}
	}
	if dropped := len(trades) - len(out); dropped > 0 {
		obs.AggregatorDroppedTradesTotal.WithLabelValues("excluded_source", pair.String()).Add(float64(dropped))
	}
	return out
}

// filterForVWAP keeps only Class=Exchange, IncludeInVWAP=true sources, so only genuine exchange trades vote; unregistered sources fail
// closed via the registry default.
// Preserves order for a deterministic VWAP and returns a fresh slice.
func filterForVWAP(trades []canonical.Trade) []canonical.Trade {
	out := make([]canonical.Trade, 0, len(trades))
	for _, t := range trades {
		md := external.Lookup(t.Source)
		if md.Class == external.ClassExchange && md.IncludeInVWAP {
			out = append(out, t)
		}
	}
	return out
}

// formatRatSigDigits is how many SIGNIFICANT digits [formatRatFixed] keeps for a value too small for its fixed scale (see [renderScale]);
// 12 gives a sub-1e-12 price the fidelity a normal price gets at 12 decimals.
const formatRatSigDigits = 12

// formatRatMinSigDigits is the fewest significant digits a render at `decimals` places may keep before [renderScale] widens it: half the places, so at 12 places truncation error stays under 1e-5 (0.1 bp) with output unchanged for prices >= 1e-7.
func formatRatMinSigDigits(decimals int) int {
	return (decimals + 1) / 2
}

// formatRatMaxScale caps the fractional places [renderScale] extends to, so a pathological micro-valued rational can't make the publish
// path render an unbounded string; mirrors storage's bridgeRateMaxScale.
const formatRatMaxScale = 60

// formatRatFixed renders r at fixed precision, truncating toward zero as the API spec mandates ((*big.Rat).FloatString rounds half-to-
// even). The scale is a FLOOR: a positive value below 1e-12
// would render "0.000...0", serve price 0 and nil the next tick's edge graph (BuildEdges rejects Sign()<=0), so [renderScale] extends it
// for those values only.
func formatRatFixed(r *big.Rat, decimals int) string {
	decimals = renderScale(r, decimals)
	// Multiply numerator by 10^decimals, divide by denominator,
	// then insert the decimal point.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	num := new(big.Int).Mul(r.Num(), scale)
	q, _ := new(big.Int).QuoRem(num, r.Denom(), new(big.Int))

	// Build the string. q is the integer part at 10^decimals scale
	// → split into int and fractional halves.
	negative := q.Sign() < 0
	if negative {
		q.Neg(q)
	}
	digits := q.String()
	if len(digits) <= decimals {
		// Left-pad fractional part.
		pad := decimals - len(digits) + 1
		digits = zeroes(pad) + digits
	}
	cut := len(digits) - decimals
	out := digits[:cut] + "." + digits[cut:]
	if negative {
		out = "-" + out
	}
	return out
}

func zeroes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// renderScale is `decimals` for values keeping [formatRatMinSigDigits] significant digits there (normal prices render byte-identical);
// smaller positive values extend to [formatRatSigDigits]
// so they neither render as zero nor lose half their value. Float-free: it counts leading fractional zeros exactly, like storage's
// rateScaleFor, with `decimals` a floor.
func renderScale(r *big.Rat, decimals int) int {
	if r == nil || r.Sign() == 0 {
		return decimals
	}
	// firstSigPlace is the decimal position of |r|'s first significant
	// digit: 0 for |r| >= 1, 1 for 0.1<=|r|<1, 2 for 0.01<=|r|<0.1, …
	x := new(big.Rat).Abs(r)
	one := big.NewRat(1, 1)
	ten := big.NewRat(10, 1)
	firstSigPlace := 0
	for x.Cmp(one) < 0 && firstSigPlace < formatRatMaxScale {
		x.Mul(x, ten)
		firstSigPlace++
	}
	// The fixed render keeps decimals-firstSigPlace+1 significant digits
	// (none once the first lands beyond the last place). Extend only when
	// fewer than formatRatMinSigDigits would survive.
	if decimals-firstSigPlace+1 >= formatRatMinSigDigits(decimals) {
		return decimals
	}
	need := firstSigPlace + formatRatSigDigits
	if need > formatRatMaxScale {
		return formatRatMaxScale
	}
	return need
}

// Stats is a value-type snapshot of the orchestrator's runtime counters, independent of further ticking.
type Stats struct {
	LastTickAt   time.Time
	TicksTotal   int64
	VWAPWrites   int64
	EmptyWindows int64
	Errors       int64
}

// Stats returns a snapshot of the counters.
func (o *Orchestrator) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Stats{
		LastTickAt:   o.lastTickAt,
		TicksTotal:   o.ticksTotal,
		VWAPWrites:   o.vwapWrites,
		EmptyWindows: o.emptyWindows,
		Errors:       o.errors,
	}
}

// flushContributions emits one ContributionRecord to the configured sink, split out of refreshPairWindow for the gocognit ceiling. Best-effort: failures log at DEBUG; the VWAP cache write is load-bearing.
func (o *Orchestrator) flushContributions(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	trades []canonical.Trade,
	tradeUSD map[string]*big.Rat,
	weights map[string]int64,
) {
	if o.cfg.ContributionSink == nil {
		return
	}
	contributions := aggregate.SourceContributions(trades)
	if len(contributions) == 0 {
		return
	}
	if len(weights) > 0 {
		perSource := make(map[string]int, len(contributions))
		for i := range trades {
			perSource[trades[i].Source] += tradeWeight(&trades[i], weights)
		}
		for i := range contributions {
			contributions[i].TradeCount = perSource[contributions[i].Source]
		}
	}
	// Sum per-source USD over the POST-filter trades, matching the VWAP's contribution population; an outlier-dropped trade contributes 0 rather than double-attributing via the pre-filter total.
	var sourceUSD map[string]*big.Rat
	if len(tradeUSD) > 0 {
		sourceUSD = make(map[string]*big.Rat, len(contributions))
		for i := range trades {
			v, ok := tradeUSD[trades[i].ID()]
			if !ok {
				continue
			}
			acc, seen := sourceUSD[trades[i].Source]
			if !seen {
				acc = new(big.Rat)
				sourceUSD[trades[i].Source] = acc
			}
			acc.Add(acc, v)
		}
	}
	if err := o.cfg.ContributionSink.RecordContributions(ctx, ContributionRecord{
		Pair:            pair,
		Window:          window,
		ComputedAt:      time.Now().UTC(),
		Contributions:   contributions,
		SourceUSDVolume: sourceUSD,
	}); err != nil {
		// Non-fatal to the tick (the VWAP is already published), but a
		// lost bucket must be visible, not a Debug line.
		obs.AggregatorContributionWriteErrorsTotal.Inc()
		o.logger.Warn("contribution sink write failed",
			"pair", pair.String(), "window", window, "err", err)
	}
}
