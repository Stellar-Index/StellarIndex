//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// A passkey signup binds the user ID into the credential before the row
// exists, so CreateUser must keep a caller-chosen ID, still generate one
// when none is given, and refuse a reused ID as a conflict.
func TestCreateUserHonoursCallerID(t *testing.T) {
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
		Name: "Fixed ID Co", Slug: "fixed-" + strings.ToLower(uuid.New().String()[:8]),
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(store)

	want := uuid.New()
	got, err := users.CreateUser(ctx, platform.User{
		ID: want, AccountID: acct.ID, Email: platform.PlaceholderEmail(want), Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user with id: %v", err)
	}
	if got.ID != want {
		t.Fatalf("user id = %s, want the caller's %s", got.ID, want)
	}

	generated, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "gen-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
	})
	if err != nil || generated.ID == uuid.Nil {
		t.Fatalf("create user without id: id=%s err=%v", generated.ID, err)
	}

	_, err = users.CreateUser(ctx, platform.User{
		ID: want, AccountID: acct.ID, Email: "other-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
	})
	if !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("reused id: err = %v, want ErrConflict", err)
	}
}
