package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TriangulationChain is one chain pricing entry: a Target pair and the ordered Legs whose product yields it; see [ValidateTriangulationChain].
type TriangulationChain struct {
	Target canonical.Pair
	Legs   []canonical.Pair
}

// ValidateTriangulationChain returns an error naming the violation when legs do not chain or Target endpoints do not match; run once per chain at startup.
func ValidateTriangulationChain(chain TriangulationChain) error {
	if len(chain.Legs) < 2 {
		return fmt.Errorf("triangulation: chain for %s has %d legs, want at least 2",
			chain.Target.String(), len(chain.Legs))
	}
	first := chain.Legs[0]
	last := chain.Legs[len(chain.Legs)-1]
	if !first.Base.Equal(chain.Target.Base) {
		return fmt.Errorf("triangulation: chain for %s — first leg base %s != target base %s",
			chain.Target.String(), first.Base.String(), chain.Target.Base.String())
	}
	if !last.Quote.Equal(chain.Target.Quote) {
		return fmt.Errorf("triangulation: chain for %s — last leg quote %s != target quote %s",
			chain.Target.String(), last.Quote.String(), chain.Target.Quote.String())
	}
	for i := 0; i < len(chain.Legs)-1; i++ {
		if !chain.Legs[i].Quote.Equal(chain.Legs[i+1].Base) {
			return fmt.Errorf("triangulation: chain for %s — leg[%d].Quote=%s does not match leg[%d].Base=%s",
				chain.Target.String(),
				i, chain.Legs[i].Quote.String(),
				i+1, chain.Legs[i+1].Base.String())
		}
	}
	for i, leg := range chain.Legs {
		// fx_quotes stores one rate_usd per ticker, so EUR/GBP is USD/GBP ÷ USD/EUR:
		// a route over it shares its data row with the USD pivot yet counts as disjoint.
		if isFXLeg(leg) && leg.Base.Code != "USD" && leg.Quote.Code != "USD" {
			return fmt.Errorf("triangulation: chain for %s — leg[%d] %s is a fiat/fiat leg without USD; route it through fiat:USD",
				chain.Target.String(), i, leg.String())
		}
	}
	return nil
}

// triangulateAll prices every configured target per window through one cross-rate edge graph (router.go). A target with >=2 agreeing routes is corroborated and feeds the freeze's source_count leg; per-target failures are logged and counted, never abort the tick.
// bucketEnd for the forex-snap rule is now.Truncate(window), so every region computes the same boundary (ADR-0018 determinism).
func (o *Orchestrator) triangulateAll(ctx context.Context) {
	if len(o.cfg.Triangulations) == 0 {
		return
	}
	// The tick's own clock read (o.tickNow), never a second wall-clock
	// read: the FX snap must key off the same instant as the leg VWAPs it
	// multiplies, and the injected clock must be able to drive it.
	now := o.tickClock()
	for _, window := range o.cfg.Windows {
		if err := ctx.Err(); err != nil {
			return
		}
		bucketEnd := now.Truncate(window)
		edges, legStatus := o.buildWindowEdges(ctx, window, bucketEnd)
		for i := range o.cfg.Triangulations {
			outcome := o.routeTarget(ctx, o.cfg.Triangulations[i], window, edges, legStatus[i])
			o.settleDirect(ctx, o.cfg.Triangulations[i].Target, window, outcome == "ok")
			obs.AggregatorTriangulationsTotal.WithLabelValues(outcome).Inc()
		}
	}
}

// tickClock is the current tick's clock read, or a fresh read of the
// injected clock outside a Tick.
func (o *Orchestrator) tickClock() time.Time {
	if o.tickNow.IsZero() {
		return o.clock().UTC()
	}
	return o.tickNow
}

// chainLegStatus is the per-chain leg result: frozen (target inherits the freeze), hardFail (Redis/parse, target untrustworthy this tick), legDry (missing_leg, so any route found is a SUBSTITUTE reroute, not the documented chain).
type chainLegStatus struct {
	frozen    bool
	frozenLeg canonical.Pair
	hardErr   string // "redis_error" / "parse_error", or "" on success

	// legDry is set when a configured leg had no price this tick. A target reached despite it is a reroute, which must be confidence-gated on a sane floor and flagged, not silently published over the direct price.
	legDry bool
	dryLeg canonical.Pair
}

