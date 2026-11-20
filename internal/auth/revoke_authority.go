// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import "slices"

// RevokeActor is the authority a revoke request carries over one identifier's keys.
type RevokeActor struct {
	// AnyKey may revoke every key on the identifier: an operator, a SEP-10 session (which proved
	// control of the account itself), or a dashboard role that manages all of the account's keys.
	AnyKey bool
	// KeyID is the presented API key, which may revoke only the keys delegated from it.
	KeyID string
}

// RevokeActorOf is the revoke authority of an authenticated request's Subject. An API key's
// Subject carries no dashboard role, so it never gets the account-wide power. A key can be
// minted at the sep10 tier (and its children inherit it), so only a session with no KeyID is one.
func RevokeActorOf(s Subject) RevokeActor {
	if s.Tier == TierOperator || (s.Tier == TierSEP10 && s.KeyID == "") {
		return RevokeActor{AnyKey: true}
	}
	return RevokeActor{KeyID: s.KeyID}
}

// MayRevoke reports whether actor may revoke keyID, given owned, the identifier's records. A key
// may revoke exactly the tree its own revoke would cascade to, minus itself — never its minter,
// a sibling or an unrelated root. A target whose record is already gone still qualifies while a
// remaining descendant of actor names it, so a retry can finish a failed cascade.
func MayRevoke(actor RevokeActor, keyID string, owned []APIKeyRecord) bool {
	if actor.AnyKey {
		return true
	}
	if actor.KeyID == "" || keyID == "" || keyID == actor.KeyID {
		return false
	}
	records := make([]ownedRecord, 0, len(owned))
	for _, rec := range owned {
		records = append(records, ownedRecord{rec: rec})
	}
	for _, d := range descendantsOf(records, actor.KeyID) {
		if d.rec.KeyID == keyID || d.rec.ParentKeyID == keyID {
			return true
		}
		if i := slices.Index(d.rec.AncestorKeyIDs, actor.KeyID); i >= 0 && slices.Contains(d.rec.AncestorKeyIDs[i+1:], keyID) {
			return true
		}
	}
	return false
}
