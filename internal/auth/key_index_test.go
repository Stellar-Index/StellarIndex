// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// These tests cover what the cost test cannot: the states in which the
// API-key lookup index (F057 / K051) is NOT simply present and right.
// Each one is a way a lookup index turns a cost defect into a security
// one — a live credential that list / revoke / the tier clamp cannot
// see — so each asserts that the credential is still found and still
// dies on revoke.

// indexDenier is a go-redis hook that rejects every command naming a
// key of the `apikey-index:*` family, the way a lockdown Redis ACL that
// has not been given `~apikey-index:*` does. The reply text is the one
// a real server sends (see test/integration/apikey_index_acl_test.go,
// which proves the same behaviour against redis-server itself).
type indexDenier struct {
	deny   atomic.Bool
	denied atomic.Int64
	// failWith, when set, replaces the NOPERM reply — used to model a
	// transient fault on the same commands.
	failWith atomic.Pointer[string]
}

func (d *indexDenier) reject(cmd redis.Cmder) error {
	if !d.deny.Load() {
		return nil
	}
	for _, a := range cmd.Args() {
		s, ok := a.(string)
		if !ok || !strings.HasPrefix(s, "apikey-index:") {
			continue
		}
		d.denied.Add(1)
		msg := "NOPERM No permissions to access a key"
		if p := d.failWith.Load(); p != nil {
			msg = *p
		}
		err := errors.New(msg)
		cmd.SetErr(err)
		return err
	}
	return nil
}

func (d *indexDenier) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (d *indexDenier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := d.reject(cmd); err != nil {
			return err
		}
		return next(ctx, cmd)
	}
}

func (d *indexDenier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if err := d.reject(cmd); err != nil {
				return err
			}
		}
		return next(ctx, cmds)
	}
}

type keyIndexFixture struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	store   *RedisAPIKeyStore
	denier  *indexDenier
	counter *commandCounter
}

func newKeyIndexFixture(t *testing.T) *keyIndexFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f := &keyIndexFixture{
		mr:      mr,
		rdb:     rdb,
		denier:  &indexDenier{},
		counter: &commandCounter{counts: map[string]int{}},
	}
	// Counter first so it also sees the commands the denier rejects.
	rdb.AddHook(f.counter)
	rdb.AddHook(f.denier)
	f.store = NewRedisAPIKeyStore(rdb)
	return f
}

func (f *keyIndexFixture) mint(t *testing.T, identifier string) (APIKeyRecord, string) {
	t.Helper()
	rec, plaintext, err := f.store.Create(context.Background(), CreateAPIKeyRequest{Identifier: identifier})
	if err != nil {
		t.Fatalf("Create(%s): %v", identifier, err)
	}
	return rec, plaintext
}

func (f *keyIndexFixture) authenticates(plaintext string) bool {
	_, err := NewRedisAPIKeyValidator(f.rdb).Lookup(context.Background(), plaintext)
	return err == nil
}

func (f *keyIndexFixture) indexField(t *testing.T, field string) (string, bool) {
	t.Helper()
	v, err := f.rdb.HGet(context.Background(), cachekeys.APIKeyIndex().String(), field).Result()
	if errors.Is(err, redis.Nil) {
		return "", false
	}
	if err != nil {
		t.Fatalf("HGET %s: %v", field, err)
	}
	return v, true
}

func listedKeyIDs(t *testing.T, store *RedisAPIKeyStore, identifier string) []string {
	t.Helper()
	recs, err := store.ListKeysForIdentifier(context.Background(), identifier)
	if err != nil {
		t.Fatalf("ListKeysForIdentifier(%s): %v", identifier, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.KeyID)
	}
	return ids
}

func sameKeyIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestKeyIndex_ACLDenied_IssuanceAndRevocationStillWork is the rollout
// state: the binary is deployed, the Redis ACL template that admits
// `apikey-index:*` has not been applied. Issuance must not go down,
// and — the half that matters — a key minted in this state must be
// listable and REVOCABLE, both now and after the ACL is applied.
func TestKeyIndex_ACLDenied_IssuanceAndRevocationStillWork(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	f.denier.deny.Store(true)

	const owner = "account:rollout"
	first, firstPlain := f.mint(t, owner)
	second, secondPlain := f.mint(t, owner)
	if !f.authenticates(firstPlain) || !f.authenticates(secondPlain) {
		t.Fatal("a key issued while the index family is ACL-denied does not authenticate: issuance is down")
	}
	if f.denier.denied.Load() == 0 {
		t.Fatal("the denier rejected nothing: this test is not exercising the denied path")
	}
	if f.mr.Exists(cachekeys.APIKeyIndex().String()) {
		t.Fatal("index key exists although every access to it was denied")
	}

	if got := listedKeyIDs(t, f.store, owner); !sameKeyIDs(got, first.KeyID, second.KeyID) {
		t.Fatalf("list while denied = %v, want both keys (the walk must still answer)", got)
	}
	if _, err := f.store.UpdateRateLimit(ctx, first.KeyID, 7); err != nil {
		t.Fatalf("UpdateRateLimit while denied: %v", err)
	}
	if err := f.store.RevokeKeyByID(ctx, owner, first.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID while denied: %v", err)
	}
	if f.authenticates(firstPlain) {
		t.Fatal("revoked key still authenticates while the index is ACL-denied: revocation silently no-ops")
	}

	// The operator applies the ACL. The key minted during the gap was
	// never indexed at issuance; the build has to pick it up.
	f.denier.deny.Store(false)
	if got := listedKeyIDs(t, f.store, owner); !sameKeyIDs(got, second.KeyID) {
		t.Fatalf("list after the ACL is applied = %v, want [%s]", got, second.KeyID)
	}
	if _, ok := f.indexField(t, keyIndexReadyField); !ok {
		t.Fatal("index not marked ready after the first lookup with the ACL applied")
	}
	if err := f.store.RevokeKeyByID(ctx, owner, second.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID after the ACL is applied: %v", err)
	}
	if f.authenticates(secondPlain) {
		t.Fatal("a key minted during the ACL gap survives revocation once the index is live")
	}
}

// TestKeyIndex_EvictionFallsBackInsteadOfHidingKeys pins the reason the
// index is ONE hash. Production Redis runs allkeys-lru: if the index
// is evicted its `ready` marker must go with it, so the next lookup
// rebuilds instead of trusting an empty index and reporting that a
// live credential does not exist.
func TestKeyIndex_EvictionFallsBackInsteadOfHidingKeys(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	const owner = "account:evicted"
	rec, plaintext := f.mint(t, owner)
	if got := listedKeyIDs(t, f.store, owner); !sameKeyIDs(got, rec.KeyID) {
		t.Fatalf("list before eviction = %v", got)
	}

	if !f.mr.Del(cachekeys.APIKeyIndex().String()) {
		t.Fatal("index key was not there to evict")
	}

	if err := f.store.RevokeKeyByID(ctx, owner, rec.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID after eviction: %v", err)
	}
	if f.authenticates(plaintext) {
		t.Fatal("after the index was evicted, revoke reported success and the key still authenticates")
	}
}

// TestKeyIndex_IssuanceAfterReadyNeedsNoWalk — once the index is ready,
// a newly minted key is visible through it immediately, with no SCAN:
// the record and its entries land in one atomic write.
func TestKeyIndex_IssuanceAfterReadyNeedsNoWalk(t *testing.T) {
	f := newKeyIndexFixture(t)
	const owner = "account:steady"
	first, _ := f.mint(t, owner)
	_ = listedKeyIDs(t, f.store, owner) // builds

	second, _ := f.mint(t, owner)
	mirrored := MirroredKey{
		Plaintext: "sip_" + strings.Repeat("ab", 32),
		Record:    APIKeyRecord{KeyID: "kid_mirrored01", Identifier: owner},
	}
	if err := f.store.CreateWithSecret(context.Background(), mirrored); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	f.counter.reset()
	got := listedKeyIDs(t, f.store, owner)
	if !sameKeyIDs(got, first.KeyID, second.KeyID, mirrored.Record.KeyID) {
		t.Fatalf("list = %v, want all three keys", got)
	}
	if n := f.counter.snapshot()["scan"]; n != 0 {
		t.Fatalf("listing after issuance issued %d SCAN(s): issuance is not indexing its own records", n)
	}
	ttl := f.mr.TTL(cachekeys.APIKey(hashAPIKey(mirrored.Plaintext)).String())
	if ttl != MirroredKeyIdleTTL {
		t.Fatalf("mirrored record TTL = %v, want %v: the indexed write dropped the idle TTL", ttl, MirroredKeyIdleTTL)
	}
	if ttl := f.mr.TTL(cachekeys.APIKeyIndex().String()); ttl != 0 {
		t.Fatalf("index TTL = %v, want none: an expiring index hides live keys", ttl)
	}
}

