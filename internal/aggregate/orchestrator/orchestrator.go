// Package orchestrator drives the aggregation layer's pre-compute
// cycle: on a fixed ticker, for every configured (pair, window)
// combination it fetches the window's trades from Timescale,
// computes VWAP, and writes the result to Redis so API requests
// serve from cache rather than recomputing on every query.
//
// Scope:
//
//   - VWAP per pair over each window ending at the last closed minute
//     (ADR-0015), so the window advances one closed bucket at a time.
//     Three windows are the built-in default (5m, 1h, 24h via
//     [DefaultWindows]); operators override via `[aggregate].windows`
//     in TOML.
//   - Class-filtered single-tier aggregation by default
//     (ClassExchange-only); operators flip
//     `[aggregate].disable_class_filter` to opt out and pull
//     aggregator + oracle classes too.
//   - Stablecoin → fiat proxy mapping (USDT/USDC/PYUSD → USD,
//     EUROC/EUROB → EUR, MXNe → MXN) when
//     `[aggregate].enable_stablecoin_fiat_proxy` is set; the
//     mapping lives in [internal/aggregate/stablecoin] and is
//     applied as a post-fetch pair rewrite before VWAP computes.
//   - Cross-pair triangulation (XLM/USD × USD/EUR = XLM/EUR) via
//     the `Triangulations` field; X2.5 forex-snap rule for
//     chained-fiat per [internal/aggregate/triangulate].
//   - Outlier filtering at fetch time via `OutlierSigmaThreshold`;
//     the math lives in [internal/aggregate/outliers].
//   - Divergence-cache refresh from each Tick via
//     `DivergenceRefresher` (the API's
//     `flags.divergence_warning` reads from the resulting
//     `div:<base>/<quote>` Redis keys).
//   - Multi-factor confidence scoring + ADR-0019 anomaly response
//     (Phase 1 + 2 — z-score / confidence / source-count freeze
//     thresholds via the `Anomaly` + `FreezeWriter` fields; the
//     API binary's `freeze.Looker` reads the markers this
//     publishes).
//
// Out of scope: CAGG refresh stays Timescale-driven (background
// job in migration 0002's `add_continuous_aggregate_policy`
// calls); the orchestrator deliberately does not refresh CAGGs
// itself.
//
// Runtime: one goroutine per window × pair pair-list entry in
// parallel during each tick. Ticks are serialised — if a tick's
// work spans longer than the tick interval, the next tick waits;
// this avoids piling queries on a slow Timescale.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
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
)

// Store is the subset of timescale.Store the orchestrator needs.
// Declared as an interface so tests can substitute a mock without
// pulling up a real Timescale container.
type Store interface {
	TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error)
}

// FXStore is the subset of timescale.Store the X2.5 forex-snap path
// needs. Optional — wired into [Config.FXStore] only when an operator
// runs chained-fiat triangulation. Nil keeps the orchestrator on the
// pre-snap cached-VWAP path for FX legs (the safe default for
// deployments without FX ingestion).
//
// Returns ([timescale.ErrNoFXQuote]) when no FX quote exists at-or-
// before cutoff — caller falls back to cached VWAP and increments
// [obs.AggregatorFXSnapFallbackTotal].
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
	// Expire is used by the freeze path (F-1345) to extend the
	// last-known-good VWAP key's TTL so it outlives the freeze marker
	// instead of expiring out from under a sustained freeze. Returns
	// a BoolCmd whose value is false when the key doesn't exist (no
	// prior bucket to keep alive) — a normal, non-error outcome.
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	// TxPipelined writes a served value together with its provenance
	// (and a composite's meta) in one MULTI/EXEC, so no reader observes
	// one without the other: a direct value clears a prior composite's
	// "triangulated" marker in the same transaction that writes it
	// (W1-flow-price-serve-2).
	TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// FreezeMarker is the side-effect interface the orchestrator uses
// to record an ActionFreeze decision. Production wiring is
// freeze.Writer from internal/aggregate/freeze; declared here as an
// interface so tests can substitute a recorder without spinning up
// a Redis client.
//
// All four methods MUST be idempotent on (asset, quote).
//
// frozenValue is the last-known-good VWAP being frozen on, encoded
// as a fixed-precision decimal string (the orchestrator formats with
// `formatRatFixed(prev, 12)`). Empty string when no prior bucket
// exists. Forwarded to the durable EventSink so freeze_events
// records the frozen-on price; the Redis marker doesn't carry it.
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