// legEdgeConfidence is the confidence of an FX (fiat/fiat) leg resolved via forex-snap. It is a ceiling: route confidence is the minimum across edges, so 1.0 means the FX reference is never the limiting edge.
// Only FX legs (isFXLeg) use it; cached non-FX legs use [cachedLegConfidence] so a stale crypto leg can still be the weakest link.
const legEdgeConfidence = 1.0

// cachedLegConfidence is the confidence of a non-FX leg resolved from cache (unknown freshness). It is the bootstrap cap, so such a route cannot clear [aggregate.RouteTrustFloor]: it neither displaces a direct price as a reroute nor corroborates.
const cachedLegConfidence = confidence.BootstrapConfidenceCap

// buildWindowEdges assembles one window's edge graph: priced-pair VWAPs with their real confidence ([newEdgeQuote]) plus every chain's resolved legs, and returns the per-chain leg status the router pass needs.
func (o *Orchestrator) buildWindowEdges(
	ctx context.Context, window time.Duration, bucketEnd time.Time,
) ([]aggregate.RouteLeg, []chainLegStatus) {
	quoteByPair := make(map[string]aggregate.Quote, len(o.tickEdgeQuotes[window])+len(o.cfg.Triangulations)*2)
	for _, q := range o.tickEdgeQuotes[window] {
		quoteByPair[q.Pair.String()] = q
	}
	status := make([]chainLegStatus, len(o.cfg.Triangulations))
	for i := range o.cfg.Triangulations {
		status[i] = o.resolveChainLegs(ctx, o.cfg.Triangulations[i], window, bucketEnd, quoteByPair)
	}
	quotes := make([]aggregate.Quote, 0, len(quoteByPair))
	for _, q := range quoteByPair {
		quotes = append(quotes, q)
	}
	edges, err := aggregate.BuildEdges(quotes)
	if err != nil {
		// A non-positive edge price should never reach here (VWAP rejects
		// empty windows; legPrice returns positive rationals), but fail
		// closed loudly rather than route off a malformed graph.
		o.logger.Warn("triangulation: edge graph build failed",
			"window", window.String(), "err", err)
		return nil, status
	}
	return edges, status
}

// resolveChainLegs resolves one chain's legs into the shared edge map. A leg already priced this tick keeps its real confidence; FX/cache legs get [legEdgeConfidence].
// FX-snap runs per chain, not per distinct leg, to keep stellarindex_aggregator_fx_snap_fallback_total calibration unchanged.
func (o *Orchestrator) resolveChainLegs(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	bucketEnd time.Time,
	quoteByPair map[string]aggregate.Quote,
) chainLegStatus {
	var st chainLegStatus
	for _, leg := range chain.Legs {
		price, provenance, outcome := o.legPrice(ctx, chain, leg, window, bucketEnd)
		switch outcome {
		case "":
			if _, seen := quoteByPair[leg.String()]; !seen {
				quoteByPair[leg.String()] = aggregate.Quote{
					Pair: leg, Price: price, Confidence: legConfidence(leg), Provenance: provenance,
				}
			}
		case outcomeFrozenLeg:
			st.frozen = true
			st.frozenLeg = leg
		case "redis_error", "parse_error":
			st.hardErr = outcome
		case "missing_leg", outcomeStaleLeg:
			// Leg absent this tick: leave it out of the graph; the router may reroute. Record it (legDry) so [routeTarget] gates and flags any substitute path.
			st.legDry = true
			st.dryLeg = leg
		}
	}
	return st
}

