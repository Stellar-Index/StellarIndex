package external_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// TestOnChainSourcesDeclareSevenDecimals pins the amount scale for every on-chain
// (SubclassDEX) source.
//
// Stellar classic amounts are 7-decimal stroops, but Metadata.AmountScaleDecimals() falls back
// to 8 when AmountDecimals is unset, so a DEX entry that omits it silently reports the CEX scale
// and understates on-chain volume 10x. That flows into approxUSDVolume
// (orchestrator/confidence.go), whose liquidity factor is zero below a $1,000 bucket floor, so
// genuinely liquid buckets lose their confidence and the 3-signal freeze AND collapses to the
// z-score alone. This makes framework.go's "Read this instead of assuming 1e8" warning
// enforceable for every DEX source, including ones added later.
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
