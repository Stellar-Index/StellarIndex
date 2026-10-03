package auth

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestReleaseSignup_DeletesOnlyTheNamedMapping — releasing a lapsed key
// must not free an email that a concurrent signup has re-claimed.
func TestReleaseSignup_DeletesOnlyTheNamedMapping(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	tr := NewRedisSignupTracker(rdb)
	ctx := context.Background()

	if err := tr.MarkSignup(ctx, "h", "kid_new"); err != nil {
		t.Fatalf("MarkSignup: %v", err)
	}
	released, err := tr.ReleaseSignup(ctx, "h", "kid_old")
	if err != nil || released {
		t.Fatalf("ReleaseSignup(stale id) = %v, %v; want false, nil", released, err)
	}
	if got, _ := tr.LookupByEmailHash(ctx, "h"); got != "kid_new" {
		t.Fatalf("mapping after stale release = %q, want kid_new", got)
	}

	released, err = tr.ReleaseSignup(ctx, "h", "kid_new")
	if err != nil || !released {
		t.Fatalf("ReleaseSignup(current id) = %v, %v; want true, nil", released, err)
	}
	if err := tr.ReserveEmail(ctx, "h"); err != nil {
		t.Fatalf("ReserveEmail after release: %v", err)
	}
}
