package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestCreateCapped_ConcurrentBurstMintsOnlyToCeiling pins
// CA2-A01-harden-6: an identifier one key below its ceiling fires a
// concurrent burst of self-service mints. A count taken apart from the
// write lets every request read 24 < 25 and mint; the capped create must
// issue exactly one key and refuse the rest.
func TestCreateCapped_ConcurrentBurstMintsOnlyToCeiling(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), PoolSize: 64})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewRedisAPIKeyStore(rdb)
	ctx := context.Background()

	const (
		owner   = "acct:burst"
		ceiling = 25
		burst   = 40
	)
	for range ceiling - 1 {
		if _, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: owner}); err != nil {
			t.Fatalf("seed Create: %v", err)
		}
	}

	var (
		wg              sync.WaitGroup
		minted, refused atomic.Int32
		start           = make(chan struct{})
	)
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := s.CreateCapped(ctx, CreateAPIKeyRequest{Identifier: owner}, ceiling)
			var over *KeyQuotaExceededError
			switch {
			case err == nil:
				minted.Add(1)
			case errors.As(err, &over):
				refused.Add(1)
			default:
				t.Errorf("CreateCapped: unexpected error %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := minted.Load(); got != 1 {
		t.Errorf("burst of %d concurrent capped mints at %d/%d minted %d keys, want exactly 1", burst, ceiling-1, ceiling, got)
	}
	if got := refused.Load(); got != burst-1 {
		t.Errorf("refused = %d, want %d", got, burst-1)
	}
	recs, err := s.ListKeysForIdentifier(ctx, owner)
	if err != nil {
		t.Fatalf("ListKeysForIdentifier: %v", err)
	}
	if len(recs) != ceiling {
		t.Errorf("identifier holds %d keys after the burst, want the ceiling %d", len(recs), ceiling)
	}
	if mr.Exists(cachekeys.APIKeyMintLock(owner).String()) {
		t.Error("mint lock still held after every mint returned")
	}
}

// TestCreateCapped_RevokedKeysDoNotCount: the ceiling is on un-revoked
// keys, so revoking one frees a slot.
func TestCreateCapped_RevokedKeysDoNotCount(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	const owner = "acct:rotate"

	first, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCapped(ctx, CreateAPIKeyRequest{Identifier: owner}, 1); err == nil {
		t.Fatal("CreateCapped at the ceiling minted")
	}
	if err := s.RevokeKeyByID(ctx, owner, first.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCapped(ctx, CreateAPIKeyRequest{Identifier: owner}, 1); err != nil {
		t.Fatalf("CreateCapped after revoking the only key: %v", err)
	}
}
