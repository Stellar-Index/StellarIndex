// Package orchestrator drives the aggregation pre-compute cycle: each tick, for every configured (pair,
// window), it fetches the window's trades from Timescale, computes VWAP over the window ending at the last
// closed minute (ADR-0015) and caches it in Redis so API requests don't recompute. Around that core it applies
// the class filter, the stablecoin→fiat proxy ([internal/aggregate/stablecoin]), outlier filtering,
// triangulation with the forex snap, divergence refresh, confidence scoring and the ADR-0019 freeze response.
// CAGG refresh stays Timescale-driven. Ticks are serialised: a slow tick delays the next rather than piling
// queries on a slow Timescale.
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

// Store is the subset of timescale.Store the orchestrator needs.
// Declared as an interface so tests can substitute a mock without
// pulling up a real Timescale container.
type Store interface {
	TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error)
}

// FXStore is the subset of timescale.Store the forex-snap path needs; nil keeps FX legs on the cached-VWAP
// path (the safe default without FX ingestion). [timescale.ErrNoFXQuote] (no quote at-or-before cutoff) makes
// the caller fall back to cached VWAP and increment [obs.AggregatorFXSnapFallbackTotal].
type FXStore interface {
	FXQuoteAtOrBefore(ctx context.Context, pair canonical.Pair, cutoff time.Time, fxSources []string) (*big.Rat, time.Time, string, error)
}

// Cache is the subset of redis.UniversalClient we need. Declared
// as an interface for test-time replacement.
//
// Get is used by the triangulation worker to read freshly-written
// leg VWAPs. Returns redis.Nil for absent keys (a leg's refresh
// produced an empty window); the triangulation pass treats absence
// as "skip this chain this tick" rather than fail.
type Cache interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	// TxPipelined writes a served value together with its provenance,
	// observed-at stamp (and a composite's meta) in one MULTI/EXEC, so
	// no reader observes one without the other: a direct value clears a
	// prior composite's "triangulated" marker in the same transaction
	// that writes it. The freeze path extends
	// the held value's and its stamp's TTLs through it too.
	TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// FreezeMarker records an ActionFreeze decision; production is freeze.Writer, an interface so tests can
// substitute a recorder. All methods MUST be idempotent on (asset, quote). frozenValue is the last-known-good
// VWAP as a fixed-precision decimal (formatRatFixed(prev, 12)), empty with no prior bucket; it reaches
// freeze_events via the EventSink, not the Redis marker.
type FreezeMarker interface {
	// Mark writes a marker with the writer's flat default TTL and no
	// lifecycle state. Used by the triangulated-composite refusal,
	// which is a per-tick decision about a DERIVED price and owns no
	// freeze lifecycle of its own.
	Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error

	// MarkHold writes a marker carrying the ADR-0019 lifecycle state,
	// with `ttl` = remaining hold + silence grace. The freeze-duration
	// path uses this; the marker's expiry is a liveness backstop, not
	// the freeze policy.
	MarkHold(ctx context.Context, asset, quote canonical.Asset, frozenValue string,
		decision anomaly.Decision, state freeze.State, ttl time.Duration) error

	// LoadState reads back the lifecycle state a previous MarkHold
	// stamped. (State{}, false, nil) means no marker — which the
	// orchestrator reads as "never frozen" on a cold key and as the
	// ADR-0019 operator force-unfreeze on a key it believes is frozen.
	LoadState(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error)

	// Clear deletes the marker, ending the freeze on the serving path
	// immediately rather than after the remaining hold's TTL.
	Clear(ctx context.Context, asset, quote canonical.Asset) error
}

// WindowedFreezeMarker is the window-aware half of [FreezeMarker]. The marker is keyed (asset, quote), being
// the pair-wide flags.frozen, while the ladder advances per (pair, window) and windows freeze independently: a
// pair-level ladder let a cold window (under [Config.MinUSDVolume], or after a restart) adopt a sibling's
// FiredAt, HoldUntil, ExtensionsUsed and Escalated. Optional: a writer that can't scope keeps the pair-wide
// behaviour, which errs towards holding a freeze. freeze.Writer implements it.
type WindowedFreezeMarker interface {
	// MarkHoldForWindow is [FreezeMarker.MarkHold] recording `window`
	// as the owner of `state`, merging with the ladders the marker
	// already carries for the pair's other windows.
	MarkHoldForWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration,
		frozenValue string, decision anomaly.Decision, state freeze.State, ttl time.Duration) error

	// LoadStateForWindow is [FreezeMarker.LoadState] with the ladder
	// scoped to `window`: presence stays pair-wide (so the operator
	// override still reads identically), but the state returned is the
	// one `window` itself owns — the zero State when it owns none.
	LoadStateForWindow(ctx context.Context, asset, quote canonical.Asset,
		window time.Duration) (freeze.State, bool, error)

	// RetireWindowLadder drops `window`'s ladder from the marker while
	// leaving the marker in place, for the release of one window of a
	// pair whose other windows are still frozen.
	RetireWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration) error

	// ReleaseWindow ends `window`'s freeze when this process knows of no other frozen window: it clears the
	// marker UNLESS the marker still records a sibling's ladder, then retires only `window`'s (kept=true). The
	// marker is asked because this process can't see windows it hasn't evaluated (under MinUSDVolume, or after a
	// restart), and deleting on that blind spot ended a sibling's freeze.
	ReleaseWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (kept bool, err error)
}

// FreezeOverrideReader tells an operator's force-unfreeze from a marker
// that lapsed on its own: both look like a live freeze's marker going
// missing. `stellarindex-ops freeze-unfreeze` records a tombstone before it
// clears the marker; this reads it. Optional like [WindowedFreezeMarker];
// production's freeze.Writer implements it.
type FreezeOverrideReader interface {
	OverrideRecorded(ctx context.Context, asset, quote canonical.Asset) (bool, error)
}

