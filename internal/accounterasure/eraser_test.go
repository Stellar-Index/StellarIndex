package accounterasure

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// fakeStore records the Postgres half's calls; the account exists until
// EraseAccount succeeds.
type fakeStore struct {
	plan     postgresstore.ErasurePlan
	gone     bool
	eraseErr error
	erased   bool
	requests []postgresstore.ErasureRequest
	renames  []string
}

func (f *fakeStore) PlanErasure(context.Context, uuid.UUID) (postgresstore.ErasurePlan, error) {
	if f.gone {
		return postgresstore.ErasurePlan{}, platform.ErrNotFound
	}
	return f.plan, nil
}

func (f *fakeStore) EraseAccount(_ context.Context, req postgresstore.ErasureRequest) (postgresstore.ErasureCounts, error) {
	f.requests = append(f.requests, req)
	if f.eraseErr != nil {
		return postgresstore.ErasureCounts{}, f.eraseErr
	}
	f.gone, f.erased = true, true
	return postgresstore.ErasureCounts{Users: 1}, nil
}

func (f *fakeStore) RenameUsageSubjects(_ context.Context, _ []string, erased string) (int64, error) {
	f.renames = append(f.renames, erased)
	return 0, nil
}

func (f *fakeStore) SlugErased(context.Context, string) (bool, error) { return f.erased, nil }

func newRig(t *testing.T) (*Eraser, *fakeStore, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := &fakeStore{plan: postgresstore.ErasurePlan{AccountID: uuid.New(), Slug: "john"}}
	return &Eraser{Store: st, Redis: rdb}, st, rdb, mr
}

func mint(t *testing.T, rdb *redis.Client, slug string) (auth.APIKeyRecord, string) {
	t.Helper()
	rec, plaintext, err := auth.NewRedisAPIKeyStore(rdb).Create(context.Background(),
		auth.CreateAPIKeyRequest{Identifier: auth.AccountIdentifier(slug)})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return rec, plaintext
}

func authenticates(rdb *redis.Client, plaintext string) bool {
	_, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext)
	return err == nil
}

// TestErase_SecondPassCatchesAnInFlightMint is the race the post-commit
// re-delete exists for: a mint that listed nothing before the commit and
// wrote its record after the first Redis pass.
func TestErase_SecondPassCatchesAnInFlightMint(t *testing.T) {
	e, st, rdb, _ := newRig(t)
	ctx := context.Background()
	rec, plaintext := mint(t, rdb, "john")
	var late string
	e.betweenPasses = func() { _, late = mint(t, rdb, "john") }

	rep, err := e.Erase(ctx, st.plan.AccountID, platform.ActorUser)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if authenticates(rdb, plaintext) {
		t.Error("the pre-commit key still authenticates")
	}
	if authenticates(rdb, late) {
		t.Error("a key minted between the two passes survived the erasure")
	}
	if got := st.requests[0].ExtraKeyIDs; len(got) != 1 || got[0] != rec.KeyID {
		t.Errorf("EraseAccount got Redis key ids %v, want [%s]", got, rec.KeyID)
	}
	if rep.RedisKeys != 2 {
		t.Errorf("RedisKeys = %d, want 2 (one per pass)", rep.RedisKeys)
	}
	if len(st.renames) != 1 || st.renames[0] != st.requests[0].ErasedSubject || rep.ErasedSubject != st.renames[0] {
		t.Errorf("post-commit rename used %v, commit used %q: must reuse one erased subject",
			st.renames, st.requests[0].ErasedSubject)
	}
}

// TestErase_UsageCountersAndOtherAccountsKeys — the account's usage
// counters go; another account's key and counters stay.
func TestErase_UsageCountersAndOtherAccountsKeys(t *testing.T) {
	e, st, rdb, mr := newRig(t)
	ctx := context.Background()
	_, other := mint(t, rdb, "yves")
	c := usage.New(rdb)
	_ = c.Increment(ctx, "id:acct:john")
	_ = c.IncrementDetail(ctx, "id:acct:john", "/v1/price", usage.ClassOK)
	_ = c.Increment(ctx, "id:acct:yves")

	if _, err := e.Erase(ctx, st.plan.AccountID, platform.ActorUser); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if n, _ := c.MonthToDate(ctx, "id:acct:john"); n != 0 {
		t.Errorf("erased account's month-to-date usage = %d, want 0", n)
	}
	if n, _ := c.MonthToDate(ctx, "id:acct:yves"); n != 1 {
		t.Errorf("other account's usage = %d, want 1", n)
	}
	if !authenticates(rdb, other) {
		t.Error("another account's key stopped authenticating")
	}
	for _, k := range mr.Keys() {
		if v, _ := rdb.Get(ctx, k).Result(); strings.Contains(k+v, "acct:john") || strings.Contains(k, url.QueryEscape("acct:john")) {
			t.Errorf("redis key %s still names the erased account", k)
		}
	}
}

// TestErase_IsIdempotent — a retry after success reports AlreadyErased
// and touches nothing; a failed commit leaves Redis alone.
func TestErase_IsIdempotent(t *testing.T) {
	e, st, rdb, _ := newRig(t)
	ctx := context.Background()
	_, plaintext := mint(t, rdb, "john")

	st.eraseErr = errors.New("boom")
	if _, err := e.Erase(ctx, st.plan.AccountID, platform.ActorUser); err == nil {
		t.Fatal("Erase succeeded although the commit failed")
	}
	if !authenticates(rdb, plaintext) {
		t.Error("a failed commit still deleted Redis state; a retry could not find the keys to scrub")
	}
	st.eraseErr = nil
	if _, err := e.Erase(ctx, st.plan.AccountID, platform.ActorUser); err != nil {
		t.Fatalf("retry: %v", err)
	}
	rep, err := e.Erase(ctx, st.plan.AccountID, platform.ActorUser)
	if err != nil || !rep.AlreadyErased {
		t.Fatalf("second erase = %+v, %v; want AlreadyErased", rep, err)
	}
}

// TestFinishBySlug_RefusesALiveSlug — the operator recovery path must not
// wipe the Redis state of an account that was never erased.
func TestFinishBySlug_RefusesALiveSlug(t *testing.T) {
	e, _, rdb, _ := newRig(t)
	_, plaintext := mint(t, rdb, "john")
	if _, err := e.FinishBySlug(context.Background(), "john"); err == nil {
		t.Fatal("FinishBySlug accepted a slug that was never erased")
	}
	if !authenticates(rdb, plaintext) {
		t.Error("refused FinishBySlug still deleted keys")
	}
}
