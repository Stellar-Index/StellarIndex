// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestResolvePendingClaimableBalances_CAP0038SameWindowCreate is T137:
// a claim against a claimable balance that CAP-0038 auto-liquidation
// created LATER IN THE SAME WINDOW must resolve from that window's
// own batch, not fall through to "unresolved" — the CAP-0038 create
// never reaches dec's in-run BalanceId index (it bypasses dec.Decode
// entirely; see classicMovementsHandleCAP0038Op), so
// classicMovementsResolvePendingClaimableBalances must fall back to
// scanning res.batch itself before counting a miss.
func TestResolvePendingClaimableBalances_CAP0038SameWindowCreate(t *testing.T) {
	// A claim against a balance_id the Decoder has never seen — this
	// mirrors decodeClaimClaimableBalance's own recordPending path,
	// exercised here via the real Decode() so the pending ref (and its
	// balance_id hex, computed the same way production does) is
	// genuine, not hand-built.
	var h xdr.Hash
	h[0] = 0x42
	bid := xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h}
	balanceIDHex := hex.EncodeToString(h[:])

	dec := classicmovements.NewDecoder()
	claimerAddr := "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF5"
	outs, err := dec.Decode(dispatcher.OpContext{
		Ledger:   40_000_100,
		TxHash:   "txclaim1",
		TxSource: claimerAddr,
		OpIndex:  0,
		Op: xdr.Operation{
			Body: xdr.OperationBody{
				Type:                    xdr.OperationTypeClaimClaimableBalance,
				ClaimClaimableBalanceOp: &xdr.ClaimClaimableBalanceOp{BalanceId: bid},
			},
		},
		OpResult: xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type:                        xdr.OperationTypeClaimClaimableBalance,
				ClaimClaimableBalanceResult: &xdr.ClaimClaimableBalanceResult{Code: xdr.ClaimClaimableBalanceResultCodeClaimClaimableBalanceSuccess},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decode(claim): %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(claim) produced %d movements, want 0 (must be pending, not resolved yet)", len(outs))
	}

	// res.batch already holds this window's CAP-0038-created balance —
	// built the way classicMovementsHandleCAP0038Op builds it (straight
	// from classicmovements.DecodeCAP0038Revocation's output), keyed on
	// the SAME balance_id the claim above references.
	res := &windowResult{
		windowCounts: map[classicmovements.Kind]int64{},
		batch: []clickhouse.AccountMovement{
			{
				MovementKind: string(classicmovements.KindClaimableBalanceCreate),
				Asset:        "native",
				Amount:       big.NewInt(500_000_000),
				FromAddress:  "GTRUSTORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF5",
				Attributes: map[string]any{
					"balance_id": balanceIDHex,
					"revocation": true,
				},
			},
		},
	}

	// chAddr is deliberately a bogus, unreachable address: if this call
	// falls through to the ClickHouse lookup at all, the test must
	// fail loudly (a network dial / DNS error) rather than silently
	// passing, proving the batch-local resolution is what actually
	// resolved the claim.
	classicMovementsResolvePendingClaimableBalances(context.Background(), "bogus.invalid:0", dec, 40_000_000, 40_000_200, res)

	if res.windowUnresolved != 0 {
		t.Fatalf("windowUnresolved = %d, want 0 — the CAP-0038 create is right there in res.batch", res.windowUnresolved)
	}
	if res.windowResolvedIndex != 1 {
		t.Fatalf("windowResolvedIndex = %d, want 1", res.windowResolvedIndex)
	}
	if len(res.batch) != 2 {
		t.Fatalf("res.batch has %d entries, want 2 (the CAP-0038 create + the now-resolved claim)", len(res.batch))
	}
	claim := res.batch[1]
	if claim.MovementKind != string(classicmovements.KindClaimableBalanceClaim) {
		t.Errorf("resolved movement kind = %q, want %q", claim.MovementKind, classicmovements.KindClaimableBalanceClaim)
	}
	if claim.Asset != "native" || claim.Amount == nil || claim.Amount.Cmp(big.NewInt(500_000_000)) != 0 {
		t.Errorf("resolved claim asset/amount = %q/%v, want native/500000000 (from the batch-local CAP-0038 create)", claim.Asset, claim.Amount)
	}
}
