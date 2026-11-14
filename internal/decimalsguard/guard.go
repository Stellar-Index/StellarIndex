// Package decimalsguard is the served-price decimals-assumption guard.
//
// The served price is Σ(quote)/Σ(base) on RAW smallest-unit integers, so decimals cancel ONLY when
// base and quote share a scale. The first non-7-decimal SEP-41 token to gain liquidity would skew
// its pairs by 10^(7-decimals) with no other alarm.
//
// This is the DETECTION half: a periodic sweep resolves decimals() of recently DEX-traded Soroban
// tokens from the lake and raises obs.DEXTradeNonstandardDecimalsTotal when one is != 7. It is the
// sole writer of nonstandard_decimals_assets; internal/aggregate.AdjustPrice only consumes it. The
// 20m sweep misses dormant and late-ingested tokens; Guard.Backfill covers 90d at startup and every
// DefaultReenumerateInterval after.
package decimalsguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StandardDecimals is the scale SACs and classic credits always carry; any other resolved value is the violation this guard catches.
const StandardDecimals = 7

// Default sweep cadence. The window is >= the interval so consecutive sweeps overlap in trade time.
// A trade ingested later than the window after its ledger time is outside every sweep; the
// re-enumeration below is what examines it.
const (
	DefaultInterval = 15 * time.Minute
	DefaultWindow   = 20 * time.Minute
)

// DefaultReenumerateInterval is how often Run repeats the Backfill enumeration, which bounds a late-ingested trade's wait to hours.
const DefaultReenumerateInterval = 6 * time.Hour

// DefaultBackfillWindow bounds the Backfill lookback: 90 days catches a token that traded then went quiet while RecentSorobanDEXTrades stays a sargable range scan; longer-dormant tokens need the runbook's manual hand-seed.
const DefaultBackfillWindow = 90 * 24 * time.Hour

// DefaultBackfillThrottle paces Backfill's per-asset ClickHouse decimals() lookups so a large asset set doesn't hammer the lake; resolved-cache hits are not throttled.
const DefaultBackfillThrottle = 100 * time.Millisecond

// TradeReader enumerates the distinct (source, Soroban-contract-asset) pairs that recently traded in the served tier.
type TradeReader interface {
	RecentSorobanDEXTrades(ctx context.Context, since time.Time) ([]timescale.SorobanDEXTradeRef, error)
}

// DecimalsResolver resolves a token's on-chain decimals() from the lake's contract-instance metadata; found=false means not derivable, which the guard treats as "cannot confirm" and never alarms on.
type DecimalsResolver interface {
	TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error)
}

// DecimalsAssetWriter persists a CONFIRMED non-7 decimals() so the API can normalize prices via aggregate.AdjustPrice; with a nil Writer the metric and log still fire but those pairs serve an uncorrected raw ratio.
type DecimalsAssetWriter interface {
	UpsertNonstandardDecimalsAsset(ctx context.Context, asset string, decimals uint32, source string) error
}

// DecimalsAssetReconciler is the read+delete half Guard.Reconcile needs: it repairs `nonstandard_decimals_assets` toward the lake (upsert; delete when the lake confirms 7, which the CHECK forbids). A decorator that doesn't forward it disarms Reconcile, and New logs that at ERROR.
type DecimalsAssetReconciler interface {
	DecimalsAssetWriter
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
	DeleteNonstandardDecimalsAsset(ctx context.Context, asset string) error
}

// Compile-time proof the production store satisfies the reconcile seam, so the type assertion in
// New cannot quietly disarm the lockstep.
var _ DecimalsAssetReconciler = (*timescale.Store)(nil)

