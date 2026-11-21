package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// The abandoned-rows log names a refetch window only when abandoned trades gave it one.
func TestAwaitExternalSink_LogsAWindowOnlyWhenThereIsOne(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		loss       pipeline.ShutdownLoss
		wantWindow bool
	}{
		"events only":      {pipeline.ShutdownLoss{Rows: 3}, false},
		"trades abandoned": {pipeline.ShutdownLoss{Rows: 3, OffChainFrom: at, OffChainTo: at.Add(time.Minute), Venues: "kraken"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			done := make(chan struct{})
			close(done)
			if !awaitExternalSink(context.Background(), done, &tc.loss, slog.New(slog.NewJSONHandler(&buf, nil))) {
				t.Fatal("awaitExternalSink = false with the lane already stopped")
			}
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log %q: %v", buf.String(), err)
			}
			_, from := rec["from"]
			_, to := rec["to"]
			_, venues := rec["venues"]
			if from != tc.wantWindow || to != tc.wantWindow || venues != tc.wantWindow || rec["rows"] != float64(3) {
				t.Errorf("log = %s; want rows=3 and window and venue fields present = %v", buf.String(), tc.wantWindow)
			}
		})
	}
}
