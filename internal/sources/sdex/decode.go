package sdex

import (
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// matchesTradeOp reports whether an op is one that could emit
// ClaimAtoms (a classic DEX trade). Covers every op type that
// stellar-core's ledger meta produces trades from.
func matchesTradeOp(op xdr.Operation) bool {
	switch op.Body.Type {
	case xdr.OperationTypeManageSellOffer,
		xdr.OperationTypeManageBuyOffer,
		xdr.OperationTypeCreatePassiveSellOffer,
		xdr.OperationTypePathPaymentStrictReceive,
		xdr.OperationTypePathPaymentStrictSend:
		return true
	}
	return false
}

// extractClaimAtoms pulls the OffersClaimed slice from whichever
// op-result variant this op produced. Returns nil when the op
// succeeded but matched no offers (no trade occurred), or when the
// op failed — both are "no trades," not an error.
func extractClaimAtoms(op xdr.Operation, result xdr.OperationResult) []xdr.ClaimAtom { //nolint:gocognit // switch over 5 trade op types, with a dual result-arm fallback for passive offers; linear and clearer unsplit.
	if result.Code != xdr.OperationResultCodeOpInner {
		return nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil
	}
	switch op.Body.Type {
	case xdr.OperationTypeManageSellOffer:
		r, ok := tr.GetManageSellOfferResult()
		if !ok || r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
			return nil
		}
		success := r.MustSuccess()
		return success.OffersClaimed

	case xdr.OperationTypeManageBuyOffer:
		r, ok := tr.GetManageBuyOfferResult()
		if !ok || r.Code != xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess {
			return nil
		}
		success := r.MustSuccess()
		return success.OffersClaimed

	case xdr.OperationTypeCreatePassiveSellOffer:
		// stellar-core emits passive-offer results under the ManageSellOfferResult arm, so
		// GetCreatePassiveSellOfferResult returns ok=false on real data and every passive
		// offer's claim atoms would be silently dropped. Try the passive arm (XDR spec) first,
		// then the manage-sell arm; both carry the same shape.
		if r, ok := tr.GetCreatePassiveSellOfferResult(); ok {
			if r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
				return nil
			}
			return r.MustSuccess().OffersClaimed
		}
		if r, ok := tr.GetManageSellOfferResult(); ok {
			if r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
				return nil
			}
			return r.MustSuccess().OffersClaimed
		}
		return nil

	case xdr.OperationTypePathPaymentStrictReceive:
		r, ok := tr.GetPathPaymentStrictReceiveResult()
		if !ok || r.Code != xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess {
			return nil
		}
		success := r.MustSuccess()
		return success.Offers

	case xdr.OperationTypePathPaymentStrictSend:
		r, ok := tr.GetPathPaymentStrictSendResult()
		if !ok || r.Code != xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess {
			return nil
		}
		success := r.MustSuccess()
		return success.Offers
	}
	return nil
}

