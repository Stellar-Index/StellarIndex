// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package sdex

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// TestDecoder_DecodeCounted_ReportsPerClaimFailures is the T214
// regression: Decode silently drops a claim atom it cannot decode
// (obs.SourceDecodeErrorsTotal is the only signal), so the
// completeness census had no way to know a ledger's expected count
// was produced from an incomplete claim set. DecodeCounted must
// surface that count so the census can mark the ledger blind
// (completeness.BlindTracker) instead of certifying a clean reconcile
// on a ledger where a claim was provably dropped.
func TestDecoder_DecodeCounted_ReportsPerClaimFailures(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	claims := []xdr.ClaimAtom{
		mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000), // valid
		mkOrderBookClaim(t, 0x22, 2, xlm, usdc, 0, 0),                   // both-zero no-op: decodeClaimAtom errors
	}
	op, result := mkManageSellOfferOp(claims)
	taker, _ := mkAccount(t, 0x01)
	ctx := dispatcher.OpContext{
		Ledger: 100, TxHash: "hash", OpIndex: 5,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	}

	outs, failed := NewDecoder().DecodeCounted(ctx)
	if len(outs) != 1 {
		t.Fatalf("DecodeCounted outs = %d, want 1 (one claim decodes, one fails)", len(outs))
	}
	if failed != 1 {
		t.Fatalf("DecodeCounted failed = %d, want 1 — the census cannot mark this ledger blind without this count", failed)
	}

	// Decode (the dispatcher.OpDecoder-conforming entry point) must keep
	// its existing contract unchanged: same trades, always a nil error.
	dOuts, err := NewDecoder().Decode(ctx)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(dOuts) != len(outs) {
		t.Fatalf("Decode outs = %d, DecodeCounted outs = %d — must agree", len(dOuts), len(outs))
	}
}

// TestDecoder_DecodeCounted_CleanOpReportsZeroFailures — the counter must
// stay silent on the healthy path or every SDEX census ledger would read
// blind.
func TestDecoder_DecodeCounted_CleanOpReportsZeroFailures(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claims := []xdr.ClaimAtom{mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000)}
	op, result := mkManageSellOfferOp(claims)
	taker, _ := mkAccount(t, 0x01)

	outs, failed := NewDecoder().DecodeCounted(dispatcher.OpContext{
		Ledger: 1, TxHash: "hash", OpIndex: 0,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0 on a fully-decodable op", failed)
	}
	if len(outs) != 1 {
		t.Errorf("outs = %d, want 1", len(outs))
	}
}
