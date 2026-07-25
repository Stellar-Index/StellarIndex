package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-010 (audit-2026-07-23) — the account-level kill switch on the
// DEFAULT auth backend.
//
// The Postgres validator has always rejected a key whose account is not
// active, and the dashboard session middleware denies suspended/closed
// accounts. This validator never read account status at all — so on
// `auth_backend=redis` (the default, and what r1 runs) suspending an
// account did not stop its keys from authenticating, not even via a
// manual `UPDATE accounts SET status='suspended'`.

// stubAccountStatusReader is a canned [AccountStatusReader].
type stubAccountStatusReader struct {
	bySlug map[string]platform.Account
	err    error
	calls  int
}

func (s *stubAccountStatusReader) GetBySlug(_ context.Context, slug string) (platform.Account, error) {
	s.calls++
	if s.err != nil {
		return platform.Account{}, s.err
	}
	a, ok := s.bySlug[slug]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

// newSuspendTestValidator wires miniredis + a validator with the
// account-status gate enabled.
func newSuspendTestValidator(t *testing.T, accounts AccountStatusReader) (*RedisAPIKeyValidator, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisAPIKeyValidator(rdb, WithAccountStatus(accounts)), mr
}

func acctWithStatus(slug string, st platform.AccountStatus) platform.Account {
	return platform.Account{ID: uuid.New(), Slug: slug, Name: slug, Tier: platform.TierPro, Status: st}
}

// TestRedisAPIKey_SuspendedAccountRejected is the core regression: a
// live, unrevoked, unexpired key belonging to a SUSPENDED account must
// not authenticate.
func TestRedisAPIKey_SuspendedAccountRejected(t *testing.T) {
	for _, st := range []platform.AccountStatus{platform.AccountSuspended, platform.AccountClosed} {
		t.Run(string(st), func(t *testing.T) {
			accounts := &stubAccountStatusReader{
				bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", st)},
			}
			v, mr := newSuspendTestValidator(t, accounts)
			seedKey(t, mr, "sip_live_key", APIKeyRecord{
				KeyID:      "kid_live",
				Identifier: AccountIdentifier("acme"),
				Tier:       TierAPIKey,
			})

			_, err := v.Lookup(context.Background(), "sip_live_key")
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Lookup err = %v, want ErrUnauthorized — a %s account's key must not authenticate", err, st)
			}
			if accounts.calls != 1 {
				t.Errorf("account lookups = %d, want 1", accounts.calls)
			}
		})
	}
}

// TestRedisAPIKey_ActiveAccountAllowed pins the allow-path: the gate
// must not break the ordinary case, and must carry the record's Subject
// through unchanged.
func TestRedisAPIKey_ActiveAccountAllowed(t *testing.T) {
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountActive)},
	}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_ok_key", APIKeyRecord{
		KeyID:           "kid_ok",
		Identifier:      AccountIdentifier("acme"),
		Tier:            TierAPIKey,
		RateLimitPerMin: 10000,
	})

	sub, err := v.Lookup(context.Background(), "sip_ok_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_ok" || sub.RateLimitPerMin != 10000 {
		t.Errorf("Subject = %+v, want kid_ok @ 10000/min", sub)
	}
}

// TestRedisAPIKey_LegacySignupIdentifierUnaffected pins the scope of
// the gate: a `signup-<emailhash>` record carries no account reference,
// so it must authenticate without any account lookup at all (the
// operator kill switch for those is DELETE /v1/admin/keys/{keyID}).
func TestRedisAPIKey_LegacySignupIdentifierUnaffected(t *testing.T) {
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{}}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_legacy_key", APIKeyRecord{
		KeyID:      "kid_legacy",
		Identifier: "signup-0011223344556677",
		Tier:       TierAPIKey,
	})

	sub, err := v.Lookup(context.Background(), "sip_legacy_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_legacy" {
		t.Errorf("Subject.KeyID = %q, want kid_legacy", sub.KeyID)
	}
	if accounts.calls != 0 {
		t.Errorf("account lookups = %d, want 0 for a legacy signup identifier", accounts.calls)
	}
}

// TestRedisAPIKey_MissingAccountRejected pins the fail-closed direction
// for a dangling reference: an `acct:` key whose account row is gone is
// the closed-account case, not an unknown one.
func TestRedisAPIKey_MissingAccountRejected(t *testing.T) {
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{}}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_orphan_key", APIKeyRecord{
		KeyID:      "kid_orphan",
		Identifier: AccountIdentifier("deleted-co"),
		Tier:       TierAPIKey,
	})

	if _, err := v.Lookup(context.Background(), "sip_orphan_key"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Lookup err = %v, want ErrUnauthorized", err)
	}
}

// TestRedisAPIKey_AccountReadErrorFailsClosed pins the degradation
// direction. Every other store failure in this package degrades toward
// serving the request; the suspension gate must not — "the account store
// blipped, so authenticate the suspended customer anyway" is the one
// degradation a kill switch cannot make.
func TestRedisAPIKey_AccountReadErrorFailsClosed(t *testing.T) {
	accounts := &stubAccountStatusReader{err: errors.New("postgres unreachable")}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_blip_key", APIKeyRecord{
		KeyID:      "kid_blip",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})

	sub, err := v.Lookup(context.Background(), "sip_blip_key")
	if err == nil {
		t.Fatalf("Lookup succeeded with Subject %+v; want an error (fail closed)", sub)
	}
	if sub.KeyID != "" {
		t.Errorf("Subject leaked on the error path: %+v", sub)
	}
}

// TestRedisAPIKey_NoAccountReaderIsPreFixBehaviour pins that the gate is
// strictly opt-in: without a reader wired there is no account lookup and
// no behaviour change at all.
func TestRedisAPIKey_NoAccountReaderIsPreFixBehaviour(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "sip_nogate_key", APIKeyRecord{
		KeyID:      "kid_nogate",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})
	sub, err := v.Lookup(context.Background(), "sip_nogate_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_nogate" {
		t.Errorf("Subject.KeyID = %q, want kid_nogate", sub.KeyID)
	}
}
