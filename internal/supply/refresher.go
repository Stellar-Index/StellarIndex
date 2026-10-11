package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"
)

// LedgerLookup is the storage-side primitive the [Refresher] uses
// to resolve "what's the most recent known chain ledger." Production
// impl takes the `ledgerstream` ingestion cursor and clamps it to
// the newest landed stellar.ledgers row at or before it; tests pass
// in-memory fakes.
type LedgerLookup interface {
	LatestKnownLedger(ctx context.Context) (uint32, time.Time, error)
}

// SnapshotComputer is the supply-package primitive — computes one
// [Supply] for the given ledger. Production impl is *XLMComputer;
// classic + SEP-41 computers can plug in once they ship.
type SnapshotComputer interface {
	Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error)
}

// SnapshotInserter writes one [Supply] into the persistence layer.
// Production impl is timescale.Store.InsertSupply; idempotent on
// (asset_key, ledger_sequence).
type SnapshotInserter interface {
	InsertSupply(ctx context.Context, snap Supply) error
}

// Outcome is what one [Refresher.Tick] produced. Drives the
// aggregator's Prometheus counters; OutcomeKind is a stable
// string suitable for a metric label.
type Outcome struct {
	Kind     OutcomeKind
	Snapshot Supply // populated on OutcomeKindOK only
	Err      error  // populated on every error outcome
	// BandBreach is "up" or "down" when a written snapshot's total moved
	// more than [WriteBandFactor]x against the previous one this Refresher wrote.
	BandBreach string
}

// OutcomeKind identifies a refresh outcome. Values are stable
// metric-label strings.
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

// DefaultStaleComponentLedgers is the freshness threshold
// the Refresher applies when none is operator-configured: a
// snapshot whose MinComponentLedger lags the snapshot ledger by
// more than 1000 ledgers (~85 min at 5s ledger close cadence)
// is rejected. Operators tune via [WithStaleComponentLedgers].
//
// Conservative default — most operator deployments see all
// supply observers complete within one ledger of the trade
// indexer, so 1000 is large enough to never false-reject under
// normal load while small enough to catch a genuinely stalled
// observer before the supply table accrues misleading rows.
const DefaultStaleComponentLedgers uint32 = 1000

// DefaultMaxDormantComponentLedgers bounds how long the dormancy carve-out
// re-stamps an unchanged component observation as the current supply: 17280
// ledgers (~24 h at 5s). Operators tune via
// [WithMaxDormantComponentLedgers].
//
// An unchanged MinComponentLedger does NOT separate a dormant asset from a
// STALLED observer: a dead producer looks dormant forever, and unbounded the
// carve-out would republish a frozen supply at the advancing tip under the
// benign `dormant` outcome the supply-refresh alert excludes. The two are
// indistinguishable from this signal, so the benefit of the doubt is bounded
// in time; past it the snapshot is refused as stale_component (ADR-0011: do
// not fabricate freshness on the market-cap/FDV surface).
//
// 24 h is generous: the longest dormant run measured on pubnet is ~4 h.
const DefaultMaxDormantComponentLedgers uint32 = 17_280

// WriteBandFactor bounds the tick-over-tick total_supply move the
// Refresher writes without flagging. Flag-only: a legitimate mint can
// 10x a young token, so a breach is recorded, never refused.
const WriteBandFactor = 10

// Refresher runs one supply-snapshot cycle per [Refresher.Tick]
// call. Composes ledger resolution + computer + inserter; the
// aggregator drives it via a ticker in its own goroutine,
// mirroring the baseline-refresher shape.
//
// One Refresher instance is bound to one watched asset (the
// aggregator constructs a dedicated Refresher per asset in
// buildSupplyRefreshers), so the per-asset dormancy memory below
// is single-keyed in practice; we still key by AssetKey for
// safety against a future shared-Refresher caller.
type Refresher struct {
	ledgers                   LedgerLookup
	computer                  SnapshotComputer
	inserter                  SnapshotInserter
	logger                    *slog.Logger
	staleComponentLedger      uint32
	staleComponentByAsset     map[string]uint32
	strictFreshnessRequired   bool
	maxDormantComponentLedger uint32

	// lastComponentLedger remembers, per asset_key, the
	// MinComponentLedger of the most recent snapshot the gate
	// evaluated. The stale-component gate compares the
	// (always-advancing) chain tip against MinComponentLedger,
	// which for a DORMANT asset (no balance changes) freezes — so
	// the gap grows past the threshold and stays there forever,
	// permanently rejecting every future tick and silently
	// freezing the asset's supply row. An UNCHANGED value
	// tick-over-tick is therefore accepted (OutcomeKindDormant) up
	// to the dormancy horizon; a moved or first-seen lagging value
	// is rejected. The unchanged case covers a dead producer too.
	lastComponentLedger map[string]uint32

	// lastWrittenTotal is the total_supply of the last snapshot this
	// Refresher wrote per asset_key; the write band compares against it.
	lastWrittenTotal map[string]*big.Int
}