// WindowedFreezeMarker is the window-aware half of [FreezeMarker]: the
// same lifecycle calls, told WHICH window's ADR-0019 ladder they are
// writing, reading or retiring.
//
// It exists because the marker is keyed (asset, quote) — its presence is
// the pair-wide `flags.frozen` the API serves — while the lifecycle it
// carries advances per (pair, window), and the windows of a pair freeze
// and release independently. A marker that records a single pair-level
// ladder cannot say whose it is, so a window arriving at the freeze step
// with a cold key — one dropped under [Config.MinUSDVolume] before the
// freeze step, or any window on the first tick after a restart — adopted
// whatever ladder the marker happened to carry: a sibling's FiredAt,
// HoldUntil, ExtensionsUsed and Escalated, on evidence that was never
// about it.
//
// Optional rather than folded into [FreezeMarker]: a writer that cannot
// scope a marker keeps exactly the pre-existing pair-wide behaviour,
// which errs towards holding a freeze rather than dropping one.
// Production wires freeze.Writer, which implements this.
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

	// ReleaseWindow ends `window`'s freeze when this process knows of no
	// other frozen window for the pair: it clears the marker UNLESS the
	// marker itself still records a sibling window's ladder, in which
	// case it only retires `window`'s and reports kept=true. The marker
	// is asked because this process's ladder map cannot see a window it
	// has not evaluated — one under [Config.MinUSDVolume], or any window
	// after a restart — and deleting the marker on that blind spot ended
	// a sibling's freeze, escalated or not.
	ReleaseWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (kept bool, err error)
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

	// TickTimeout is the deadline one [Tick] runs under, inherited by
	// every store, cache and reference call the tick makes — the trades
	// fetch, both FX snap queries (triangulation and the
	// composite-reference evaluator), the freeze record and the
	// divergence refresh. Before it existed a tick ran on the process
	// context, so ONE query that stopped answering (a half-open
	// connection, a lock wait) stalled every price indefinitely (K025).
	//
	// It is a WEDGE GUARD, not a budget: it must never cut a tick that
	// is merely slow. refreshOrder is fixed, so a bound a healthy tick
	// can exceed would starve the same tail pairs on every tick — a
	// pricing outage caused by the guard. Zero defaults to
	// DefaultTickTimeoutIntervals × Interval.
	TickTimeout time.Duration

	// MaxTradesPerWindow caps per-query row count to protect
	// Timescale from a runaway scan on an unexpectedly active
	// pair. Defaults to 10_000.
	MaxTradesPerWindow int

	// EnableStablecoinFiatProxy, when true, expands each fiat-
	// denominated target pair into the direct pair plus one
	// stablecoin-backed source pair per known peg and rewrites the
	// fetched trades through aggregate.ProxyPair before VWAP
	// computes. An operator who configures `XLM/fiat:USD` with
	// this enabled gets a VWAP drawn from XLM/fiat:USD (FX-feed
	// origins), XLM/crypto:USDT, XLM/crypto:USDC, XLM/crypto:DAI,
	// XLM/crypto:PYUSD, XLM/crypto:USDP — all collapsed onto the
	// target pair at the aggregator layer.
	//
	// Default (zero value = false): no expansion — the operator's
	// configured Pairs are fetched verbatim. Eager on-by-default
	// is held back because the expansion issues N+1 TradesInRange
	// calls per (pair, window) and many deployments that only
	// care about XLM/USDT want to opt into that extra IO
	// deliberately.
	//
	// See internal/aggregate/stablecoin.go for the pegged-token
	// map and the "aggregator policy, not decoder policy"
	// rationale (late binding keeps depeg signal visible in the
	// raw trade feed).
	EnableStablecoinFiatProxy bool

	// USDPeggedClassicAssets is the operator's parsed list of
	// classic credit assets (e.g. Circle's
	// `USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN`)
	// they declare as USD-pegged. Exists alongside the abstract-
	// stablecoin map in internal/aggregate/stablecoin.go: that map
	// keys on `crypto:CODE` (USDT/USDC/…) which is the layer most
	// CEX feeds report; classic credits carry full issuer identity
	// and are intentionally NOT in the abstract map. On Stellar
	// mainnet today the dominant USD-denominated DEX pairs are
	// quoted in classic credits, so without this list every
	// XLM/fiat:USD VWAP would be empty even with
	// EnableStablecoinFiatProxy on.
	//
	// Wired by the binary from cfg.Trades.USDPeggedClassicAssets so
	// the operator declares the allow-list in one place and both the
	// indexer (for trades.usd_volume population) and the aggregator
	// (for VWAP source expansion) pick it up. Empty = no classic
	// expansion, abstract-stablecoin map only.
	//
	// Only consulted when EnableStablecoinFiatProxy is true and the
	// target pair's quote is fiat:USD.
	USDPeggedClassicAssets []canonical.Asset

	// USDPeggedSorobanAssets is the operator's resolved allow-list of
	// Soroban Stellar-Asset-Contract (SAC) wrappers that inherit a
	// USD peg transitively from USDPeggedClassicAssets — e.g. the SAC
	// contract that wraps Circle's classic USDC. Each entry is
	// Type=AssetSoroban with ContractID set to the SAC's C-strkey.
	//
	// Unlike USDPeggedClassicAssets there is no dedicated TOML knob
	// for this list: it's derived at the binary boundary by
	// resolving `[supply].sac_wrappers` (SAC contract id →
	// "CODE:ISSUER") against `[trades].usd_pegged_classic_assets`,
	// the SAME two operator-declared inputs
	// internal/storage/timescale.NewUSDVolumeQuoteSpec already
	// combines to recognise a SAC-wrapped peg for trades.usd_volume
	// at insert time (see resolveUSDPeggedSorobanAssets in
	// cmd/stellarindex-aggregator/main.go). A SAC always shares its
	// wrapped classic's 7-decimal scale, so no separate decimals
	// input is needed.
	//
	// Used by [usdQuoteDecimals] to extend the MinUSDVolume floor to
	// directly-configured Soroban-quoted target pairs (e.g.
	// "native/CCW6…" — a SAC-USDC-quoted pair), closing the gap where
	// such pairs served VWAP completely unguarded regardless of
	// window volume. Empty = no Soroban pair gets a recognised USD
	// peg; those pairs fall into [usdQuoteDecimals]'s unvaluable
	// branch (pass-through + WARN + metric, not fail-closed — see
	// dropForMinUSDVolume).
	USDPeggedSorobanAssets []canonical.Asset

	// MinUSDVolume, when > 0, requires a window's total USD volume
	// (post-class, post-outlier) to meet the threshold before its
	// VWAP publishes.
	//
	// Applies to every target pair whose quote leg [usdQuoteDecimals]
	// can resolve to a USD value: fiat:USD directly (every
	// contributing trade originates off-chain at the uniform 10^8
	// quote-decimal convention, so sum/1e8 is exact), an abstract
	// USD-pegged stablecoin ticker (crypto:USDT/USDC/DAI/PYUSD/USDP —
	// same off-chain 10^8 convention), a classic asset on
	// USDPeggedClassicAssets (7-decimal Stellar-classic invariant), or
	// a Soroban SAC wrapper on USDPeggedSorobanAssets (same 7-decimal
	// invariant, transitively). Before 2026-07-10 the floor applied
	// ONLY to fiat:USD-quoted pairs — a directly-configured Soroban-
	// or classic-quoted target pair (e.g. "native/CCW6…", a
	// SAC-USDC-quoted pair) served VWAP unguarded at any volume, so a
	// single dust trade could set the price.
	//
	// R-008 (audit 2026-07-23): the stablecoin-fiat-proxy expansion
	// path (EnableStablecoinFiatProxy) WAS affected, in the opposite
	// direction, and this comment used to claim otherwise. The gate's
	// APPLICABILITY was never in doubt on that path (fetchForTarget
	// rewrites trades onto the fiat:USD target before the gate runs),
	// but its INPUT was: the per-trade USD values are computed against
	// the SOURCE pair's quote (`BASE/crypto:USDT`, …) before the
	// rewrite, and the abstract stablecoin tickers weren't a
	// recognised USD surface — so every proxy-fetched CEX leg counted
	// as $0 and windows carrying real dollar volume were dropped as
	// "below floor". Tier 2 of [usdQuoteDecimals] closes that.
	//
	// Non-USD fiat pairs (fiat:EUR, fiat:GBP, …) remain exempt — the
	// $10k-style threshold is a USD figure and converting a EUR- or
	// GBP-denominated window into USD needs a live FX rate this gate
	// doesn't have (a distinct, still-open question from the
	// Soroban/classic gap above). A quote asset this package can't
	// value in USD by any of the three tiers above (e.g. a pure
	// Soroban/Soroban pair with no declared peg) also stays exempt —
	// see dropForMinUSDVolume's unvaluable branch for why that's a
	// deliberate pass-through rather than fail-closed.
	//
	// Default 0 = filter off. Production deployments stamp 10_000
	// (== $10k in window) per the AggregateConfig default, matching
	// L2.1 in `docs/architecture/launch-readiness-backlog.md`.
	MinUSDVolume float64

	// OutlierSigmaThreshold, when > 0, drops trades whose
	// QuoteAmount/BaseAmount price sits more than sigma robust
	// scales (1.4826·MAD, NOT mean/stdev) from EVERY reference it is
	// scored against — the whole window's median and its time-local
	// neighbourhood's — before VWAP computes
	// (aggregate.FilterOutliersLocal). A lone wild print disagrees
	// with both and is dropped; an agreed regime shift agrees with
	// its neighbours and survives (the 2026-08-28 drift artifact
	// the whole-window filter produced). 0 (zero value) disables the
	// filter — every fetched trade contributes.
	//
	// Applied AFTER class filtering and stablecoin expansion: the
	// fetched-and-rewritten trade set is already homogenised onto
	// the target pair, so the robust statistics are computed over
	// comparable price values rather than across different markets.
	// Windows with fewer than 3 valid prices fall through unchanged
	// (too few samples to form a robust centre).
	//
	// Default value (0) leaves the filter off so a fresh
	// orchestrator behaves identically to its pre-filter
	// predecessor; AggregateConfig in internal/config/config.go
	// stamps a 4.0 default at the binary boundary.
	OutlierSigmaThreshold float64

	// Anomaly, when non-nil, evaluates each fresh VWAP against its
	// previous bucket before publishing. Per ADR-0019:
	//
	//   - ActionAllow → publish normally.
	//   - ActionWarn  → publish; downstream divergence-warning path
	//                   (already handled out-of-band via #205).
	//   - ActionFreeze → DO NOT publish the new bucket; serve the
	//                    previous bucket's last-known-good value
	//                    instead. FreezeWriter writes the marker so
	//                    the API's flags.frozen fires.
	//
	// Nil = anomaly evaluation is off; every fresh VWAP publishes
	// regardless of deviation. Acceptable for early-bring-up
	// deployments where threshold tuning hasn't happened yet;
	// production deployments wire this at the binary boundary.
	Anomaly *anomaly.Checker

	// Triangulations is the operator-configured set of chain pricing
	// entries. After the per-(pair, window) refresh loop runs in
	// each Tick, the orchestrator iterates each chain, reads each
	// leg's freshly-cached VWAP, and prices the target through the
	// graph router (aggregate.BuildEdges → CombineRoutes/CompositeRate,
	// which enumerates + composites the best route), writing the implied
	// target VWAP to its own cache key. Empty (default) = no
	// triangulation. (NOTE: aggregate.Triangulate/TriangulateChain are
	// the old direct-multiply helpers and are NOT on this path — they
	// have no non-test callers; the live math is the router. Corrected
	// 2026-08-03.)
	//
	// Cardinality: each chain contributes len(Windows) cache keys
	// per tick. Operators tune the chain set explicitly — eager
	// triangulation across every fiat × stablecoin combinatorial
	// would blow out cardinality and bandwidth without proportional
	// downstream value.
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

	// FreezeWriter, when non-nil and Anomaly is also non-nil, writes
	// a freeze marker to Redis when Anomaly returns ActionFreeze.
	// The API's freeze.Looker (#226) reads the same key to set
	// flags.frozen=true on /v1/price responses for the affected
	// pair.
	//
	// Nil = freeze action is observed (logged + metric incremented)
	// but no Redis marker is written — loud-but-not-actionable, and a
	// Phase 2 refusal then serves its last value with flags.frozen
	// ABSENT (a stale price presented as fresh). Only acceptable in
	// tests/bring-up: the Phase 2 lifecycle (stepPhase2Freeze) runs
	// regardless of Anomaly, so production deployments must wire
	// FreezeWriter even with Anomaly nil — the aggregator binary now
	// builds it unconditionally (2026-08-22, r1 XLM/GBP incident:
	// Phase-1-off config froze 5m/1h windows with no marker).
	FreezeWriter FreezeMarker

	// DisableClassFilter, when true, suppresses the aggregator's
	// default "ClassExchange trades only" filter and lets every row
	// in the fetched window contribute to VWAP regardless of source
	// class.
	//
	// Default (zero value = false): filter is ON. Rationale lives
	// in internal/sources/external/registry.go — aggregator-class
	// sources (coingecko / coinmarketcap / cryptocompare) are
	// derivatives of other venues' data and mixing them into our
	// VWAP double-counts the upstream; oracle-class sources publish
	// already-aggregated derived prices with their own governance.
	// Both belong in the /v1/sources feed for transparency but not
	// in the computed-VWAP numerator.
	//
	// Inverted phrasing (Disable-X rather than Only-X) is
	// deliberate: a Go bool can't distinguish "left unset" from
	// "explicitly false", so the safer default (filter on) is
	// encoded as the zero value and opt-out is an explicit true.
	// Flip this for historical-parity testing against a prior
	// release that hadn't yet introduced class filtering.
	DisableClassFilter bool

	// Phase2Thresholds tunes the ADR-0019 Phase 2 freeze condition
	// (3-signal AND on confidence + z + source count). Zero-value
	// fields fall back to the [Default*] package constants — an
	// operator with no override gets the documented stop-gap
	// behaviour. Set per-field to tighten or loosen any single
	// signal without restating the others.
	Phase2Thresholds Phase2Thresholds

	// Baselines, when non-nil, is consulted by the per-tick
	// confidence-score step (ADR-0019 §"Multi-factor confidence
	// score"). The orchestrator computes a [confidence.Score] from
	// the freshly-published VWAP + the cached MultiBaseline and
	// writes the result to Redis at `confidence:<base>:<quote>:<window>`.
	//
	// Nil = confidence step is skipped. Production wiring is an
	// adapter around `*timescale.Store.LatestBaseline`. The score
	// requires both a baseline (this field) and a previous-tick
	// VWAP comparator slot (kept internally) — the first tick after
	// startup always skips because there's no return to score yet.
	Baselines BaselineSource

	// FXStore, when non-nil, enables the X2.5 forex-factor snap rule
	// during triangulation. For each FX leg in a chain (a leg whose
	// Base AND Quote are both AssetFiat), the orchestrator queries the
	// most recent FX-source quote at-or-before the bucket-end
	// timestamp, instead of reading the leg's cached VWAP. This is
	// ADR-0018's across-region consistency primitive: every region
	// serving the same closed bucket queries the same hypertable and
	// gets the same FX rate.
	//
	// Nil = the snap rule is off; FX legs use the cached VWAP path
	// (almost-equivalent in steady state but not strictly compliant
	// with ADR-0018 across multi-region partitions). Wired to
	// timescale.Store at the binary boundary; the unit-test path
	// substitutes a mock implementing only [FXStore].
	FXStore FXStore

	// CompositeReference gates the current-bucket composite-reference
	// corroboration of the phase-2 freeze for structurally single-venue
	// targets (2026-08-29; see composite_reference.go). Zero value =
	// off, which is what a Config assembled directly in a test gets.
	CompositeReference CompositeReferenceConfig

	// DivergenceRefresher, when non-nil, is called once per pair
	// per [Tick] to refresh the `div:<base>/<quote>` Redis cache so the
	// API's `flags.divergence_warning` flag has a producer (per
	// ADR-0019 / launch-readiness L2.10 + L2.11). Wired to
	// `internal/divergence.Service` at the aggregator binary
	// boundary; nil preserves the pre-Phase behaviour where the
	// cache stays empty and the flag is always false.
	//
	// Drives off the SHORTEST configured window's VWAP per pair —
	// gives operators ~Interval-fresh divergence detection without
	// hammering the external references on every (pair, window)
	// combination per tick.
	DivergenceRefresher DivergenceRefresher

	// DivergenceMinInterval gates how often [Tick] actually invokes
	// the divergence refresher. Tick still fires every Interval, but
	// the divergence pass is skipped if elapsed since the last
	// successful pass is less than this value. Zero = refresh every
	// tick (legacy behaviour).
	//
	// Rationale (F-0030 follow-up, 2026-05-27): the CMC free tier is
	// 10,000 calls / MONTH. Even with the per-tick batched lookup
	// shipped earlier, refreshing every 30 s × 12 pairs is ~2,880
	// calls/day = ~86,000/month — 8.6 × over cap. The
	// `div:<base>/<quote>` Redis entry has a 5-minute TTL
	// (cachekeys.DivergenceTTL), so a 5-minute refresh interval
	// keeps the cache continuously populated while burning roughly
	// one-tenth the external quota. The divergence warning is an
	// anomaly signal, not a price input — 5-minute detection
	// latency is acceptable per ADR-0019.
	DivergenceMinInterval time.Duration

	// StreamPublisher, when non-nil, is called once per successful
	// closed-bucket VWAP write to fan the event out to API-side SSE
	// subscribers (`/v1/price/stream`). Production wiring is the
	// Redis-pub/sub publisher in `internal/api/streaming/redispub`;
	// the matching API-side subscriber republishes on the in-process
	// streaming.Hub so SSE clients receive the event. Best-effort:
	// publish errors log + increment a metric but never block the
	// tick (the VWAP cache write itself is the source of truth).
	//
	// Nil = no fan-out. Leaves `/v1/price/stream` with no producer,
	// matching the pre-launch state where `s.hub == nil` returns 503.
	StreamPublisher StreamPublisher

	// ContributionSink, when non-nil, receives the per-source
	// breakdown of every successful VWAP compute. Production wires
	// `internal/storage/timescale.PriceSourceContributionSink` so
	// the explorer source-contribution donut on every price card
	// reads from a postgres-resident history rather than recomputing
	// at request time. Best-effort — sink failures log + continue.
	//
	// See migrations/0026 + Phase 2 of
	// docs/architecture/explorer-implementation-plan.md.
	ContributionSink ContributionSink

	// DecimalsLookup, when non-nil, is consulted immediately after each
	// window's raw VWAP computes to correct for a non-7-decimal leg —
	// see aggregate.AdjustPrice / docs/operations/runbooks/
	// dex-nonstandard-decimals.md. This is the ORCHESTRATOR's own
	// published VWAP (Redis-cached, feeds /v1/price's Redis fallback,
	// the confidence/anomaly/freeze chain, the contribution sink, AND
	// the `/v1/price/stream` SSE fan-out) — none of those downstream
	// consumers pass through internal/api/v1's per-endpoint
	// declineIfNonstandardDecimals guard, so this is the one place a
	// confirmed non-7-decimals asset was still silently leaking a wrong
	// price to real subscribers even after that guard shipped
	// (2026-07-09).
	//
	// Nil (the default — every deployment before this field existed,
	// and every existing test) means [aggregate.ResolveDecimals] always
	// returns [aggregate.StandardDecimals] for both legs, so the
	// adjustment factor is exactly 1 and refreshPairWindow's published
	// VWAP is byte-identical to pre-normalization behaviour. Production
	// wiring is a small in-process cache over `nonstandard_decimals_assets`
	// (migration 0093) — see cmd/stellarindex-aggregator/decimals_cache.go.
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

	// SourceUSDVolume is the per-source USD-volume breakdown
	// computed from the POST-filter trade slice — class filter +
	// outlier filter have already run. Keys are the same
	// `Source` values that appear in Contributions. F-1242
	// (codex audit-2026-05-12): the prior shape was a pre-filter
	// USDVolumeTotal split by post-filter weights, which
	// over-attributed dollars when outliers dropped — non-NULL
	// rows looked authoritative while drifting from the
	// contribution set actually published. The sink now reads
	// SourceUSDVolume directly so persisted `volume_usd` matches
	// what VWAP actually saw.
	SourceUSDVolume map[string]float64
}

