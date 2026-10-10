package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLabelOracleSACs pins that a SAC id in an oracle block becomes the
// classic asset it re-derives to, while a contract the derivation check
// rejects and non-contract tokens are left verbatim.
func TestLabelOracleSACs(t *testing.T) {
	const xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const other = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	s := &Server{Options: Options{Explorer: &sacStubReader{name: "native", found: true}}}
	blk := &timescale.BespokeBlock{
		Category: "oracle",
		Breakdowns: []timescale.BespokeBreakdown{{Rows: []timescale.BespokeBreakdownRow{
			{Label: xlmSAC + " / fiat:USD"},
			{Label: "raw:FOO / fiat:USD"},
		}}},
		Tables: []timescale.BespokeTable{{Rows: [][]string{{xlmSAC, "fiat:USD", "12"}, {other, "fiat:USD", "3"}}}},
	}
	s.labelOracleSACs(context.Background(), blk)

	rows := blk.Breakdowns[0].Rows
	if rows[0].Label != "native / fiat:USD" || rows[1].Label != "raw:FOO / fiat:USD" {
		t.Fatalf("breakdown labels = %q, %q", rows[0].Label, rows[1].Label)
	}
	tbl := blk.Tables[0].Rows
	if tbl[0][0] != "native" || tbl[1][0] != other || tbl[0][2] != "12" {
		t.Fatalf("table rows = %v", tbl)
	}

	dex := &timescale.BespokeBlock{Category: "dex", Tables: []timescale.BespokeTable{{Rows: [][]string{{xlmSAC}}}}}
	s.labelOracleSACs(context.Background(), dex)
	if dex.Tables[0].Rows[0][0] != xlmSAC {
		t.Fatal("non-oracle block was relabelled")
	}
}
