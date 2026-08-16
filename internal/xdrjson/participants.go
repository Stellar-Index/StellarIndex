package xdrjson

import (
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ParticipantAccounts returns the non-source G-account strkeys an operation
// touches — the "incoming"/counterparty side the participant index (ADR-0038
// Phase B) needs so an account's RECEIVED activity (it's the payment
// destination, the trustor, the merge target, the clawback victim, …) is
// queryable, not just what it sourced.
//
// Implementation: decode the op body and, keyed on the op type, collect ONLY
// the fields that are genuine account addresses (payment/path-payment
// destination, allow-trust / set-trust-line-flags trustor, clawback `from`,
// account-merge / create-account destination, muxed destinations resolved to
// their underlying G-account, and validated Soroban Address SCVal arguments).
// Opaque free-text fields (a manage_data name/value, a memo, a contract string
// arg) are NEVER interpreted as participants even when they happen to spell a
// valid G-strkey — a per-type allowlist is the only safe way to keep an
// attacker-controlled blob out of a victim account's history. The operation's
// own source account is handled separately (it's a lake column), so it is NOT
// returned here. Deduplicated + sorted (deterministic → idempotent re-derive).
func ParticipantAccounts(bodyB64 string) ([]string, error) {
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]struct{}{}
	// add classifies a candidate strkey and records the underlying G-account.
	// A plain G-strkey is added as-is; an M-strkey is resolved to its
	// underlying ed25519 account (that's the account whose RECEIVED activity we
	// index); anything else (empty, contract C-, claimable-balance B-, …) is
	// dropped. Only strkeys that reach here come from account-typed fields, so
	// this never promotes free text to a participant.
	add := func(candidate string) {
		var g string
		switch {
		case canonical.IsAccountID(candidate):
			g = candidate
		case canonical.IsMuxedAccount(candidate):
			resolved, ok := muxedToAccountID(candidate)
			if !ok {
				return
			}
			g = resolved
		default:
			return
		}
		if _, dup := seen[g]; dup {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}

	switch body.Type {
	case xdr.OperationTypeCreateAccount:
		op := body.MustCreateAccountOp()
		add(op.Destination.Address())
	case xdr.OperationTypePayment:
		add(muxedAddr(body.MustPaymentOp().Destination))
	case xdr.OperationTypePathPaymentStrictReceive:
		add(muxedAddr(body.MustPathPaymentStrictReceiveOp().Destination))
	case xdr.OperationTypePathPaymentStrictSend:
		add(muxedAddr(body.MustPathPaymentStrictSendOp().Destination))
	case xdr.OperationTypeAllowTrust:
		op := body.MustAllowTrustOp()
		add(op.Trustor.Address())
	case xdr.OperationTypeSetTrustLineFlags:
		op := body.MustSetTrustLineFlagsOp()
		add(op.Trustor.Address())
	case xdr.OperationTypeAccountMerge:
		add(muxedAddr(body.MustDestination()))
	case xdr.OperationTypeClawback:
		op := body.MustClawbackOp()
		add(muxedAddr(op.From))
	case xdr.OperationTypeInvokeHostFunction:
		addInvokeHostFunctionParticipants(body.MustInvokeHostFunctionOp(), add)
	}

	sort.Strings(out)
	return out, nil
}

// addInvokeHostFunctionParticipants collects the account addresses carried as
// Soroban InvokeContract arguments. Only ScVal::Address arguments are
// considered — a contract can pass a string/symbol/bytes arg that spells a
// valid G-strkey, and those must NOT become participants. Contract (C-),
// claimable-balance and liquidity-pool addresses are dropped by `add` (they are
// not accounts); account and muxed-account addresses resolve to a G-account.
func addInvokeHostFunctionParticipants(op xdr.InvokeHostFunctionOp, add func(string)) {
	if op.HostFunction.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
		return
	}
	for _, arg := range op.HostFunction.MustInvokeContract().Args {
		if arg.Type != xdr.ScValTypeScvAddress {
			continue
		}
		if s, err := scval.AsAddressStrkey(arg); err == nil {
			add(s)
		}
	}
}

// muxedToAccountID converts an M-strkey to its underlying G-strkey (the first
// 32 bytes of the 40-byte muxed payload are the ed25519 key). ok=false on a
// malformed M-strkey.
func muxedToAccountID(m string) (string, bool) {
	raw, err := strkey.Decode(strkey.VersionByteMuxedAccount, m)
	if err != nil || len(raw) < 32 {
		return "", false
	}
	g, err := strkey.Encode(strkey.VersionByteAccountID, raw[:32])
	if err != nil {
		return "", false
	}
	return g, true
}
