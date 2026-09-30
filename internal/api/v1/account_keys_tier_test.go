// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestAccountKeysCreate_SEP10TierForbidden — a SEP-10 subject is not an
// account and must not mint child keys; the store is never touched.
func TestAccountKeysCreate_SEP10TierForbidden(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	subject := auth.Subject{
		Identifier: "GCLIENTWALLETADDRESS",
		Tier:       auth.TierSEP10,
	}
	ts := newAdminTestServer(t, subject, store, &recordingAuditSink{})

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"wallet"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (sep10 subject minting a key)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create called %d times, want 0", store.calls)
	}
}

// TestAccountKeysCreate_APIKeyTierStillMints — the tier gate admits the
// customer tier.
func TestAccountKeysCreate_APIKeyTierStillMints(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	subject := auth.Subject{
		Identifier: "acct:customer-1",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	}
	ts := newAdminTestServer(t, subject, store, &recordingAuditSink{})

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Errorf("Create called %d times, want 1", store.calls)
	}
}

// TestAccountKeysCreate_ChildRecordHasIdleTTL — the record behind a key
// minted through the endpoint carries a Redis TTL, so an abandoned
// self-service key cannot occupy the keyspace forever.
func TestAccountKeysCreate_ChildRecordHasIdleTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	subject := auth.Subject{Identifier: "acct:customer-ttl", Tier: auth.TierAPIKey, KeyID: "kid_parent"}
	ts := newAccountTestServer(t, subject, auth.NewRedisAPIKeyStore(rdb))
	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	sum := sha256.Sum256([]byte(body.Data.Plaintext))
	key := cachekeys.APIKey(hex.EncodeToString(sum[:])).String()
	if !mr.Exists(key) {
		t.Fatalf("minted record %s not found", key)
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > auth.MirroredKeyIdleTTL {
		t.Fatalf("minted record TTL = %s, want in (0, %s]", ttl, auth.MirroredKeyIdleTTL)
	}
}
