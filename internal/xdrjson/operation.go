// Package xdrjson decodes raw Stellar XDR (base64, as stored in the ClickHouse
// Tier-1 lake) into clean, human-readable JSON maps for the network-explorer
// API (ADR-0038). One decoder per operation type / memo / asset, reused by
// every explorer endpoint — handlers never touch XDR directly.
//
// Invariant (ADR-0003): every amount is rendered as a decimal STRING in the
// asset's smallest unit (stroops for classic), never a JSON number — classic
// Int64 amounts can exceed 2^53.
package xdrjson

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// opTypeName maps the XDR operation-type enum to the explorer's stable
// snake_case wire value. Centralised so the wire vocabulary is controlled (not
// derived from the SDK's CamelCase enum string, which could shift).
var opTypeName = map[xdr.OperationType]string{
	xdr.OperationTypeCreateAccount:                 "create_account",
	xdr.OperationTypePayment:                       "payment",
	xdr.OperationTypePathPaymentStrictReceive:      "path_payment_strict_receive",
	xdr.OperationTypePathPaymentStrictSend:         "path_payment_strict_send",
	xdr.OperationTypeManageSellOffer:               "manage_sell_offer",
	xdr.OperationTypeManageBuyOffer:                "manage_buy_offer",
	xdr.OperationTypeCreatePassiveSellOffer:        "create_passive_sell_offer",
	xdr.OperationTypeSetOptions:                    "set_options",
	xdr.OperationTypeChangeTrust:                   "change_trust",
	xdr.OperationTypeAllowTrust:                    "allow_trust",
	xdr.OperationTypeAccountMerge:                  "account_merge",
	xdr.OperationTypeInflation:                     "inflation",
	xdr.OperationTypeManageData:                    "manage_data",
	xdr.OperationTypeBumpSequence:                  "bump_sequence",
	xdr.OperationTypeCreateClaimableBalance:        "create_claimable_balance",
	xdr.OperationTypeClaimClaimableBalance:         "claim_claimable_balance",
	xdr.OperationTypeBeginSponsoringFutureReserves: "begin_sponsoring_future_reserves",
	xdr.OperationTypeEndSponsoringFutureReserves:   "end_sponsoring_future_reserves",
	xdr.OperationTypeRevokeSponsorship:             "revoke_sponsorship",
	xdr.OperationTypeClawback:                      "clawback",
	xdr.OperationTypeClawbackClaimableBalance:      "clawback_claimable_balance",
	xdr.OperationTypeSetTrustLineFlags:             "set_trust_line_flags",
	xdr.OperationTypeLiquidityPoolDeposit:          "liquidity_pool_deposit",
	xdr.OperationTypeLiquidityPoolWithdraw:         "liquidity_pool_withdraw",
	xdr.OperationTypeInvokeHostFunction:            "invoke_host_function",
	xdr.OperationTypeExtendFootprintTtl:            "extend_footprint_ttl",
	xdr.OperationTypeRestoreFootprint:              "restore_footprint",
}

// OpTypeName returns the snake_case wire name for an XDR op type, or
// "unknown_<n>" for an enum the map doesn't cover (forward-compat for a future
// protocol op).
func OpTypeName(t xdr.OperationType) string {
	if s, ok := opTypeName[t]; ok {
		return s
	}
	return fmt.Sprintf("unknown_%d", int(t))
}

// DecodedOp is the result of decoding one operation body.
type DecodedOp struct {
	// Type is the snake_case op type (e.g. "payment").
	Type string
	// Fields are the decoded, human-readable operation fields. Empty for op
	// types not yet field-decoded (the type + RawXDR still identify it).
	Fields map[string]any
	// RawXDR is the original base64 body, included only when Fields is empty
	// (so nothing is lost for not-yet-decoded types).
	RawXDR string
}

// DecodeOperationBody decodes a base64 operation body into a DecodedOp. The
// source account (operations may override the tx source) is passed separately
// by the caller since it lives outside the body in the lake.
func DecodeOperationBody(bodyB64 string) (DecodedOp, error) {
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		return DecodedOp{}, fmt.Errorf("xdrjson: unmarshal op body: %w", err)
	}
	d := DecodedOp{Type: OpTypeName(body.Type), Fields: map[string]any{}}
	fillOpFields(body, d.Fields)
	if len(d.Fields) == 0 {
		d.RawXDR = bodyB64
	}
	return d, nil
}