// routeTarget prices ONE target through the edge graph (direct base-quote edge excluded, so routes use hubs) and returns the
// [obs.AggregatorTriangulationsTotal] outcome label.
// low_confidence = no route clearing min_route_confidence, or a reroute below [aggregate.RouteTrustFloor] (flagged, not published over the
// direct price); a target frozen on its OWN market keeps serving its frozen LKG.
func (o *Orchestrator) routeTarget(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	edges []aggregate.RouteLeg,
	st chainLegStatus,
) string {
	if st.hardErr != "" {
		// A leg hard-failed (Redis/parse): we can't trust this target's
		// composite this tick. Same refusal-to-publish semantics as the
		// static path.
		return st.hardErr
	}
	routeEdges := excludeDirectEdge(edges, chain.Target)
	composite, combinedConf, servedRouteCount, pathCount, corroboration, diverged, lowConf, err := aggregate.CombineRoutes(
		routeEdges, chain.Target.Base, chain.Target.Quote, o.cfg.MaxHops, o.cfg.MinRouteConfidence)
	switch {
	case errors.Is(err, aggregate.ErrNoRoute):
		// Unreachable. A frozen leg of THIS chain means freeze inheritance (LKG served, flags.frozen); otherwise the legs are dry (missing_leg, which the chains-dry alert watches).
		// If the target published its own direct print this tick, serve it unmarked under a distinct outcome: inheriting the freeze would expire a fresh value to FreezeTTL and mark it frozen, and it is not dry.
		if st.frozen {
			if _, ok := o.tickEdgePrice(chain.Target, window); ok {
				return outcomeFrozenLegDirectServed
			}
			o.inheritLegFreeze(ctx, chain, window, st.frozenLeg)
			return outcomeFrozenLeg
		}
		return "missing_leg"
	case err != nil:
		o.logger.Warn("triangulation: route combine failed",
			"chain", chain.Target.String(), "err", err)
		return "parse_error"
	}

	// A dry leg, or a frozen leg the router walked around, means the composite came from a SUBSTITUTE path: gate it on the route trust floor and flag Rerouted.
	// A frozen leg's LKG was declined upstream, so a reroute must not republish at max trust.
	rerouted := st.legDry || st.frozen

	// The TARGET froze on its own direct market: do not overwrite its frozen LKG with a fresh composite. Flag the meta for Step 3, record no corroboration, and reuse the frozen_leg outcome.
	if o.frozenLeg(chain.Target, window) {
		o.writeCompositeMeta(ctx, chain.Target, window, compositeMeta{
			ServedRouteCount:   servedRouteCount,
			PathCount:          pathCount,
			CombinedConfidence: combinedConf,
			LowConfidence:      lowConf,
			Diverged:           diverged,
			Rerouted:           rerouted,
		})
		return outcomeFrozenLeg
	}

	if lowConf || (rerouted && !aggregate.ClearsRouteTrustFloor(combinedConf)) {
		// Every route runs through a thin edge below min_route_confidence, or a reroute missed the floor: do not overwrite the direct price. Carry the flags for Step 3 and record no corroboration, so the freeze falls back to the direct source count.
		if rerouted && !lowConf {
			// Observability: a thin substitute was BLOCKED from
			// displacing the direct price — surface which leg dried so it is
			// not a silent behaviour change.
			o.logger.Info("triangulation: leg-substitution reroute below floor — direct price serves",
				"chain", chain.Target.String(),
				"dry_leg", st.dryLeg.String(),
				"window", window.String(),
				"combined_confidence", combinedConf,
				"floor", aggregate.RouteTrustFloor,
			)
		}
		o.writeCompositeMeta(ctx, chain.Target, window, compositeMeta{
			ServedRouteCount:   servedRouteCount,
			PathCount:          pathCount,
			CombinedConfidence: combinedConf,
			LowConfidence:      true,
			Diverged:           diverged,
			Rerouted:           rerouted,
		})
		return "low_confidence"
	}
	if o.refuseProxyPivot(ctx, chain, window, compositeMeta{
		ServedRouteCount: servedRouteCount, PathCount: pathCount, CombinedConfidence: combinedConf, Diverged: diverged, Rerouted: rerouted,
	}) {
		return outcomeProxyPivot
	}
	return o.publishComposite(ctx, chain, window, composite, servedRouteCount, pathCount, corroboration, combinedConf, diverged, rerouted)
}

