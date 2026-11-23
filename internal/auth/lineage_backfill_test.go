// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

type backfillFixture struct {
	*lineageFixture
	rootPlain, midPlain, leafPlain, hashlessPlain string
	mid, leaf, hashless, orphan                   APIKeyRecord
}

// newBackfillFixture holds a pre-chain record (mid), a complete one (leaf), one with ids but no hashes
// (hashless) and a pre-chain record whose parent's record is gone (orphan).
func newBackfillFixture(t *testing.T) *backfillFixture {
	t.Helper()
	ctx := context.Background()
	f := &backfillFixture{lineageFixture: newLineageFixture(t)}
	var rootSub, midSub Subject
	_, f.rootPlain, rootSub = f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	f.mid, f.midPlain, midSub = f.legacyChild(t, rootSub)
	f.leaf, f.leafPlain, _ = f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
	f.hashless, f.hashlessPlain, _ = f.mint(t, ChildKeyRequest(rootSub, "hashless", nil))
	stripped := f.hashless
	stripped.AncestorHashes = nil
	body, err := json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rdb.Set(ctx, cachekeys.APIKey(hashAPIKey(f.hashlessPlain)).String(), body, redis.KeepTTL).Err(); err != nil {
		t.Fatal(err)
	}

	_, gonePlain, goneSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-y"})
	f.orphan, _, _ = f.legacyChild(t, goneSub)
	f.mr.Del(cachekeys.APIKey(hashAPIKey(gonePlain)).String())
	return f
}

// snapshot is every key's value and TTL.
func (f *backfillFixture) snapshot() map[string]string {
	out := map[string]string{}
	for _, k := range f.mr.Keys() {
		v, _ := f.mr.Get(k)
		out[k] = v + "|" + f.mr.TTL(k).String()
	}
	return out
}

func TestBackfillAncestorHashes_WritesChainsAndReportsOrphans(t *testing.T) {
	f := newBackfillFixture(t)
	ctx := context.Background()
	hashlessKey := cachekeys.APIKey(hashAPIKey(f.hashlessPlain)).String()
	ttlBefore := f.mr.TTL(hashlessKey)
	leafBefore := f.storedRecord(t, f.leafPlain)

	rep, err := f.store.BackfillAncestorHashes(ctx, true)
	if err != nil {
		t.Fatalf("BackfillAncestorHashes: %v", err)
	}
	if want := sortedIDs(f.mid.KeyID, f.hashless.KeyID); !slices.Equal(rep.Resolved, want) {
		t.Errorf("Resolved = %v, want %v", rep.Resolved, want)
	}
	if want := []string{f.orphan.KeyID}; !slices.Equal(rep.Orphaned, want) {
		t.Errorf("Orphaned = %v, want %v", rep.Orphaned, want)
	}
	if rep.Complete != 1 || rep.Delegated != 4 || rep.MaxDepth != 2 || len(rep.Changed) != 0 {
		t.Errorf("report = %+v, want 4 delegated, 1 complete, max depth 2, none changed", rep)
	}

	rootHash := hashAPIKey(f.rootPlain)
	for name, plain := range map[string]string{"pre-chain": f.midPlain, "ids without hashes": f.hashlessPlain} {
		got := f.storedRecord(t, plain)
		if !slices.Equal(got.AncestorKeyIDs, []string{got.ParentKeyID}) || !slices.Equal(got.AncestorHashes, []string{rootHash}) {
			t.Errorf("%s record after backfill: %d ids, %d hashes; want the root's id and hash", name, len(got.AncestorKeyIDs), len(got.AncestorHashes))
		}
		if _, err := f.validator.Lookup(ctx, plain); err != nil {
			t.Errorf("%s key after backfill: Lookup err = %v", name, err)
		}
	}
	if ttl := f.mr.TTL(hashlessKey); ttl != ttlBefore || ttl <= 0 {
		t.Errorf("rewritten record TTL = %v, want it kept at %v", ttl, ttlBefore)
	}
	if got := f.storedRecord(t, f.leafPlain); !slices.Equal(got.AncestorHashes, leafBefore.AncestorHashes) {
		t.Error("backfill rewrote a record that already carried its chain")
	}

	again, err := f.store.BackfillAncestorHashes(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Resolved) != 0 || again.Complete != 3 {
		t.Errorf("second run = %+v, want nothing left to resolve and 3 complete", again)
	}
}

func TestBackfillAncestorHashes_DryRunWritesNothing(t *testing.T) {
	f := newBackfillFixture(t)
	before := f.snapshot()
	rep, err := f.store.BackfillAncestorHashes(context.Background(), false)
	if err != nil {
		t.Fatalf("BackfillAncestorHashes: %v", err)
	}
	if len(rep.Resolved) != 2 || len(rep.Orphaned) != 1 {
		t.Errorf("dry run report = %+v, want 2 resolved and 1 orphaned", rep)
	}
	after := f.snapshot()
	if len(after) != len(before) {
		t.Errorf("dry run changed the key count: %d, then %d", len(before), len(after))
	}
	changed := 0
	for k, v := range before {
		if after[k] != v {
			changed++
		}
	}
	if changed != 0 {
		t.Errorf("dry run changed %d records", changed)
	}
}

// A record rewritten between the walk's read and the backfill's write is left alone and reported.
func TestBackfillAncestorHashes_RecordChangedUnderTheWriteIsNotOverwritten(t *testing.T) {
	f := newBackfillFixture(t)
	ctx := context.Background()
	o := ownedRecord{hash: hashAPIKey(f.midPlain), rec: f.storedRecord(t, f.midPlain)}
	f.rdb.AddHook(&beforeCmd{
		match: func(args []any) bool { return len(args) > 0 && (args[0] == "evalsha" || args[0] == "eval") },
		run: func() {
			changed := o.rec
			changed.Label = "relabelled"
			body, _ := json.Marshal(changed)
			f.mr.Set(cachekeys.APIKey(o.hash).String(), string(body))
		},
	})
	wrote, err := f.store.recordLineage(ctx, o, []string{o.rec.ParentKeyID}, []string{hashAPIKey(f.rootPlain)})
	if err != nil || wrote {
		t.Fatalf("recordLineage over a concurrently changed record = %v, %v; want false, nil", wrote, err)
	}
	if got := f.storedRecord(t, f.midPlain); got.Label != "relabelled" || len(got.AncestorHashes) != 0 {
		t.Error("the concurrent writer's record was overwritten")
	}
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}
