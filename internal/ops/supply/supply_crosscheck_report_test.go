package supply

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// A partial-wrap result whose escrow leg never ran has divergence 0 by
// construction; the CLI must not print that as a pass or exit 0 on it
// (ADR-0011: read SubsetBoundChecked alongside WithinTolerance).
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
	}{
		"partial, escrow leg unevaluated": {
			classic:    snap("classic", 1_000, nil),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "UNCHECKED",
			wantFail:   true,
			wantIs:     errCrossCheckUnchecked,
		},
		"partial, escrow leg passes": {
			classic:    snap("classic", 1_000, big.NewInt(400)),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "WITHIN TOLERANCE",
		},
		"partial, escrow leg breached": {
			classic:    snap("classic", 1_000, big.NewInt(500)),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassPartial,
			wantStatus: "OVER TOLERANCE",
			wantFail:   true,
		},
		"full wrap agrees without an escrow component": {
			classic:    snap("classic", 400, nil),
			sac:        snap("sac", 400, nil),
			class:      supply.WrapClassFull,
			wantStatus: "WITHIN TOLERANCE",
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
		})
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
