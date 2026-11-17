// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newBoundTestStore(t *testing.T) (*RedisAPIKeyStore, *RedisAPIKeyValidator) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return NewRedisAPIKeyStore(rdb, WithStoreClock(func() time.Time { return now })),
		NewRedisAPIKeyValidator(rdb, WithClock(func() time.Time { return now }))
}

// restrictedParent is a minter narrowed in every dimension a key record can carry.
func restrictedParent() Subject {
	return Subject{
		KeyID:               "kid_parent",
		Identifier:          "acct-parent",
		Tier:                TierAPIKey,
		Scopes:              []string{"account"},
		RateLimitPerMin:     120,
		MonthlyQuota:        50_000,
		ExpiresAt:           time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		EmailVerifiedAt:     time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		IPAllowlist:         []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		RefererAllowlist:    []string{"app.example"},
		AllowAllPermissions: false,
		AllowPermissions:    []SubjectPermissionEntry{{EndpointPrefix: "/v1/account"}},
		DenyPermissions:     []SubjectPermissionEntry{{Endpoint: "POST /v1/account/keys"}},
	}
}

// TestDelegatedKeyNeverExceedsMinter_InEveryDimension mints a child through the production
// path (ChildKeyRequest -> Create -> Lookup) from a minter restricted in every dimension and
// requires the child's Subject to carry each restriction.
func TestDelegatedKeyNeverExceedsMinter_InEveryDimension(t *testing.T) {
	store, validator := newBoundTestStore(t)
	parent := restrictedParent()

	_, plain, err := store.Create(context.Background(), ChildKeyRequest(parent, "child", nil))
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	child, err := validator.Lookup(context.Background(), plain)
	if err != nil {
		t.Fatalf("Lookup child: %v", err)
	}

	if child.Identifier != parent.Identifier || child.Tier != parent.Tier {
		t.Errorf("child owner/tier = %q/%q, want the minter's %q/%q", child.Identifier, child.Tier, parent.Identifier, parent.Tier)
	}
	if !slices.Equal(child.Scopes, parent.Scopes) {
		t.Errorf("child scopes = %v, want the minter's %v", child.Scopes, parent.Scopes)
	}
	if child.RateLimitPerMin != parent.RateLimitPerMin || child.MonthlyQuota != parent.MonthlyQuota {
		t.Errorf("child rate/quota = %d/%d, want the minter's %d/%d",
			child.RateLimitPerMin, child.MonthlyQuota, parent.RateLimitPerMin, parent.MonthlyQuota)
	}
	if !child.ExpiresAt.Equal(parent.ExpiresAt) || !child.EmailVerifiedAt.Equal(parent.EmailVerifiedAt) {
		t.Errorf("child expiry/verified = %v/%v, want the minter's %v/%v",
			child.ExpiresAt, child.EmailVerifiedAt, parent.ExpiresAt, parent.EmailVerifiedAt)
	}
	if !slices.Equal(child.IPAllowlist, parent.IPAllowlist) {
		t.Errorf("child IPAllowlist = %v, want the minter's %v (an empty list admits every IP)", child.IPAllowlist, parent.IPAllowlist)
	}
	if !slices.Equal(child.RefererAllowlist, parent.RefererAllowlist) {
		t.Errorf("child RefererAllowlist = %v, want the minter's %v", child.RefererAllowlist, parent.RefererAllowlist)
	}
	if child.AllowAllPermissions {
		t.Error("child AllowAllPermissions = true from a minter in allow-list mode")
	}
	if !slices.Equal(child.AllowPermissions, parent.AllowPermissions) {
		t.Errorf("child AllowPermissions = %v, want the minter's %v", child.AllowPermissions, parent.AllowPermissions)
	}
	if !slices.Equal(child.DenyPermissions, parent.DenyPermissions) {
		t.Errorf("child DenyPermissions = %v, want the minter's %v", child.DenyPermissions, parent.DenyPermissions)
	}
}

