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
	"strconv"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
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
	if err := scval.UnmarshalBase64(bodyB64, &body); err != nil {
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
		f["destination"] = op.Destination.Address() // AccountId (never muxed)
		f["starting_balance"] = amount(int64(op.StartingBalance))
	case xdr.OperationTypePayment:
		op := b.MustPaymentOp()
		f["destination"] = muxedAddr(op.Destination)
		f["asset"] = assetID(op.Asset)
		f["amount"] = amount(int64(op.Amount))
	case xdr.OperationTypePathPaymentStrictReceive:
		op := b.MustPathPaymentStrictReceiveOp()
		f["destination"] = muxedAddr(op.Destination)
		f["send_asset"] = assetID(op.SendAsset)
		f["send_max"] = amount(int64(op.SendMax))
		f["dest_asset"] = assetID(op.DestAsset)
		f["dest_amount"] = amount(int64(op.DestAmount))
		f["path"] = assetPath(op.Path)
	case xdr.OperationTypePathPaymentStrictSend:
		op := b.MustPathPaymentStrictSendOp()
		f["destination"] = muxedAddr(op.Destination)
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
		f["offer_id"] = strconv.FormatInt(int64(op.OfferId), 10)
	case xdr.OperationTypeManageBuyOffer:
		op := b.MustManageBuyOfferOp()
		f["selling"] = assetID(op.Selling)
		f["buying"] = assetID(op.Buying)
		f["buy_amount"] = amount(int64(op.BuyAmount))
		f["price"] = price(op.Price)
		f["offer_id"] = strconv.FormatInt(int64(op.OfferId), 10)
	case xdr.OperationTypeCreatePassiveSellOffer:
		op := b.MustCreatePassiveSellOfferOp()
		f["selling"] = assetID(op.Selling)
		f["buying"] = assetID(op.Buying)
		f["amount"] = amount(int64(op.Amount))
		f["price"] = price(op.Price)
	case xdr.OperationTypeSetOptions:
		fillSetOptionsFields(b.MustSetOptionsOp(), f)
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
		f["destination"] = muxedAddr(b.MustDestination())
	case xdr.OperationTypeManageData:
		op := b.MustManageDataOp()
		// name is XDR string64 — opaque bytes, not guaranteed UTF-8.
		// name_base64 is the lossless companion (mirrors value_base64 below);
		// encoding/json would otherwise silently replace invalid bytes in
		// name with U+FFFD.
		f["name"] = string(op.DataName)
		f["name_base64"] = base64Bytes([]byte(op.DataName))
		if op.DataValue != nil {
			f["value_base64"] = base64Bytes([]byte(*op.DataValue))
		}
	case xdr.OperationTypeBumpSequence:
		op := b.MustBumpSequenceOp()
		f["bump_to"] = strconv.FormatInt(int64(op.BumpTo), 10)
	case xdr.OperationTypeClawback:
		op := b.MustClawbackOp()
		f["from"] = op.From.Address()
		f["asset"] = assetID(op.Asset)
		f["amount"] = amount(int64(op.Amount))
	case xdr.OperationTypeCreateClaimableBalance:
		op := b.MustCreateClaimableBalanceOp()
		f["asset"] = assetID(op.Asset)
		f["amount"] = amount(int64(op.Amount))
		f["claimants"] = claimantsFields(op.Claimants)
	case xdr.OperationTypeClaimClaimableBalance:
		op := b.MustClaimClaimableBalanceOp()
		if id, ok := claimableBalanceIDHex(op.BalanceId); ok {
			f["balance_id"] = id
		}
	case xdr.OperationTypeClawbackClaimableBalance:
		op := b.MustClawbackClaimableBalanceOp()
		if id, ok := claimableBalanceIDHex(op.BalanceId); ok {
			f["balance_id"] = id
		}
	case xdr.OperationTypeBeginSponsoringFutureReserves:
		op := b.MustBeginSponsoringFutureReservesOp()
		f["sponsored_id"] = op.SponsoredId.Address()
	case xdr.OperationTypeRevokeSponsorship:
		fillRevokeSponsorshipFields(b.MustRevokeSponsorshipOp(), f)
	case xdr.OperationTypeLiquidityPoolDeposit:
		op := b.MustLiquidityPoolDepositOp()
		f["liquidity_pool_id"] = poolIDHex(op.LiquidityPoolId)
		f["max_amount_a"] = amount(int64(op.MaxAmountA))
		f["max_amount_b"] = amount(int64(op.MaxAmountB))
		f["min_price"] = price(op.MinPrice)
		f["max_price"] = price(op.MaxPrice)
	case xdr.OperationTypeLiquidityPoolWithdraw:
		op := b.MustLiquidityPoolWithdrawOp()
		f["liquidity_pool_id"] = poolIDHex(op.LiquidityPoolId)
		f["amount"] = amount(int64(op.Amount))
		f["min_amount_a"] = amount(int64(op.MinAmountA))
		f["min_amount_b"] = amount(int64(op.MinAmountB))
	case xdr.OperationTypeInvokeHostFunction:
		fillInvokeHostFunction(b.MustInvokeHostFunctionOp(), f)
	case xdr.OperationTypeExtendFootprintTtl:
		op := b.MustExtendFootprintTtlOp()
		f["extend_to"] = uint32(op.ExtendTo)
	case xdr.OperationTypeRestoreFootprint:
		// The op body carries only the (currently always-void) extension
		// point — the restored read-write footprint lives in the envelope's
		// SorobanTransactionData, outside DecodeOperationBody's scope. Real,
		// not fabricated: a future protocol version that adds data here
		// extends this arm rather than needing a new one.
		op := b.MustRestoreFootprintOp()
		f["ext"] = int32(op.Ext.V)
	}
}

