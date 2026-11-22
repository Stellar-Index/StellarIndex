// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A recovered freeze stays readable as a window, so the served 1m series can refuse the buckets it covered.
func TestFreezeWindowsForPair_ReturnsRecoveredAndOpenFreezes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	sink := timescale.NewFreezeEventSink(store)
	xlm, _ := c.NewCryptoAsset("XLM")
	usd, _ := c.NewFiatAsset("USD")
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND"}
	before := time.Now().UTC().Add(-time.Minute)
	if err := sink.RecordFreeze(ctx, xlm, usd, "0.100000000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	if err := sink.MarkRecovered(ctx, xlm, usd, "operator:test"); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}

	got, err := store.FreezeWindowsForPair(ctx, []string{"native", "crypto:XLM"}, "fiat:USD", before)
	if err != nil {
		t.Fatalf("FreezeWindowsForPair: %v", err)
	}
	if len(got) != 1 || got[0].RecoveredAt == nil || !got[0].CoversMinute(got[0].FrozenAt) {
		t.Fatalf("FreezeWindowsForPair = %+v, want the one recovered freeze covering its own minute", got)
	}
	if later, err := store.FreezeWindowsForPair(ctx, []string{"crypto:XLM"}, "fiat:USD", got[0].RecoveredAt.Add(time.Second)); err != nil || len(later) != 0 {
		t.Fatalf("a freeze recovered before since must not be returned: %+v, %v", later, err)
	}
	if other, err := store.FreezeWindowsForPair(ctx, []string{"crypto:XLM"}, "fiat:EUR", before); err != nil || len(other) != 0 {
		t.Fatalf("another quote's freezes must not be returned: %+v, %v", other, err)
	}
}
