package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Composite-reference corroboration for structurally single-venue
// targets (2026-08-29, product decision — design doc
// docs/design/composite-route-corroboration-for-structurally-single-venue.md
// §10 amendment).
//
// A pair like crypto:XLM/fiat:GBP is quoted by one or two venues, so
// the ADR-0019 phase-2 freeze (confidence < 0.45 AND z > 5 AND
// source_count <= 1) fires on every large move whether the move is a
// venue-specific spike or the whole market repricing. The direct
// venue's own history cannot tell those apart — but its configured
// triangulation chain can: XLM/USD (multi-venue, deep) × USD/GBP
// (institutional FX, massive.com) is an excellent REFERENCE for XLM/GBP
// fair value. This file evaluates that reference on the CURRENT bucket
// and lets it CORROBORATE or REFUTE the freeze decision:
//
//   - direct print agrees with the composite within
//     [CompositeReferenceConfig.ToleranceBps] → the move is market-wide
//     → the phase-2 fire is SUPPRESSED (or, mid-hold, the release lens
//     agrees) and the decision carries corroboration_basis=composite;
//   - composite disagrees (flat while the venue spiked, or moved
//     elsewhere) → venue-specific → freeze exactly as before,
//     corroboration_basis=venue;
//   - composite unavailable (leg not refreshed this tick, leg too thin,
//     FX leg stale / not from the FX source class, chain unconfigured)
//     → freeze exactly as before, the reason string says why.
//
// Why CURRENT bucket, never a prior tick's sample (the rejected first
// implementation, 95da898d): a tick-lagged composite that AGREED with
// the pre-spike print certified the pre-spike LEVEL, not the spike —
// it suppressed a z≈50 single-venue manipulation that must freeze. The
// reference here is rebuilt from THIS tick's XLM/USD publish and an FX
// snap at THIS bucket's end, so it can only agree with the spike if the
// deep market moved with it.
//
// Hard invariants (pinned by composite_reference_test.go):
//   - the composite NEVER contributes to VWAP and NEVER raises the
//     served source_count / [Orchestrator.effectiveSourceCount] — the
//     `sources=` field in the freeze reason stays the real venue count;
//     it only changes the freeze VERDICT and says on what basis;
//   - the reference is only as strong as its weakest leg: the priced
//     (crypto/USD) leg must itself carry >= MinLegSources distinct
//     venues on the current bucket, and the FX leg must be fresh within
//     FXMaxAge and come from the FX source class (never an oracle);
//   - targets with >= 2 real venues on the bucket are never evaluated,
//     so their outputs are byte-identical to before.

// CompositeReferenceConfig gates and tunes the composite-reference
// corroboration. Zero-valued numeric fields fall back to the
// Default* constants in [CompositeReferenceConfig.withDefaults]; a
// zero-valued struct (Enabled=false) disables the mechanism entirely,
// which is what a Config assembled directly in a test gets.
type CompositeReferenceConfig struct {
	// Enabled is the master switch. The binary's config default is ON
	// for the Targets allow-list below (config.Default()).
	Enabled bool

	// Targets is the allow-list of structurally single-venue pairs the
	// reference may corroborate. A target must also have a configured
	// [TriangulationChain] (its legs are the reference).
	Targets []canonical.Pair

	// ToleranceBps is the maximum |direct − composite| / composite, in
	// basis points, for the composite to CORROBORATE the direct print.
	ToleranceBps int

	// MinLegSources is the minimum number of distinct venues the
	// priced (non-FX) legs must each carry on the current bucket. A
	// single-venue leg cannot corroborate anything.
	MinLegSources int

	// FXMaxAge is the staleness budget for the FX leg's snap
	// observation. fx_quotes buckets are DAILY and the feed pauses over
	// market closes, so this mirrors the Chainlink FX feed budget (76h)
	// rather than the 6h poll-liveness alert.
	FXMaxAge time.Duration

	// LegDispersionBps is the leg-dispersion guard (verifier advisory A1,
	// 2026-08-29): every venue's own bucket VWAP on a priced leg must be
	// within this many bps of the leg VWAP, else the leg cannot
	// corroborate (`composite_unavailable: leg_dispersion=…`). A leg
	// where one venue dominates and a dust print on another sits 3 % off
	// is not two agreeing venues. 0 = ToleranceBps.
	LegDispersionBps int

	// ReleaseBandPct is the mid-hold RELEASE agreement band for a target
	// whose reference resolved on the bucket (advisory A2): the fresh
	// candidate must sit within this % of the current-bucket composite
	// for the auto-unfreeze streak to advance — dedicated and tighter
	// than the shared cross-oracle band (releaseAgreementMaxPct, 5 %),
	// which a held +4 % venue-specific offset would satisfy. 0 = default.
	ReleaseBandPct float64
}

