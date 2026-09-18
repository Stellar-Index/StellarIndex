// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// scanCostReproEnv opts in to the reproduction below. It is OFF in the
// suite because the test is RED by design: it documents a defect
// (findings F057 / K051) whose fix needs files outside the unit that
// wrote it — see the NEEDS-COORDINATION note on the test.
const scanCostReproEnv = "STELLARINDEX_F057_REPRO"

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
// findings F057 / K051, committed as evidence. Run it with
//
//	STELLARINDEX_F057_REPRO=1 go test ./internal/auth/ -run TestKeyLookupsDoNotWalkTheKeyspace -v
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
// WHY THIS IS NOT FIXED HERE (NEEDS-COORDINATION). The fix is a
// per-owner and a per-KeyID index written AT ISSUANCE, and both
// issuance writers are outside this unit's file set:
//
//   - internal/auth/store.go         — Create (POST /v1/account/keys, /v1/signup, ops mint)
//   - internal/auth/store_mirror.go  — CreateWithSecret (POST /v1/register mirror)
//
// plus the places a new Redis key family has to be declared:
//
//   - internal/cachekeys/keys.go     — typed key family (ADR-0007 guard)
//   - configs/ansible/roles/redis-sentinel/templates/users.acl.j2 — the
//     API's Redis ACL allow-lists key patterns (`~apikey:*` …); an index
//     family outside the list is NOPERM in production, and an index
//     write that fails inside Create would take key issuance down.
//
// An index maintained only by the readers in this unit cannot be
// correct: a key minted after a one-time backfill would be invisible to
// list / revoke / clamp, which turns a cost defect into a revocation
// that silently no-ops. The design also has to settle: backfilling the
// records that exist today (including operator-seeded ones written
// outside the store), pruning index members whose record TTL'd out (the
// register mirror carries a 90-day idle TTL), and keeping the index
// write and the record write atomic (MULTI or a script).
func TestKeyLookupsDoNotWalkTheKeyspace(t *testing.T) {
	if os.Getenv(scanCostReproEnv) == "" {
		t.Skipf("reproduction of open findings F057/K051 (red by design); set %s=1 to run", scanCostReproEnv)
	}

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

	// The caller owns exactly two keys.
	const owner = "account:caller"
	var ownKeyIDs []string
	for i := 0; i < 2; i++ {
		rec, _, err := store.Create(ctx, CreateAPIKeyRequest{Identifier: owner, Tier: TierAPIKey})
		if err != nil {
			t.Fatalf("seed own key: %v", err)
		}
		ownKeyIDs = append(ownKeyIDs, rec.KeyID)
	}

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