// TestMintedKeyNeverExceedsMinter_RefusesBroaderRequest: a request broader than its minter in
// any bounded dimension is refused with ErrMintExceedsCaller, never silently persisted.
func TestMintedKeyNeverExceedsMinter_RefusesBroaderRequest(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Subject, *CreateAPIKeyRequest)
		field string
	}{
		{"identifier: non-operator mints for another identifier", func(_ *Subject, r *CreateAPIKeyRequest) { r.Identifier = "acct-other" }, "Identifier"},
		{"tier: apikey minter asks for operator", func(_ *Subject, r *CreateAPIKeyRequest) { r.Tier = TierOperator }, "Tier"},
		{"parent: ParentKeyID names another key", func(_ *Subject, r *CreateAPIKeyRequest) { r.ParentKeyID = "kid_other" }, "ParentKeyID"},
		{"scopes: outside the minter's", func(_ *Subject, r *CreateAPIKeyRequest) { r.Scopes = []string{"admin"} }, "Scopes"},
		{"rate: above the minter's", func(_ *Subject, r *CreateAPIKeyRequest) { r.RateLimitPerMin = 121 }, "RateLimitPerMin"},
		{"quota: above the minter's", func(_ *Subject, r *CreateAPIKeyRequest) { r.MonthlyQuota = 50_001 }, "MonthlyQuota"},
		{"expiry: later than the minter's", func(m *Subject, r *CreateAPIKeyRequest) { r.ExpiresAt = m.ExpiresAt.Add(time.Hour) }, "ExpiresAt"},
		{"email: verified from an unverified minter", func(m *Subject, _ *CreateAPIKeyRequest) { m.EmailVerifiedAt = time.Time{} }, "EmailVerifiedAt"},
		{"ip: wider than the minter's", func(_ *Subject, r *CreateAPIKeyRequest) { r.Restrictions.IPAllowlist = []string{"203.0.0.0/16"} }, "IPAllowlist"},
		{"ip: dropped", func(_ *Subject, r *CreateAPIKeyRequest) { r.Restrictions = nil }, "IPAllowlist"},
		{"referer: host the minter lacks", func(_ *Subject, r *CreateAPIKeyRequest) { r.Restrictions.RefererAllowlist = []string{"evil.example"} }, "RefererAllowlist"},
		{"permissions: all from an allow-list minter", func(_ *Subject, r *CreateAPIKeyRequest) { r.Restrictions.PermissionsAll = true }, "PermissionsAll"},
		{"permissions: allow entry the minter lacks", func(_ *Subject, r *CreateAPIKeyRequest) {
			r.Restrictions.AllowPermissions = []SubjectPermissionEntry{{EndpointPrefix: "/v1"}}
		}, "AllowPermissions"},
		{"permissions: drops a minter deny", func(_ *Subject, r *CreateAPIKeyRequest) {
			r.Restrictions.DenyPermissions = []SubjectPermissionEntry{{Endpoint: "GET /v1/other"}}
		}, "DenyPermissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newBoundTestStore(t)
			minter := restrictedParent()
			req := ChildKeyRequest(minter, "child", nil)
			tc.edit(&minter, &req)
			req.MintedBy = &minter
			_, _, err := store.Create(context.Background(), req)
			if !errors.Is(err, ErrMintExceedsCaller) {
				t.Fatalf("Create = %v, want ErrMintExceedsCaller (%s must be bounded by the minter)", err, tc.field)
			}
		})
	}
}

// TestMintedKeyWithinMinter_AdminIssuanceStillWorks: an unrestricted operator still issues a
// customer key on another identifier, which comes out full-access with no allowlist.
func TestMintedKeyWithinMinter_AdminIssuanceStillWorks(t *testing.T) {
	store, validator := newBoundTestStore(t)
	op := Subject{KeyID: "kid_op", Identifier: "staff", Tier: TierOperator, AllowAllPermissions: true}
	_, plain, err := store.Create(context.Background(), CreateAPIKeyRequest{
		MintedBy: &op, Identifier: "acct-customer", Tier: TierAPIKey, RateLimitPerMin: 600,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := validator.Lookup(context.Background(), plain)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Identifier != "acct-customer" || !got.AllowAllPermissions || len(got.IPAllowlist) != 0 {
		t.Fatalf("customer key = %+v, want acct-customer full-access with no allowlist", got)
	}
}

// TestRevokingMinterRevokesDelegatedKeys: a key delegated from a revoked key (at any depth) stops
// authenticating with it, while an unrelated key on the same identifier survives.
func TestRevokingMinterRevokesDelegatedKeys(t *testing.T) {
	store, validator := newBoundTestStore(t)
	ctx := context.Background()
	mint := func(req CreateAPIKeyRequest) (APIKeyRecord, string, Subject) {
		t.Helper()
		rec, plain, err := store.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		sub, err := validator.Lookup(ctx, plain)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		return rec, plain, sub
	}
	root, _, rootSub := mint(CreateAPIKeyRequest{Identifier: "acct-x"})
	_, _, sibling := mint(CreateAPIKeyRequest{Identifier: "acct-x"})
	_, childPlain, childSub := mint(ChildKeyRequest(rootSub, "child", nil))
	_, grandPlain, _ := mint(ChildKeyRequest(childSub, "grandchild", nil))
	_, siblingChildPlain, _ := mint(ChildKeyRequest(sibling, "other", nil))

	if err := store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	for name, plain := range map[string]string{"child": childPlain, "grandchild": grandPlain} {
		if _, err := validator.Lookup(ctx, plain); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s of a revoked key: Lookup err = %v, want ErrUnauthorized", name, err)
		}
	}
	if _, err := validator.Lookup(ctx, siblingChildPlain); err != nil {
		t.Errorf("key delegated from an unrevoked sibling: Lookup err = %v, want it to survive", err)
	}
}

// TestMintBoundRules_CoverEveryRecordField fails when APIKeyRecord gains a field without a
// minter-bound rule, so a new dimension cannot reach a minted key unbounded.
func TestMintBoundRules_CoverEveryRecordField(t *testing.T) {
	rt := reflect.TypeFor[APIKeyRecord]()
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if _, ok := mintBoundRules[name]; !ok {
			t.Errorf("APIKeyRecord.%s has no entry in mintBoundRules: state how a minted key's %s is bounded by its minter", name, name)
		}
	}
	for name := range mintBoundRules {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("mintBoundRules names %q, which APIKeyRecord no longer has", name)
		}
	}
}
