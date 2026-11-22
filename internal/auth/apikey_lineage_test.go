// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// storedRecord reads the record a plaintext authenticates through, as Redis holds it.
func (f *lineageFixture) storedRecord(t *testing.T, plain string) APIKeyRecord {
	t.Helper()
	raw, err := f.mr.Get(cachekeys.APIKey(hashAPIKey(plain)).String())
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec APIKeyRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCreate_ChildRecordsAncestorHashes(t *testing.T) {
	f := newLineageFixture(t)
	_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	_, midPlain, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	leaf, leafPlain, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))

	want := []string{hashAPIKey(rootPlain), hashAPIKey(midPlain)}
	if !slices.Equal(leaf.AncestorHashes, want) {
		t.Errorf("returned %d AncestorHashes, want the root's then the middle key's record hash", len(leaf.AncestorHashes))
	}
	if got := f.storedRecord(t, leafPlain).AncestorHashes; !slices.Equal(got, want) {
		t.Errorf("stored %d AncestorHashes, want the root's then the middle key's record hash", len(got))
	}
	if got := f.storedRecord(t, rootPlain).AncestorHashes; got != nil {
		t.Errorf("root has %d AncestorHashes, want none", len(got))
	}
}

// A minter whose record predates stored hashes still gives its child the full chain of hashes.
func TestCreate_LegacyMinterGivesItsChildTheFullChainOfHashes(t *testing.T) {
	f := newLineageFixture(t)
	_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	_, midPlain, midSub := f.legacyChild(t, rootSub)
	_, leafPlain, leafSub := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	_, _, _ = f.mint(t, ChildKeyRequest(leafSub, "below", nil))

	leaf := f.storedRecord(t, leafPlain)
	if want := []string{hashAPIKey(rootPlain), hashAPIKey(midPlain)}; !slices.Equal(leaf.AncestorHashes, want) {
		t.Errorf("%d AncestorHashes, want the root's then mid's record hash", len(leaf.AncestorHashes))
	}
}

// A parent that has ids but no hashes resolves each id at mint; a gone ancestor refuses the mint.
func TestCreate_HashlessParentWithAGoneAncestorIsRefused(t *testing.T) {
	f := newLineageFixture(t)
	_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	mid, midPlain, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	mid.AncestorHashes = nil
	body, err := json.Marshal(mid)
	if err != nil {
		t.Fatal(err)
	}
	f.mr.Set(cachekeys.APIKey(hashAPIKey(midPlain)).String(), string(body))
	f.mr.Del(cachekeys.APIKey(hashAPIKey(rootPlain)).String())

	if _, _, err := f.store.Create(context.Background(), ChildKeyRequest(midSub, "leaf", nil)); !errors.Is(err, ErrMinterNotLive) {
		t.Fatalf("Create under a hashless parent whose root is gone: err = %v, want ErrMinterNotLive", err)
	}
}

// The write itself refuses when any ancestor record is gone, on the indexed path and the ACL-denied fallback.
func TestCreate_DelegatedWriteRefusedWhenAnAncestorRecordIsGone(t *testing.T) {
	for name, denyIndex := range map[string]bool{"indexed": false, "index denied by ACL": true} {
		t.Run(name, func(t *testing.T) {
			f := newLineageFixture(t)
			if denyIndex {
				denier := &indexDenier{}
				denier.deny.Store(true)
				f.rdb.AddHook(denier)
			}
			_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
			_, _, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
			f.mr.Del(cachekeys.APIKey(hashAPIKey(rootPlain)).String())
			before := len(f.mr.Keys())

			if _, _, err := f.store.Create(context.Background(), ChildKeyRequest(midSub, "leaf", nil)); !errors.Is(err, ErrMinterNotLive) {
				t.Fatalf("Create under a key whose root record is gone: err = %v, want ErrMinterNotLive", err)
			}
			if after := len(f.mr.Keys()); after != before {
				t.Errorf("refused mint changed the keyspace: %d keys, then %d", before, after)
			}
		})
	}
}

