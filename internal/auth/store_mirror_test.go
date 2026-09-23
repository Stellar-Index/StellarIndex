package auth

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestCreateWithSecret_RoundTripsThroughTheValidator is the audit
// 2026-08-13 F1/F11 regression, and the ONE test that would have
// caught both this defect and the v0.32.0 one before it.
//
// Every prior test proved a component: the handler's Postgres row was
// right, and the mirror received the right STRUCT. Neither could see
// that the record the DEPLOYED validator reads had no monthly quota —
// so /v1/register advertised a 1,000,000/month cap on keys that were
// completely unmetered in production. This drives the real store into
// the real validator and asserts on the Subject the middleware
// actually consumes.
func TestCreateWithSecret_RoundTripsThroughTheValidator(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewRedisAPIKeyStore(rdb)
	const plaintext = "sip_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	want := MirroredKey{
		Plaintext: plaintext,
		Record: APIKeyRecord{
			KeyID:           "kid_abc123",
			Identifier:      AccountIdentifier("reg-test"),
			Label:           "registration key",
			RateLimitPerMin: 1000,
			MonthlyQuota:    1_000_000,
			PermissionsAll:  true,
		},
	}
	if err := store.CreateWithSecret(context.Background(), want); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	validator := NewRedisAPIKeyValidator(rdb)
	sub, err := validator.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Lookup after mirror: %v", err)
	}

	// Assert against LITERALS, never against `want`'s own fields: an
	// input-vs-output comparison passes when both sides are the zero
	// value, which is precisely the bug (a dropped field reads as 0 on
	// both sides). The tautological version of this test would have
	// shipped the defect it exists to catch.
	if sub.MonthlyQuota != 1_000_000 {
		t.Errorf("MonthlyQuota = %d, want 1000000 — a quota of 0 means UNMETERED (the middleware short-circuits at <= 0) on a surface that advertises a cap",
			sub.MonthlyQuota)
	}
	if sub.RateLimitPerMin != 1000 {
		t.Errorf("RateLimitPerMin = %d, want 1000", sub.RateLimitPerMin)
	}
	if sub.KeyID != "kid_abc123" {
		t.Errorf("KeyID = %q, want kid_abc123 (revocation + listings key off this)", sub.KeyID)
	}
	if sub.Identifier != AccountIdentifier("reg-test") {
		t.Errorf("Identifier = %q, want %q (the tier-clamp fan-out finds keys by this)",
			sub.Identifier, AccountIdentifier("reg-test"))
	}
}

// gatedManagementRow is a dashboard-shaped management row carrying every
// gate the dashboard collects, none of them the permissive default.
func gatedManagementRow(expiresAt time.Time) platform.APIKey {
	return platform.APIKey{
		ID:               "kid_gated01",
		Name:             "dashboard key",
		Tier:             platform.APIKeyTierAPIKey,
		RateLimitPerMin:  60,
		MonthlyQuota:     5_000,
		Scopes:           []string{"read:prices"},
		IPAllowlist:      []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		RefererAllowlist: []string{"https://app.example.com"},
		ExpiresAt:        expiresAt,
		Permissions: platform.KeyPermissions{
			All:   false,
			Allow: []platform.KeyPermissionEntry{{Endpoint: "/v1/price"}},
			Deny:  []platform.KeyPermissionEntry{{EndpointPrefix: "/v1/admin"}},
		},
	}
}

// TestCreateWithSecret_MirrorsEveryGateOfTheManagementRow pins GH-1318:
// the mirror is the seam for writing a Postgres-minted key into the Redis
// validator store, so it must carry every gate on the management row. The
// old MirroredKey had no field for scopes, expiry, IP/referer allowlists or
// permission entries and hardcoded PermissionsAll=true, so a gated key came
// back as an unrestricted one.
func TestCreateWithSecret_MirrorsEveryGateOfTheManagementRow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const plaintext = "sip_" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	expiresAt := time.Now().Add(time.Hour).UTC()
	rec, err := APIKeyRecordFromPlatform(gatedManagementRow(expiresAt), AccountIdentifier("dash-acct"))
	if err != nil {
		t.Fatalf("APIKeyRecordFromPlatform: %v", err)
	}
	if err := NewRedisAPIKeyStore(rdb).CreateWithSecret(context.Background(), MirroredKey{Plaintext: plaintext, Record: rec}); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	sub, err := NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.AllowAllPermissions {
		t.Error("AllowAllPermissions = true, want false: the mirror widened a restricted key to full access")
	}
	if len(sub.AllowPermissions) != 1 || sub.AllowPermissions[0].Endpoint != "/v1/price" {
		t.Errorf("AllowPermissions = %+v, want exactly /v1/price", sub.AllowPermissions)
	}
	if len(sub.DenyPermissions) != 1 || sub.DenyPermissions[0].EndpointPrefix != "/v1/admin" {
		t.Errorf("DenyPermissions = %+v, want exactly prefix /v1/admin", sub.DenyPermissions)
	}
	if !slices.Equal(sub.Scopes, []string{"read:prices"}) {
		t.Errorf("Scopes = %v, want [read:prices]", sub.Scopes)
	}
	if len(sub.IPAllowlist) != 1 || sub.IPAllowlist[0] != netip.MustParsePrefix("203.0.113.0/24") {
		t.Errorf("IPAllowlist = %v, want [203.0.113.0/24]", sub.IPAllowlist)
	}
	if !slices.Equal(sub.RefererAllowlist, []string{"https://app.example.com"}) {
		t.Errorf("RefererAllowlist = %v, want [https://app.example.com]", sub.RefererAllowlist)
	}
	if sub.MonthlyQuota != 5_000 || sub.RateLimitPerMin != 60 {
		t.Errorf("MonthlyQuota/RateLimitPerMin = %d/%d, want 5000/60", sub.MonthlyQuota, sub.RateLimitPerMin)
	}

	later := NewRedisAPIKeyValidator(rdb, WithClock(func() time.Time { return expiresAt.Add(time.Minute) }))
	if _, err := later.Lookup(context.Background(), plaintext); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Lookup after ExpiresAt: err = %v, want ErrTokenExpired (expiry was dropped by the mirror)", err)
	}
}

// TestAPIKeyRecordFromPlatform_RefusesUnresolvedQuota: on the management
// row MonthlyQuota 0 means "inherit from plan", but the Redis record reads
// 0 as unmetered, so mapping it verbatim would ship an uncapped key.
func TestAPIKeyRecordFromPlatform_RefusesUnresolvedQuota(t *testing.T) {
	row := gatedManagementRow(time.Time{})
	row.MonthlyQuota = 0
	if _, err := APIKeyRecordFromPlatform(row, AccountIdentifier("dash-acct")); err == nil {
		t.Fatal("APIKeyRecordFromPlatform accepted MonthlyQuota 0; want an error (0 is unmetered on the Redis record)")
	}
}
