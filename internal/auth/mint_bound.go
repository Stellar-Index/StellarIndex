// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"net/netip"
	"slices"
)

// KeyRestrictions is the per-key policy a minted record carries: IP and Referer allowlists and
// endpoint permissions, in the [APIKeyRecord] encoding.
type KeyRestrictions struct {
	IPAllowlist      []string
	RefererAllowlist []string
	PermissionsAll   bool
	AllowPermissions []SubjectPermissionEntry
	DenyPermissions  []SubjectPermissionEntry
}

// restrictionsOf returns s's policy in the record encoding.
func restrictionsOf(s Subject) *KeyRestrictions {
	return &KeyRestrictions{
		IPAllowlist:      encodeIPAllowlist(s.IPAllowlist),
		RefererAllowlist: slices.Clone(s.RefererAllowlist),
		PermissionsAll:   s.AllowAllPermissions,
		AllowPermissions: slices.Clone(s.AllowPermissions),
		DenyPermissions:  slices.Clone(s.DenyPermissions),
	}
}

// applyTo writes r onto rec; a nil r leaves rec's defaults.
func (r *KeyRestrictions) applyTo(rec *APIKeyRecord) {
	if r == nil {
		return
	}
	rec.IPAllowlist = r.IPAllowlist
	rec.RefererAllowlist = r.RefererAllowlist
	rec.PermissionsAll = r.PermissionsAll
	rec.AllowPermissions = r.AllowPermissions
	rec.DenyPermissions = r.DenyPermissions
}

// mintBound reports whether rec's value for one field stays within minter's; nil means it does.
type mintBound func(minter Subject, rec APIKeyRecord) error

// mintBoundRules holds one rule per APIKeyRecord field (TestMintBoundRules_CoverEveryRecordField),
// so a field added to the record cannot reach a minted key without a stated bound.
var mintBoundRules = map[string]mintBound{
	"KeyID":            unbounded, // store-generated
	"KeyPrefix":        unbounded, // store-generated
	"CreatedAt":        unbounded, // store-stamped
	"Label":            unbounded, // display only
	"RevokedAt":        boundRevokedAt,
	"Identifier":       boundIdentifier,
	"ParentKeyID":      boundParentKeyID,
	"AncestorKeyIDs":   boundAncestorKeyIDs,
	"Tier":             boundTier,
	"Scopes":           boundScopes,
	"RateLimitPerMin":  boundRateLimit,
	"MonthlyQuota":     boundMonthlyQuota,
	"ExpiresAt":        boundExpiresAt,
	"EmailVerifiedAt":  boundEmailVerifiedAt,
	"IPAllowlist":      boundIPAllowlist,
	"RefererAllowlist": boundRefererAllowlist,
	"PermissionsAll":   boundPermissionsAll,
	"AllowPermissions": boundAllowPermissions,
	"DenyPermissions":  boundDenyPermissions,
}

// checkWithinMinter is the single chokepoint every MintedBy record passes before it is written.
func checkWithinMinter(minter Subject, rec APIKeyRecord) error {
	for field, bound := range mintBoundRules {
		if err := bound(minter, rec); err != nil {
			return fmt.Errorf("%w: %s %w", ErrMintExceedsCaller, field, err)
		}
	}
	return nil
}

func exceeds(format string, args ...any) error { return fmt.Errorf(format, args...) }

func unbounded(Subject, APIKeyRecord) error { return nil }

func boundRevokedAt(_ Subject, rec APIKeyRecord) error {
	if !rec.RevokedAt.IsZero() {
		return exceeds("must be unset on a new key")
	}
	return nil
}

// Only an operator issues keys on another account's identifier.
func boundIdentifier(m Subject, rec APIKeyRecord) error {
	if rec.Identifier != m.Identifier && m.Tier != TierOperator {
		return exceeds("%q differs from this key's own and only an operator key may mint for another identifier", rec.Identifier)
	}
	return nil
}

func boundParentKeyID(m Subject, rec APIKeyRecord) error {
	if rec.ParentKeyID == "" {
		return nil
	}
	if rec.ParentKeyID != m.KeyID || rec.Identifier != m.Identifier {
		return exceeds("must name the minting key on its own identifier")
	}
	return nil
}

// The chain is derived, never requested: a child that could name its own ancestors could drop
// the one an owner later revokes.
func boundAncestorKeyIDs(m Subject, rec APIKeyRecord) error {
	if !slices.Equal(rec.AncestorKeyIDs, lineageOf(m, rec.ParentKeyID)) {
		return exceeds("must be exactly this key's own ancestors followed by this key")
	}
	return nil
}

