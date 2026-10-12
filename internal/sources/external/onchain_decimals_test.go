package external_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// TestOnChainSourcesDeclareSevenDecimals pins 7-decimal stroop scale for every SubclassDEX source: AmountScaleDecimals() falls
// back to 8 when AmountDecimals is unset, understating on-chain volume 10x and zeroing approxUSDVolume's liquidity factor
// below the $1,000 floor.
func TestOnChainSourcesDeclareSevenDecimals(t *testing.T) {
	const stellarStroopDecimals = 7

	checked := 0
	for name, md := range external.Registry {
		if md.Subclass != external.SubclassDEX {
			continue
		}
		checked++
		if got := md.AmountScaleDecimals(); got != stellarStroopDecimals {
			t.Errorf("source %q (SubclassDEX): AmountScaleDecimals() = %d, want %d — "+
				"on-chain amounts are 7-decimal stroops; leaving AmountDecimals unset "+
				"falls back to the 8-decimal CEX scale and understates this source's "+
				"volume by 10x everywhere it is divided by 10^dec (CS-040)",
				name, got, stellarStroopDecimals)
		}
	}
	if checked == 0 {
		t.Fatal("no SubclassDEX sources found in the registry — this test is no longer protecting anything")
	}
}
