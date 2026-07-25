package timescale

import (
	"testing"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestReDeriveNullVolumeGuard_ACRIT1 locks in the fail-closed guard for the
// audit-2026-07-24 critical: a re-derive entry point that stamps a positive
// derive_generation but never calls InstallUSDVolumeResolution would compute
// usd_volume=NULL and, because the high generation wins the ON CONFLICT guard,
// silently overwrite correct stored values — unrecoverably (a live gen-0 replay
// can never beat it). projected-rebuild and the main on-chain backfill both
// shipped exactly that combination.
//
// The guard is deliberately PRECISE, and these cases pin that precision: it must
// fire only on the destructive combination (about to write NULL, resolution never
// wired), NOT on every re-derive write. A blanket guard would refuse tier-1
// CEX-USD trades, which price correctly from quote decimals with no resolver at
// all — that over-broad first cut broke the INV-3 integration test, which is why
// the "computed != nil passes" case below exists.
func TestReDeriveNullVolumeGuard_ACRIT1(t *testing.T) {
	priced := "12.34000000"
	tr := c.Trade{Source: "binance", Ledger: 7}

	cases := []struct {
		name      string
		gen       int64
		installed bool
		computed  *string
		wantErr   bool
	}{
		// Live ingest is never guarded, however the volume resolved.
		{"live ingest, null volume", 0, false, nil, false},
		{"live ingest, priced", 0, false, &priced, false},

		// THE destructive combination — re-derive, no resolution wired, and this
		// write would land a NULL over a possibly-correct stored value.
		{"re-derive, not installed, null volume", 1, false, nil, true},

		// Precision cases: neither of these can corrupt, so neither may be refused.
		// A priced trade (tier 1 CEX-USD needs no resolver) writes a real value.
		{"re-derive, not installed, but priced", 1, false, &priced, false},
		// Properly-wired re-derive: a genuinely unpriceable trade writes an
		// honest NULL (e.g. a no-pegs deployment, where Install is a no-op).
		{"re-derive, installed, null volume", 1, true, nil, false},
		{"re-derive, installed, priced", 1, true, &priced, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{}
			s.SetDeriveGeneration(tc.gen)
			s.usdVolumeResolutionInstalled = tc.installed
			err := s.reDeriveNullVolumeGuard(tr, tc.computed)
			if tc.wantErr && err == nil {
				t.Fatal("want the write refused (A-CRIT-1 destructive combination), got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want the write allowed, got refusal: %v", err)
			}
		})
	}
}
