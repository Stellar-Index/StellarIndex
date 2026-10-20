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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Composite-reference corroboration for structurally single-venue targets (docs/design/composite-route-corroboration-for-structurally-single-venue.md §10).
// The triangulation chain is evaluated on the CURRENT bucket (never a prior tick's sample, which certified a pre-spike level) to corroborate or refute the ADR-0019 phase-2 freeze; agreement within ToleranceBps suppresses it.
// Invariants (composite_reference_test.go): the composite NEVER contributes to VWAP or source_count, only the freeze VERDICT; targets with >= 2 real venues are never evaluated.

// CompositeReferenceConfig gates and tunes composite-reference corroboration; zero numeric fields take the Default* values and a zero struct (Enabled=false) disables it.
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

	// FXMaxAge is the staleness budget for the FX leg's snap observation. fx_quotes buckets are DAILY and the feed pauses over
	// market closes, so this mirrors divergence.DefaultMaxComparableAgeFX (76h) rather than the 6h poll-liveness alert.
	FXMaxAge time.Duration

	// LegDispersionBps is the leg-dispersion guard: each venue's bucket VWAP on a priced leg must be within this many bps of the leg VWAP, else the leg cannot corroborate. 0 = ToleranceBps.
	LegDispersionBps int

	// ReleaseBandPct is the mid-hold RELEASE agreement band (percent) against the current-bucket composite; tighter than the shared releaseAgreementMaxPct, which a venue-specific +4 % offset would satisfy. 0 = default.
	ReleaseBandPct float64
}

const (
	// DefaultCompositeReferenceToleranceBps is 75 bps: wide enough for a daily FX leg's intraday drift, far inside the >5 sigma venue-specific spike the freeze targets.
	DefaultCompositeReferenceToleranceBps = 75

	// DefaultCompositeReferenceMinLegSources — 2 real venues on the
	// crypto/USD leg: a single-venue leg is the very
	// thing under suspicion and corroborates nothing.
	DefaultCompositeReferenceMinLegSources = 2

	// DefaultCompositeReferenceFXMaxAge is 76h: fx_quotes rows are keyed by DAY, so Monday's read of Friday's bucket is ~72h old and still valid.
	DefaultCompositeReferenceFXMaxAge = 76 * time.Hour

	// DefaultCompositeReferenceReleaseBandPct is 2 %. A genuine repricing lands the venue within low single digits of the
	// composite; a held venue-specific offset (~5-40 % measured) never does, nor does a +4 % offset the shared 5 % band would
	// wave through.
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
	// print (composite refuted it or was unavailable).
	corroborationBasisVenue = "venue"
)

// compositeVerdict is the reference's reading of one bucket.
type compositeVerdict string

const (
	compositeVerdictCorroborated compositeVerdict = "corroborated"
	compositeVerdictRefuted      compositeVerdict = "refuted"
	compositeVerdictUnavailable  compositeVerdict = "unavailable"
)

// compositeVerdicts enumerates every verdict so the gauge is set for ALL of them each evaluation (a label only ever set to 1 leaves stale
// 1s).
var compositeVerdicts = []compositeVerdict{
	compositeVerdictCorroborated, compositeVerdictRefuted, compositeVerdictUnavailable,
}

// legRef is one priced pair's CURRENT-tick publish (exact VWAP, distinct venue count); only confident publishes are recorded, so a frozen/dropped/empty leg fails the reference closed.
type legRef struct {
	price   *big.Rat
	sources int

	// dispersion is max |venueVWAP - legVWAP| / legVWAP across the distinct survivor venues (exact Rat; nil under two venues); the leg-
	// dispersion guard reads it.
	dispersion *big.Rat

	// dispersionUncomputable is set when a venue's own VWAP could not be computed: the guard cannot vouch for the leg, so it is refused
	// (`leg_dispersion=uncomputable`) rather than skipped.
	dispersionUncomputable bool

	// proxyShare is the fraction of survivor base volume (at the common scale the VWAP weights by) that fetchForTarget rewrote onto this pair
	// from a
	// stablecoin-proxy source pair ([aggregate.ProxyPair]): prints quoted in a stablecoin, priced here at par with real USD.
	proxyShare float64

	// surfaceDivergence is |proxyVWAP - ownQuoteVWAP| / ownQuoteVWAP, exposing a stablecoin de-peg the par rewrite absorbs; nil when a surface is empty, surfaceUncomputable when a VWAP failed.
	surfaceDivergence   *big.Rat
	surfaceUncomputable bool
}

