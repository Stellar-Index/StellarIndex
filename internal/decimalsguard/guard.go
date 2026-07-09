// Package decimalsguard is the served-price decimals-assumption guard
// (decoder-correctness audit Finding 2, HIGH-latent).
//
// The served price is Σ(quote_amount)/Σ(base_amount) computed on RAW
// smallest-unit integers — in the prices_* continuous aggregates
// (migrations/0002_create_price_aggregates.up.sql) and in aggregate.VWAP.
// Per-asset decimals CANCEL in that ratio ONLY when the base and quote
// assets share the same scale. Every DEX-traded Stellar token observed to
// date is 7-decimal (SACs are always 7; the pure-SEP-41 tokens all declare
// decimals=7), so the raw ratio equals the true price and everything is
// correct. The decoders are correct too: they store faithful native-decimal
// amounts (ADR-0003).
//
// The latent risk: the first non-7-decimal SEP-41 token to gain DEX
// liquidity (an 18-decimal bridged asset, a 6-dp token, …) makes every
// served price for a pair involving it silently skew by 10^(7−decimals),
// with no other alarm — a wrong price on a real pair, invisible.
//
// This package is the DETECTION half of the mitigation: a periodic sweep
// that resolves the on-chain decimals() of every recently-DEX-traded
// Soroban token from the certified lake and raises
// obs.DEXTradeNonstandardDecimalsTotal the moment one is != 7 — turning a
// silent landmine into a loud, per-asset signal (the analogue of the
// FX-freshness alert). The forward-looking NORMALIZATION (dividing the
// ratio by 10^(dec_base−dec_quote)) is a deferred follow-up: it cannot be
// applied consistently without rewriting the decade-deep prices_* CAGGs,
// which is not warranted for a latent, not-yet-firing risk. See the runbook
// docs/operations/runbooks/dex-nonstandard-decimals.md.
//
// The periodic sweep only enumerates a short trailing window (20m
// default), so it only catches a token that is STILL trading — a token
// that traded a handful of times and then went dormant is invisible to
// it forever. Guard.Backfill is the one-time startup self-seed pass that
// closes that gap: it runs the same classify+report path over a much
// longer (90d default) historical window once at process start, so a
// historically-traded-but-now-dormant offender gets upserted into
// nonstandard_decimals_assets without an operator hand-seeding the row
// (the gap behind the 2026-07-09 CC2RB… incident — see the runbook's
// changelog).
package decimalsguard

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// StandardDecimals is the decimals scale every DEX-traded Stellar token
// carries today: SACs are always 7, classic credits are uniformly 7, and
// every pure-SEP-41 token observed on-chain so far declares 7. A resolved
// value other than this is exactly the assumption-violation this guard
// exists to catch.
const StandardDecimals = 7

// Default sweep cadence. The window is >= the interval so consecutive
// sweeps overlap and no trade falls between them: any Soroban token that
// trades even once is enumerated by at least one sweep, and — because the
// counter is monotonic and per-(source,asset) deduped — a single detection
// latches a persistent alert for the process lifetime.
const (
	DefaultInterval = 15 * time.Minute
	DefaultWindow   = 20 * time.Minute
)

// DefaultBackfillWindow bounds the one-time startup self-seed pass
// (Backfill): how far back it looks for distinct Soroban-legged
// (source, asset) trade pairs. The periodic sweep's short Window (20m)
// only catches a token that is STILL trading — a token that traded once
// and went dormant before the guard ever ran (or before it traded again
// inside a 20-minute window) is invisible to Sweep forever. That gap is
// exactly what let token CC2RB… (decimals()=9, confirmed 2026-07-09) go
// unseeded after the v0.10.0 deploy until an operator hand-inserted the
// row per the runbook.
//
// 90 days is a judgment call, not a derived constant: long enough to
// catch a token that traded a handful of times and then went quiet,
// short enough that RecentSorobanDEXTrades stays a sargable index-range
// scan (see soroban_dex_assets.go) rather than drifting toward "full
// history". A token dormant for longer than this window is NOT caught by
// Backfill; the runbook's manual hand-seed step remains the fallback for
// that residual, long-dormant case. Config-surfaced via
// internal/config.DecimalsGuardConfig.BackfillWindowDays.
const DefaultBackfillWindow = 90 * 24 * time.Hour

