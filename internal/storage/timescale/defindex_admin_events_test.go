package timescale

import (
	"context"
	"testing"
	"time"
)

// A zero-value Store has a nil db, so any case that slips past a guard
// panics — which keeps these cases non-vacuous.
func TestInsertDefindexAdminEvent_guards(t *testing.T) {
	valid := DefindexAdminEvent{
		Ledger:          60_903_337,
		LedgerCloseTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "admintx",
		ContractID:      "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		EventKind:       "nmanager",
		NewAddress:      "GBJB3Z4SZ2E7ZSN6RZR5VTPH5OC6XWMEVQYEBVOMDP2FDGVVW52Q2PYA",
	}
	cases := []struct {
		name   string
		mutate func(*DefindexAdminEvent)
	}{
		{"empty TxHash", func(e *DefindexAdminEvent) { e.TxHash = "" }},
		{"empty ContractID", func(e *DefindexAdminEvent) { e.ContractID = "" }},
		{"zero LedgerCloseTime", func(e *DefindexAdminEvent) { e.LedgerCloseTime = time.Time{} }},
		{"unknown EventKind", func(e *DefindexAdminEvent) { e.EventKind = "rebalance" }},
	}
	s := &Store{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := valid
			tc.mutate(&row)
			if err := s.InsertDefindexAdminEvent(context.Background(), row); err == nil {
				t.Error("InsertDefindexAdminEvent accepted an invalid row, want a guard error")
			}
		})
	}
}
