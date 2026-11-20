// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// failSecondRemoval fails the second record removal (removeIndexedRecordScript with delete-record "1") once.
type failSecondRemoval struct {
	armed     bool
	succeeded int
}

func (*failSecondRemoval) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *failSecondRemoval) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		name := strings.ToLower(cmd.Name())
		isRemoval := (name == "evalsha" || name == "eval") && len(args) == 9 && args[8] == "1"
		if !isRemoval || !f.armed {
			return next(ctx, cmd)
		}
		if f.succeeded == 1 {
			f.armed = false
			err := errors.New("injected redis failure")
			cmd.SetErr(err)
			return err
		}
		err := next(ctx, cmd)
		if err == nil {
			f.succeeded++
		}
		return err
	}
}

func (*failSecondRemoval) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type lineageFixture struct {
	store     *RedisAPIKeyStore
	validator *RedisAPIKeyValidator
	mr        *miniredis.Miniredis
	rdb       *redis.Client
	fail      *failSecondRemoval
}

func newLineageFixture(t *testing.T) *lineageFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f := &failSecondRemoval{}
	rdb.AddHook(f)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &lineageFixture{
		store:     NewRedisAPIKeyStore(rdb, WithStoreClock(func() time.Time { return now })),
		validator: NewRedisAPIKeyValidator(rdb, WithClock(func() time.Time { return now })),
		mr:        mr,
		rdb:       rdb,
		fail:      f,
	}
}

// mint creates req and returns its record, plaintext and the Subject it authenticates as.
func (f *lineageFixture) mint(t *testing.T, req CreateAPIKeyRequest) (APIKeyRecord, string, Subject) {
	t.Helper()
	rec, plain, err := f.store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := f.validator.Lookup(context.Background(), plain)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return rec, plain, sub
}

// legacyChild mints a key and strips its chain, leaving only ParentKeyID as records written before the chain do.
func (f *lineageFixture) legacyChild(t *testing.T, parent Subject) (APIKeyRecord, string, Subject) {
	t.Helper()
	rec, plain, _ := f.mint(t, CreateAPIKeyRequest{Identifier: parent.Identifier, ParentKeyID: parent.KeyID})
	rec.AncestorKeyIDs = nil
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.writeRecord(context.Background(), hashAPIKey(plain), rec, body, 0); err != nil {
		t.Fatalf("strip chain: %v", err)
	}
	sub, err := f.validator.Lookup(context.Background(), plain)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return rec, plain, sub
}

// TestCreate_LegacyMinterGivesItsChildTheFullChain: a key minted by a pre-chain key carries the chain walked through
// stored records, so revoking the root still reaches it after the key between them is gone.
func TestCreate_LegacyMinterGivesItsChildTheFullChain(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	mid, midPlain, midSub := f.legacyChild(t, rootSub)
	leaf, leafPlain, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	if want := []string{root.KeyID, mid.KeyID}; !slices.Equal(leaf.AncestorKeyIDs, want) {
		t.Fatalf("leaf chain = %v, want %v", leaf.AncestorKeyIDs, want)
	}
	f.mr.Del(cachekeys.APIKey(hashAPIKey(midPlain)).String())
	if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	f.assertDead(t, map[string]string{"leaf": leafPlain})
}

// TestCreate_LegacyMinterWithAMissingAncestorIsRefused: when the walk cannot reach the root, no partial chain is stored.
func TestCreate_LegacyMinterWithAMissingAncestorIsRefused(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	_, _, midSub := f.legacyChild(t, rootSub)
	f.mr.Del(cachekeys.APIKey(hashAPIKey(rootPlain)).String())
	if _, _, err := f.store.Create(ctx, ChildKeyRequest(midSub, "leaf", nil)); !errors.Is(err, ErrMinterNotLive) {
		t.Fatalf("Create under a legacy key whose root is gone = %v, want ErrMinterNotLive", err)
	}
}

// seedMinter writes a live record for sub, as one the validator read it from, so sub may delegate.
func seedMinter(t *testing.T, s *RedisAPIKeyStore, sub Subject) string {
	t.Helper()
	rec := recordFromSubject(sub)
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	hash := hashAPIKey("sip_fixture-minter-" + sub.KeyID)
	if err := s.writeRecord(context.Background(), hash, rec, body, 0); err != nil {
		t.Fatalf("seed minter: %v", err)
	}
	return hash
}

