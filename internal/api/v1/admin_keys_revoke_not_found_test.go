// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// GH-628: the operator kill switch answered 204 and wrote a key.revoke
// audit row for a revoke that matched nothing, so an on-call engineer
// who mistyped one character of a leaked key's identifier was told the
// incident was contained while the key kept authenticating. Driven
// through the production store so the store's not-found contract and
// the handler's mapping of it are proven together.
func TestAdminKeysRevoke_TypoedIdentifierIs404WithNoAuditRow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := auth.NewRedisAPIKeyStore(rdb)
	leaked, plaintext, err := store.Create(context.Background(),
		auth.CreateAPIKeyRequest{Identifier: "signup-9f3e", Label: "leaked"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	sink := &recordingAuditSink{}
	ts := newAdminKeyServer(t, operatorSubject(), store, sink)

	resp := adminDelete(t, ts.URL+"/v1/admin/keys/"+leaked.KeyID+"?identifier=signup-9f3f", "key in a public gist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — nothing was revoked, so the kill switch must not report success", resp.StatusCode)
	}
	for _, e := range sink.entries {
		if e.Action == "key.revoke" {
			t.Errorf("a key.revoke audit row was written for a revoke that revoked nothing: %+v", e)
		}
	}
	if _, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("the untouched key should still authenticate (test premise): %v", err)
	}

	resp = adminDelete(t, ts.URL+"/v1/admin/keys/"+leaked.KeyID+"?identifier=signup-9f3e", "key in a public gist")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("correct identifier: status = %d, want 204", resp.StatusCode)
	}
	if len(sink.entries) != 1 || sink.entries[0].Action != "key.revoke" {
		t.Errorf("audit entries after the real revoke = %+v, want exactly one key.revoke", sink.entries)
	}
}