// Guard periodically sweeps recently-DEX-traded Soroban tokens and raises obs.DEXTradeNonstandardDecimalsTotal for a confirmed non-7 decimals(); it never alarms on a resolution error or non-derivable declaration.
type Guard struct {
	reader   TradeReader
	resolver DecimalsResolver
	writer   DecimalsAssetWriter
	// reconciler is writer when it also satisfies DecimalsAssetReconciler; nil disables Reconcile
	// only.
	reconciler DecimalsAssetReconciler
	window     time.Duration
	logger     *slog.Logger

	backfillWindow   time.Duration
	backfillThrottle time.Duration
	reenumerateEvery time.Duration

	mu sync.Mutex
	// resolved caches CONFIRMED decimals per contract id (decimals() is effectively immutable).
	// Errors and not-derivable declarations are NOT cached, so a token whose instance is
	// captured later is still checked.
	resolved map[string]uint32
	// alarmed dedups the alarm (metric + ERROR log) to one per (source, asset) per process; the
	// signal is "this landmine exists", not a per-trade rate.
	alarmed map[string]struct{}
	// persisted dedups the durable write. Unlike alarmed it latches ONLY after the write succeeds.
	persisted map[string]struct{}
	// pending holds confirmed offenders whose durable write failed. Every tick retries them, because
	// a dormant token is not enumerated again and would otherwise serve a skewed price until restart.
	pending map[string]pendingWrite
	// unresolved holds enumerated assets whose decimals() the lake could not derive: priced on the
	// 7-decimals assumption, not on a reading.
	unresolved map[string]struct{}
}

type pendingWrite struct {
	ref      timescale.SorobanDEXTradeRef
	decimals uint32
}

// passTally counts one pass's lake reads, so a pass that could read nothing is not reported as a clean one.
type passTally struct{ reads, failed int }

// Options configures a Guard.
type Options struct {
	// Window is the trailing window each sweep enumerates; zero => DefaultWindow. Keep it >= the
	// Run interval so no trade falls between sweeps.
	Window time.Duration
	// Logger; nil => slog.Default().
	Logger *slog.Logger
	// Writer, when non-nil, persists each confirmed non-7-decimals asset into
	// `nonstandard_decimals_assets` for the API's read-time correction. Nil disables
	// persistence; detection (metric + log) is unaffected.
	Writer DecimalsAssetWriter
	// BackfillWindow is the lookback for the startup Backfill; zero => DefaultBackfillWindow.
	BackfillWindow time.Duration
	// BackfillThrottle paces Backfill's per-asset lookups; zero => DefaultBackfillThrottle.
	BackfillThrottle time.Duration
}

// resolveReconciler returns w as a DecimalsAssetReconciler when it is one. A non-nil Writer that
// is not one leaves Reconcile disarmed, logged at ERROR since the likeliest cause is a wrapper
// that forgot to forward the interface.
func resolveReconciler(w DecimalsAssetWriter, logger *slog.Logger) DecimalsAssetReconciler {
	if w == nil {
		return nil
	}
	r, ok := w.(DecimalsAssetReconciler)
	if !ok {
		logger.Error("decimals-guard: Writer does not implement DecimalsAssetReconciler; lockstep reconcile is DISABLED",
			"writer_type", fmt.Sprintf("%T", w))
	}
	return r
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
		reconciler:       resolveReconciler(opts.Writer, logger),
		window:           window,
		backfillWindow:   backfillWindow,
		backfillThrottle: backfillThrottle,
		reenumerateEvery: DefaultReenumerateInterval,
		logger:           logger,
		resolved:         make(map[string]uint32),
		alarmed:          make(map[string]struct{}),
		persisted:        make(map[string]struct{}),
		pending:          make(map[string]pendingWrite),
		unresolved:       make(map[string]struct{}),
	}
}

// MarkEnabled seeds the sweep heartbeat at startup, before the lake dial, so staleness counts from process start; a gauge at 0 means not enabled.
func MarkEnabled(now time.Time) {
	obs.DecimalsGuardSweepLastSuccessUnix.Set(float64(now.Unix()))
}

// Run sweeps immediately, then every interval until ctx is cancelled, and repeats the Backfill enumeration on its own slower ticker; a failed pass is logged and retried next tick.
func (g *Guard) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}
	g.logger.Info("decimals-guard started", "interval", interval, "window", g.window)

	g.tick(ctx, "initial")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reenumerate := time.NewTicker(g.reenumerateEvery)
	defer reenumerate.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			g.tick(ctx, "periodic")
		case <-reenumerate.C:
			if err := g.Backfill(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("decimals-guard: re-enumeration failed", "err", err)
			}
		}
	}
}

