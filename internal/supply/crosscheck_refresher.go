package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
)

// SnapshotReader is the storage primitive the [CrossCheckRefresher]
// uses to fetch the most-recent snapshot for an asset_key. Production
// impl is timescale.Store.LatestSupply; the asset_key form is the
// supply-package canonical key (see [AssetKey]).
//
// Implementations MUST return a wrapped sentinel that satisfies
// `errors.Is(err, ErrNoSnapshot)` when the asset has no recorded
// snapshot — the refresher distinguishes that from transient read
// errors so the bootstrap state (no snapshot yet) doesn't surface as
// a read failure on the dashboard.
type SnapshotReader interface {
	LatestSupply(ctx context.Context, assetKey string) (Supply, error)
}

// ErrNoSnapshot is the sentinel a [SnapshotReader] returns when the
// asset has no rows in `asset_supply_history` yet. Callers wrap this
// (e.g. `fmt.Errorf("…: %w", ErrNoSnapshot)`) so `errors.Is` keeps
// working through the wrap chain.
//
// Production storage maps timescale.ErrNotFound → ErrNoSnapshot via
// the supplyStorageReader adapter in cmd/stellarindex-aggregator —
// the supply package itself doesn't import timescale.
var ErrNoSnapshot = errors.New("supply: no snapshot for asset_key")

// CrossCheckPair binds a classic asset's supply.AssetKey form (the
// "CODE:ISSUER" colon-separated key — produced by [AssetKey] for a
// classic asset) to its SAC-wrapper contract id (a bare C-strkey).
// Both must already be in the watched-set so the per-asset refreshers
// produce snapshots for them; the cross-check refresher reads those
// snapshots back.
type CrossCheckPair struct {
	ClassicKey string
	SACKey     string

	// WrapClass selects which invariant [CrossCheckForClass] checks
	// for this pair. Zero value ("") normalizes to [WrapClassPartial]
	// — the safe default — via [normalizeWrapClass], so existing
	// callers that don't set this field get the corrected
	// 2026-07-08 behaviour automatically rather than silently
	// reverting to the pre-fix equality compare.
	WrapClass WrapClass
}

// CrossCheckOutcomeKind is the per-pair outcome of one tick. Stable
// metric-label strings; the aggregator-level counter uses these
// values directly as the `outcome` label per
// `stellarindex_supply_cross_check_total`.
type CrossCheckOutcomeKind string

const (
	// CrossCheckOutcomeWithin — both snapshots loaded, divergence ≤
	// CrossCheckTolerance. The gauge is set to the divergence value
	// (≥0 stroops, almost always 0).
	CrossCheckOutcomeWithin CrossCheckOutcomeKind = "within"

	// CrossCheckOutcomeOver — both snapshots loaded, divergence >
	// CrossCheckTolerance. The gauge is set to the divergence; the
	// supply_cross_check_divergence alert fires after `for: 5m`.
	CrossCheckOutcomeOver CrossCheckOutcomeKind = "over"

	// CrossCheckOutcomeMissing — at least one of (classic, SAC) has
	// no snapshot in storage yet. Common during early bring-up
	// before either side's refresher has produced its first row.
	// The pair's gauge series is CLEARED — operators see the bootstrap
	// signal via the counter (and its alert) rather than a zero or a
	// stale reading that would imply "checked, agreed".
	CrossCheckOutcomeMissing CrossCheckOutcomeKind = "missing_snapshot"

	// CrossCheckOutcomeReadError — a transient storage error fired
	// while reading either side. The gauge series is CLEARED; the
	// surface is the counter so operators can chart and alert on a
	// sustained read-failure rate.
	CrossCheckOutcomeReadError CrossCheckOutcomeKind = "read_error"

	// CrossCheckOutcomeMisaligned — both snapshots loaded, but their
	// LedgerSequences are further apart than
	// [CrossCheckLedgerTolerance], so the invariant is not evaluable
	// (MNY-04). Neither passes nor pages: the gauge series is CLEARED
	// (a stale reading must not be served as agreement) and no
	// divergence is computed (a lagging snapshot on either side makes
	// the subset bound meaningless in BOTH directions — a stale
	// classic total can sit below a fresh SAC total with no over-mint
	// whatsoever, and a stale SAC total can hide a real one).
	// Operators chart and alert on this via the counter.
	CrossCheckOutcomeMisaligned CrossCheckOutcomeKind = "misaligned"

	// CrossCheckOutcomeUnchecked — a partial-wrap pair whose classic
	// snapshot carries no SACWrappedStroops, so leg 2 (the only leg that
	// feeds the divergence) was never evaluated. That is the steady state
	// for any asset with no sac_balance_observations row, so it is not a
	// transient. Like Missing / Misaligned the gauge series is CLEARED:
	// a divergence of 0 here would read as "checked, agreed".
	CrossCheckOutcomeUnchecked CrossCheckOutcomeKind = "unchecked"
)

