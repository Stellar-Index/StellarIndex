// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// registerAccountsAsPlatform lets the /v1/register account fake prove key ownership on the revoke path.
type registerAccountsAsPlatform struct{ *fakeRegisterAccountStore }

func (registerAccountsAsPlatform) UpdateAtomic(
	context.Context, uuid.UUID, func(*platform.Account) error,
) (platform.Account, platform.Account, error) {
	panic("unused")
}

// newPostgresBackendRevokeServer is cmd/stellarindex-api's auth_backend=postgres shape: no self-service
// AccountStore, a Postgres key store with its owner store, the Redis mirror and the validator's cache evictor.
func newPostgresBackendRevokeServer(
	t *testing.T, subject auth.Subject, stores v1.APIKeyBudgetStores, accts v1.PlatformAccountStore, sink v1.AuditSink,
) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(subject),
		APIKeyBudgets:    stores,
		PlatformAccounts: accts,
		Audit:            sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func revokeAccountKey(t *testing.T, ts *httptest.Server, keyID string) *http.Response {
	t.Helper()
	return doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+keyID, "", "")
}

// TestAccountKeysRevoke_PostgresBackendKeyRevokesItself: a /v1/register key, minted with its canonical Redis
// record as production does and authenticated by the Postgres validator (which reads that record first), revokes
// itself and fails authentication on the next lookup, leaving a key.revoke audit row naming the key as its own actor.
func TestAccountKeysRevoke_PostgresBackendKeyRevokesItself(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	reg := newRegisterTestServer(t, accounts, keys, nil, auth.NewRedisAPIKeyStore(rdb))
	resp := postRegister(t, reg.URL, "application/json", `{"name":"agent"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, want 200", resp.StatusCode)
	}
	minted := decodeRegisterResult(t, resp)
	if n := len(rdb.Keys(context.Background(), "apikey:*").Val()); n == 0 {
		t.Fatal("register wrote no canonical apikey: record; the test would not cover the production key shape")
	}

	validator, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys: keys, Accounts: accounts, Cache: rdb, CacheTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPostgresAPIKeyValidator: %v", err)
	}
	subject, err := validator.Lookup(context.Background(), minted.APIKey)
	if err != nil {
		t.Fatalf("Lookup(minted key) before revoke: %v", err)
	}

	sink := &recordingAuditSink{}
	ts := newPostgresBackendRevokeServer(t, subject, v1.APIKeyBudgetStores{
		Platform:         keys,
		RedisMirror:      auth.NewRedisAPIKeyStore(rdb),
		CacheInvalidator: validator,
	}, registerAccountsAsPlatform{accounts}, sink)

	if got := revokeAccountKey(t, ts, minted.KeyID).StatusCode; got != http.StatusNoContent {
		t.Fatalf("self-revoke status = %d, want 204: a leaked /v1/register key must be able to kill itself", got)
	}
	if _, err := validator.Lookup(context.Background(), minted.APIKey); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Lookup(revoked key) err = %v, want ErrUnauthorized: the key still authenticates", err)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 key.revoke row", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "key.revoke" || e.TargetKind != "api_key" || e.TargetID != minted.KeyID {
		t.Errorf("audit entry = %+v, want key.revoke of %s", e, minted.KeyID)
	}
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatalf("audit metadata: %v", err)
	}
	if meta["actor_key_id"] != minted.KeyID || meta["route"] != "/v1/account/keys" {
		t.Errorf("audit metadata = %v, want actor_key_id %s on route /v1/account/keys", meta, minted.KeyID)
	}
}

const (
	selfKeyID    = "kid_00000000000000a1"
	siblingKeyID = "kid_00000000000000a2"
)

// seedPostgresBackendKeys seeds selfKeyID and siblingKeyID as live rows of one account, each with a hash.
func seedPostgresBackendKeys() (*fakeRegisterKeyStore, *fakePlatformAccountStore, auth.Subject) {
	owner := platform.Account{ID: uuid.New(), Slug: "reg-abc123"}
	keys := newFakeRegisterKeyStore()
	keys.byID[selfKeyID] = platform.APIKey{ID: selfKeyID, AccountID: owner.ID, KeyHash: []byte{0xa1}}
	keys.byID[siblingKeyID] = platform.APIKey{ID: siblingKeyID, AccountID: owner.ID, KeyHash: []byte{0xa2}}
	subject := auth.Subject{Identifier: auth.AccountIdentifier(owner.Slug), Tier: auth.TierAPIKey, KeyID: selfKeyID}
	return keys, newFakePlatformAccountStore(owner), subject
}

// TestAccountKeysRevoke_PostgresBackendRevokesOnlyItself: with no AccountStore there is no delegation tree to
// consult, so the route revokes the presented key and nothing else, for an operator too, and the answer for
// another key is the same whether that key exists or not.
func TestAccountKeysRevoke_PostgresBackendRevokesOnlyItself(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*auth.Subject)
		target string
	}{
		{"sibling key", func(*auth.Subject) {}, siblingKeyID},
		{"unknown key", func(*auth.Subject) {}, "kid_00000000000000ff"},
		{"operator on another key", func(s *auth.Subject) { s.Tier = auth.TierOperator }, siblingKeyID},
		{"session with no key id", func(s *auth.Subject) { s.KeyID = "" }, siblingKeyID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, accts, subject := seedPostgresBackendKeys()
			tc.mutate(&subject)
			mirror := &revokingMirror{}
			inv := &recordingKeyCacheInvalidator{}
			sink := &recordingAuditSink{}
			ts := newPostgresBackendRevokeServer(t, subject,
				v1.APIKeyBudgetStores{Platform: keys, RedisMirror: mirror, CacheInvalidator: inv}, accts, sink)

			resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+tc.target, "r", "")
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if got := problemType(t, resp); got != "https://api.stellarindex.io/errors/account-store-unavailable" {
				t.Errorf("problem type = %q: the answer must not depend on whether the target exists", got)
			}
			if len(keys.revokedIDs) != 0 || len(mirror.revoked) != 0 || len(inv.evicted) != 0 || len(sink.entries) != 0 {
				t.Errorf("revoked rows %v, mirror %v, evicted %v, audit %d: another key was touched",
					keys.revokedIDs, mirror.revoked, inv.evicted, len(sink.entries))
			}
		})
	}
}

// TestAccountKeysRevoke_PostgresBackendFailureIsNotSuccess: a store that cannot complete its leg turns the revoke
// into a 5xx, never a 204, and an owner that cannot be proven is refused before anything is touched.
func TestAccountKeysRevoke_PostgresBackendFailureIsNotSuccess(t *testing.T) {
	boom := errors.New("store down")
	for _, tc := range []struct {
		name       string
		evictErr   error
		revokeErr  error
		mirrorErr  error
		noOwner    bool
		wantStatus int
	}{
		{name: "cache eviction fails", evictErr: boom, wantStatus: http.StatusInternalServerError},
		{name: "postgres revoke fails", revokeErr: boom, wantStatus: http.StatusInternalServerError},
		{name: "redis mirror fails", mirrorErr: boom, wantStatus: http.StatusInternalServerError},
		{name: "owner store missing", noOwner: true, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, accts, subject := seedPostgresBackendKeys()
			keys.revokeErr = tc.revokeErr
			var owners v1.PlatformAccountStore = accts
			if tc.noOwner {
				owners = nil
			}
			stores := v1.APIKeyBudgetStores{
				Platform:         keys,
				RedisMirror:      &revokingMirror{err: tc.mirrorErr},
				CacheInvalidator: &recordingKeyCacheInvalidator{err: tc.evictErr},
			}
			ts := newPostgresBackendRevokeServer(t, subject, stores, owners, nil)
			if got := revokeAccountKey(t, ts, selfKeyID).StatusCode; got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}
		})
	}
}

// TestAccountKeysRevoke_PostgresBackendAlreadyRevokedIs204: a retry racing a completed revoke finds nothing
// live in either store and answers 204 without a second audit row.
func TestAccountKeysRevoke_PostgresBackendAlreadyRevokedIs204(t *testing.T) {
	keys, accts, subject := seedPostgresBackendKeys()
	k := keys.byID[selfKeyID]
	k.RevokedAt = time.Now().UTC()
	keys.byID[selfKeyID] = k
	inv := &recordingKeyCacheInvalidator{}
	sink := &recordingAuditSink{}
	ts := newPostgresBackendRevokeServer(t, subject, v1.APIKeyBudgetStores{
		Platform: keys, RedisMirror: &revokingMirror{err: auth.ErrKeyNotFound}, CacheInvalidator: inv,
	}, accts, sink)

	if got := revokeAccountKey(t, ts, selfKeyID).StatusCode; got != http.StatusNoContent {
		t.Errorf("status = %d, want 204", got)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0 for a revoke that changed nothing", len(sink.entries))
	}
	if len(keys.revokedIDs) != 0 {
		t.Errorf("store Revoke called for %v: re-revoking a revoked row overwrites its first revoker", keys.revokedIDs)
	}
	if len(inv.evicted) != 1 || inv.evicted[0] != hex.EncodeToString([]byte{0xa1}) {
		t.Errorf("evicted = %v, want only the key's own cache row", inv.evicted)
	}
}

// TestKeyRevoke_PartialCommitIsAudited: when the Postgres row is revoked but the Redis mirror leg fails, the key is
// already dead and a retry cannot repeat the revoke, so the 500 must still leave exactly one key.revoke row marked
// failed, on the self-revoke route and on the staff kill switch that shares the every-store revoke.
func TestKeyRevoke_PartialCommitIsAudited(t *testing.T) {
	boom := errors.New("redis down")
	for _, tc := range []struct {
		name string
		url  func(base string) string
		who  func(self auth.Subject) auth.Subject
	}{
		{
			name: "self-revoke",
			url:  func(base string) string { return base + "/v1/account/keys/" + selfKeyID },
			who:  func(self auth.Subject) auth.Subject { return self },
		},
		{
			name: "admin kill switch",
			url: func(base string) string {
				return base + "/v1/admin/keys/" + selfKeyID + "?identifier=" + auth.AccountIdentifier("reg-abc123")
			},
			who: func(auth.Subject) auth.Subject { return operatorSubject() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, accts, subject := seedPostgresBackendKeys()
			sink := &recordingAuditSink{}
			ts := newPostgresBackendRevokeServer(t, tc.who(subject), v1.APIKeyBudgetStores{
				Platform: keys, RedisMirror: &revokingMirror{err: boom}, CacheInvalidator: &recordingKeyCacheInvalidator{},
			}, accts, sink)

			if got := doWithReason(t, http.MethodDelete, tc.url(ts.URL), "leaked", "").StatusCode; got != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", got)
			}
			if len(keys.revokedIDs) != 1 {
				t.Fatalf("postgres revokes = %v, want the row revoked", keys.revokedIDs)
			}
			if len(sink.entries) != 1 {
				t.Fatalf("audit entries = %d, want exactly 1 for a revoke that committed its Postgres leg", len(sink.entries))
			}
			var meta map[string]any
			if err := json.Unmarshal(sink.entries[0].Metadata, &meta); err != nil {
				t.Fatalf("audit metadata: %v", err)
			}
			if sink.entries[0].Action != "key.revoke" || meta["outcome"] != "failed" {
				t.Errorf("audit entry %s with metadata %v, want key.revoke with outcome failed", sink.entries[0].Action, meta)
			}
		})
	}
}

// TestAccountKeysRevoke_PostgresBackendNoOwnerStoreTouchesNothing: with a Postgres key store but no account store
// the row's owner cannot be proven, so the self-revoke is refused with 503 before any store is touched.
func TestAccountKeysRevoke_PostgresBackendNoOwnerStoreTouchesNothing(t *testing.T) {
	keys, _, subject := seedPostgresBackendKeys()
	mirror := &revokingMirror{}
	inv := &recordingKeyCacheInvalidator{}
	sink := &recordingAuditSink{}
	ts := newPostgresBackendRevokeServer(t, subject,
		v1.APIKeyBudgetStores{Platform: keys, RedisMirror: mirror, CacheInvalidator: inv}, nil, sink)

	resp := revokeAccountKey(t, ts, selfKeyID)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if len(keys.revokedIDs) != 0 || len(mirror.revoked) != 0 || len(inv.evicted) != 0 || len(sink.entries) != 0 {
		t.Errorf("revoked rows %v, mirror %v, evicted %v, audit %d: want nothing touched",
			keys.revokedIDs, mirror.revoked, inv.evicted, len(sink.entries))
	}
}
