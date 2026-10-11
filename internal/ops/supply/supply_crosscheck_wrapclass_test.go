package supply

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestCrossCheckWrapClass: the CLI picks the aggregator's invariant, strict ADR-0011 equality ONLY for a SAC attested fully wrapped and the subset bound otherwise;
// unconditional equality would fail every partial_wrap pair (all configured r1 pairs) on healthy data.
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
