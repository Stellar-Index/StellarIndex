package xdrjson

import (
	"sort"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ParticipantAccounts returns the non-source G-account strkeys an op touches, from a per-type allowlist of genuine account fields only. Free text (manage_data, memo, contract args) and Soroban InvokeContract args/auth are attacker-controlled and never interpreted. Output is deduped and sorted.
func ParticipantAccounts(bodyB64 string) ([]string, error) {
	var body xdr.OperationBody
	if err := scval.UnmarshalBase64(bodyB64, &body); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]struct{}{}
	// add records the underlying G-account: a G-strkey as-is, an M-strkey resolved to its ed25519
	// account, anything else (empty, C-, B-, ...) dropped. Only account-typed fields reach here,
	// so free text is never promoted to a participant.
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

// participantCandidateAddrs returns the raw (possibly muxed) addresses an operation body names in
// an account-typed field, keyed on op type. It is split out of ParticipantAccounts to keep both
// under the gocyclo threshold; see that godoc for the allowlist and InvokeHostFunction exclusion.
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
		// Deliberately no participants: call args and op.Auth entries are attacker-controllable here,
		// and deriving one would let an attacker inject a victim into that victim's permanent public
		// account history. The op source is still indexed via operations.source_account.
	}

	return out
}

// revokeSponsorshipLedgerKeyAccount returns the G-account owning the sponsored entry named by a
// RevokeSponsorship op's ledger-key arm, or "" when there is no single owner (e.g. a claimable
// balance or liquidity pool), which `add` drops.
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
