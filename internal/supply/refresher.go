package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"
)

// LedgerLookup resolves the most recent known chain ledger: the `ledgerstream` cursor clamped to the newest landed stellar.ledgers row at or before it.
type LedgerLookup interface {
	LatestKnownLedger(ctx context.Context) (uint32, time.Time, error)
}

// SnapshotComputer computes one [Supply] for the given ledger (production: *XLMComputer).
type SnapshotComputer interface {
	Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error)
}

// SnapshotInserter writes one [Supply] to persistence (production: timescale.Store.InsertSupply), idempotent on (asset_key, ledger_sequence).
type SnapshotInserter interface {
	InsertSupply(ctx context.Context, snap Supply) error
}

// Outcome is what one [Refresher.Tick] produced; OutcomeKind is a stable string suitable for a metric label.
type Outcome struct {
	Kind     OutcomeKind
	Snapshot Supply // populated on OutcomeKindOK only
	Err      error  // populated on every error outcome
	// BandBreach is "up" or "down" when a written snapshot's total moved
	// more than [WriteBandFactor]x against the previous one this Refresher wrote.
	BandBreach string
}

// OutcomeKind identifies a refresh outcome; values are stable metric-label strings.
type OutcomeKind string

const (
	OutcomeKindOK               OutcomeKind = "ok"
	OutcomeKindNoLedger         OutcomeKind = "no_ledger"         // LedgerLookup error
	OutcomeKindNoObservation    OutcomeKind = "no_observation"    // ChainReader fell through with no static fallback either
	OutcomeKindComputeError     OutcomeKind = "compute_error"     // computer failed for non-observation reasons
	OutcomeKindStaleComponent   OutcomeKind = "stale_component"   // a component observation lags the snapshot ledger past the configured threshold AND either moved since the last tick (or this is the first lagging tick), or has been frozen past [DefaultMaxDormantComponentLedgers]. Says nothing about producer liveness inside the horizon: a producer that dies with a frozen anchor reports dormant until then.
	OutcomeKindMissingFreshness OutcomeKind = "missing_freshness" // strict mode + MinComponentLedger==0 (no signal); reject rather than publish without a freshness anchor
	OutcomeKindMissingBaseline  OutcomeKind = "missing_baseline"  // migration 0088: SEP-41 total negative because the pre-Soroban genesis baseline hasn't been seeded yet — a range-scoped-baseline-missing condition (needs `stellarindex-ops supply seed-sep41-genesis`), NOT indexer corruption. Benign: excluded from error_dominant.
	OutcomeKindDormant          OutcomeKind = "dormant"           // MinComponentLedger lags past threshold but is UNCHANGED tick-over-tick; the last observation is re-stamped as current (snapshot inserted). NOT evidence the producer is alive: the anchor is a producer-wide watermark, so assets dormant together mean a producer stalled (the supply_refresh_dormant_fleet alert), and [DefaultMaxDormantComponentLedgers] bounds it per asset — past it the gate fails closed to stale_component.
	OutcomeKindWriteError       OutcomeKind = "write_error"       // InsertSupply failed
	OutcomeKindStaticReserve    OutcomeKind = "static_reserve"    // snapshot inserted, but its reserve balances came from the dated static map (BasisXLMSDFReserveExclusionStatic), not the live observer. Not benign: counted by the error_dominant alert so a sustained fallback pages.
)

// DefaultStaleComponentLedgers is the default freshness threshold: a snapshot whose MinComponentLedger lags by more than 1000 ledgers (~85 min) is rejected; see [WithStaleComponentLedgers].
const DefaultStaleComponentLedgers uint32 = 1000

// DefaultMaxDormantComponentLedgers (17280 ledgers, ~24 h) bounds how long an unchanged component observation counts as dormant before it is refused as stale_component (ADR-0011: don't fabricate freshness).
const DefaultMaxDormantComponentLedgers uint32 = 17_280

// WriteBandFactor bounds the tick-over-tick total_supply move written without flagging; flag-only, a legitimate mint can 10x a young token, so a breach is recorded, never refused.
const WriteBandFactor = 10