// Config controls the orchestrator's behaviour. Built from config.go
// at startup; the orchestrator itself doesn't know about TOML.
type Config struct {
	// Pairs is the list of pairs the orchestrator pre-computes
	// VWAP for. Empty = orchestrator is a no-op (valid for
	// deployments that want the binary running as a placeholder
	// while operators configure their pair set).
	Pairs []canonical.Pair

	// Windows is the list of rolling windows the orchestrator
	// computes VWAP over. If empty, defaults to [5m, 1h, 24h].
	Windows []time.Duration

	// Interval is the gap between tick-driven refreshes. Defaults
	// to 30 s — matches the Redis `price:` TTL of 60 s with
	// headroom for tick lateness.
	Interval time.Duration

	// TickTimeout bounds one [Tick] and every store, cache and reference call in it, so one wedged query (a
	// half-open connection, a lock wait) can't stall every price. It is a WEDGE GUARD, not a budget: refreshOrder
	// is fixed, so a bound a healthy tick can exceed would starve the same tail pairs every tick. Zero defaults to
	// DefaultTickTimeoutIntervals × Interval.
	TickTimeout time.Duration

	// MaxTradesPerWindow caps per-query row count to protect
	// Timescale from a runaway scan on an unexpectedly active
	// pair. Defaults to 10_000.
	MaxTradesPerWindow int

	// EnableStablecoinFiatProxy expands each fiat target into the direct pair plus one stablecoin-backed pair per
	// peg (XLM/fiat:USD also draws XLM/crypto:USDT, USDC, DAI, PYUSD, USDP) and rewrites the trades onto the
	// target via aggregate.ProxyPair before VWAP. Off by default: it costs N+1 TradesInRange calls per (pair,
	// window). Mapping at the aggregator, not the decoder, keeps a depeg visible in the raw trades
	// (internal/aggregate/stablecoin.go).
	EnableStablecoinFiatProxy bool

	// USDPeggedClassicAssets is the operator's list of classic credits declared USD-pegged. The abstract
	// stablecoin map keys on `crypto:CODE` (what CEX feeds report) and deliberately excludes issuer-bearing
	// credits, yet mainnet's main USD DEX pairs are quoted in classic credits, so without this list every
	// XLM/fiat:USD VWAP is empty. Wired from cfg.Trades.USDPeggedClassicAssets so the indexer (usd_volume) and the
	// aggregator share one allow-list. Consulted only with EnableStablecoinFiatProxy and a fiat:USD target.
	USDPeggedClassicAssets []canonical.Asset

	// USDPeggedSorobanAssets is the SAC wrappers that inherit a USD peg from USDPeggedClassicAssets
	// (Type=AssetSoroban, ContractID = the SAC). No TOML knob: the binary derives it from `[supply].sac_wrappers`
	// × `[trades].usd_pegged_classic_assets`, the same inputs NewUSDVolumeQuoteSpec uses
	// (resolveUSDPeggedSorobanAssets). A SAC shares its classic's 7 decimals. [usdQuoteDecimals] uses it to apply
	// MinUSDVolume to SAC-USDC-quoted pairs, which otherwise served unguarded VWAP.
	USDPeggedSorobanAssets []canonical.Asset

	// MinUSDVolume, when > 0, requires a window's post-class, post-outlier USD volume to meet the threshold before
	// its VWAP publishes, so a single dust trade cannot set the price. It applies to every quote
	// [usdQuoteDecimals] can value: fiat:USD and abstract USD stablecoins (off-chain 10^8), and classic or SAC
	// pegs (7 decimals). Proxy-expanded trades are valued against their SOURCE quote before the rewrite. Non-USD
	// fiat converts at the [Config.FXStore] snap and fails closed without an admissible rate
	// (dropForMinUSDVolume). 0 = off; production stamps 10_000 ($10k) via AggregateConfig.
	MinUSDVolume float64

	// OutlierSigmaThreshold, when > 0, drops trades whose price sits more than sigma robust scales (1.4826·MAD,
	// not stdev) from BOTH the window median and its time-local neighbourhood (aggregate.FilterOutliersLocal): a
	// lone wild print disagrees with both, an agreed regime shift survives. Applied after class filtering and the
	// stablecoin rewrite so it compares like prices; windows with < 3 prices pass unchanged. 0 = off;
	// AggregateConfig stamps 4.0.
	OutlierSigmaThreshold float64

	// Anomaly, when non-nil, evaluates each fresh VWAP against the previous bucket (ADR-0019): Allow publishes,
	// Warn publishes, Freeze does NOT publish and keeps serving the last-known-good value while FreezeWriter's
	// marker sets flags.frozen. Nil = every VWAP publishes; production wires it at the binary boundary.
	Anomaly *anomaly.Checker

	// Triangulations is the operator's chain set. After the per-(pair, window) loop each Tick, each chain reads
	// its legs' fresh VWAPs and prices the target through the graph router (aggregate.BuildEdges →
	// CombineRoutes/CompositeRate) into its own cache key; aggregate.Triangulate/TriangulateChain are not on this
	// path. Cardinality: len(Windows) keys per chain per tick, which is why chains are explicit rather than every
	// fiat × stablecoin combination.
	Triangulations []TriangulationChain

	// MaxHops bounds the router's cross-rate route length (LEGS, so a
	// base→hub→quote route is 2 legs) when pricing a triangulation
	// TARGET via the graph router (internal/aggregate/router.go). 0
	// falls back to [DefaultMaxHops]; values outside [2,4] are clamped
	// in [New] (config-load validation already rejects them, so the
	// clamp only guards a struct assembled directly in a test).
	MaxHops int

	// MinRouteConfidence is the weakest-link confidence floor a router
	// route must clear to be treated as CONFIDENT (see
	// aggregate.CombineRoutes). Routes below it are excluded so a
	// dust/thin edge can't set a confident cross; when NO route clears
	// it the composite is flagged low-confidence and NOT published over
	// the direct price. 0 (default) disables the floor — every route is
	// confident, matching the pre-router static-chain behaviour.
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

	// ExcludedSources drops every stored trade from the named sources
	// at read time, before the class filter, so an operator can remove
	// a misbehaving source from VWAP without purging history. Applies
	// even when DisableClassFilter is set.
	ExcludedSources []string

	// Phase2Thresholds tunes the ADR-0019 Phase 2 freeze condition
	// (3-signal AND on confidence + z + source count). Zero-value
	// fields fall back to the [Default*] package constants — an
	// operator with no override gets the documented stop-gap
	// behaviour. Set per-field to tighten or loosen any single
	// signal without restating the others.
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

	// CompositeReference gates the current-bucket composite-reference
	// corroboration of the phase-2 freeze for structurally single-venue
	// targets (see composite_reference.go). Zero value =
	// off, which is what a Config assembled directly in a test gets.
	CompositeReference CompositeReferenceConfig

	// DivergenceRefresher, when non-nil, refreshes the `div:<base>/<quote>` cache once per pair per [Tick] so
	// flags.divergence_warning has a producer (ADR-0019); production is internal/divergence.Service, nil leaves
	// the flag always false. It uses the SHORTEST window's VWAP to stay ~Interval-fresh without hitting external
	// references per window.
	DivergenceRefresher DivergenceRefresher

	// DivergenceMinInterval skips the divergence pass until this long after the last successful one (zero =
	// every tick). The CMC free tier is 10,000 calls/MONTH; every 30s × 12 pairs is ~86,000/month, so a 5-minute
	// interval fits. The div:<base>/<quote> TTL covers this cadence plus a worst-case pass
	// (divergence.ServiceOptions.PairCount); 5-minute detection latency is acceptable for an anomaly signal
	// (ADR-0019).
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

	// DecimalsLookup corrects a window's raw VWAP for a non-7-decimal leg (aggregate.AdjustPrice,
	// docs/operations/runbooks/dex.md). The orchestrator's VWAP feeds the Redis fallback, the freeze chain, the
	// contribution sink and the SSE stream, none of which pass through internal/api/v1's
	// declineIfNonstandardDecimals guard, so this is the one place to stop a wrong price leaking. Nil resolves both
	// legs to StandardDecimals (factor exactly 1); production caches nonstandard_decimals_assets
	// (cmd/stellarindex-aggregator/decimals_cache.go).
	DecimalsLookup aggregate.DecimalsLookup

	// Logger is the structured logger. If nil, slog.Default() is
	// used.
	Logger *slog.Logger
}

// ContributionSink is the optional durable-mirror seam for
// per-source contributions to a windowed VWAP. Called once per
// (pair, window) at every successful VWAP compute.
type ContributionSink interface {
	RecordContributions(ctx context.Context, rec ContributionRecord) error
}

// ContributionRecord is the per-(pair, window, tick) shape passed
// to ContributionSink. Decoupled from the storage row shape so the
// sink can evolve without the orchestrator changing.
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

// DivergenceRefresher keeps `div:<base>/<quote>` populated (production: [internal/divergence.Service]).
// ourPrice is the pair's shortest-window VWAP, observedAt the Tick time; the implementation fetches references,
// computes divergence and writes the entry. RefreshPinnedPair is for a frozen pair, whose cached VWAP is the
// pinned last-known-good: references refresh but no verdict is reached against it.
type DivergenceRefresher interface {
	RefreshPair(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time) error
	RefreshPinnedPair(ctx context.Context, pair canonical.Pair, pinnedPrice float64, observedAt time.Time) error
}

// StreamPublisher fans closed-bucket events out (production: [internal/api/streaming/redispub.Publisher];
// the API republishes on its in-process Hub for `/v1/price/stream`). Called once per (pair, window) on each
// successful VWAP write; best-effort, since the VWAP cache is durable and the stream is enrichment. coverage
// is nil when unknown (composites don't track their legs'). PublishFrozenBucket is called once instead for a
// bucket a freeze refused, so a frozen series isn't silent; frozenSince is its first refused bucket's end.
type StreamPublisher interface {
	PublishClosedBucket(ctx context.Context, pair canonical.Pair, window time.Duration, valueDecimal string, observedAt time.Time, coverage *cachekeys.WindowCoverage) error
	PublishFrozenBucket(ctx context.Context, pair canonical.Pair, window time.Duration, observedAt, frozenSince time.Time) error
}