// DefaultBackfillThrottle paces Backfill's per-asset ClickHouse
// decimals() lookups so a large distinct-asset set doesn't hammer the
// lake in a tight loop at process start. Only applied between resolver
// calls that actually reach ClickHouse — a resolved-cache hit (the same
// asset seen twice, e.g. traded against two different quote assets)
// never queries the lake, so it isn't throttled.
const DefaultBackfillThrottle = 100 * time.Millisecond

// TradeReader enumerates the distinct (source, Soroban-contract-asset)
// pairs that recently traded in the served tier. Satisfied by
// *timescale.Store.
type TradeReader interface {
	RecentSorobanDEXTrades(ctx context.Context, since time.Time) ([]timescale.SorobanDEXTradeRef, error)
}

// DecimalsResolver resolves a token contract's on-chain decimals() from the
// certified lake (contract-instance METADATA, no WASM execution). Satisfied
// structurally by *clickhouse.ExplorerReader. found=false means the
// declaration isn't derivable (instance not captured, or a non-token-sdk
// contract) — the guard treats that as "cannot confirm" and never alarms on
// it.
type DecimalsResolver interface {
	TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error)
}

// DecimalsAssetWriter persists a CONFIRMED non-7 decimals() declaration so
// the API-side READ-TIME serving guard (internal/api/v1) can decline to
// serve /v1/price, /v1/vwap, /v1/history, /v1/ohlc for any pair touching
// the asset — turning this package's detection-only signal into an actual
// stop-serving lever (docs/operations/runbooks/dex-nonstandard-decimals.md
// "Mitigation" step). Satisfied by *timescale.Store via
// UpsertNonstandardDecimalsAsset.
//
// Optional: a nil Writer disables persistence — the guard still fires the
// metric + ERROR log unconditionally, it just leaves the serving-side guard
// unfed (the pre-2026-07-09 behaviour).
type DecimalsAssetWriter interface {
	UpsertNonstandardDecimalsAsset(ctx context.Context, asset string, decimals uint32, source string) error
}

// Guard periodically sweeps recently-DEX-traded Soroban tokens and raises
// obs.DEXTradeNonstandardDecimalsTotal for any whose on-chain decimals() is
// confirmed != 7. It is conservative: it alarms ONLY on a confirmed non-7
// value, never on a resolution error or a non-derivable declaration.
type Guard struct {
	reader   TradeReader
	resolver DecimalsResolver
	writer   DecimalsAssetWriter
	window   time.Duration
	logger   *slog.Logger

	// backfillWindow / backfillThrottle configure the one-time startup
	// self-seed pass. See Backfill.
	backfillWindow   time.Duration
	backfillThrottle time.Duration

	mu sync.Mutex
	// resolved caches CONFIRMED decimals per contract id (both standard and
	// non-standard), so repeated sweeps of the same steady token set issue no
	// further lake queries. A contract's decimals() is effectively immutable.
	// Resolution errors and not-derivable declarations are deliberately NOT
	// cached, so a token whose instance is captured later is still checked.
	resolved map[string]uint32
	// fired dedups the counter to one increment per (source, asset) per
	// process — the signal is "this landmine exists", latched, not a
	// per-trade rate.
	fired map[string]struct{}
}

// Options configures a Guard.
type Options struct {
	// Window is the trailing time window each sweep enumerates. Zero =>
	// DefaultWindow. Should be >= the Run interval so consecutive sweeps
	// overlap and no trade falls between them.
	Window time.Duration
	// Logger; nil => slog.Default().
	Logger *slog.Logger
	// Writer, when non-nil, persists each confirmed non-7-decimals asset
	// into `nonstandard_decimals_assets` (migration 0093) so the API's
	// read-time serving guard can decline pricing for it. Nil disables
	// persistence — detection (metric + log) is unaffected.
	Writer DecimalsAssetWriter
	// BackfillWindow is the trailing lookback for the one-time startup
	// self-seed pass (Backfill). Zero => DefaultBackfillWindow (90 days).
	BackfillWindow time.Duration
	// BackfillThrottle paces Backfill's per-asset ClickHouse decimals()
	// lookups. Zero => DefaultBackfillThrottle.
	BackfillThrottle time.Duration
}

