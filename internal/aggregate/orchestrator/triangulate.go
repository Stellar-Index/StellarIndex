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

// TriangulationChain is one chain pricing entry. Target is the
// implied pair (e.g. XLM/EUR); Legs is the ordered chain whose
// product yields the target price (e.g. [XLM/USD, USD/EUR]).
//
// Validation: at least 2 legs; Legs[0].Base must equal Target.Base;
// Legs[N-1].Quote must equal Target.Quote; adjacent legs must share
// their pivot asset (Legs[i].Quote == Legs[i+1].Base). Caller-side
// validation lives in [ValidateTriangulationChain].
type TriangulationChain struct {
	Target canonical.Pair
	Legs   []canonical.Pair
}

// ValidateTriangulationChain returns nil when the chain is
// structurally consistent (chainable legs, Target endpoints match
// the chain endpoints). Returns an error naming the specific
// violation otherwise. Cheap; runs once per chain at startup.
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
	return nil
}

// triangulateAll runs the post-refresh triangulation pass. For each
// window it builds ONE cross-rate edge graph (this tick's priced-pair
// VWAPs plus every configured chain's resolved legs) and prices every
// target through the graph router (internal/aggregate/router.go). A
// target reachable by a single route behaves exactly like the old
// static single-chain multiply (pathCount=1); a target reachable by ≥2
// agreeing routes is corroborated, which feeds the freeze's source_count
// leg (see triangulate_corroborate.go). Per-target failures are logged +
// counted but never abort the tick.
//
// `bucketEnd` for the X2.5 forex-snap rule is computed once per window as
// `now.Truncate(window)` — the most recent UTC-aligned boundary
// at-or-before now. Every region computes the same boundary for the same
// wall-clock now, which is what makes the chained-fiat output
// across-region deterministic per ADR-0018.
func (o *Orchestrator) triangulateAll(ctx context.Context) {
	if len(o.cfg.Triangulations) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, window := range o.cfg.Windows {
		if err := ctx.Err(); err != nil {
			return
		}
		bucketEnd := now.Truncate(window)
		edges, legStatus := o.buildWindowEdges(ctx, window, bucketEnd)
		for i := range o.cfg.Triangulations {
			outcome := o.routeTarget(ctx, o.cfg.Triangulations[i], window, edges, legStatus[i])
			obs.AggregatorTriangulationsTotal.WithLabelValues(outcome).Inc()
		}
	}
}

// chainLegStatus is the per-chain result of resolving that chain's legs
// while building the window's edge graph: whether a leg was frozen this
// tick (so an unreachable target inherits the freeze, MNY-22), whether a
// leg hard-failed (Redis/parse — the target can't be trusted this tick),
// and whether a configured leg was DRY (missing_leg) so any route the
// router still finds to the target is a SUBSTITUTE path (a reroute), not
// the documented direct chain.
type chainLegStatus struct {
	frozen    bool
	frozenLeg canonical.Pair
	hardErr   string // "redis_error" / "parse_error", or "" on success

	// legDry is set when a configured chain leg had no price this tick
	// (missing_leg). A target reached DESPITE a dry configured leg is a
	// reroute/leg-substitution — the router walked around the gap through
	// other markets — which R3 requires be confidence-gated on a sane
	// floor and flagged, not silently published over the direct price.
	legDry bool
	dryLeg canonical.Pair
}

// legEdgeConfidence is the weakest-link confidence assigned to an FX
// (fiat/fiat) chain leg that is not one of this tick's priced pairs — the
// legs that resolve through the X2.5 forex-snap path against the
// institutional FX feed (the authority-sanity reference). It is a MINIMUM
// ceiling, not a claim of perfection: the router's route confidence is the
// minimum across a route's edges, so a value of 1.0 here simply means the FX
// reference is never the LIMITING edge — the route's trust reflects the
// crypto/USD leg's own confidence score (the genuinely less-certain leg).
// Priced-pair legs never use this; they carry their real per-pair confidence
// from [recordEdgeQuote].
//
// This applies ONLY to FX legs (isFXLeg). A cached NON-FX leg uses the
// conservative [cachedLegConfidence] instead (L1): before the fix EVERY
// cached leg entered at 1.0, so a stale cached crypto leg could never be a
// route's weakest link.
const legEdgeConfidence = 1.0