// DefaultWindows is the built-in window set — three buckets
// covering hot (5m), warm (1h), and cold (24h) consumer needs.
var DefaultWindows = []time.Duration{
	5 * time.Minute,
	1 * time.Hour,
	24 * time.Hour,
}

// normalizeWindows returns the positive windows ascending and de-duplicated,
// in a fresh slice. refreshDivergenceAll reads Windows[0] as the shortest;
// config validation rejects the rest, this covers a directly built Config.
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

// effectiveWindows is the window set [New] runs for a configured one:
// normalized, or [DefaultWindows] when nothing positive was configured.
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

// ShortestWindow is the window whose VWAP the divergence refresh compares
// for a configured window set: Windows[0] of an orchestrator built from ws.
// The divergence service stamps it on every verdict it caches.
func ShortestWindow(ws []time.Duration) time.Duration {
	return effectiveWindows(ws)[0]
}

// closedBucket is the aggregator's closed-bucket granularity: every
// window ends at the last closed boundary of this size (ADR-0015 — serve
// the last closed bucket, never an in-progress one). One minute is the
// served tier's closed bucket, the SSE observed_at cadence, and the return
// granularity ADR-0019's freeze baselines are calibrated at, so successive
// scored buckets are one baseline step apart for every window size.
const closedBucket = time.Minute

// decidedBucket is the outcome of the first evaluation of one closed
// bucket for one (pair, window); see [Orchestrator.refreshPairWindow].
type decidedBucket struct {
	end time.Time
	// published is nil when the bucket was held (frozen, empty, below
	// the volume floor).
	published *publishedBucket
	// frozen records that the bucket was refused by a freeze, so a
	// replaying tick keeps the leg out of triangulation.
	frozen       bool
	compositeRef *compositeReference
}

// publishedBucket is what a published closed bucket wrote, kept so a
// replaying tick republishes the scored value rather than a re-fetch.
type publishedBucket struct {
	value    string
	coverage cachekeys.WindowCoverage
	score    confidence.Score
	confOK   bool
	edge     aggregate.Quote
	leg      legRef
}

// DefaultInterval is the built-in tick cadence. 30s matches the
// Redis price-key TTL of 60s with headroom for missed ticks;
// higher-frequency aggregation is a follow-up once the API's
// consumer pattern stabilises.
const DefaultInterval = 30 * time.Second

// DefaultTickTimeoutIntervals sizes the default [Config.TickTimeout]: 4 × 30s = 120s, the
// `stellarindex_api_price_stale` threshold. A tick still running then is already serving prices the alert
// calls stale, so cutting it costs nothing new, and a wedged call is released inside the alert's 5-minute
// `for:`. Ticks have no duration metric, so a tighter bound can't be shown safe for the slowest healthy tick.
const DefaultTickTimeoutIntervals = 4

// DefaultMaxTradesPerWindow bounds one refresh's Timescale scan. It's wide for the 5m window, but a liquid
// pair (XLM/USDC on a busy day) can exceed it in 1h and 24h; TradesInRange then returns the NEWEST 10,000 and
// AggregatorWindowTruncatedTotal fires. Raise the cap or move large windows to a SQL aggregate if it fires
// sustainedly.
const DefaultMaxTradesPerWindow = 10_000

// DefaultMaxHops is the router route-length cap used when
// [Config.MaxHops] is unset (0). Three legs reaches every
// crypto→USD→fiat cross in the default coverage set plus one extra
// pivot; [maxRouterHops] clamps the accepted range to [2,4].
const DefaultMaxHops = 3

// maxRouterHops is the hard upper bound on [Config.MaxHops]. Four legs
// is the obscure×obscure worst case (see aggregate.FindRoutes); beyond
// it the acyclic-path search cost grows without buying reachability the
// default coverage set needs.
const maxRouterHops = 4