// publishComposite writes a confident composite to the target's VWAP key with provenance, streams it, and records it as next tick's corroboration. rerouted marks a leg-substitution publish that cleared the floor, flagged for observability.
func (o *Orchestrator) publishComposite(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	composite *big.Rat,
	servedRouteCount, pathCount, corroboration int,
	combinedConf float64,
	diverged, rerouted bool,
) string {
	value := formatRatFixed(composite, 12)

	// Never overwrite the served direct price with a rendering that reparses non-positive for a positive composite: a pathologically tiny value would clamp, serve price 0 and collapse the next window's edge graph.
	if composite.Sign() > 0 {
		if parsed, ok := new(big.Rat).SetString(value); !ok || parsed.Sign() <= 0 {
			o.logger.Warn("triangulation: composite rendered non-positive — refusing to publish",
				"chain", chain.Target.String(),
				"window", window.String(),
				"rendered", value)
			return "parse_error"
		}
	}

	key := cachekeys.VWAP(chain.Target.Base, chain.Target.Quote, window)
	ttl := o.vwapTTL(window) // derived from this orchestrator's own cadence
	metaKey := cachekeys.VWAPCompositeMeta(chain.Target.Base, chain.Target.Quote, window)
	metaBody, err := json.Marshal(o.withPivotComposition(chain, window, o.withCorroborationBasis(chain.Target, window, compositeMeta{
		ServedRouteCount:   servedRouteCount,
		PathCount:          pathCount,
		CorroborationCount: corroboration,
		CombinedConfidence: combinedConf,
		LowConfidence:      false,
		Diverged:           diverged,
		Rerouted:           rerouted,
	})))
	if err != nil {
		o.logger.Warn("triangulation: composite meta encode failed — refusing to publish",
			"chain", chain.Target.String(), "err", err)
		return "parse_error"
	}

	// Value, quality-flags meta, triangulated-provenance marker and observed-at stamp land in one MULTI/EXEC: a reader sees the old state or all of the new, never a value under a prior tick's qualifiers.
	provKey := cachekeys.VWAPProvenance(chain.Target.Base, chain.Target.Quote, window)
	atKey := cachekeys.VWAPObservedAt(chain.Target.Base, chain.Target.Quote, window)
	covKey := cachekeys.VWAPCoverage(chain.Target.Base, chain.Target.Quote, window)
	bucketEnd := o.tickClock().Truncate(closedBucket)
	if _, err := o.cache.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, metaKey.String(), metaBody, ttl)
		p.Set(ctx, provKey.String(), cachekeys.VWAPProvenanceTriangulated, ttl)
		p.Set(ctx, atKey.String(), cachekeys.FormatVWAPObservedAt(bucketEnd), ttl)
		// A composite does not track its legs' coverage: unknown, never a
		// direct print's coverage left beneath it.
		p.Del(ctx, covKey.String())
		p.Set(ctx, key.String(), value, ttl)
		return nil
	}); err != nil {
		o.logger.Warn("triangulation: composite publish failed — refusing to publish",
			"chain", chain.Target.String(), "err", err)
		return "redis_error"
	}
	// The served value is now the composite: the direct comparator's coverage
	// no longer describes it, so a later reseed or hold must not restore it.
	delete(o.prevVWAPCoverage, chain.Target.String()+":"+window.String())

	// The stream carries the value /v1/price serves at this window, on the
	// same once-per-closed-bucket contract as the direct path.
	o.streamBucketOnce(ctx, chain.Target, window, value, bucketEnd, nil)

	// The VWAP key was just written, so stamp the pair write clock the staleness gauge reads. This is the key's second writer: a target served only via its chain never reaches refreshPairWindow's stamp and would read as dead. Only here, after the value landed, with the Tick's clock.
	o.recordPairWrite(chain.Target, o.tickNow)

	// Corroboration input for the NEXT tick (freeze source_count via the independent corroboration count, and the divergence factor). Recorded only on a confident publish so failed or unpublished targets leave no evidence behind.
	o.recordComposite(chain.Target, window, composite, corroboration, combinedConf, diverged)
	return "ok"
}

