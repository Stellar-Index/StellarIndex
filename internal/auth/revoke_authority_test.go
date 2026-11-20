// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import "testing"

func TestMayRevoke(t *testing.T) {
	// root → mid (record gone after a failed cascade) → leaf; root → sib; legacy → legacyChild (ParentKeyID only).
	owned := []APIKeyRecord{
		{KeyID: "kid_root"},
		{KeyID: "kid_leaf", ParentKeyID: "kid_mid", AncestorKeyIDs: []string{"kid_root", "kid_mid"}},
		{KeyID: "kid_sib", ParentKeyID: "kid_root", AncestorKeyIDs: []string{"kid_root"}},
		{KeyID: "kid_other"},
		{KeyID: "kid_legacy", ParentKeyID: "kid_root"},
		{KeyID: "kid_legacyChild", ParentKeyID: "kid_legacy"},
	}
	key := func(id string) RevokeActor { return RevokeActor{KeyID: id} }
	cases := []struct {
		name   string
		actor  RevokeActor
		target string
		want   bool
	}{
		{"own child", key("kid_root"), "kid_sib", true},
		{"own grandchild", key("kid_root"), "kid_leaf", true},
		{"missing child a descendant still names", key("kid_root"), "kid_mid", true},
		{"legacy grandchild", key("kid_root"), "kid_legacyChild", true},
		{"own parent", key("kid_sib"), "kid_root", false},
		{"ancestor named in a descendant's chain", key("kid_mid"), "kid_root", false},
		{"sibling", key("kid_sib"), "kid_leaf", false},
		{"other root", key("kid_root"), "kid_other", false},
		{"self", key("kid_root"), "kid_root", true},
		{"child self", key("kid_sib"), "kid_sib", true},
		{"unknown id", key("kid_root"), "kid_nope", false},
		{"no key and no account-wide authority", RevokeActor{}, "kid_sib", false},
		{"account-wide authority", RevokeActor{AnyKey: true}, "kid_other", true},
	}
	for _, tc := range cases {
		if got := MayRevoke(tc.actor, tc.target, owned); got != tc.want {
			t.Errorf("%s: MayRevoke = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		subject Subject
		want    bool
	}{
		{Subject{Tier: TierOperator, KeyID: "kid_x"}, true},
		{Subject{Tier: TierSEP10}, true},
		{Subject{Tier: TierSEP10, KeyID: "kid_x"}, false},
		{Subject{Tier: TierAPIKey, KeyID: "kid_x"}, false},
	} {
		if got := RevokeActorOf(tc.subject).AnyKey; got != tc.want {
			t.Errorf("RevokeActorOf(tier %s, key %q).AnyKey = %v, want %v", tc.subject.Tier, tc.subject.KeyID, got, tc.want)
		}
	}
}
