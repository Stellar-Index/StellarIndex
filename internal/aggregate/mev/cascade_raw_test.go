package mev

import "testing"

// TestDetectLiquidationCascades_RawOracleRowsAreNotEvidence pins the
// oracle capture-totality consumer rule for the one unkeyed
// oracle_updates reader: `raw:<symbol>` rows (unmapped oracle symbols
// recorded verbatim — canonical.AssetOracleRaw) must never correlate
// a cascade. 12 raw rows and one mapped row sit inside the bracket;
// the evidence must contain only the mapped row, and with raw rows
// alone there is no candidate at all.
func TestDetectLiquidationCascades_RawOracleRowsAreNotEvidence(t *testing.T) {
	fills := []AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLB", "GUSER2", "GFILL2", txB, 105),
	}
	var raws []OracleRef
	for i := 0; i < 12; i++ {
		raws = append(raws, OracleRef{
			Source: "reflector-cex", Asset: "raw:NOTACOIN", Quote: "fiat:USD",
			Ledger: 101 + uint32(i%4), TxHash: txO, OpIndex: uint32(i),
		})
	}

	// Raw rows alone: no evidence, no candidate.
	if got := DetectLiquidationCascades(fills, raws); len(got) != 0 {
		t.Fatalf("raw-only oracle rows produced %d cascade candidate(s): %+v", len(got), got)
	}

	// 12 raw + 1 mapped: exactly the mapped row is the evidence.
	mapped := OracleRef{Source: "reflector-dex", Asset: usdc, Quote: "fiat:USD", Ledger: 102, TxHash: txO}
	got := DetectLiquidationCascades(fills, append(raws, mapped))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	d, ok := got[0].Detail.(cascadeDetail)
	if !ok {
		t.Fatalf("detail = %T", got[0].Detail)
	}
	if len(d.OracleUpdates) != 1 {
		t.Fatalf("evidence has %d oracle rows, want only the mapped row: %+v", len(d.OracleUpdates), d.OracleUpdates)
	}
	if d.OracleUpdates[0].Asset != usdc {
		t.Errorf("evidence asset = %q, want %q", d.OracleUpdates[0].Asset, usdc)
	}
}
