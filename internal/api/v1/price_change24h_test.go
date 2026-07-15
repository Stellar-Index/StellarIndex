// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type stubChange24h struct{ then string }

func (s stubChange24h) USDPrice24hAgo(context.Context, canonical.Asset) (string, error) {
	return s.then, nil
}

// TestBatchChange24h pins the board-#41 contract: USD-quoted batch
// rows carry the signed trailing-24h change; non-USD quotes omit it.
func TestBatchChange24h(t *testing.T) {
	s := &Server{change24h: stubChange24h{then: "0.20"}}
	usd := defaultPriceQuote
	if got := s.batchChange24h(context.Background(), canonical.NativeAsset(), usd, "0.21"); got == nil || *got != "+5.00" {
		t.Fatalf("change = %v, want +5.00", got)
	}
	eur, _ := canonical.ParseAsset("fiat:EUR")
	if got := s.batchChange24h(context.Background(), canonical.NativeAsset(), eur, "0.21"); got != nil {
		t.Fatalf("non-USD quote should omit change, got %v", *got)
	}
	s.change24h = nil
	if got := s.batchChange24h(context.Background(), canonical.NativeAsset(), usd, "0.21"); got != nil {
		t.Fatal("nil reader should omit change")
	}
}
