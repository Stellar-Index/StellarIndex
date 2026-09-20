//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Q148 (audit-2026-09-18) — PATCH /v1/admin/accounts/{id} previously
// did Get -> mutate in memory -> Update with no lock and no version
// check. Update rewrites every mutable column (not a diff), so two
// concurrent PATCHes on the SAME account race: whichever commits
// second silently discards the first's change. Status is the operator
// kill switch (C3-010) — a lost SUSPEND under this race is a live
// hole.
//
// This proves AccountStore.UpdateAtomic serialises the race: one
// PATCH raises the rate-limit override, a concurrent one suspends the
// account, and BOTH must land — a lost update (pre-fix Get+Update)
// would show one field reverted to its pre-race value.
func TestAccountStoreUpdateAtomic_SerialisesConcurrentPatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accounts := postgresstore.NewAccountStore(postgresstore.New(db))

	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "race-account",
		Slug:         "race-account",
		BillingEmail: "race@example.com",
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)

	// Racer A: raises the rate-limit override.
	go func() {
		defer wg.Done()
		_, _, err := accounts.UpdateAtomic(ctx, acct.ID, func(a *platform.Account) error {
			a.RateLimitPerMinOverride = 500
			return nil
		})
		errs <- err
	}()

	// Racer B: suspends the account (the kill switch).
	go func() {
		defer wg.Done()
		_, _, err := accounts.UpdateAtomic(ctx, acct.ID, func(a *platform.Account) error {
			a.Status = platform.AccountSuspended
			a.SuspendedReason = "race-test"
			return nil
		})
		errs <- err
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpdateAtomic racer failed: %v", err)
		}
	}

	final, err := accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get final account: %v", err)
	}
	if final.RateLimitPerMinOverride != 500 {
		t.Errorf("RateLimitPerMinOverride = %d, want 500 (Racer A's write was lost)", final.RateLimitPerMinOverride)
	}
	if final.Status != platform.AccountSuspended {
		t.Errorf("Status = %q, want %q (Racer B's write was lost — the kill switch did not stick)",
			final.Status, platform.AccountSuspended)
	}
}

// TestAccountStoreUpdateAtomic_NotFound — the id-absent path returns
// platform.ErrNotFound unwrapped, matching Get/Update's existing
// contract, and never begins a mutate call.
func TestAccountStoreUpdateAtomic_NotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accounts := postgresstore.NewAccountStore(postgresstore.New(db))

	mutateCalled := false
	_, _, err = accounts.UpdateAtomic(ctx, uuid.New(), func(a *platform.Account) error {
		mutateCalled = true
		return nil
	})
	if !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("err = %v, want platform.ErrNotFound", err)
	}
	if mutateCalled {
		t.Error("mutate was called for an absent account id")
	}
}
