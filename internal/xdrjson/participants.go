package xdrjson

import (
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
// account-merge / create-account destination, and muxed destinations resolved
// to their underlying G-account).
// Opaque free-text fields (a manage_data name/value, a memo, a contract string
// arg) are NEVER interpreted as participants even when they happen to spell a
// valid G-strkey — a per-type allowlist is the only safe way to keep an
// attacker-controlled blob out of a victim account's history. Soroban
// InvokeContract ops contribute NOTHING here: both the call arguments and the
// SorobanAuthorizationEntry auth entries are attacker-controllable at this
// XDR-decode layer — an auth-entry signature is verified only by the network
// during apply (and only for consumed require_auth entries on SUCCESSFUL txs),
// while the indexer also decodes failed-tx op bodies — so neither an arg- nor an
// auth-derived address can establish participation. Truthful Soroban received-
// activity belongs to the event-based /movements path (SEP-41 transfer events),
// not to arg derivation. The operation's own source account is handled
// separately (it's a lake column), so it is NOT returned here. Deduplicated +
// sorted (deterministic → idempotent re-derive).
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
		// Deliberately contributes no participants. A Soroban InvokeContract's
		// call args AND its op.Auth SorobanAuthorizationEntry entries are both
		// attacker-controllable at this XDR-decode layer (auth signatures are
		// verified only by the network during apply, and the indexer decodes
		// failed-tx op bodies too), so neither can be trusted to name a
		// participant — deriving one lets an attacker inject an arbitrary victim
		// into that victim's permanent public account history. See the godoc.
		// The op source is still indexed via operations.source_account.
	}

	sort.Strings(out)
	return out, nil
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
