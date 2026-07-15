package xdrjson

import (
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ParticipantAccounts returns the non-source G-account strkeys an operation
// touches — the "incoming"/counterparty side the participant index (ADR-0038
// Phase B) needs so an account's RECEIVED activity (it's the payment
// destination, the trustor, the merge target, the clawback victim, …) is
// queryable, not just what it sourced.
//
// Implementation: decode the op body and collect every decoded field value
// that is a valid G-strkey. This is deliberately generic — it picks up
// destination / from / trustor / to_address / etc. across all field-decoded op
// types without a per-type participant list to drift. The operation's own
// source account is handled separately (it's a lake column), so it is NOT
// returned here. Deduplicated + sorted (deterministic → idempotent re-derive).
func ParticipantAccounts(bodyB64 string) ([]string, error) {
	d, err := DecodeOperationBody(bodyB64)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(g string) {
		if _, dup := seen[g]; dup {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	for _, v := range d.Fields {
		s, ok := v.(string)
		if !ok {
			continue
		}
		switch {
		case canonical.IsAccountID(s):
			add(s)
		case canonical.IsMuxedAccount(s):
			// A muxed (M-) destination resolves to its underlying ed25519
			// account — that's the account whose RECEIVED activity we index.
			if g, ok := muxedToAccountID(s); ok {
				add(g)
			}
		}
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
