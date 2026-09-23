package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// newTestStore lives in store_redis_test.go; reuse its
// (store, miniredis, anchor-time) shape and discard what we
// don't need.

func TestUpdateRateLimit_HappyPath(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()

	rec, _, err := store.Create(ctx, CreateAPIKeyRequest{
		Identifier:      "customer-acme",
		Label:           "production",
		Tier:            TierAPIKey,
		RateLimitPerMin: 1000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := store.UpdateRateLimit(ctx, rec.KeyID, 10000)
	if err != nil {
		t.Fatalf("UpdateRateLimit: %v", err)
	}
	if updated.KeyID != rec.KeyID {
		t.Errorf("KeyID = %q, want %q", updated.KeyID, rec.KeyID)
	}
	if updated.RateLimitPerMin != 10000 {
		t.Errorf("RateLimitPerMin = %d, want 10000", updated.RateLimitPerMin)
	}
	// Other fields preserved.
	if updated.Identifier != rec.Identifier {
		t.Errorf("Identifier mutated: %q != %q", updated.Identifier, rec.Identifier)
	}
	if updated.Label != rec.Label {
		t.Errorf("Label mutated: %q != %q", updated.Label, rec.Label)
	}
	if !updated.CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("CreatedAt mutated: %v != %v", updated.CreatedAt, rec.CreatedAt)
	}
}

func TestUpdateRateLimit_NotFound(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.UpdateRateLimit(context.Background(), "kid_definitely_not_real", 5000)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestUpdateRateLimit_FindsCorrectKeyAmongMultiple(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()

	r1, _, _ := store.Create(ctx, CreateAPIKeyRequest{Identifier: "c1", Tier: TierAPIKey, RateLimitPerMin: 1000})
	r2, _, _ := store.Create(ctx, CreateAPIKeyRequest{Identifier: "c2", Tier: TierAPIKey, RateLimitPerMin: 1000})
	r3, _, _ := store.Create(ctx, CreateAPIKeyRequest{Identifier: "c3", Tier: TierAPIKey, RateLimitPerMin: 1000})

	// Lift r2 only.
	updated, err := store.UpdateRateLimit(ctx, r2.KeyID, 50000)
	if err != nil {
		t.Fatalf("UpdateRateLimit: %v", err)
	}
	if updated.KeyID != r2.KeyID {
		t.Errorf("returned KeyID = %q, want %q", updated.KeyID, r2.KeyID)
	}
	if updated.RateLimitPerMin != 50000 {
		t.Errorf("RateLimitPerMin = %d, want 50000", updated.RateLimitPerMin)
	}
	if updated.Identifier != "c2" {
		t.Errorf("Identifier = %q, want c2", updated.Identifier)
	}

	// Verify r1 and r3 are untouched by running a no-op
	// UpdateRateLimit (set to their current value) which returns
	// the record. If they'd been mutated by the r2 update, this
	// would surface.
	for _, kid := range []string{r1.KeyID, r3.KeyID} {
		got, err := store.UpdateRateLimit(ctx, kid, 1000)
		if err != nil {
			t.Errorf("re-read %s: %v", kid, err)
			continue
		}
		if got.RateLimitPerMin != 1000 {
			t.Errorf("%s RateLimitPerMin = %d, want 1000 (untouched)", kid, got.RateLimitPerMin)
		}
	}
}

func TestUpdateRateLimit_RejectsNegative(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()
	r1, _, _ := store.Create(ctx, CreateAPIKeyRequest{Identifier: "c1", Tier: TierAPIKey, RateLimitPerMin: 1000})
	_, err := store.UpdateRateLimit(ctx, r1.KeyID, -1)
	if err == nil {
		t.Errorf("expected error for negative rate-limit, got nil")
	}
}

func TestUpdateRateLimit_RejectsEmptyKeyID(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.UpdateRateLimit(context.Background(), "", 5000)
	if err == nil {
		t.Errorf("expected error for empty keyID, got nil")
	}
}

// TestUpdateRateLimit_PreservesMirroredKeyTTL is the Q186 regression: a
// register-mirrored record is written with the sliding idle TTL
// (MirroredKeyIdleTTL) so an abandoned open-registration key ages out of
// Redis. Pre-fix, the read-modify-write did `SET ... 0`, which clears any
// existing TTL — so the FIRST rate-limit change on such a key silently
// turned it permanent, defeating the idle-expiry bound entirely.
func TestUpdateRateLimit_PreservesMirroredKeyTTL(t *testing.T) {
	store, mr, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateWithSecret(ctx, MirroredKey{
		Plaintext:  "sip_mirrored_secret",
		KeyID:      "kid_mirrored",
		Identifier: AccountIdentifier("acme"),
	}); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	hash := HashAPIKey("sip_mirrored_secret")
	recordKey := cachekeys.APIKey(hash).String()
	before := mr.TTL(recordKey)
	if before <= 0 {
		t.Fatalf("mirrored record TTL before update = %v, want > 0 (MirroredKeyIdleTTL)", before)
	}

	if _, err := store.UpdateRateLimit(ctx, "kid_mirrored", 5000); err != nil {
		t.Fatalf("UpdateRateLimit: %v", err)
	}

	after := mr.TTL(recordKey)
	if after <= 0 {
		t.Fatalf("mirrored record TTL after UpdateRateLimit = %v, want > 0 (TTL must survive the write-back, not be cleared to persistent)", after)
	}
}