// Refresher runs one supply-snapshot cycle per Tick, bound to one watched asset.
type Refresher struct {
	ledgers                   LedgerLookup
	computer                  SnapshotComputer
	inserter                  SnapshotInserter
	logger                    *slog.Logger
	staleComponentLedger      uint32
	staleComponentByAsset     map[string]uint32
	strictFreshnessRequired   bool
	maxDormantComponentLedger uint32

	// lastComponentLedger remembers each asset's last-evaluated MinComponentLedger. A DORMANT asset's value freezes while the tip advances,
	// which would reject every future tick, so an UNCHANGED value is accepted (OutcomeKindDormant) up to the dormancy horizon; a moved or
	// first-seen lagging value is rejected.
	lastComponentLedger map[string]uint32

	// lastWrittenTotal is the total_supply of the last snapshot this
	// Refresher wrote per asset_key; the write band compares against it.
	lastWrittenTotal map[string]*big.Int
}

// RefresherOption tunes a [Refresher].
type RefresherOption func(*Refresher)

// WithStaleComponentLedgers overrides the freshness threshold: reject when LedgerSequence - MinComponentLedger exceeds it AND MinComponentLedger > 0; zero disables the gate.
func WithStaleComponentLedgers(maxLag uint32) RefresherOption {
	return func(r *Refresher) {
		r.staleComponentLedger = maxLag
	}
}

// WithStaleComponentLedgersFor sets a per-asset stale-component threshold; assetKey is exact-match in [AssetKey] form (normalise via [CanonicalizeStaleComponentLedgers]), zero disables the gate for it.
func WithStaleComponentLedgersFor(assetKey string, maxLag uint32) RefresherOption {
	return func(r *Refresher) {
		if r.staleComponentByAsset == nil {
			r.staleComponentByAsset = make(map[string]uint32)
		}
		r.staleComponentByAsset[assetKey] = maxLag
	}
}

// WithMaxDormantComponentLedgers overrides how far the snapshot ledger may run ahead of a FROZEN MinComponentLedger before it is a stalled observer ([OutcomeKindStaleComponent]); zero leaves it unbounded, so a dead observer looks dormant.
func WithMaxDormantComponentLedgers(maxDormant uint32) RefresherOption {
	return func(r *Refresher) {
		r.maxDormantComponentLedger = maxDormant
	}
}

// WithStrictFreshnessRequired rejects snapshots with `MinComponentLedger == 0` ([OutcomeKindMissingFreshness]); default false so producers without freshness keep publishing.
func WithStrictFreshnessRequired(strict bool) RefresherOption {
	return func(r *Refresher) {
		r.strictFreshnessRequired = strict
	}
}

