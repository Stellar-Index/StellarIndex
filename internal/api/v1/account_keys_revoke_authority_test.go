// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// revokeTree is one account's keys: owner root, a member's root, the owner's child (with a grandchild), a sibling
// child, and a child narrowed to the account scope; plus a key on another account.
type revokeTree struct {
	store     *auth.RedisAPIKeyStore
	validator *auth.RedisAPIKeyValidator
	subjects  map[string]auth.Subject
	plains    map[string]string
}

func newRevokeTree(t *testing.T) *revokeTree {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	tr := &revokeTree{
		store:     auth.NewRedisAPIKeyStore(rdb),
		validator: auth.NewRedisAPIKeyValidator(rdb),
		subjects:  map[string]auth.Subject{},
		plains:    map[string]string{},
	}
	tr.mint(t, "owner", auth.CreateAPIKeyRequest{Identifier: "acct:co"})
	tr.mint(t, "member", auth.CreateAPIKeyRequest{Identifier: "acct:co"})
	tr.mint(t, "child", auth.ChildKeyRequest(tr.subjects["owner"], "child", nil))
	tr.mint(t, "grandchild", auth.ChildKeyRequest(tr.subjects["child"], "grandchild", nil))
	tr.mint(t, "sibling", auth.ChildKeyRequest(tr.subjects["owner"], "sibling", nil))
	tr.mint(t, "restricted", auth.ChildKeyRequest(tr.subjects["owner"], "restricted", []string{"account"}))
	tr.mint(t, "other", auth.CreateAPIKeyRequest{Identifier: "acct:other-co"})
	return tr
}

func (tr *revokeTree) mint(t *testing.T, name string, req auth.CreateAPIKeyRequest) {
	t.Helper()
	_, plain, err := tr.store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("mint %s: %v", name, err)
	}
	sub, err := tr.validator.Lookup(context.Background(), plain)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	tr.subjects[name], tr.plains[name] = sub, plain
}

func (tr *revokeTree) live(name string) bool {
	_, err := tr.validator.Lookup(context.Background(), tr.plains[name])
	return !errors.Is(err, auth.ErrUnauthorized)
}

// An operator and a SEP-10 session revoke a /v1/register key's Postgres management row through this route too.
func TestAccountKeysRevoke_AccountWideActorsRevokeThePostgresRow(t *testing.T) {
	keyID := "kid_" + strings.Repeat("0a", 12)
	for name, subject := range map[string]auth.Subject{
		"operator": {Identifier: "acct:reg-abc123", Tier: auth.TierOperator, KeyID: "kid_00000000000000aa"},
		"sep10":    auth.NewSEP10Session("acct:reg-abc123", time.Now()),
	} {
		t.Run(name, func(t *testing.T) {
			platformKeys, accts := seedOwnedPlatformKey("reg-abc123", keyID)
			ts := newAdminTestServerWithPlatformKeys(t, subject, &fakeAccountStore{}, platformKeys, accts)
			resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+keyID, "leaked key", "")
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			if len(platformKeys.revokedIDs) != 1 || platformKeys.revokedIDs[0] != keyID {
				t.Errorf("postgres management row revokes = %v, want [%s]", platformKeys.revokedIDs, keyID)
			}
		})
	}
}

// A path value not shaped like a key id is refused before any store is read, for every caller.
func TestAccountKeysRevoke_RejectsMalformedKeyIDBeforeAnyStoreCall(t *testing.T) {
	for _, keyID := range []string{"abc", "kid_XYZ0123456789ab", "kid_0123", "kid_" + strings.Repeat("0", 65), "kid_0123456789ab%2Fx"} {
		for name, subject := range map[string]auth.Subject{
			"key":      {Identifier: "acct:co", Tier: auth.TierAPIKey, KeyID: "kid_00000000000000aa"},
			"operator": {Identifier: "acct:co", Tier: auth.TierOperator, KeyID: "kid_00000000000000aa"},
			"sep10":    auth.NewSEP10Session("acct:co", time.Now()),
		} {
			store := &fakeAccountStore{}
			ts := newAdminTestServer(t, subject, store, &recordingAuditSink{})
			resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+keyID, "r", "")
			if resp.StatusCode != http.StatusBadRequest || store.listCalls != 0 || store.revokeCalls != 0 {
				t.Errorf("%s %q: status %d, list %d, revoke %d; want 400 and no store call",
					name, keyID, resp.StatusCode, store.listCalls, store.revokeCalls)
			}
		}
	}
}

