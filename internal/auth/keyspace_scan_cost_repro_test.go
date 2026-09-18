// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// commandCounter is a go-redis hook that tallies the commands a store
// method actually sends, so the test measures Redis work rather than
// inferring it from the source.
type commandCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *commandCounter) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (c *commandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.add(cmd.Name())
		return next(ctx, cmd)
	}
}

func (c *commandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			c.add(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (c *commandCounter) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[strings.ToLower(name)]++
}

func (c *commandCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = map[string]int{}
}

func (c *commandCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// TestKeyLookupsDoNotWalkTheKeyspace is the numeric reproduction of
// findings F057 / K051, and the regression guard for their fix.
//
// THE DEFECT. Four store methods answer "which record has this owner /
// this KeyID" by SCANning `apikey:*` and issuing one GET per match:
// ListKeysForIdentifier and RevokeKeyByID (list_keys.go),
// UpdateRateLimit (store_update.go) and MarkEmailVerified
// (store_mark_email_verified.go). Three sit on request paths any
// anonymously-registered free key can drive — GET / POST / DELETE
// /v1/account/keys — so the cost is O(every credential in the
// deployment) per request against the single-threaded Redis that is
// also the rate limiter, and open registration makes that keyspace
// attacker-sized. SCAN additionally walks every OTHER key family in the
// DB, because MATCH filters after the cursor step.
//
// THE PROPERTY. Work must be proportional to the caller's OWN keys: no
// SCAN at all, and a GET count bounded by the records the caller owns.
//
// THE FIX. Both issuance writers (Create, CreateWithSecret) write the
// record and its entries in the `apikey-index:v1` hash as one atomic
// step, and the four lookups read that hash. Records that predate the
// index are covered by a build the first lookup runs — ONE walk per
// index lifetime, which the "index is built once" subtest pins. The
// fixture makes one of the caller's two keys such a legacy record, so
// the numbers below also prove the build makes old keys reachable.
//
// Measured on the unfixed code with this same fixture: 302 / 106 / 106
// / 139 GETs and one SCAN per call. The budget is unchanged from the
// reproduction as first committed.
func TestKeyLookupsDoNotWalkTheKeyspace(t *testing.T) {
	mr := miniredis.RunT(t)
	counter := &commandCounter{counts: map[string]int{}}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(counter)

	ctx := context.Background()
	store := NewRedisAPIKeyStore(rdb)

	// The victim deployment: other customers' credentials plus the
	// unrelated families that share the DB.
	const foreignKeys = 300
	for i := 0; i < foreignKeys; i++ {
		if _, _, err := store.Create(ctx, CreateAPIKeyRequest{
			Identifier: fmt.Sprintf("account:other-%d", i),
			Tier:       TierAPIKey,
		}); err != nil {
			t.Fatalf("seed foreign key %d: %v", i, err)
		}
	}
	for i := 0; i < 500; i++ {
		if err := mr.Set(fmt.Sprintf("rl:bucket:%d", i), "1"); err != nil {
			t.Fatalf("seed unrelated key: %v", err)
		}
	}

	// The caller owns exactly two keys. The first is a LEGACY record:
	// written raw, the way a binary that predates the index wrote it,
	// so nothing in the index knows it until a build runs.
	const owner = "account:caller"
	legacy := APIKeyRecord{KeyID: "kid_legacy0000000001", Identifier: owner, Tier: TierAPIKey, PermissionsAll: true}
	legacyBody, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	legacyRecordKey := cachekeys.APIKey(hashAPIKey("legacy-fixture-not-a-credential")).String()
	if err := rdb.Set(ctx, legacyRecordKey, legacyBody, 0).Err(); err != nil {
		t.Fatalf("seed legacy record: %v", err)
	}
	minted, _, err := store.Create(ctx, CreateAPIKeyRequest{Identifier: owner, Tier: TierAPIKey})
	if err != nil {
		t.Fatalf("seed own key: %v", err)
	}
	ownKeyIDs := []string{legacy.KeyID, minted.KeyID}

	// The index is built by the first lookup that finds it absent, and
	// never again: the walk is a one-time migration cost, not a
	// per-request one. (On the unfixed code both calls walk.)
	t.Run("index is built once", func(t *testing.T) {
		for call, wantScans := range []int{1, 0} {
			counter.reset()
			if _, err := store.ListKeysForIdentifier(ctx, "account:nobody"); err != nil {
				t.Fatalf("warm-up lookup %d: %v", call, err)
			}
			if got := counter.snapshot()["scan"]; got != wantScans {
				t.Errorf("warm-up lookup %d issued %d SCAN command(s), want %d", call, got, wantScans)
			}
		}
	})

	// A lookup may touch the caller's own records plus a constant
	// number of index reads — never the foreign population.
	const getBudget = 2 + 2

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"ListKeysForIdentifier", func() error {
			got, err := store.ListKeysForIdentifier(ctx, owner)
			if err == nil && len(got) != 2 {
				return fmt.Errorf("listed %d keys, want 2", len(got))
			}
			return err
		}},
		{"UpdateRateLimit", func() error {
			_, err := store.UpdateRateLimit(ctx, ownKeyIDs[0], 120)
			return err
		}},
		{"MarkEmailVerified", func() error {
			_, err := store.MarkEmailVerified(ctx, ownKeyIDs[0], time.Unix(1_700_000_000, 0).UTC())
			return err
		}},
		{"RevokeKeyByID", func() error {
			return store.RevokeKeyByID(ctx, owner, ownKeyIDs[1])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter.reset()
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := counter.snapshot()
			t.Logf("%s sent %v", tc.name, got)
			if got["scan"] != 0 {
				t.Errorf("%s issued %d SCAN command(s) over a keyspace of %d foreign credentials + 500 "+
					"unrelated keys; a per-request lookup must not walk the keyspace",
					tc.name, got["scan"], foreignKeys)
			}
			if got["get"] > getBudget {
				t.Errorf("%s issued %d GETs for a caller owning 2 keys (budget %d): cost scales with "+
					"every credential in the deployment, not with the caller's own",
					tc.name, got["get"], getBudget)
			}
		})
	}
}