// quoteSurfaceRefusal returns why this leg cannot stand for one quote asset (proxy-rewritten and own-quote prints disagree by more than
// maxBps, or the comparison could not run), or "".
func (lr legRef) quoteSurfaceRefusal(maxBps int) string {
	if lr.surfaceUncomputable {
		return "quote_surface=uncomputable"
	}
	if lr.surfaceDivergence != nil && lr.surfaceDivergence.Cmp(big.NewRat(int64(maxBps), 10_000)) > 0 {
		return fmt.Sprintf("quote_surface=%.1fbps", ratBps(lr.surfaceDivergence))
	}
	return ""
}

// quoteSurfaces measures how much of a published bucket came through a stablecoin-proxy rewrite (proxied holds those trade IDs) and how
// far
// those prints sit from the pair's own-quote prints; both are priced with computeNormalizedVWAP, as the leg VWAP is.
func (o *Orchestrator) quoteSurfaces(
	pair canonical.Pair, trades []canonical.Trade, proxied map[string]struct{},
) (share float64, divergence *big.Rat, uncomputable bool) {
	if len(proxied) == 0 {
		return 0, nil, false
	}
	scaled := aggregate.NormalizeAmountScale(trades, amountScaleDecimalsFor).Trades()
	var proxy, own []canonical.Trade
	proxyBase, totalBase := new(big.Int), new(big.Int)
	for i := range trades {
		totalBase.Add(totalBase, scaled[i].BaseAmount.BigInt())
		if _, ok := proxied[trades[i].ID()]; ok {
			proxy = append(proxy, trades[i])
			proxyBase.Add(proxyBase, scaled[i].BaseAmount.BigInt())
			continue
		}
		own = append(own, trades[i])
	}
	if len(proxy) == 0 || totalBase.Sign() <= 0 {
		return 0, nil, false
	}
	share, _ = new(big.Rat).SetFrac(proxyBase, totalBase).Float64() // i128:ok a share for reporting, not a served amount
	if len(own) == 0 {
		return share, nil, false
	}
	proxyVWAP, perr := o.computeNormalizedVWAP(proxy, pair)
	ownVWAP, oerr := o.computeNormalizedVWAP(own, pair)
	if perr != nil || oerr != nil || proxyVWAP.Sign() <= 0 || ownVWAP.Sign() <= 0 {
		return share, nil, true
	}
	divergence = new(big.Rat).Sub(proxyVWAP, ownVWAP)
	divergence.Abs(divergence).Quo(divergence, ownVWAP)
	return share, divergence, false
}

// legDispersion computes each venue's own VWAP over the post-filter survivors against the leg VWAP. (nil, false): under two venues, guard N/A. (nil, true): a venue VWAP could not be computed, which is NOT a pass, so the caller fails closed.
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
	f, _ := r.Float64() // i128:ok basis points for reporting only
	return f * 10_000
}

