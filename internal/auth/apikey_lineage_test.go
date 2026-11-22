// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

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
		t.Errorf("returned AncestorHashes = %v, want the root's then the middle key's record hash", leaf.AncestorHashes)
	}
	if got := f.storedRecord(t, leafPlain).AncestorHashes; !slices.Equal(got, want) {
		t.Errorf("stored AncestorHashes = %v, want %v", got, want)
	}
	if got := f.storedRecord(t, rootPlain).AncestorHashes; got != nil {
		t.Errorf("root AncestorHashes = %v, want none", got)
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
		t.Errorf("AncestorHashes = %v, want %v", leaf.AncestorHashes, want)
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
