// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

//go:build t688evidence

package chops

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A -write run never executes its own CAGG follow-up: restampCAGGFollowUp output is only printed, and no restamp store
// has an exec or RefreshContinuousAggregate capability, only Plan/Apply; making it an executor is a product decision, so
// print-only stays.
type evidenceStore struct{}

func (evidenceStore) PlanXLMBaseUSDVolumeRestamp(_ context.Context, _ timescale.XLMBaseRestampParams) (*timescale.XLMBaseRestampPlan, error) {
	return &timescale.XLMBaseRestampPlan{Stats: timescale.NewXLMBaseRestampStats()}, nil
}

func (evidenceStore) ApplyXLMBaseUSDVolumeRestamp(_ context.Context, _ *timescale.XLMBaseRestampPlan, _ int64, _ int) (int64, error) {
	return 0, nil
}

func TestWriteRunNeverExecutesItsOwnCAGGFollowUp(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout = w
	os.Stderr = w
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from
	runErr := runXLMBaseRestamp(context.Background(), evidenceStore{}, "/etc/stellarindex.toml", from, to, xlmBaseRestampOptions{
		Slice: 24 * time.Hour, Batch: 2000, Write: true, SampleSize: 3, MaxGeneration: -1,
	})
	os.Stdout, os.Stderr = origOut, origErr
	_ = w.Close()
	out := <-done
	_ = r.Close()

	if runErr != nil {
		t.Fatalf("runXLMBaseRestamp: %v", runErr)
	}
	// The operator is handed the CALL text...
	if !bytes.Contains([]byte(out), []byte("CALL refresh_continuous_aggregate('prices_1m'")) {
		t.Fatalf("expected the printed CAGG follow-up in a -write run's output, got:\n%s", out)
	}
	// ...so a -write run that changed `trades` leaves every served surface stale until a human
	// pastes that SQL in by hand: xlmBaseRestampStore, the only store seam this run type is
	// given, has no method that could send it.
}