// New builds a Guard.
func New(reader TradeReader, resolver DecimalsResolver, opts Options) *Guard {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	backfillWindow := opts.BackfillWindow
	if backfillWindow <= 0 {
		backfillWindow = DefaultBackfillWindow
	}
	backfillThrottle := opts.BackfillThrottle
	if backfillThrottle <= 0 {
		backfillThrottle = DefaultBackfillThrottle
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Guard{
		reader:           reader,
		resolver:         resolver,
		writer:           opts.Writer,
		window:           window,
		backfillWindow:   backfillWindow,
		backfillThrottle: backfillThrottle,
		logger:           logger,
		resolved:         make(map[string]uint32),
		fired:            make(map[string]struct{}),
	}
}

// Run sweeps immediately (so a standing offender is caught at startup, not
// one interval later) and then every interval until ctx is cancelled. A
// failed sweep is logged and retried next tick — a transient lake/DB blip
// never wedges the guard.
func (g *Guard) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}
	g.logger.Info("decimals-guard started", "interval", interval, "window", g.window)

	if err := g.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		g.logger.Warn("decimals-guard: initial sweep failed", "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := g.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("decimals-guard: sweep failed", "err", err)
			}
		}
	}
}

// Sweep enumerates recently-DEX-traded Soroban tokens, resolves each one's
// decimals, and raises the metric (once per source+asset) for any confirmed
// non-7 value. Returns an error only when the trade enumeration itself fails
// — per-asset resolution failures are swallowed (retried next sweep).
func (g *Guard) Sweep(ctx context.Context) error {
	refs, err := g.reader.RecentSorobanDEXTrades(ctx, time.Now().Add(-g.window))
	if err != nil {
		return err
	}
	for _, ref := range refs {
		decimals, confirmed := g.classify(ctx, ref.Asset)
		if !confirmed || decimals == StandardDecimals {
			continue
		}
		g.report(ctx, ref, decimals)
	}
	return nil
}

// Backfill is the ONE-TIME startup self-seed pass. It enumerates every
// distinct Soroban-legged (source, asset) pair that traded within the
// trailing BackfillWindow (default DefaultBackfillWindow, 90 days) and
// runs each one through the SAME classify+report path Sweep uses — so a
// token that traded and then went DORMANT before the periodic sweep's
// short Window (default 20m) ever observed it is still resolved,
// confirmed, and upserted into nonstandard_decimals_assets at process
// start, instead of staying invisible until it trades again or an
// operator hand-seeds the row per the runbook.
//
// This closes the gap behind the 2026-07-09 production incident: token
// CC2RB… traded starting 2026-06-22 but the guard (added later) only
// enumerates the trailing 20 minutes on each tick, so a token that never
// happened to trade again inside one of those windows after the guard
// started was never picked up automatically — an operator had to hand-
// insert the row.
//
// Bounded by design: RecentSorobanDEXTrades is a time-windowed,
// index-sargable scan (base_asset/quote_asset lead the composite index,
// ts DESC — see soroban_dex_assets.go). Widening the window to 90 days
// keeps the same query shape; it does not become the forbidden unbounded
// full-table DISTINCT. A token that hasn't traded in longer than
// BackfillWindow is NOT caught by this pass — the runbook's manual
// hand-seed step remains the fallback for that residual, long-dormant
// case.
//
// Intended to be called once per process, before Run starts its periodic
// loop — Run's Sweep continues to own ongoing freshness on the short
// window. Resolution errors and not-yet-confirmable declarations are
// swallowed exactly like Sweep (a token whose instance is captured later
// is still checked, next time Backfill or Sweep sees it); Backfill
// returns an error only if the enumeration query itself fails.
func (g *Guard) Backfill(ctx context.Context) error {
	since := time.Now().Add(-g.backfillWindow)
	refs, err := g.reader.RecentSorobanDEXTrades(ctx, since)
	if err != nil {
		return err
	}

	confirmed := 0
	for _, ref := range refs {
		// Throttle only calls that will actually reach the resolver — a
		// cache hit (the same asset already resolved earlier in this
		// pass, or by a Sweep tick that ran first) never queries
		// ClickHouse, so it doesn't need pacing.
		if g.backfillThrottle > 0 && !g.isCached(ref.Asset) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(g.backfillThrottle):
			}
		}

		decimals, confirmedDecimals := g.classify(ctx, ref.Asset)
		if !confirmedDecimals || decimals == StandardDecimals {
			continue
		}
		confirmed++
		g.report(ctx, ref, decimals)
	}

	g.logger.Info("decimals-guard: startup backfill complete",
		"window", g.backfillWindow, "scanned_pairs", len(refs), "confirmed_offenders", confirmed)
	return nil
}