// DivergenceRefresher is the seam the orchestrator uses to keep the
// `div:<base>/<quote>` Redis cache populated. Production impl is
// [internal/divergence.Service]; tests substitute a fake that records
// invocations without making network calls.
//
// `ourPrice` is the per-pair shortest-window VWAP the orchestrator
// just computed; `observedAt` is the Tick's wall-clock time. The
// implementation is responsible for fetching external references,
// computing divergence percent, and writing the cache entry.
type DivergenceRefresher interface {
	RefreshPair(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time) error
}

// StreamPublisher is the seam the orchestrator uses to fan out
// closed-bucket events. Production impl is
// [internal/api/streaming/redispub.Publisher] (Redis PUBLISH); the
// API binary's matching subscriber (PR 2 of L3.9) republishes the
// event on its in-process [internal/api/streaming.Hub] so SSE
// subscribers on `/v1/price/stream` get fed.
//
// Called once per (pair, window) on every successful VWAP cache
// write — same call site as the freeze writer / confidence cache
// write, just on the publish side. Best-effort: a publish error
// logs + increments a metric but never blocks the next tick (the
// closed-bucket row is durable via the VWAP cache; the stream is
// enrichment, not a source-of-truth).
//
// Nil = no fan-out. Acceptable when no API binary is subscribed
// (e.g. local dev). Tests substitute a fake that records
// invocations.
type StreamPublisher interface {
	PublishClosedBucket(ctx context.Context, pair canonical.Pair, window time.Duration, valueDecimal string, observedAt time.Time) error
}

// DefaultWindows is the built-in window set — three buckets
// covering hot (5m), warm (1h), and cold (24h) consumer needs.
var DefaultWindows = []time.Duration{
	5 * time.Minute,
	1 * time.Hour,
	24 * time.Hour,
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
	// replaying tick keeps the leg out of triangulation (MNY-22).
	frozen       bool
	compositeRef *compositeReference
}

// publishedBucket is what a published closed bucket wrote, kept so a
// replaying tick republishes the scored value rather than a re-fetch.
type publishedBucket struct {
	value  string
	score  confidence.Score
	confOK bool
	edge   aggregate.Quote
	leg    legRef
}

// DefaultInterval is the built-in tick cadence. 30s matches the
// Redis price-key TTL of 60s with headroom for missed ticks;
// higher-frequency aggregation is a follow-up once the API's
// consumer pattern stabilises.
const DefaultInterval = 30 * time.Second

// DefaultTickTimeoutIntervals sizes the default [Config.TickTimeout] as
// a multiple of the tick cadence: 4 × 30s = 120s. That is the
// `stellarindex_api_price_stale` threshold, which is what makes it a
// safe wedge guard rather than a guess at a budget: a tick still running
// at 120s is already serving prices the alert calls stale, so cutting it
// costs nothing the alert does not already say, while a wedged call is
// released inside the alert's 5-minute `for:` instead of never. Ticks
// have no duration metric yet, so a tighter bound (one Interval was
// suggested) could not be shown safe for the slowest healthy tick.
const DefaultTickTimeoutIntervals = 4