// CrossCheckLedgerTolerance is the largest |classic.LedgerSequence −
// sac.LedgerSequence| gap at which the two snapshots are still treated
// as describing the same moment.
//
// The refresher reads each side's LATEST snapshot independently, so
// nothing in the read path guarantees they were computed at the same
// ledger; without a bound the comparison silently pits an hours-old
// total against a fresh one.
//
// 1000 ledgers (~1.4h at 5s close times) is
// [DefaultStaleComponentLedgers] — the lag this package already
// declares acceptable for a supply component. Reusing it makes the two
// freshness defences agree rather than each picking its own number,
// and it is comfortably wider than the 5m aggregator refresh cadence
// that produces both sides, so steady state never trips it.
const CrossCheckLedgerTolerance uint32 = DefaultStaleComponentLedgers

// ledgerGap returns |a − b| for two ledger sequences without the
// uint32 underflow a bare subtraction would produce.
func ledgerGap(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// CrossCheckOutcome is the per-pair result of one tick. The
// refresher emits one Outcome per configured pair regardless of
// success/failure — the per-tick slice has stable length so
// aggregator-level counter cardinality stays bounded by the pair
// count.
type CrossCheckOutcome struct {
	Pair   CrossCheckPair
	Kind   CrossCheckOutcomeKind
	Result CrossCheckResult // populated on Within / Over
	Err    error            // populated on Missing / ReadError / Misaligned
}

// CrossCheckEmitter is the metric-emission seam — kept as an
// interface so the refresher stays Prometheus-agnostic and unit
// tests can capture emitted values without a registry.
//
// Production impl wraps obs.SupplyCrossCheckDivergenceStroops +
// obs.SupplyCrossCheckTotal; the wiring lives in
// cmd/stellarindex-aggregator/main.go where the supply package
// can stay free of the obs dependency.
type CrossCheckEmitter interface {
	// Divergence sets the per-asset gauge to the stroop divergence,
	// labelled by the pair's WrapClass. Called only on Within / Over
	// outcomes. Negative values are a caller bug ([CrossCheck] /
	// [CrossCheckSubsetBound] always return a non-negative value).
	//
	// wrapClass is carried through as a metric label (2026-07-08,
	// BACKLOG #59) so operators can see which invariant produced a
	// given reading — purely observational: the alert threshold
	// doesn't need to filter on it, because DivergenceStroops itself
	// is already zero in the benign partial-wrap case (see
	// [CrossCheckSubsetBound]).
	Divergence(classicKey string, wrapClass WrapClass, stroops float64)

	// ClearDivergence removes the pair's gauge series. Called on
	// every outcome that evaluated nothing (Missing / ReadError /
	// Misaligned): a gauge left at its last value is re-exported on
	// every scrape, so a stalled pair would read as its last verdict
	// indefinitely. Absence is the honest state; the outcome counter
	// carries the reason.
	ClearDivergence(classicKey string, wrapClass WrapClass)

	// Outcome increments the per-outcome counter, labelled by
	// WrapClass. Called for every outcome (including Missing /
	// ReadError) so operators see the "is the cross-checker even
	// running" signal.
	Outcome(kind CrossCheckOutcomeKind, wrapClass WrapClass)
}

// CrossCheckRefresher runs one cross-check cycle per [Tick] call.
// Loads the most-recent snapshot for each side of every configured
// pair, runs [CrossCheck] on the pair, and emits the result.
//
// The refresher is policy-free: it neither chooses pairs nor decides
// tolerance. Pairs come from the aggregator at construction time
// (derived from `[supply].sac_wrappers` ∩ watched-sets) and the
// tolerance is the package-level [CrossCheckTolerance].
type CrossCheckRefresher struct {
	pairs   []CrossCheckPair
	reader  SnapshotReader
	emitter CrossCheckEmitter
	logger  *slog.Logger
}

// NewCrossCheckRefresher constructs the refresher. Empty pairs is
// valid (the operator hasn't configured any SAC wrappers in the
// watched set yet) — Tick is a no-op and emits no outcomes in that
// case. Returns an error on duplicate pairs to surface operator
// config bugs early; sorts the input so per-tick emission order is
// stable across process restarts.
func NewCrossCheckRefresher(pairs []CrossCheckPair, reader SnapshotReader, emitter CrossCheckEmitter, logger *slog.Logger) (*CrossCheckRefresher, error) {
	if reader == nil {
		return nil, errors.New("supply: cross-check refresher needs a SnapshotReader")
	}
	if emitter == nil {
		return nil, errors.New("supply: cross-check refresher needs a CrossCheckEmitter")
	}
	if logger == nil {
		return nil, errors.New("supply: cross-check refresher needs a logger")
	}
	for i, p := range pairs {
		if p.ClassicKey == "" {
			return nil, fmt.Errorf("supply: cross-check pair[%d] has empty ClassicKey", i)
		}
		if p.SACKey == "" {
			return nil, fmt.Errorf("supply: cross-check pair[%d] (%s) has empty SACKey", i, p.ClassicKey)
		}
	}
	sorted := make([]CrossCheckPair, len(pairs))
	copy(sorted, pairs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ClassicKey < sorted[j].ClassicKey })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].ClassicKey == sorted[i-1].ClassicKey {
			return nil, fmt.Errorf("supply: cross-check pairs duplicate ClassicKey %q", sorted[i].ClassicKey)
		}
	}
	return &CrossCheckRefresher{
		pairs:   sorted,
		reader:  reader,
		emitter: emitter,
		logger:  logger,
	}, nil
}

