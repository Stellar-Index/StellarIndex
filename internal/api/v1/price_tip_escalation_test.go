// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// escalationHistoryStub returns no trades for windows narrower than
// the trade's age, simulating a pair that last traded ~20s ago — the
// quiet-second case that used to fall through to the closed bucket.
type escalationHistoryStub struct {
	HistoryReader // nil — only TradesInRange is exercised
	tradeAge      time.Duration
	calls         []time.Duration
}

func (h *escalationHistoryStub) TradesInRange(_ context.Context, pair canonical.Pair, from, to time.Time, _ int) ([]canonical.Trade, error) {
	h.calls = append(h.calls, to.Sub(from))
	if to.Sub(from) < h.tradeAge {
		return nil, nil
	}
	return []canonical.Trade{{
		Source:      "kraken",
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_0000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(200_0000)),
		Timestamp:   to.Add(-h.tradeAge),
	}}, nil
}

// TestComputeTip_EscalatesBeforeClosedBucket pins board #42: an empty
// default (5s) window widens ONCE to the 30s SLA bound and serves a
// fresh rolling VWAP instead of falling through to the closed-bucket
// store price (which live-sampled at ~90s staleness).
func TestComputeTip_EscalatesBeforeClosedBucket(t *testing.T) {
	h := &escalationHistoryStub{tradeAge: 20 * time.Second}
	s := &Server{history: h}
	pairAsset := canonical.NativeAsset()
	quote, _ := canonical.ParseAsset("fiat:USD")

	snap, sources, err := s.computeTip(context.Background(), pairAsset, quote, 5)
	if err != nil {
		t.Fatalf("computeTip: %v", err)
	}
	if snap.PriceType != "vwap" || snap.WindowSeconds != tipEscalationWindowSeconds {
		t.Fatalf("snap = type %q window %d, want escalated vwap at %d",
			snap.PriceType, snap.WindowSeconds, tipEscalationWindowSeconds)
	}
	if time.Since(snap.ObservedAt) > 5*time.Second {
		t.Fatalf("observed_at = %s — escalated tip must be NOW-anchored", snap.ObservedAt)
	}
	if len(sources) != 1 || sources[0] != "kraken" {
		t.Fatalf("sources = %v", sources)
	}
	// Windows tried: caller's (5s per alias combination), then 30s.
	sawEscalation := false
	for _, w := range h.calls {
		if w == tipEscalationWindowSeconds*time.Second {
			sawEscalation = true
		}
	}
	if !sawEscalation {
		t.Fatalf("no 30s escalation query issued; windows tried: %v", h.calls)
	}
}
