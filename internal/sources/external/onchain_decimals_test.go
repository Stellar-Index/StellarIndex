package external_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// TestOnChainSourcesDeclareSevenDecimals pins the amount scale for every
// on-chain (SubclassDEX) source.
//
// Stellar classic amounts are 7-decimal stroops. Metadata.AmountScaleDecimals()
// falls back to 8 when AmountDecimals is unset, so a DEX entry that omits it
// silently reports the CEX scale — and every consumer that divides by 10^dec
// then UNDERSTATES on-chain volume by 10x. That is not cosmetic: the value flows
// into approxUSDVolume (orchestrator/confidence.go), which drives the confidence
// liquidity factor, which is zero below a $1,000 bucket floor. A 10x
// understatement therefore pushes genuinely-liquid on-chain buckets under that
// floor, zeroing their confidence — and a zeroed confidence collapses the
// Phase-2 3-signal freeze AND down to the z-score alone.
//
// framework.go's own comment on AmountDecimals warns "Read this instead of
// assuming 1e8 (CS-040)" — this test makes that warning enforceable for the
// sources where it matters, including any DEX source added later.
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
