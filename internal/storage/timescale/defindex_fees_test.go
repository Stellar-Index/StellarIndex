package timescale

import (
	"context"
	"testing"
	"time"
)

// The defensive pre-DB guards must reject an incomplete dfees row
// BEFORE touching the pool (a zero-value Store has a nil db, so any
// case that slips past a guard panics — making these non-vacuous).
// The happy-path insert/upsert is integration-tested against real
// Postgres (test/integration), like the sibling protocol inserts.
func TestInsertDefindexFee_guards(t *testing.T) {
	valid := DefindexFee{
		Ledger:          60_903_337,
		LedgerCloseTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "dfeestx",
		ContractID:      "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		Token:           "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Amount:          "7686",
	}
	cases := []struct {
		name   string
		mutate func(*DefindexFee)
	}{
		{"empty TxHash", func(e *DefindexFee) { e.TxHash = "" }},
		{"empty ContractID", func(e *DefindexFee) { e.ContractID = "" }},
		{"empty Token", func(e *DefindexFee) { e.Token = "" }},
		{"empty Amount", func(e *DefindexFee) { e.Amount = "" }},
		{"zero LedgerCloseTime", func(e *DefindexFee) { e.LedgerCloseTime = time.Time{} }},
	}
	s := &Store{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := valid
			tc.mutate(&row)
			if err := s.InsertDefindexFee(context.Background(), row); err == nil {
				t.Error("InsertDefindexFee accepted an invalid row, want a guard error")
			}
		})
	}
}