// excludeDirectEdge removes the target's own direct base-quote edge (both directions) so [routeTarget] prices strictly through hub assets.
func excludeDirectEdge(edges []aggregate.RouteLeg, target canonical.Pair) []aggregate.RouteLeg {
	out := make([]aggregate.RouteLeg, 0, len(edges))
	for _, e := range edges {
		if (e.From.Equal(target.Base) && e.To.Equal(target.Quote)) ||
			(e.From.Equal(target.Quote) && e.To.Equal(target.Base)) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// newEdgeQuote builds a published (pair, window) VWAP as a router edge input, reusing the pair's quality signal ([edgeConfidence]). Built only after a confident publish, so frozen/empty/below-floor windows add no edge; min_usd_volume keeps dust pairs out of the graph.
func newEdgeQuote(
	pair canonical.Pair,
	vwap *big.Rat,
	conf confidenceComputation,
	confOK bool,
	trades []canonical.Trade,
) aggregate.Quote {
	return aggregate.Quote{
		Pair: pair,
		// Defensive copy: vwap is the caller's working value and must not
		// alias into the edge graph or the next tick.
		Price:      new(big.Rat).Set(vwap),
		Confidence: edgeConfidence(conf, confOK, trades),
	}
}

// appendTickEdgeQuote adds one published pair to this tick's router edge
// inputs for window.
func (o *Orchestrator) appendTickEdgeQuote(window time.Duration, q aggregate.Quote) {
	if o.tickEdgeQuotes == nil {
		o.tickEdgeQuotes = make(map[time.Duration][]aggregate.Quote, len(o.cfg.Windows))
	}
	o.tickEdgeQuotes[window] = append(o.tickEdgeQuotes[window], q)
}

// tickEdgePrice returns the VWAP refreshPairWindow priced (pair, window) at on this tick (its router edge), false when this tick did not
// price it.
func (o *Orchestrator) tickEdgePrice(pair canonical.Pair, window time.Duration) (*big.Rat, bool) {
	for _, q := range o.tickEdgeQuotes[window] {
		if q.Pair.Base.Equal(pair.Base) && q.Pair.Quote.Equal(pair.Quote) {
			return new(big.Rat).Set(q.Price), true
		}
	}
	return nil, false
}

// edgeConfidence derives an edge's weakest-link confidence from the multi-factor score, else the source-count factor alone
// ([confidence.SourceCountFactor]).
// The unscored fallback is capped at [confidence.BootstrapConfidenceCap]: uncapped, a bare source-count factor outranked fully-scored
// edges and cleared the reroute and corroboration gates.
func edgeConfidence(conf confidenceComputation, confOK bool, trades []canonical.Trade) float64 {
	if confOK {
		return conf.Score.Confidence
	}
	return math.Min(
		confidence.SourceCountFactor(distinctSourceCount(trades)),
		confidence.BootstrapConfidenceCap,
	)
}

// compositeMeta is the router quality decomposition carried alongside a composite so downstream (market-cap gating, /v1/price flags)
// needn't recompute; written to [cachekeys.VWAPCompositeMeta].
type compositeMeta struct {
	// ServedRouteCount is the size of the highest-confidence tier that produced the served composite, unrelated to PathCount (the survivor count of the full gated set); never read PathCount as how many routes back the value.
	ServedRouteCount int `json:"served_route_count"`
	PathCount        int `json:"path_count"`
	// CorroborationCount is the router's independent, tightly-agreeing route count behind a PUBLISHED composite
	// (aggregate.CombineRoutes' corroborationCount); 0 on every refusal. Audit only: never feeds a source count
	// (triangulate_corroborate.go).
	CorroborationCount int     `json:"corroboration_count"`
	CombinedConfidence float64 `json:"combined_confidence"`
	LowConfidence      bool    `json:"low_confidence"`
	Diverged           bool    `json:"diverged"`
	// Rerouted marks a composite whose routes substituted around a DRY configured leg; if it also failed the trust floor it was not published (LowConfidence is set too).
	Rerouted bool `json:"rerouted,omitempty"`

	// CorroborationBasis and CompositeLegSources record the composite-reference reading the freeze used this tick ("composite" corroborated, "venue" refuted or unavailable); omitted when not evaluated. CompositeLegSources maps each leg to its distinct venue/provider count, never a count for the target itself.
	CorroborationBasis  string         `json:"corroboration_basis,omitempty"`
	CompositeLegSources map[string]int `json:"composite_leg_sources,omitempty"`

	// PivotProxyShare maps each stablecoin-proxy leg to its share of base volume priced in a par stablecoin; PivotSurfaceRefusal names the leg whose stablecoin and own-quote prints disagree (outcome proxy_pivot).
	PivotProxyShare     map[string]float64 `json:"pivot_proxy_share,omitempty"`
	PivotSurfaceRefusal string             `json:"pivot_surface_refusal,omitempty"`
	// PivotUnverified marks a published composite with a priced leg made
	// only of stablecoin-proxy prints: no own-quote prints to check the
	// par assumption against, so a de-peg there cannot be refused.
	PivotUnverified bool `json:"pivot_unverified,omitempty"`
}

// outcomeProxyPivot is the outcome for a composite refused because a priced leg's stablecoin-proxy prints disagree with its own-quote
// prints beyond the leg-dispersion bound: the direct serves.
const outcomeProxyPivot = "proxy_pivot"

// pivotComposition reports, for legs published this tick, the stablecoin-proxy share, the first leg whose two quote surfaces disagree ("" if none), and whether any leg lacked an own-quote surface. Unpublished legs are not judged.
func (o *Orchestrator) pivotComposition(chain TriangulationChain, window time.Duration) (shares map[string]float64, refusal string, unverified bool) {
	maxBps := o.cfg.CompositeReference.withDefaults().LegDispersionBps
	for _, leg := range chain.Legs {
		lr, ok := o.tickLegRefs[window][leg.String()]
		if !ok || isFXLeg(leg) || lr.proxyShare <= 0 {
			continue
		}
		if shares == nil {
			shares = make(map[string]float64, len(chain.Legs))
		}
		shares[leg.String()] = lr.proxyShare
		if lr.surfaceDivergence == nil && !lr.surfaceUncomputable {
			unverified = true
		}
		if why := lr.quoteSurfaceRefusal(maxBps); why != "" && refusal == "" {
			refusal = leg.String() + " " + why
		}
	}
	return shares, refusal, unverified
}

// refuseProxyPivot withholds a composite whose pivot leg is two different USDs (real-USD prints vs stablecoin prints taken at par that
// disagree) rather than overwrite the direct print;
// writes the flags for Step 3 and reports whether it refused.
func (o *Orchestrator) refuseProxyPivot(ctx context.Context, chain TriangulationChain, window time.Duration, meta compositeMeta) bool {
	shares, refusal, _ := o.pivotComposition(chain, window)
	if refusal == "" {
		return false
	}
	o.logger.Warn("triangulation: pivot leg's stablecoin-proxy prints disagree with its own-quote prints — direct price serves",
		"chain", chain.Target.String(), "window", window.String(), "refusal", refusal)
	meta.PivotProxyShare = shares
	meta.PivotSurfaceRefusal = refusal
	o.writeCompositeMeta(ctx, chain.Target, window, meta)
	return true
}

// withCorroborationBasis stamps this tick's composite-reference reading
// (if any) onto a composite_meta before it is written.
func (o *Orchestrator) withCorroborationBasis(target canonical.Pair, window time.Duration, meta compositeMeta) compositeMeta {
	ref, ok := o.currentCompositeReference(target, window)
	if !ok {
		return meta
	}
	meta.CorroborationBasis = ref.basis()
	if len(ref.legSources) > 0 {
		meta.CompositeLegSources = ref.legSources
	}
	return meta
}

// withPivotComposition stamps the chain legs' stablecoin-proxy share and
// unverified-pivot flag onto a composite_meta before it is written.
func (o *Orchestrator) withPivotComposition(chain TriangulationChain, window time.Duration, meta compositeMeta) compositeMeta {
	meta.PivotProxyShare, _, meta.PivotUnverified = o.pivotComposition(chain, window)
	return meta
}

// writeCompositeMeta persists a composite's quality flags for Step 3, best-effort (failure logs at debug). Used where the served value is not overwritten; the overwriting path writes meta in the same MULTI/EXEC ([publishComposite]). TTL matches the VWAP key.
func (o *Orchestrator) writeCompositeMeta(
	ctx context.Context, target canonical.Pair, window time.Duration, meta compositeMeta,
) {
	if err := o.setCompositeMeta(ctx, target, window, meta); err != nil {
		o.logger.Debug("triangulation: composite meta set failed",
			"chain", target.String(), "err", err)
	}
}

// setCompositeMeta writes the composite quality flags and returns any
// error.
func (o *Orchestrator) setCompositeMeta(
	ctx context.Context, target canonical.Pair, window time.Duration, meta compositeMeta,
) error {
	body, err := json.Marshal(o.withCorroborationBasis(target, window, meta))
	if err != nil {
		return err
	}
	key := cachekeys.VWAPCompositeMeta(target.Base, target.Quote, window)
	return o.cache.Set(ctx, key.String(), body, o.vwapTTL(window)).Err()
}

// isFXLeg reports whether both sides are [canonical.AssetFiat], the structural test for the forex-snap rule (ADR-0018); crypto/fiat legs stay on the cached-VWAP path.
func isFXLeg(leg canonical.Pair) bool {
	return leg.Base.Type == canonical.AssetFiat && leg.Quote.Type == canonical.AssetFiat
}

// legConfidence returns [legEdgeConfidence] for an FX leg and [cachedLegConfidence] for a non-FX cached leg, which must be able to limit the route.
func legConfidence(leg canonical.Pair) float64 {
	if isFXLeg(leg) {
		return legEdgeConfidence
	}
	return cachedLegConfidence
}

// legPrice returns a leg's price and provenance ([aggregate.RouteLeg.Provenance]), or (nil, nil, outcomeLabel) on a recoverable miss. FX
// legs try the forex-snap path via [Config.FXStore];
// misses and snaps refused by [fxSnapRejection] fall back to cached VWAP. Only a genuine snap hit carries FX provenance (USD/GBP and
// EUR/GBP share a GBP row, which must not count as independent corroboration).
func (o *Orchestrator) legPrice(
	ctx context.Context,
	chain TriangulationChain,
	leg canonical.Pair,
	window time.Duration,
	bucketEnd time.Time,
) (*big.Rat, []string, string) {
	if isFXLeg(leg) && o.cfg.FXStore != nil {
		price, observedAt, source, err := o.cfg.FXStore.FXQuoteAtOrBefore(ctx, leg, bucketEnd, external.FXSources())
		switch {
		case err == nil:
			// Same budget and source class the corroborating reference
			// enforces: a dead FX feed must not keep pricing the chain off
			// its last rate as a fresh composite.
			reason := fxSnapRejection(o.tickClock(), observedAt, source,
				o.cfg.CompositeReference.withDefaults().FXMaxAge)
			if reason == "" {
				return price, fxLegProvenance(leg), ""
			}
			o.logger.Warn("triangulation: FX snap refused",
				"chain", chain.Target.String(), "leg", leg.String(), "reason", reason)
		case errors.Is(err, timescale.ErrNoFXQuote):
		default:
			o.logger.Warn("triangulation: FX snap query failed",
				"chain", chain.Target.String(),
				"leg", leg.String(),
				"err", err)
			return nil, nil, "redis_error"
		}
		// Snap missing or refused: soft fallback to the cached VWAP —
		// degraded, but the chain can still publish. Counter drives the
		// dashboard / alert (>50% sustained = FX ingestion is sick).
		obs.AggregatorFXSnapFallbackTotal.WithLabelValues(leg.String()).Inc()
	}
	price, outcome := o.legPriceFromCache(ctx, chain, leg, window)
	return price, nil, outcome
}

// fxLegProvenance names the fx_quotes tickers an FX-snap price came from, excluding the USD anchor (mirrors fxSnapTickers in timescale/fx_quotes.go); USD/GBP and EUR/GBP share "fx:GBP" so the router refuses to count them as independent.
func fxLegProvenance(leg canonical.Pair) []string {
	var out []string
	if leg.Base.Code != "USD" {
		out = append(out, "fx:"+leg.Base.Code)
	}
	if leg.Quote.Code != "USD" {
		out = append(out, "fx:"+leg.Quote.Code)
	}
	return out
}

// outcomeFrozenLeg is the [obs.AggregatorTriangulationsTotal] label for "a leg of this chain was frozen this tick, so the chain did not
// publish".
const outcomeFrozenLeg = "frozen_leg"

// outcomeFrozenLegDirectServed is the [obs.AggregatorTriangulationsTotal] label for a frozen leg with the route unreachable but the target's own fresh direct print served unmarked; no freeze is inherited.
const outcomeFrozenLegDirectServed = "frozen_leg_direct_served"

// outcomeStaleLeg is legPriceFromCache's refusal of a leg this orchestrator refreshes itself but did not publish this tick; the caller
// treats it as a dry leg (out of the graph, any route around it gated and flagged as a reroute).
const outcomeStaleLeg = "stale_leg"

// legPriceFromCache reads a leg's cached VWAP for non-FX legs and FX snap misses. It refuses a leg frozen earlier this tick (the freeze
// keeps its LKG in cache; reading it would launder a declined value into a derived pair
// with no frozen flag; the caller inherits the freeze). A pair the aggregator prices itself resolves to this tick's VWAP or
// [outcomeStaleLeg], never a previous tick's entry.
func (o *Orchestrator) legPriceFromCache(
	ctx context.Context,
	chain TriangulationChain,
	leg canonical.Pair,
	window time.Duration,
) (*big.Rat, string) {
	if o.frozenLeg(leg, window) {
		return nil, outcomeFrozenLeg
	}
	if containsPair(o.cfg.Pairs, leg) {
		if price, ok := o.tickEdgePrice(leg, window); ok {
			return price, ""
		}
		return nil, outcomeStaleLeg
	}
	key := cachekeys.VWAP(leg.Base, leg.Quote, window)
	raw, err := o.cache.Get(ctx, key.String()).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, "missing_leg"
	case err != nil:
		o.logger.Warn("triangulation: cache get failed",
			"chain", chain.Target.String(),
			"leg", leg.String(),
			"err", err)
		return nil, "redis_error"
	}
	price, ok := new(big.Rat).SetString(raw)
	if !ok {
		o.logger.Warn("triangulation: parse leg VWAP",
			"chain", chain.Target.String(),
			"leg", leg.String(),
			"raw", raw)
		return nil, "parse_error"
	}
	// A leg VWAP parsing non-positive (e.g. a legacy zero string) must not enter the edge graph: BuildEdges rejects Sign()<=0 by nilling the whole window. Treat it as an absent leg so only this target is missing_leg.
	if price.Sign() <= 0 {
		o.logger.Warn("triangulation: leg VWAP parsed non-positive — treating leg as absent",
			"chain", chain.Target.String(),
			"leg", leg.String(),
			"raw", raw)
		return nil, "missing_leg"
	}
	return price, ""
}

// inheritLegFreeze applies direct-pair freeze semantics to a target whose chain could not publish because a leg was frozen (keep the prior
// value's TTL alive, set flags.frozen), else a stale derived price serves unmarked.
// It does NOT enter the freeze lifecycle (no hold, ladder or marker): the leg's lifecycle owns the hold, and a second ladder would outlast
// the leg and page twice. Best-effort: LKG read and marker write log, not propagate.
func (o *Orchestrator) inheritLegFreeze(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	leg canonical.Pair,
) {
	// The target's own direct value, if it priced this tick, is what
	// serves under the inherited flag and what the freeze row records.
	o.settleDirect(ctx, chain.Target, window, false)

	o.mu.Lock()
	o.freezesEngaged++
	o.mu.Unlock()

	class := anomaly.ClassDefault
	if o.cfg.Anomaly != nil {
		class = o.cfg.Anomaly.ClassOf(chain.Target.Base)
	}
	obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(class)).Inc()

	o.logger.Warn("triangulation: leg frozen — target inherits the freeze",
		"chain", chain.Target.String(),
		"leg", leg.String(),
		"window", window.String(),
		"class", class,
		"writer_wired", o.cfg.FreezeWriter != nil,
	)

	// The chain skipped its value write, so the target's prior value
	// must outlive its ordinary TTL exactly as a directly-frozen pair's
	// does.
	o.keepFrozenVWAPAlive(ctx, chain.Target, window, cachekeys.FreezeTTL)

	// The target has no lifecycle of its own, so frozen_since is the leg's.
	// Zero firedAt omits frozen_since: the target may have priced via a reroute after the leg froze.
	o.streamFrozenOnce(ctx, chain.Target, window, o.tickClock().Truncate(closedBucket), time.Time{})

	if o.cfg.FreezeWriter == nil {
		return
	}
	decision := anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  class,
		// No per-class deviation is computed here — the deviation that
		// justified the freeze was measured on the LEG. Reason carries
		// the provenance.
		Reason: fmt.Sprintf("triangulation:leg_frozen leg=%s window=%s", leg.String(), window.String()),
	}
	frozenValue, err := o.cache.Get(ctx, cachekeys.VWAP(chain.Target.Base, chain.Target.Quote, window).String()).Result()
	if err != nil {
		// redis.Nil (no prior derived value) is the ordinary first-tick
		// case; anything else is a blip. Either way an empty frozen
		// value is a valid marker — the sink stamps NULL.
		frozenValue = ""
	}
	if err := o.cfg.FreezeWriter.Mark(ctx, chain.Target.Base, chain.Target.Quote, frozenValue, decision); err != nil {
		o.logger.Warn("triangulation: inherited freeze marker write failed",
			"chain", chain.Target.String(), "err", err)
	}
}