// isCached reports whether asset's decimals() is already in the resolved
// cache. Used by Backfill to skip throttling ahead of a call that will
// hit the cache rather than ClickHouse.
func (g *Guard) isCached(asset string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.resolved[asset]
	return ok
}

// classify resolves and caches a contract's decimals. confirmed=false when
// the value could not be established (resolution error, or not derivable) —
// the caller must NOT alarm in that case.
func (g *Guard) classify(ctx context.Context, asset string) (decimals uint32, confirmed bool) {
	g.mu.Lock()
	if d, ok := g.resolved[asset]; ok {
		g.mu.Unlock()
		return d, true
	}
	g.mu.Unlock()

	d, found, rerr := g.resolver.TokenDecimals(ctx, asset)
	if rerr != nil {
		// Transient lake error, or a key that isn't a valid contract strkey.
		// Not cached — retried next sweep.
		g.logger.Debug("decimals-guard: decimals resolve failed; will retry", "asset", asset, "err", rerr)
		return 0, false
	}
	if !found {
		// No on-chain decimals derivable yet. Not cached (a later-captured
		// instance is re-checked) and never an offender — we alarm only on a
		// CONFIRMED non-7 value.
		return 0, false
	}
	g.mu.Lock()
	g.resolved[asset] = d
	g.mu.Unlock()
	return d, true
}

// report increments the landmine counter (once per source+asset per
// process), logs the detail the runbook needs, and — when a Writer is
// wired — persists the confirmation so the API's read-time serving guard
// picks it up on its next cache refresh (~60s) and declines pricing for
// pairs touching this asset.
func (g *Guard) report(ctx context.Context, ref timescale.SorobanDEXTradeRef, decimals uint32) {
	key := ref.Source + "\x00" + ref.Asset
	g.mu.Lock()
	if _, seen := g.fired[key]; seen {
		g.mu.Unlock()
		return
	}
	g.fired[key] = struct{}{}
	g.mu.Unlock()

	obs.DEXTradeNonstandardDecimalsTotal.WithLabelValues(ref.Source, ref.Asset).Inc()
	g.logger.Error(
		"DEX trade for a non-7-decimal Soroban token — served price for pairs involving "+
			"this asset is silently skewed by 10^(7-decimals); apply decimals normalization "+
			"(runbook: dex-nonstandard-decimals.md)",
		"source", ref.Source,
		"asset", ref.Asset,
		"decimals", decimals,
		"price_skew_decades", skewDecades(decimals),
	)

	if g.writer == nil {
		return
	}
	if err := g.writer.UpsertNonstandardDecimalsAsset(ctx, ref.Asset, decimals, ref.Source); err != nil {
		g.logger.Warn(
			"decimals-guard: failed to persist confirmed nonstandard-decimals asset — "+
				"the API read-time serving guard will NOT decline this pair until a later "+
				"sweep succeeds (metric + log alarm above are unaffected)",
			"source", ref.Source, "asset", ref.Asset, "decimals", decimals, "err", err,
		)
	}
}

// skewDecades is the order-of-magnitude the served ratio is off for a pair
// with a 7-dp counter-leg: |7 - decimals|. Direction (over- vs under-stated)
// depends on whether the offending token is the base or quote leg — see the
// runbook.
func skewDecades(decimals uint32) int {
	d := int(decimals) - StandardDecimals
	if d < 0 {
		return -d
	}
	return d
}
