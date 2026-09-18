// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestNewKeyCacheInvalidatorForBackend pins which backends get a
// key-cache invalidator at all (findings F056 / K050 / Q145).
//
// `apikey:<hash>` is a rebuildable cache entry only under
// auth_backend=postgres. Under every other value — the shipped default
// "redis", or unset, which buildAPIKeyValidator also treats as redis —
// it is the canonical credential, and handing a mutation path something
// that DELs it is how an admin PATCH destroyed customers' keys.
func TestNewKeyCacheInvalidatorForBackend(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	for _, backend := range []string{"redis", "", "Postgres", "postgres ", "pg"} {
		if inv := NewKeyCacheInvalidatorForBackend(backend, rdb); inv != nil {
			t.Errorf("backend %q: got an invalidator, want nil — only the exact value %q has a key cache",
				backend, BackendPostgres)
		}
	}
	if inv := NewKeyCacheInvalidatorForBackend(BackendPostgres, nil); inv != nil {
		t.Error("postgres backend without a cache client: got an invalidator, want nil")
	}

	inv := NewKeyCacheInvalidatorForBackend(BackendPostgres, rdb)
	if inv == nil {
		t.Fatal("postgres backend: got nil, want an invalidator")
	}
	const hexHash = "00ff"
	entry := cachekeys.APIKey(hexHash).String()
	if err := mr.Set(entry, `{"identifier":"account:cached"}`); err != nil {
		t.Fatalf("seed cache entry: %v", err)
	}
	if err := inv.InvalidateCachedKey(context.Background(), hexHash); err != nil {
		t.Fatalf("InvalidateCachedKey: %v", err)
	}
	if mr.Exists(entry) {
		t.Errorf("postgres backend: cache entry %s survived eviction", entry)
	}
}