func (f *lineageFixture) assertDead(t *testing.T, what map[string]string) {
	t.Helper()
	for name, plain := range what {
		if _, err := f.validator.Lookup(context.Background(), plain); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Lookup err = %v, want ErrUnauthorized", name, err)
		}
	}
}

// TestChildRecordsItsAncestorChain: a delegated key stores its minter's chain plus the minter, root first.
func TestChildRecordsItsAncestorChain(t *testing.T) {
	f := newLineageFixture(t)
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	mid, _, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	leaf, _, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	if len(root.AncestorKeyIDs) != 0 || !slices.Equal(mid.AncestorKeyIDs, []string{root.KeyID}) ||
		!slices.Equal(leaf.AncestorKeyIDs, []string{root.KeyID, mid.KeyID}) {
		t.Fatalf("ancestors root/mid/leaf = %v/%v/%v, want []/[root]/[root mid]", root.AncestorKeyIDs, mid.AncestorKeyIDs, leaf.AncestorKeyIDs)
	}
}

// TestBoundAncestorKeyIDs_RefusesAnyChainButTheMintersOwn: a child cannot shed an ancestor, and a non-delegated key carries none.
func TestBoundAncestorKeyIDs_RefusesAnyChainButTheMintersOwn(t *testing.T) {
	m := Subject{KeyID: "kid_mid", Identifier: "acct-x", AncestorKeyIDs: []string{"kid_root"}}
	cases := []struct {
		name   string
		rec    APIKeyRecord
		wantOK bool
	}{
		{"exact chain", APIKeyRecord{ParentKeyID: "kid_mid", AncestorKeyIDs: []string{"kid_root", "kid_mid"}}, true},
		{"dropped root", APIKeyRecord{ParentKeyID: "kid_mid", AncestorKeyIDs: []string{"kid_mid"}}, false},
		{"no chain on a delegated key", APIKeyRecord{ParentKeyID: "kid_mid"}, false},
		{"chain on a non-delegated key", APIKeyRecord{AncestorKeyIDs: []string{"kid_root"}}, false},
		{"non-delegated", APIKeyRecord{}, true},
	}
	for _, tc := range cases {
		err := boundAncestorKeyIDs(m, tc.rec)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.wantOK)
		}
	}
}

// TestRevoke_RetryAfterMidwayFailureRemovesEveryDescendant: a revoke that fails part-way through the
// cascade leaves every remaining descendant reachable, so the retry kills the whole chain.
func TestRevoke_RetryAfterMidwayFailureRemovesEveryDescendant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"ancestor chain", false}, {"legacy ParentKeyID only", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLineageFixture(t)
			ctx := context.Background()
			root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
			child := func(p Subject, label string) (string, Subject) {
				if tc.legacy {
					_, plain, sub := f.legacyChild(t, p)
					return plain, sub
				}
				_, plain, sub := f.mint(t, ChildKeyRequest(p, label, nil))
				return plain, sub
			}
			midPlain, midSub := child(rootSub, "mid")
			leafPlain, _ := child(midSub, "leaf")

			f.fail.armed = true
			if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err == nil {
				t.Fatal("RevokeKeyByID with an injected failure returned nil")
			}
			if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
				t.Fatalf("retry RevokeKeyByID: %v", err)
			}
			f.assertDead(t, map[string]string{"mid": midPlain, "leaf": leafPlain})
		})
	}
}

// TestRevoke_SweepsWhenTheRootHasNoRecord: a key whose record here is gone (a Postgres-backed key
// whose mirror lapsed) still takes its delegated keys with it.
func TestRevoke_SweepsWhenTheRootHasNoRecord(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	pgKey := Subject{KeyID: "pg-row-1", Identifier: "acct-x", Tier: TierAPIKey, AllowAllPermissions: true}
	mirror := seedMinter(t, f.store, pgKey)
	_, childPlain, childSub := f.mint(t, ChildKeyRequest(pgKey, "child", nil))
	_, grandPlain, _ := f.mint(t, ChildKeyRequest(childSub, "grand", nil))
	f.mr.Del(cachekeys.APIKey(mirror).String())

	if err := f.store.RevokeKeyByID(ctx, "acct-x", pgKey.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID of a record-less root with descendants: %v, want nil", err)
	}
	f.assertDead(t, map[string]string{"child": childPlain, "grandchild": grandPlain})
	if err := f.store.RevokeKeyByID(ctx, "acct-x", pgKey.KeyID); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("second revoke = %v, want ErrKeyNotFound (no root and nothing left)", err)
	}
}