// TestKeyIndex_OwnershipIsCheckedAgainstTheRecord — the index only says
// where to look. Entries forged into another owner's list, or a KeyID
// pointer aimed at someone else's record, must neither disclose nor
// revoke that record.
func TestKeyIndex_OwnershipIsCheckedAgainstTheRecord(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	victim, victimPlain := f.mint(t, "account:victim")
	_ = listedKeyIDs(t, f.store, "account:victim") // builds

	index := cachekeys.APIKeyIndex().String()
	victimHash := hashAPIKey(victimPlain)
	f.mr.HSet(index, keyIndexOwnerPrefix+"account:attacker", victimHash)

	if got := listedKeyIDs(t, f.store, "account:attacker"); len(got) != 0 {
		t.Fatalf("a forged owner entry disclosed another account's key: %v", got)
	}
	if err := f.store.RevokeKeyByID(ctx, "account:attacker", victim.KeyID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("RevokeKeyByID on another owner's key = %v, want ErrKeyNotFound", err)
	}
	if !f.authenticates(victimPlain) {
		t.Fatal("a caller revoked a key it does not own")
	}
	if ptr, ok := f.indexField(t, keyIndexKeyIDPrefix+victim.KeyID); !ok || ptr != victimHash {
		t.Fatalf("a refused cross-account revoke disturbed the victim's index pointer: %q, %v", ptr, ok)
	}
}

// TestKeyIndex_DanglingEntriesAreSkippedAndPruned — a record can vanish
// without the store hearing of it (the mirrored-key idle TTL, the
// Postgres-backend cache invalidator). The entry left behind must not
// surface as a key, and must not survive being noticed.
func TestKeyIndex_DanglingEntriesAreSkippedAndPruned(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	const owner = "account:dangling"
	gone, gonePlain := f.mint(t, owner)
	kept, _ := f.mint(t, owner)
	_ = listedKeyIDs(t, f.store, owner) // builds

	f.mr.Del(cachekeys.APIKey(hashAPIKey(gonePlain)).String())

	if got := listedKeyIDs(t, f.store, owner); !sameKeyIDs(got, kept.KeyID) {
		t.Fatalf("list = %v, want only the surviving key", got)
	}
	owned, _ := f.indexField(t, keyIndexOwnerPrefix+owner)
	if strings.Contains(owned, hashAPIKey(gonePlain)) {
		t.Fatalf("owner list still names the vanished record after a list: %q", owned)
	}
	if _, err := f.store.UpdateRateLimit(ctx, gone.KeyID, 5); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("UpdateRateLimit on a vanished record = %v, want ErrKeyNotFound", err)
	}
	if _, ok := f.indexField(t, keyIndexKeyIDPrefix+gone.KeyID); ok {
		t.Fatal("KeyID pointer to a vanished record was not pruned once a lookup found it dangling")
	}
}