// tick is one Run iteration: retry stranded writes, Sweep, then the lockstep Reconcile against the
// lake. They are independent (a failed Sweep must not skip the reconcile, nor vice versa) and both
// are logged and retried next tick, never fatal.
func (g *Guard) tick(ctx context.Context, phase string) {
	g.retryPending(ctx)
	if err := g.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		g.logger.Warn("decimals-guard: sweep failed", "phase", phase, "err", err)
	}
	if err := g.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
		g.logger.Warn("decimals-guard: lockstep reconcile failed", "phase", phase, "err", err)
	}
}

// Sweep resolves decimals for recently-DEX-traded Soroban tokens and raises the metric once per source+asset for a confirmed non-7 value. It errors if trade enumeration fails or if every lake read of the pass failed; any other pass stamps obs.DecimalsGuardSweepLastSuccessUnix so "found nothing" differs from "never armed" and from "could not look".
func (g *Guard) Sweep(ctx context.Context) error {
	refs, err := g.reader.RecentSorobanDEXTrades(ctx, time.Now().Add(-g.window))
	if err != nil {
		return err
	}
	var tally passTally
	for _, ref := range refs {
		decimals, confirmed := g.classify(ctx, ref.Asset, &tally)
		if !confirmed || decimals == StandardDecimals {
			continue
		}
		g.report(ctx, ref, decimals)
	}
	g.publishGauges()
	if tally.failed > 0 && tally.failed == tally.reads {
		return fmt.Errorf("decimals-guard: all %d lake decimals reads failed", tally.reads)
	}
	if tally.failed > 0 {
		g.logger.Warn("decimals-guard: some lake decimals reads failed; retried next sweep", "failed", tally.failed, "reads", tally.reads)
	}
	obs.DecimalsGuardSweepLastSuccessUnix.Set(float64(time.Now().Unix()))
	return nil
}

// Backfill is the wide self-seed: every pair that traded within BackfillWindow goes through Sweep's classify+report path, via a time-windowed sargable scan, never an unbounded DISTINCT. Call once before Run, which repeats it; errors if enumeration fails or any lake read failed.
func (g *Guard) Backfill(ctx context.Context) error {
	since := time.Now().Add(-g.backfillWindow)
	refs, err := g.reader.RecentSorobanDEXTrades(ctx, since)
	if err != nil {
		return err
	}

	confirmed := 0
	var tally passTally
	for _, ref := range refs {
		// Throttle only calls that reach the resolver; a cache hit never queries ClickHouse.
		if g.backfillThrottle > 0 && !g.isCached(ref.Asset) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(g.backfillThrottle):
			}
		}

		decimals, confirmedDecimals := g.classify(ctx, ref.Asset, &tally)
		if !confirmedDecimals || decimals == StandardDecimals {
			continue
		}
		confirmed++
		g.report(ctx, ref, decimals)
	}

	unresolved := g.publishGauges()
	g.logger.Info("decimals-guard: backfill complete",
		"window", g.backfillWindow, "scanned_pairs", len(refs), "confirmed_offenders", confirmed,
		"unresolved_assets", unresolved, "failed_reads", tally.failed)
	if tally.failed > 0 {
		return fmt.Errorf("decimals-guard: %d of %d lake decimals reads failed", tally.failed, tally.reads)
	}
	return nil
}

// publishGauges exports the stranded-write and unverified-asset counts and returns the latter.
func (g *Guard) publishGauges() (unresolved int) {
	g.mu.Lock()
	pending, unresolved := len(g.pending), len(g.unresolved)
	g.mu.Unlock()
	obs.DecimalsGuardPendingWrites.Set(float64(pending))
	obs.DecimalsGuardUnresolvedAssets.Set(float64(unresolved))
	return unresolved
}

// isCached reports whether asset's decimals() is in the resolved cache, so Backfill skips
// throttling a call that will not reach ClickHouse.
func (g *Guard) isCached(asset string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.resolved[asset]
	return ok
}