// fillSetOptionsFields decodes a set_options body. Every field is optional —
// an account only sets what it's changing — so each is emitted only when
// present; a bare re-signing set_options with everything nil legitimately
// decodes to nothing and falls back to raw_xdr like any other empty body.
func fillSetOptionsFields(op xdr.SetOptionsOp, f map[string]any) {
	if op.InflationDest != nil {
		f["inflation_dest"] = op.InflationDest.Address()
	}
	if op.ClearFlags != nil {
		f["clear_flags"] = uint32(*op.ClearFlags)
	}
	if op.SetFlags != nil {
		f["set_flags"] = uint32(*op.SetFlags)
	}
	if op.MasterWeight != nil {
		f["master_weight"] = uint32(*op.MasterWeight)
	}
	if op.LowThreshold != nil {
		f["low_threshold"] = uint32(*op.LowThreshold)
	}
	if op.MedThreshold != nil {
		f["med_threshold"] = uint32(*op.MedThreshold)
	}
	if op.HighThreshold != nil {
		f["high_threshold"] = uint32(*op.HighThreshold)
	}
	if op.HomeDomain != nil {
		f["home_domain"] = string(*op.HomeDomain)
	}
	if op.Signer != nil {
		addr, _ := op.Signer.Key.GetAddress()
		f["signer"] = map[string]any{"key": addr, "weight": uint32(op.Signer.Weight)}
	}
}

// claimantsFields renders a create_claimable_balance op's claimant list: the
// destination account and the predicate tree gating when it can claim.
func claimantsFields(cs []xdr.Claimant) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		v0, ok := c.GetV0()
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"destination": v0.Destination.Address(),
			"predicate":   claimPredicateFields(v0.Predicate),
		})
	}
	return out
}

// claimPredicateFields renders one node of a claim predicate tree. Recursive:
// and/or/not carry nested predicates.
func claimPredicateFields(p xdr.ClaimPredicate) map[string]any {
	switch p.Type {
	case xdr.ClaimPredicateTypeClaimPredicateUnconditional:
		return map[string]any{"type": "unconditional"}
	case xdr.ClaimPredicateTypeClaimPredicateAnd:
		return map[string]any{"type": "and", "predicates": claimPredicateList(p.MustAndPredicates())}
	case xdr.ClaimPredicateTypeClaimPredicateOr:
		return map[string]any{"type": "or", "predicates": claimPredicateList(p.MustOrPredicates())}
	case xdr.ClaimPredicateTypeClaimPredicateNot:
		m := map[string]any{"type": "not"}
		if inner := p.MustNotPredicate(); inner != nil {
			m["predicate"] = claimPredicateFields(*inner)
		}
		return m
	case xdr.ClaimPredicateTypeClaimPredicateBeforeAbsoluteTime:
		return map[string]any{"type": "before_absolute_time", "abs_before": strconv.FormatInt(int64(p.MustAbsBefore()), 10)}
	case xdr.ClaimPredicateTypeClaimPredicateBeforeRelativeTime:
		return map[string]any{"type": "before_relative_time", "rel_before": strconv.FormatInt(int64(p.MustRelBefore()), 10)}
	default:
		return map[string]any{"type": "unknown"}
	}
}