// TestKeyIndex_RevokeRemovesRecordAndEntries closes the writer→reader
// loop this change opened: what issuance writes, revocation must take
// back, or the index grows a tombstone per revoked key.
func TestKeyIndex_RevokeRemovesRecordAndEntries(t *testing.T) {
	f := newKeyIndexFixture(t)
	const owner = "account:rollback"
	_ = listedKeyIDs(t, f.store, owner) // builds (empty deployment)
	rec, plaintext := f.mint(t, owner)

	if err := f.store.RevokeKeyByID(context.Background(), owner, rec.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	if f.authenticates(plaintext) {
		t.Fatal("revoked key still authenticates")
	}
	if _, ok := f.indexField(t, keyIndexKeyIDPrefix+rec.KeyID); ok {
		t.Error("KeyID pointer survived the revoke")
	}
	if owned, ok := f.indexField(t, keyIndexOwnerPrefix+owner); ok {
		t.Errorf("owner list survived the revoke of its only key: %q", owned)
	}
	if _, ok := f.indexField(t, keyIndexReadyField); !ok {
		t.Error("revoking the last key took the ready marker with it")
	}
}

// TestKeyIndex_TransientIndexFailureIssuesNothing — only an ACL denial
// may fall back to the record-only write. Any other failure of the
// indexed write must fail issuance with NOTHING written: a record the
// trusted index does not know is exactly the unrevocable key.
func TestKeyIndex_TransientIndexFailureIssuesNothing(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	_ = listedKeyIDs(t, f.store, "account:anyone") // builds: the index is trusted from here

	fault := "LOADING Redis is loading the dataset in memory"
	f.denier.failWith.Store(&fault)
	f.denier.deny.Store(true)
	f.counter.reset()

	_, plaintext, err := f.store.Create(ctx, CreateAPIKeyRequest{Identifier: "account:unlucky"})
	if err == nil {
		t.Fatal("Create succeeded although the indexed write failed with a non-ACL error")
	}
	if plaintext != "" {
		t.Fatal("Create surfaced a plaintext alongside an error")
	}
	if n := f.counter.snapshot()["set"]; n != 0 {
		t.Fatalf("Create fell back to %d plain SET(s) on a non-ACL failure", n)
	}
	for _, k := range f.mr.Keys() {
		if strings.HasPrefix(k, "apikey:") {
			t.Fatalf("a record was written despite the failed issuance: %s", k)
		}
	}
}

// TestKeyIndex_BuildIsSingleFlightAndLookupsStayCorrect — while another
// process holds the build lock, a lookup must neither wait for it nor
// trust the half-built index: it walks.
func TestKeyIndex_BuildIsSingleFlightAndLookupsStayCorrect(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	const owner = "account:busy"
	// Lock first: issuance reads the index too, so the mint itself must
	// run against the held lock rather than build the index ahead of it.
	if err := f.mr.Set(cachekeys.APIKeyIndexBuildLock().String(), "held-by-another-process"); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	rec, _ := f.mint(t, owner)

	if _, err := f.store.BuildKeyIndex(ctx); !errors.Is(err, ErrKeyIndexBuildBusy) {
		t.Fatalf("BuildKeyIndex under a held lock = %v, want ErrKeyIndexBuildBusy", err)
	}
	if got := listedKeyIDs(t, f.store, owner); !sameKeyIDs(got, rec.KeyID) {
		t.Fatalf("list while another process builds = %v, want [%s]", got, rec.KeyID)
	}
	if _, ok := f.indexField(t, keyIndexReadyField); ok {
		t.Fatal("index marked ready by a process that did not hold the build lock")
	}

	f.mr.Del(cachekeys.APIKeyIndexBuildLock().String())
	n, err := f.store.BuildKeyIndex(ctx)
	if err != nil {
		t.Fatalf("BuildKeyIndex: %v", err)
	}
	if n != 1 {
		t.Fatalf("BuildKeyIndex indexed %d records, want 1", n)
	}
	if f.mr.Exists(cachekeys.APIKeyIndexBuildLock().String()) {
		t.Fatal("a successful build left its lock behind")
	}
}

// TestKeyIndex_BuildIndexesALargeLegacyPopulation crosses the pipeline
// batch boundary with records written the way a pre-index binary wrote
// them, and checks every one is reachable by KeyID afterwards.
func TestKeyIndex_BuildIndexesALargeLegacyPopulation(t *testing.T) {
	f := newKeyIndexFixture(t)
	ctx := context.Background()
	const population = keyIndexBuildBatch*2 + 37
	for i := 0; i < population; i++ {
		body := fmt.Sprintf(`{"key_id":"kid_legacy_%d","identifier":"account:legacy-%d","tier":"apikey"}`, i, i%50)
		if err := f.mr.Set(cachekeys.APIKey(hashAPIKey(fmt.Sprintf("legacy-fixture-%d", i))).String(), body); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// One undecodable record must not abort the build.
	if err := f.mr.Set(cachekeys.APIKey("corrupt").String(), "{not json"); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}

	n, err := f.store.BuildKeyIndex(ctx)
	if err != nil {
		t.Fatalf("BuildKeyIndex: %v", err)
	}
	if n != population {
		t.Fatalf("BuildKeyIndex indexed %d of %d records", n, population)
	}
	for _, i := range []int{0, keyIndexBuildBatch - 1, keyIndexBuildBatch, population - 1} {
		if _, err := f.store.UpdateRateLimit(ctx, fmt.Sprintf("kid_legacy_%d", i), 9); err != nil {
			t.Errorf("legacy record %d unreachable after the build: %v", i, err)
		}
	}
	if got := listedKeyIDs(t, f.store, "account:legacy-7"); len(got) != (population+42)/50 {
		t.Errorf("owner legacy-7 lists %d keys, want %d", len(got), (population+42)/50)
	}
}
