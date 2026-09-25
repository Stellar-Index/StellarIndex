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
