package timescale

import "testing"

// TestBandGenesis_GapDetectorTarget pins the fourth copy of Band's
// genesis ledger. See internal/api/v1/band_genesis_test.go for why the
// number is 50,842,736 and why an event-table probe cannot find it.
//
// Four constants carried this value and two of them disagreed by 9.16M
// ledgers (#361/#363). The commit that corrected them claimed one test
// pinned all four; a review proved it pinned one. This is one of the
// three that were unguarded.
func TestBandGenesis_GapDetectorTarget(t *testing.T) {
	const bandGenesisLedger = 50_842_736

	for _, tg := range DefaultGapDetectorTargets {
		if tg.Source != "band" {
			continue
		}
		if tg.Genesis != bandGenesisLedger {
			t.Errorf("gap-detector band target Genesis = %d, want %d — "+
				"a shortened range lets the source read clean over a window that "+
				"excludes most of its history", tg.Genesis, bandGenesisLedger)
		}
		return
	}
	t.Fatal("band is not among DefaultGapDetectorTargets")
}
