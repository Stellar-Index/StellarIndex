//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// The stored WebAuthn signature counter is the only clone-detection
// signal, so it is a high-water mark: two overlapping login ceremonies
// both verify against the same stored value and may commit in either
// order. A late-committing lower count must not lower it, while
// last_used_at still records the latest write.
func TestWebAuthnSignCountNeverDecreases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	acct, err := postgresstore.NewAccountStore(store).Create(ctx, platform.Account{
		Name: "Counter Co", Slug: "counter-" + strings.ToLower(uuid.New().String()[:8]),
		BillingEmail: "counter-" + uuid.New().String() + "@p.example",
		Tier:         platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := postgresstore.NewUserStore(store).CreateUser(ctx, platform.User{
		AccountID: acct.ID,
		Email:     "counter-" + uuid.New().String() + "@p.example",
		Role:      platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	passkeys := postgresstore.NewWebAuthnCredentialStore(store)
	credID := []byte("cred-" + uuid.New().String())
	created, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
		UserID:          user.ID,
		Name:            "YubiKey",
		CredentialID:    credID,
		PublicKey:       []byte{0xA5, 0x01, 0x02, 0x03, 0x26},
		AttestationType: "none",
		Transports:      []string{"usb"},
		SignCount:       100,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}

	assertStored := func(t *testing.T, wantCount int64, wantUsed time.Time) {
		t.Helper()
		got, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, credID)
		if err != nil {
			t.Fatalf("get credential: %v", err)
		}
		if got.SignCount != wantCount {
			t.Errorf("SignCount = %d, want %d", got.SignCount, wantCount)
		}
		if !got.LastUsedAt.Equal(wantUsed) {
			t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, wantUsed)
		}
	}

	base := time.Now().UTC().Truncate(time.Microsecond)

	// Ceremony B (count 102) commits first, then ceremony A (count 101).
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 102, base); err != nil {
		t.Fatalf("update 102: %v", err)
	}
	assertStored(t, 102, base)

	later := base.Add(time.Second)
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 101, later); err != nil {
		t.Fatalf("update 101: %v", err)
	}
	assertStored(t, 102, later)

	// An authenticator that reports 0 (counter unsupported) must not
	// erase the high-water mark either.
	latest := later.Add(time.Second)
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 0, latest); err != nil {
		t.Fatalf("update 0: %v", err)
	}
	assertStored(t, 102, latest)

	// A genuinely higher count still advances it.
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 103, latest); err != nil {
		t.Fatalf("update 103: %v", err)
	}
	assertStored(t, 103, latest)
}
