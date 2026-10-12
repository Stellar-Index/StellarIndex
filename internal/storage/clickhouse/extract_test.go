package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// claimAtomCount must mirror dispatcher.census and sdex.decode exactly, as classic_trade_effect_count is
// gated against the census oracle. A wrong union-arm accessor silently undercounts, so this table covers
// every claim-bearing op variant.

// claimNative / claimUSDC are the two distinct legs every fixture claim carries. Zero-valued legs are native
// on both sides, a self-cross stellar-core never emits and sdex drops, so fixture claims must be shaped like
// real ones.
var (
	claimNative = xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	claimUSDC   = xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: xdr.AssetCode4{'U', 'S', 'D', 'C'},
			Issuer: xdr.AccountId{
				Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
				Ed25519: &xdr.Uint256{1},
			},
		},
	}
)

func mkClaims(n int) []xdr.ClaimAtom {
	claims := make([]xdr.ClaimAtom, n)
	for i := range claims {
		// Real trades carry non-zero amounts on two distinct assets;
		// RealTradeCount keeps these.
		claims[i] = xdr.ClaimAtom{
			Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
			OrderBook: &xdr.ClaimOfferAtom{
				AssetSold: claimNative, AmountSold: 100,
				AssetBought: claimUSDC, AmountBought: 200,
			},
		}
	}
	return claims
}

// mkClaimsMixed builds `real` value-moving claims followed by `zero` both-zero
// no-op crosses (the dust/rounding artifacts the decoder + census must drop).
func mkClaimsMixed(nReal, zero int) []xdr.ClaimAtom {
	claims := mkClaims(nReal)
	for i := 0; i < zero; i++ {
		claims = append(claims, xdr.ClaimAtom{
			Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
			OrderBook: &xdr.ClaimOfferAtom{
				AssetSold: claimNative, AmountSold: 0,
				AssetBought: claimUSDC, AmountBought: 0,
			},
		})
	}
	return claims
}

func TestClaimAtomCount_perOpVariant(t *testing.T) {
	cases := []struct {
		name   string
		op     xdr.Operation
		result xdr.OperationResult
		want   int
	}{
		{
			name: "ManageSellOffer",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageSellOffer,
					ManageSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mkClaims(2)},
					},
				},
			},
			want: 2,
		},
		{
			name: "ManageBuyOffer",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageBuyOffer}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageBuyOffer,
					ManageBuyOfferResult: &xdr.ManageBuyOfferResult{
						Code:    xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mkClaims(3)},
					},
				},
			},
			want: 3,
		},
		{
			// CreatePassiveSellOffer encoded under its own (spec) arm.
			name: "CreatePassiveSellOffer (passive arm)",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeCreatePassiveSellOffer}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeCreatePassiveSellOffer,
					CreatePassiveSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mkClaims(4)},
					},
				},
			},
			want: 4,
		},
		{
			// REAL on-chain encoding: stellar-core emits passive-offer results under the MANAGE_SELL_OFFER arm, so
			// GetCreatePassiveSellOfferResult returns ok=false here and the fallback must still count the claims.
			name: "CreatePassiveSellOffer (manage-sell arm — real)",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeCreatePassiveSellOffer}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageSellOffer,
					ManageSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mkClaims(2)},
					},
				},
			},
			want: 2,
		},
		{
			name: "PathPaymentStrictReceive",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePathPaymentStrictReceive}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypePathPaymentStrictReceive,
					PathPaymentStrictReceiveResult: &xdr.PathPaymentStrictReceiveResult{
						Code:    xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess,
						Success: &xdr.PathPaymentStrictReceiveResultSuccess{Offers: mkClaims(5)},
					},
				},
			},
			want: 5,
		},
		{
			name: "PathPaymentStrictSend",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePathPaymentStrictSend}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypePathPaymentStrictSend,
					PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
						Code:    xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess,
						Success: &xdr.PathPaymentStrictSendResultSuccess{Offers: mkClaims(6)},
					},
				},
			},
			want: 6,
		},
		{
			name:   "non-trade op yields zero",
			op:     xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePayment}},
			result: xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{Type: xdr.OperationTypePayment}},
			want:   0,
		},
		{
			// Both-zero no-op crosses (dust/rounding) are dropped — the census
			// must equal COUNT(trades), not the raw claim count. 3 real + 2
			// both-zero ⇒ 3 (mirrors sdex.decodeClaimAtom's both-zero drop).
			name: "both-zero no-ops dropped",
			op:   xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}},
			result: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageSellOffer,
					ManageSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mkClaimsMixed(3, 2)},
					},
				},
			},
			want: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimAtomCount(tc.op, tc.result); got != tc.want {
				t.Fatalf("claimAtomCount(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestLedgerHeaderCounts_FailedTxBasisMismatch documents a defect: tx_count and op_count include failed
// transactions but soroban_event_count does not. Delete this test once the bases are aligned or split.
func TestLedgerHeaderCounts_FailedTxBasisMismatch(t *testing.T) {
	ev := gateContractEvent(t)
	tx := gateTx(t, ev, false) // failed transaction
	tx.Envelope.V1.Tx.Operations = []xdr.Operation{{Body: xdr.OperationBody{
		Type:      xdr.OperationTypePayment,
		PaymentOp: &xdr.PaymentOp{Destination: xdr.MustMuxedAddress(ecTestG), Asset: claimNative, Amount: 1},
	}}}

	ext := &LedgerExtract{}
	extractOps(ext, tx, 100, time.Unix(1_700_000_000, 0).UTC(), "failedtxhash", ecTestG, 0, false)
	extractEvents(ext, tx, 100, time.Unix(1_700_000_000, 0).UTC(), "failedtxhash", nil, false)

	if ext.Ledger.OpCount == 0 {
		t.Fatalf("OpCount = 0, want > 0 — extractOps counts a failed tx's ops unconditionally " +
			"(if this now reads 0, the bases have converged and this test/GH-1068 should be revisited)")
	}
	if ext.Ledger.SorobanEventCount != 0 {
		t.Fatalf("SorobanEventCount = %d, want 0 — extractEvents excludes a failed tx entirely",
			ext.Ledger.SorobanEventCount)
	}
}

// TestExtractLedger_UnreadableTxCountsTowardTxCount: an unresolvable tx is skipped but still counted in
// tx_count, so gate.go's stored-vs-rows comparison fails instead of agreeing on fewer transactions.
func TestExtractLedger_UnreadableTxCountsTowardTxCount(t *testing.T) {
	lcm := xdr.LedgerCloseMeta{
		V: 0,
		V0: &xdr.LedgerCloseMetaV0{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{Header: xdr.LedgerHeader{LedgerSeq: 100}},
			// Result names a hash with no envelope in TxSet: Read() errors.
			TxProcessing: []xdr.TransactionResultMeta{{
				Result: xdr.TransactionResultPair{TransactionHash: xdr.Hash{0x01}},
			}},
		},
	}
	ext, err := ExtractLedger(lcm, "Test SDF Network ; September 2015")
	if err != nil {
		t.Fatalf("ExtractLedger: %v", err)
	}
	if ext.TxReadErrors != 1 {
		t.Fatalf("TxReadErrors = %d, want 1 (fixture must trigger a read error)", ext.TxReadErrors)
	}
	if len(ext.Txs) != 0 {
		t.Fatalf("transactions rows = %d, want 0", len(ext.Txs))
	}
	if ext.Ledger.TxCount != 1 {
		t.Fatalf("Ledger.TxCount = %d, want 1 (the unreadable tx must be counted)", ext.Ledger.TxCount)
	}
}