// compositeReference is the evaluated reference for one (target,
// window) bucket.
type compositeReference struct {
	verdict compositeVerdict

	// price is the composite (product of the chain legs) when the
	// reference resolved; nil when unavailable.
	price *big.Rat

	// divergencePct is |direct - composite| / composite * 100 when the reference resolved (see [Orchestrator.triangulationDivergencePct]);
	// reporting only, a float64 mirror of [deviationRatio] for gauges and logs.
	divergencePct float64

	// deviationRatio is the exact *big.Rat |direct - composite| / composite that [releaseCorroborated] compares against its band, avoiding float64 rounding at the boundary.
	deviationRatio *big.Rat

	// legSources maps each chain leg (canonical pair string) to its distinct source count on this bucket (FX legs: distinct FX providers in
	// the snap, normally 1).
	legSources map[string]int

	// legDispersionBps maps each priced leg to its venue dispersion in
	// bps (see legRef.dispersion); absent for FX legs.
	legDispersionBps map[string]float64

	// unavailable names why the reference could not be evaluated (e.g. "leg_sources=1", "fx_stale", "leg_not_refreshed"); empty when it
	// resolved.
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

// resolved reports whether the reference produced a composite (corroborated or refuted): those readings may stand in for the prior-tick
// chain sample and drive the release lens.
func (r compositeReference) resolved() bool {
	return r.verdict == compositeVerdictCorroborated || r.verdict == compositeVerdictRefuted
}

// reasonSuffix is appended to the phase-2 freeze reason (freeze_events.detail->>'reason', the `freeze engaged` log): basis, agreement
// measured, each leg's strength.
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

// compositeReferenceEligible reports whether the reference is evaluated: enabled, pair on the allow-list, and single-venue by the freeze's source_count leg. Multi-venue buckets are never evaluated.
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

// recordLegRef captures a confidently-published bucket as a CURRENT-tick leg, at the publish point in refreshPairWindow. Rebuilt every Tick; same single-Tick-at-a-time invariant as tickEdgeQuotes.
func (o *Orchestrator) recordLegRef(pair canonical.Pair, window time.Duration, vwap *big.Rat, trades []canonical.Trade) {
	o.setTickLegRef(pair, window, o.newLegRef(pair, vwap, trades, nil))
}

// newLegRef builds the reference-leg reading for a published bucket; proxied is the set of trade IDs fetchForTarget rewrote from a
// stablecoin-proxy source pair.
func (o *Orchestrator) newLegRef(pair canonical.Pair, vwap *big.Rat, trades []canonical.Trade, proxied map[string]struct{}) legRef {
	dispersion, uncomputable := o.legDispersion(pair, trades, vwap)
	share, surfaceDiv, surfaceUncomputable := o.quoteSurfaces(pair, trades, proxied)
	return legRef{
		price:                  new(big.Rat).Set(vwap), // defensive copy, as newEdgeQuote
		sources:                distinctSourceCount(trades),
		dispersion:             dispersion,
		dispersionUncomputable: uncomputable,
		proxyShare:             share,
		surfaceDivergence:      surfaceDiv,
		surfaceUncomputable:    surfaceUncomputable,
	}
}

// setTickLegRef records lr as this tick's reference leg for (pair, window).
func (o *Orchestrator) setTickLegRef(pair canonical.Pair, window time.Duration, lr legRef) {
	if o.tickLegRefs == nil {
		o.tickLegRefs = make(map[time.Duration]map[string]legRef, len(o.cfg.Windows))
	}
	if o.tickLegRefs[window] == nil {
		o.tickLegRefs[window] = make(map[string]legRef, len(o.cfg.Pairs))
	}
	o.tickLegRefs[window][pair.String()] = lr
}

// evaluateCompositeReference builds the reference for (pair, window) on the CURRENT bucket, compares it with the direct VWAP and records it in tickCompositeRefs.
// Every failure is fail-closed: verdict "unavailable" with a named cause, freeze semantics unchanged.
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
	compositeF, _ := composite.Float64() // i128:ok prices for a percentage divergence compare, not served
	directF, _ := direct.Float64()       // i128:ok prices for a percentage divergence compare, not served
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
	ref.deviationRatio = deviation
	tolerance := big.NewRat(int64(cfg.ToleranceBps), 10_000)
	if deviation.Cmp(tolerance) <= 0 {
		ref.verdict = compositeVerdictCorroborated
	} else {
		ref.verdict = compositeVerdictRefuted
	}
	return ref
}

// referenceLeg resolves ONE chain leg: (price, distinct source count, "") or (nil, 0, cause).
// A priced leg must be published THIS tick (refreshOrder guarantees legs refresh first) with >= MinLegSources venues. An FX leg is snapped at `now` via [FXStore], within FXMaxAge, all providers of the FX source class; nil FXStore reads as unavailable, never the cached VWAP (unknown provenance).
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
		// Leg-dispersion guard: two venues only count as two when they AGREE. A dominant venue plus a dust print 3 % off is one
		// opinion and an artefact, and the artefact can be the attacker's. A guard that could not run has not been passed.
		if lr.dispersionUncomputable {
			return nil, lr.sources, "leg_dispersion=uncomputable"
		}
		if lr.dispersion != nil && lr.dispersion.Cmp(big.NewRat(int64(cfg.LegDispersionBps), 10_000)) > 0 {
			return nil, lr.sources, fmt.Sprintf("leg_dispersion=%.1fbps", ratBps(lr.dispersion))
		}
		// Agreeing venues are still not one USD when part of the leg was
		// priced in a de-pegged stablecoin at par.
		if why := lr.quoteSurfaceRefusal(cfg.LegDispersionBps); why != "" {
			return nil, lr.sources, "leg_" + why
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
	if reason := fxSnapRejection(now, observedAt, source, cfg.FXMaxAge); reason != "" {
		return nil, 0, reason
	}
	return price, len(strings.Split(source, "+")), ""
}