// cachedLegConfidence is the weakest-link confidence assigned to a NON-FX
// chain leg resolved from CACHE (a crypto/crypto or crypto/fiat leg that was
// not one of this tick's priced pairs, so it carries no fresh per-pair quality
// signal from [recordEdgeQuote]). Unlike an FX leg it has unknown freshness
// and provenance, so it must be able to be the route's LIMITING edge rather
// than silently entering at max trust and dragging a stale crypto price
// through a hub at full confidence (L1). Set to the reroute/corroboration
// floor (0.5): a cache-only route sits exactly at the trust boundary —
// confident enough to publish + corroborate, but out-trusted by any priced
// pair scoring above it, and correctly limiting any route that also carries a
// thinner priced leg.
const cachedLegConfidence = 0.5

// buildWindowEdges assembles the cross-rate edge graph for one window:
// this tick's priced-pair VWAPs (each with its real per-pair
// confidence, captured in [recordEdgeQuote]) plus every configured
// chain's resolved legs (FX legs via the snap path, others from cache).
// It returns the deterministic edge set and the per-chain leg status the
// router pass needs to distinguish a dry leg from a frozen one.
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

// resolveChainLegs resolves one chain's legs into the shared window edge
// map and reports the chain's leg status. A leg that is already a priced
// pair this tick keeps its real per-pair confidence (not overwritten by
// the leg version); an FX/cache leg is added with [legEdgeConfidence].
//
// FX-snap resolution runs PER CHAIN (not once per distinct leg), so the
// stellarindex_aggregator_fx_snap_fallback_total counting stays exactly
// as it was before the router landed — the alert calibration is
// unchanged.
func (o *Orchestrator) resolveChainLegs(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	bucketEnd time.Time,
	quoteByPair map[string]aggregate.Quote,
) chainLegStatus {
	var st chainLegStatus
	for _, leg := range chain.Legs {
		price, outcome := o.legPrice(ctx, chain, leg, window, bucketEnd)
		switch outcome {
		case "":
			if _, seen := quoteByPair[leg.String()]; !seen {
				quoteByPair[leg.String()] = aggregate.Quote{
					Pair: leg, Price: price, Confidence: legConfidence(leg),
				}
			}
		case outcomeFrozenLeg:
			st.frozen = true
			st.frozenLeg = leg
		case "redis_error", "parse_error":
			st.hardErr = outcome
		case "missing_leg":
			// Leg absent this tick — leave it out of the graph. The router
			// may still reach the target via an alternative route, or
			// report the target unreachable (missing_leg) below. Record the
			// substitution so [routeTarget] can gate + flag any reroute (R3)
			// rather than let a thin substitute path silently overwrite the
			// direct price.
			st.legDry = true
			st.dryLeg = leg
		}
	}
	return st
}

// rerouteMinConfidence is the SANE confidence floor a leg-substitution
// reroute must clear before its composite may publish OVER the direct
// price (R3). It applies IN ADDITION to (and independently of)
// min_route_confidence — the shipped config leaves that knob at 0, so
// without this floor a dry FX leg could silently reroute through thin
// crypto cross-pairs and overwrite the direct price with an unvetted
// substitute. A reroute below this floor does not publish: the direct
// price serves and the reroute is flagged. It does NOT gate the ordinary
// (all-legs-present) chain, only reroutes. 0.5 is the same weakest-link
// floor the dust-edge guard uses (route_test.go): a substitute whose
// flimsiest leg is below it is not trustworthy enough to displace a
// direct market.
const rerouteMinConfidence = 0.5