func claimPredicateList(ps []xdr.ClaimPredicate) []map[string]any {
	out := make([]map[string]any, len(ps))
	for i, p := range ps {
		out[i] = claimPredicateFields(p)
	}
	return out
}

// fillRevokeSponsorshipFields decodes a revoke_sponsorship body: either a
// ledger-entry key (the sponsored entry) or a signer (account + key being
// de-sponsored).
func fillRevokeSponsorshipFields(op xdr.RevokeSponsorshipOp, f map[string]any) {
	switch op.Type {
	case xdr.RevokeSponsorshipTypeRevokeSponsorshipLedgerEntry:
		f["sponsorship_type"] = "ledger_entry"
		fillLedgerKeyFields(op.MustLedgerKey(), f)
	case xdr.RevokeSponsorshipTypeRevokeSponsorshipSigner:
		s := op.MustSigner()
		f["sponsorship_type"] = "signer"
		f["account_id"] = s.AccountId.Address()
		if addr, err := s.SignerKey.GetAddress(); err == nil {
			f["signer_key"] = addr
		}
	}
}

// fillLedgerKeyFields identifies the sponsored ledger entry for the six
// classic entry types (account/trustline/offer/data/claimable-balance/
// liquidity-pool). The three Soroban-only key types (contract data/code,
// config setting, TTL) are sponsorable but rendered by type name only — their
// identifying fields are contract-storage keys, not accounts.
func fillLedgerKeyFields(lk xdr.LedgerKey, f map[string]any) {
	switch lk.Type {
	case xdr.LedgerEntryTypeAccount:
		f["account_id"] = lk.Account.AccountId.Address()
	case xdr.LedgerEntryTypeTrustline:
		f["account_id"] = lk.TrustLine.AccountId.Address()
		f["asset"] = TrustLineAssetID(lk.TrustLine.Asset)
	case xdr.LedgerEntryTypeOffer:
		f["seller_id"] = lk.Offer.SellerId.Address()
		f["offer_id"] = strconv.FormatInt(int64(lk.Offer.OfferId), 10)
	case xdr.LedgerEntryTypeData:
		f["account_id"] = lk.Data.AccountId.Address()
		f["name"] = string(lk.Data.DataName)
	case xdr.LedgerEntryTypeClaimableBalance:
		if id, ok := claimableBalanceIDHex(lk.ClaimableBalance.BalanceId); ok {
			f["balance_id"] = id
		}
	case xdr.LedgerEntryTypeLiquidityPool:
		f["liquidity_pool_id"] = poolIDHex(lk.LiquidityPool.LiquidityPoolId)
	default:
		f["ledger_entry_type"] = ledgerEntryTypeName(lk.Type)
	}
}

// ledgerEntryTypeName maps the three Soroban-only ledger-entry types a
// revoke_sponsorship can target to the explorer's snake_case vocabulary —
// same discipline as opTypeName: controlled, not derived from the SDK's
// CamelCase enum string.
func ledgerEntryTypeName(t xdr.LedgerEntryType) string {
	switch t {
	case xdr.LedgerEntryTypeContractData:
		return "contract_data"
	case xdr.LedgerEntryTypeContractCode:
		return "contract_code"
	case xdr.LedgerEntryTypeConfigSetting:
		return "config_setting"
	case xdr.LedgerEntryTypeTtl:
		return "ttl"
	default:
		return fmt.Sprintf("unknown_%d", int(t))
	}
}

// fillInvokeHostFunction decodes a Soroban host-function op: the kind, and for
// InvokeContract the target contract + function name + the full argument list.
// Args are rendered through scval.Display — the compact human-readable form the
// explorer's contract-event rows already use — so i128 amounts stay decimal
// strings (ADR-0003) and addresses render as strkeys. `arg_count` predates the
// full decode and is kept for wire back-compat.
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
		args := make([]string, len(ic.Args))
		for i, a := range ic.Args {
			args[i] = scval.Display(a)
		}
		f["args"] = args
	case xdr.HostFunctionTypeHostFunctionTypeCreateContract:
		f["function"] = "create_contract"
	case xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm:
		f["function"] = "upload_wasm"
	default:
		f["function"] = fmt.Sprintf("host_function_%d", int(op.HostFunction.Type))
	}
	// The nested AUTHORIZATION tree carried in op.Auth. This is the subtree of
	// contract calls that required authorization — NOT the full execution/call
	// tree (that lives in the tx meta's diagnostic events, which the lake does
	// not store). For a contract-heavy tx it still surfaces the nested call
	// structure the /tx view otherwise omits (the piece stellar.expert shows
	// and we did not). Empty for a simple single-call invoke with no sub-auth.
	if tree := authInvocationTree(op.Auth); len(tree) > 0 {
		f["authorizations"] = tree
	}
}

