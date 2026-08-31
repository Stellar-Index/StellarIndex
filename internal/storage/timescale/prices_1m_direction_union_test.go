package timescale

import (
	"strings"
	"testing"
)

// The both-directions readers of prices_1m must express the two stored
// market orientations as a UNION ALL of two single-direction branches —
// never as one `(A AND B) OR (B AND A)` disjunction.
//
// Measured on r1 (2026-08-31), prices_1m at 34 chunks, reading the pair
// native/fiat:USD which has ZERO rows:
//
//	OR form        10682.994 ms
//	UNION ALL form     3.610 ms
//
// A populated pair is unaffected (0.852 ms vs 1.279 ms), which is what
// made this invisible: every pair anyone spot-checks by hand HAS rows.
//
// Why the disjunction is so much worse. prices_1m carries
// `prices_1m_pair_bucket_idx` on (base_asset, quote_asset, bucket DESC)
// — an exact cover for ONE direction. Postgres cannot drive a single
// index scan from an OR of two different (base, quote) equality pairs,
// so with `ORDER BY bucket DESC LIMIT n` it switches to the plain
// `bucket DESC` index and applies the pair test as a post-index FILTER.
// When the pair matches nothing, "no rows" can only be proven by
// walking every chunk to exhaustion — the LIMIT never fills, so it
// never short-circuits. Splitting the directions lets each branch use
// the pair index, where an empty pair is an immediate miss.
//
// This is a REGRESSION, not an original defect: both queries read a
// single direction until 2026-08-31, when wave-D UNAUTH-DOS-9 folded in
// the second orientation to fix a genuine correctness bug (a bucket
// holding only the flipped leg went missing). The correctness fix is
// right and must stay; only its shape was wrong. Hence a shape guard
// rather than a revert — it pins BOTH properties at once, so neither
// can be traded away for the other again.
//
// Guarding the template text rather than a plan is deliberate: it needs
// no database, so it runs in the normal unit suite where a planner
// regression would otherwise only surface as production latency.

// bothDirectionUnionQueries are the prices_1m readers that fold the two
// stored orientations and are driven by ORDER BY bucket DESC + LIMIT —
// the shape where the OR plan degrades to a full multi-chunk walk.
var bothDirectionUnionQueries = map[string]string{
	"closedVWAP1mAtOrBeforeQuery":     closedVWAP1mAtOrBeforeQuery,
	"recentClosedVWAP1mForPairQuery":  recentClosedVWAP1mForPairQuery,
	"recentClosedVWAP1mCombinedQuery": recentClosedVWAP1mCombinedTemplate,
}

func TestBothDirectionReadersUseUnionNotOr(t *testing.T) {
	for name, q := range bothDirectionUnionQueries {
		t.Run(name, func(t *testing.T) {
			// The disjunction is the defect. Its absence is the guard.
			if strings.Contains(q, "OR (base_asset") {
				t.Errorf("%s folds both directions with an OR disjunction. The "+
					"planner cannot drive prices_1m_pair_bucket_idx from an OR of two "+
					"(base,quote) pairs, so ORDER BY bucket DESC LIMIT n degrades to a "+
					"full walk of every chunk — 10683 ms vs 3.6 ms measured on r1 for a "+
					"pair with no rows. Use a UNION ALL of two single-direction "+
					"branches.\n\nquery:\n%s", name, q)
			}
			if !strings.Contains(q, "UNION ALL") {
				t.Errorf("%s does not use UNION ALL — the two stored orientations "+
					"must each be their own indexable branch", name)
			}
			// Correctness half: BOTH orientations must still be read. A
			// UNION ALL that dropped a leg would be fast and wrong, which is
			// exactly the trade this guard exists to prevent.
			if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") {
				t.Errorf("%s lost the forward direction", name)
			}
			if !strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
				t.Errorf("%s lost the flipped direction (wave-D UNAUTH-DOS-9)", name)
			}
			// Each branch must carry its own ORDER BY + LIMIT. Without them
			// the branches materialise in full before the outer sort and the
			// walk comes straight back.
			// The outer sort needs a tiebreaker. `bucket DESC` alone is not
			// a total order once a bucket holds BOTH directions, and the two
			// branch outputs interleave differently than the OR plan's did —
			// verified on r1: same 20 rows, different intra-bucket order.
			// [combineDirVWAP] is commutative so the served number is
			// unaffected, but ADR-0015's byte-identical cross-region property
			// should not rest on that; `base_asset` makes the order total.
			if !strings.Contains(q, "ORDER BY bucket DESC, base_asset") {
				t.Errorf("%s outer sort lacks the base_asset tiebreaker — with both "+
					"directions in one bucket the row order would be planner-defined, "+
					"which ADR-0015 byte-identical cross-region serving cannot rely on",
					name)
			}
			if strings.Count(q, "ORDER BY bucket DESC") < 3 {
				t.Errorf("%s: expected an ORDER BY bucket DESC in each branch AND on "+
					"the outer query (3 total), found %d — an unbounded branch "+
					"re-introduces the full scan this guard prevents",
					name, strings.Count(q, "ORDER BY bucket DESC"))
			}
		})
	}
}

// The sargable-bound rule from TestClosedVWAPAtOrBeforeQueryShape applies
// to every one of these readers, not just the one that had its own test.
// Measurement note: on its own the bound is NOT what made the pathological
// read slow (rewriting it alone changed 10841 ms to 13946 ms — i.e.
// nothing). It is still the correct form, and pinning it here stops the
// weaker shape spreading by copy-paste.
func TestBothDirectionReadersKeepSargableBucketBound(t *testing.T) {
	for name, q := range bothDirectionUnionQueries {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(q, "bucket + INTERVAL") {
				t.Errorf("%s applies a function to the indexed bucket column "+
					"(`bucket + INTERVAL … <= x`). Put the interval on the RHS: "+
					"`bucket <= x - INTERVAL …`", name)
			}
		})
	}
}