// routeTarget prices ONE target through the window's edge graph and
// returns the [obs.AggregatorTriangulationsTotal] outcome label.
//
// The direct base→quote edge is excluded so the router is forced through
// hub assets — a 1-hop "route" equal to the direct price would
// corroborate nothing and defeat triangulation. Outcome mapping is
// backward-compatible with the static path: a single-route target
// publishes "ok" (or "missing_leg" when its leg is dry, which the
// triangulation-chains-dry alert reads); an unreachable target whose leg
// was frozen inherits the freeze ("frozen_leg", MNY-22). "low_confidence"
// covers both "no route clears min_route_confidence" AND (R3) "a
// leg-substitution reroute did not clear rerouteMinConfidence": in both
// the composite is flagged but NOT published over the direct price.
//
// R3 — when a configured leg is DRY (st.legDry) but the router still
// reaches the target, the composite came from a SUBSTITUTE path. The
// reroute is kept (it is the multi-path robustness we want) but it is (a)
// gated on rerouteMinConfidence so a thin substitute cannot silently
// displace the direct price, and (b) flagged (compositeMeta.Rerouted) so
// the substitution is observable rather than a silent behaviour change.
//
// H2 — a leg FROZEN this tick (st.frozen) that the router still reaches the
// target AROUND is the same kind of substitution as a dry leg, so it is gated
// + flagged identically (rerouted := st.legDry || st.frozen). And a target
// frozen this tick on its OWN direct market is left serving its frozen
// last-known-good: a fresh composite must not silently overwrite the value a
// freeze marker says is frozen (an honest value paired with a contradictory
// state). Both respect the freeze consistently instead of letting
// triangulation walk around it unflagged.
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
	composite, combinedConf, pathCount, corroboration, diverged, lowConf, err := aggregate.CombineRoutes(
		routeEdges, chain.Target.Base, chain.Target.Quote, o.cfg.MaxHops, o.cfg.MinRouteConfidence)
	switch {
	case errors.Is(err, aggregate.ErrNoRoute):
		// Unreachable. A frozen leg of THIS chain makes it a freeze
		// inheritance (MNY-22): the target keeps serving its LKG and
		// carries flags.frozen. Otherwise the legs are simply dry/absent —
		// the missing_leg the chains-dry alert watches.
		if st.frozen {
			o.inheritLegFreeze(ctx, chain, window, st.frozenLeg)
			return outcomeFrozenLeg
		}
		return "missing_leg"
	case err != nil:
		o.logger.Warn("triangulation: route combine failed",
			"chain", chain.Target.String(), "err", err)
		return "parse_error"
	}

	// A leg was DRY, or a leg FROZE this tick and the router reached the
	// target by walking AROUND it (st.frozen with a route found, not
	// ErrNoRoute) — either way the composite came from a SUBSTITUTE path, so
	// it is gated on rerouteMinConfidence and flagged Rerouted the same way
	// (H2). A frozen leg's LKG was declined upstream; a reroute around it must
	// not silently republish at max trust any more than a dry-leg reroute may.
	rerouted := st.legDry || st.frozen

	// The TARGET itself froze this tick on its own direct market. Respect that
	// freeze: do NOT overwrite its frozen last-known-good with a fresh
	// composite (the marker says frozen — overwriting the value contradicts
	// it). Leave the LKG serving, flag the meta for Step 3, and record no
	// corroboration. Reuses the frozen_leg outcome: "we refused to publish a
	// derived price because [the target] was frozen" (H2).
	if o.frozenLeg(chain.Target, window) {
		o.writeCompositeMeta(ctx, chain.Target, window, compositeMeta{
			PathCount:          pathCount,
			CombinedConfidence: combinedConf,
			LowConfidence:      lowConf,
			Diverged:           diverged,
			Rerouted:           rerouted,
		})
		return outcomeFrozenLeg
	}

	if lowConf || (rerouted && combinedConf < rerouteMinConfidence) {
		// Either every route runs through a dust/thin edge below
		// min_route_confidence, OR this is a leg-substitution reroute (R3)
		// whose best route does not clear the sane reroute floor. In both
		// cases: do NOT overwrite the direct price (requirement (b)); carry
		// the flags for Step 3 and leave the direct value serving. Nothing
		// is recorded as corroboration, so the freeze falls back to the
		// direct source count.
		if rerouted && !lowConf {
			// Observability (R3b): a thin substitute was BLOCKED from
			// displacing the direct price — surface which leg dried so it is
			// not a silent behaviour change.
			o.logger.Info("triangulation: leg-substitution reroute below floor — direct price serves",
				"chain", chain.Target.String(),
				"dry_leg", st.dryLeg.String(),
				"window", window.String(),
				"combined_confidence", combinedConf,
				"floor", rerouteMinConfidence,
			)
		}
		o.writeCompositeMeta(ctx, chain.Target, window, compositeMeta{
			PathCount:          pathCount,
			CombinedConfidence: combinedConf,
			LowConfidence:      true,
			Diverged:           diverged,
			Rerouted:           rerouted,
		})
		return "low_confidence"
	}
	return o.publishComposite(ctx, chain, window, composite, pathCount, corroboration, combinedConf, diverged, rerouted)
}