// AuthInvocation is one node of a Soroban AUTHORIZATION tree — the nested
// SorobanAuthorizedInvocation structure carried in an InvokeHostFunction op's
// auth entries. Exported so the API view can type it; a create-contract
// authorization has Kind "create_contract" and no function/args. Credentials
// is set only on the root of each tree — a sub-invocation's authorization is
// implied by its parent's, the XDR carries none of its own.
type AuthInvocation struct {
	Kind           string           `json:"kind"` // invoke_contract | create_contract
	ContractID     string           `json:"contract_id,omitempty"`
	FunctionName   string           `json:"function_name,omitempty"`
	Args           []string         `json:"args,omitempty"`
	Credentials    *AuthCredentials `json:"credentials,omitempty"`
	SubInvocations []AuthInvocation `json:"sub_invocations,omitempty"`
}

// AuthCredentials describes who authorized one auth entry's root invocation:
// the op's own source account, or a separate address (which may be a
// different signer entirely — e.g. a relayer-submitted transaction carrying
// a user's signed auth entry, GH-1138).
type AuthCredentials struct {
	Kind                      string `json:"kind"` // source_account | address
	Address                   string `json:"address,omitempty"`
	Nonce                     string `json:"nonce,omitempty"`
	SignatureExpirationLedger uint32 `json:"signature_expiration_ledger,omitempty"`
}

// authInvocationTree builds the authorized-invocation forest from an op's auth
// entries (one root per entry). Mirrors the dispatcher's walkAuthTree traversal.
func authInvocationTree(auth []xdr.SorobanAuthorizationEntry) []AuthInvocation {
	if len(auth) == 0 {
		return nil
	}
	out := make([]AuthInvocation, 0, len(auth))
	for i := range auth {
		node := buildAuthInvocation(&auth[i].RootInvocation)
		node.Credentials = authCredentialsFields(auth[i].Credentials)
		out = append(out, node)
	}
	return out
}

// authCredentialsFields renders the SorobanCredentials for one auth entry's
// root. source_account carries nothing further (the op's own source signed
// it); the address variants (including the V2 and delegated forms) carry the
// authorizing address, its nonce, and its signature-expiration ledger.
func authCredentialsFields(c xdr.SorobanCredentials) *AuthCredentials {
	switch c.Type {
	case xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount:
		return &AuthCredentials{Kind: "source_account"}
	case xdr.SorobanCredentialsTypeSorobanCredentialsAddress:
		return addressCredentialsFields(c.MustAddress())
	case xdr.SorobanCredentialsTypeSorobanCredentialsAddressV2:
		return addressCredentialsFields(c.MustAddressV2())
	case xdr.SorobanCredentialsTypeSorobanCredentialsAddressWithDelegates:
		return addressCredentialsFields(c.MustAddressWithDelegates().AddressCredentials)
	default:
		return nil
	}
}

// addressCredentialsFields renders the common shape shared by the address,
// address-v2, and address-with-delegates SorobanCredentials variants.
func addressCredentialsFields(a xdr.SorobanAddressCredentials) *AuthCredentials {
	addr, _ := contractAddress(a.Address)
	return &AuthCredentials{
		Kind:                      "address",
		Address:                   addr,
		Nonce:                     strconv.FormatInt(int64(a.Nonce), 10),
		SignatureExpirationLedger: uint32(a.SignatureExpirationLedger),
	}
}

// buildAuthInvocation renders one SorobanAuthorizedInvocation node + its
// sub-invocations recursively, reusing the same arg display (scval.Display) and
// contract-strkey rendering as the top-level InvokeContract decode.
func buildAuthInvocation(node *xdr.SorobanAuthorizedInvocation) AuthInvocation {
	n := AuthInvocation{Kind: "create_contract"}
	if node.Function.Type == xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn {
		ic := node.Function.MustContractFn()
		n.Kind = "invoke_contract"
		if cid, ok := contractAddress(ic.ContractAddress); ok {
			n.ContractID = cid
		}
		n.FunctionName = string(ic.FunctionName)
		n.Args = make([]string, len(ic.Args))
		for i, a := range ic.Args {
			n.Args[i] = scval.Display(a)
		}
	}
	for i := range node.SubInvocations {
		n.SubInvocations = append(n.SubInvocations, buildAuthInvocation(&node.SubInvocations[i]))
	}
	return n
}
