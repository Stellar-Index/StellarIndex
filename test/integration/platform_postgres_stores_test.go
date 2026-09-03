//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
)

// TestPlatformPostgresStores exercises the AccountStore +
// UserStore + TokenStore implementations against the schema
// from migration 0027. One container per test (no shared
// fixture) per the existing storage-test convention.
func TestPlatformPostgresStores(t *testing.T) {
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
	accounts := postgresstore.NewAccountStore(store)
	users := postgresstore.NewUserStore(store)
	tokens := postgresstore.NewTokenStore(store)

	t.Run("Account/CRUD", func(t *testing.T) {
		acme, err := accounts.Create(ctx, platform.Account{
			Name:         "Acme Corp",
			Slug:         "acme",
			BillingEmail: "billing@acme.example",
			Tier:         platform.TierFree,
			Status:       platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if acme.ID == uuid.Nil {
			t.Fatal("ID not populated")
		}
		if acme.CreatedAt.IsZero() {
			t.Fatal("CreatedAt not populated")
		}

		// Get by id, slug.
		got, err := accounts.Get(ctx, acme.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Name != "Acme Corp" {
			t.Errorf("Name = %q", got.Name)
		}

		bySlug, err := accounts.GetBySlug(ctx, "acme")
		if err != nil {
			t.Fatalf("get by slug: %v", err)
		}
		if bySlug.ID != acme.ID {
			t.Errorf("slug lookup got different account")
		}

		// Update tier; verify.
		// Legacy tier in → CANONICAL tier out (free-platform pivot
		// 2026-08-10): the store persists a CHECK-legal legacy string
		// and canonicalises on read, so writing the deprecated `pro`
		// must round-trip as `partner`. Asserting `pro` back would pin
		// the pre-pivot behaviour.
		acme.Tier = platform.TierPro
		if err := accounts.Update(ctx, acme); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, err = accounts.Get(ctx, acme.ID)
		if err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if got.Tier != platform.TierPartner {
			t.Errorf("legacy tier %q did not canonicalise on read: got %q, want %q",
				platform.TierPro, got.Tier, platform.TierPartner)
		}

		// And a canonical tier round-trips unchanged.
		acme.Tier = platform.TierFree
		if err := accounts.Update(ctx, acme); err != nil {
			t.Fatalf("update (free): %v", err)
		}
		got, err = accounts.Get(ctx, acme.ID)
		if err != nil {
			t.Fatalf("get after update (free): %v", err)
		}
		if got.Tier != platform.TierFree {
			t.Errorf("canonical tier didn't persist: %q", got.Tier)
		}

		// Suspend → unsuspend (idempotency).
		if err := accounts.Suspend(ctx, acme.ID, "abuse"); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if err := accounts.Suspend(ctx, acme.ID, "abuse-again"); err != nil {
			t.Fatalf("suspend (idempotent): %v", err)
		}
		got, err = accounts.Get(ctx, acme.ID)
		if err != nil {
			t.Fatalf("get after suspend: %v", err)
		}
		if got.Status != platform.AccountSuspended {
			t.Errorf("not suspended: %q", got.Status)
		}
		if got.SuspendedAt.IsZero() {
			t.Errorf("SuspendedAt not stamped")
		}
		if got.SuspendedReason != "abuse-again" {
			t.Errorf("SuspendedReason = %q", got.SuspendedReason)
		}

		if err := accounts.Unsuspend(ctx, acme.ID); err != nil {
			t.Fatalf("unsuspend: %v", err)
		}
		got, err = accounts.Get(ctx, acme.ID)
		if err != nil {
			t.Fatalf("get after unsuspend: %v", err)
		}
		if got.Status != platform.AccountActive {
			t.Errorf("not active after unsuspend: %q", got.Status)
		}
		if !got.SuspendedAt.IsZero() {
			t.Errorf("SuspendedAt not cleared")
		}

		// Slug uniqueness → ErrConflict.
		_, err = accounts.Create(ctx, platform.Account{
			Name: "Acme 2", Slug: "acme",
			BillingEmail: "x@y.com",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("expected ErrConflict on duplicate slug, got %v", err)
		}

		// ErrNotFound on absent.
		if _, err := accounts.Get(ctx, uuid.New()); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Account/ReapSuspendedOrphans", func(t *testing.T) {
		// A childless orphan Suspended with the signup-race reason —
		// the only row that should be reaped.
		orphan, err := accounts.Create(ctx, platform.Account{
			Name: "race orphan", Slug: "race-orphan",
			BillingEmail: "orphan@race.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create orphan: %v", err)
		}
		if err := accounts.Suspend(ctx, orphan.ID, signupreaper.SignupRaceReasonPrefix+" orphan speculative account orphan@race.example"); err != nil {
			t.Fatalf("suspend orphan: %v", err)
		}

		// A signup-race-suspended account that DID get a user attached
		// — must survive (childless guard).
		withUser, err := accounts.Create(ctx, platform.Account{
			Name: "race with user", Slug: "race-with-user",
			BillingEmail: "winner@race.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create race-with-user: %v", err)
		}
		if _, err := users.CreateUser(ctx, platform.User{
			AccountID: withUser.ID, Email: "winner@race.example", Role: platform.RoleOwner,
		}); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := accounts.Suspend(ctx, withUser.ID, signupreaper.SignupRaceReasonPrefix+" orphan speculative account winner@race.example"); err != nil {
			t.Fatalf("suspend race-with-user: %v", err)
		}

		// A suspended account with a DIFFERENT reason — must survive
		// (reason gate).
		abuse, err := accounts.Create(ctx, platform.Account{
			Name: "abuse victim", Slug: "abuse-victim",
			BillingEmail: "abuse@x.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create abuse: %v", err)
		}
		if err := accounts.Suspend(ctx, abuse.ID, "abuse: fraud"); err != nil {
			t.Fatalf("suspend abuse: %v", err)
		}

		// Age gate: an olderThan in the past (before the just-stamped
		// suspended_at) reaps nothing.
		n, err := accounts.ReapSuspendedOrphans(ctx, signupreaper.SignupRaceReasonPrefix, time.Now().UTC().Add(-time.Hour))
		if err != nil {
			t.Fatalf("reap (age gate): %v", err)
		}
		if n != 0 {
			t.Errorf("age gate: reaped %d, want 0 (rows younger than olderThan must survive)", n)
		}
		if _, err := accounts.Get(ctx, orphan.ID); err != nil {
			t.Errorf("orphan removed by the age-gated reap: %v", err)
		}

		// Real reap: olderThan in the future makes the just-suspended
		// orphan eligible. Only the childless signup-race orphan goes.
		n, err = accounts.ReapSuspendedOrphans(ctx, signupreaper.SignupRaceReasonPrefix, time.Now().UTC().Add(time.Hour))
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if n != 1 {
			t.Fatalf("reaped %d, want exactly 1 (only the childless signup-race orphan)", n)
		}
		if _, err := accounts.Get(ctx, orphan.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("orphan not deleted: Get returned %v, want ErrNotFound", err)
		}
		if _, err := accounts.Get(ctx, withUser.ID); err != nil {
			t.Errorf("race-with-user wrongly reaped (childless guard failed): %v", err)
		}
		if _, err := accounts.Get(ctx, abuse.ID); err != nil {
			t.Errorf("abuse-victim wrongly reaped (reason gate failed): %v", err)
		}
	})

	t.Run("User/CRUD+sessions", func(t *testing.T) {
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Beta Co", Slug: "beta",
			BillingEmail: "b@beta.example",
			Tier:         platform.TierStarter, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}

		alice, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "alice@beta.example",
			Role:      platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		if alice.ID == uuid.Nil {
			t.Fatal("user ID not populated")
		}

		// Email lookup is case-insensitive (citext column).
		got, err := users.GetUserByEmail(ctx, "ALICE@BETA.EXAMPLE")
		if err != nil {
			t.Fatalf("get by email (case-insensitive): %v", err)
		}
		if got.ID != alice.ID {
			t.Errorf("citext lookup didn't match")
		}

		// Duplicate email → ErrConflict.
		_, err = users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "alice@beta.example",
			Role:      platform.RoleMember,
		})
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("expected ErrConflict on duplicate email, got %v", err)
		}

		// List users for account.
		_, err = users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "bob@beta.example",
			Role:      platform.RoleMember,
		})
		if err != nil {
			t.Fatalf("create bob: %v", err)
		}

		list, err := users.ListUsersForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("list len = %d, want 2", len(list))
		}

		// Session round-trip.
		ip := net.ParseIP("203.0.113.42")
		// W1-auth-passkey-2: the cookie carries a random token; the row
		// stores only sha256(token). Simulate mintSession's contract.
		sessTokenHash := sha256.Sum256([]byte("session-cookie-token-abc"))
		sess, err := users.CreateSession(ctx, platform.Session{
			UserID:       alice.ID,
			TokenHash:    sessTokenHash[:],
			ExpiresAt:    time.Now().Add(30 * 24 * time.Hour),
			IPFirstSeen:  ip,
			IPLastSeen:   ip,
			UserAgent:    "Mozilla/5.0",
			GeoFirstSeen: "US",
			GeoLastSeen:  "US",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}

		gotSess, err := users.GetSession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if gotSess.UserID != alice.ID {
			t.Errorf("session UserID = %v", gotSess.UserID)
		}

		// The stored token_hash is the hash we supplied — persisted and
		// scanned back verbatim.
		if !bytes.Equal(gotSess.TokenHash, sessTokenHash[:]) {
			t.Errorf("stored token_hash = %x, want %x", gotSess.TokenHash, sessTokenHash[:])
		}

		// Authentication path: lookup BY token hash resolves the row —
		// this is what resolveSession does with sha256(cookie).
		byHash, err := users.GetSessionByTokenHash(ctx, sessTokenHash[:])
		if err != nil {
			t.Fatalf("get session by token hash: %v", err)
		}
		if byHash.ID != sess.ID {
			t.Errorf("GetSessionByTokenHash returned session %v, want %v", byHash.ID, sess.ID)
		}

		// A read of the sessions table yields the PK + the hash, neither
		// of which is a presentable credential: hashing the primary key
		// string (what an attacker with a table dump would try) resolves
		// nothing, and a random other hash resolves nothing.
		pkAsCookieHash := sha256.Sum256([]byte(sess.ID.String()))
		if _, err := users.GetSessionByTokenHash(ctx, pkAsCookieHash[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("session PK (hashed) resolved as a token — raw-id replay path is live: err=%v", err)
		}
		otherHash := sha256.Sum256([]byte("not-the-token"))
		if _, err := users.GetSessionByTokenHash(ctx, otherHash[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("unknown token hash resolved a session: err=%v", err)
		}

		// Touch updates last_seen + ip_last + UA.
		newIP := net.ParseIP("203.0.113.99")
		if err := users.TouchSession(ctx, sess.ID, newIP, "curl/8"); err != nil {
			t.Fatalf("touch: %v", err)
		}
		gotSess, err = users.GetSession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get session after touch: %v", err)
		}
		if !gotSess.IPLastSeen.Equal(newIP) {
			t.Errorf("IPLastSeen = %v, want %v", gotSess.IPLastSeen, newIP)
		}
		if gotSess.UserAgent != "curl/8" {
			t.Errorf("UserAgent = %q", gotSess.UserAgent)
		}

		// Revoke → subsequent GetSession returns ErrNotFound.
		if err := users.RevokeSession(ctx, sess.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := users.GetSession(ctx, sess.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound after revoke, got %v", err)
		}
		// A revoked session must not resolve on the auth path either.
		if _, err := users.GetSessionByTokenHash(ctx, sessTokenHash[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("revoked session still resolved by token hash: %v", err)
		}

		// Re-revoke is a no-op.
		if err := users.RevokeSession(ctx, sess.ID); err != nil {
			t.Errorf("re-revoke: %v", err)
		}
	})

	t.Run("MagicLinkToken/lifecycle", func(t *testing.T) {
		hash := sha256.Sum256([]byte("token-1"))

		// Future expiry: consume succeeds.
		err := tokens.CreateMagicLinkToken(ctx, platform.MagicLinkToken{
			TokenHash:   hash[:],
			Email:       "user@example.com",
			Purpose:     platform.TokenPurposeLogin,
			ExpiresAt:   time.Now().Add(15 * time.Minute),
			RequestedIP: net.ParseIP("203.0.113.1"),
		})
		if err != nil {
			t.Fatalf("create token: %v", err)
		}

		got, err := tokens.ConsumeMagicLinkToken(ctx, hash[:])
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if got.Email != "user@example.com" {
			t.Errorf("email = %q", got.Email)
		}
		if got.ConsumedAt.IsZero() {
			t.Errorf("ConsumedAt not stamped")
		}

		// Second consume → ErrNotFound (already consumed).
		if _, err := tokens.ConsumeMagicLinkToken(ctx, hash[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound on re-consume, got %v", err)
		}

		// Expired token: classify as ErrTokenExpired.
		expHash := sha256.Sum256([]byte("expired-token"))
		err = tokens.CreateMagicLinkToken(ctx, platform.MagicLinkToken{
			TokenHash:   expHash[:],
			Email:       "user2@example.com",
			Purpose:     platform.TokenPurposeLogin,
			ExpiresAt:   time.Now().Add(-1 * time.Minute),
			RequestedIP: net.ParseIP("203.0.113.2"),
		})
		if err != nil {
			t.Fatalf("create expired token: %v", err)
		}
		_, err = tokens.ConsumeMagicLinkToken(ctx, expHash[:])
		if !errors.Is(err, platform.ErrTokenExpired) {
			t.Errorf("expected ErrTokenExpired, got %v", err)
		}

		// Missing token → ErrNotFound.
		nope := sha256.Sum256([]byte("never-existed"))
		if _, err := tokens.ConsumeMagicLinkToken(ctx, nope[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Invite/lifecycle", func(t *testing.T) {
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Invite Co", Slug: "invite-co-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "i@i.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}

		inviter, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "inviter-" + uuid.New().String() + "@x.example",
			Role:      platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create inviter: %v", err)
		}

		hash := sha256.Sum256([]byte("invite-1"))
		err = tokens.CreateInvite(ctx, platform.Invite{
			TokenHash:       hash[:],
			AccountID:       acct.ID,
			Email:           "newcomer@i.example",
			Role:            platform.RoleMember,
			InvitedByUserID: inviter.ID,
			ExpiresAt:       time.Now().Add(7 * 24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("create invite: %v", err)
		}

		// Pending list should include it.
		pending, err := tokens.ListInvitesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list invites: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("pending = %d, want 1", len(pending))
		}

		// Accept.
		got, err := tokens.AcceptInvite(ctx, hash[:])
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if got.AccountID != acct.ID || got.Email != "newcomer@i.example" {
			t.Errorf("invite shape mismatched: %+v", got)
		}

		// Pending list now empty.
		pending, err = tokens.ListInvitesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list invites after accept: %v", err)
		}
		if len(pending) != 0 {
			t.Errorf("pending after accept = %d, want 0", len(pending))
		}

		// Re-accept → ErrNotFound.
		if _, err := tokens.AcceptInvite(ctx, hash[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("re-accept: expected ErrNotFound, got %v", err)
		}

		// Revoke pre-accept (separate token).
		hash2 := sha256.Sum256([]byte("invite-2"))
		// Check the create error: a swallowed failure here made the
		// downstream ErrNotFound assertion pass for the WRONG reason —
		// row never created vs. row revoked (audit-2026-06-14 A20).
		if err := tokens.CreateInvite(ctx, platform.Invite{
			TokenHash:       hash2[:],
			AccountID:       acct.ID,
			Email:           "second@i.example",
			Role:            platform.RoleMember,
			InvitedByUserID: inviter.ID,
			ExpiresAt:       time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create invite 2: %v", err)
		}
		if err := tokens.RevokeInvite(ctx, hash2[:]); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		// Accepting a revoked invite → ErrNotFound.
		if _, err := tokens.AcceptInvite(ctx, hash2[:]); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("accept-after-revoke: expected ErrNotFound, got %v", err)
		}
	})

	// COR-15 (audit-2026-07-23): ListInvitesForAccount used to filter
	// on SQL `now()` (the Postgres server's clock) instead of the
	// injected [postgresstore.TokenStore.WithClock] like every other
	// expiry check in the file — so WithClock had NO effect on this
	// method despite the type's doc claiming tests use it, and no
	// test exercised WithClock at all. Asserts the corrected value: a
	// TokenStore whose injected clock has advanced past an invite's
	// expiry excludes it from the pending list, while the SAME invite
	// still appears through a real-clock store (real time hasn't
	// actually passed).
	t.Run("Invite/ListRespectsInjectedClock", func(t *testing.T) {
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Clocked Co", Slug: "clocked-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "c@c.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		inviter, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "clock-inviter-" + uuid.New().String() + "@x.example",
			Role:      platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create inviter: %v", err)
		}

		hash := sha256.Sum256([]byte("clock-invite-1"))
		expiresAt := time.Now().Add(1 * time.Hour) // not yet expired in real time
		if err := tokens.CreateInvite(ctx, platform.Invite{
			TokenHash:       hash[:],
			AccountID:       acct.ID,
			Email:           "clocked-newcomer@i.example",
			Role:            platform.RoleMember,
			InvitedByUserID: inviter.ID,
			ExpiresAt:       expiresAt,
		}); err != nil {
			t.Fatalf("create invite: %v", err)
		}

		// Real-clock store: real time hasn't passed the 1h expiry —
		// the invite is still pending.
		realClockPending, err := tokens.ListInvitesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list invites (real clock): %v", err)
		}
		if !containsInviteHash(realClockPending, hash[:]) {
			t.Fatal("real-clock store: expected the not-yet-expired invite to be pending")
		}

		// Clock-shifted store (same underlying Postgres connection):
		// injected "now" is 2h past creation, i.e. 1h past the
		// invite's expiry. If WithClock actually governs this query,
		// the invite must be excluded even though the DB's own
		// now() hasn't moved.
		futureClock := func() time.Time { return time.Now().Add(2 * time.Hour) }
		clockedTokens := postgresstore.NewTokenStore(store).WithClock(futureClock)
		clockedPending, err := clockedTokens.ListInvitesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list invites (shifted clock): %v", err)
		}
		if containsInviteHash(clockedPending, hash[:]) {
			t.Error("clock-shifted store: expected the invite to be excluded as expired per the injected clock, " +
				"but it was returned — ListInvitesForAccount is not honouring WithClock")
		}
	})

	t.Run("APIKey/CRUD+revoke+touch", func(t *testing.T) {
		keys := postgresstore.NewAPIKeyStore(store)

		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Keyed Co", Slug: "keyed-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "k@k.example",
			Tier:         platform.TierStarter, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		owner, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "owner-" + uuid.New().String() + "@k.example",
			Role:      platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create owner: %v", err)
		}

		hash := sha256.Sum256([]byte("sip_plaintext_xyz"))
		key := platform.APIKey{
			ID:              "kid_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12],
			AccountID:       acct.ID,
			CreatedByUserID: owner.ID,
			Name:            "primary",
			Description:     "production traffic",
			KeyHash:         hash[:],
			// `sip_` — the namespace every minter actually emits
			// (auth/store.go, dashboardkeys/handlers.go). This fixture
			// said `rek_` (the pre-rebrand namespace) and so matched
			// migration 0027's stale CHECK instead of production
			// reality, masking the fact that EVERY real key mint failed
			// a check_violation until migration 0133 (cold audit
			// 2026-08-03).
			KeyPrefix:              "sip_4f9c1d8b",
			Tier:                   platform.APIKeyTierAPIKey,
			RateLimitPerMin:        1000,
			MonthlyQuota:           500000,
			Permissions:            platform.KeyPermissions{All: true},
			RefererAllowlist:       []string{"https://example.com"},
			UsageAlertThresholdPct: 80,
		}
		// Add an IP allowlist entry to exercise cidr[] path.
		prefix, perr := netip.ParsePrefix("203.0.113.0/24")
		if perr != nil {
			t.Fatalf("parse prefix: %v", perr)
		}
		key.IPAllowlist = []netip.Prefix{prefix}

		out, err := keys.Create(ctx, key, 25)
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if out.CreatedAt.IsZero() {
			t.Error("CreatedAt not populated")
		}
		if out.AccountID != acct.ID {
			t.Errorf("AccountID round-trip: got %v want %v", out.AccountID, acct.ID)
		}
		if !out.Permissions.All {
			t.Errorf("Permissions.All didn't round-trip")
		}
		if len(out.IPAllowlist) != 1 || out.IPAllowlist[0].String() != "203.0.113.0/24" {
			t.Errorf("IPAllowlist round-trip: %+v", out.IPAllowlist)
		}
		if len(out.RefererAllowlist) != 1 || out.RefererAllowlist[0] != "https://example.com" {
			t.Errorf("RefererAllowlist round-trip: %+v", out.RefererAllowlist)
		}

		// Get by id, by hash.
		byID, err := keys.Get(ctx, key.ID)
		if err != nil {
			t.Fatalf("get by id: %v", err)
		}
		if byID.Name != "primary" {
			t.Errorf("Name = %q", byID.Name)
		}
		byHash, err := keys.GetByHash(ctx, hash[:])
		if err != nil {
			t.Fatalf("get by hash: %v", err)
		}
		if byHash.ID != key.ID {
			t.Errorf("hash lookup got different key")
		}

		// List for account.
		list, err := keys.ListForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("list len = %d, want 1", len(list))
		}

		// Update: bump rate limit + add description.
		byID.RateLimitPerMin = 5000
		byID.Description = "production traffic — bumped"
		if err := keys.Update(ctx, byID); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, err := keys.Get(ctx, byID.ID)
		if err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if got.RateLimitPerMin != 5000 {
			t.Errorf("RateLimitPerMin = %d", got.RateLimitPerMin)
		}
		if !strings.Contains(got.Description, "bumped") {
			t.Errorf("Description didn't persist")
		}

		// Touch usage.
		ip := net.ParseIP("198.51.100.7")
		if err := keys.TouchUsage(ctx, byID.ID, ip, "curl/8"); err != nil {
			t.Fatalf("touch: %v", err)
		}
		got, err = keys.Get(ctx, byID.ID)
		if err != nil {
			t.Fatalf("get after touch: %v", err)
		}
		if got.LastUsedAt.IsZero() {
			t.Errorf("LastUsedAt not stamped")
		}
		if !got.LastUsedIP.Equal(ip) {
			t.Errorf("LastUsedIP = %v", got.LastUsedIP)
		}

		// Revoke + idempotency.
		if err := keys.Revoke(ctx, byID.ID, owner.ID, "rotated"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		got, err = keys.Get(ctx, byID.ID)
		if err != nil {
			t.Fatalf("get after revoke: %v", err)
		}
		if got.RevokedAt.IsZero() {
			t.Errorf("RevokedAt not stamped")
		}
		if got.IsActive(time.Now()) {
			t.Errorf("IsActive returned true on revoked key")
		}
		if err := keys.Revoke(ctx, byID.ID, owner.ID, "still rotated"); err != nil {
			t.Errorf("re-revoke: %v", err)
		}

		// Hash-collision (re-Create same hash) → ErrConflict.
		dup := key
		dup.ID = "kid_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
		_, err = keys.Create(ctx, dup, 25)
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("expected ErrConflict on duplicate hash, got %v", err)
		}

		// ErrNotFound on absent.
		if _, err := keys.Get(ctx, "kid_nonexistent00"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("WebhookStore/CRUD+queue", func(t *testing.T) {
		webhooks := postgresstore.NewWebhookStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name:         "Hooked Co",
			Slug:         "hooked-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "hook-" + uuid.New().String() + "@h.example",
			Tier:         platform.TierStarter,
			Status:       platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create webhook-test account: %v", err)
		}

		// 1. Create + Get
		hash := sha256.Sum256([]byte("seekrit"))
		created, err := webhooks.CreateWebhook(ctx, platform.CustomerWebhook{
			AccountID:  acct.ID,
			Name:       "ops-slack",
			URL:        "https://hooks.slack.example/services/T/B/X",
			SecretHash: hash[:],
			Events: []string{
				string(platform.WebhookEventIncidentSEV1),
				string(platform.WebhookEventAnomalyFreeze),
			},
			Enabled: true,
		}, 10)
		if err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		if created.ID == uuid.Nil {
			t.Error("ID not populated on create")
		}
		got, err := webhooks.GetWebhook(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetWebhook: %v", err)
		}
		if got.URL != "https://hooks.slack.example/services/T/B/X" {
			t.Errorf("URL round-trip: %q", got.URL)
		}
		if len(got.Events) != 2 {
			t.Errorf("Events round-trip: %v", got.Events)
		}

		// 2. List
		listed, err := webhooks.ListWebhooksForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("ListWebhooksForAccount: %v", err)
		}
		if len(listed) != 1 {
			t.Errorf("expected 1 webhook in list, got %d", len(listed))
		}

		// 3. Update
		got.Name = "ops-slack-renamed"
		got.Enabled = false
		if err := webhooks.UpdateWebhook(ctx, got); err != nil {
			t.Fatalf("UpdateWebhook: %v", err)
		}
		after, err := webhooks.GetWebhook(ctx, got.ID)
		if err != nil {
			t.Fatalf("GetWebhook after update: %v", err)
		}
		if after.Name != "ops-slack-renamed" {
			t.Errorf("name not updated: %q", after.Name)
		}
		if after.Enabled {
			t.Errorf("enabled should be false after update")
		}

		// 4. EnqueueDelivery + ListPendingDeliveries
		err = webhooks.EnqueueDelivery(ctx, platform.WebhookDelivery{
			WebhookID:     created.ID,
			EventType:     string(platform.WebhookEventIncidentSEV1),
			Payload:       []byte(`{"incident_id":"abc","summary":"test"}`),
			NextAttemptAt: time.Now().UTC().Add(-time.Second), // due immediately
		})
		if err != nil {
			t.Fatalf("EnqueueDelivery: %v", err)
		}
		pending, err := webhooks.ListPendingDeliveries(ctx, 10)
		if err != nil {
			t.Fatalf("ListPendingDeliveries: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("expected 1 pending delivery, got %d", len(pending))
		}
		if pending[0].EventType != string(platform.WebhookEventIncidentSEV1) {
			t.Errorf("EventType round-trip: %q", pending[0].EventType)
		}
		if pending[0].AttemptCount != 0 {
			t.Errorf("AttemptCount should start at 0, got %d", pending[0].AttemptCount)
		}

		// 5. MarkAttemptFailed bumps the counter and reschedules
		nextTry := time.Now().UTC().Add(2 * time.Minute)
		if err := webhooks.MarkAttemptFailed(ctx, pending[0].ID, "503 bad gateway", 503, nextTry); err != nil {
			t.Fatalf("MarkAttemptFailed: %v", err)
		}
		// Should no longer be in the due-now list.
		pending, err = webhooks.ListPendingDeliveries(ctx, 10)
		if err != nil {
			t.Fatalf("ListPendingDeliveries after retry: %v", err)
		}
		if len(pending) != 0 {
			t.Errorf("expected 0 pending (rescheduled to future), got %d", len(pending))
		}

		// 6. MarkDelivered closes the row out
		// (write a fresh delivery + immediately mark delivered)
		err = webhooks.EnqueueDelivery(ctx, platform.WebhookDelivery{
			WebhookID:     created.ID,
			EventType:     string(platform.WebhookEventAnomalyFreeze),
			Payload:       []byte(`{}`),
			NextAttemptAt: time.Now().UTC().Add(-time.Second),
		})
		if err != nil {
			t.Fatalf("EnqueueDelivery #2: %v", err)
		}
		pending, err = webhooks.ListPendingDeliveries(ctx, 10)
		if err != nil {
			t.Fatalf("ListPendingDeliveries (fresh): %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("expected 1 fresh pending delivery, got %d", len(pending))
		}
		if err := webhooks.MarkDelivered(ctx, pending[0].ID, 200); err != nil {
			t.Fatalf("MarkDelivered: %v", err)
		}
		// 7. ListDeliveries returns the full history including the just-marked one
		hist, err := webhooks.ListDeliveries(ctx, created.ID, 10)
		if err != nil {
			t.Fatalf("ListDeliveries: %v", err)
		}
		if len(hist) != 2 {
			t.Errorf("expected 2 attempts in history, got %d", len(hist))
		}

		// 8. Delete cascades to deliveries
		if err := webhooks.DeleteWebhook(ctx, created.ID); err != nil {
			t.Fatalf("DeleteWebhook: %v", err)
		}
		if _, err := webhooks.GetWebhook(ctx, created.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound on deleted webhook, got %v", err)
		}
		histAfter, err := webhooks.ListDeliveries(ctx, created.ID, 10)
		if err != nil {
			t.Fatalf("ListDeliveries after delete: %v", err)
		}
		if len(histAfter) != 0 {
			t.Errorf("expected deliveries cascade-deleted with webhook; got %d", len(histAfter))
		}
	})

	// F-1248 (codex audit-2026-05-12): per-account webhook quota
	// must hold under concurrent CreateWebhook calls. Pre-fix the
	// unlocked count-CTE allowed two snapshot-readers at n=cap-1
	// to both insert. The advisory-lock-wrapped transaction now
	// serialises them — verify here.
	t.Run("WebhookStore/Concurrent_QuotaCap_Holds", func(t *testing.T) {
		webhooks := postgresstore.NewWebhookStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "RaceCo", Slug: "race-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "race-" + uuid.New().String() + "@h.example",
			Tier:         platform.TierStarter, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		const (
			cap_       = 3
			goroutines = 10
		)
		var (
			wg        sync.WaitGroup
			ok        int64
			quotaErrs int64
			otherErrs int64
			start     = make(chan struct{})
			hash      = sha256.Sum256([]byte("seekrit"))
		)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				<-start
				_, err := webhooks.CreateWebhook(ctx, platform.CustomerWebhook{
					AccountID:  acct.ID,
					Name:       fmt.Sprintf("hook-%d", i),
					URL:        fmt.Sprintf("https://hooks.example/%d", i),
					SecretHash: hash[:],
					Events:     []string{string(platform.WebhookEventAnomalyFreeze)},
					Enabled:    true,
				}, cap_)
				switch {
				case err == nil:
					atomic.AddInt64(&ok, 1)
				case errors.Is(err, platform.ErrWebhookQuotaExceeded):
					atomic.AddInt64(&quotaErrs, 1)
				default:
					atomic.AddInt64(&otherErrs, 1)
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := atomic.LoadInt64(&ok); got != cap_ {
			t.Errorf("successful creates = %d, want exactly %d (the cap)", got, cap_)
		}
		if got := atomic.LoadInt64(&quotaErrs); got != goroutines-cap_ {
			t.Errorf("quota-exceeded errors = %d, want %d (cap losers)", got, goroutines-cap_)
		}
		if got := atomic.LoadInt64(&otherErrs); got != 0 {
			t.Errorf("unexpected errors = %d, want 0", got)
		}
		listed, err := webhooks.ListWebhooksForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != cap_ {
			t.Errorf("persisted rows = %d, want %d (the cap)", len(listed), cap_)
		}
	})

	// F-1257 (codex audit-2026-05-12): same shape for the API key
	// quota. Concurrent Create calls at the cap boundary must end
	// at exactly the cap, with the losers receiving
	// ErrAPIKeyQuotaExceeded.
	t.Run("APIKeyStore/Concurrent_QuotaCap_Holds", func(t *testing.T) {
		keys := postgresstore.NewAPIKeyStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "KeyRaceCo", Slug: "keyrace-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "keyrace-" + uuid.New().String() + "@k.example",
			Tier:         platform.TierStarter, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		const (
			cap_       = 4
			goroutines = 12
		)
		var (
			wg        sync.WaitGroup
			ok        int64
			quotaErrs int64
			otherErrs int64
			start     = make(chan struct{})
		)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				<-start
				// F-1263 (codex audit-2026-05-13): both `id` and
				// `key_prefix` have schema CHECK constraints that
				// the prior fixture violated:
				//
				//   - `api_keys_id_check       (id ~ '^kid_[a-f0-9]{12,}$')`
				//   - `api_keys_key_prefix_check (key_prefix ~ '^rek_[a-f0-9]{8}$')`
				//
				// `uuid.New().String()` includes hyphens; the
				// previous `plaintext := "rek_race_%d_%s"` shape
				// also tripped the prefix regex. Build hex-only
				// values that match what the production
				// `generateKeyID` / `generatePlaintext` emit
				// (`sip_` namespace), so the test reaches the
				// actual advisory-lock assertions for F-1257.
				hexA := strings.ReplaceAll(uuid.New().String(), "-", "")
				hexB := strings.ReplaceAll(uuid.New().String(), "-", "")
				plaintext := "sip_" + hexA[:8]
				hash := sha256.Sum256([]byte(plaintext + fmt.Sprintf("-%d", i)))
				_, err := keys.Create(ctx, platform.APIKey{
					ID:              "kid_" + hexB[:12],
					AccountID:       acct.ID,
					Name:            fmt.Sprintf("k-%d", i),
					KeyHash:         hash[:],
					KeyPrefix:       plaintext,
					Tier:            platform.APIKeyTierAPIKey,
					RateLimitPerMin: 100,
					CreatedAt:       time.Now().UTC(),
				}, cap_)
				switch {
				case err == nil:
					atomic.AddInt64(&ok, 1)
				case errors.Is(err, platform.ErrAPIKeyQuotaExceeded):
					atomic.AddInt64(&quotaErrs, 1)
				default:
					atomic.AddInt64(&otherErrs, 1)
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := atomic.LoadInt64(&ok); got != cap_ {
			t.Errorf("successful creates = %d, want exactly %d (the cap)", got, cap_)
		}
		if got := atomic.LoadInt64(&quotaErrs); got != goroutines-cap_ {
			t.Errorf("quota-exceeded errors = %d, want %d (cap losers)", got, goroutines-cap_)
		}
		if got := atomic.LoadInt64(&otherErrs); got != 0 {
			t.Errorf("unexpected errors = %d, want 0", got)
		}
		listed, err := keys.ListForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != cap_ {
			t.Errorf("persisted active rows = %d, want %d (the cap)", len(listed), cap_)
		}
	})

	// Admin Phase 1.5 incident tooling — status_notices (migration 0082).
	t.Run("StatusNoticeStore/CRUD+resolve", func(t *testing.T) {
		notices := postgresstore.NewStatusNoticeStore(store)

		// 1. Create → born active with server timestamps.
		created, err := notices.Create(ctx, platform.StatusNotice{
			Title:     "Scheduled maintenance",
			Body:      "Aggregator restart 02:00–03:00 UTC.",
			Severity:  platform.NoticeMaintenance,
			CreatedBy: "kid_operator1",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created.ID == uuid.Nil {
			t.Fatal("ID not populated")
		}
		if created.Status != platform.NoticeActive {
			t.Errorf("Status = %q, want active", created.Status)
		}
		if created.CreatedAt.IsZero() {
			t.Error("CreatedAt not populated")
		}

		// 2. Get round-trips the fields.
		got, err := notices.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Title != "Scheduled maintenance" || got.Severity != platform.NoticeMaintenance {
			t.Errorf("round-trip mismatch: %+v", got)
		}
		if got.CreatedBy != "kid_operator1" {
			t.Errorf("CreatedBy = %q", got.CreatedBy)
		}

		// 3. A second, resolved-later notice; ListActive shows both.
		second, err := notices.Create(ctx, platform.StatusNotice{
			Title: "Pricing lag", Body: "CEX feed delayed.", Severity: platform.NoticeMajor,
		})
		if err != nil {
			t.Fatalf("Create second: %v", err)
		}
		active, err := notices.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(active) != 2 {
			t.Fatalf("ListActive len = %d, want 2", len(active))
		}
		// Newest first.
		if !active[0].CreatedAt.After(active[1].CreatedAt) && active[0].ID != second.ID {
			t.Errorf("ListActive not newest-first: %v", active)
		}

		// 4. Resolve the first; it drops off ListActive but stays in List.
		resolved, err := notices.Resolve(ctx, created.ID)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved.Status != platform.NoticeResolved || resolved.ResolvedAt.IsZero() {
			t.Errorf("resolve didn't stamp: %+v", resolved)
		}
		active, err = notices.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive after resolve: %v", err)
		}
		if len(active) != 1 || active[0].ID != second.ID {
			t.Errorf("ListActive after resolve = %+v, want only the second", active)
		}
		all, err := notices.List(ctx, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 2 {
			t.Errorf("List (all) len = %d, want 2 (incl resolved)", len(all))
		}

		// 5. Resolve is idempotent — resolved_at doesn't move.
		firstResolvedAt := resolved.ResolvedAt
		again, err := notices.Resolve(ctx, created.ID)
		if err != nil {
			t.Fatalf("Resolve (idempotent): %v", err)
		}
		if !again.ResolvedAt.Equal(firstResolvedAt) {
			t.Errorf("resolved_at moved on re-resolve: %v → %v", firstResolvedAt, again.ResolvedAt)
		}

		// 6. ErrNotFound on absent.
		if _, err := notices.Get(ctx, uuid.New()); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("Get absent: expected ErrNotFound, got %v", err)
		}
		if _, err := notices.Resolve(ctx, uuid.New()); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("Resolve absent: expected ErrNotFound, got %v", err)
		}
	})

	// Passkey persistence — webauthn_credentials (migration 0140).
	// Auth-grade store: sign-count round-trips feed clone detection,
	// credential-ID uniqueness is spec-required, and delete is
	// owner-scoped (no cross-user probe oracle).
	t.Run("WebAuthnStore/CRUD+signcount", func(t *testing.T) {
		passkeys := postgresstore.NewWebAuthnCredentialStore(store)

		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Passkey Co", Slug: "passkey-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "pk-" + uuid.New().String() + "@p.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		alice, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID,
			Email:     "passkey-alice-" + uuid.New().String() + "@p.example",
			Role:      platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		// 1. Create a fully-populated credential (registration path).
		credID := []byte("cred-" + uuid.New().String())
		publicKey := []byte{0xA5, 0x01, 0x02, 0x03, 0x26} // COSE-ish blob; opaque to the store
		aaguid := bytes.Repeat([]byte{0xAB}, 16)
		created, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID:          alice.ID,
			Name:            "MacBook Touch ID",
			CredentialID:    credID,
			PublicKey:       publicKey,
			AttestationType: "none",
			Transports:      []string{"internal", "hybrid"},
			SignCount:       0,
			BackupEligible:  true,
			BackupState:     true,
			AAGUID:          aaguid,
		})
		if err != nil {
			t.Fatalf("create credential: %v", err)
		}
		if created.ID == uuid.Nil {
			t.Fatal("ID not populated")
		}
		if created.CreatedAt.IsZero() {
			t.Error("CreatedAt not populated")
		}
		if !created.LastUsedAt.IsZero() {
			t.Errorf("LastUsedAt should be zero (never used), got %v", created.LastUsedAt)
		}
		if len(created.Transports) != 2 || created.Transports[0] != "internal" || created.Transports[1] != "hybrid" {
			t.Errorf("Transports round-trip: %v", created.Transports)
		}
		if !bytes.Equal(created.AAGUID, aaguid) {
			t.Errorf("AAGUID round-trip: %x", created.AAGUID)
		}
		if !created.BackupEligible || !created.BackupState {
			t.Errorf("backup flags didn't round-trip: BE=%v BS=%v", created.BackupEligible, created.BackupState)
		}

		// 2. Lookup by credential ID — the login-assertion key. The
		// public key must come back byte-exact (it verifies signatures).
		byCred, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, credID)
		if err != nil {
			t.Fatalf("get by credential id: %v", err)
		}
		if byCred.ID != created.ID {
			t.Errorf("credential-id lookup got different row")
		}
		if !bytes.Equal(byCred.PublicKey, publicKey) {
			t.Errorf("PublicKey round-trip: %x, want %x", byCred.PublicKey, publicKey)
		}
		// Absent credential ID → ErrNotFound.
		if _, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, []byte("never-registered")); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("expected ErrNotFound for absent credential id, got %v", err)
		}

		// 3. A second credential in the production handler's minimal
		// shape: empty (non-nil) transports, nil AAGUID. The empty
		// text[] must come back as a non-nil empty slice, the NULL
		// aaguid as an empty AAGUID.
		cred2ID := []byte("cred-" + uuid.New().String())
		second, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID:       alice.ID,
			Name:         "iCloud passkey",
			CredentialID: cred2ID,
			PublicKey:    []byte{0x01},
			Transports:   []string{},
		})
		if err != nil {
			t.Fatalf("create minimal credential: %v", err)
		}
		if second.Transports == nil || len(second.Transports) != 0 {
			t.Errorf("empty transports round-trip: %#v, want non-nil empty slice", second.Transports)
		}
		if len(second.AAGUID) != 0 {
			t.Errorf("nil AAGUID round-trip: %x, want empty", second.AAGUID)
		}

		// 4. List for user: both rows, newest first.
		list, err := passkeys.ListWebAuthnCredentialsForUser(ctx, alice.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("list len = %d, want 2", len(list))
		}
		for i := 1; i < len(list); i++ {
			if list[i-1].CreatedAt.Before(list[i].CreatedAt) {
				t.Errorf("list not newest-first at %d: %v < %v", i, list[i-1].CreatedAt, list[i].CreatedAt)
			}
		}

		// 5. Duplicate credential ID → ErrConflict (spec-required
		// global uniqueness; webauthn_credentials_credential_idx).
		_, err = passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID:       alice.ID,
			Name:         "same authenticator again",
			CredentialID: credID,
			PublicKey:    []byte{0x02},
			Transports:   []string{},
		})
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("expected ErrConflict on duplicate credential id, got %v", err)
		}

		// 6. Post-assertion sign-count update (clone detection).
		// uint32-max value exercises the bigint headroom.
		lastUsed := time.Now().UTC().Truncate(time.Microsecond)
		if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 4294967295, lastUsed); err != nil {
			t.Fatalf("update sign count: %v", err)
		}
		byCred, err = passkeys.GetWebAuthnCredentialByCredentialID(ctx, credID)
		if err != nil {
			t.Fatalf("get after sign-count update: %v", err)
		}
		if byCred.SignCount != 4294967295 {
			t.Errorf("SignCount = %d, want 4294967295", byCred.SignCount)
		}
		if !byCred.LastUsedAt.Equal(lastUsed) {
			t.Errorf("LastUsedAt = %v, want %v", byCred.LastUsedAt, lastUsed)
		}
		// Absent row → ErrNotFound.
		if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, uuid.New(), 1, lastUsed); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("update absent: expected ErrNotFound, got %v", err)
		}

		// 7. Owner delete removes the row; the credential can no longer
		// resolve a login assertion. Re-delete → ErrNotFound.
		if err := passkeys.DeleteWebAuthnCredential(ctx, created.ID, alice.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, credID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("deleted credential still resolves by credential id: %v", err)
		}
		if err := passkeys.DeleteWebAuthnCredential(ctx, created.ID, alice.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("re-delete: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("WebAuthnStore/CrossUserIsolation", func(t *testing.T) {
		passkeys := postgresstore.NewWebAuthnCredentialStore(store)

		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Iso Co", Slug: "iso-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "iso-" + uuid.New().String() + "@p.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		alice, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID, Email: "iso-alice-" + uuid.New().String() + "@p.example", Role: platform.RoleOwner,
		})
		if err != nil {
			t.Fatalf("create alice: %v", err)
		}
		bob, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID, Email: "iso-bob-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
		})
		if err != nil {
			t.Fatalf("create bob: %v", err)
		}

		aliceCredID := []byte("iso-cred-" + uuid.New().String())
		aliceCred, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID: alice.ID, Name: "alice key", CredentialID: aliceCredID,
			PublicKey: []byte{0x0A}, Transports: []string{},
		})
		if err != nil {
			t.Fatalf("create alice cred: %v", err)
		}
		bobCred, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID: bob.ID, Name: "bob key", CredentialID: []byte("iso-cred-" + uuid.New().String()),
			PublicKey: []byte{0x0B}, Transports: []string{},
		})
		if err != nil {
			t.Fatalf("create bob cred: %v", err)
		}

		// Lists are user-scoped: no cross-user leakage.
		aliceList, err := passkeys.ListWebAuthnCredentialsForUser(ctx, alice.ID)
		if err != nil {
			t.Fatalf("list alice: %v", err)
		}
		if len(aliceList) != 1 || aliceList[0].ID != aliceCred.ID {
			t.Errorf("alice list leaked or lost rows: %+v", aliceList)
		}
		bobList, err := passkeys.ListWebAuthnCredentialsForUser(ctx, bob.ID)
		if err != nil {
			t.Fatalf("list bob: %v", err)
		}
		if len(bobList) != 1 || bobList[0].ID != bobCred.ID {
			t.Errorf("bob list leaked or lost rows: %+v", bobList)
		}

		// A user with no credentials gets an empty slice, not an error.
		nobody, err := users.CreateUser(ctx, platform.User{
			AccountID: acct.ID, Email: "iso-nobody-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
		})
		if err != nil {
			t.Fatalf("create nobody: %v", err)
		}
		empty, err := passkeys.ListWebAuthnCredentialsForUser(ctx, nobody.ID)
		if err != nil {
			t.Fatalf("list nobody: %v", err)
		}
		if empty == nil || len(empty) != 0 {
			t.Errorf("expected non-nil empty list, got %#v", empty)
		}

		// Uniqueness is GLOBAL: bob registering alice's credential ID
		// is ErrConflict, not a second row.
		_, err = passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
			UserID: bob.ID, Name: "stolen id", CredentialID: aliceCredID,
			PublicKey: []byte{0x0C}, Transports: []string{},
		})
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("cross-user duplicate credential id: expected ErrConflict, got %v", err)
		}

		// Delete is owner-scoped: bob deleting alice's row by its real
		// ID gets ErrNotFound (no probe oracle) and the row survives.
		if err := passkeys.DeleteWebAuthnCredential(ctx, aliceCred.ID, bob.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("cross-user delete: expected ErrNotFound, got %v", err)
		}
		if _, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, aliceCredID); err != nil {
			t.Errorf("alice's credential vanished after bob's delete attempt: %v", err)
		}
	})

	// Customer price alerts — price_alerts (migration 0080).
	t.Run("PriceAlertStore/CRUD+precision", func(t *testing.T) {
		alerts := postgresstore.NewPriceAlertStore(store)

		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Alert Co", Slug: "alert-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "alert-" + uuid.New().String() + "@a.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}

		// ADR-0003: threshold is NUMERIC end-to-end. Integer part is
		// 2^53+1 — a float64 round-trip would corrupt it — plus a
		// 9-decimal fractional tail for full-precision round-trip.
		const bigThreshold = "9007199254740993.000000001"
		created, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID:       acct.ID,
			BaseAsset:       "native",
			QuoteAsset:      "fiat:USD",
			Condition:       platform.AlertAbove,
			Threshold:       bigThreshold,
			CooldownSeconds: 300,
			Enabled:         true,
		}, 25)
		if err != nil {
			t.Fatalf("create alert: %v", err)
		}
		if created.ID == uuid.Nil {
			t.Fatal("ID not populated")
		}
		if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
			t.Error("CreatedAt/UpdatedAt not populated")
		}

		got, err := alerts.GetPriceAlert(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.AccountID != acct.ID || got.BaseAsset != "native" || got.QuoteAsset != "fiat:USD" {
			t.Errorf("identity round-trip: %+v", got)
		}
		if got.Condition != platform.AlertAbove || got.CooldownSeconds != 300 || !got.Enabled {
			t.Errorf("settings round-trip: %+v", got)
		}
		if !got.LastFiredAt.IsZero() {
			t.Errorf("LastFiredAt should be zero for a never-fired alert (NULL→sentinel translation), got %v", got.LastFiredAt)
		}
		// Exact-value comparison via big.Rat: catches any float64 or
		// scale truncation regardless of textual normalisation.
		if mustRat(t, got.Threshold).Cmp(mustRat(t, bigThreshold)) != 0 {
			t.Errorf("threshold precision lost: stored %q, want value-equal to %q", got.Threshold, bigThreshold)
		}

		// Input validation refuses malformed alerts before any SQL.
		if _, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: platform.AlertAbove, Threshold: "1",
		}, 25); err == nil {
			t.Error("expected error for empty AccountID")
		}
		if _, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID: acct.ID, QuoteAsset: "fiat:USD",
			Condition: platform.AlertAbove, Threshold: "1",
		}, 25); err == nil {
			t.Error("expected error for empty BaseAsset")
		}
		if _, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: "sideways", Threshold: "1",
		}, 25); err == nil {
			t.Error("expected error for invalid condition")
		}
		if _, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: platform.AlertBelow,
		}, 25); err == nil {
			t.Error("expected error for empty threshold")
		}
		// None of the rejects left a row behind.
		list, err := alerts.ListPriceAlertsForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list after rejects: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("rejected creates leaked rows: list len = %d, want 1", len(list))
		}

		// Get absent → ErrNotFound.
		if _, err := alerts.GetPriceAlert(ctx, uuid.New()); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("get absent: expected ErrNotFound, got %v", err)
		}

		// Update flips every mutable field; a full-precision small
		// threshold (18 decimals) must survive too.
		const tinyThreshold = "0.000000000000000123"
		upd := got
		upd.Condition = platform.AlertBelow
		upd.Threshold = tinyThreshold
		upd.CooldownSeconds = 0
		upd.Enabled = false
		if err := alerts.UpdatePriceAlert(ctx, upd); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, err = alerts.GetPriceAlert(ctx, created.ID)
		if err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if got.Condition != platform.AlertBelow || got.CooldownSeconds != 0 || got.Enabled {
			t.Errorf("update didn't persist: %+v", got)
		}
		if mustRat(t, got.Threshold).Cmp(mustRat(t, tinyThreshold)) != 0 {
			t.Errorf("tiny threshold precision lost: stored %q, want value-equal to %q", got.Threshold, tinyThreshold)
		}
		if got.UpdatedAt.Before(created.UpdatedAt) {
			t.Errorf("updated_at went backwards: %v < %v", got.UpdatedAt, created.UpdatedAt)
		}
		// Update absent → ErrNotFound; invalid condition refused.
		absent := upd
		absent.ID = uuid.New()
		if err := alerts.UpdatePriceAlert(ctx, absent); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("update absent: expected ErrNotFound, got %v", err)
		}
		bad := upd
		bad.Condition = "diagonal"
		if err := alerts.UpdatePriceAlert(ctx, bad); err == nil {
			t.Error("expected error for invalid condition on update")
		}

		// ClaimPriceAlertFire stamps last_fired_at (cooldown clock). The
		// alert above carries cooldown_seconds = 0, so a never-fired row
		// claims immediately.
		firedAt := time.Now().UTC().Truncate(time.Microsecond)
		claimed, err := alerts.ClaimPriceAlertFire(ctx, created.ID, firedAt)
		if err != nil {
			t.Fatalf("claim fire: %v", err)
		}
		if !claimed {
			t.Fatal("claim fire on a never-fired alert returned claimed=false")
		}
		got, err = alerts.GetPriceAlert(ctx, created.ID)
		if err != nil {
			t.Fatalf("get after fire: %v", err)
		}
		if !got.LastFiredAt.Equal(firedAt) {
			t.Errorf("LastFiredAt = %v, want %v", got.LastFiredAt, firedAt)
		}

		// Delete, then idempotent re-delete.
		if err := alerts.DeletePriceAlert(ctx, created.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := alerts.GetPriceAlert(ctx, created.ID); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("get after delete: expected ErrNotFound, got %v", err)
		}
		if err := alerts.DeletePriceAlert(ctx, created.ID); err != nil {
			t.Errorf("re-delete should be idempotent, got %v", err)
		}
	})

	t.Run("PriceAlertStore/ListScoping+EnabledSweep", func(t *testing.T) {
		alerts := postgresstore.NewPriceAlertStore(store)

		mkAccount := func(tag string) platform.Account {
			t.Helper()
			acct, err := accounts.Create(ctx, platform.Account{
				Name: "Sweep " + tag, Slug: "sweep-" + tag + "-" + strings.ToLower(uuid.New().String()[:8]),
				BillingEmail: "sweep-" + uuid.New().String() + "@a.example",
				Tier:         platform.TierFree, Status: platform.AccountActive,
			})
			if err != nil {
				t.Fatalf("create account %s: %v", tag, err)
			}
			return acct
		}
		acctA := mkAccount("a")
		acctB := mkAccount("b")

		mkAlert := func(acct uuid.UUID, enabled bool) platform.PriceAlert {
			t.Helper()
			a, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
				AccountID: acct, BaseAsset: "native", QuoteAsset: "fiat:USD",
				Condition: platform.AlertAbove, Threshold: "0.25", Enabled: enabled,
			}, 25)
			if err != nil {
				t.Fatalf("create alert: %v", err)
			}
			return a
		}
		aEnabled := mkAlert(acctA.ID, true)
		aDisabled := mkAlert(acctA.ID, false)
		bEnabled := mkAlert(acctB.ID, true)

		// Per-account list is owner-scoped: A sees exactly its two
		// (disabled included), none of B's.
		listA, err := alerts.ListPriceAlertsForAccount(ctx, acctA.ID)
		if err != nil {
			t.Fatalf("list A: %v", err)
		}
		if len(listA) != 2 {
			t.Fatalf("list A len = %d, want 2", len(listA))
		}
		for _, a := range listA {
			if a.AccountID != acctA.ID {
				t.Errorf("list A leaked foreign alert %v (account %v)", a.ID, a.AccountID)
			}
		}
		listB, err := alerts.ListPriceAlertsForAccount(ctx, acctB.ID)
		if err != nil {
			t.Fatalf("list B: %v", err)
		}
		if len(listB) != 1 || listB[0].ID != bEnabled.ID {
			t.Errorf("list B = %+v, want only B's alert", listB)
		}

		// The evaluator sweep sees every ENABLED alert across accounts
		// and no disabled ones. Other subtests may own rows too, so
		// assert membership, not cardinality.
		sweep, err := alerts.ListEnabledPriceAlerts(ctx)
		if err != nil {
			t.Fatalf("enabled sweep: %v", err)
		}
		seen := map[uuid.UUID]bool{}
		for _, a := range sweep {
			if !a.Enabled {
				t.Errorf("sweep returned disabled alert %v", a.ID)
			}
			seen[a.ID] = true
		}
		if !seen[aEnabled.ID] || !seen[bEnabled.ID] {
			t.Errorf("sweep missing enabled alerts: A=%v B=%v", seen[aEnabled.ID], seen[bEnabled.ID])
		}
		if seen[aDisabled.ID] {
			t.Errorf("sweep included the disabled alert %v", aDisabled.ID)
		}
	})

	// maxPerAccount <= 0 falls back to the in-store default of 5, and
	// the cap counts ALL rows for the account — disabled included —
	// since COUNT(*) is unfiltered.
	t.Run("PriceAlertStore/DefaultCap+DisabledRowsCount", func(t *testing.T) {
		alerts := postgresstore.NewPriceAlertStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "Capped Co", Slug: "capped-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "capped-" + uuid.New().String() + "@a.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		var last platform.PriceAlert
		for i := 0; i < 5; i++ {
			last, err = alerts.CreatePriceAlert(ctx, platform.PriceAlert{
				AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
				Condition: platform.AlertBelow, Threshold: fmt.Sprintf("%d.5", i+1), Enabled: true,
			}, 0) // 0 → default cap of 5
			if err != nil {
				t.Fatalf("create %d under default cap: %v", i, err)
			}
		}
		// Disabling one does not free a slot.
		last.Enabled = false
		if err := alerts.UpdatePriceAlert(ctx, last); err != nil {
			t.Fatalf("disable: %v", err)
		}
		_, err = alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: platform.AlertBelow, Threshold: "9.5", Enabled: true,
		}, 0)
		if !errors.Is(err, platform.ErrPriceAlertQuotaExceeded) {
			t.Errorf("expected ErrPriceAlertQuotaExceeded at default cap, got %v", err)
		}
	})

	// Same shape as WebhookStore/APIKeyStore Concurrent_QuotaCap_Holds:
	// the advisory-lock + CTE-gated INSERT must hold the per-account
	// cap under concurrent creates, with losers getting
	// ErrPriceAlertQuotaExceeded.
	t.Run("PriceAlertStore/Concurrent_QuotaCap_Holds", func(t *testing.T) {
		alerts := postgresstore.NewPriceAlertStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "AlertRaceCo", Slug: "alertrace-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "alertrace-" + uuid.New().String() + "@a.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		const (
			cap_       = 3
			goroutines = 10
		)
		var (
			wg        sync.WaitGroup
			ok        int64
			quotaErrs int64
			otherErrs int64
			start     = make(chan struct{})
		)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				<-start
				_, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
					AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
					Condition: platform.AlertAbove, Threshold: fmt.Sprintf("%d.125", i+1),
					Enabled: true,
				}, cap_)
				switch {
				case err == nil:
					atomic.AddInt64(&ok, 1)
				case errors.Is(err, platform.ErrPriceAlertQuotaExceeded):
					atomic.AddInt64(&quotaErrs, 1)
				default:
					atomic.AddInt64(&otherErrs, 1)
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := atomic.LoadInt64(&ok); got != cap_ {
			t.Errorf("successful creates = %d, want exactly %d (the cap)", got, cap_)
		}
		if got := atomic.LoadInt64(&quotaErrs); got != goroutines-cap_ {
			t.Errorf("quota-exceeded errors = %d, want %d (cap losers)", got, goroutines-cap_)
		}
		if got := atomic.LoadInt64(&otherErrs); got != 0 {
			t.Errorf("unexpected errors = %d, want 0", got)
		}
		listed, err := alerts.ListPriceAlertsForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != cap_ {
			t.Errorf("persisted rows = %d, want %d (the cap)", len(listed), cap_)
		}
	})

	// #368 M10 on real Postgres: the once-per-cooldown-window guarantee is
	// the conditional UPDATE, not the evaluator's in-memory coolingDown
	// check — that check reads the snapshot ListEnabledPriceAlerts took at
	// the top of the sweep, so every concurrent evaluator passes it.
	//
	// Modelled exactly as production would race: N goroutines (stand-ins
	// for a second aggregator / an R2-R3 standby / an overlapping deploy)
	// claim the SAME crossing on the SAME alert at the same instant, all
	// passing the same stale gate. Exactly one may win, because each
	// customer webhook the loser would fan out to is a duplicate
	// notification on a delivery id the customer cannot dedup.
	//
	// This has to be a DB test: the property is Postgres' row-lock
	// serialisation of concurrent UPDATEs (the loser re-evaluates the
	// predicate against the winner's committed row under READ COMMITTED),
	// which no fake reproduces.
	t.Run("PriceAlertStore/Concurrent_FireClaim_ExactlyOneWinner", func(t *testing.T) {
		alerts := postgresstore.NewPriceAlertStore(store)
		acct, err := accounts.Create(ctx, platform.Account{
			Name: "ClaimRaceCo", Slug: "claimrace-" + strings.ToLower(uuid.New().String()[:8]),
			BillingEmail: "claimrace-" + uuid.New().String() + "@a.example",
			Tier:         platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		created, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
			AccountID: acct.ID, BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
			CooldownSeconds: 3600, // a once-per-hour crossing notification
		}, 25)
		if err != nil {
			t.Fatalf("create alert: %v", err)
		}

		const goroutines = 10
		// One shared instant: every evaluator observed the same crossing on
		// the same closed bucket, which is precisely when duplicates hurt.
		firedAt := time.Now().UTC().Truncate(time.Microsecond)
		var (
			wg        sync.WaitGroup
			won       int64
			lost      int64
			claimErrs int64
			start     = make(chan struct{})
		)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				claimed, err := alerts.ClaimPriceAlertFire(ctx, created.ID, firedAt)
				switch {
				case err != nil:
					atomic.AddInt64(&claimErrs, 1)
					t.Errorf("unexpected claim error: %v", err)
				case claimed:
					atomic.AddInt64(&won, 1)
				default:
					atomic.AddInt64(&lost, 1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := atomic.LoadInt64(&won); got != 1 {
			t.Errorf("claims won = %d, want exactly 1 — every extra winner is a duplicate customer webhook for one crossing (#368 M10)", got)
		}
		if got := atomic.LoadInt64(&lost); got != goroutines-1 {
			t.Errorf("claims refused = %d, want %d", got, goroutines-1)
		}
		if got := atomic.LoadInt64(&claimErrs); got != 0 {
			t.Errorf("claim errors = %d, want 0 — a refused claim is not an error", got)
		}

		got, err := alerts.GetPriceAlert(ctx, created.ID)
		if err != nil {
			t.Fatalf("get after claim race: %v", err)
		}
		if !got.LastFiredAt.Equal(firedAt) {
			t.Errorf("LastFiredAt = %v, want %v — the winner's stamp must be durable", got.LastFiredAt, firedAt)
		}

		// A later crossing INSIDE the cooldown is still refused, and one
		// past it claims again: the predicate is the cooldown, not a
		// fire-once latch.
		if claimed, err := alerts.ClaimPriceAlertFire(ctx, created.ID, firedAt.Add(59*time.Minute)); err != nil {
			t.Fatalf("claim inside cooldown: %v", err)
		} else if claimed {
			t.Error("claimed 59 minutes into a 3600s cooldown; want refused")
		}
		after := firedAt.Add(3600 * time.Second)
		if claimed, err := alerts.ClaimPriceAlertFire(ctx, created.ID, after); err != nil {
			t.Fatalf("claim after cooldown: %v", err)
		} else if !claimed {
			t.Error("refused a claim exactly at the cooldown boundary; want claimed")
		}

		// A deleted alert claims nothing and is not an error — the
		// evaluator treats it identically to losing the race.
		if err := alerts.DeletePriceAlert(ctx, created.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if claimed, err := alerts.ClaimPriceAlertFire(ctx, created.ID, after.Add(time.Hour)); err != nil {
			t.Errorf("claim on a deleted alert returned an error: %v", err)
		} else if claimed {
			t.Error("claimed a fire on a deleted alert")
		}
	})
}

// mustRat parses a decimal string into a big.Rat, failing the test on
// garbage. Threshold comparisons go through big.Rat so the assertion is
// about VALUE precision (ADR-0003), not textual normalisation.
func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("unparseable numeric string %q", s)
	}
	return r
}

// containsInviteHash reports whether any invite in the list carries
// the given token hash.
func containsInviteHash(invites []platform.Invite, hash []byte) bool {
	for _, inv := range invites {
		if bytes.Equal(inv.TokenHash, hash) {
			return true
		}
	}
	return false
}