// fillOpFields populates the clean field map for the decoded types. Types not
// handled here leave Fields empty (caller attaches RawXDR).
func fillOpFields(b xdr.OperationBody, f map[string]any) { //nolint:gocyclo,funlen // one arm per op type; a flat switch is the clearest shape.
	switch b.Type {
	case xdr.OperationTypeCreateAccount:
		op := b.MustCreateAccountOp()
		f["destination"] = op.Destination.Address()
		f["starting_balance"] = amount(int64(op.StartingBalance))
	case xdr.OperationTypePayment:
		op := b.MustPaymentOp()
		f["destination"] = op.Destination.Address()
		f["asset"] = assetID(op.Asset)
		f["amount"] = amount(int64(op.Amount))
	case xdr.OperationTypePathPaymentStrictReceive:
		op := b.MustPathPaymentStrictReceiveOp()
		f["destination"] = op.Destination.Address()
		f["send_asset"] = assetID(op.SendAsset)
		f["send_max"] = amount(int64(op.SendMax))
		f["dest_asset"] = assetID(op.DestAsset)
		f["dest_amount"] = amount(int64(op.DestAmount))
		f["path"] = assetPath(op.Path)
	case xdr.OperationTypePathPaymentStrictSend:
		op := b.MustPathPaymentStrictSendOp()
		f["destination"] = op.Destination.Address()
		f["send_asset"] = assetID(op.SendAsset)
		f["send_amount"] = amount(int64(op.SendAmount))
		f["dest_asset"] = assetID(op.DestAsset)
		f["dest_min"] = amount(int64(op.DestMin))
		f["path"] = assetPath(op.Path)
	case xdr.OperationTypeManageSellOffer:
		op := b.MustManageSellOfferOp()
		f["selling"] = assetID(op.Selling)
		f["buying"] = assetID(op.Buying)
		f["amount"] = amount(int64(op.Amount))
		f["price"] = price(op.Price)
		f["offer_id"] = int64(op.OfferId)
	case xdr.OperationTypeManageBuyOffer:
		op := b.MustManageBuyOfferOp()
		f["selling"] = assetID(op.Selling)
		f["buying"] = assetID(op.Buying)
		f["buy_amount"] = amount(int64(op.BuyAmount))
		f["price"] = price(op.Price)
		f["offer_id"] = int64(op.OfferId)
	case xdr.OperationTypeCreatePassiveSellOffer:
		op := b.MustCreatePassiveSellOfferOp()
		f["selling"] = assetID(op.Selling)
		f["buying"] = assetID(op.Buying)
		f["amount"] = amount(int64(op.Amount))
		f["price"] = price(op.Price)
	case xdr.OperationTypeChangeTrust:
		op := b.MustChangeTrustOp()
		f["line"] = changeTrustAsset(op.Line)
		f["limit"] = amount(int64(op.Limit))
	case xdr.OperationTypeAllowTrust:
		op := b.MustAllowTrustOp()
		f["trustor"] = op.Trustor.Address()
		f["authorize"] = uint32(op.Authorize)
	case xdr.OperationTypeSetTrustLineFlags:
		op := b.MustSetTrustLineFlagsOp()
		f["trustor"] = op.Trustor.Address()
		f["asset"] = assetID(op.Asset)
		f["set_flags"] = uint32(op.SetFlags)
		f["clear_flags"] = uint32(op.ClearFlags)
	case xdr.OperationTypeAccountMerge:
		dest := b.MustDestination()
		f["destination"] = dest.Address()
	case xdr.OperationTypeManageData:
		op := b.MustManageDataOp()
		f["name"] = string(op.DataName)
		if op.DataValue != nil {
			f["value_base64"] = base64Bytes([]byte(*op.DataValue))
		}
	case xdr.OperationTypeBumpSequence:
		op := b.MustBumpSequenceOp()
		f["bump_to"] = int64(op.BumpTo)
	case xdr.OperationTypeClawback:
		op := b.MustClawbackOp()
		f["from"] = op.From.Address()
		f["asset"] = assetID(op.Asset)
		f["amount"] = amount(int64(op.Amount))
	case xdr.OperationTypeInvokeHostFunction:
		fillInvokeHostFunction(b.MustInvokeHostFunctionOp(), f)
	}
}

// fillInvokeHostFunction summarises a Soroban host-function op: the kind, and
// for InvokeContract the target contract + function name. Full arg decode is a
// Phase-A follow-up; contract_id + function_name are the high-value fields.
func fillInvokeHostFunction(op xdr.InvokeHostFunctionOp, f map[string]any) {
	switch op.HostFunction.Type {
	case xdr.HostFunctionTypeHostFunctionTypeInvokeContract:
		f["function"] = "invoke_contract"
		ic := op.HostFunction.MustInvokeContract()
		if cid, ok := contractAddress(ic.ContractAddress); ok {
			f["contract_id"] = cid
		}
		f["function_name"] = string(ic.FunctionName)
		f["arg_count"] = len(ic.Args)
	case xdr.HostFunctionTypeHostFunctionTypeCreateContract:
		f["function"] = "create_contract"
	case xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm:
		f["function"] = "upload_wasm"
	default:
		f["function"] = fmt.Sprintf("host_function_%d", int(op.HostFunction.Type))
	}
}
