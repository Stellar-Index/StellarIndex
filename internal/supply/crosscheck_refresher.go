package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
)

// SnapshotReader fetches the latest snapshot for an asset_key; implementations MUST return an error satisfying errors.Is(err, ErrNoSnapshot) when none exists.
type SnapshotReader interface {
	LatestSupply(ctx context.Context, assetKey string) (Supply, error)
}

// ErrNoSnapshot is what a [SnapshotReader] returns (wrapped with %w) when the asset has no asset_supply_history rows.
var ErrNoSnapshot = errors.New("supply: no snapshot for asset_key")

// CrossCheckPair binds a classic asset's [AssetKey] ("CODE:ISSUER") to its SAC-wrapper contract id (bare C-strkey); both must be in the watched-set.
type CrossCheckPair struct {
	ClassicKey string
	SACKey     string

	// WrapClass selects the invariant [CrossCheckForClass] checks. The zero value normalizes to
	// [WrapClassPartial] (subset bound, the safe default) via [normalizeWrapClass].
	WrapClass WrapClass
}

// CrossCheckOutcomeKind is the per-pair outcome of one tick; the strings are stable, being the `outcome` metric label.
type CrossCheckOutcomeKind string

const (
	// CrossCheckOutcomeWithin — both snapshots loaded, divergence ≤ CrossCheckTolerance; the gauge
	// is set to the divergence (almost always 0).
	CrossCheckOutcomeWithin CrossCheckOutcomeKind = "within"

	// CrossCheckOutcomeOver — both snapshots loaded, divergence > CrossCheckTolerance; the
	// supply_cross_check_divergence alert fires after `for: 5m`.
	CrossCheckOutcomeOver CrossCheckOutcomeKind = "over"

	// CrossCheckOutcomeMissing: at least one side has no snapshot yet (early bring-up). The gauge
	// series is CLEARED so a zero or stale reading can't imply "checked, agreed".
	CrossCheckOutcomeMissing CrossCheckOutcomeKind = "missing_snapshot"

	// CrossCheckOutcomeReadError — a transient storage error reading either side. The gauge series
	// is CLEARED; the counter carries the sustained read-failure rate.
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

// CrossCheckLedgerTolerance is the largest ledger gap (1000, ~1.4h) at which two independently-read LATEST snapshots still describe the same moment; it bounds alignment, not component age.
const CrossCheckLedgerTolerance uint32 = 1000

// ledgerGap returns |a − b| without the uint32 underflow of a bare subtraction.
func ledgerGap(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// CrossCheckOutcome is the per-pair result of one tick, emitted per configured pair so counter cardinality stays bounded.
type CrossCheckOutcome struct {
	Pair   CrossCheckPair
	Kind   CrossCheckOutcomeKind
	Result CrossCheckResult // populated on Within / Over
	Err    error            // populated on Missing / ReadError / Misaligned
}

// CrossCheckEmitter is the metric-emission seam that keeps the refresher Prometheus-agnostic.
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

	// Outcome increments the per-outcome counter, labelled by WrapClass. It is called for every
	// outcome, including Missing and ReadError, so operators can tell the checker is running.
	Outcome(kind CrossCheckOutcomeKind, wrapClass WrapClass)
}

// CrossCheckRefresher runs one cross-check cycle per [Tick]: loads both sides' latest snapshots per pair, runs [CrossCheck] and emits the result; policy-free.
type CrossCheckRefresher struct {
	pairs   []CrossCheckPair
	reader  SnapshotReader
	emitter CrossCheckEmitter
	logger  *slog.Logger
}

// NewCrossCheckRefresher constructs the refresher; empty pairs is valid (Tick is a no-op), duplicate pairs are an error, and input is sorted for stable emission order.
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

// Tick runs one cycle across every pair; per-pair errors are logged and surfaced in the outcome slice (length = len(pairs)) so one failure doesn't drop the others.
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
	// Each snapshot is the LATEST for its own asset_key, so the two can describe wildly different
	// ledgers; CrossCheckForClass refuses such a pair rather than publish an unsupported verdict.
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
		// Without the escrow leg nothing feeding the divergence was checked (leg 1 is diagnostic).
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
		// Leg 2 (escrow excess) breached: mints are missing or burns double-counted. Leg 1
		// (over-mint) is diagnostic context only.
		"over_mint_stroops", bigOrUnset(result.OverMintStroops),
		"escrow_excess_stroops", bigOrUnset(result.EscrowExcessStroops),
		"subset_bound_checked", result.SubsetBoundChecked,
		"sac_wrapped_stroops", bigOrUnset(result.SACWrapped))
	return CrossCheckOutcome{Pair: p, Kind: CrossCheckOutcomeOver, Result: result}
}

// bigOrUnset renders an optional *big.Int, mapping nil to "unset" rather than a misleading "0":
// a per-leg field is nil exactly when that leg was not evaluated.
func bigOrUnset(v *big.Int) string {
	if v == nil {
		return "unset"
	}
	return v.String()
}