// TestRevoke_WritesNothingForAKeyTheCallerDoesNotHold: a revoke of an unknown, oversized or foreign key id
// changes no state, so neither a flood of junk ids nor another account's id leaves a revoke marker behind.
func TestRevoke_WritesNothingForAKeyTheCallerDoesNotHold(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	foreign, _, foreignSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-victim"})
	_, foreignChildPlain, _ := f.mint(t, ChildKeyRequest(foreignSub, "child", nil))
	before := f.mr.Keys()
	for name, id := range map[string]string{
		"unknown":   "kid_0123456789abcdef",
		"oversized": "kid_" + strings.Repeat("a", 512<<10),
		"foreign":   foreign.KeyID,
	} {
		if _, err := f.store.RevokeKeyTree(ctx, "acct-attacker", id); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("%s: RevokeKeyTree = %v, want ErrKeyNotFound", name, err)
		}
	}
	if after := f.mr.Keys(); !slices.Equal(before, after) {
		t.Fatalf("keyspace changed by refused revokes: before %v, after %v", before, after)
	}
	if _, err := f.validator.Lookup(ctx, foreignChildPlain); err != nil {
		t.Fatalf("foreign child after the refused revoke: %v, want live", err)
	}
	if _, _, err := f.store.Create(ctx, ChildKeyRequest(foreignSub, "after", nil)); err != nil {
		t.Fatalf("foreign key minting after the refused revoke: %v, want nil", err)
	}
}

// TestCreate_DelegatedMintAfterItsMinterIsRevokedIsRefused: a mint authenticated before its minter
// was revoked, but writing after, must not leave a live child of the revoked key.
func TestCreate_DelegatedMintAfterItsMinterIsRevokedIsRefused(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	if _, _, err := f.store.Create(ctx, ChildKeyRequest(rootSub, "late", nil)); !errors.Is(err, ErrMinterNotLive) {
		t.Fatalf("Create from a revoked minter = %v, want ErrMinterNotLive", err)
	}
	if _, _, err := f.store.CreateCapped(ctx, ChildKeyRequest(rootSub, "late", nil), 25); !errors.Is(err, ErrMinterNotLive) {
		t.Fatalf("CreateCapped from a revoked minter = %v, want ErrMinterNotLive", err)
	}
	if recs, err := f.store.ListKeysForIdentifier(ctx, "acct-x"); err != nil || len(recs) != 0 {
		t.Fatalf("keys after refused mints = %d (err %v), want none", len(recs), err)
	}
}

// beforeCmd runs run once, just before the first command match accepts.
type beforeCmd struct {
	match func(args []any) bool
	run   func()
	fired bool
}

func (*beforeCmd) DialHook(next redis.DialHook) redis.DialHook { return next }

func (b *beforeCmd) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !b.fired && b.match(cmd.Args()) {
			b.fired = true
			b.run()
		}
		return next(ctx, cmd)
	}
}

func (*beforeCmd) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func isScript(args []any, s *redis.Script) bool {
	return len(args) > 1 && args[1] == s.Hash()
}

// TestRevoke_DoesNotWaitForTheMintLock: a mint holding the identifier's lock (or a flood of them) cannot delay or
// fail a revoke.
func TestRevoke_DoesNotWaitForTheMintLock(t *testing.T) {
	f := newLineageFixture(t)
	root, rootPlain, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	release, err := f.store.lockMint(context.Background(), "acct-x")
	if err != nil {
		t.Fatalf("lockMint: %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID while a mint holds the lock = %v, want nil", err)
	}
	f.assertDead(t, map[string]string{"root": rootPlain})
}

// Interleavings of a delegated mint (check minter, then write) with a revoke of its tree (mark, delete the root,
// list, delete descendants deepest first):
//   - the write lands before the revoke lists: the child is found by its chain and deleted
//     (TestRevokingMinterRevokesDelegatedKeys);
//   - the write lands after the revoke ran: the minter's record is gone and the root is marked, so the write is
//     refused (TestCreate_DelegatedWriteAfterTheRevokeIsRefused);
//   - the write lands after the listing but before its minter is deleted: the root's mark refuses it
//     (TestCreate_DelegatedWriteDuringTheSweepIsRefused).

