package ingest

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// TestCapCheckpointAtLoss pins the resume-cursor cap: backfill used to
// checkpoint lastFullyEnqueued even when a sink dropped or abandoned rows
// (a SIGINT while the soroban-events sink applied back-pressure, or a
// served-tier drain that timed out), so -resume restarted past them and
// they never landed.
func TestCapCheckpointAtLoss(t *testing.T) {
	const startFrom, enqueued = 1_000, 1_500
	cases := []struct {
		name        string
		loss        pipeline.ShutdownLoss
		rawMin      uint32
		rawUnlanded bool
		want        uint32
		wantErr     bool
	}{
		{name: "nothing lost", want: enqueued},
		{name: "soroban-events dropped the tail of ledger 1500", rawMin: 1_500, rawUnlanded: true, want: 1_499, wantErr: true},
		{name: "abandoned trades from ledger 1200", loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1_200}, want: 1_199, wantErr: true},
		{name: "lowest of both sinks wins", loss: pipeline.ShutdownLoss{Rows: 1, MinLedger: 1_400}, rawMin: 1_300, rawUnlanded: true, want: 1_299, wantErr: true},
		{name: "loss above the enqueued watermark does not raise it", rawMin: 1_600, rawUnlanded: true, want: enqueued, wantErr: true},
		{name: "ledger-less abandoned event records nothing new", loss: pipeline.ShutdownLoss{Rows: 1, LedgerUnknown: true}, want: startFrom - 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := capCheckpointAtLoss(enqueued, startFrom, tc.loss, tc.rawMin, tc.rawUnlanded)
			if got != tc.want {
				t.Errorf("checkpoint = %d; want %d", got, tc.want)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v; wantErr %t", err, tc.wantErr)
			}
		})
	}
}
