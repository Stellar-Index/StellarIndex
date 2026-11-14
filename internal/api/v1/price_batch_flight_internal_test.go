// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ctxFreezeStub is a freeze marker read that fails on a finished context, as a Redis GET does; otherwise it reports frozen.
type ctxFreezeStub struct{ failing bool }

func (f ctxFreezeStub) FrozenForPair(ctx context.Context, _, _ canonical.Asset) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.failing {
		return false, context.DeadlineExceeded
	}
	return true, nil
}

type fixedPriceReader struct{ snap PriceSnapshot }

func (r fixedPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return r.snap, []string{"sdex"}, false, nil
}

func (fixedPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, nil
}

func batchFlightServer(freeze FrozenLooker) *Server {
	return &Server{
		Options: Options{Freeze: freeze, Prices: fixedPriceReader{snap: PriceSnapshot{Price: "9.99", PriceType: "vwap"}}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestBatchRowFlight_LeaderPastItsDeadlineStillRunsTheFreezeGate: the shared row must not inherit a leader's spent deadline.
// With it, the freeze read failed, the gate failed open, and every follower got the bucket the freeze refused.
func TestBatchRowFlight_LeaderPastItsDeadlineStillRunsTheFreezeGate(t *testing.T) {
	s := batchFlightServer(ctxFreezeStub{})
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset(USD): %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/price/batch?asset_ids=native&quote=fiat:USD", nil).WithContext(ctx)

	row := s.resolveBatchRowShared(ctx, req, "native", usd)
	if row.ok {
		t.Fatalf("served %q from a frozen pair with nothing held: the freeze gate did not run", row.snap.Price)
	}
	if !row.skip {
		t.Errorf("row = %+v, want an omitted row", row)
	}
}

// TestBatchRow_UnreadFreezeMarkerDerivesNoSingleSource: a row served with no freeze verdict says so (degraded) and does
// not assert single_source, as /v1/price does.
func TestBatchRow_UnreadFreezeMarkerDerivesNoSingleSource(t *testing.T) {
	s := batchFlightServer(ctxFreezeStub{failing: true})
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset(USD): %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/price/batch?asset_ids=native&quote=fiat:USD", nil)

	row := s.resolveBatchRowShared(req.Context(), req, "native", usd)
	if !row.ok || !row.freezeUnknown {
		t.Fatalf("row ok = %v freezeUnknown = %v, want a served row marked unknown", row.ok, row.freezeUnknown)
	}
	results := []batchRowResult{row}
	env := batchEnvelope([]string{"native"}, results)
	if env.Flags.SingleSource || env.Flags.Frozen {
		t.Errorf("single_source = %v frozen = %v, want neither derived from an unread marker", env.Flags.SingleSource, env.Flags.Frozen)
	}
	if !batchDegraded(results) {
		t.Error("a row served with no freeze verdict must degrade the envelope")
	}
}
