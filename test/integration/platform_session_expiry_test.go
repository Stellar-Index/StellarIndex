//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestSessionByTokenHash_RejectsExpired pins GH-1302: the authentication-path
// lookup itself refuses an expired-but-unrevoked session. Expired rows persist
// until the reaper's grace window passes, so leaving expiry to one caller-side
// `if` made every other consumer of this lookup an authentication bypass.
func TestSessionByTokenHash_RejectsExpired(t *testing.T) {
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
		Name: "Expiry Co", Slug: "expiry-co", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(store)
	user, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "owner@expiry.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	mint := func(token string, expiresAt time.Time) []byte {
		t.Helper()
		hash := sha256.Sum256([]byte(token))
		ip := net.ParseIP("203.0.113.7")
		if _, err := users.CreateSession(ctx, platform.Session{
			UserID: user.ID, TokenHash: hash[:], ExpiresAt: expiresAt,
			IPFirstSeen: ip, IPLastSeen: ip, UserAgent: "test",
		}); err != nil {
			t.Fatalf("create session %s: %v", token, err)
		}
		return hash[:]
	}
	expired := mint("expired-cookie-token", time.Now().Add(-time.Minute))
	live := mint("live-cookie-token", time.Now().Add(time.Hour))

	if s, err := users.GetSessionByTokenHash(ctx, expired); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("expired, unrevoked session resolved on the auth path: session=%v err=%v", s.ID, err)
	}
	if _, err := users.GetSessionByTokenHash(ctx, live); err != nil {
		t.Errorf("live session must still resolve: %v", err)
	}
}

// TestRevokeOtherUserSessions_KeepsOnlyTheNamedSession pins the store half of
// "adding a passkey ends every other session": the kept session and other
// users' sessions stay live, every other session of the user is revoked.
// It then exercises CapUserSessions on the same fixture.
func TestRevokeOtherUserSessions_KeepsOnlyTheNamedSession(t *testing.T) {
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
		Name: "Revoke Co", Slug: "revoke-co", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(store)
	newUser := func(email string, role platform.Role) platform.User {
		t.Helper()
		u, err := users.CreateUser(ctx, platform.User{AccountID: acct.ID, Email: email, Role: role})
		if err != nil {
			t.Fatalf("create user %s: %v", email, err)
		}
		return u
	}
	owner := newUser("owner@revoke.example", platform.RoleOwner)
	member := newUser("member@revoke.example", platform.RoleMember)

	mint := func(u platform.User, token string) (platform.Session, []byte) {
		t.Helper()
		hash := sha256.Sum256([]byte(token))
		ip := net.ParseIP("203.0.113.7")
		s, err := users.CreateSession(ctx, platform.Session{
			UserID: u.ID, TokenHash: hash[:], ExpiresAt: time.Now().Add(time.Hour),
			IPFirstSeen: ip, IPLastSeen: ip, UserAgent: "test",
		})
		if err != nil {
			t.Fatalf("create session %s: %v", token, err)
		}
		return s, hash[:]
	}
	kept, keptHash := mint(owner, "kept-cookie-token")
	_, otherHash := mint(owner, "other-cookie-token")
	_, memberHash := mint(member, "member-cookie-token")

	for range 2 { // idempotent
		if err := users.RevokeOtherUserSessions(ctx, owner.ID, kept.ID); err != nil {
			t.Fatalf("RevokeOtherUserSessions: %v", err)
		}
	}
	if _, err := users.GetSessionByTokenHash(ctx, keptHash); err != nil {
		t.Errorf("kept session must still resolve: %v", err)
	}
	if _, err := users.GetSessionByTokenHash(ctx, otherHash); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("the user's other session still resolves: err=%v", err)
	}
	if _, err := users.GetSessionByTokenHash(ctx, memberHash); err != nil {
		t.Errorf("another user's session must be untouched: %v", err)
	}

	// CapUserSessions: with the kept session plus three newer others live,
	// a cap of 3 keeps the kept one and the two newest, revoking the oldest.
	var hashes [][]byte
	for _, tok := range []string{"cap-a", "cap-b", "cap-c"} {
		_, h := mint(owner, tok)
		hashes = append(hashes, h)
	}
	for range 2 { // idempotent
		if err := users.CapUserSessions(ctx, owner.ID, kept.ID, 3); err != nil {
			t.Fatalf("CapUserSessions: %v", err)
		}
	}
	if _, err := users.GetSessionByTokenHash(ctx, keptHash); err != nil {
		t.Errorf("kept session must survive the cap: %v", err)
	}
	if _, err := users.GetSessionByTokenHash(ctx, hashes[0]); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("oldest session beyond the cap still resolves: err=%v", err)
	}
	for _, h := range hashes[1:] {
		if _, err := users.GetSessionByTokenHash(ctx, h); err != nil {
			t.Errorf("newest sessions within the cap must survive: %v", err)
		}
	}
	if _, err := users.GetSessionByTokenHash(ctx, memberHash); err != nil {
		t.Errorf("another user's session must be untouched by the cap: %v", err)
	}
}