func TestCreate_RefusesMintBeyondMaxDepth(t *testing.T) {
	f := newLineageFixture(t)
	_, _, sub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	for range MaxDelegationDepth {
		_, _, sub = f.mint(t, ChildKeyRequest(sub, "", nil))
	}
	if got := len(sub.AncestorKeyIDs); got != MaxDelegationDepth {
		t.Fatalf("deepest key has %d ancestors, want %d", got, MaxDelegationDepth)
	}
	if _, _, err := f.store.Create(context.Background(), ChildKeyRequest(sub, "", nil)); !errors.Is(err, ErrMintExceedsCaller) {
		t.Fatalf("Create beyond depth %d: err = %v, want ErrMintExceedsCaller", MaxDelegationDepth, err)
	}
}

// Using a child slides its ancestors' idle windows too, on both validators, so an unused middle key cannot age
// out from under a child in daily use.
func TestLookup_ChildUseKeepsAncestorsAlive(t *testing.T) {
	const day = 24 * time.Hour
	for name, use := range map[string]func(f *lineageFixture, plain string) error{
		"redis validator": func(f *lineageFixture, plain string) error {
			_, err := f.validator.Lookup(context.Background(), plain)
			return err
		},
		"postgres validator canonical hit": func(f *lineageFixture, plain string) error {
			v := &PostgresAPIKeyValidator{cache: f.rdb, now: time.Now, cacheTTL: time.Hour}
			_, hit, err := v.cacheLookup(context.Background(), hashAPIKey(plain))
			if err == nil && !hit {
				err = errors.New("no cache hit")
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLineageFixture(t)
			_, rootPlain, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
			_, midPlain, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
			_, leafPlain, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))

			f.mr.FastForward(89 * day)
			if err := use(f, leafPlain); err != nil {
				t.Fatalf("use the leaf: %v", err)
			}
			f.mr.FastForward(89 * day)
			for name, plain := range map[string]string{"root": rootPlain, "mid": midPlain} {
				if ttl := f.mr.TTL(cachekeys.APIKey(hashAPIKey(plain)).String()); ttl <= 0 {
					t.Errorf("%s record TTL = %v after its descendant was used, want it still alive", name, ttl)
				}
			}
			if err := use(f, leafPlain); err != nil {
				t.Errorf("leaf after 178 days of daily use: %v", err)
			}
		})
	}
}

// A non-delegated key's Lookup costs what it did before lineage was recorded: one GET and one EXPIRE.
func TestLookup_NonDelegatedKeyIssuesOneGet(t *testing.T) {
	f := newLineageFixture(t)
	_, plain, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	counter := &commandCounter{counts: map[string]int{}}
	f.rdb.AddHook(counter)
	if _, err := f.validator.Lookup(context.Background(), plain); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got, want := counter.snapshot(), map[string]int{"get": 1, "expire": 1}; !maps.Equal(got, want) {
		t.Errorf("Lookup issued %v, want %v", got, want)
	}
}

// PR-1 records nothing it enforces: a delegated record written before hashes were stored still authenticates.
func TestLookup_DelegatedRecordWithoutHashesStillAuthenticates(t *testing.T) {
	f := newLineageFixture(t)
	_, _, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	_, midPlain, midSub := f.legacyChild(t, rootSub)
	leaf, leafPlain, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	leaf.AncestorHashes = nil
	body, err := json.Marshal(leaf)
	if err != nil {
		t.Fatal(err)
	}
	f.mr.Set(cachekeys.APIKey(hashAPIKey(leafPlain)).String(), string(body))

	for name, plain := range map[string]string{"pre-chain record": midPlain, "chain without hashes": leafPlain} {
		if _, err := f.validator.Lookup(context.Background(), plain); err != nil {
			t.Errorf("%s: Lookup err = %v, want it to authenticate", name, err)
		}
	}
}