// A retried revoke (its 204 lost) is still 204 for the key that was allowed to make it, and stays 403 for anyone
// else; a refused revoke leaves no retry note behind.
func TestAccountKeysRevoke_RetryAfterSuccessIsIdempotent(t *testing.T) {
	tr := newRevokeTree(t)
	childID := tr.subjects["child"].KeyID
	del := func(actor string) int {
		ts := newAdminTestServer(t, tr.subjects[actor], tr.store, &recordingAuditSink{})
		return doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+childID, "", "").StatusCode
	}
	if got := del("sibling"); got != http.StatusForbidden {
		t.Fatalf("sibling revoke = %d, want 403", got)
	}
	if note, err := tr.store.RevokedBy(context.Background(), "acct:co", childID); err != nil || note != nil {
		t.Fatalf("refused revoke left a retry note %v (err %v)", note, err)
	}
	if got := del("owner"); got != http.StatusNoContent {
		t.Fatalf("first revoke = %d, want 204", got)
	}
	if got := del("owner"); got != http.StatusNoContent {
		t.Errorf("retried revoke = %d, want 204", got)
	}
	for _, actor := range []string{"sibling", "member", "other"} {
		if got := del(actor); got != http.StatusForbidden {
			t.Errorf("%s revoke of the revoked child = %d, want 403", actor, got)
		}
	}
}

// An API key may revoke only keys delegated from it: never its minter, a sibling, or another root on the account (a
// member's key and the owner's are both roots). A SEP-10 session proved control of the account and may revoke any key.
func TestAccountKeysRevoke_OnlyTheCallersDelegatedKeys(t *testing.T) {
	cases := []struct {
		actor, target string
		sep10         bool
		want          int
		dead          []string
	}{
		{actor: "owner", target: "child", want: http.StatusNoContent, dead: []string{"child", "grandchild"}},
		{actor: "owner", target: "grandchild", want: http.StatusNoContent, dead: []string{"grandchild"}},
		{actor: "child", target: "grandchild", want: http.StatusNoContent, dead: []string{"grandchild"}},
		{actor: "child", target: "owner", want: http.StatusForbidden},
		{actor: "child", target: "sibling", want: http.StatusForbidden},
		{actor: "restricted", target: "sibling", want: http.StatusForbidden},
		{actor: "restricted", target: "owner", want: http.StatusForbidden},
		{actor: "member", target: "owner", want: http.StatusForbidden},
		{actor: "member", target: "child", want: http.StatusForbidden},
		{actor: "owner", target: "member", want: http.StatusForbidden},
		{actor: "other", target: "child", want: http.StatusForbidden},
		{actor: "owner", target: "absent", want: http.StatusForbidden},
		{actor: "sep10", target: "member", sep10: true, want: http.StatusNoContent, dead: []string{"member"}},
		// A key may kill itself, and the cascade takes everything it minted: a register-only account has no dashboard.
		{
			actor: "owner", target: "owner", want: http.StatusNoContent,
			dead: []string{"owner", "child", "grandchild", "sibling", "restricted"},
		},
		{actor: "child", target: "child", want: http.StatusNoContent, dead: []string{"child", "grandchild"}},
		// No descendant names a leaf, so only the self rule lets it go.
		{actor: "sibling", target: "sibling", want: http.StatusNoContent, dead: []string{"sibling"}},
	}
	for _, tc := range cases {
		t.Run(tc.actor+"→"+tc.target, func(t *testing.T) {
			tr := newRevokeTree(t)
			subject := tr.subjects[tc.actor]
			if tc.sep10 {
				subject = auth.NewSEP10Session("acct:co", time.Now())
			}
			targetID := "kid_0000000000000000"
			if s, ok := tr.subjects[tc.target]; ok {
				targetID = s.KeyID
			}
			sink := &recordingAuditSink{}
			ts := newAdminTestServer(t, subject, tr.store, sink)
			resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+targetID, "", "")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusNoContent {
				if len(sink.entries) != 1 || !strings.Contains(string(sink.entries[0].Metadata), `"actor_key_id":"`+subject.KeyID+`"`) {
					t.Errorf("audit entries = %+v, want one key.revoke naming actor %q", sink.entries, subject.KeyID)
				}
			}
			dead := map[string]bool{}
			for _, n := range tc.dead {
				dead[n] = true
			}
			for name := range tr.plains {
				if got := tr.live(name); got == dead[name] {
					t.Errorf("%s live = %v, want %v", name, got, !dead[name])
				}
			}
		})
	}
}

