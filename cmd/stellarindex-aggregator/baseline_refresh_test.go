// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
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

// partialVWAPSource answers only `good` and fails every other pair's read.
type partialVWAPSource struct{ good canonical.Pair }

func (s partialVWAPSource) TimedVWAPsForPair1m(_ context.Context, p canonical.Pair, _, _ time.Time) ([]baseline.TimedVWAP, error) {
	if p == s.good {
		return nil, nil
	}
	return nil, errors.New("prices_1m read failed")
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

// TestRefreshBaselinesOnce_ReadErrorsHoldGauge: one answered pair must not mask a pass in which every other read failed.
func TestRefreshBaselinesOnce_ReadErrorsHoldGauge(t *testing.T) {
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pairs := make([]canonical.Pair, 0, 100)
	for i := range 100 {
		base, err := canonical.NewClassicAsset(fmt.Sprintf("T%03d", i), "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatalf("NewClassicAsset: %v", err)
		}
		p, err := canonical.NewPair(base, usd)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		pairs = append(pairs, p)
	}
	r := baseline.NewRefresher(partialVWAPSource{good: pairs[0]}, discardBaselineSink{}, 0, slog.Default())
	obs.AggregatorBaselineRefreshLastSuccessUnix.Set(1)
	sum := refreshBaselinesOnce(context.Background(), r, pairs, time.Minute, slog.Default())
	if sum.ReadErrors != 99 || sum.NotEnoughSamples != 1 {
		t.Fatalf("ReadErrors=%d NotEnoughSamples=%d, want 99 and 1", sum.ReadErrors, sum.NotEnoughSamples)
	}
	if got := testutil.ToFloat64(obs.AggregatorBaselineRefreshLastSuccessUnix); got != 1 {
		t.Errorf("last-success gauge = %v after a pass with read errors, want unchanged 1", got)
	}
}
