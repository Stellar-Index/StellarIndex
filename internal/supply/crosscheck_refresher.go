package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
)

// SnapshotReader fetches the latest snapshot for an asset_key (production: timescale.Store.LatestSupply).
// Implementations MUST return an error satisfying errors.Is(err, ErrNoSnapshot) when none exists,
// so bootstrap isn't reported as a read failure.
type SnapshotReader interface {
	LatestSupply(ctx context.Context, assetKey string) (Supply, error)
}

// ErrNoSnapshot is what a [SnapshotReader] returns when the asset has no asset_supply_history rows;
// wrap it with %w. The supplyStorageReader adapter in cmd/stellarindex-aggregator maps
// timescale.ErrNotFound to it, since supply doesn't import timescale.
var ErrNoSnapshot = errors.New("supply: no snapshot for asset_key")

// CrossCheckPair binds a classic asset's [AssetKey] ("CODE:ISSUER") to its SAC-wrapper contract
// id (bare C-strkey). Both must be in the watched-set so their refreshers produce the snapshots
// this refresher reads back.
type CrossCheckPair struct {
	ClassicKey string
	SACKey     string

	// WrapClass selects the invariant [CrossCheckForClass] checks. The zero value normalizes to
	// [WrapClassPartial] (subset bound, the safe default) via [normalizeWrapClass].
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

	// CrossCheckOutcomeMissing: at least one side has no snapshot yet (early bring-up). The gauge
	// series is CLEARED so a zero or stale reading can't imply "checked, agreed".
	CrossCheckOutcomeMissing CrossCheckOutcomeKind = "missing_snapshot"

	// CrossCheckOutcomeReadError — a transient storage error fired
	// while reading either side. The gauge series is CLEARED; the
	// surface is the counter so operators can chart and alert on a
	// sustained read-failure rate.
	CrossCheckOutcomeReadError CrossCheckOutcomeKind = "read_error"

	// CrossCheckOutcomeMisaligned: both snapshots loaded but further apart than
	// [CrossCheckLedgerTolerance], so the invariant is not evaluable. The gauge series is CLEARED
	// and no divergence is computed: a lagging snapshot on either side makes the subset bound
	// meaningless in both directions.
	CrossCheckOutcomeMisaligned CrossCheckOutcomeKind = "misaligned"

	// CrossCheckOutcomeUnchecked: a partial-wrap pair whose classic snapshot has no SACWrappedStroops,
	// so leg 2 (the only leg feeding the divergence) never ran; steady state for assets with no
	// sac_balance_observations row. The gauge series is CLEARED: 0 would read as "checked, agreed".
	CrossCheckOutcomeUnchecked CrossCheckOutcomeKind = "unchecked"
)

// CrossCheckLedgerTolerance is the largest |classic.LedgerSequence - sac.LedgerSequence| at which two
// independently-read LATEST snapshots still describe the same moment. 1000 ledgers (~1.4h) exceeds the
// 5m refresh cadence; it bounds alignment, not component age.
const CrossCheckLedgerTolerance uint32 = 1000

// ledgerGap returns |a − b| for two ledger sequences without the
// uint32 underflow a bare subtraction would produce.
func ledgerGap(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// CrossCheckOutcome is the per-pair result of one tick. One is emitted per configured pair
// regardless of outcome, keeping counter cardinality bounded by the pair count.
type CrossCheckOutcome struct {
	Pair   CrossCheckPair
	Kind   CrossCheckOutcomeKind
	Result CrossCheckResult // populated on Within / Over
	Err    error            // populated on Missing / ReadError / Misaligned
}

// CrossCheckEmitter is the metric-emission seam, keeping the refresher Prometheus-agnostic; the
// production impl (obs.SupplyCrossCheck*) is wired in cmd/stellarindex-aggregator/main.go.
type CrossCheckEmitter interface {
	// Divergence sets the per-asset gauge to the stroop divergence, labelled by WrapClass. Called
	// only on Within / Over; the value is non-negative. The label is observational only: alerts
	// needn't filter on it because the benign partial-wrap divergence is already zero (see
	// [CrossCheckSubsetBound]).
	Divergence(classicKey string, wrapClass WrapClass, stroops float64)

	// ClearDivergence removes the pair's gauge series on outcomes that evaluated nothing (Missing /
	// ReadError / Misaligned): a gauge left at its last value is re-exported on every scrape, so a
	// stalled pair would read as its last verdict. The outcome counter carries the reason.
	ClearDivergence(classicKey string, wrapClass WrapClass)

	// Outcome increments the per-outcome counter, labelled by
	// WrapClass. Called for every outcome (including Missing /
	// ReadError) so operators see the "is the cross-checker even
	// running" signal.
	Outcome(kind CrossCheckOutcomeKind, wrapClass WrapClass)
}

// CrossCheckRefresher runs one cross-check cycle per [Tick]: it loads the latest snapshot for each
// side of every pair, runs [CrossCheck], and emits the result. It is policy-free: pairs come from
// the aggregator (`[supply].sac_wrappers` intersect watched-sets), tolerance is [CrossCheckTolerance].
type CrossCheckRefresher struct {
	pairs   []CrossCheckPair
	reader  SnapshotReader
	emitter CrossCheckEmitter
	logger  *slog.Logger
}

// NewCrossCheckRefresher constructs the refresher. Empty pairs is valid (Tick is a no-op).
// Duplicate pairs are an error to surface config bugs; input is sorted so emission order is
// stable across restarts.
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

// Tick runs one cycle across every pair. Per-pair errors are logged and surfaced in the outcome
// slice (length = len(pairs)) so one storage hiccup doesn't drop the other pairs' checks.
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
	// Each snapshot is the LATEST for its own asset_key, written
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
		// only. See the supply runbook (supply.md).
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
