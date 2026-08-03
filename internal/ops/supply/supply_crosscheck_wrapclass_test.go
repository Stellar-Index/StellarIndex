package supply

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The CLI must pick the same invariant the aggregator's cross-check
// refresher picks: strict ADR-0011 equality ONLY for a SAC the operator
// has attested is fully wrapped, and the subset bound for everything
// else.
//
// Before this, `supply audit -cross-check` ran the equality compare
// unconditionally, which is a guaranteed failure for a partially
// wrapped pair — classic supply that never entered the SAC is exactly
// the gap the subset bound exists to tolerate. Every configured r1 pair
// is partial_wrap, so the runbook's own diagnostic command printed
// "OVER TOLERANCE ✗ — investigate" and exited non-zero on healthy data,
// while the runbook tells operators to chain `|| operator-escalate`
// (cold audit 2026-08-03; BACKLOG #59 landed in the refresher but never
// here).
func TestCrossCheckWrapClass(t *testing.T) {
	t.Parallel()

	const (
		wrappedSAC = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
		otherSAC   = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)

	for name, tc := range map[string]struct {
		fullyWrapped []string
		sacID        string
		want         supply.WrapClass
	}{
		"attested fully wrapped":   {[]string{wrappedSAC}, wrappedSAC, supply.WrapClassFull},
		"not attested":             {[]string{wrappedSAC}, otherSAC, supply.WrapClassPartial},
		"no attestations at all":   {nil, wrappedSAC, supply.WrapClassPartial},
		"empty list, empty sac id": {nil, "", supply.WrapClassPartial},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := crossCheckWrapClass(tc.fullyWrapped, tc.sacID); got != tc.want {
				t.Errorf("crossCheckWrapClass(%v, %q) = %q, want %q",
					tc.fullyWrapped, tc.sacID, got, tc.want)
			}
		})
	}
}
