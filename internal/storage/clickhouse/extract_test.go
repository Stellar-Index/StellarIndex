package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// claimAtomCount must mirror dispatcher.census + sdex.decode exactly,
// because classic_trade_effect_count is gated against the census
// oracle. The package previously had no test, and the 1000-ledger PoC
// sample happened to contain no crossing CreatePassiveSellOffer — so a
// wrong union-arm accessor (GetManageSellOfferResult instead of
// GetCreatePassiveSellOfferResult) silently undercounted and slipped
// past the gate. This table covers every claim-bearing op variant.

// claimNative / claimUSDC are the two DISTINCT legs every fixture claim
// carries. C2-010 (audit-2026-07-23): these atoms used to leave
// AssetSold/AssetBought at their zero value, which is
// xdr.AssetTypeAssetTypeNative on BOTH legs — a native/native self-cross
// stellar-core never emits and internal/sources/sdex has always dropped
// (canonical.NewPair rejects base == quote). That was invisible while
// sdexclaim.RealTradeCount looked only at amounts; now that it applies
// the decoder's full rule set, a fixture claim has to be shaped like a
// real one. Every `want` in the table below is unchanged.
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
			// REAL on-chain encoding: stellar-core emits passive-offer results
			// under the MANAGE_SELL_OFFER arm. GetCreatePassiveSellOfferResult
			// returns ok=false here; the fallback must still count the claims.
			// Confirmed vs Hubble at ledger 62701151 (these were dropped).
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
