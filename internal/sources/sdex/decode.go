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
		// CreatePassiveSellOffer results are emitted by stellar-core under the
		// ManageSellOfferResult union arm (core processes passive offers as
		// manage-sell-offers), so GetCreatePassiveSellOfferResult returns
		// ok=false on real on-chain data and we'd silently drop every passive
		// offer's claim atoms (confirmed vs Hubble at ledger 62701151). Try the
		// passive arm first (XDR spec), then fall back to the manage-sell arm
		// (what core actually emits). Both carry the same ManageSellOfferResult
		// shape.
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

// decodeClaimAtom turns one ClaimAtom into a canonical.Trade.
// tradeIndex is the 0-based position of this claim within the op
// — used to generate unique OpIndex values for multi-claim trades
// via a fanout stride (same pattern as aquarius + reflector).
//
// Taker is the tx-level source for the first claim, which matches
// what a human would read as "the account that placed this trade."
// (Subsequent claims in the same op are still that same taker —
// they're chained fills of a single offer-side action.)
func decodeClaimAtom(
	atom xdr.ClaimAtom,
	ledgerSeq uint32,
	closedAt time.Time,
	txHash string,
	opIdx int,
	tradeIndex int,
	takerAccount string,
) (canonical.Trade, error) {
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
		// Liquidity-pool claim: the counterparty is a classic
		// liquidity pool (NOT the Soroban AMMs) — identified by
		// its pool ID, not a G-address. We record the pool ID as
		// the Maker so analysts can distinguish order-book trades
		// from LP trades.
		lp := atom.MustLiquidityPool()
		// PoolID is a Hash; encode as hex for the Maker field.
		sellerAccount = fmt.Sprintf("%x", lp.LiquidityPoolId)
		soldAsset = lp.AssetSold
		boughtAsset = lp.AssetBought
		soldAmount = lp.AmountSold
		boughtAmount = lp.AmountBought

	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		// F-1233 (codex audit-2026-05-12): legacy pre-CAP-27 shape.
		// Distinguishable from ClaimOfferAtom only by carrying the
		// seller's raw ed25519 bytes (uint256) instead of an
		// AccountId discriminant. Surface as a regular OrderBook
		// trade by deriving the G-strkey from the raw bytes — the
		// rest of the shape (offer_id, asset+amount sold/bought) is
		// identical. Pre-fix we returned ErrUnknownClaimAtomType
		// here and the parent decoder's per-claim skip dropped V0
		// fills silently, leaving since-inception SDEX history with
		// a coverage hole.
		v0 := atom.MustV0()
		sellerAccount, _ = strkey.Encode(strkey.VersionByteAccountID, v0.SellerEd25519[:])
		soldAsset = v0.AssetSold
		boughtAsset = v0.AssetBought
		soldAmount = v0.AmountSold
		boughtAmount = v0.AmountBought

	default:
		return canonical.Trade{}, fmt.Errorf("%w: type=%d", ErrUnknownClaimAtomType, atom.Type)
	}

	// Drop only the both-zero no-op claim atoms stellar-core occasionally emits
	// (Hubble drops those too). KEEP one-side-zero fills — a rounding artifact
	// where one leg rounds to 0: these are real trades Hubble records, so we
	// capture them for completeness. They carry no price (one leg is 0), but the
	// aggregator/OHLC/outlier paths already skip zero legs (Sign()<=0) and
	// tradeUSDVolume returns NULL, so pricing is unaffected.
	if soldAmount <= 0 && boughtAmount <= 0 {
		return canonical.Trade{}, fmt.Errorf("%w: both-zero no-op claim sold=%d bought=%d",
			ErrMalformedClaimAtom, soldAmount, boughtAmount)
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
// C2-010 (audit-2026-07-23): the body moved to
// [canonical.AssetFromXDR]. It stays a thin local wrapper so this
// package's call sites and error-wrapping read unchanged, but the RULE
// — which on-chain assets are representable, and therefore which SDEX
// fills become trade rows — now lives in the leaf package where
// internal/sdexclaim can apply the identical test. Before the hoist,
// the dispatcher census and the ClickHouse extractor counted fills this
// function would have rejected, so their "should equal COUNT(trades)"
// oracles could never balance.
func xdrAssetToCanonical(a xdr.Asset) (canonical.Asset, error) {
	return canonical.AssetFromXDR(a)
}

// amountFromInt64 converts a classic-Stellar 7-decimal-scaled
// int64 amount (the XDR form) to canonical.Amount. No precision
// loss — classic amounts fit in int64.
func amountFromInt64(n xdr.Int64) canonical.Amount {
	return canonical.FromInt128Parts(int64(n>>63), uint64(int64(n)))
}

// opIndexFanoutStride spaces the synthetic op_index values for
// multi-claim operations. A single ManageOffer can cross many book
// levels in one op; each claim becomes a distinct Trade with a
// unique (source, ledger, tx_hash, op_index, ts) primary key.
//
// 1024 handles any plausible on-chain op (stellar caps ops-per-tx
// at 100, and classic DEX depth rarely exceeds a few dozen claims
// per aggressive trade). If we ever see this cap in the wild we
// have bigger problems.
const opIndexFanoutStride = 1024