// publishComposite writes a CONFIDENT composite to the target's VWAP
// cache key (replacing the direct print for the tick, provenance
// stamped), records it as this tick's corroboration for the next tick,
// and carries its quality flags for Step 3. rerouted marks a publish that
// came from a leg-substitution path (R3) — it cleared rerouteMinConfidence
// so it publishes, but the substitution is flagged for observability.
func (o *Orchestrator) publishComposite(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	composite *big.Rat,
	pathCount, corroboration int,
	combinedConf float64,
	diverged, rerouted bool,
) string {
	value := formatRatFixed(composite, 12)

	// R-1 belt-and-suspenders: never overwrite the served direct price
	// with a rendering that reparses to a non-positive price for a
	// strictly-positive composite. formatRatFixed now renders
	// magnitude-relative precision so this cannot fire for any value
	// above 10^-formatRatMaxScale, but a pathologically tiny composite
	// would still clamp — refuse rather than publish a zero that would be
	// served as price 0 AND collapse the next tick's window edge graph.
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
	ttl := cachekeys.VWAPTTL(window)

	// R-2: write the composite's QUALIFIERS (quality-flags meta and the
	// triangulated-provenance marker) BEFORE the value they qualify, and
	// refuse the publish if either fails. The served value is load-bearing
	// and must never overwrite the direct price while its diverged/rerouted
	// flags lag, are missing, or carry a prior tick's state. Ordering the
	// dependents first makes the residual (non-atomic) read window
	// fail-SAFE: a reader landing mid-write sees the OLD value with the NEW
	// flags (over-warns) rather than the NEW value with stale clean flags
	// (silently under-warns a diverged/rerouted composite).
	if err := o.setCompositeMeta(ctx, chain.Target, window, compositeMeta{
		PathCount:          pathCount,
		CombinedConfidence: combinedConf,
		LowConfidence:      false,
		Diverged:           diverged,
		Rerouted:           rerouted,
	}); err != nil {
		o.logger.Warn("triangulation: composite meta set failed — refusing to publish",
			"chain", chain.Target.String(), "err", err)
		return "redis_error"
	}

	// Provenance marker — lets the API set flags.triangulated=true on the
	// Redis-fallback path.
	provKey := cachekeys.VWAPProvenance(chain.Target.Base, chain.Target.Quote, window)
	if err := o.cache.Set(ctx, provKey.String(), cachekeys.VWAPProvenanceTriangulated, ttl).Err(); err != nil {
		o.logger.Warn("triangulation: provenance marker set failed — refusing to publish",
			"chain", chain.Target.String(), "err", err)
		return "redis_error"
	}

	// Value LAST: once it lands, its qualifiers are already in place.
	if err := o.cache.Set(ctx, key.String(), value, ttl).Err(); err != nil {
		o.logger.Warn("triangulation: cache set failed",
			"chain", chain.Target.String(), "err", err)
		return "redis_error"
	}

	// Corroboration input for the NEXT tick — both the freeze's
	// source_count leg (via the INDEPENDENT corroboration count, not the
	// raw survivor pathCount) and the confidence divergence factor
	// ([Orchestrator.triangulationDivergencePct]). Recorded only on a
	// confident publish: a low-confidence, missing-leg, frozen-leg or
	// failed target must leave no value behind for the next tick to treat
	// as evidence.
	o.recordComposite(chain.Target, window, composite, corroboration, combinedConf, diverged)
	return "ok"
}

// excludeDirectEdge returns edges with the target's own direct
// base↔quote edge (both directions) removed, so [routeTarget] prices the
// composite strictly through hub assets.
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