const (
	// DefaultCompositeReferenceToleranceBps — 75 bps. A genuine
	// repricing carries the composite with the venue to within tens of
	// bps; a venue-specific spike the freeze exists for is >5σ, i.e.
	// several percent, so the band has ample margin on both sides while
	// still absorbing a daily-bucket FX leg's intraday drift.
	DefaultCompositeReferenceToleranceBps = 75

	// DefaultCompositeReferenceMinLegSources — 2 real venues on the
	// crypto/USD leg (Ash, 2026-08-29): a single-venue leg is the very
	// thing under suspicion and corroborates nothing.
	DefaultCompositeReferenceMinLegSources = 2

	// DefaultCompositeReferenceFXMaxAge — 76h, the same budget the
	// Chainlink FX feeds carry in the shipped template ("FX markets
	// close weekends; 3h crypto default reads them dead Fri-Mon").
	// fx_quotes rows are keyed by DAY and updated in place, so a
	// Monday-morning read of Friday's bucket is ~72h old and still
	// valid; a feed that missed Monday is stale by Tuesday.
	DefaultCompositeReferenceFXMaxAge = 76 * time.Hour

	// DefaultCompositeReferenceReleaseBandPct — 2 %. A genuine repricing
	// lands the venue back within low single digits of the composite; a
	// held venue-specific offset the 2026-08-24 corroborated-release
	// panel measured (~5–40 %) never does, and neither does the +4 %
	// offset the shared 5 % band would have waved through.
	DefaultCompositeReferenceReleaseBandPct = 2.0
)

// withDefaults fills zero-valued tunables. Enabled and Targets are
// left exactly as configured — there is no implicit allow-list.
func (c CompositeReferenceConfig) withDefaults() CompositeReferenceConfig {
	if c.ToleranceBps <= 0 {
		c.ToleranceBps = DefaultCompositeReferenceToleranceBps
	}
	if c.MinLegSources <= 0 {
		c.MinLegSources = DefaultCompositeReferenceMinLegSources
	}
	if c.FXMaxAge <= 0 {
		c.FXMaxAge = DefaultCompositeReferenceFXMaxAge
	}
	if c.LegDispersionBps <= 0 {
		c.LegDispersionBps = c.ToleranceBps
	}
	if c.ReleaseBandPct <= 0 {
		c.ReleaseBandPct = DefaultCompositeReferenceReleaseBandPct
	}
	return c
}

// Corroboration basis labels carried in the freeze reason string,
// composite_meta.corroboration_basis and the verdict gauge.
const (
	// corroborationBasisComposite: the composite reference agreed with
	// the direct print on the current bucket and decided the verdict.
	corroborationBasisComposite = "composite"
	// corroborationBasisVenue: the decision rests on the venue's own
	// print (composite refuted it or was unavailable) — the pre-2026-08-29
	// behaviour.
	corroborationBasisVenue = "venue"
)

// compositeVerdict is the reference's reading of one bucket.
type compositeVerdict string

const (
	compositeVerdictCorroborated compositeVerdict = "corroborated"
	compositeVerdictRefuted      compositeVerdict = "refuted"
	compositeVerdictUnavailable  compositeVerdict = "unavailable"
)

// compositeVerdicts enumerates every verdict so the gauge can be set
// for ALL of them each evaluation (a label that is only ever set to 1
// leaves stale 1s behind when the verdict changes).
var compositeVerdicts = []compositeVerdict{
	compositeVerdictCorroborated, compositeVerdictRefuted, compositeVerdictUnavailable,
}

// legRef is one priced pair's CURRENT-tick publish, recorded by
// refreshPairWindow for the reference evaluator: the exact VWAP and the
// distinct venue count behind it. Only confident publishes are
// recorded — a frozen / dropped / empty leg leaves no entry, so the
// reference fails closed on it.
type legRef struct {
	price   *big.Rat
	sources int

	// dispersion is max |venueVWAP − legVWAP| / legVWAP across the
	// distinct venues in the survivor slice (exact Rat; nil when fewer
	// than two venues). The leg-dispersion guard reads it.
	dispersion *big.Rat

	// dispersionUncomputable is set when a venue's own VWAP could not be
	// computed (A4): the guard cannot vouch for the leg, so the leg is
	// refused (`leg_dispersion=uncomputable`) rather than skipped.
	dispersionUncomputable bool
}