// Tick runs one cycle across every configured pair. Per-pair errors
// don't bubble up — they're logged and surfaced via the outcome
// slice so a single transient storage hiccup doesn't drop the
// remaining pairs' cross-checks. The slice has stable length =
// len(pairs) so callers can size dashboards accordingly.
func (r *CrossCheckRefresher) Tick(ctx context.Context) []CrossCheckOutcome {
	if len(r.pairs) == 0 {
		return nil
	}
	out := make([]CrossCheckOutcome, 0, len(r.pairs))
	for _, p := range r.pairs {
		wrapClass := normalizeWrapClass(p.WrapClass)
		outcome := r.tickOne(ctx, p)
		r.emitter.Outcome(outcome.Kind, wrapClass)
		switch outcome.Kind {
		case CrossCheckOutcomeWithin, CrossCheckOutcomeOver:
			stroops, _ := outcome.Result.DivergenceStroops.Float64() // i128:ok Prometheus gauge value; the NUMERIC record keeps full precision
			r.emitter.Divergence(p.ClassicKey, wrapClass, stroops)
		case CrossCheckOutcomeMissing, CrossCheckOutcomeReadError, CrossCheckOutcomeMisaligned, CrossCheckOutcomeUnchecked:
			r.emitter.ClearDivergence(p.ClassicKey, wrapClass)
		}
		out = append(out, outcome)
	}
	return out
}