// RefresherOption tunes a [Refresher].
type RefresherOption func(*Refresher)

// WithStaleComponentLedgers overrides the freshness threshold. The Refresher rejects
// a snapshot when (snap.LedgerSequence - snap.MinComponentLedger)
// exceeds this value AND MinComponentLedger > 0 (zero means the
// computer didn't populate the field, so the gate skips). Set to 0 to disable the gate.
func WithStaleComponentLedgers(maxLag uint32) RefresherOption {
	return func(r *Refresher) {
		r.staleComponentLedger = maxLag
	}
}

// WithStaleComponentLedgersFor sets a per-asset override of the
// stale-component threshold. Low-activity governance tokens like PHO see their trustline
// observer lag the snapshot ledger by ~1200 ledgers (~100 min) —
// past the 1000-ledger global default. A per-asset override lets
// operators relax the gate for known-low-activity assets without
// loosening it for high-traffic XLM / USDC. Pass assetKey in the
// [AssetKey] form the computers stamp on each snapshot ("XLM",
// "PHO:GAX5TXB5...", or a bare contract id); lookup is exact-match,
// so normalise operator input via [CanonicalizeStaleComponentLedgers].
// Repeated calls layer additively; the last per-asset value wins.
//
// A zero per-asset value disables the gate for that asset alone
// (the global default still applies to other assets); use the
// option twice to mix relaxed + tightened per-asset thresholds.
func WithStaleComponentLedgersFor(assetKey string, maxLag uint32) RefresherOption {
	return func(r *Refresher) {
		if r.staleComponentByAsset == nil {
			r.staleComponentByAsset = make(map[string]uint32)
		}
		r.staleComponentByAsset[assetKey] = maxLag
	}
}

// WithMaxDormantComponentLedgers overrides the dormancy horizon: how far the snapshot ledger
// may run ahead of a FROZEN MinComponentLedger before the Refresher
// stops treating it as a dormant asset and starts treating it as a
// stalled component observer (rejecting with
// [OutcomeKindStaleComponent] rather than re-stamping the frozen
// value at the current ledger).
//
// Set to 0 to disable the horizon, leaving the dormancy carve-out
// unbounded. That is appropriate only for deployments that watch
// assets legitimately dormant for very long stretches AND monitor
// their component observers by some other means, since it re-opens
// the "dead observer looks dormant forever" hole this bound closes.
func WithMaxDormantComponentLedgers(maxDormant uint32) RefresherOption {
	return func(r *Refresher) {
		r.maxDormantComponentLedger = maxDormant
	}
}

// WithStrictFreshnessRequired flips the Refresher into the
// strict posture:
// a snapshot whose `MinComponentLedger == 0` is rejected with
// [OutcomeKindMissingFreshness] rather than passing the gate.
// Default false keeps the permissive interpretation
// of zero ("no freshness signal — let it through") so
// deployments running the static-XLM fallback or where one of
// the freshness producers can transiently fail (Postgres
// timeout, Redis blip) keep publishing snapshots.
//
// Operators turn this on after every freshness producer is
// confirmed wired AND every reader is shown to never
// fail-open under steady-state load — typically post-launch,
// after a few weeks of green snapshot timers. Once enabled,
// the supply table only ever accumulates rows whose component
// observations are demonstrably anchored to a recent ledger.
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