// Orchestrator holds the wired dependencies and runs the tick loop.
type Orchestrator struct {
	store  Store
	cache  Cache
	cfg    Config
	logger *slog.Logger

	// prevVWAPs is the last published VWAP per (pair, window), the anomaly evaluator's comparator; bounded by
	// len(Pairs) × len(Windows). It doesn't advance during a freeze, so the next bucket compares against the same
	// prev.
	//
	// WARNING: read and written LOCK-FREE, safe ONLY because one Tick runs at a time and its per-pair loop is
	// sequential. The sibling per-Tick maps (frozenThisTick, tickEdgeQuotes, lastComposites, freezeStates) rely on
	// the same invariant: parallelising the refresh loop REQUIRES guarding all of them with o.mu first.
	prevVWAPs map[string]*big.Rat

	// prevVWAPAt is when each prevVWAPs entry was set; an entry with no
	// stamp is never aged.
	prevVWAPAt map[string]time.Time

	// prevVWAPBucketEnd is the closed-bucket end each prevVWAPs entry was
	// published for: the observed-at stamp a re-seeded held value must carry.
	prevVWAPBucketEnd map[string]time.Time
	// prevVWAPCoverage is the window coverage of each prevVWAPs entry, so
	// a freeze reseed restores the held value with its own coverage.
	prevVWAPCoverage map[string]cachekeys.WindowCoverage

	// frozenPrevVWAPs is the shadow comparator for pairs whose bucket the freeze REFUSED. Scored against the pinned
	// pre-freeze prevVWAPs, z measures total drift since the freeze, so ADR-0019's auto-unfreeze (z < 3 twice) is
	// reachable only if the market returns to the freeze-time price; after a restart there is no prev at all and
	// every bucket is unscored ("phase2:unscored"), stalling the freeze. The shadow advances with each refused
	// bucket's fresh VWAP, scoring "is the market calm NOW". Cleared on publish/release; same invariant as prevVWAPs.
	frozenPrevVWAPs map[string]*big.Rat

	// scoredMinutes is the newest closed minute each (pair, window)
	// decision scored, where the next decision's scoring starts (see
	// marginalBuckets). Same single-Tick-at-a-time invariant as prevVWAPs.
	scoredMinutes map[string]minuteVWAP

	// lastWriteAt is the last successful VWAP write per FULL pair (so one quote can't vouch for another), written
	// only via recordPairWrite, by both served-key writers and the first-sighting seed; emitStalenessGauges reads it
	// for `stellarindex_api_price_stale`. Bounded by len(cfg.Pairs) + len(cfg.Triangulations); the gauge iterates
	// cfg.Pairs only, so cardinality is unchanged. Same single-Tick invariant as prevVWAPs.
	lastWriteAt map[string]time.Time

	// lastDivergenceRefreshAt is the wall-clock time of the most
	// recent successful refreshDivergenceAll pass. Read +
	// updated only inside [Tick] (single-runner invariant), so no
	// lock needed. Zero value means "never refreshed" — the first
	// tick after startup unconditionally runs the pass.
	lastDivergenceRefreshAt time.Time

	// frozenThisTick holds the `<pair>:<window>` keys this tick refused to publish (Phase 1 or 2), so the
	// triangulation pass doesn't re-publish a frozen leg's last-known-good as a fresh derived price
	// ([Orchestrator.legPriceFromCache]). Rebuilt each [Tick] and written only by the freeze step, so a frozen
	// window that returns early never re-enters it; [Orchestrator.frozenLeg] also reads freezeStates.
	//
	// WARNING: lock-free only because Tick is sequential; written in the per-pair loop and read in the
	// triangulation pass, so parallelising without o.mu could launder a frozen leg. See prevVWAPs.
	frozenThisTick map[string]struct{}

	// tickNow is the CURRENT tick's `now` — the injected clock, read once
	// at the top of [Tick]. publishComposite stamps the pair-level write
	// clock with it, so the composite writer, the direct writer and
	// emitStalenessGauges all judge one instant rather than the composite
	// stamping triangulateAll's own wall-clock read. Zero outside
	// a Tick. Same single-Tick-at-a-time invariant as frozenThisTick.
	tickNow time.Time

	// decidedBuckets holds, per `pair:window` stateKey, the outcome of
	// the last closed bucket that was decided, so a later tick inside the
	// same bucket replays it instead of scoring it a second time. Same
	// single-Tick-at-a-time invariant as prevVWAPs, so no lock is needed.
	decidedBuckets map[string]decidedBucket

	// heldDirect holds, per `pair:window` stateKey, the direct value of a
	// triangulation target priced this tick until the triangulation pass
	// decides whether it or the composite is served, so the shared served
	// key has one writer per tick. Rebuilt every Tick; same
	// single-Tick-at-a-time invariant as decidedBuckets.
	heldDirect map[string]heldDirect

	// streamedBuckets records, per `pair:window` stateKey, the closed
	// bucket last published to the stream, so every bucket is streamed
	// once whichever writer served it. Same invariant as decidedBuckets.
	streamedBuckets map[string]time.Time

	// tickEdgeQuotes collects, per window, this tick's published pair VWAPs (with a confidence from their quality
	// signals) as router edges for the triangulation pass. Frozen, dropped and empty windows add no edge, which is
	// how min_usd_volume keeps a dust pair from setting a confident cross. Rebuilt each [Tick].
	//
	// WARNING: lock-free only because Tick is sequential; a concurrent append is a data race. See prevVWAPs.
	tickEdgeQuotes map[time.Duration][]aggregate.Quote

	// lastComposites holds the latest composite price per (target pair, window), so the NEXT tick's confidence
	// step can compare a direct VWAP against an independently routed opinion (triangulate_corroborate.go explains
	// the one-tick lag and why it never feeds the freeze's source-count leg).
	//
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	lastComposites map[string]compositeSample

	// tickLegRefs holds, per window, this tick's confidently published VWAP + distinct venue count per pair, the
	// legs the composite-reference evaluator multiplies (composite_reference.go). Written at the publish point and
	// read by a LATER pair in the same loop (refreshOrder puts legs first). Rebuilt each Tick.
	//
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	tickLegRefs map[time.Duration]map[string]legRef

	// tickCompositeRefs holds this tick's composite-reference reading
	// per `<pair>:<window>` stateKey for the pairs it was evaluated on
	// (single-venue buckets of allow-listed targets). Written once per
	// bucket in refreshPairWindow BEFORE the confidence + freeze steps
	// and read by both of them (so they see the SAME sample) and by the
	// triangulation pass (composite_meta.corroboration_basis). Rebuilt
	// at the top of every Tick; same L4 lock-free invariant as prevVWAPs.
	tickCompositeRefs map[string]compositeReference

	// refreshOrder is cfg.Pairs re-ordered so composite-reference legs
	// refresh before their targets (see refreshOrder in
	// composite_reference.go); identical to cfg.Pairs when the mechanism
	// is off. Computed once in New.
	refreshOrder []canonical.Pair

	// freezeStates holds the ADR-0019 lifecycle per `<pair>:<window>`: hold, 4-extension ladder, escalation and
	// the auto-unfreeze streak (internal/aggregate/freeze.Policy). A present-but-inactive entry marks a key as
	// evaluated, so the Redis hydrate doesn't re-run each tick for healthy pairs. In-memory is the working copy:
	// the first evaluation after a restart re-hydrates from the marker, so a deploy doesn't restart the 2-hour
	// escalation clock.
	//
	// WARNING: lock-free only because Tick is sequential. See prevVWAPs.
	freezeStates map[string]freeze.State

	// bootstrapReleased is each pair's last bootstrap-cap gate state,
	// keyed by pair string: true once its score cleared the cap. It is
	// the prior state the gate's hysteresis band reads
	// ([confidence.BootstrapReengageDensityDays]). In-memory only, so a
	// restart falls back to the stricter upper gate. Same
	// single-Tick-at-a-time invariant as prevVWAPs, so no lock is needed.
	bootstrapReleased map[string]bool

	// windowedFreeze is [Config.FreezeWriter] when it can scope a
	// marker's ladder to the window that owns it
	// ([WindowedFreezeMarker]), nil otherwise. Resolved once in [New]
	// rather than type-asserted per tick.
	windowedFreeze WindowedFreezeMarker

	// overrideReader is [Config.FreezeWriter] when it can tell an
	// operator's force-unfreeze from a lapse ([FreezeOverrideReader]),
	// nil otherwise.
	overrideReader FreezeOverrideReader

	// clock is the orchestrator's time source, injectable so the
	// freeze lifecycle's hold/extension/escalation ladder — which is
	// measured in tens of minutes — is testable without sleeping.
	// Defaults to time.Now in [New].
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
	// Router hop budget: 0 = "use default"; clamp anything out of the
	// accepted [2,4] band. Config-load validation already rejects bad
	// values, so this only guards a Config assembled directly in a test.
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
	// A freeze writer that records which window owns each ladder lets
	// every window rehydrate ITS OWN freeze on a cold key instead of a
	// sibling's — see [WindowedFreezeMarker].
	if windowed, ok := cfg.FreezeWriter.(WindowedFreezeMarker); ok {
		o.windowedFreeze = windowed
	}
	if reader, ok := cfg.FreezeWriter.(FreezeOverrideReader); ok {
		o.overrideReader = reader
	}
	return o
}

// Run blocks until ctx is cancelled, invoking [Tick] on
// [Config.Interval] cadence. First tick fires immediately on
// startup so a freshly-launched aggregator has warm Redis keys
// before the API's first query.
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

// Tick runs one aggregation cycle — fetch trades, compute VWAP,
// write Redis for every (pair, window) combination in Config.
// Exported so tests can drive deterministic cycles without waiting
// on the ticker.
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

	// Every store and cache call below runs on tickCtx, so none of them
	// can outlive the tick's wedge guard — see [Config.TickTimeout].
	tickCtx, cancel := context.WithTimeout(ctx, o.cfg.TickTimeout)
	defer cancel()

	tickHadError, loopErr := o.refreshAllPairs(tickCtx, now)
	if loopErr != nil && ctx.Err() != nil {
		// The CALLER's context ended (shutdown): stop where we are, as
		// before. Only the tick's own deadline finishes the accounting.
		return loopErr
	}

	// Both passes are skipped once the tick's deadline has fired, whether
	// it cut the loop short (loopErr) or landed inside its last refresh.
	if tickCtx.Err() == nil {
		// Triangulation pass — runs AFTER the per-pair refresh so each
		// chain's legs read from the freshly-cached VWAPs. Per-chain
		// failures are logged + counted but never abort the tick.
		// Skipped on a cut tick: a composite must not be routed over
		// the partial edge set of a refresh that did not finish, and the
		// divergence pass must not record a refresh it could not run.
		o.triangulateAll(tickCtx)

		// Divergence refresh — runs AFTER the per-pair VWAPs are in
		// cache so RefreshPair has a fresh price to compare against
		// external references. Best-effort per-pair (errors logged +
		// counted, never abort the tick); the API's
		// `flags.divergence_warning` reads from the cache this populates.
		o.refreshDivergenceAll(tickCtx, now)
	}
	o.flushHeldDirect(tickCtx)

	// The tick's own deadline fired (the caller's context is still
	// live): some call stopped answering. The tick is an error, and it
	// still does its end-of-tick accounting below — the staleness gauges
	// in particular must keep climbing for every pair the cut tick did
	// not reach, not sit frozen at their last "fresh" reading.
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

	// Emit per-asset staleness so the
	// `stellarindex_api_price_stale` alert has a producer. Runs at end-of-
	// Tick whether or not any window wrote, so pairs with no fresh
	// trades climb past the alert threshold even though Tick doesn't
	// publish anything new for them.
	o.emitStalenessGauges(now)

	// ADR-0019 freeze lifecycle: publish how many (pair, window)
	// freezes are being HELD right now. A gauge, unlike the engaged
	// counter, tells "one pair frozen for an hour" apart from "sixty
	// pairs frozen for one tick" — see obs.AnomalyFreezeActive.
	obs.AnomalyFreezeActive.Set(float64(o.activeFreezeCount()))

	if timedOut {
		return fmt.Errorf("orchestrator: tick cut by its %s wedge guard: %w",
			o.cfg.TickTimeout, context.DeadlineExceeded)
	}
	return nil
}

