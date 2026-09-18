// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RLT-404 (reverification 2026-09-18): POST /v1/account/keys built its
// [auth.CreateAPIKeyRequest] without a monthly cap, so a child minted
// from a METERED parent persisted MonthlyQuota=0 and
// middleware.MonthlyQuota's `MonthlyQuota <= 0` short-circuit left it
// UNMETERED — a metered customer could mint an uncapped credential from
// a capped one, and the cap the operator sold them was one rotation
// away from gone.
//
// The fix carries the caller's OWN effective quota onto the child. It
// copies a cap and never invents one: a parent with no cap still mints
// an uncapped child (pinned below), because the ceiling is opt-in by
// contract and defaulting it here would arm a 429 for every key that
// never had one.

// mintChildKeySubject drives the PRODUCTION chain for a self-service
// rotation — handler → [auth.RedisAPIKeyStore] →
// [auth.RedisAPIKeyValidator] — and returns the Subject the auth layer
// hands the quota middleware for the freshly minted child.
func mintChildKeySubject(t *testing.T, parent auth.Subject) auth.Subject {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ts := newAccountTestServer(t, parent, auth.NewRedisAPIKeyStore(rdb))
	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/account/keys status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Data struct {
			KeyID     string `json:"key_id"`
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if body.Data.Plaintext == "" {
		t.Fatal("mint response carried no plaintext")
	}
	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), body.Data.Plaintext)
	if err != nil {
		t.Fatalf("lookup minted child: %v", err)
	}
	return sub
}

// TestAccountKeysCreate_ChildInheritsMonthlyQuota — a child minted from
// a metered key is metered at the same ceiling. Proven red on the
// unfixed tree: Subject.MonthlyQuota came back 0.
func TestAccountKeysCreate_ChildInheritsMonthlyQuota(t *testing.T) {
	const parentQuota int64 = 250_000

	sub := mintChildKeySubject(t, auth.Subject{
		Identifier:   "acct-metered",
		Tier:         auth.TierAPIKey,
		KeyID:        "kid_parent",
		MonthlyQuota: parentQuota,
	})
	if sub.MonthlyQuota != parentQuota {
		t.Fatalf("child Subject.MonthlyQuota = %d, want %d (inherited from the metered parent; "+
			"a zero cap short-circuits middleware.MonthlyQuota and the child bills unmetered)",
			sub.MonthlyQuota, parentQuota)
	}
}

// TestAccountKeysCreate_UncappedParentMintsUncappedChild is the
// negative pin the fix must not cross: the ceiling is OPT-IN, so a
// caller without one must not have one invented for its child. Guards
// against "default the child to the tier ceiling", which would convert
// the documented opt-in contract into on-by-default and 429 keys that
// never had a cap.
func TestAccountKeysCreate_UncappedParentMintsUncappedChild(t *testing.T) {
	sub := mintChildKeySubject(t, auth.Subject{
		Identifier: "acct-uncapped",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	})
	if sub.MonthlyQuota != 0 {
		t.Fatalf("child Subject.MonthlyQuota = %d, want 0 for an uncapped parent (the cap is opt-in; "+
			"inventing one here 429s keys that never had a ceiling)", sub.MonthlyQuota)
	}
}
