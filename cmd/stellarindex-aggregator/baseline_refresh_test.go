// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type hungVWAPSource struct{}

func (hungVWAPSource) TimedVWAPsForPair1m(ctx context.Context, _ canonical.Pair, _, _ time.Time) ([]baseline.TimedVWAP, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type emptyVWAPSource struct{}

func (emptyVWAPSource) TimedVWAPsForPair1m(context.Context, canonical.Pair, time.Time, time.Time) ([]baseline.TimedVWAP, error) {
	return nil, nil
}

type discardBaselineSink struct{}

func (discardBaselineSink) UpsertBaseline(context.Context, canonical.Pair, time.Time, time.Time, time.Time, baseline.MultiBaseline) error {
	return nil
}

func baselineTestPairs(t *testing.T) []canonical.Pair {
	t.Helper()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	p, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return []canonical.Pair{p}
}

// TestRefreshBaselinesOnce_HungReadIsBounded: a read that never returns must end the pass at its timeout and leave the last-success
// gauge where it was, so the stale-baseline alert can fire instead of the loop stalling silently.
func TestRefreshBaselinesOnce_HungReadIsBounded(t *testing.T) {
	r := baseline.NewRefresher(hungVWAPSource{}, discardBaselineSink{}, 0, slog.Default())
	obs.AggregatorBaselineRefreshLastSuccessUnix.Set(1)

	pairs := baselineTestPairs(t)
	done := make(chan baseline.RefreshSummary, 1)
	go func() {
		done <- refreshBaselinesOnce(context.Background(), r, pairs, 50*time.Millisecond, slog.Default())
	}()
	select {
	case sum := <-done:
		if sum.ReadErrors != 1 {
			t.Errorf("ReadErrors = %d, want 1", sum.ReadErrors)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh pass did not return after its timeout")
	}
	if got := testutil.ToFloat64(obs.AggregatorBaselineRefreshLastSuccessUnix); got != 1 {
		t.Errorf("last-success gauge = %v after a timed-out pass, want unchanged 1", got)
	}
}

// TestRefreshBaselinesOnce_AnsweredPassAdvancesGauge: a pass the store answered counts as alive even when every pair is in bootstrap.
func TestRefreshBaselinesOnce_AnsweredPassAdvancesGauge(t *testing.T) {
	r := baseline.NewRefresher(emptyVWAPSource{}, discardBaselineSink{}, 0, slog.Default())
	obs.AggregatorBaselineRefreshLastSuccessUnix.Set(1)
	before := time.Now().Unix()
	sum := refreshBaselinesOnce(context.Background(), r, baselineTestPairs(t), time.Minute, slog.Default())
	if sum.NotEnoughSamples != 1 {
		t.Fatalf("NotEnoughSamples = %d, want 1", sum.NotEnoughSamples)
	}
	if got := testutil.ToFloat64(obs.AggregatorBaselineRefreshLastSuccessUnix); got < float64(before) {
		t.Errorf("last-success gauge = %v, want >= %d", got, before)
	}
}
