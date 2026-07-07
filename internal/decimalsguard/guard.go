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

// Guard periodically sweeps recently-DEX-traded Soroban tokens and raises
// obs.DEXTradeNonstandardDecimalsTotal for any whose on-chain decimals() is
// confirmed != 7. It is conservative: it alarms ONLY on a confirmed non-7
// value, never on a resolution error or a non-derivable declaration.
type Guard struct {
	reader   TradeReader
	resolver DecimalsResolver
	window   time.Duration
	logger   *slog.Logger

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
}

// New builds a Guard.
func New(reader TradeReader, resolver DecimalsResolver, opts Options) *Guard {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Guard{
		reader:   reader,
		resolver: resolver,
		window:   window,
		logger:   logger,
		resolved: make(map[string]uint32),
		fired:    make(map[string]struct{}),
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
		g.report(ref, decimals)
	}
	return nil
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
// process) and logs the detail the runbook needs.
func (g *Guard) report(ref timescale.SorobanDEXTradeRef, decimals uint32) {
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