// NewRefresher constructs the Refresher.
func NewRefresher(ledgers LedgerLookup, computer SnapshotComputer, inserter SnapshotInserter, logger *slog.Logger, opts ...RefresherOption) *Refresher {
	if logger == nil {
		// Tick derefs the logger on a background timer; default it so a
		// nil can't nil-panic the refresh goroutine minutes after boot.
		logger = slog.Default()
	}
	r := &Refresher{
		ledgers:                   ledgers,
		computer:                  computer,
		inserter:                  inserter,
		logger:                    logger,
		staleComponentLedger:      DefaultStaleComponentLedgers,
		maxDormantComponentLedger: DefaultMaxDormantComponentLedgers,
		lastComponentLedger:       make(map[string]uint32),
		lastWrittenTotal:          make(map[string]*big.Int),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Tick runs one refresh cycle (resolve latest ledger, compute, insert idempotently) and returns an [Outcome]; it logs rather than returns errors so a transient issue never crashes the aggregator loop.
func (r *Refresher) Tick(ctx context.Context) Outcome {
	ledger, observedAt, err := r.ledgers.LatestKnownLedger(ctx)
	if err != nil {
		r.logger.Warn("supply refresh: no ledger", "err", err)
		return Outcome{Kind: OutcomeKindNoLedger, Err: err}
	}

	snap, err := r.computer.Compute(ctx, ledger, observedAt)
	if err != nil {
		// ErrNoObservation (live and static both fell through) is split out for the bootstrap-progress signal.
		// ErrNegativeTotalMissingBaseline (unseeded SAC-wrapper opening balance, so Σburn > Σmint) routes to
		// `missing_baseline`, excluded from error_dominant; once seeded, a still-negative total is
		// ErrNegativeTotalSupply -> compute_error, which pages.
		kind := OutcomeKindComputeError
		switch {
		case errors.Is(err, ErrNoObservation):
			kind = OutcomeKindNoObservation
		case errors.Is(err, ErrNegativeTotalMissingBaseline), errors.Is(err, ErrGenesisBaselineNotSeeded):
			kind = OutcomeKindMissingBaseline
		}
		r.logger.Warn("supply refresh: compute failed",
			"err", err, "ledger", ledger, "kind", string(kind))
		return Outcome{Kind: kind, Err: err}
	}

	// Strict mode rejects snapshots with NO freshness signal (MinComponentLedger == 0), or built from the
	// static reserve map, instead of the permissive "let it through".
	staticReserve := snap.Basis == BasisXLMSDFReserveExclusionStatic
	if r.strictFreshnessRequired && (snap.MinComponentLedger == 0 || staticReserve) {
		err := fmt.Errorf("supply: strict-freshness mode — snapshot has no MinComponentLedger anchor (basis %s)", snap.Basis)
		r.logger.Warn("supply refresh: rejecting freshness-less snapshot under strict mode",
			"asset", snap.AssetKey,
			"basis", string(snap.Basis),
			"snapshot_ledger", snap.LedgerSequence)
		return Outcome{Kind: OutcomeKindMissingFreshness, Err: err, Snapshot: snap}
	}

	// Reject snapshots whose component observations lag the snapshot ledger by more than the threshold;
	// a per-asset staleComponentByAsset entry wins over the global one, and zero disables the gate.
	if outcome, handled := r.applyStaleComponentGate(ctx, snap); handled {
		return outcome
	}

	if err := r.inserter.InsertSupply(ctx, snap); err != nil {
		r.logger.Error("supply refresh: insert failed",
			"err", err, "asset", snap.AssetKey, "ledger", snap.LedgerSequence)
		return Outcome{Kind: OutcomeKindWriteError, Err: err, Snapshot: snap}
	}
	breach := r.checkWriteBand(snap)

	if staticReserve {
		r.logger.Warn("supply refresh: published from the static reserve-balance map, live account observer could not answer",
			"asset", snap.AssetKey,
			"ledger", snap.LedgerSequence,
			"circulating", snap.CirculatingSupply.String())
		return Outcome{Kind: OutcomeKindStaticReserve, Snapshot: snap, BandBreach: breach}
	}
	r.logger.Debug("supply refresh ok",
		"asset", snap.AssetKey,
		"ledger", snap.LedgerSequence,
		"circulating", snap.CirculatingSupply.String())
	return Outcome{Kind: OutcomeKindOK, Snapshot: snap, BandBreach: breach}
}

// applyStaleComponentGate returns (outcome, true) when the gate decides the tick (stale-component REJECTION, or dormant ACCEPT), else
// (zero, false). Past the threshold a CHANGED (or first-tick lagging) value rejects; an UNCHANGED one re-stamps (OutcomeKindDormant)
// within the dormancy horizon, so a dead producer reads as dormant until [DefaultMaxDormantComponentLedgers].
func (r *Refresher) applyStaleComponentGate(ctx context.Context, snap Supply) (Outcome, bool) {
	threshold := r.staleComponentLedger
	thresholdSource := "default"
	if r.staleComponentByAsset != nil {
		if perAsset, ok := r.staleComponentByAsset[snap.AssetKey]; ok {
			threshold = perAsset
			thresholdSource = "per_asset"
		}
	}
	// Gate disabled, or the computer didn't populate freshness
	// → no opinion, fall through.
	if threshold == 0 || snap.MinComponentLedger == 0 {
		return Outcome{}, false
	}
	withinThreshold := snap.LedgerSequence <= snap.MinComponentLedger ||
		snap.LedgerSequence-snap.MinComponentLedger <= threshold
	if withinThreshold {
		// Fresh — track it so a later move into the lagging band reads
		// as a CHANGE (producer regressing), not a cold-start.
		r.lastComponentLedger[snap.AssetKey] = snap.MinComponentLedger
		return Outcome{}, false
	}

	last, seen := r.lastComponentLedger[snap.AssetKey]
	r.lastComponentLedger[snap.AssetKey] = snap.MinComponentLedger
	dormant := seen && last == snap.MinComponentLedger
	if !dormant {
		err := fmt.Errorf("supply: stale component — snapshot ledger %d, min component ledger %d, gap %d > threshold %d",
			snap.LedgerSequence, snap.MinComponentLedger,
			snap.LedgerSequence-snap.MinComponentLedger, threshold)
		r.logger.Warn("supply refresh: rejecting stale-component snapshot",
			"asset", snap.AssetKey,
			"snapshot_ledger", snap.LedgerSequence,
			"min_component_ledger", snap.MinComponentLedger,
			"gap", snap.LedgerSequence-snap.MinComponentLedger,
			"threshold", threshold,
			"threshold_source", thresholdSource,
			"first_observation", !seen)
		return Outcome{Kind: OutcomeKindStaleComponent, Err: err, Snapshot: snap}, true
	}
	// The dormancy carve-out is BOUNDED: a frozen MinComponentLedger is what a dead observer looks like,
	// so past the horizon fail closed as stale_component (not the alert-excluded `dormant`) and WARN with
	// the knob name so the operator can tell a long-dormant asset from a stalled producer.
	if r.maxDormantComponentLedger > 0 &&
		snap.LedgerSequence-snap.MinComponentLedger > r.maxDormantComponentLedger {
		err := fmt.Errorf("supply: stalled component observer — min component ledger %d frozen for %d ledgers (snapshot ledger %d), past dormancy horizon %d",
			snap.MinComponentLedger, snap.LedgerSequence-snap.MinComponentLedger,
			snap.LedgerSequence, r.maxDormantComponentLedger)
		r.logger.Warn("supply refresh: rejecting snapshot — component ledger frozen past the dormancy horizon (stalled observer, not a dormant asset)",
			"asset", snap.AssetKey,
			"snapshot_ledger", snap.LedgerSequence,
			"min_component_ledger", snap.MinComponentLedger,
			"gap", snap.LedgerSequence-snap.MinComponentLedger,
			"threshold", threshold,
			"threshold_source", thresholdSource,
			"dormancy_horizon", r.maxDormantComponentLedger,
			"remedy", "check the component observer is advancing; raise [supply] max_dormant_component_ledgers only for genuinely long-dormant assets")
		return Outcome{Kind: OutcomeKindStaleComponent, Err: err, Snapshot: snap}, true
	}

	// Dormant: last observation is current — insert and report the
	// benign outcome so the per-asset counter shows the asset is quiet,
	// not failing.
	r.logger.Debug("supply refresh: accepting dormant-asset snapshot (component ledger unchanged)",
		"asset", snap.AssetKey,
		"snapshot_ledger", snap.LedgerSequence,
		"min_component_ledger", snap.MinComponentLedger,
		"gap", snap.LedgerSequence-snap.MinComponentLedger,
		"threshold", threshold,
		"threshold_source", thresholdSource)
	if err := r.inserter.InsertSupply(ctx, snap); err != nil {
		r.logger.Error("supply refresh: insert failed",
			"err", err, "asset", snap.AssetKey, "ledger", snap.LedgerSequence)
		return Outcome{Kind: OutcomeKindWriteError, Err: err, Snapshot: snap}, true
	}
	return Outcome{Kind: OutcomeKindDormant, Snapshot: snap, BandBreach: r.checkWriteBand(snap)}, true
}

// checkWriteBand compares a just-written snapshot's total with the previous total this Refresher wrote for the asset and returns "up" or
// "down" when it moved more than [WriteBandFactor]x, else ""; a zero or absent previous total never fires.
func (r *Refresher) checkWriteBand(snap Supply) string {
	if snap.TotalSupply == nil {
		return ""
	}
	prev := r.lastWrittenTotal[snap.AssetKey]
	r.lastWrittenTotal[snap.AssetKey] = new(big.Int).Set(snap.TotalSupply)
	if prev == nil || prev.Sign() <= 0 {
		return ""
	}
	factor := big.NewInt(WriteBandFactor)
	var direction string
	switch {
	case snap.TotalSupply.Cmp(new(big.Int).Mul(prev, factor)) > 0:
		direction = "up"
	case new(big.Int).Mul(snap.TotalSupply, factor).Cmp(prev) < 0:
		direction = "down"
	default:
		return ""
	}
	r.logger.Warn("supply refresh: total supply moved outside the write band (row still written)",
		"asset", snap.AssetKey,
		"ledger", snap.LedgerSequence,
		"direction", direction,
		"previous_total", prev.String(),
		"total", snap.TotalSupply.String(),
		"band_factor", WriteBandFactor)
	return direction
}
