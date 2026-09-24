package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCCTPRozoGenesisLocksStepAcrossRegistries pins cctp's and rozo's
// genesis ledger across the three places it is restated: the source
// package's exported constant, protocols_registry.go's ProtocolMeta,
// and per_source_gaps.go's DefaultGapDetectorTargets floor for that
// source. Without this, correcting one copy (as happened historically
// with cctp's stale 62_403_000 ingestion-config floor) leaves the
// others silently wrong and nothing fails (GH-898).
func TestCCTPRozoGenesisLocksStepAcrossRegistries(t *testing.T) {
	cases := []struct {
		source  string
		wantGen uint32
	}{
		{source: "cctp", wantGen: cctp.GenesisLedger},
		{source: "rozo", wantGen: rozo.GenesisLedger},
	}

	for _, tc := range cases {
		meta, ok := protocolByName(tc.source)
		if !ok {
			t.Fatalf("%s: not found in protocolRegistry", tc.source)
		}
		if meta.GenesisLedger != tc.wantGen {
			t.Errorf("%s: protocols_registry.go GenesisLedger = %d, source package constant = %d",
				tc.source, meta.GenesisLedger, tc.wantGen)
		}

		floor, found := int64(-1), false
		for _, tgt := range timescale.DefaultGapDetectorTargets {
			if tgt.Source != tc.source {
				continue
			}
			if !found || tgt.Genesis < floor {
				floor = tgt.Genesis
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no DefaultGapDetectorTargets row for this source", tc.source)
		}
		if floor != int64(tc.wantGen) {
			t.Errorf("%s: per_source_gaps.go gap-detector floor = %d, source package constant = %d",
				tc.source, floor, tc.wantGen)
		}
	}
}