// refreshAllPairs runs the per-(pair, window) refresh in refreshOrder.
// hadError reports a refresh that failed and was skipped (the tick
// carries on); err is non-nil only when ctx ended before the loop did —
// the caller's cancellation or the tick's own deadline — and the loop
// stopped early.
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

// activeFreezeCount counts the (pair, window) keys currently holding
// a freeze. Released keys keep a present-but-inactive entry (so the
// hydrate-once path stays once), so this walks the map rather than
// taking its len.
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

// recordPairWrite stamps a successful VWAP publish for the FULL pair: a base-only key let XLM/USD publishes
// keep a dead XLM/GBP reading fresh. It is the only writer of lastWriteAt, called by both served-key writers
// (refreshPairWindow and publishComposite), since a chain-only pair never reaches the direct writer. A zero
// `at` (outside a Tick) records nothing: an unstamped gauge keeps climbing, the fail-safe direction.
func (o *Orchestrator) recordPairWrite(pair canonical.Pair, at time.Time) {
	if at.IsZero() {
		return
	}
	o.lastWriteAt[pair.String()] = at
}

// pairLastWrite is the freshest write that can answer a lookup for
// `pair`. For every base but XLM that is the pair's own stamp. XLM has
// two interchangeable base forms (see emitStalenessGauges), so either
// form's write for the SAME quote counts — never another quote's.
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

// emitStalenessGauges sets `stellarindex_price_staleness_seconds` per configured (base, quote) pair to
// `now - lastWriteAt[pair]`. Its alert is the only serving-freshness alert, so each quote needs its own series.
// A never-written pair falls back to `now`: ~0 on the first tick, then climbing. Labels must match the asset_id
// customers query: the aggregator's pair is `crypto:XLM/fiat:USD` while /v1/price uses `native`, so a known
// alias pair is emitted under both forms (as internal/api/v1/changes.go::aliasEntityIDs).
func (o *Orchestrator) emitStalenessGauges(now time.Time) {
	// First sighting — treat as "just observed" so the metric is
	// present but doesn't immediately page. Seeded for every pair
	// BEFORE any is read, so the dual-form merge in pairLastWrite sees
	// the same map whichever form cfg.Pairs lists first.
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

// refreshPairWindow computes VWAP for one (pair, window) over the window
// ending at the last CLOSED bucket boundary (ADR-0015) and writes it to
// Redis. The first tick to see a bucket closed decides it — scores it,
// steps the freeze lifecycle, publishes or holds; a later tick inside the
// same bucket replays that decision instead of scoring the same data
// again, so every scored step is exactly one closed bucket apart.
// ErrNoTrades is a normal-path outcome (the window was empty for this
// pair) and not propagated as an error.
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

// decideBucket is the first evaluation of one closed bucket: fetch
// [bucketEnd-window, bucketEnd), filter, VWAP, the Phase 1 and Phase 2
// freeze steps, and — when the bucket clears them — the publish. Returns
// the published bucket, or nil when the bucket was held (frozen, empty,
// or below the volume floor).
func (o *Orchestrator) decideBucket(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	bucketEnd time.Time,
	now time.Time,
) (*publishedBucket, error) {
	from := bucketEnd.Add(-window)
	trades, tradeUSD, proxied, coverage, err := o.fetchForTarget(ctx, pair, from, bucketEnd)
	if err != nil {
		return nil, fmt.Errorf("fetch %s %v: %w", pair.String(), window, err)
	}
	trades = dropExcludedSources(pair, trades, o.cfg.ExcludedSources)
	preFilter := len(trades)
	if !o.cfg.DisableClassFilter {
		trades = filterForVWAP(trades)
		if dropped := preFilter - len(trades); dropped > 0 {
			// `pair` is the CONFIGURED target pair (bounded: only
			// o.cfg.Pairs entries reach here); per-pair drops let
			// outlier_storm attribute a single-issuer SDEX token farm.
			obs.AggregatorDroppedTradesTotal.WithLabelValues("class", pair.String()).Add(float64(dropped))
		}
	}
	trades = dropUnpriceable(pair, trades)
	// Venue-level view of the set the outlier filter is handed: the
	// outlier_storm alert reads per-venue DISAGREEMENT from this, not
	// the trim re-count.
	recordWindowStage(pair, window, "fetched", preFilter)
	recordWindowStageVolume(pair, window, "class", trades)
	o.recordVenueVWAPs(pair, window, trades)
	if o.cfg.OutlierSigmaThreshold > 0 {
		preOutlier := len(trades)
		// Time-local trimming: a print is dropped only when it
		// disagrees with the whole window AND its neighbourhood, so an
		// agreed regime shift survives while a lone wild print does not
		// (see aggregate.FilterOutliersLocal).
		// A trim that would leave less base volume than it removes
		// withholds the window instead, compared at the same
		// common scale computeNormalizedVWAP weights by.
		trades = aggregate.FilterOutliersLocal(trades, aggregate.LocalOutlierOptions{
			Sigma:               o.cfg.OutlierSigmaThreshold,
			AmountScaleDecimals: amountScaleDecimalsFor,
		})
		if dropped := preOutlier - len(trades); dropped > 0 {
			obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String()).Add(float64(dropped))
		}
	}
	recordWindowStageVolume(pair, window, "outlier", trades)
	if len(trades) == 0 {
		o.noteEmptyWindow()
		return o.unpricedBucket(ctx, pair, window, now)
	}

	// Sum USD across the SURVIVOR
	// slice, not the pre-filter total returned by fetchForTarget.
	// Without this, windows that get gutted by class/outlier filters
	// can still publish above MinUSDVolume on volume that never made
	// it into the VWAP — the gate is supposed to keep thin survivor
	// sets out, so the input it evaluates must be the survivor set.
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

	o.flushContributions(ctx, pair, window, trades, tradeUSD)

	// Phase 1 anomaly evaluation BEFORE cache write — class-deviation
	// + source-count threshold (the L2.4 stop-gap). On freeze we
	// keep the previous bucket's value in cache (don't overwrite)
	// and emit a freeze marker so flags.frozen=true on the next read.
	// evaluateAndMaybeFreeze stands down while THIS window already holds
	// an active freeze, so the Phase 2 lifecycle below stays
	// the sole release authority once frozen.
	stateKey := pair.String() + ":" + window.String()
	mb := o.scoreMinutes(trades, pair, stateKey, vwap)
	if action, ok := o.evaluateAndMaybeFreeze(ctx, pair, window, mb, trades, stateKey, now); !ok {
		_ = action
		// Freeze: evaluateAndMaybeFreeze has already refreshed the LKG
		// VWAP key's TTL. Skip the cache write so the prior
		// bucket's value keeps serving.
		return nil, nil
	}

	// Phase 2 (ADR-0019): compute confidence, then advance the freeze lifecycle, both BEFORE the cache write so a
	// freeze leaves the prior value in cache. The step runs unconditionally, not only when the 3-signal AND fires:
	// a held pair stays frozen through quiet buckets and is released only by the auto-unfreeze condition (two
	// healthy buckets after the initial hold), and an unscored bucket must not release a live freeze.
	prevForConfidence := o.prevVWAPs[stateKey]
	if shadow, frozen := o.frozenPrevVWAPs[stateKey]; frozen {
		// Mid-freeze: when the window holds no earlier minute, score
		// against the PREVIOUS refused bucket's fresh VWAP, not the
		// pinned pre-freeze baseline — see frozenPrevVWAPs.
		prevForConfidence = shadow
	}
	// Composite-reference corroboration: for an allow-listed
	// structurally single-venue target, build the chain composite on the
	// CURRENT bucket (this tick's leg publishes + an FX snap at `now`) and
	// read it against this fresh VWAP. Evaluated BEFORE the confidence
	// step so the confidence factor and the freeze verdict below see the
	// SAME sample (triangulationDivergencePct prefers it over the prior
	// tick's chain output). Not evaluated at all for multi-venue buckets.
	var compositeRef compositeReference
	if o.compositeReferenceEligible(pair, trades) {
		compositeRef = o.evaluateCompositeReference(ctx, pair, window, now, vwap)
	} else {
		// Not evaluated this tick — retire the previous tick's verdict
		// rather than leaving it standing (see clearCompositeReference).
		o.clearCompositeReference(pair, window)
	}
	conf, confOK := o.computeConfidence(ctx, pair, window, vwap, mb.returns(prevForConfidence), trades, now)
	// The freeze's source_count leg counts VENUES only; router routes are
	// never a second venue (ADR-0019 amendment §2, see
	// triangulate_corroborate.go). The composite reference changes only
	// the VERDICT (compositeRef), never the count.
	if o.stepPhase2Freeze(ctx, pair, window, stateKey, now,
		conf, confOK, distinctSourceCount(trades), vwap, compositeRef) {
		// Refused: advance the shadow comparator with this bucket's
		// fresh VWAP so the NEXT frozen bucket scores a per-tick
		// return (and a post-restart frozen pair becomes scorable
		// from its second bucket instead of stalling unscored).
		o.frozenPrevVWAPs[stateKey] = vwap
		return nil, nil
	}
	// Published (or released this tick): the shadow's job is done.
	delete(o.frozenPrevVWAPs, stateKey)

	// Aggregator writers stay in big.Rat / big.Int land; API readers
	// parse the string back to a decimal. Float encoding is prohibited
	// on this path per ADR-0003.
	pub := &publishedBucket{
		value:    formatRatFixed(vwap, 12),
		coverage: coverage,
		score:    conf.Score,
		confOK:   confOK,
		// Only a published bucket becomes a router edge / reference leg:
		// frozen, dropped, empty and below-floor buckets returned above,
		// which is how a dust pair stays out of the cross-rate graph.
		edge: newEdgeQuote(pair, vwap, conf, confOK, trades),
		leg:  o.newLegRef(pair, vwap, trades, proxied),
	}
	if err := o.publishDirect(ctx, pair, window, pub, bucketEnd, now); err != nil {
		return nil, err
	}

	// Update the prev-VWAP comparator slot ONLY on successful
	// publish. Frozen buckets do not advance THIS slot — but they do
	// advance frozenPrevVWAPs above, which is what mid-freeze scoring
	// compares against; keeping the pinned value here as the sole
	// comparator was the auto-unfreeze ratchet (see frozenPrevVWAPs).
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

// ageComparator drops a prevVWAPs entry the window has not refreshed within
// [Orchestrator.vwapMaxAge]. A comparator that old no longer has a cached
// last-known-good beside it, so a freeze fired against it would refuse the
// bucket with no value held to serve. A live freeze keeps its comparator:
// it is the held value.
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

// heldDirect is a triangulation target's priced direct bucket awaiting
// the triangulation pass's decision (see [Orchestrator.heldDirect]).
type heldDirect struct {
	pair      canonical.Pair
	window    time.Duration
	pub       *publishedBucket
	bucketEnd time.Time
	now       time.Time
}

// publishDirect records a decided bucket's router edge and reference leg
// for the triangulation pass and serves its direct VWAP. Shared by the
// deciding tick and every replaying tick of the same bucket, so the
// replay republishes exactly what was scored. A triangulation target's
// value is held instead, for the triangulation pass to serve or drop
// ([Orchestrator.settleDirect]): a replaying tick writing it would flip
// the key back to the direct print under a composite on every tick.
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

// vwapMaxAge returns this orchestrator's silence grace: 10 missed ticks
// at its OWN configured cadence, floored at [cachekeys.VWAPMaxAge].
// Deriving it from cfg.Interval keeps a raised interval from silently
// shrinking the grace, and a tick cycle longer than cachekeys.VWAPMaxAge
// (a large pair set, a slow Timescale, a retry storm) from flapping the
// long windows between 200 and 404. The floor keeps a FASTER-than-
// default interval from tightening the documented grace.
func (o *Orchestrator) vwapMaxAge() time.Duration {
	if derived := 10 * o.cfg.Interval; derived > cachekeys.VWAPMaxAge {
		return derived
	}
	return cachekeys.VWAPMaxAge
}

// vwapTTL is [cachekeys.VWAPTTL] derived from this orchestrator's own
// tick cadence rather than the package-default cadence.
func (o *Orchestrator) vwapTTL(window time.Duration) time.Duration {
	return cachekeys.VWAPTTLWithMaxAge(window, o.vwapMaxAge())
}

// serveDirect writes a direct VWAP to the pair's served key, stamped
// with the closed bucket its window ends at, and clears any
// "triangulated" provenance a prior composite left there, in one
// MULTI/EXEC: the API serves the Redis fallback only under that marker,
// so a direct (possibly thin, single-source) value must never be
// readable beneath it. Then it streams the
// bucket, once.
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
		// Operators alert on `rate(...vwap_cache_write_errors_total[5m])
		// > 0`: without it a Redis that refuses every write (BGSAVE
		// MISCONF, the May-10 incident) is invisible until /v1/price 404s.
		obs.AggregatorVWAPCacheWriteErrorsTotal.Inc()
		return fmt.Errorf("redis publish %s: %w", key, err)
	}

	// Cache write confidence (only on successful publish — frozen
	// buckets must NOT carry a stale score forward). Best-effort:
	// confidence enrichment, never a publish-blocking signal.
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

