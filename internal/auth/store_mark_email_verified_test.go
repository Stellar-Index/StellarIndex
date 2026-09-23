package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestMarkEmailVerified_HappyPath — flips a freshly-minted
// key's EmailVerifiedAt to the supplied timestamp and the
// updated record round-trips through a subsequent SCAN. F-1218
// wave 45 (codex audit-2026-05-12).
func TestMarkEmailVerified_HappyPath(t *testing.T) {
	store, _, now := newTestStore(t)
	ctx := context.Background()
	rec, _, err := store.Create(ctx, CreateAPIKeyRequest{
		Identifier:      "signup-test123",
		Tier:            TierAPIKey,
		RateLimitPerMin: 1000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !rec.EmailVerifiedAt.IsZero() {
		t.Fatalf("seed EmailVerifiedAt = %v, want zero", rec.EmailVerifiedAt)
	}

	at := now.Add(time.Hour)
	updated, err := store.MarkEmailVerified(ctx, rec.KeyID, at)
	if err != nil {
		t.Fatalf("MarkEmailVerified: %v", err)
	}
	if !updated.EmailVerifiedAt.Equal(at) {
		t.Errorf("EmailVerifiedAt = %v, want %v", updated.EmailVerifiedAt, at)
	}

	// Idempotent re-mark with a different timestamp updates the
	// stamp without erroring (the verify handler relies on this
	// when the customer clicks the link twice in 24h).
	at2 := now.Add(2 * time.Hour)
	updated, err = store.MarkEmailVerified(ctx, rec.KeyID, at2)
	if err != nil {
		t.Fatalf("MarkEmailVerified second: %v", err)
	}
	if !updated.EmailVerifiedAt.Equal(at2) {
		t.Errorf("EmailVerifiedAt re-mark = %v, want %v", updated.EmailVerifiedAt, at2)
	}
}

// TestMarkEmailVerified_NotFound — a typo'd or absent KeyID
// returns ErrKeyNotFound (matches the UpdateRateLimit shape so
// downstream callers can errors.Is uniformly).
func TestMarkEmailVerified_NotFound(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.MarkEmailVerified(context.Background(), "kid_definitely_not_real", time.Now())
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("err = %v, want ErrKeyNotFound", err)
	}
}

// TestMarkEmailVerified_ZeroAtUsesNow — passing a zero `at`
// causes the store to stamp `now()`, matching the verify-handler
// production wiring (which passes time.Time{} so the customer
// sees "verified at <now>").
func TestMarkEmailVerified_ZeroAtUsesNow(t *testing.T) {
	store, _, fixedNow := newTestStore(t)
	ctx := context.Background()
	rec, _, err := store.Create(ctx, CreateAPIKeyRequest{
		Identifier:      "signup-zeroAt",
		Tier:            TierAPIKey,
		RateLimitPerMin: 1000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := store.MarkEmailVerified(ctx, rec.KeyID, time.Time{})
	if err != nil {
		t.Fatalf("MarkEmailVerified: %v", err)
	}
	if !updated.EmailVerifiedAt.Equal(fixedNow.UTC()) {
		t.Errorf("EmailVerifiedAt = %v, want fixedNow %v", updated.EmailVerifiedAt, fixedNow.UTC())
	}
}

// TestMarkEmailVerified_RejectsEmptyKeyID — defence-in-depth.
func TestMarkEmailVerified_RejectsEmptyKeyID(t *testing.T) {
	store, _, _ := newTestStore(t)
	if _, err := store.MarkEmailVerified(context.Background(), "", time.Now()); err == nil {
		t.Error("expected error for empty keyID")
	}
}

// TestMarkEmailVerified_PreservesMirroredKeyTTL is the Q186 regression:
// the /v1/signup/verify handler calls this on a register-mirrored key,
// which is written with the sliding idle TTL (MirroredKeyIdleTTL). Pre-fix
// the write-back did `SET ... 0`, clearing that TTL — so the customer's
// FIRST click on the verification link turned their open-registration key
// permanent, defeating the idle-expiry bound.
func TestMarkEmailVerified_PreservesMirroredKeyTTL(t *testing.T) {
	store, mr, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateWithSecret(ctx, MirroredKey{
		Plaintext:  "sip_mirrored_verify",
		KeyID:      "kid_mirrored_verify",
		Identifier: AccountIdentifier("acme"),
	}); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	hash := HashAPIKey("sip_mirrored_verify")
	recordKey := cachekeys.APIKey(hash).String()
	before := mr.TTL(recordKey)
	if before <= 0 {
		t.Fatalf("mirrored record TTL before verify = %v, want > 0 (MirroredKeyIdleTTL)", before)
	}

	if _, err := store.MarkEmailVerified(ctx, "kid_mirrored_verify", time.Time{}); err != nil {
		t.Fatalf("MarkEmailVerified: %v", err)
	}

	after := mr.TTL(recordKey)
	if after <= 0 {
		t.Fatalf("mirrored record TTL after MarkEmailVerified = %v, want > 0 (TTL must survive the write-back, not be cleared to persistent)", after)
	}
}