func (r *CrossCheckRefresher) tickOne(ctx context.Context, p CrossCheckPair) CrossCheckOutcome {
	classic, err := r.reader.LatestSupply(ctx, p.ClassicKey)
	if err != nil {
		if errors.Is(err, ErrNoSnapshot) {
			r.logger.Debug("cross-check: no classic snapshot yet",
				"classic_key", p.ClassicKey, "sac_key", p.SACKey)
			return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeMissing, Err: err}
		}
		r.logger.Warn("cross-check: classic read failed",
			"classic_key", p.ClassicKey, "err", err)
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeReadError, Err: err}
	}
	sac, err := r.reader.LatestSupply(ctx, p.SACKey)
	if err != nil {
		if errors.Is(err, ErrNoSnapshot) {
			r.logger.Debug("cross-check: no sac snapshot yet",
				"classic_key", p.ClassicKey, "sac_key", p.SACKey)
			return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeMissing, Err: err}
		}
		r.logger.Warn("cross-check: sac read failed",
			"sac_key", p.SACKey, "err", err)
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeReadError, Err: err}
	}
	// MNY-04: each snapshot is the LATEST for its own asset_key, written
	// by its own per-asset refresher, so they can describe wildly
	// different ledgers; CrossCheckForClass refuses such a pair rather
	// than publish a verdict the data can't support.
	result, err := CrossCheckForClass(classic, sac, p.WrapClass)
	if errors.Is(err, ErrCrossCheckMisaligned) {
		r.logger.Warn("cross-check: snapshots misaligned, comparison skipped",
			"classic_key", p.ClassicKey,
			"sac_key", p.SACKey,
			"classic_ledger", classic.LedgerSequence,
			"sac_ledger", sac.LedgerSequence,
			"tolerance_ledgers", CrossCheckLedgerTolerance,
			"err", err)
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeMisaligned, Err: err}
	}
	if err != nil {
		r.logger.Warn("cross-check: compare failed",
			"classic_key", p.ClassicKey, "sac_key", p.SACKey, "err", err)
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeReadError, Err: err}
	}
	if result.WrapClass == WrapClassPartial && !result.SubsetBoundChecked {
		// A partial-wrap check whose escrow leg was not evaluated checked
		// nothing that feeds the divergence (leg 1 is diagnostic only).
		r.logger.Debug("cross-check: escrow leg UNCHECKED (no sac_wrapped_stroops on the classic snapshot)",
			"classic_key", p.ClassicKey,
			"sac_key", p.SACKey,
			"classic_total", result.ClassicTotal.String(),
			"sac_total", result.SACTotal.String(),
			"over_mint_stroops", bigOrUnset(result.OverMintStroops))
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeUnchecked, Result: result}
	}
	if result.WithinTolerance {
		return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeWithin, Result: result}
	}
	r.logger.Warn("cross-check: divergence over tolerance",
		"classic_key", p.ClassicKey,
		"sac_key", p.SACKey,
		"wrap_class", string(result.WrapClass),
		"divergence_stroops", result.DivergenceStroops.String(),
		// Leg 2 (escrow excess) is what breached: mints are missing or
		// burns double-counted. Leg 1 (over-mint) is diagnostic context
		// only. See the supply-cross-check-divergence runbook.
		"over_mint_stroops", bigOrUnset(result.OverMintStroops),
		"escrow_excess_stroops", bigOrUnset(result.EscrowExcessStroops),
		"subset_bound_checked", result.SubsetBoundChecked,
		"sac_wrapped_stroops", bigOrUnset(result.SACWrapped))
	return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeOver, Result: result}
}

// bigOrUnset renders an optional *big.Int for a log line, mapping nil
// to "unset" rather than "<nil>" or a misleading "0" — the per-leg
// fields are nil exactly when that leg was not evaluated.
func bigOrUnset(v *big.Int) string {
	if v == nil {
		return "unset"
	}
	return v.String()
}