// classify resolves and caches a contract's decimals. confirmed=false when the value could not be
// established (resolution error, or not derivable); the caller must NOT alarm then.
func (g *Guard) classify(ctx context.Context, asset string, tally *passTally) (decimals uint32, confirmed bool) {
	g.mu.Lock()
	if d, ok := g.resolved[asset]; ok {
		g.mu.Unlock()
		return d, true
	}
	g.mu.Unlock()

	tally.reads++
	d, found, rerr := g.resolver.TokenDecimals(ctx, asset)
	if rerr != nil {
		// Transient lake error, or a key that isn't a valid contract strkey.
		// Not cached — retried next sweep.
		tally.failed++
		g.logger.Debug("decimals-guard: decimals resolve failed; will retry", "asset", asset, "err", rerr)
		return 0, false
	}
	if !found {
		// Not derivable yet: not cached (a later-captured instance is re-checked), never an offender.
		g.mu.Lock()
		_, known := g.unresolved[asset]
		g.unresolved[asset] = struct{}{}
		g.mu.Unlock()
		if !known {
			g.logger.Warn("decimals-guard: decimals() not derivable from the lake; pairs of this asset are priced on the 7-decimals assumption (runbook: dex.md)", "asset", asset)
		}
		return 0, false
	}
	g.remember(asset, d)
	return d, true
}

// report raises the alarm (metric + ERROR log, once per source+asset per process) and, when a Writer is wired, persists the
// confirmation for the API's read-time guard. The effects latch independently: the alarm on first observation, the write ONLY
// after a successful upsert (idempotent, so a retry or double-fire is safe).
func (g *Guard) report(ctx context.Context, ref timescale.SorobanDEXTradeRef, decimals uint32) {
	key := ref.Source + "\x00" + ref.Asset

	g.mu.Lock()
	_, alarmed := g.alarmed[key]
	_, persisted := g.persisted[key]
	if !alarmed {
		g.alarmed[key] = struct{}{}
	}
	g.mu.Unlock()

	// Observability alarm: fires exactly once per (source, asset).
	if !alarmed {
		obs.DEXTradeNonstandardDecimalsTotal.WithLabelValues(ref.Source, ref.Asset).Inc()
		g.logger.Error(
			"DEX trade for a non-7-decimal Soroban token — served price for pairs involving "+
				"this asset is silently skewed by 10^(7-decimals); apply decimals normalization "+
				"(runbook: dex.md)",
			"source", ref.Source,
			"asset", ref.Asset,
			"decimals", decimals,
			"price_skew_decades", skewDecades(decimals),
		)
	}

	if g.writer == nil || persisted {
		return
	}
	g.persist(ctx, ref, decimals)
}

// persist writes one confirmed offender. A failure parks it in pending, which every tick retries.
func (g *Guard) persist(ctx context.Context, ref timescale.SorobanDEXTradeRef, decimals uint32) {
	key := ref.Source + "\x00" + ref.Asset
	err := g.writer.UpsertNonstandardDecimalsAsset(ctx, ref.Asset, decimals, ref.Source)
	g.mu.Lock()
	if err == nil {
		delete(g.pending, key)
		g.persisted[key] = struct{}{}
	} else {
		g.pending[key] = pendingWrite{ref: ref, decimals: decimals}
	}
	g.mu.Unlock()
	if err != nil {
		g.logger.Warn(
			"decimals-guard: failed to persist confirmed nonstandard-decimals asset — "+
				"retried every tick; the API read-time serving guard stays unfed until a "+
				"write succeeds (metric + log alarm above are unaffected)",
			"source", ref.Source, "asset", ref.Asset, "decimals", decimals, "err", err,
		)
	}
}

// retryPending re-attempts every stranded durable write, whether or not its asset is enumerated again.
func (g *Guard) retryPending(ctx context.Context) {
	g.mu.Lock()
	stranded := make([]pendingWrite, 0, len(g.pending))
	for _, w := range g.pending {
		stranded = append(stranded, w)
	}
	g.mu.Unlock()
	for _, w := range stranded {
		g.persist(ctx, w.ref, w.decimals)
	}
}

