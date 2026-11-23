package v1

import (
	"context"
	"maps"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// RWAPremiumReferenceSourceExcluded — an operator source exclusion covers the oracle or fund-NAV reading the row would be
// valued at, so neither the reference, its valuation nor a premium is served; the envelope carries fills_withheld_reason.
const RWAPremiumReferenceSourceExcluded = RWAPremiumExcludedSourceExcluded

// rwaUnansweredExclusions names the source-exclusion read in the outage note when it did not answer.
const rwaUnansweredExclusions = "the source-exclusion read"

// rwaWithheldRef is a snapshot reference the exclusion set withholds, kept so a barred NAV still falls through as before.
type rwaWithheldRef struct {
	w      pricingguard.Withholding
	source string
}

// rwaRefVerdict is one snapshot reference's exclusion verdict as a build judged it; a served view is re-judged against it.
type rwaRefVerdict struct {
	asset  canonical.Asset
	source string
	asOf   time.Time
	w      pricingguard.Withholding
}

// rwaJudgedReferences is the only way a build reads the reference snapshot: every reference an exclusion covers, or that an
// unread set cannot clear, moves out of byFeed and fundNAV into the withheld maps. The cached snapshot stays unjudged.
func (s *Server) rwaJudgedReferences(ctx context.Context, snap rwaReferences) (rwaReferences, []rwaRefVerdict) {
	set, judge, unread := s.exclusionSnapshot(ctx)
	verdicts := make([]rwaRefVerdict, 0, len(snap.byFeed)+len(snap.fundNAV))
	judgeAll := func(refs map[string]rwaReference) (map[string]rwaReference, map[string]rwaWithheldRef) {
		kept, withheld := refs, map[string]rwaWithheldRef(nil)
		for key, ref := range refs {
			w := rwaRefVerdictOf(ctx, set, judge, unread, ref.asset, ref.source, ref.asOf)
			verdicts = append(verdicts, rwaRefVerdict{asset: ref.asset, source: ref.source, asOf: ref.asOf, w: w})
			if w == pricingguard.NotWithheld {
				continue
			}
			if withheld == nil {
				kept, withheld = maps.Clone(refs), map[string]rwaWithheldRef{}
			}
			delete(kept, key)
			withheld[key] = rwaWithheldRef{w: w, source: ref.source}
		}
		return kept, withheld
	}
	snap.byFeed, snap.withheldFeed = judgeAll(snap.byFeed)
	snap.fundNAV, snap.withheldNAV = judgeAll(snap.fundNAV)
	return snap, verdicts
}

// rwaRefVerdictOf judges one oracle reading at its own instant under asset/USD, as the global-market reference is judged.
func rwaRefVerdictOf(
	ctx context.Context, set pricingguard.ExclusionSet, judge bool, unread pricingguard.Withholding,
	asset canonical.Asset, source string, asOf time.Time,
) pricingguard.Withholding {
	switch {
	case unread != pricingguard.NotWithheld:
		return unread
	case !judge:
		return pricingguard.NotWithheld
	}
	return set.Withholding(ctx, asset, defaultPriceQuote, []string{source}, asOf, asOf.Add(time.Nanosecond))
}

// rwaRefVerdictsHold reports whether every reference a build judged would be judged the same now. The view caches its judged
// rows, so a changed exclusion set must not be answered from it.
func (s *Server) rwaRefVerdictsHold(ctx context.Context, verdicts []rwaRefVerdict) bool {
	if len(verdicts) == 0 {
		return true
	}
	set, judge, unread := s.exclusionSnapshot(ctx)
	for _, v := range verdicts {
		if rwaRefVerdictOf(ctx, set, judge, unread, v.asset, v.source, v.asOf) != v.w {
			return false
		}
	}
	return true
}

// rwaRefuseWithheldReference refuses a row whose reference the exclusion set withholds, reporting whether it did. An unread
// set is an outage, never a finding that the row has no reference.
func rwaRefuseWithheldReference(a *RWAAsset, w pricingguard.Withholding) bool {
	switch w {
	case pricingguard.NotWithheld:
		return false
	case pricingguard.WithheldSourceExcluded:
		rwaRefuseReference(a, RWAPremiumReferenceSourceExcluded)
	default:
		rwaRefuseReference(a, RWAPremiumReferenceUnavailable)
		a.ReferenceValuation.unansweredBy = rwaUnansweredExclusions
	}
	return true
}

// rwaReferencesExcluded reports a row whose reference an exclusion withheld, which the envelope must say.
func rwaReferencesExcluded(assets []RWAAsset) bool {
	for _, a := range assets {
		if a.ReferenceValuation.Status == RWAPremiumReferenceSourceExcluded {
			return true
		}
	}
	return false
}