// legDispersion computes the leg-dispersion statistic for one
// published bucket: each venue's own VWAP over the post-filter
// survivor trades (the same slice and the same normalisation the leg
// VWAP came from), measured against the leg VWAP. (nil, false) when the
// bucket has fewer than two venues (nothing to disperse — the guard
// does not apply). (nil, true) when a venue VWAP cannot be computed:
// the guard could not run, which is NOT the same as passing it, so the
// caller fails closed (A4).
func (o *Orchestrator) legDispersion(pair canonical.Pair, trades []canonical.Trade, vwap *big.Rat) (dispersion *big.Rat, uncomputable bool) {
	if vwap == nil || vwap.Sign() <= 0 {
		return nil, true
	}
	byVenue := make(map[string][]canonical.Trade)
	for _, tr := range trades {
		byVenue[tr.Source] = append(byVenue[tr.Source], tr)
	}
	if len(byVenue) < 2 {
		return nil, false
	}
	worst := new(big.Rat)
	for _, venueTrades := range byVenue {
		venueVWAP, err := o.computeNormalizedVWAP(venueTrades, pair)
		if err != nil || venueVWAP == nil || venueVWAP.Sign() <= 0 {
			return nil, true
		}
		dev := new(big.Rat).Sub(venueVWAP, vwap)
		dev.Abs(dev).Quo(dev, vwap)
		if dev.Cmp(worst) > 0 {
			worst = dev
		}
	}
	return worst, false
}

// ratBps renders a ratio as basis points (float, for reporting only).
func ratBps(r *big.Rat) float64 {
	if r == nil {
		return 0
	}
	f, _ := r.Float64()
	return f * 10_000
}

// compositeReference is the evaluated reference for one (target,
// window) bucket.
type compositeReference struct {
	verdict compositeVerdict

	// price is the composite (product of the chain legs) when the
	// reference resolved; nil when unavailable.
	price *big.Rat

	// divergencePct is |direct − composite| / composite × 100 when the
	// reference resolved (the same orientation as
	// [Orchestrator.triangulationDivergencePct]).
	divergencePct float64

	// legSources maps each chain leg (canonical pair string) to the
	// distinct source count behind it on this bucket (FX legs: the
	// number of distinct FX providers in the snap, normally 1).
	legSources map[string]int

	// legDispersionBps maps each priced leg to its venue dispersion in
	// bps (see legRef.dispersion); absent for FX legs.
	legDispersionBps map[string]float64

	// unavailable names why the reference could not be evaluated
	// (e.g. "leg_sources=1", "fx_stale", "leg_not_refreshed"). Empty
	// when the reference resolved.
	unavailable string
}

// basis is the corroboration_basis label for this reading.
func (r compositeReference) basis() string {
	if r.verdict == compositeVerdictCorroborated {
		return corroborationBasisComposite
	}
	return corroborationBasisVenue
}