// fxSnapRejection is the admission rule for every FX snap (chain leg and reference): within maxAge of now and every provider of the FX source class. Returns "" when usable, else the reason.
func fxSnapRejection(now, observedAt time.Time, source string, maxAge time.Duration) string {
	if observedAt.IsZero() || now.Sub(observedAt) > maxAge {
		return "fx_stale"
	}
	for _, p := range strings.Split(source, "+") {
		if !external.IsFXSource(p) {
			return "fx_source_class=" + p
		}
	}
	return ""
}

// emitCompositeReference publishes the verdict gauge (one series per verdict, exactly one is 1) and the per-leg source-count gauge; a corroborated bucket changes a freeze decision so it logs at Info, the rest at Debug.
func (o *Orchestrator) emitCompositeReference(pair canonical.Pair, window time.Duration, ref compositeReference) {
	for _, v := range compositeVerdicts {
		val := 0.0
		if v == ref.verdict {
			val = 1
		}
		obs.AggregatorCompositeCorroboration.WithLabelValues(pair.String(), windowLabel(window), string(v)).Set(val)
	}
	// Delete-then-set, as recordVenueVWAPs: a leg that did not resolve (or
	// was never reached) this tick must not keep last tick's reading.
	labels := prometheus.Labels{"pair": pair.String(), "window": windowLabel(window)}
	obs.AggregatorCompositeReferenceLegSources.DeletePartialMatch(labels)
	obs.AggregatorCompositeReferenceLegDispersionBps.DeletePartialMatch(labels)
	for leg, n := range ref.legSources {
		obs.AggregatorCompositeReferenceLegSources.WithLabelValues(pair.String(), windowLabel(window), leg).Set(float64(n))
	}
	for leg, bps := range ref.legDispersionBps {
		obs.AggregatorCompositeReferenceLegDispersionBps.WithLabelValues(pair.String(), windowLabel(window), leg).Set(bps)
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
	key := pair.String() + ":" + window.String()
	if ref.unavailable != o.lastCompositeUnavailable[key] {
		if o.lastCompositeUnavailable == nil {
			o.lastCompositeUnavailable = make(map[string]string)
		}
		o.lastCompositeUnavailable[key] = ref.unavailable
		// The unavailable alert's operator needs the cause, which no metric carries.
		if ref.unavailable != "" {
			o.logger.Info("composite reference unavailable", attrs...)
		}
	}
	if ref.verdict == compositeVerdictCorroborated {
		o.logger.Info("composite reference corroborates the direct print on the current bucket", attrs...)
		return
	}
	o.logger.Debug("composite reference evaluated", attrs...)
}

// clearCompositeReference drops every composite-reference series for (pair, window) on a tick where the pair is not eligible, so a stale "corroborated" gauge never claims a freeze consulted a reference it never saw.
// Label values MUST match emitCompositeReference's (windowLabel, not window.String()) or this deletes nothing.
func (o *Orchestrator) clearCompositeReference(pair canonical.Pair, window time.Duration) {
	labels := prometheus.Labels{"pair": pair.String(), "window": windowLabel(window)}
	obs.AggregatorCompositeCorroboration.DeletePartialMatch(labels)
	obs.AggregatorCompositeReferenceLegSources.DeletePartialMatch(labels)
	obs.AggregatorCompositeReferenceLegDispersionBps.DeletePartialMatch(labels)
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

// refreshOrder returns cfg.Pairs with the priced legs of every enabled composite-reference chain moved to the front, since the reference is evaluated inside the TARGET's refresh and its legs must publish first.
// Returns cfg.Pairs itself when the mechanism is off; refresh order does not affect per-pair output.
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
