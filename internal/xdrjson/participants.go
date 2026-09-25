package xdrjson

import (
	"sort"

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
// account-merge / create-account destination, create-claimable-balance
// claimant destinations, begin-sponsoring-future-reserves and
// revoke-sponsorship sponsorship targets, and muxed destinations resolved to
// their underlying G-account).
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
	if err := scval.UnmarshalBase64(bodyB64, &body); err != nil {
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
			resolved, ok := canonical.MuxedAccountID(candidate)
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

	for _, candidate := range participantCandidateAddrs(body) {
		add(candidate)
	}

	sort.Strings(out)
	return out, nil
}

// participantCandidateAddrs returns the raw (possibly muxed) address
// strings an operation body names in an account-typed field, keyed on op
// type. Split out of ParticipantAccounts to keep the per-type dispatch and
// the seen/candidate bookkeeping each independently under the gocyclo
// threshold; behaviour is unchanged. See ParticipantAccounts' godoc for the
// allowlist rationale and the InvokeHostFunction exclusion.
func participantCandidateAddrs(body xdr.OperationBody) []string {
	var out []string

	switch body.Type {
	case xdr.OperationTypeCreateAccount:
		op := body.MustCreateAccountOp()
		out = append(out, op.Destination.Address())
	case xdr.OperationTypePayment:
		out = append(out, muxedAddr(body.MustPaymentOp().Destination))
	case xdr.OperationTypePathPaymentStrictReceive:
		out = append(out, muxedAddr(body.MustPathPaymentStrictReceiveOp().Destination))
	case xdr.OperationTypePathPaymentStrictSend:
		out = append(out, muxedAddr(body.MustPathPaymentStrictSendOp().Destination))
	case xdr.OperationTypeAllowTrust:
		op := body.MustAllowTrustOp()
		out = append(out, op.Trustor.Address())
	case xdr.OperationTypeSetTrustLineFlags:
		op := body.MustSetTrustLineFlagsOp()
		out = append(out, op.Trustor.Address())
	case xdr.OperationTypeAccountMerge:
		out = append(out, muxedAddr(body.MustDestination()))
	case xdr.OperationTypeClawback:
		op := body.MustClawbackOp()
		out = append(out, muxedAddr(op.From))
	case xdr.OperationTypeCreateClaimableBalance:
		op := body.MustCreateClaimableBalanceOp()
		for _, c := range op.Claimants {
			v0, ok := c.GetV0()
			if !ok {
				continue
			}
			out = append(out, v0.Destination.Address())
		}
	case xdr.OperationTypeClaimClaimableBalance:
		// The claim's only address-shaped field is the ClaimableBalanceId
		// itself, not an account. The claimant is the op's own source
		// account, already indexed via operations.source_account.
	case xdr.OperationTypeBeginSponsoringFutureReserves:
		op := body.MustBeginSponsoringFutureReservesOp()
		out = append(out, op.SponsoredId.Address())
	case xdr.OperationTypeRevokeSponsorship:
		op := body.MustRevokeSponsorshipOp()
		if lk, ok := op.GetLedgerKey(); ok {
			out = append(out, revokeSponsorshipLedgerKeyAccount(lk))
		} else if signer, ok := op.GetSigner(); ok {
			out = append(out, signer.AccountId.Address())
		}
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

	return out
}

// revokeSponsorshipLedgerKeyAccount returns the G-account that owns the
// sponsored ledger entry named by a RevokeSponsorship op's ledger-key arm —
// the account whose reserve requirement the revocation returns to it — or ""
// when the entry has no single owning account (e.g. a claimable balance or
// liquidity pool), which `add` safely drops.
func revokeSponsorshipLedgerKeyAccount(lk xdr.LedgerKey) string {
	switch lk.Type {
	case xdr.LedgerEntryTypeAccount:
		return lk.MustAccount().AccountId.Address()
	case xdr.LedgerEntryTypeTrustline:
		return lk.MustTrustLine().AccountId.Address()
	case xdr.LedgerEntryTypeOffer:
		return lk.MustOffer().SellerId.Address()
	case xdr.LedgerEntryTypeData:
		return lk.MustData().AccountId.Address()
	default:
		return ""
	}
}
