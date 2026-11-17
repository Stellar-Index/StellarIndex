// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
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

// legacyChild mints a key that records only ParentKeyID, as records written before the ancestor chain do.
func (f *lineageFixture) legacyChild(t *testing.T, parent Subject) (APIKeyRecord, string, Subject) {
	t.Helper()
	return f.mint(t, CreateAPIKeyRequest{Identifier: parent.Identifier, ParentKeyID: parent.KeyID})
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

// TestRevoke_SweepsWhenTheRootHasNoRecord: a key that authenticates without a record in this store
// (a Postgres-backed key) still takes its delegated keys with it.
func TestRevoke_SweepsWhenTheRootHasNoRecord(t *testing.T) {
	f := newLineageFixture(t)
	ctx := context.Background()
	pgKey := Subject{KeyID: "pg-row-1", Identifier: "acct-x", Tier: TierAPIKey, AllowAllPermissions: true}
	_, childPlain, childSub := f.mint(t, ChildKeyRequest(pgKey, "child", nil))
	_, grandPlain, _ := f.mint(t, ChildKeyRequest(childSub, "grand", nil))

	if err := f.store.RevokeKeyByID(ctx, "acct-x", pgKey.KeyID); err != nil {
		t.Fatalf("RevokeKeyByID of a record-less root with descendants: %v, want nil", err)
	}
	f.assertDead(t, map[string]string{"child": childPlain, "grandchild": grandPlain})
	if err := f.store.RevokeKeyByID(ctx, "acct-x", pgKey.KeyID); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("second revoke = %v, want ErrKeyNotFound (no root and nothing left)", err)
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