// Reconcile re-reads every persisted `nonstandard_decimals_assets` row against the lake (bypassing the resolved cache): a read error or found=false leaves the row (hand-seeded rows must survive); a mismatch is counted, logged at ERROR and repaired toward the lake (upsert non-7, DELETE on 7). Errors only if the row load fails.
func (g *Guard) Reconcile(ctx context.Context) error {
	if g.reconciler == nil {
		return nil
	}
	rows, err := g.reconciler.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		return err
	}
	repaired := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if g.reconcileRow(ctx, row) {
			repaired++
		}
	}
	if repaired > 0 {
		g.logger.Info("decimals-guard: lockstep reconcile repaired drifted rows toward the lake",
			"rows", len(rows), "repaired", repaired)
	}
	return nil
}

// reconcileRowReadTimeout bounds one lake decimals() read: Reconcile runs serially after Sweep, so
// a hung ClickHouse query would otherwise stall the whole tick.
const reconcileRowReadTimeout = 5 * time.Second

// reconcileRow checks one persisted row against a fresh lake read and repairs a mismatch. It
// reports whether a repair was WRITTEN (a failed write reports false and is retried next tick).
func (g *Guard) reconcileRow(ctx context.Context, row timescale.NonstandardDecimalsAsset) bool {
	rctx, cancel := context.WithTimeout(ctx, reconcileRowReadTimeout)
	defer cancel()
	lake, found, rerr := g.resolver.TokenDecimals(rctx, row.Asset)
	if rerr != nil {
		g.logger.Debug("decimals-guard: lockstep lake read failed; leaving row as-is", "asset", row.Asset, "err", rerr)
		return false
	}
	if !found {
		g.logger.Debug("decimals-guard: lockstep lake read not derivable; leaving row as-is", "asset", row.Asset)
		return false
	}
	if int(lake) == row.Decimals {
		g.remember(row.Asset, lake)
		return false
	}

	obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("guard_reconcile", row.Asset).Inc()
	g.logger.Error(
		"decimals-guard: nonstandard_decimals_assets row DISAGREES with the lake's on-chain decimals() — "+
			"repairing toward the lake (every serving path normalised on the stale value until now)",
		"asset", row.Asset, "source", row.Source,
		"persisted_decimals", row.Decimals, "lake_decimals", lake,
	)

	if lake == StandardDecimals {
		if err := g.reconciler.DeleteNonstandardDecimalsAsset(ctx, row.Asset); err != nil {
			g.logger.Warn("decimals-guard: lockstep delete failed; retrying next tick", "asset", row.Asset, "err", err)
			return false
		}
		g.forgetPersisted(row.Asset)
	} else {
		if err := g.reconciler.UpsertNonstandardDecimalsAsset(ctx, row.Asset, lake, row.Source); err != nil {
			g.logger.Warn("decimals-guard: lockstep upsert failed; retrying next tick", "asset", row.Asset, "err", err)
			return false
		}
	}
	g.remember(row.Asset, lake)
	return true
}

// remember caches a CONFIRMED lake reading so the next Sweep agrees with what Reconcile verified.
func (g *Guard) remember(asset string, decimals uint32) {
	g.mu.Lock()
	g.resolved[asset] = decimals
	delete(g.unresolved, asset)
	g.mu.Unlock()
}

// forgetPersisted clears the durable-write latch and any stranded write for every (source, asset)
// key of a row Reconcile deleted, so a later confirmed non-7 reading is persisted again and a
// stale one is not.
func (g *Guard) forgetPersisted(asset string) {
	suffix := "\x00" + asset
	g.mu.Lock()
	for key := range g.persisted {
		if strings.HasSuffix(key, suffix) {
			delete(g.persisted, key)
		}
	}
	for key := range g.pending {
		if strings.HasSuffix(key, suffix) {
			delete(g.pending, key)
		}
	}
	g.mu.Unlock()
}

// skewDecades is the order of magnitude a served ratio is off for a pair with a 7-dp counter-leg:
// |7 - decimals|. Direction depends on whether the offender is the base or quote leg (see runbook).
func skewDecades(decimals uint32) int {
	d := int(decimals) - StandardDecimals
	if d < 0 {
		return -d
	}
	return d
}