// DefaultMaxTradesPerWindow caps per-query scan size to bound a single
// refresh's Timescale cost. 10,000 rows is comfortably wider than the
// 5m default window at network-wide trade rates, but a single liquid
// pair (e.g. XLM/USDC on a busy day) can clear 10,000 trades well
// inside the 1h and 24h windows — when it does, TradesInRange returns
// the NEWEST 10,000 (F-1319 fixed the prior oldest-N truncation) and
// the orchestrator emits AggregatorWindowTruncatedTotal so operators
// can see the VWAP is over a partial slice. Raise the cap (or move the
// large windows to a SQL-side aggregate) if that counter fires
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

	// prevVWAPs holds the last published VWAP per (pair, window) for
	// the anomaly evaluator's comparison input. Bounded by
	// len(Pairs) × len(Windows) — small. Reset to nil on
	// ActionFreeze (we publish-or-not but don't update the
	// comparator slot during a freeze, so the next bucket compares
	// against the same prev).
	//
	// Tick is serialised (the ticker drops events that arrive while
	// a previous Tick is still running), and refreshPairWindow runs
	// sequentially within Tick — so this map needs no separate lock.
	//
	// WARNING (L4): this map is read+written LOCK-FREE, and that is safe
	// ONLY because a single Tick runs at a time and its per-pair loop is
	// strictly sequential. It is the canonical example the sibling per-Tick
	// maps (frozenThisTick, tickEdgeQuotes, lastComposites, freezeStates)
	// point back to. If you EVER parallelise the per-(pair, window) refresh
	// loop — or add any other concurrent writer — you MUST guard every
	// access to this map (and each of those siblings) with o.mu FIRST. Do
	// not do one without the other.
	prevVWAPs map[string]*big.Rat

	// frozenPrevVWAPs is the SHADOW comparator for pairs whose bucket was
	// REFUSED by the freeze lifecycle (2026-08-24, the XLM/GBP ratchet +
	// unscored-stall incidents). prevVWAPs deliberately does not advance
	// on a refused bucket — but scoring the NEXT frozen bucket against
	// that pinned pre-freeze value makes z measure TOTAL DRIFT SINCE
	// FREEZE (divided by a per-minute MAD), so ADR-0019's auto-unfreeze
	// (z < 3 twice) is only reachable if the market RETURNS to the
	// freeze-time price; any real move ratchets the ladder to escalation
	// instead (observed: z=87 ≈ 8% drift). Worse, prevVWAPs is in-memory:
	// after a restart a frozen pair has NO prev at all and every bucket
	// is UNSCORED — which can neither fire nor release, stalling the
	// freeze forever (observed live as reason "phase2:unscored"). The
	// shadow advances with each refused bucket's FRESH computed VWAP, so
	// a frozen pair is scored on its per-tick return — "is the market
	// calm NOW", the condition the ADR's auto-unfreeze describes.
	// Cleared on publish/release. Same single-Tick-at-a-time invariant
	// as prevVWAPs.
	frozenPrevVWAPs map[string]*big.Rat

	// lastWriteAt tracks the wall-clock timestamp of the most recent
	// successful VWAP cache-write per pair, keyed by `pair.String()` —
	// base AND quote, so one quote's publishes cannot vouch for
	// another's (F067). Written only through `recordPairWrite`: by both
	// writers of the served VWAP key (refreshPairWindow's direct publish
	// and publishComposite's composite publish) and by the first-sighting
	// seed in `emitStalenessGauges`. Used
	// by `emitStalenessGauges` at end-of-Tick to drive the
	// `stellarindex_api_price_stale` alert (F-1306, codex audit-2026-05-13).
	// Bounded by len(cfg.Pairs) + len(cfg.Triangulations) — both small
	// operator-curated lists (a chain target need not be a configured
	// pair; its stamp is then recorded but never emitted, because the
	// gauge iterates cfg.Pairs only), so gauge cardinality is unchanged
	// and fits well inside Prometheus's per-metric comfort zone. Same
	// single-Tick-at-a-time invariant as prevVWAPs, so no lock needed.
	lastWriteAt map[string]time.Time

	// lastDivergenceRefreshAt is the wall-clock time of the most
	// recent successful refreshDivergenceAll pass. Read +
	// updated only inside [Tick] (single-runner invariant), so no
	// lock needed. Zero value means "never refreshed" — the first
	// tick after startup unconditionally runs the pass.
	lastDivergenceRefreshAt time.Time

	// frozenThisTick holds the `<pair>:<window>` stateKeys this tick
	// refused to publish because Phase 1 or Phase 2 froze them. The
	// triangulation pass reads it so a chain does not silently
	// re-publish a frozen leg's last-known-good value as a fresh
	// derived price (MNY-22) — see [Orchestrator.legPriceFromCache].
	//
	// Rebuilt at the top of every [Tick], and written only from the
	// freeze step — so it is NOT the whole answer to "is this leg
	// frozen". A frozen window that returns early this tick (empty,
	// under the USD-volume floor) never re-enters it, while the hold's
	// TTL keeps its LKG in cache for tens of minutes;
	// [Orchestrator.frozenLeg] therefore also reads the live ladder in
	// freezeStates. Same single-Tick-at-a-time
	// invariant as prevVWAPs (refreshPairWindow and triangulateAll run
	// sequentially inside one Tick), so no lock is needed.
	//
	// WARNING (L4): LOCK-FREE ONLY because Tick is strictly sequential. It
	// is WRITTEN in the per-pair loop (markFrozenThisTick) and READ in the
	// triangulation pass (frozenLeg); parallelising the refresh without
	// adding o.mu here would race those two and could silently launder a
	// frozen leg into a derived price. See prevVWAPs.
	frozenThisTick map[string]struct{}

	// tickNow is the CURRENT tick's `now` — the injected clock, read once
	// at the top of [Tick]. publishComposite stamps the pair-level write
	// clock with it, so the composite writer, the direct writer and
	// emitStalenessGauges all judge one instant rather than the composite
	// stamping triangulateAll's own wall-clock read (F067). Zero outside
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

	// tickEdgeQuotes accumulates, per window, the priced-pair VWAPs of
	// the CURRENT tick as router edge inputs (aggregate.Quote). The
	// per-pair refresh loop appends one entry per successfully-published
	// (pair, window) — with the pair's exact VWAP and a confidence
	// derived from its existing quality signals — and the triangulation
	// pass that runs afterwards reads it to build the cross-rate graph
	// for the tick. Frozen / dropped / empty windows contribute no edge,
	// which is exactly how the min_usd_volume gate keeps a dust pair from
	// setting a confident cross (INV-11). Rebuilt at the top of every
	// [Tick]; same single-Tick-at-a-time invariant as prevVWAPs, so no
	// lock is needed.
	//
	// WARNING (L4): LOCK-FREE ONLY because Tick is strictly sequential —
	// APPENDED by the per-pair loop, READ by the triangulation pass, both
	// within one Tick. Parallelising refresh REQUIRES o.mu around every
	// access here first (a concurrent append is a data race). See prevVWAPs.
	tickEdgeQuotes map[time.Duration][]aggregate.Quote

	// lastComposites holds the most recent composite (triangulated)
	// price the chain pass published per (target pair, window), so the
	// NEXT tick's confidence step can compare a freshly-computed direct
	// VWAP against an independently-routed opinion of the same pair.
	// See triangulate_corroborate.go for why the comparison is one tick
	// behind and why it feeds confidence but never the freeze's
	// source-count leg. Same single-Tick-at-a-time invariant as
	// prevVWAPs, so no lock is needed.
	//
	// WARNING (L4): LOCK-FREE ONLY because Tick is strictly sequential (it
	// is written by this tick's chain pass and read by the next tick's
	// confidence/freeze step, never concurrently). Parallelising refresh
	// REQUIRES o.mu around every access here first. See prevVWAPs.
	lastComposites map[string]compositeSample

	// tickLegRefs holds, per window, the CURRENT tick's confidently-
	// published VWAP + distinct venue count per pair, keyed by
	// pair.String() — the priced legs the composite-reference evaluator
	// multiplies (composite_reference.go). Rebuilt at the top of every
	// Tick; written by refreshPairWindow at the publish point and read
	// by a LATER pair's refresh in the same loop (refreshOrder puts the
	// legs first). Same single-Tick-at-a-time invariant as prevVWAPs.
	//
	// WARNING (L4): LOCK-FREE ONLY because Tick is strictly sequential.
	// Parallelising refresh REQUIRES o.mu around every access here first.
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

	// freezeStates holds the ADR-0019 freeze lifecycle state per
	// `<pair>:<window>` stateKey — how long the current freeze must
	// hold, how far up the 4-extension ladder it has climbed, whether
	// it has escalated, and how many consecutive buckets have met the
	// auto-unfreeze condition (see internal/aggregate/freeze.Policy).
	//
	// An entry is present-but-inactive (`freeze.State{}`) once a key
	// has been evaluated at least once; that is what stops the
	// hydrate-from-Redis path below from re-running every tick for
	// pairs that are simply healthy. Same single-Tick-at-a-time
	// invariant as prevVWAPs, so no lock is needed.
	//
	// In-memory is the WORKING copy, not the authority: on the first
	// evaluation after a restart the ladder is re-hydrated from the
	// Redis marker, so a deploy mid-freeze does not silently restart
	// the 2-hour escalation clock.
	//
	// WARNING (L4): LOCK-FREE ONLY because Tick is strictly sequential.
	// Parallelising the per-(pair, window) refresh — which both reads and
	// mutates this ladder state — REQUIRES o.mu around every access here
	// first. See prevVWAPs.
	freezeStates map[string]freeze.State

	// windowedFreeze is [Config.FreezeWriter] when it can scope a
	// marker's ladder to the window that owns it
	// ([WindowedFreezeMarker]), nil otherwise. Resolved once in [New]
	// rather than type-asserted per tick.
	windowedFreeze WindowedFreezeMarker

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
	if len(cfg.Windows) == 0 {
		cfg.Windows = DefaultWindows
	}
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
		store:           store,
		cache:           cache,
		cfg:             cfg,
		logger:          logger,
		prevVWAPs:       make(map[string]*big.Rat, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		frozenPrevVWAPs: make(map[string]*big.Rat),
		lastWriteAt:     make(map[string]time.Time, len(cfg.Pairs)),
		lastComposites:  make(map[string]compositeSample, len(cfg.Triangulations)*max(len(cfg.Windows), 1)),
		freezeStates:    make(map[string]freeze.State, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		decidedBuckets:  make(map[string]decidedBucket, len(cfg.Pairs)*max(len(cfg.Windows), 1)),
		refreshOrder:    refreshOrder(cfg),
		clock:           time.Now,
	}
	// A freeze writer that records which window owns each ladder lets
	// every window rehydrate ITS OWN freeze on a cold key instead of a
	// sibling's — see [WindowedFreezeMarker].
	if windowed, ok := cfg.FreezeWriter.(WindowedFreezeMarker); ok {
		o.windowedFreeze = windowed
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

	// F-1306 (codex audit-2026-05-13): emit per-asset staleness so the
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

// recordPairWrite stamps a successful VWAP publish for `pair`. The key
// is the FULL pair (base and quote): a base-only key let every publish
// of XLM/USD reset the one timestamp a dead XLM/GBP was judged by, so
// the only serving-freshness alert read fresh through a per-quote
// outage (F067).
//
// It is the ONLY writer of lastWriteAt, and the served VWAP key has two
// writers that both come through here: the direct publish in
// refreshPairWindow and the composite publish in publishComposite. A
// pair served only through a triangulation chain publishes every tick
// without ever reaching the direct writer, so stamping from one of them
// alone reads such a pair as dead while it serves.
//
// A zero `at` (publishComposite driven outside a Tick, where tickNow is
// unset) records nothing. That is the fail-safe direction: an unstamped
// pair's gauge keeps climbing; it is never vouched fresh by no clock.
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

// emitStalenessGauges sets `stellarindex_price_staleness_seconds` for
// every configured base asset to the age of its STALEST configured
// quote: max over the asset's pairs of `now - lastWriteAt[pair]`. The
// gauge carries one `asset` label, and the alert on it is the only
// serving-freshness alert, so the value has to be the worst pair — a
// freshest-pair (or shared-key) reading stays at 0 while one quote
// serves nothing (F067).
//
// Pairs that have never written carry the wall-clock age since the
// aggregator started (orchestrator construction time would be cleaner
// but the orchestrator doesn't currently track its own birthday — the
// "no writes yet" branch falls back to `now` so a fresh aggregator
// shows ~0 staleness on the first tick and then climbs if it never
// produces a write, which matches the alert intent).
//
// F-1308 (codex audit-2026-05-13): the gauge label has to match the
// canonical asset_id the customer queries with. `/v1/price?asset=native`
// goes through the priceFallback path for XLM because the aggregator's
// configured pair is `crypto:XLM/fiat:USD` (matching the oracle source's
// global-ticker form) — the public surface and the internal pair-key
// disagree. Emit under BOTH forms when the pair maps to a known alias
// pair (the same translation list `internal/api/v1/changes.go::
// aliasEntityIDs` already documents).
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

	// XLM appears in two canonical forms across the codebase:
	// `native` (per-network) and `crypto:XLM` (global ticker).
	// Customers query with `native` via /v1/price; oracles publish
	// `crypto:XLM`. For ONE quote the customer's freshness is the
	// freshest of the two forms — if EITHER has just been written, the
	// API will resolve the lookup (pairLastWrite). Both forms fold into
	// one entry here and both labels are set from it, so the
	// api_price_stale alert isn't order-dependent on cfg.Pairs
	// iteration. Pre-fix, the last pair iterated overwrote the other
	// label via a one-way mirror; iteration order decided whether the
	// alert was "always fresh" or "always stale".
	worst := make(map[string]float64, len(o.cfg.Pairs))
	for _, pair := range o.cfg.Pairs {
		asset := pair.Base.String()
		if asset == stalenessXLMTicker {
			asset = stalenessXLMNative
		}
		stale := now.Sub(o.pairLastWrite(pair)).Seconds()
		if cur, seen := worst[asset]; !seen || stale > cur {
			worst[asset] = stale
		}
	}
	for asset, stale := range worst {
		obs.PriceStalenessSeconds.WithLabelValues(asset).Set(stale)
		if asset == stalenessXLMNative {
			obs.PriceStalenessSeconds.WithLabelValues(stalenessXLMTicker).Set(stale)
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
	pub, err := o.decideBucket(ctx, pair, window, bucketEnd, now)
	if err != nil {
		// Undecided: the next tick retries this bucket from scratch.
		return err
	}
	_, frozen := o.frozenThisTick[frozenTickKey(pair, window)]
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
	// `_` here is the pre-filter USD total. F-1260 (codex audit-
	// 2026-05-12) moved the MinUSDVolume gate to a survivor-only sum
	// computed below from `tradeUSD`, so the pre-filter scalar isn't
	// the gate input anymore. Kept on the return value for backwards
	// compatibility with future callers + lint readability.
	trades, _, tradeUSD, err := o.fetchForTarget(ctx, pair, from, bucketEnd)
	if err != nil {
		return nil, fmt.Errorf("fetch %s %v: %w", pair.String(), window, err)
	}
	preFilter := len(trades)
	if !o.cfg.DisableClassFilter {
		trades = filterForVWAP(trades)
		if dropped := preFilter - len(trades); dropped > 0 {
			// `pair` is the CONFIGURED target pair (bounded: only
			// o.cfg.Pairs entries reach here) — the 2026-08-14
			// outlier_storm needed ad-hoc SQL to attribute a
			// single-issuer SDEX token farm because drops carried
			// no pair.
			obs.AggregatorDroppedTradesTotal.WithLabelValues("class", pair.String()).Add(float64(dropped))
		}
	}
	// Venue-level view of the set the outlier filter is handed: the
	// outlier_storm alert reads per-venue DISAGREEMENT from this, not
	// the trim re-count (2026-08-28).
	recordWindowStage(pair, window, "fetched", preFilter)
	recordWindowStage(pair, window, "class", len(trades))
	o.recordVenueVWAPs(pair, window, trades)
	if o.cfg.OutlierSigmaThreshold > 0 {
		preOutlier := len(trades)
		// Time-local trimming: a print is dropped only when it
		// disagrees with the whole window AND its neighbourhood, so an
		// agreed regime shift survives while a lone wild print does not
		// (see aggregate.FilterOutliersLocal for the 2026-08-28 drift
		// artifact this replaces the whole-window filter for).
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
	recordWindowStage(pair, window, "outlier", len(trades))
	if len(trades) == 0 {
		o.mu.Lock()
		o.emptyWindows++
		o.mu.Unlock()
		obs.AggregatorEmptyWindowsTotal.Inc()
		return nil, nil
	}

	// F-1260 (codex audit-2026-05-12): sum USD across the SURVIVOR
	// slice, not the pre-filter total returned by fetchForTarget.
	// Without this, windows that get gutted by class/outlier filters
	// can still publish above MinUSDVolume on volume that never made
	// it into the VWAP — the gate is supposed to keep thin survivor
	// sets out, so the input it evaluates must be the survivor set.
	survivorUSD := survivorUSDVolume(trades, tradeUSD)
	if o.dropForMinUSDVolume(pair, trades, survivorUSD) {
		return nil, nil
	}

	vwap, err := o.computeNormalizedVWAP(trades, pair)
	if err != nil {
		if errors.Is(err, aggregate.ErrNoTrades) {
			o.mu.Lock()
			o.emptyWindows++
			o.mu.Unlock()
			obs.AggregatorEmptyWindowsTotal.Inc()
			return nil, nil
		}
		return nil, fmt.Errorf("vwap %s %v: %w", pair.String(), window, err)
	}

	o.flushContributions(ctx, pair, window, trades, tradeUSD)

	// Phase 1 anomaly evaluation BEFORE cache write — class-deviation
	// + source-count threshold (the L2.4 stop-gap). On freeze we
	// keep the previous bucket's value in cache (don't overwrite)
	// and emit a freeze marker so flags.frozen=true on the next read.
	// evaluateAndMaybeFreeze stands down while THIS window already holds
	// an active freeze (W3-freeze-3), so the Phase 2 lifecycle below stays
	// the sole release authority once frozen.
	stateKey := pair.String() + ":" + window.String()
	if action, ok := o.evaluateAndMaybeFreeze(ctx, pair, window, vwap, trades, stateKey, now); !ok {
		_ = action
		// Freeze: evaluateAndMaybeFreeze has already refreshed the LKG
		// VWAP key's TTL (F-1345). Skip the cache write so the prior
		// bucket's value keeps serving.
		return nil, nil
	}

	// Phase 2 (ADR-0019): compute confidence, then advance the freeze
	// LIFECYCLE with it. Both happen BEFORE the VWAP cache write so a
	// freeze leaves the prior bucket's value intact in cache — same
	// semantic as Phase 1.
	//
	// The lifecycle step runs unconditionally, NOT only when the
	// 3-signal AND fires. That is the whole point: a pair inside its
	// ADR-0019 hold stays frozen through buckets the AND does not fire
	// for, and is released only by the ADR's auto-unfreeze condition
	// (two consecutive healthy buckets, once the initial hold has been
	// served). It also runs when the bucket could not be scored at all
	// — an unscored bucket must not release a live freeze by default.
	prevForConfidence := o.prevVWAPs[stateKey]
	if shadow, frozen := o.frozenPrevVWAPs[stateKey]; frozen {
		// Mid-freeze: score this bucket against the PREVIOUS refused
		// bucket's fresh VWAP (per-tick return), not the pinned
		// pre-freeze baseline — see frozenPrevVWAPs.
		prevForConfidence = shadow
	}
	// Composite-reference corroboration (2026-08-29): for an allow-listed
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
	conf, confOK := o.computeConfidence(ctx, pair, window, vwap, prevForConfidence, trades)
	// The freeze's source_count leg (ADR-0019 3-signal AND) reads the
	// INDEPENDENCE signal, not just the direct trade sources: a pair
	// reproduced by ≥2 mutually-agreeing, confidence-gated router routes
	// is no longer single-source, so a thin FX cross stops false-firing
	// the freeze (see [Orchestrator.effectiveSourceCount]). The
	// confidence Inputs.SourceCount stays the raw trade count — only the
	// freeze leg widens. The composite reference never touches either
	// count: it can only change the VERDICT (compositeRef).
	if o.stepPhase2Freeze(ctx, pair, window, stateKey, now,
		conf, confOK, o.effectiveSourceCount(pair, window, trades), prevForConfidence, vwap, compositeRef) {
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
		value:  formatRatFixed(vwap, 12),
		score:  conf.Score,
		confOK: confOK,
		// Only a published bucket becomes a router edge / reference leg:
		// frozen, dropped, empty and below-floor buckets returned above,
		// which is how a dust pair stays out of the cross-rate graph
		// (INV-11).
		edge: newEdgeQuote(pair, vwap, conf, confOK, trades),
		leg:  o.newLegRef(pair, vwap, trades),
	}
	if err := o.publishDirect(ctx, pair, window, pub, bucketEnd, now); err != nil {
		return nil, err
	}

	// Update the prev-VWAP comparator slot ONLY on successful
	// publish. Frozen buckets do not advance THIS slot — but they do
	// advance frozenPrevVWAPs above, which is what mid-freeze scoring
	// compares against; keeping the pinned value here as the sole
	// comparator was the auto-unfreeze ratchet (see frozenPrevVWAPs).
	o.prevVWAPs[stateKey] = vwap
	return pub, nil
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

// serveDirect writes a direct VWAP to the pair's served key and clears
// any "triangulated" provenance a prior composite left there, in one
// MULTI/EXEC: the API serves the Redis fallback only under that marker,
// so a direct (possibly thin, single-source) value must never be
// readable beneath it (W1-flow-price-serve-2). Then it streams the
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
	if _, err := o.cache.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, provKey.String())
		p.Set(ctx, key.String(), pub.value, cachekeys.VWAPTTL(window))
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

	// Pair-level write clock for `stellarindex_price_staleness_seconds`
	// (F-1306).
	o.recordPairWrite(pair, now)
	o.streamBucketOnce(ctx, pair, window, pub.value, bucketEnd)
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

// streamBucketOnce publishes a served closed bucket to the stream the
// first time any writer serves it. The SSE contract (ADR-0015, openapi
// price/stream) promises byte-identical payloads across subscribers and
// regions on the same (asset, quote, window), so the event is stamped
// with the bucket it covers, not the tick's jittered clock, and a
// replaying tick does not re-emit it.
func (o *Orchestrator) streamBucketOnce(
	ctx context.Context, pair canonical.Pair, window time.Duration, value string, bucketEnd time.Time,
) {
	k := pair.String() + ":" + window.String()
	if last, ok := o.streamedBuckets[k]; ok && last.Equal(bucketEnd) {
		return
	}
	if o.streamedBuckets == nil {
		o.streamedBuckets = make(map[string]time.Time)
	}
	o.streamedBuckets[k] = bucketEnd
	o.publishToStream(ctx, pair, window, value, bucketEnd)
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

// computeNormalizedVWAP computes VWAP over trades and applies the
// dex-nonstandard-decimals forward normalization in the same step:
// aggregate.VWAP sums raw smallest-unit trades.QuoteAmount/
// trades.BaseAmount with no per-asset decimals adjustment, which is
// correct only when both legs of pair share a decimals scale. A nil
// o.cfg.DecimalsLookup (or a pair with no confirmed non-7-decimals leg)
// resolves both sides to aggregate.StandardDecimals, making the
// normalization an exact no-op — see aggregate.AdjustPrice /
// docs/operations/runbooks/dex-nonstandard-decimals.md. Doing both here
// (rather than a separate call site in refreshPairWindow) means every
// downstream consumer of the returned value sees the SAME corrected
// number, and keeps refreshPairWindow under the funlen ceiling.
func (o *Orchestrator) computeNormalizedVWAP(trades []canonical.Trade, pair canonical.Pair) (*big.Rat, error) {
	// Scale-normalize BEFORE the weighted sum: this pair's window can mix
	// on-chain (7dp) and CEX (8dp) trades (native vs crypto:XLM spellings
	// resolve to the same aggregate.Pair, and triangulation legs draw from
	// both tiers), and the raw Σquote/Σbase mean over-weights the
	// finer-scaled source ~10× per decimal (CS-040) — see
	// aggregate.NormalizeAmountScale. This is the same correction
	// internal/api/v1's price_tip.go and ohlc_fiat_combine.go already apply
	// on their VWAP paths; a single-scale window (today's common case) is
	// returned byte-identical.
	trades = aggregate.NormalizeAmountScale(trades, amountScaleDecimalsFor)

	vwap, err := aggregate.VWAP(trades)
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

// markFrozenThisTick records that (pair, window) was refused
// publication by a freeze on this tick, so
// [Orchestrator.legPriceFromCache] can refuse to feed its
// last-known-good value into a triangulated chain (MNY-22).
//
// Called from the freeze paths themselves — [Orchestrator.markPhase2Freeze]
// and [Orchestrator.evaluateAndMaybeFreeze] — rather than from their
// caller, so a future third freeze path cannot forget to record and
// silently reopen the laundering route.
//
// Tolerates a nil map so an Orchestrator driven by a direct
// refreshPairWindow call in a test (rather than through Tick) behaves
// as "nothing frozen" instead of panicking.
func (o *Orchestrator) markFrozenThisTick(pair canonical.Pair, window time.Duration) {
	if o.frozenThisTick == nil {
		o.frozenThisTick = make(map[string]struct{})
	}
	o.frozenThisTick[frozenTickKey(pair, window)] = struct{}{}
}

// frozenLeg reports whether (pair, window) is frozen as far as the
// triangulation pass is concerned: refused publication earlier in this
// tick, OR still inside a freeze this process is holding for it.
//
// The second arm is not redundant with the first. frozenThisTick is
// rebuilt every tick and written only from the freeze step, and
// refreshPairWindow returns BEFORE that step when the window is empty,
// under [Config.MinUSDVolume], or has no VWAP. A pair that froze a tick
// ago and whose next bucket is empty — the ordinary aftermath of a thin
// venue being manipulated — is therefore in nobody's per-tick set, while
// its hold runs for tens of minutes and its last-known-good value is
// still in Redis because the freeze deliberately kept it there
// ([Orchestrator.keepFrozenVWAPAlive]). Reading that value as a leg
// laundered it into a derived price with no frozen flag (MNY-22, one tick
// later than the case the per-tick set closes).
//
// Bounded by the hold plus the marker grace, which is exactly how long the
// freeze keeps the marker and the LKG alive. An in-memory ladder that is
// never evaluated is never advanced and never released either, so an
// unbounded read would refuse every chain through the pair for as long as
// its window stayed empty — long after there was any LKG left to launder.
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

// keepFrozenVWAPAlive extends the TTL of the last-known-good VWAP
// key for (pair, window) so it survives for at least as long as the
// freeze marker (F-1345, G13-03).
//
// Why: a freeze skips the VWAP cache write, so the LKG value keeps
// the TTL it was written with — equal to the window. A freeze that
// persists past one window-worth of seconds would let the LKG expire
// out of Redis while flags.frozen is still set, and the API would
// then read frozen=true with no value to serve.
//
// ttl MUST be the marker's own TTL, which since the ADR-0019
// lifecycle landed is "remaining hold + silence grace" and can reach
// ~35 minutes — not the flat [cachekeys.FreezeTTL] it used to be.
// Passing the smaller constant here would recreate exactly the bug
// F-1345 fixed, one order of magnitude later in the hold: the LKG
// would evaporate 5 minutes into a 30-minute freeze. ttl <= 0 falls
// back to FreezeTTL so a caller with no lifecycle of its own (the
// composite-refusal path) keeps the original behaviour.
//
// Best-effort + nil-safe on a missing key: Expire returns
// BoolCmd=false (not an error) when the key doesn't exist — the
// first-tick-freeze-on-this-pair case where there's no prior bucket
// to keep alive. A transient Redis error is logged at debug and
// swallowed; the freeze marker write is the load-bearing operation
// and already happened upstream.
func (o *Orchestrator) keepFrozenVWAPAlive(ctx context.Context, pair canonical.Pair, window, ttl time.Duration) {
	if ttl <= 0 {
		ttl = cachekeys.FreezeTTL
	}
	key := cachekeys.VWAP(pair.Base, pair.Quote, window)
	if err := o.cache.Expire(ctx, key.String(), ttl).Err(); err != nil {
		o.logger.Debug("freeze: LKG VWAP TTL refresh failed",
			"pair", pair.String(), "window", window, "key", key, "err", err)
	}
}

// publishToStream fans the closed-bucket event out to the
// configured StreamPublisher (Redis pub/sub in production). Pure
// best-effort: never returns an error — failures log + increment
// the per-outcome counter. The VWAP cache write upstream is the
// source of truth; the stream is enrichment for SSE subscribers.
func (o *Orchestrator) publishToStream(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	value string,
	observedAt time.Time,
) {
	if o.cfg.StreamPublisher == nil {
		return
	}
	if err := o.cfg.StreamPublisher.PublishClosedBucket(ctx, pair, window, value, observedAt); err != nil {
		obs.AggregatorStreamPublishTotal.WithLabelValues("error").Inc()
		o.logger.Warn("stream publish failed",
			"pair", pair.String(), "window", window, "err", err)
		return
	}
	obs.AggregatorStreamPublishTotal.WithLabelValues("ok").Inc()
}

// evaluateAndMaybeFreeze runs the anomaly check on a fresh VWAP
// and writes a freeze marker when the decision says so. Returns
// (decision, ok=true) for Allow / Warn — caller proceeds to the
// cache write — and (decision, ok=false) for Freeze — caller skips
// the cache write so the previous bucket's value continues to
// serve.
//
// When o.cfg.Anomaly is nil, the evaluator is off — every fresh
// VWAP returns Allow without computing a decision. Acceptable for
// early bring-up; production deployments wire Anomaly + FreezeWriter
// at the binary boundary.
func (o *Orchestrator) evaluateAndMaybeFreeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	currVWAP *big.Rat,
	trades []canonical.Trade,
	stateKey string,
	now time.Time,
) (anomaly.Action, bool) {
	if o.cfg.Anomaly == nil {
		return anomaly.ActionAllow, true
	}

	// W3-freeze-3: once THIS window holds an active freeze, Phase 1 stands
	// down and the ADR-0019 lifecycle (driven by the Phase 2 confidence
	// step in refreshPairWindow) becomes the SOLE release authority.
	//
	// Why it must: Phase 1's deviation is measured against prevVWAPs, the
	// last-known-good comparator, which is deliberately held fixed for the
	// whole hold (frozen buckets skip the prevVWAPs update). So a price
	// that settles at any residual level past the class FreezePct — even
	// one that is statistically normal for the asset — keeps Phase 1 firing
	// on every bucket. Each such fire returns ok=false to refreshPairWindow,
	// which short-circuits BEFORE the Phase 2 confidence step; and the
	// auto-unfreeze streak that releases a freeze is produced ONLY by that
	// step (Scored=true, healthy). A live freeze whose Phase 1 keeps firing
	// could therefore never accumulate the streak — it extended to
	// escalation and pinned the last-known-good price indefinitely, even
	// after the anomaly cleared and every bucket met the auto-unfreeze
	// condition (z < 3 AND confidence > 0.30).
	//
	// Standing down does NOT weaken the freeze: returning ok=true here only
	// lets the caller reach the Phase 2 lifecycle step, which keeps the
	// bucket refused for as long as the lifecycle stays Active and re-fires
	// its own 3-signal AND on a still-anomalous bucket (resetting the
	// streak). A fresh, not-yet-frozen pair is unaffected — Phase 1 still
	// engages the class-deviation freeze below.
	if o.freezeStates[stateKey].Active() {
		return anomaly.ActionAllow, true
	}

	prev := o.prevVWAPs[stateKey]
	decision := o.cfg.Anomaly.Evaluate(anomaly.Observation{
		Pair:     pair,
		PrevVWAP: prev,
		CurrVWAP: currVWAP,
		// Same independence widening as the Phase 2 leg: a pair
		// corroborated by ≥2 agreeing router routes is not single-source,
		// so Phase 1's `deviation > FreezePct AND source_count <= 1` guard
		// also stops false-firing on thin corroborated crosses (see
		// [Orchestrator.effectiveSourceCount]).
		SourceCount: o.effectiveSourceCount(pair, window, trades),
	})
	if !decision.IsFrozen() {
		if decision.IsWarn() {
			// COR-09 / AGT-06: this decision used to end here and vanish.
			// The caller discards the returned Action on the non-freeze
			// path, so a bucket deviating past warn_pct — loud enough to
			// call out, not loud enough to freeze — left no trace at all
			// and `warn_pct` was an inert knob. It is NOT folded into
			// flags.divergence_warning despite what several doc comments
			// used to say: that flag is the cross-reference divergence
			// service's, and is meaningful only alongside
			// divergence_checked (CS-087). An anomaly warn runs no
			// cross-reference check, so setting it would publish
			// divergence_warning=true / divergence_checked=false — the
			// exact state CS-087 calls un-interpretable.
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

	// Phase 1 shares the ADR-0019 freeze lifecycle with Phase 2 — one
	// owner per `freeze:<asset>:<quote>` key. Two owners writing the
	// same marker with different TTL semantics would let a Phase 1
	// fire truncate a Phase 2 hold's marker to the flat grace TTL, and
	// would leave the ladder un-advanced (never extending, never
	// escalating) for as long as Phase 1 kept firing.
	//
	// Scored=false: the Phase 1 path returns before the confidence
	// step runs, so this bucket contributes no evidence of health and
	// cannot earn an auto-unfreeze streak. Corroborated=false for the
	// same reason — Phase 1 has no corroboration signal, so its first
	// hold is the shorter uncorroborated one.
	//
	// A `Fires: true` signal always ends frozen EXCEPT when the
	// operator has cleared the marker out of band, which the lifecycle
	// honours as a force-unfreeze. Returning ok=true there publishes
	// the bucket, so the override behaves identically whichever phase
	// flagged the pair; if the anomaly persists the next tick
	// re-freezes it, which is intended — the durable remedy for a
	// mis-calibration is a threshold change, not repeated overrides.
	if !o.stepFreezeLifecycle(ctx, pair, window, stateKey,
		freeze.Signal{Now: now, Fires: true}, decision, prev) {
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

// fetchForTarget pulls trades from the store for a single target
// pair and window. When EnableStablecoinFiatProxy is off this is a
// single TradesInRange call for pair itself; when on, the pair is
// expanded via aggregate.ExpandTargetPair into a direct pair plus
// one backer pair per peg, each backer pair is fetched and its
// trades are rewritten onto the target pair.
//
// The returned `usdVolume` is the correctly-scaled total USD value
// of every merged trade, computed BEFORE pair rewrites blur the
// original quote-decimal convention. This is the value the min-
// volume gate compares against — without it, classic/SAC USD-pegged
// proxy trades (7-decimal scale) would be summed under the off-chain
// uniform-1e8 assumption and the gate would see 10× understatement.
// F-1213 (codex audit-2026-05-12).
//
// `tradeUSD` is a parallel per-trade USD-value map keyed by
// canonical.Trade.ID(). Lets the filter chain drop trades by index
// while preserving USD attribution: F-1242 (codex audit-2026-05-12)
// — `flushContributions` sums per-source USD over the post-filter
// survivors so the persisted `volume_usd` matches the contribution
// population the VWAP was actually computed against, not the
// pre-filter total.
//
// Per-backer fetch errors are logged and skipped rather than
// aborting the whole window — a single connector misbehaving at
// the Timescale layer shouldn't black out an otherwise-healthy
// aggregation target.
// fetchTradesDetectTruncation wraps the store fetch with the per-query
// cap and bumps AggregatorWindowTruncatedTotal (+ a WARN) when the
// returned row count hits the cap — i.e. the window held more trades
// than `MaxTradesPerWindow` and the VWAP is computed over only the
// newest `cap` of them. `target` is the aggregation target (for the log
// line); `fetch` is the actual pair queried (== target for the direct
// path, a stablecoin-backer pair under proxy expansion).
func (o *Orchestrator) fetchTradesDetectTruncation(
	ctx context.Context, target, fetch canonical.Pair, from, to time.Time,
) ([]canonical.Trade, error) {
	t, err := o.store.TradesInRange(ctx, fetch, from, to, o.cfg.MaxTradesPerWindow)
	if err != nil {
		return nil, err
	}
	if len(t) >= o.cfg.MaxTradesPerWindow {
		obs.AggregatorWindowTruncatedTotal.Inc()
		o.logger.Warn("trade window truncated at MaxTradesPerWindow — VWAP over newest-N slice only",
			"target", target.String(),
			"fetch_pair", fetch.String(),
			"cap", o.cfg.MaxTradesPerWindow,
			"from", from.UTC(),
			"to", to.UTC(),
		)
	}
	return t, nil
}

func (o *Orchestrator) fetchForTarget(
	ctx context.Context,
	target canonical.Pair,
	from, to time.Time,
) (trades []canonical.Trade, usdVolume float64, tradeUSD map[string]float64, err error) {
	if !o.cfg.EnableStablecoinFiatProxy {
		t, err := o.fetchTradesDetectTruncation(ctx, target, target, from, to)
		if err != nil {
			return nil, 0, nil, err
		}
		total, perTrade := usdVolumeForPairPerTrade(target, t, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets)
		return t, total, perTrade, nil
	}

	sources, err := aggregate.ExpandTargetPairWithClassicPegs(target, o.cfg.USDPeggedClassicAssets)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("expand target %s: %w", target.String(), err)
	}

	var merged []canonical.Trade
	var sumUSD float64
	tradeUSD = map[string]float64{}
	for _, src := range sources {
		batch, ferr := o.fetchTradesDetectTruncation(ctx, target, src, from, to)
		if ferr != nil {
			o.logger.Warn("stablecoin-expansion fetch failed",
				"target", target.String(),
				"source_pair", src.String(),
				"err", ferr,
			)
			continue
		}
		// Per-trade USD value against the SOURCE pair's quote-decimal
		// convention — captured BEFORE the rewrite below blurs the
		// original 7-vs-8 decimal.
		batchTotal, batchPerTrade := usdVolumeForPairPerTrade(src, batch, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets)
		sumUSD += batchTotal
		for id, v := range batchPerTrade {
			tradeUSD[id] = v
		}
		if src.Equal(target) {
			merged = append(merged, batch...)
			continue
		}
		for i := range batch {
			batch[i].Pair = target
			merged = append(merged, batch[i])
		}
	}
	return merged, sumUSD, tradeUSD, nil
}

// usdVolumeForPair was the F-1213 entry point that returned only
// the windowed total. Superseded by [usdVolumeForPairPerTrade]
// which exposes the per-trade map needed for F-1242 post-filter
// per-source attribution. Kept here as a documentation pointer;
// the implementation lives in usdVolumeForPairPerTrade.
func usdVolumeForPair(pair canonical.Pair, batch []canonical.Trade, classicUSDPegs, sorobanUSDPegs []canonical.Asset) float64 {
	total, _ := usdVolumeForPairPerTrade(pair, batch, classicUSDPegs, sorobanUSDPegs)
	return total
}

// _ = usdVolumeForPair retains the function as a stable seam in
// case future code wants the just-the-total signature back.
var _ = usdVolumeForPair

// usdVolumeForPairPerTrade is the F-1242 (codex audit-2026-05-12)
// extension of [usdVolumeForPair] — it returns the same total plus
// a per-trade.ID() → USD-value map. The map is keyed before
// `fetchForTarget` rewrites Pair to the target, so the
// per-source filter chain can drop trades by index without losing
// the per-trade USD attribution the contribution sink uses.
//
// Returns (0, nil) when the pair's quote isn't a recognised USD
// surface — the contribution sink stamps NULL `volume_usd` in
// that case, matching the prior all-NULL posture for unrecognised
// quotes. Decimal-scale resolution is delegated to
// [usdQuoteDecimals] — the SAME classification [dropForMinUSDVolume]
// uses to decide whether the MinUSDVolume floor applies to a given
// target pair, so the two can never disagree about which quote
// shapes are USD-valuable (Guard 1, 2026-07-10).
func usdVolumeForPairPerTrade(pair canonical.Pair, batch []canonical.Trade, classicUSDPegs, sorobanUSDPegs []canonical.Asset) (float64, map[string]float64) {
	if len(batch) == 0 {
		return 0, nil
	}
	if _, ok := usdQuoteDecimals(pair.Quote, classicUSDPegs, sorobanUSDPegs); !ok {
		return 0, nil
	}
	// One scale per DISTINCT decimals value, not per trade — a window
	// carries hundreds of trades across at most three decimal classes.
	scales := make(map[int]*big.Int, 3)
	perTrade := make(map[string]float64, len(batch))
	var total float64
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
		rat := new(big.Rat).SetFrac(amt, scale)
		v, _ := rat.Float64() // i128:ok per-trade USD volume for the MinUSDVolume floor gate, not served
		perTrade[batch[i].ID()] = v
		total += v
	}
	return total, perTrade
}

// usdQuoteDecimalsForTrade resolves the fixed-point scale for ONE
// trade's QuoteAmount. Same four tiers as [usdQuoteDecimals], but the
// two OFF-CHAIN tiers read the emitting source's declared scale from
// the external registry instead of assuming the 1e8 CEX convention
// (MNY-05 / CS-040 — the same per-source resolution
// [approxUSDVolume] already uses for the confidence liquidity factor).
//
// The off-chain convention is NOT uniform: the CEX pollers stamp 8
// decimals, but the FX pollers stamp 6
// ([external.Registry]'s `massive` /
// `exchangeratesapi` all declare AmountDecimals:6, with the comment
// "so the USD-volume gate scales them right"). Valuing a 6dp
// fiat:USD-quoted amount at 1e8 understates it 100× — enough on its own
// to drop an otherwise-healthy window below MinUSDVolume ($10K by
// default) every tick, and to persist a 100×-low `volume_usd` on the
// source-contribution rows. Reading the registry is what makes the
// declaration the registry already carries actually load-bearing.
//
// The two ON-CHAIN tiers keep the structural 7: a classic credit and
// its SAC wrapper are 7-decimal by Stellar protocol invariant, whoever
// reports the trade, and [external.Metadata.AmountScaleDecimals]
// falls back to 8 for any source missing from the registry — so
// deferring to it there would turn one unregistered on-chain venue
// into a silent 10× understatement of a real USD peg.
func usdQuoteDecimalsForTrade(quote canonical.Asset, source string, classicUSDPegs, sorobanUSDPegs []canonical.Asset) (decimals int, ok bool) {
	switch {
	case quote.Type == canonical.AssetFiat && quote.Code == "USD",
		aggregate.IsFiatProxyFor(quote, "USD"):
		return external.Lookup(source).AmountScaleDecimals(), true
	default:
		return usdQuoteDecimals(quote, classicUSDPegs, sorobanUSDPegs)
	}
}

// usdQuoteDecimals resolves the fixed-point decimal scale needed to
// read a trade's QuoteAmount as a USD figure, for a pair whose quote
// leg is one of the four shapes this package can value in USD
// without a live price lookup:
//
//  1. fiat:USD directly — decimals 8, the off-chain CEX DEFAULT.
//  2. An abstract USD-pegged stablecoin ticker (`crypto:USDT`,
//     `crypto:USDC`, `crypto:DAI`, `crypto:PYUSD`, `crypto:USDP` —
//     whatever [aggregate.IsFiatProxyFor] maps to "USD") — decimals 8
//     as well, because only off-chain sources ever stamp the ABSTRACT
//     ticker (Binance XLMUSDT, Kraken XLM/USDT, …; the on-chain legs
//     carry classic/Soroban identity instead).
//
// The 8 on tiers 1-2 is a per-PAIR default, not the per-trade truth:
// the off-chain convention splits 8 (CEX) vs 6 (FX pollers), so the
// valuation path resolves it per trade from the emitting source's
// registry declaration — see [usdQuoteDecimalsForTrade] (MNY-05). This
// function answers the pair-level question ("is this quote a USD
// surface at all"), which is source-independent.
//  3. A classic Stellar credit on `classicUSDPegs` — decimals 7
//     (the Stellar-classic invariant).
//  4. A Soroban SAC wrapper on `sorobanUSDPegs` — decimals 7 (a SAC
//     always mirrors the 7-decimal scale of the classic asset it
//     wraps; Guard 1, 2026-07-10).
//
// Tier 2 is R-008 (audit 2026-07-23). It is the shape the
// stablecoin-fiat-proxy expansion fetches under
// (`ExpandTargetPairWithClassicPegs` emits `BASE/crypto:USDT` &
// friends for a fiat:USD target), and it used to fall through to
// ok=false — so [usdVolumeForPairPerTrade] valued the WHOLE batch at
// $0, [survivorUSDVolume] summed $0 for those trades, and the
// fiat:USD target window was measured against MinUSDVolume as if the
// CEX stablecoin legs carried no dollars at all. A window whose
// volume was mostly (or entirely) USDT-quoted was dropped every tick.
// The peg set is read from the aggregate stablecoin map rather than
// re-listed here so the two can't drift.
//
// ok=false means none of the four tiers apply — the quote asset has
// no USD valuation this package can compute cleanly. That covers
// non-USD fiat (fiat:EUR, fiat:GBP, …; would need a live FX rate),
// non-USD stablecoin tickers (crypto:EURC → fiat:EUR; same missing
// FX rate), an un-pegged classic/Soroban quote (would need a live
// price lookup — "rare" per the Guard 1 finding, and deliberately
// NOT built here; see dropForMinUSDVolume's unvaluable branch), and
// any other crypto/RWA/native quote shape.
//
// Both [usdVolumeForPairPerTrade] (valuation) and
// [dropForMinUSDVolume] (MinUSDVolume applicability) call this so
// the two questions — "can we value this pair's USD volume" and
// "does the manipulation floor apply to this pair" — are answered by
// exactly one classification, not two that can drift apart. Before
// 2026-07-10 they WERE two separate checks (minUSDVolumeApplies
// tested only fiat:USD; this switch also recognised classic pegs)
// and had drifted: a directly-configured classic- or Soroban-quoted
// target pair could be valued here but the floor never consulted
// that value.
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

// survivorUSDVolume returns the USD volume contributed by the
// post-filter survivor slice, looked up by stable trade ID in the
// per-trade map captured before fetchForTarget's pair rewrites.
//
// F-1260 (codex audit-2026-05-12): the MinUSDVolume manipulation
// gate is documented as a post-class, post-outlier publish gate,
// but previously evaluated the pre-filter total — letting thin
// survivor windows clear the floor on volume the filter had
// already discarded. This helper bridges the rewrite scheme
// (Pair carries the target after fetchForTarget) with the source-
// pair quote-decimal accounting that fed the gate's input.
//
// A missing key contributes zero — the only way an ID misses the
// map is if `usdVolumeForPairPerTrade` decided the source pair's
// quote isn't a recognised USD surface, in which case the trade
// doesn't contribute to the USD-volume gate by definition.
func survivorUSDVolume(trades []canonical.Trade, tradeUSD map[string]float64) float64 {
	if len(trades) == 0 || len(tradeUSD) == 0 {
		return 0
	}
	var total float64
	for i := range trades {
		total += tradeUSD[trades[i].ID()]
	}
	return total
}

// dropForMinUSDVolume returns true (and bumps the matching counters
// + emptyWindows stat) when the post-class + post-outlier window
// fails the per-pair USD-volume threshold. Caller treats the true
// case the same as a literally-empty window — skip the publish and
// move on. Extracted from refreshPairWindow to keep its cognitive
// complexity under the linter cap.
//
// `usdVolume` is the SURVIVOR-set USD total — F-1260 (codex audit-
// 2026-05-12) replaced the pre-filter scalar with [survivorUSDVolume]
// of the post-class + post-outlier slice. Before F-1260 the caller
// passed in the pre-filter total, which let thin windows publish
// above MinUSDVolume on volume the filter had already discarded.
//
// Applicability is [usdQuoteDecimals] — the SAME classification
// [usdVolumeForPairPerTrade] uses to compute `usdVolume` in the
// first place, so "can we value this pair" and "does the floor
// apply" can't drift apart (Guard 1, 2026-07-10). Three outcomes:
//
//   - Quote is USD-valuable (fiat:USD / classic peg / Soroban SAC
//     peg): floor applies — this is the normal gate path below.
//   - Quote is on-chain (classic or Soroban) but NOT a recognised
//     peg: unvaluable WITHOUT a live price lookup this package
//     deliberately doesn't build (see [Config.MinUSDVolume]). This
//     branch FAILS CLOSED (window dropped) as of 2026-08-04. History:
//     before 2026-07-10 it passed through with no floor SILENTLY;
//     2026-07-10 made it loud (WARN + metric) but kept the
//     pass-through, reasoning that fail-closed would blackout a
//     future legitimate not-yet-pegged pair. The 2026-08-04 valuation
//     incident settled that trade the other way: an unvaluable quote
//     is exactly the shape a mint-and-dust attacker produces, and "if
//     the volume cannot be valued, the floor cannot be verified, so
//     do not publish" is the operator's stated policy (the serving
//     side got the same posture via pricingguard.SubstanceGate). The
//     blackout concern keeps its answer — the WARN + metric name the
//     missing peg, and adding it to usd_pegged_classic_assets /
//     sac_wrappers un-blacks the pair deliberately, with valuation.
//   - Quote is fiat but not USD (EUR, GBP, …): exempt, no WARN — a
//     distinct, pre-existing, already-understood scope boundary (the
//     threshold is a USD figure; converting a EUR/GBP window needs a
//     live FX rate, same "no live lookup" limit as above, but this
//     shape isn't new and isn't a manipulation-guard regression, so
//     it doesn't need the same loud surfacing).
//
// See [Config.MinUSDVolume] for the full threshold semantics.
func (o *Orchestrator) dropForMinUSDVolume(pair canonical.Pair, trades []canonical.Trade, usdVolume float64) bool {
	_ = trades // retained for tracing dimensions if future gates want it
	if o.cfg.MinUSDVolume <= 0 {
		return false
	}
	if _, valuable := usdQuoteDecimals(pair.Quote, o.cfg.USDPeggedClassicAssets, o.cfg.USDPeggedSorobanAssets); !valuable {
		if pair.Quote.Type == canonical.AssetClassic || pair.Quote.Type == canonical.AssetSoroban {
			obs.AggregatorMinUSDVolumeUnvaluableTotal.WithLabelValues(pair.String()).Inc()
			o.logger.Warn("min_usd_volume floor unverifiable: on-chain quote asset has no recognised USD peg — window DROPPED (fail-closed; add the peg to usd_pegged_classic_assets / sac_wrappers to publish this pair)",
				"pair", pair.String())
			obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume_unvaluable").Inc()
			o.mu.Lock()
			o.emptyWindows++
			o.mu.Unlock()
			obs.AggregatorEmptyWindowsTotal.Inc()
			return true
		}
		return false
	}
	if usdVolume >= o.cfg.MinUSDVolume {
		return false
	}
	obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume").Inc()
	o.mu.Lock()
	o.emptyWindows++
	o.mu.Unlock()
	obs.AggregatorEmptyWindowsTotal.Inc()
	return true
}

// filterForVWAP drops trades whose source is not registered as a
// Class=Exchange + IncludeInVWAP=true venue. This is the
// aggregator-policy layer that implements the "only genuine
// exchange trades contribute to the average" rule.
//
// Unknown sources (not in external.Registry) are dropped — the
// registry's fail-closed default (ClassExchange, IncludeInVWAP=
// false) already handles that: they're VISIBLE in /v1/sources but
// don't vote in VWAP unless an operator explicitly registers them.
//
// Preserves input order so VWAP's weighted-mean semantics stay
// deterministic under the same input set.
func filterForVWAP(trades []canonical.Trade) []canonical.Trade {
	out := trades[:0]
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

// formatRatMaxScale caps the fractional places [renderScale] will
// extend to, so a pathological (or hostile) micro-valued rational can
// never make us render an unbounded string on the publish path. Mirrors
// storage-tier bridgeRateMaxScale.
const formatRatMaxScale = 60

// formatRatFixed returns a fixed-precision decimal string
// representation of r. 12 decimal places covers every sensible
// crypto/fiat price range without float-precision loss.
//
// We don't use (*big.Rat).FloatString because Go's default
// rounding is banker's round-half-to-even — fine for accounting
// but not the "truncate toward zero" convention the API spec
// mandates. Rolling a tiny fixed-precision formatter keeps the
// rounding behaviour explicit.
//
// R-1: the fixed scale is a FLOOR, not a hard cap. A strictly-positive
// rational whose first significant digit falls beyond `decimals` places
// (e.g. a high-supply token priced in BTC at <1e-12) would truncate to
// "0.000…0", reparse to zero via big.Rat.SetString, and be served as
// price 0 — which then poisons the next tick's window edge graph
// (BuildEdges rejects a Sign()<=0 leg and nils the whole graph).
// [renderScale] extends the scale magnitude-relatively for exactly those
// values so no positive price ever renders to a zero-reparsing string,
// while normal-magnitude prices keep byte-identical output.
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

// renderScale returns the number of fractional decimal places
// [formatRatFixed] should render r with. It is the requested `decimals`
// for any value that renders non-zero there (so normal-magnitude prices
// keep byte-identical output), but for a strictly-positive magnitude
// whose first significant digit falls BEYOND `decimals` places it
// EXTENDS the scale to keep [formatRatSigDigits] significant digits — so
// a tiny-but-non-zero price never truncates to a "0.000…0" string that
// reparses to zero (R-1).
//
// Deliberately float-free (ADR-0003): a log10 to find the magnitude is
// the obvious way to reintroduce float error into the money path. This
// counts leading fractional zeros exactly, mirroring the proven
// storage-tier rateScaleFor.
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
	// The fixed `decimals` render is all-zeros exactly when that first
	// significant digit lands beyond the last rendered place. Only then
	// extend — otherwise leave the requested scale untouched.
	if firstSigPlace <= decimals {
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
	tradeUSD map[string]float64,
) {
	if o.cfg.ContributionSink == nil {
		return
	}
	contributions := aggregate.SourceContributions(trades)
	if len(contributions) == 0 {
		return
	}
	// F-1242 (codex audit-2026-05-12): walk the POST-filter trade
	// slice and sum per-source USD value from the per-trade map.
	// This matches the contribution population VWAP was computed
	// against; an outlier-dropped trade contributes 0 USD to its
	// source's row instead of double-attributing through the
	// pre-filter total.
	var sourceUSD map[string]float64
	if len(tradeUSD) > 0 {
		sourceUSD = make(map[string]float64, len(contributions))
		for i := range trades {
			if v, ok := tradeUSD[trades[i].ID()]; ok {
				sourceUSD[trades[i].Source] += v
			}
		}
	}
	if err := o.cfg.ContributionSink.RecordContributions(ctx, ContributionRecord{
		Pair:            pair,
		Window:          window,
		ComputedAt:      time.Now().UTC(),
		Contributions:   contributions,
		SourceUSDVolume: sourceUSD,
	}); err != nil {
		o.logger.Debug("contribution sink",
			"pair", pair.String(), "window", window, "err", err)
	}
}