// Tick runs one refresh cycle:
//
//  1. Resolve the latest known chain ledger.
//  2. Compute the supply at that ledger.
//  3. Insert the snapshot (idempotent on conflict).
//
// Returns an [Outcome] for metric emission. Tick does NOT bubble
// errors — it logs them and returns the outcome so the
// surrounding goroutine never crashes the aggregator's whole
// loop on a transient supply-side issue.
func (r *Refresher) Tick(ctx context.Context) Outcome {
	ledger, observedAt, err := r.ledgers.LatestKnownLedger(ctx)
	if err != nil {
		r.logger.Warn("supply refresh: no ledger", "err", err)
		return Outcome{Kind: OutcomeKindNoLedger, Err: err}
	}

	snap, err := r.computer.Compute(ctx, ledger, observedAt)
	if err != nil {
		// Distinguish the "no observation" outcome (which the
		// ChainReader surfaces with ErrNoObservation when both live
		// AND static fall through) from generic compute errors so
		// operators can chart the bootstrap-progress signal.
		//
		// ErrNegativeTotalMissingBaseline is a
		// benign bootstrap-like state — a SAC-wrapper whose pre-Soroban
		// opening balance hasn't been seeded yet reads Σburn > Σmint over
		// the Soroban-era window. Route it to `missing_baseline` (excluded
		// from error_dominant) so it prompts a seed instead of paging;
		// once seeded, a still-negative total surfaces as ErrNegativeTotalSupply
		// → compute_error, which DOES page (genuine inconsistency).
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

	// Strict mode rejects snapshots that arrive with NO freshness
	// signal (MinComponentLedger == 0), instead of the permissive
	// interpretation ("no signal — let it through").
	// Default off, for deployments on
	// the static-XLM fallback or with transiently-failing
	// freshness producers. Operators turn it on once every
	// producer is wired + every reader is shown to never
	// fail-open under steady-state load.
	staticReserve := snap.Basis == BasisXLMSDFReserveExclusionStatic
	if r.strictFreshnessRequired && (snap.MinComponentLedger == 0 || staticReserve) {
		err := fmt.Errorf("supply: strict-freshness mode — snapshot has no MinComponentLedger anchor (basis %s)", snap.Basis)
		r.logger.Warn("supply refresh: rejecting freshness-less snapshot under strict mode",
			"asset", snap.AssetKey,
			"basis", string(snap.Basis),
			"snapshot_ledger", snap.LedgerSequence)
		return Outcome{Kind: OutcomeKindMissingFreshness, Err: err, Snapshot: snap}
	}

	// Reject snapshots whose per-component observations lag the
	// snapshot ledger by more than the configured threshold.
	// MinComponentLedger == 0 means the computer didn't populate the
	// field; we don't gate in that case, so deployments without
	// freshness-aware computers are unaffected.
	//
	// Per-asset overrides via
	// staleComponentByAsset[snap.AssetKey] win over the global
	// staleComponentLedger when present. A zero per-asset value
	// disables the gate for that asset alone.
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

// applyStaleComponentGate runs the stale-component freshness gate for a
// computed snapshot. It returns (outcome, true) when the gate decides the
// tick (stale-component REJECTION, or dormant-asset ACCEPT which inserts
// here) and (zero, false) when the caller should proceed to its normal insert.
//
// Per-asset staleComponentByAsset overrides win over the global threshold;
// zero disables the gate for that asset. The gap is the advancing tip minus
// the change-driven MinComponentLedger, so a dormant asset would otherwise
// be rejected forever. Past the threshold the decision uses the watermark:
//   - CHANGED since the last tick (or first tick already lagging): reject,
//     OutcomeKindStaleComponent.
//   - UNCHANGED: re-stamp as current (OutcomeKindDormant) while the frozen
//     gap is within the dormancy horizon; reject past it.
//
// "No balance change" and "no observer" are the same signal, so a producer
// that dies is accepted as dormant until [DefaultMaxDormantComponentLedgers]
// is crossed; the horizon is what bounds a dead producer.
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
	// The dormancy carve-out is BOUNDED. A
	// frozen MinComponentLedger is exactly what a dead component
	// observer looks like, so past the horizon we stop giving it the
	// benefit of the doubt and fail closed rather than republish a
	// frozen supply stamped at the current tip. Reported as
	// stale_component (not the alert-excluded `dormant`) so the
	// existing per-asset supply-refresh alert fires on it, and logged
	// at WARN with the knob name so the operator can tell a genuinely
	// long-dormant asset from a stalled producer and act.
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

// checkWriteBand compares a just-written snapshot's total against the
// previous total this Refresher wrote for the asset and returns "up" or
// "down" when it moved more than [WriteBandFactor]x, else "". A zero or
// absent previous total has no ratio to test, so it never fires.
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
