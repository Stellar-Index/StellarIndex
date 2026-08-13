package auth

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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
		Plaintext:       plaintext,
		KeyID:           "kid_abc123",
		Identifier:      AccountIdentifier("reg-test"),
		Label:           "registration key",
		RateLimitPerMin: 1000,
		MonthlyQuota:    1_000_000,
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
