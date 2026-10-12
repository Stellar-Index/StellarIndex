package supply

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestReportCrossCheck: a partial-wrap result whose escrow leg never ran has divergence 0 by construction and must not print as a pass (ADR-0011: read SubsetBoundChecked beside WithinTolerance).
func TestReportCrossCheck(t *testing.T) {
	t.Parallel()

	snap := func(key string, total int64, wrapped *big.Int) supply.Supply {
		return supply.Supply{AssetKey: key, TotalSupply: big.NewInt(total), SACWrappedStroops: wrapped}
	}

	for name, tc := range map[string]struct {
		classic, sac supply.Supply
		class        supply.WrapClass
		wantStatus   string
		wantFail     bool
		wantIs       error
		wantLegs     []string
	}{
		"partial, escrow leg unevaluated": {
			classic:    snap("classic", 1_000, nil),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "UNCHECKED",
			wantFail:   true,
			wantIs:     errCrossCheckUnchecked,
			wantLegs: []string{
				"subset_bound_checked: false",
				"over_mint_stroops:    0",
				"escrow_excess_stroops: n/a (not evaluated)",
			},
		},
		"partial, escrow leg passes": {
			classic:    snap("classic", 1_000, big.NewInt(400)),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "WITHIN TOLERANCE",
			wantLegs: []string{
				"subset_bound_checked: true",
				"over_mint_stroops:    0",
				"escrow_excess_stroops: 0",
			},
		},
		"partial, escrow leg breached": {
			classic:    snap("classic", 1_000, big.NewInt(500)),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "OVER TOLERANCE",
			wantFail:   true,
			wantLegs: []string{
				"subset_bound_checked: true",
				"escrow_excess_stroops: 100",
			},
		},
		"partial, SAC over-mints classic": {
			classic:    snap("classic", 1_000, big.NewInt(1_200)),
			sac:        snap("sac", 1_250, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "WITHIN TOLERANCE",
			wantLegs: []string{
				"subset_bound_checked: true",
				"over_mint_stroops:    250",
				"escrow_excess_stroops: 0",
			},
		},
		"full wrap agrees without an escrow component": {
			classic:    snap("classic", 400, nil),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassFull,
			wantStatus: "WITHIN TOLERANCE",
			wantLegs: []string{
				"subset_bound_checked: false",
				"over_mint_stroops:    n/a (not evaluated)",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := supply.CrossCheckForClass(tc.classic, tc.sac, tc.class)
			if err != nil {
				t.Fatalf("CrossCheckForClass: %v", err)
			}
			var out bytes.Buffer
			err = reportCrossCheck(&out, result, "classic")

			status := statusLine(out.String())
			if !strings.Contains(status, tc.wantStatus) {
				t.Errorf("status line = %q, want it to contain %q", status, tc.wantStatus)
			}
			if (err != nil) != tc.wantFail {
				t.Errorf("err = %v, want failure = %v", err, tc.wantFail)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want %v", err, tc.wantIs)
			}
			for _, leg := range tc.wantLegs {
				if !strings.Contains(out.String(), "  "+leg+"\n") {
					t.Errorf("output lacks line %q:\n%s", leg, out.String())
				}
			}
		})
	}
}

// TestCrossCheckAndReport_MisalignedPairHasNoVerdict: a pair the aggregator refuses as misaligned gets no CLI verdict either.
func TestCrossCheckAndReport_MisalignedPairHasNoVerdict(t *testing.T) {
	t.Parallel()
	classic := supply.Supply{
		AssetKey: "classic", TotalSupply: big.NewInt(1_000),
		SACWrappedStroops: big.NewInt(400), LedgerSequence: 50_000_000,
	}
	sac := supply.Supply{AssetKey: "sac", TotalSupply: big.NewInt(400), LedgerSequence: 50_050_000}

	for _, class := range []supply.WrapClass{supply.WrapClassPartial, supply.WrapClassFull} {
		var out bytes.Buffer
		err := crossCheckAndReport(&out, classic, sac, class, "classic")
		if !errors.Is(err, supply.ErrCrossCheckMisaligned) {
			t.Errorf("%s: err = %v, want ErrCrossCheckMisaligned", class, err)
		}
		if status := statusLine(out.String()); !strings.Contains(status, "MISALIGNED") {
			t.Errorf("%s: status line = %q, want MISALIGNED", class, status)
		}
	}

	aligned := sac
	aligned.LedgerSequence = classic.LedgerSequence + supply.CrossCheckLedgerTolerance
	var out bytes.Buffer
	if err := crossCheckAndReport(&out, classic, aligned, supply.WrapClassPartial, "classic"); err != nil {
		t.Fatalf("aligned pair: err = %v, want pass", err)
	}
	if status := statusLine(out.String()); !strings.Contains(status, "WITHIN TOLERANCE") {
		t.Errorf("aligned pair: status line = %q, want WITHIN TOLERANCE", status)
	}
}

func statusLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "status:") {
			return line
		}
	}
	return ""
}

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