// recordEdgeQuote captures a successfully-published (pair, window) VWAP
// as a router edge input for THIS tick's triangulation pass. The edge's
// weakest-link confidence reuses the pair's existing quality signal (see
// [edgeConfidence]); nothing new is invented. Called from
// refreshPairWindow only after a confident publish, so frozen / dropped /
// empty / below-floor windows contribute no edge — the min_usd_volume
// gate is what keeps a dust pair out of the cross-rate graph (INV-11).
func (o *Orchestrator) recordEdgeQuote(
	pair canonical.Pair,
	window time.Duration,
	vwap *big.Rat,
	conf confidenceComputation,
	confOK bool,
	trades []canonical.Trade,
) {
	if o.tickEdgeQuotes == nil {
		o.tickEdgeQuotes = make(map[time.Duration][]aggregate.Quote, len(o.cfg.Windows))
	}
	o.tickEdgeQuotes[window] = append(o.tickEdgeQuotes[window], aggregate.Quote{
		Pair: pair,
		// Defensive copy: vwap is the caller's working value and must not
		// alias into the edge graph or the next tick.
		Price:      new(big.Rat).Set(vwap),
		Confidence: edgeConfidence(conf, confOK, trades),
	})
}

// edgeConfidence derives a router edge's weakest-link confidence from
// the pair's EXISTING quality signals — the multi-factor confidence
// score when this tick computed one, else the source-count factor alone
// (the same [confidence.SourceCountFactor] the score itself uses). No
// new formula; a single-source edge still reads as low-confidence so it
// cannot set a confident cross.
//
// The unscored fallback is capped at [confidence.BootstrapConfidenceCap].
// An edge reaches it precisely when the scorer COULDN'T run — no
// baseline row, first tick after a restart, no prior comparator — which
// is the state the scorer itself treats as stricter than bootstrap (a
// negative BaselineAgeDays sentinel caps the score). Uncapped, a bare
// source-count factor (0.731 at 4 sources, 0.953 at 6) OUTRANKED every
// fully-scored edge and cleared both the reroute and corroboration
// gates that a scored edge only ties — so the least-evidenced edges,
// with no z-score, no liquidity measure and no cross-oracle check, were
// the ones setting composites and widening the freeze's source-count
// leg (cold audit 2026-08-03). Ranking among unscorable edges is
// preserved below the cap.
func edgeConfidence(conf confidenceComputation, confOK bool, trades []canonical.Trade) float64 {
	if confOK {
		return conf.Score.Confidence
	}
	return math.Min(
		confidence.SourceCountFactor(distinctSourceCount(trades)),
		confidence.BootstrapConfidenceCap,
	)
}