// TestCreate_DelegatedWriteAfterTheRevokeIsRefused: a mint that passed its minter check before a revoke but
// writes after it leaves nothing live.
func TestCreate_DelegatedWriteAfterTheRevokeIsRefused(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	var revokeErr error
	f.rdb.AddHook(&beforeCmd{
		match: func(args []any) bool { return isScript(args, writeDelegatedRecordScript) },
		run:   func() { revokeErr = f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID) },
	})
	if _, _, err := f.store.Create(ctx, ChildKeyRequest(rootSub, "late", nil)); !errors.Is(err, ErrMinterNotLive) {
		t.Fatalf("Create whose write follows its minter's revoke = %v, want ErrMinterNotLive", err)
	}
	if revokeErr != nil {
		t.Fatalf("RevokeKeyByID: %v", revokeErr)
	}
	if recs, err := f.store.ListKeysForIdentifier(ctx, "acct-x"); err != nil || len(recs) != 0 {
		t.Fatalf("keys after the refused mint = %d (err %v), want none", len(recs), err)
	}
}

// TestCreate_DelegatedWriteDuringTheSweepIsRefused: a grandchild minted from a descendant the sweep has listed
// but not yet deleted is refused, so the sweep leaves no orphan behind it.
func TestCreate_DelegatedWriteDuringTheSweepIsRefused(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	mid, _, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	var mintErr error
	f.rdb.AddHook(&beforeCmd{
		match: func(args []any) bool { return len(args) == 9 && args[8] == "1" && args[6] == mid.KeyID },
		run:   func() { _, _, mintErr = f.store.Create(ctx, ChildKeyRequest(midSub, "late", nil)) },
	})
	if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	if !errors.Is(mintErr, ErrMinterNotLive) {
		t.Fatalf("mint under a key the sweep listed = %v, want ErrMinterNotLive", mintErr)
	}
	if recs, err := f.store.ListKeysForIdentifier(ctx, "acct-x"); err != nil || len(recs) != 0 {
		t.Fatalf("keys after the revoke = %d (err %v), want none", len(recs), err)
	}
}

// TestDelegatedRate_FollowsTheMintersStoredRateNotTheAccountFloor: a child minted while the account's
// rate override (a floor) lifts its minter must not keep that rate once the override is removed.
func TestDelegatedRate_FollowsTheMintersStoredRateNotTheAccountFloor(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	acct := acctWithStatus("floor", platform.AccountActive)
	acct.RateLimitPerMinOverride = 5000
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{"floor": acct}}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	validator := NewRedisAPIKeyValidator(f.store.rdb, WithAccountStatus(accounts), WithClock(func() time.Time { return now }))
	lookup := func(plain string) Subject {
		t.Helper()
		sub, err := validator.Lookup(ctx, plain)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		return sub
	}

	_, parentPlain, err := f.store.Create(ctx, CreateAPIKeyRequest{Identifier: AccountIdentifier("floor"), RateLimitPerMin: 100})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	parent := lookup(parentPlain)
	if parent.RateLimitPerMin != 5000 {
		t.Fatalf("parent rate under the override = %d, want 5000", parent.RateLimitPerMin)
	}
	child, childPlain, err := f.store.Create(ctx, ChildKeyRequest(parent, "child", nil))
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.RateLimitPerMin != 100 {
		t.Errorf("stored child rate = %d, want the minter's stored 100", child.RateLimitPerMin)
	}

	acct.RateLimitPerMinOverride = 0
	accounts.bySlug["floor"] = acct
	now = now.Add(time.Hour) // past the account-status cache
	if p, c := lookup(parentPlain).RateLimitPerMin, lookup(childPlain).RateLimitPerMin; c > p {
		t.Errorf("after the override is removed child rate %d exceeds its minter's %d", c, p)
	}
}

// TestRevoke_ReachesPastAnExpiredMiddleKey: a grandchild stays linked to the root after the key between
// them aged out of Redis.
func TestRevoke_ReachesPastAnExpiredMiddleKey(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	root, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	_, midPlain, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	_, leafPlain, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	f.mr.Del(cachekeys.APIKey(hashAPIKey(midPlain)).String())

	if err := f.store.RevokeKeyByID(ctx, "acct-x", root.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	f.assertDead(t, map[string]string{"leaf": leafPlain})
}