// isTriangulationTarget reports whether pair is the target of a
// configured triangulation chain, i.e. its served key has a second,
// composite writer.
func (o *Orchestrator) isTriangulationTarget(pair canonical.Pair) bool {
	for i := range o.cfg.Triangulations {
		t := o.cfg.Triangulations[i].Target
		if t.Base.Equal(pair.Base) && t.Quote.Equal(pair.Quote) {
			return true
		}
	}
	return false
}

// settleDirect resolves a target's held direct value once the
// triangulation pass has decided (pair, window): published when the
// composite was not served, otherwise dropped, keeping only the direct
// market's confidence score, which the served composite carried before
// the hold existed. Idempotent.
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

// flushHeldDirect runs after the triangulation pass. It serves any direct
// value the pass left undecided, or drops them when the tick's context
// has ended: a write on it cannot land, and the next tick replays the
// bucket.
func (o *Orchestrator) flushHeldDirect(ctx context.Context) {
	for _, h := range o.heldDirect {
		if ctx.Err() != nil {
			delete(o.heldDirect, h.pair.String()+":"+h.window.String())
			continue
		}
		o.settleDirect(ctx, h.pair, h.window, false)
	}
}

// replayDecidedBucket re-applies an already-decided closed bucket on a
// later tick inside the same bucket: the same value is republished (the
// shared key may since hold a composite) and the per-tick state the
// triangulation pass reads is restored, but nothing is re-scored — the
// freeze lifecycle, its release streak and the comparators advance once
// per closed bucket, not once per tick.
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

// computeNormalizedVWAP computes VWAP and applies the non-standard-decimals normalization in one step, since
// aggregate.VWAP is correct only when both legs share a scale (aggregate.AdjustPrice,
// docs/operations/runbooks/dex.md). A nil DecimalsLookup makes it an exact no-op. One call site means every
// consumer sees the same corrected number.
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