// lineageOf is the ancestor chain of a key minted by minter: none unless it is delegated (parentKeyID set).
func lineageOf(minter Subject, parentKeyID string) []string {
	if parentKeyID == "" {
		return nil
	}
	return append(slices.Clone(minter.AncestorKeyIDs), minter.KeyID)
}

func boundTier(m Subject, rec APIKeyRecord) error {
	if m.Tier != TierOperator && rec.Tier != m.Tier {
		return exceeds("%q differs from this key's own %q", rec.Tier, m.Tier)
	}
	return nil
}

func boundScopes(m Subject, rec APIKeyRecord) error {
	if len(m.Scopes) == 0 {
		return nil
	}
	if len(rec.Scopes) == 0 {
		return exceeds("empty (full access) from a key scoped to %v", m.Scopes)
	}
	for _, s := range rec.Scopes {
		if !m.HasScope(s) {
			return exceeds("%q is outside this key's own %v", s, m.Scopes)
		}
	}
	return nil
}

// A minter on the tier default (0) holds the default, not unlimited; only an operator sets
// another key's explicit rate.
func boundRateLimit(m Subject, rec APIKeyRecord) error {
	switch {
	case m.RateLimitPerMin > 0 && (rec.RateLimitPerMin <= 0 || rec.RateLimitPerMin > m.RateLimitPerMin):
		return exceeds("%d is not within this key's own %d (0 is the tier default)", rec.RateLimitPerMin, m.RateLimitPerMin)
	case m.RateLimitPerMin == 0 && rec.RateLimitPerMin != 0 && m.Tier != TierOperator:
		return exceeds("%d from a key on the tier default", rec.RateLimitPerMin)
	}
	return nil
}

// A stored 0 is unmetered on a non-account record, so a metered minter needs a positive cap.
func boundMonthlyQuota(m Subject, rec APIKeyRecord) error {
	if m.MonthlyQuota > 0 && (rec.MonthlyQuota <= 0 || rec.MonthlyQuota > m.MonthlyQuota) {
		return exceeds("%d is not within this key's own %d", rec.MonthlyQuota, m.MonthlyQuota)
	}
	return nil
}

func boundExpiresAt(m Subject, rec APIKeyRecord) error {
	if !m.ExpiresAt.IsZero() && (rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(m.ExpiresAt)) {
		return exceeds("must not outlive this key's own expiry")
	}
	return nil
}

func boundEmailVerifiedAt(m Subject, rec APIKeyRecord) error {
	if !rec.EmailVerifiedAt.IsZero() && (m.EmailVerifiedAt.IsZero() || rec.Identifier != m.Identifier) {
		return exceeds("a key may only carry its own identifier's verification")
	}
	return nil
}

func boundIPAllowlist(m Subject, rec APIKeyRecord) error {
	if len(m.IPAllowlist) == 0 {
		return nil
	}
	got, err := decodeIPAllowlist(rec.IPAllowlist)
	if err != nil {
		return err
	}
	if len(got) == 0 {
		return exceeds("empty (every IP) from a key confined to %v", m.IPAllowlist)
	}
	for _, p := range got {
		if !slices.ContainsFunc(m.IPAllowlist, func(q netip.Prefix) bool { return q.Bits() <= p.Bits() && q.Contains(p.Addr()) }) {
			return exceeds("%s is outside this key's own %v", p, m.IPAllowlist)
		}
	}
	return nil
}

func boundRefererAllowlist(m Subject, rec APIKeyRecord) error {
	if len(m.RefererAllowlist) == 0 {
		return nil
	}
	if len(rec.RefererAllowlist) == 0 {
		return exceeds("empty (every Referer) from a key confined to %v", m.RefererAllowlist)
	}
	for _, h := range rec.RefererAllowlist {
		if !slices.Contains(m.RefererAllowlist, h) {
			return exceeds("%q is outside this key's own %v", h, m.RefererAllowlist)
		}
	}
	return nil
}

func boundPermissionsAll(m Subject, rec APIKeyRecord) error {
	if rec.PermissionsAll && !m.AllowAllPermissions {
		return exceeds("full access from a key in allow-list mode")
	}
	return nil
}

func boundAllowPermissions(m Subject, rec APIKeyRecord) error {
	if m.AllowAllPermissions {
		return nil
	}
	for _, e := range rec.AllowPermissions {
		if !slices.Contains(m.AllowPermissions, e) {
			return exceeds("%+v is not among this key's own grants", e)
		}
	}
	return nil
}

func boundDenyPermissions(m Subject, rec APIKeyRecord) error {
	for _, e := range m.DenyPermissions {
		if !slices.Contains(rec.DenyPermissions, e) {
			return exceeds("drops this key's own deny %+v", e)
		}
	}
	return nil
}