// decodeClaimAtom turns one ClaimAtom into a canonical.Trade. tradeIndex is the claim's
// 0-based position within the op, used with a fanout stride to make OpIndex unique for
// multi-claim trades. Taker is the tx-level source for every claim: later claims are chained
// fills of the same offer-side action.
func decodeClaimAtom(
	atom xdr.ClaimAtom,
	ledgerSeq uint32,
	closedAt time.Time,
	txHash string,
	opIdx int,
	tradeIndex int,
	takerAccount string,
) (canonical.Trade, error) {
	if tradeIndex < 0 || tradeIndex >= opIndexFanoutStride {
		return canonical.Trade{}, fmt.Errorf("%w: tradeIndex=%d stride=%d",
			ErrClaimIndexOverflow, tradeIndex, opIndexFanoutStride)
	}

	var (
		sellerAccount string
		soldAsset     xdr.Asset
		boughtAsset   xdr.Asset
		soldAmount    xdr.Int64
		boughtAmount  xdr.Int64
	)

	switch atom.Type {
	case xdr.ClaimAtomTypeClaimAtomTypeOrderBook:
		ob := atom.MustOrderBook()
		sellerAccount, _ = strkey.Encode(strkey.VersionByteAccountID, ob.SellerId.Ed25519[:])
		soldAsset = ob.AssetSold
		boughtAsset = ob.AssetBought
		soldAmount = ob.AmountSold
		boughtAmount = ob.AmountBought

	case xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool:
		// The counterparty is a classic liquidity pool (not a Soroban AMM), identified by
		// pool ID rather than a G-address. The pool ID is stored as Maker so order-book and
		// LP trades stay distinguishable.
		lp := atom.MustLiquidityPool()
		// PoolID is a Hash; encode as hex for the Maker field.
		sellerAccount = fmt.Sprintf("%x", lp.LiquidityPoolId)
		soldAsset = lp.AssetSold
		boughtAsset = lp.AssetBought
		soldAmount = lp.AmountSold
		boughtAmount = lp.AmountBought

	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		// Legacy pre-CAP-27 shape: the seller is raw ed25519 bytes, not an AccountId. Derive
		// the G-strkey and surface it as a regular OrderBook trade; returning
		// ErrUnknownClaimAtomType would make the per-claim skip drop V0 fills silently and
		// leave a hole in SDEX history.
		v0 := atom.MustV0()
		sellerAccount, _ = strkey.Encode(strkey.VersionByteAccountID, v0.SellerEd25519[:])
		soldAsset = v0.AssetSold
		boughtAsset = v0.AssetBought
		soldAmount = v0.AmountSold
		boughtAmount = v0.AmountBought

	default:
		return canonical.Trade{}, fmt.Errorf("%w: type=%d", ErrUnknownClaimAtomType, atom.Type)
	}

	// Drop only both-zero no-op claim atoms (Hubble drops them too). KEEP one-side-zero
	// fills: a rounding artifact where one leg rounds to 0, a real trade Hubble records. They
	// carry no price, but aggregator/OHLC/outlier paths skip zero legs (Sign()<=0) and
	// tradeUSDVolume returns NULL, so pricing is unaffected.
	if soldAmount <= 0 && boughtAmount <= 0 {
		return canonical.Trade{}, fmt.Errorf("%w: sold=%d bought=%d",
			ErrNoOpClaim, soldAmount, boughtAmount)
	}

	base, err := xdrAssetToCanonical(soldAsset)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: sold asset: %w", ErrMalformedClaimAtom, err)
	}
	quote, err := xdrAssetToCanonical(boughtAsset)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: bought asset: %w", ErrMalformedClaimAtom, err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: pair: %w", ErrMalformedClaimAtom, err)
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      ledgerSeq,
		TxHash:      txHash,
		OpIndex:     uint32(opIdx)*opIndexFanoutStride + uint32(tradeIndex),
		Timestamp:   closedAt,
		Pair:        pair,
		BaseAmount:  amountFromInt64(soldAmount),
		QuoteAmount: amountFromInt64(boughtAmount),
		Taker:       takerAccount,
		Maker:       sellerAccount,
	}, nil
}

// xdrAssetToCanonical converts an xdr.Asset to canonical.Asset.
//
// The rule for which on-chain assets are representable, and so which fills become trade
// rows, lives in [canonical.AssetFromXDR] so internal/sdexclaim applies the identical test;
// otherwise the census and ClickHouse extractor would count fills this function rejects and
// their "should equal COUNT(trades)" oracles could never balance.
func xdrAssetToCanonical(a xdr.Asset) (canonical.Asset, error) {
	return canonical.AssetFromXDR(a)
}

// amountFromInt64 converts a classic-Stellar 7-decimal-scaled
// int64 amount (the XDR form) to canonical.Amount. No precision
// loss — classic amounts fit in int64.
func amountFromInt64(n xdr.Int64) canonical.Amount {
	return canonical.FromInt128Parts(int64(n>>63), uint64(int64(n)))
}

// opIndexFanoutStride spaces synthetic op_index values for multi-claim operations: one
// ManageOffer can cross many book levels, and each claim is a distinct Trade needing a unique
// (source, ledger, tx_hash, op_index, ts) key. 1024 exceeds any plausible claim count per op.
const opIndexFanoutStride = 1024