// amountScaleDecimalsFor resolves a trade source's smallest-unit scale from
// the external registry, mirroring internal/api/v1's identically-named
// helper. aggregate.NormalizeAmountScale takes this as a parameter rather
// than importing internal/sources/external itself — that package imports
// aggregate, so the reverse edge would be an import cycle.
func amountScaleDecimalsFor(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// markFrozenThisTick records a freeze refusal so [Orchestrator.legPriceFromCache] won't feed the
// last-known-good into a chain. Called from the freeze paths themselves so a new freeze path can't forget it
// and reopen the laundering route. Tolerates a nil map for tests that call refreshPairWindow directly.
func (o *Orchestrator) markFrozenThisTick(pair canonical.Pair, window time.Duration) {
	if o.frozenThisTick == nil {
		o.frozenThisTick = make(map[string]struct{})
	}
	o.frozenThisTick[frozenTickKey(pair, window)] = struct{}{}
}

// frozenLeg reports whether (pair, window) is frozen for the triangulation pass: refused this tick, OR inside
// a freeze this process holds. The second arm matters: a bucket whose fetch or VWAP errors never reaches the
// freeze step, yet its LKG stays in Redis ([Orchestrator.keepFrozenVWAPAlive]) and would launder into a
// derived price. Bounded by hold + marker grace, how long the LKG lives; unbounded, a never-evaluated ladder
// would block chains through the pair long after there was anything to launder.
func (o *Orchestrator) frozenLeg(pair canonical.Pair, window time.Duration) bool {
	if _, ok := o.frozenThisTick[frozenTickKey(pair, window)]; ok {
		return true
	}
	// Same `pair:window` shape refreshPairWindow keys o.freezeStates by.
	st := o.freezeStates[pair.String()+":"+window.String()]
	grace := o.cfg.Phase2Thresholds.Lifecycle.WithDefaults().MarkerGrace
	return freeze.LadderStillLive(st, grace, o.clock())
}

// frozenTickKey is the [Orchestrator.frozenThisTick] key. Its own key
// space — deliberately not shared with refreshPairWindow's stateKey,
// which happens to use the same shape but serves the prevVWAPs map.
func frozenTickKey(pair canonical.Pair, window time.Duration) string {
	return pair.String() + ":" + window.String()
}

// keepFrozenVWAPAlive extends the TTL of the last-known-good VWAP key and every qualifier beside it
// (observed-at, coverage, triangulated marker, composite flags meta, confidence) to the marker's. A freeze
// skips the cache write, so the LKG keeps its window-length TTL and would expire mid-freeze, leaving
// frozen=true with no value; a stray qualifier would relabel a frozen composite as direct. The value isn't
// rewritten, so observed_at stays honest. ttl MUST be the marker's own (remaining hold + grace, up to ~35 min);
// <= 0 falls back to FreezeTTL. Best-effort: a missing key or transient Redis error is not fatal.
func (o *Orchestrator) keepFrozenVWAPAlive(ctx context.Context, pair canonical.Pair, window, ttl time.Duration) {
	if ttl <= 0 {
		ttl = cachekeys.FreezeTTL
	}
	key := cachekeys.VWAP(pair.Base, pair.Quote, window)
	atKey := cachekeys.VWAPObservedAt(pair.Base, pair.Quote, window)
	provKey := cachekeys.VWAPProvenance(pair.Base, pair.Quote, window)
	metaKey := cachekeys.VWAPCompositeMeta(pair.Base, pair.Quote, window)
	covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window)
	// The score cached at the LKG's publish describes the LKG, so it lives
	// exactly as long as the held value (ADR-0019: confidence on every
	// published price, frozen included). The refused bucket's score is
	// never written.
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

// evaluateAndMaybeFreeze runs the anomaly check on a fresh VWAP and writes a freeze marker when told to.
// ok=true (Allow/Warn): the caller writes the cache; ok=false (Freeze): it skips the write so the previous
// value keeps serving. A nil Anomaly returns Allow.
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

	// Once THIS window holds an active freeze, Phase 1 stands down and the ADR-0019 lifecycle is the SOLE release
	// authority. Phase 1 measures against prevVWAPs, held fixed for the whole hold, so a price settling past the
	// class FreezePct keeps it firing; each fire short-circuits before the Phase 2 step that alone earns the
	// auto-unfreeze streak, pinning the LKG to escalation. Standing down doesn't weaken the freeze: the lifecycle
	// keeps the bucket refused while Active and re-fires its own AND on an anomalous bucket.
	if o.freezeStates[stateKey].Active() {
		return anomaly.ActionAllow, true
	}

	prev := o.prevVWAPs[stateKey]
	step := mb.worstStep(prev)
	decision := o.cfg.Anomaly.Evaluate(anomaly.Observation{
		Pair:     pair,
		PrevVWAP: step.prev,
		CurrVWAP: step.curr,
		// Venues only, as in the Phase 2 leg: router corroboration is not
		// a second source (see triangulate_corroborate.go).
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

	// Phase 1 shares the ADR-0019 lifecycle with Phase 2: one owner per `freeze:<asset>:<quote>` key, or a Phase 1
	// fire would truncate a Phase 2 hold's TTL and leave the ladder un-advanced. Scored=false and
	// Corroborated=false: Phase 1 has no confidence or corroboration evidence, so it earns no streak and gets the
	// shorter first hold. A firing signal ends frozen unless the operator cleared the marker (a force-unfreeze);
	// a persisting anomaly re-freezes next tick, since the durable fix for mis-calibration is a threshold change.
	if !o.stepFreezeLifecycle(ctx, pair, window, stateKey,
		freeze.Signal{Now: now, Fires: true}, decision) {
		return decision.Action, true
	}
	return decision.Action, false
}

// distinctSourceCount returns how many distinct trade.Source values
// contributed to the supplied trades. Zero on empty input — the
// caller short-circuits before calling Evaluate, but the guard is
// cheap enough to keep here too.
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

// fetchForTarget pulls one target pair's window trades: a single TradesInRange without the stablecoin proxy,
// otherwise the aggregate.ExpandTargetPair backers each fetched and rewritten onto the target. A failing
// backer is logged and skipped so one connector can't black out the target. tradeUSD (by Trade.ID()) is valued
// BEFORE the rewrite blurs the quote scale (classic/SAC 7, off-chain 1e8) and feeds [survivorUSDVolume] and
// flushContributions over post-filter survivors. proxied lists rewritten trade IDs ([legRef]'s proxyShare;
// nil with the proxy off). coverage is truncated if any read hit the row cap.
func (o *Orchestrator) fetchForTarget(
	ctx context.Context,
	target canonical.Pair,
	from, to time.Time,
) (trades []canonical.Trade, tradeUSD map[string]*big.Rat, proxied map[string]struct{}, coverage cachekeys.WindowCoverage, err error) {
	if !o.cfg.EnableStablecoinFiatProxy {
		t, coveredFrom, err := o.fetchTradesDetectTruncation(ctx, target, target, from, to)
		if err != nil {
			return nil, nil, nil, coverage, err
		}
		usd := usdVolumeForPairPerTrade(target, t, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets)
		return t, usd, nil, widenCoverage(coverage, coveredFrom), nil
	}

	sources, err := aggregate.ExpandTargetPairWithClassicPegs(target, o.cfg.USDPeggedClassicAssets)
	if err != nil {
		return nil, nil, nil, coverage, fmt.Errorf("expand target %s: %w", target.String(), err)
	}

	var merged []canonical.Trade
	var fetchErrs []error
	tradeUSD = map[string]*big.Rat{}
	proxied = map[string]struct{}{}
	for _, src := range sources {
		batch, coveredFrom, ferr := o.fetchTradesDetectTruncation(ctx, target, src, from, to)
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
		return nil, nil, nil, coverage, fmt.Errorf("all %d source pairs failed: %w", len(sources), errors.Join(fetchErrs...))
	}
	return merged, tradeUSD, proxied, coverage, nil
}

// usdVolumeForPairPerTrade returns exact USD value per Trade.ID(), keyed before fetchForTarget rewrites Pair so
// filters can drop trades without losing attribution. nil when the quote isn't a USD surface (volume_usd is
// then NULL). Scale comes from [usdQuoteDecimals], the classification [dropForMinUSDVolume] also uses, so the
// two can't disagree.
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

// usdQuoteDecimalsForTrade resolves ONE trade's QuoteAmount scale: the [usdQuoteDecimals] tiers, but the
// off-chain tiers read the source's declared scale from the external registry, as [approxUSDVolume] does. CEX
// pollers stamp 8, FX pollers 6 (`massive`, `exchangeratesapi`); valuing 6dp at 1e8 understates 100×, enough to
// drop a healthy window below MinUSDVolume every tick and persist a 100×-low volume_usd. On-chain tiers keep the
// protocol 7, because [external.Metadata.AmountScaleDecimals] defaults to 8 and would understate an unregistered
// venue 10×.
func usdQuoteDecimalsForTrade(quote canonical.Asset, source string, classicUSDPegs, sorobanUSDPegs []canonical.Asset) (decimals int, ok bool) {
	switch {
	case quote.Type == canonical.AssetFiat && quote.Code == "USD",
		aggregate.IsFiatProxyFor(quote, "USD"):
		return external.Lookup(source).AmountScaleDecimals(), true
	default:
		return usdQuoteDecimals(quote, classicUSDPegs, sorobanUSDPegs)
	}
}

// usdQuoteDecimals answers the pair-level question "is this quote a USD surface" with its decimal scale,
// without a live price:
//  1. fiat:USD: 8.
//  2. An abstract USD stablecoin ticker ([aggregate.IsFiatProxyFor] → "USD"): 8, since only off-chain sources
//     stamp the abstract ticker. Tiers 1-2 are per-pair defaults; [usdQuoteDecimalsForTrade] resolves 8 vs 6
//     per trade. Tier 2 is what the proxy expansion fetches under; without it a USDT-quoted window is $0.
//  3. A classic credit on classicUSDPegs: 7.
//  4. A SAC on sorobanUSDPegs: 7.
//
// ok=false covers non-USD fiat and stablecoins (need FX), unpegged on-chain quotes (dropForMinUSDVolume's
// unvaluable branch) and everything else. Valuation and the MinUSDVolume floor both call this, so they can't drift.
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

// isUSDPeggedClassic reports whether `asset` is one of the
// operator-declared classic USD-pegged credits. Matched by exact
// (code, issuer) equality — the same shape the orchestrator's
// expansion path uses.
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

// isUSDPeggedSoroban reports whether `asset` is one of the
// operator-resolved Soroban SAC-wrapper USD-pegged assets (see
// [Config.USDPeggedSorobanAssets]). Matched by exact ContractID
// equality — the Soroban twin of [isUSDPeggedClassic].
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

// survivorUSDVolume sums the USD volume of the post-filter survivors from the per-trade map captured before
// the rewrite. The MinUSDVolume gate is post-class, post-outlier: a pre-filter total would let thin windows
// clear the floor on discarded volume. A missing key (quote not a USD surface) contributes zero.
func survivorUSDVolume(trades []canonical.Trade, tradeUSD map[string]*big.Rat) *big.Rat {
	sum := new(big.Rat)
	for i := range trades {
		if v, ok := tradeUSD[trades[i].ID()]; ok {
			sum.Add(sum, v)
		}
	}
	return sum
}

// minUSDVolumeRat is the configured floor as the exact decimal the
// operator wrote: the shortest float64 round-trip, not its binary expansion.
// nil for NaN/±Inf, which have no exact value.
func minUSDVolumeRat(floor float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(floor, 'g', -1, 64))
	if !ok {
		return nil
	}
	return r
}

// dropForMinUSDVolume reports (and counts) a post-filter window that fails the USD-volume floor; the caller
// treats it as empty. usdVolume is the survivor total ([survivorUSDVolume]). Applicability is
// [usdQuoteDecimals], the same classification that valued it:
//   - USD-valuable quote: the floor applies.
//   - on-chain but not a recognised peg: FAILS CLOSED. An unvaluable quote is exactly what a mint-and-dust
//     attacker produces (the serving side's pricingguard.SubstanceGate agrees); the WARN + metric name the peg,
//     and adding it to usd_pegged_classic_assets / sac_wrappers re-admits the pair.
//   - non-USD fiat: converted at the [Config.FXStore] snap ([fiatWindowUSDVolume]); no admissible rate drops it.
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

// fiatWindowUSDVolume values a non-USD fiat-quoted window in USD: the
// quote volume at each source's declared off-chain scale (the fiat:USD
// tier of [usdQuoteDecimalsForTrade]) times the quote→USD rate from the
// same FX snap and admission rule triangulation uses. Returns a non-empty
// refusal reason when no admissible rate exists.
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

// dropUnpriceable removes stored trades with a zero leg before the venue
// VWAPs and outlier statistics: they have no price, so they must neither
// count as outliers nor weigh a venue's VWAP with price-less volume.
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

// filterForVWAP keeps only Class=Exchange, IncludeInVWAP=true sources: only genuine exchange trades vote.
// Unregistered sources fail closed via the registry default (visible in /v1/sources, no VWAP vote). Preserves
// order for deterministic VWAP and returns a fresh slice.
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

// formatRatSigDigits is how many SIGNIFICANT digits [formatRatFixed]
// preserves for a value too small to render at its requested fixed
// scale — see [renderScale]. 12 keeps a sub-1e-12 price round-tripping
// with the same fidelity a normal-magnitude price gets at 12 decimals.
const formatRatSigDigits = 12

// formatRatMinSigDigits is the fewest significant digits a render at
// `decimals` places may keep before [renderScale] widens it: half the
// requested places, so leading zeros never spend more than half of the
// precision the caller asked for. At the published 12 places that is 6
// digits — a relative truncation error under 1e-5 (0.1 bp, a hundredth of
// baseline.MinMAD's 10 bp) with output byte-identical for prices >= 1e-7.
func formatRatMinSigDigits(decimals int) int {
	return (decimals + 1) / 2
}

// formatRatMaxScale caps the fractional places [renderScale] will
// extend to, so a pathological (or hostile) micro-valued rational can
// never make us render an unbounded string on the publish path. Mirrors
// storage-tier bridgeRateMaxScale.
const formatRatMaxScale = 60

// formatRatFixed renders r at fixed precision, truncating toward zero as the API spec mandates
// ((*big.Rat).FloatString rounds half-to-even). 12 places covers sensible price ranges. The scale is a FLOOR:
// a positive value below 1e-12 would render "0.000…0", reparse to zero, serve price 0 and nil the next tick's
// edge graph (BuildEdges rejects Sign()<=0), so [renderScale] extends it for those values only.
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

// renderScale is `decimals` for values keeping [formatRatMinSigDigits] significant digits there (normal
// prices render byte-identical); smaller positive values extend to [formatRatSigDigits] so they neither render
// as zero nor lose up to half their value. Float-free (ADR-0003): it counts leading fractional zeros exactly,
// like storage's rateScaleFor, except `decimals` is a floor.
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

// Stats is a snapshot of the orchestrator's runtime counters.
// All fields are value types; returning by value gives the
// caller an independent copy that won't change under their feet
// while the orchestrator keeps ticking.
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

// flushContributions emits one ContributionRecord per call to the
// configured sink (if any). Pulled out of refreshPairWindow so the
// hot-path function stays under the gocognit ceiling.
//
// Best-effort: sink failures log at DEBUG and don't propagate. The
// load-bearing operation is the VWAP cache write that happens
// after this returns.
func (o *Orchestrator) flushContributions(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	trades []canonical.Trade,
	tradeUSD map[string]*big.Rat,
) {
	if o.cfg.ContributionSink == nil {
		return
	}
	contributions := aggregate.SourceContributions(trades)
	if len(contributions) == 0 {
		return
	}
	// Walk the POST-filter trade
	// slice and sum per-source USD value from the per-trade map.
	// This matches the contribution population VWAP was computed
	// against; an outlier-dropped trade contributes 0 USD to its
	// source's row instead of double-attributing through the
	// pre-filter total.
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