// compositeMeta is the router quality decomposition carried alongside a
// composite so downstream (Step 3: market-cap gating, /v1/price flags)
// can respect it without recomputing. Written to
// [cachekeys.VWAPCompositeMeta].
type compositeMeta struct {
	PathCount          int     `json:"path_count"`
	CombinedConfidence float64 `json:"combined_confidence"`
	LowConfidence      bool    `json:"low_confidence"`
	Diverged           bool    `json:"diverged"`
	// Rerouted marks a composite whose route(s) substituted around a DRY
	// configured chain leg (R3). true means the documented direct chain
	// could not resolve and the router walked an alternative path; when it
	// also failed rerouteMinConfidence the composite was NOT published over
	// the direct price (LowConfidence is set too). Lets Step 3 / the API
	// surface a leg-substitution instead of it being a silent change.
	Rerouted bool `json:"rerouted,omitempty"`

	// CorroborationBasis / CompositeLegSources (2026-08-29) carry the
	// composite-reference reading the freeze decision for this target
	// used on THIS tick: "composite" when the current-bucket reference
	// corroborated the direct print, "venue" when it refuted it or was
	// unavailable. Absent (omitempty) when the reference was not
	// evaluated (multi-venue bucket, target not allow-listed, mechanism
	// off) — so the JSON is byte-identical to before in those cases.
	// CompositeLegSources maps each chain leg to the distinct venue /
	// provider count behind it, so a reader can see how strong the
	// agreement was. Never a source count for the target itself.
	CorroborationBasis  string         `json:"corroboration_basis,omitempty"`
	CompositeLegSources map[string]int `json:"composite_leg_sources,omitempty"`
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

// writeCompositeMeta persists a composite's quality flags for Step 3.
// Best-effort: a marshal/write failure logs at debug and is swallowed.
// Used by the branches that do NOT overwrite the served value (frozen
// target, low-confidence / below-floor reroute) — there the meta is the
// only artefact and a miss just leaves the untouched direct value
// without flags. The value-overwriting path uses [setCompositeMeta]
// instead, which propagates the error so the write can be made atomic
// with the value it qualifies (R-2). TTL matches the VWAP key so the
// flags can't outlive the price they describe.
func (o *Orchestrator) writeCompositeMeta(
	ctx context.Context, target canonical.Pair, window time.Duration, meta compositeMeta,
) {
	if err := o.setCompositeMeta(ctx, target, window, meta); err != nil {
		o.logger.Debug("triangulation: composite meta set failed",
			"chain", target.String(), "err", err)
	}
}

// setCompositeMeta writes the composite quality flags and returns any
// error, so the publish path can refuse to overwrite the served value
// when its qualifiers could not be persisted (R-2).
func (o *Orchestrator) setCompositeMeta(
	ctx context.Context, target canonical.Pair, window time.Duration, meta compositeMeta,
) error {
	body, err := json.Marshal(o.withCorroborationBasis(target, window, meta))
	if err != nil {
		return err
	}
	key := cachekeys.VWAPCompositeMeta(target.Base, target.Quote, window)
	return o.cache.Set(ctx, key.String(), body, cachekeys.VWAPTTL(window)).Err()
}

// isFXLeg reports whether a leg should use the X2.5 forex-snap rule.
// Per the design note (approach A): a leg is FX iff its Base AND
// Quote are both [canonical.AssetFiat]. This is a structural test —
// crypto-vs-fiat legs (XLM/USD) stay on the cached-VWAP path because
// CEX/DEX trades dominate that pair; fiat-vs-fiat legs (USD/EUR) are
// the chained-fiat factor that ADR-0018 mandates be snapped.
func isFXLeg(leg canonical.Pair) bool {
	return leg.Base.Type == canonical.AssetFiat && leg.Quote.Type == canonical.AssetFiat
}

// legConfidence returns the weakest-link confidence for a CACHE-resolved
// chain leg: [legEdgeConfidence] (1.0) for an FX leg (the authoritative snap
// reference, never the limiting edge) and the conservative
// [cachedLegConfidence] for a non-FX cached leg (unknown freshness — it must
// be able to limit the route, L1).
func legConfidence(leg canonical.Pair) float64 {
	if isFXLeg(leg) {
		return legEdgeConfidence
	}
	return cachedLegConfidence
}

// legPrice returns the price for one leg of a triangulation chain.
// On success, returns (price, ""); on a recoverable miss/error,
// returns (nil, outcomeLabel) where outcomeLabel is the metric label
// the caller bubbles up via [obs.AggregatorTriangulationsTotal].
//
// FX legs (both sides fiat) attempt the X2.5 snap path via
// [Config.FXStore]. Snap misses (no FX quote at-or-before bucketEnd)
// fall back to the cached-VWAP path and increment
// [obs.AggregatorFXSnapFallbackTotal]; this keeps the chain publishing
// during fresh deploys / FX-source outages instead of black-holing.
// Snap DB errors propagate up as "redis_error"-class outcomes — the
// FX-store error means we can't trust ANY chained-fiat output this
// tick, so the chain skips publish.
//
// Non-FX legs (and FX legs when FXStore is nil) read the cached VWAP
// the per-pair refresh wrote earlier this tick.
func (o *Orchestrator) legPrice(
	ctx context.Context,
	chain TriangulationChain,
	leg canonical.Pair,
	window time.Duration,
	bucketEnd time.Time,
) (*big.Rat, string) {
	if isFXLeg(leg) && o.cfg.FXStore != nil {
		price, _, _, err := o.cfg.FXStore.FXQuoteAtOrBefore(ctx, leg, bucketEnd, external.FXSources())
		switch {
		case err == nil:
			return price, ""
		case errors.Is(err, timescale.ErrNoFXQuote):
			// Soft fallback to cached VWAP — degraded but the chain
			// still publishes. Counter drives the dashboard /
			// alert (>50% sustained = FX ingestion is sick).
			obs.AggregatorFXSnapFallbackTotal.WithLabelValues(leg.String()).Inc()
			// fallthrough to cached-VWAP read below
		default:
			o.logger.Warn("triangulation: FX snap query failed",
				"chain", chain.Target.String(),
				"leg", leg.String(),
				"err", err)
			return nil, "redis_error"
		}
	}
	return o.legPriceFromCache(ctx, chain, leg, window)
}

// outcomeFrozenLeg is the [obs.AggregatorTriangulationsTotal] label
// for "a leg of this chain was frozen this tick, so the chain did not
// publish" (MNY-22).
const outcomeFrozenLeg = "frozen_leg"

// legPriceFromCache reads a leg's freshly-cached VWAP. Used for non-
// FX legs and for FX legs when the snap path produced ErrNoFXQuote.
//
// Refuses (MNY-22) when the leg was frozen earlier in this same tick.
// A freeze means "we do not trust this pair's newest bucket, keep
// serving its last-known-good and flag it" — and the freeze path
// deliberately leaves that LKG in the cache (keepFrozenVWAPAlive).
// Reading it here would launder a value we just declined to publish
// into a DERIVED pair that carries no frozen flag of its own: the
// manipulated leg reaches consumers anyway, one multiplication later,
// looking fresh. The chain is refused instead, and the caller inherits
// the freeze onto the target ([Orchestrator.inheritLegFreeze]).
func (o *Orchestrator) legPriceFromCache(
	ctx context.Context,
	chain TriangulationChain,
	leg canonical.Pair,
	window time.Duration,
) (*big.Rat, string) {
	if o.frozenLeg(leg, window) {
		return nil, outcomeFrozenLeg
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
	// R-1 belt-and-suspenders: a leg VWAP that parses to a non-positive
	// price (e.g. a legacy "0.000000000000" written before the
	// magnitude-relative render, or any future zero-reparsing string)
	// must NOT enter the edge graph — aggregate.BuildEdges rejects a
	// Sign()<=0 quote by nilling the ENTIRE window, turning one
	// micro-valued pair into a window-wide triangulation outage. Treat it
	// as an absent leg instead: the router either reroutes around it or
	// reports this one target unreachable (missing_leg), leaving every
	// other target in the window priced.
	if price.Sign() <= 0 {
		o.logger.Warn("triangulation: leg VWAP parsed non-positive — treating leg as absent",
			"chain", chain.Target.String(),
			"leg", leg.String(),
			"raw", raw)
		return nil, "missing_leg"
	}
	return price, ""
}

// inheritLegFreeze applies the direct-pair freeze semantics to a
// triangulated TARGET whose chain could not publish because a leg was
// frozen this tick (MNY-22).
//
// It mirrors [Orchestrator.engageFreeze] deliberately, because the
// consumer-visible situation is the same one: we are declining to
// publish a new value and continuing to serve the prior one, so the
// prior one's TTL must be kept alive and the pair must carry
// flags.frozen=true. Without the marker the target would serve a stale
// derived price with no indication that anything is wrong — worse than
// the frozen leg it descends from, which at least tells the truth.
//
// It deliberately does NOT enter the ADR-0019 freeze lifecycle (no
// hold, no extension ladder, flat [cachekeys.FreezeTTL] marker). This
// refusal is not a judgement about the target pair's own price — it
// is inherited, per tick, from whichever leg is frozen, and the LEG's
// lifecycle already owns the hold. Giving the derived pair a second,
// independent 30-minute ladder would keep a target frozen long after
// its leg was released, and would page twice for one event.
//
// Best-effort throughout: the target's LKG is read from cache to stamp
// the freeze_events row, and both the read and the marker write log
// rather than propagate — a failure here must not cost the rest of the
// triangulation pass.
func (o *Orchestrator) inheritLegFreeze(
	ctx context.Context,
	chain TriangulationChain,
	window time.Duration,
	leg canonical.Pair,
) {
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
	// does (F-1345).
	o.keepFrozenVWAPAlive(ctx, chain.Target, window, cachekeys.FreezeTTL)

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