var errNoteStore = errors.New("retry note store down")

// noteFailingStore is the real store with every retry-note read and write failing.
type noteFailingStore struct{ *auth.RedisAPIKeyStore }

func (noteFailingStore) RevokedBy(context.Context, string, string) ([]string, error) {
	return nil, errNoteStore
}

func (noteFailingStore) RecordRevoked(context.Context, string, string, []string) error {
	return errNoteStore
}

// A fault in the retry-note store must not block a revoke the live tree already authorises, and must never stand
// in for an authorisation the tree does not give.
func TestAccountKeysRevoke_RetryNoteFaultBlocksOnlyRevokesThatNeedTheNote(t *testing.T) {
	cases := []struct {
		actor, target string
		want          int
		dead          []string
	}{
		{actor: "sibling", target: "sibling", want: http.StatusNoContent, dead: []string{"sibling"}},
		{actor: "owner", target: "child", want: http.StatusNoContent, dead: []string{"child", "grandchild"}},
		{actor: "child", target: "owner", want: http.StatusInternalServerError},
		{actor: "owner", target: "absent", want: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.actor+"→"+tc.target, func(t *testing.T) {
			tr := newRevokeTree(t)
			targetID := "kid_0000000000000000"
			if s, ok := tr.subjects[tc.target]; ok {
				targetID = s.KeyID
			}
			ts := newAdminTestServer(t, tr.subjects[tc.actor], noteFailingStore{tr.store}, &recordingAuditSink{})
			resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+targetID, "", "")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			dead := map[string]bool{}
			for _, n := range tc.dead {
				dead[n] = true
			}
			for name := range tr.plains {
				if got := tr.live(name); got == dead[name] {
					t.Errorf("%s live = %v, want %v", name, got, !dead[name])
				}
			}
		})
	}
}

// A key revoking itself loses its credential with the Redis leg, so its management row is revoked first: a failed
// row leaves the key alive, and its retry completes both stores.
func TestAccountKeysRevoke_SelfRevokeFailedRowLeavesTheKeyToRetry(t *testing.T) {
	tr := newRevokeTree(t)
	self := tr.subjects["sibling"]
	platformKeys, accts := seedOwnedPlatformKey("co", self.KeyID)
	platformKeys.revokeErr = errors.New("postgres down")
	ts := newAdminTestServerWithPlatformKeys(t, self, tr.store, platformKeys, accts)
	del := func() int {
		return doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/"+self.KeyID, "leaked", "").StatusCode
	}

	if got := del(); got != http.StatusInternalServerError {
		t.Fatalf("revoke with the row failing = %d, want 500", got)
	}
	if !tr.live("sibling") {
		t.Fatal("a failed row revoke killed the key, so it can never retry and the row stays live")
	}
	platformKeys.mu.Lock()
	platformKeys.revokeErr = nil
	platformKeys.mu.Unlock()
	if got := del(); got != http.StatusNoContent {
		t.Fatalf("retry = %d, want 204", got)
	}
	if tr.live("sibling") {
		t.Error("key still authenticates after the retry")
	}
	if len(platformKeys.revokedIDs) != 1 || platformKeys.revokedIDs[0] != self.KeyID {
		t.Errorf("row revokes = %v, want [%s]", platformKeys.revokedIDs, self.KeyID)
	}
}