// legSourcesString renders legSources deterministically for the
// freeze reason string: `{crypto:XLM/fiat:USD:3,fiat:USD/fiat:GBP:1}`.
func (r compositeReference) legSourcesString() string {
	keys := make([]string, 0, len(r.legSources))
	for k := range r.legSources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, r.legSources[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// legDispersionString renders legDispersionBps deterministically:
// `{crypto:XLM/fiat:USD:12.5}`; empty string when nothing was measured.
func (r compositeReference) legDispersionString() string {
	if len(r.legDispersionBps) == 0 {
		return ""
	}
	keys := make([]string, 0, len(r.legDispersionBps))
	for k := range r.legDispersionBps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%.1f", k, r.legDispersionBps[k]))
	}
	return " composite_leg_dispersion_bps={" + strings.Join(parts, ",") + "}"
}

// resolved reports whether the reference produced a composite on this
// bucket (corroborated or refuted) — the readings that may stand in
// for the prior-tick chain sample and drive the release lens.
func (r compositeReference) resolved() bool {
	return r.verdict == compositeVerdictCorroborated || r.verdict == compositeVerdictRefuted
}

// reasonSuffix is appended to the phase-2 freeze reason so
// freeze_events.detail->>'reason' and the `freeze engaged` log carry
// the basis, the agreement measured and how strong each leg was.
func (r compositeReference) reasonSuffix() string {
	switch r.verdict {
	case compositeVerdictUnavailable:
		return fmt.Sprintf(" corroboration_basis=%s composite_unavailable: %s composite_leg_sources=%s%s",
			corroborationBasisVenue, r.unavailable, r.legSourcesString(), r.legDispersionString())
	case compositeVerdictCorroborated, compositeVerdictRefuted:
		return fmt.Sprintf(" corroboration_basis=%s composite_%s divergence_pct=%.3f composite_leg_sources=%s%s",
			r.basis(), string(r.verdict), r.divergencePct, r.legSourcesString(), r.legDispersionString())
	default:
		return ""
	}
}

// compositeReferenceEligible reports whether the reference is evaluated
// for this bucket at all: the mechanism is enabled, the pair is on the
// allow-list, and the bucket is single-venue by the freeze's own
// source_count leg. A multi-venue bucket is never evaluated — its
// freeze path, confidence input, reason string and composite_meta stay
// byte-identical to before (pinned by the differential test).
func (o *Orchestrator) compositeReferenceEligible(pair canonical.Pair, trades []canonical.Trade) bool {
	cfg := o.cfg.CompositeReference
	if !cfg.Enabled || !containsPair(cfg.Targets, pair) {
		return false
	}
	return distinctSourceCount(trades) <= o.cfg.Phase2Thresholds.withDefaults().SourceCountMaxFreeze
}

// containsPair reports whether pairs holds pair (by canonical identity).
func containsPair(pairs []canonical.Pair, pair canonical.Pair) bool {
	for _, p := range pairs {
		if p.Base.Equal(pair.Base) && p.Quote.Equal(pair.Quote) {
			return true
		}
	}
	return false
}

// chainForTarget returns the first configured chain whose Target is
// pair, and whether one exists.
func (o *Orchestrator) chainForTarget(pair canonical.Pair) (TriangulationChain, bool) {
	for _, chain := range o.cfg.Triangulations {
		if chain.Target.Base.Equal(pair.Base) && chain.Target.Quote.Equal(pair.Quote) {
			return chain, true
		}
	}
	return TriangulationChain{}, false
}

// recordLegRef captures a confidently-published (pair, window) bucket
// as a CURRENT-tick leg for the reference evaluator. Called from
// refreshPairWindow at the publish point (next to recordEdgeQuote), so
// frozen / dropped / empty / below-floor buckets leave no entry.
// Rebuilt at the top of every Tick; same single-Tick-at-a-time
// invariant as tickEdgeQuotes (L4).
func (o *Orchestrator) recordLegRef(pair canonical.Pair, window time.Duration, vwap *big.Rat, trades []canonical.Trade) {
	if o.tickLegRefs == nil {
		o.tickLegRefs = make(map[time.Duration]map[string]legRef, len(o.cfg.Windows))
	}
	if o.tickLegRefs[window] == nil {
		o.tickLegRefs[window] = make(map[string]legRef, len(o.cfg.Pairs))
	}
	dispersion, uncomputable := o.legDispersion(pair, trades, vwap)
	o.tickLegRefs[window][pair.String()] = legRef{
		price:                  new(big.Rat).Set(vwap), // defensive copy, as recordEdgeQuote
		sources:                distinctSourceCount(trades),
		dispersion:             dispersion,
		dispersionUncomputable: uncomputable,
	}
}

// evaluateCompositeReference builds the reference for (pair, window)
// on the CURRENT bucket, compares it with the fresh direct VWAP and
// records the reading in tickCompositeRefs (read by the confidence
// step, the freeze step and the triangulation pass's composite_meta).
// Every failure mode is fail-closed: the verdict is "unavailable" with
// a named cause and the freeze semantics are exactly the pre-existing
// ones.
func (o *Orchestrator) evaluateCompositeReference(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	now time.Time,
	direct *big.Rat,
) compositeReference {
	ref := o.resolveCompositeReference(ctx, pair, window, now, direct)
	if o.tickCompositeRefs == nil {
		o.tickCompositeRefs = make(map[string]compositeReference)
	}
	o.tickCompositeRefs[pair.String()+":"+window.String()] = ref
	o.emitCompositeReference(pair, window, ref)
	return ref
}

// resolveCompositeReference is the pure(ish) evaluation behind
// [Orchestrator.evaluateCompositeReference].
func (o *Orchestrator) resolveCompositeReference(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	now time.Time,
	direct *big.Rat,
) compositeReference {
	cfg := o.cfg.CompositeReference.withDefaults()
	ref := compositeReference{
		verdict: compositeVerdictUnavailable, legSources: map[string]int{}, legDispersionBps: map[string]float64{},
	}
	chain, ok := o.chainForTarget(pair)
	if !ok {
		ref.unavailable = "no_chain"
		return ref
	}
	if direct == nil || direct.Sign() <= 0 {
		ref.unavailable = "direct_non_positive"
		return ref
	}
	composite := big.NewRat(1, 1)
	for _, leg := range chain.Legs {
		price, sources, why := o.referenceLeg(ctx, leg, window, now, cfg)
		if lr, ok := o.tickLegRefs[window][leg.String()]; ok && !isFXLeg(leg) && lr.dispersion != nil {
			ref.legDispersionBps[leg.String()] = ratBps(lr.dispersion)
		}
		if sources > 0 {
			// Recorded even when the leg is the reason the reference is
			// unavailable (a thin leg), so the operator sees HOW thin.
			ref.legSources[leg.String()] = sources
		}
		if why != "" {
			ref.unavailable = why
			return ref
		}
		composite.Mul(composite, price)
	}
	compositeF, _ := composite.Float64()
	directF, _ := direct.Float64()
	if compositeF <= 0 || directF <= 0 || math.IsInf(compositeF, 0) || math.IsInf(directF, 0) {
		ref.unavailable = "non_finite"
		return ref
	}
	ref.price = composite
	// Reported in float (same orientation as triangulationDivergencePct);
	// DECIDED in exact Rat space so the tolerance boundary is not at the
	// mercy of binary rounding (0.75 × 100 ≠ 75 in float64).
	ref.divergencePct = math.Abs(directF-compositeF) / compositeF * 100.0
	deviation := new(big.Rat).Sub(direct, composite)
	deviation.Abs(deviation).Quo(deviation, composite)
	tolerance := big.NewRat(int64(cfg.ToleranceBps), 10_000)
	if deviation.Cmp(tolerance) <= 0 {
		ref.verdict = compositeVerdictCorroborated
	} else {
		ref.verdict = compositeVerdictRefuted
	}
	return ref
}

// referenceLeg resolves ONE chain leg for the current bucket. Returns
// (price, distinct source count, "") on success or (nil, 0, cause)
// when the leg cannot back a reference this bucket.
//
// A priced (non-FX) leg must have been published THIS tick
// (tickLegRefs — [Orchestrator.refreshOrder] guarantees the legs are
// refreshed before their targets) with >= MinLegSources venues. An FX
// leg is snapped at `now` through the same [FXStore] the chain pass
// uses, must be within FXMaxAge, and every provider in its source
// label must be of the FX source class — an oracle can never be the FX
// leg of a reference. FXStore nil (a test Config) reads as unavailable
// rather than falling back to the cached VWAP: the cached value's
// freshness and provenance are unknown.
func (o *Orchestrator) referenceLeg(
	ctx context.Context,
	leg canonical.Pair,
	window time.Duration,
	now time.Time,
	cfg CompositeReferenceConfig,
) (*big.Rat, int, string) {
	if !isFXLeg(leg) {
		lr, ok := o.tickLegRefs[window][leg.String()]
		if !ok || lr.price == nil || lr.price.Sign() <= 0 {
			return nil, 0, "leg_not_refreshed"
		}
		if lr.sources < cfg.MinLegSources {
			return nil, lr.sources, fmt.Sprintf("leg_sources=%d", lr.sources)
		}
		// Leg-dispersion guard (A1): two venues only count as two when
		// they AGREE. A dominant venue plus a dust print 3 % off is one
		// opinion and an artefact, and the artefact can be the attacker's.
		// A guard that could not run has not been passed (A4).
		if lr.dispersionUncomputable {
			return nil, lr.sources, "leg_dispersion=uncomputable"
		}
		if lr.dispersion != nil && lr.dispersion.Cmp(big.NewRat(int64(cfg.LegDispersionBps), 10_000)) > 0 {
			return nil, lr.sources, fmt.Sprintf("leg_dispersion=%.1fbps", ratBps(lr.dispersion))
		}
		return lr.price, lr.sources, ""
	}
	if o.cfg.FXStore == nil {
		return nil, 0, "fx_store_unwired"
	}
	price, observedAt, source, err := o.cfg.FXStore.FXQuoteAtOrBefore(ctx, leg, now, external.FXSources())
	switch {
	case errors.Is(err, timescale.ErrNoFXQuote):
		return nil, 0, "fx_missing"
	case err != nil:
		o.logger.Warn("composite reference: FX snap query failed",
			"target_leg", leg.String(), "err", err)
		return nil, 0, "fx_error"
	}
	if price == nil || price.Sign() <= 0 {
		return nil, 0, "fx_non_positive"
	}
	if observedAt.IsZero() || now.Sub(observedAt) > cfg.FXMaxAge {
		return nil, 0, "fx_stale"
	}
	providers := strings.Split(source, "+")
	for _, p := range providers {
		if !external.IsFXSource(p) {
			return nil, 0, "fx_source_class=" + p
		}
	}
	return price, len(providers), ""
}

// emitCompositeReference publishes the verdict gauge (one series per
// verdict, exactly one of them 1) and the per-leg source-count gauge,
// and logs the load-bearing outcome: a corroborated bucket is the one
// that changes a freeze decision, so it is logged at Info with the
// full decomposition; the others are Debug (they leave the freeze
// path exactly as before).
func (o *Orchestrator) emitCompositeReference(pair canonical.Pair, window time.Duration, ref compositeReference) {
	for _, v := range compositeVerdicts {
		val := 0.0
		if v == ref.verdict {
			val = 1
		}
		obs.AggregatorCompositeCorroboration.WithLabelValues(pair.String(), window.String(), string(v)).Set(val)
	}
	for leg, n := range ref.legSources {
		obs.AggregatorCompositeReferenceLegSources.WithLabelValues(pair.String(), window.String(), leg).Set(float64(n))
	}
	for leg, bps := range ref.legDispersionBps {
		obs.AggregatorCompositeReferenceLegDispersionBps.WithLabelValues(pair.String(), window.String(), leg).Set(bps)
	}
	attrs := []any{
		"pair", pair.String(),
		"window", window.String(),
		"verdict", string(ref.verdict),
		"basis", ref.basis(),
		"divergence_pct", ref.divergencePct,
		"leg_sources", ref.legSourcesString(),
		"leg_dispersion_bps", strings.TrimPrefix(ref.legDispersionString(), " composite_leg_dispersion_bps="),
	}
	if ref.unavailable != "" {
		attrs = append(attrs, "unavailable", ref.unavailable)
	}
	if ref.verdict == compositeVerdictCorroborated {
		o.logger.Info("composite reference corroborates the direct print on the current bucket", attrs...)
		return
	}
	o.logger.Debug("composite reference evaluated", attrs...)
}

// currentCompositeReference returns this tick's reference reading for
// (pair, window), if one was evaluated.
func (o *Orchestrator) currentCompositeReference(pair canonical.Pair, window time.Duration) (compositeReference, bool) {
	if len(o.tickCompositeRefs) == 0 {
		return compositeReference{}, false
	}
	ref, ok := o.tickCompositeRefs[pair.String()+":"+window.String()]
	return ref, ok
}

// refreshOrder returns the per-tick refresh order: cfg.Pairs with the
// priced (non-FX) legs of every enabled composite-reference chain
// moved to the front, relative order otherwise preserved. The
// reference is evaluated inside the TARGET's refresh, so its legs must
// already have published this tick; without this an operator-supplied
// `aggregate.pairs` that lists XLM/GBP before XLM/USD would leave the
// mechanism silently dead ("leg_not_refreshed" every bucket). Returns
// cfg.Pairs itself when the mechanism is off, so nothing changes for a
// deployment that has not enabled it. Refresh order has no effect on
// any per-pair output: refreshPairWindow reads and writes only
// per-(pair, window) state.
func refreshOrder(cfg Config) []canonical.Pair {
	if !cfg.CompositeReference.Enabled || len(cfg.CompositeReference.Targets) == 0 {
		return cfg.Pairs
	}
	isLeg := func(p canonical.Pair) bool {
		for _, chain := range cfg.Triangulations {
			if !containsPair(cfg.CompositeReference.Targets, chain.Target) {
				continue
			}
			for _, leg := range chain.Legs {
				if !isFXLeg(leg) && leg.Base.Equal(p.Base) && leg.Quote.Equal(p.Quote) {
					return true
				}
			}
		}
		return false
	}
	legs := make([]canonical.Pair, 0, len(cfg.Pairs))
	rest := make([]canonical.Pair, 0, len(cfg.Pairs))
	for _, p := range cfg.Pairs {
		if isLeg(p) {
			legs = append(legs, p)
		} else {
			rest = append(rest, p)
		}
	}
	return append(legs, rest...)
}
