package accounterasure

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

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
	plan      postgresstore.ErasurePlan
	gone      bool
	eraseErr  error
	erased    bool
	requests  []postgresstore.ErasureRequest
	renames   []string
	renameErr error
	// renameFailures fails that many RenameUsageSubjects calls before
	// renameErr applies.
	renameFailures int
	// afterPlan, when set, runs after each PlanErasure has taken its
	// snapshot: the account changing between two reads.
	afterPlan func(*fakeStore)
	plans     int
}

func (f *fakeStore) PlanErasure(context.Context, uuid.UUID) (postgresstore.ErasurePlan, error) {
	if f.gone {
		return postgresstore.ErasurePlan{}, platform.ErrNotFound
	}
	p := f.plan
	f.plans++
	if f.afterPlan != nil {
		f.afterPlan(f)
	}
	return p, nil
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
	if f.renameFailures > 0 {
		f.renameFailures--
		return 0, errors.New("transient")
	}
	return 0, f.renameErr
}

func (f *fakeStore) SlugErased(context.Context, string) (bool, error) { return f.erased, nil }

func newRig(t *testing.T) (*Eraser, *fakeStore, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := &fakeStore{plan: postgresstore.ErasurePlan{AccountID: uuid.New(), Slug: "john"}}
	return &Eraser{Store: st, Redis: rdb, cleanupTimeout: 300 * time.Millisecond}, st, rdb, mr
}

func mint(ctx context.Context, t *testing.T, rdb *redis.Client, slug string) (auth.APIKeyRecord, string) {
	t.Helper()
	rec, plaintext, err := auth.NewRedisAPIKeyStore(rdb).Create(ctx,
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
	rec, plaintext := mint(t.Context(), t, rdb, "john")
	var late string
	e.betweenPasses = func() { _, late = mint(t.Context(), t, rdb, "john") }

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

// lineageDelFailer fails, with OOM, every lineage delete (the scripts and DEL naming an apikey-lineage: key).
type lineageDelFailer struct{}

func (lineageDelFailer) fails(cmd redis.Cmder) bool {
	switch strings.ToLower(cmd.Name()) {
	case "del", "evalsha", "eval":
	default:
		return false
	}
	for _, a := range cmd.Args() {
		if s, ok := a.(string); ok && strings.HasPrefix(s, "apikey-lineage:") {
			cmd.SetErr(errors.New("OOM command not allowed"))
			return true
		}
	}
	return false
}

func (lineageDelFailer) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h lineageDelFailer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.fails(cmd) {
			return cmd.Err()
		}
		return next(ctx, cmd)
	}
}

func (h lineageDelFailer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if h.fails(cmd) {
				return cmd.Err()
			}
		}
		return next(ctx, cmds)
	}
}

// A failed lineage delete never fails the erasure or keeps a key authenticating, and the report says so.
func TestErase_ReportsAFailedLineageDelete(t *testing.T) {
	for name, fail := range map[string]bool{"lineage delete fails": true, "lineage delete succeeds": false} {
		t.Run(name, func(t *testing.T) {
			e, st, rdb, _ := newRig(t)
			_, plaintext := mint(t.Context(), t, rdb, "john")
			if fail {
				rdb.AddHook(lineageDelFailer{})
			}
			rep, err := e.Erase(t.Context(), st.plan.AccountID, platform.ActorUser)
			if err != nil {
				t.Fatalf("Erase: %v", err)
			}
			if authenticates(rdb, plaintext) {
				t.Error("the key still authenticates")
			}
			if rep.LineageDeleteFailed != fail {
				t.Errorf("LineageDeleteFailed = %v, want %v", rep.LineageDeleteFailed, fail)
			}
		})
	}
}

// TestErase_UsageCountersAndOtherAccountsKeys — the account's usage
// counters go; another account's key and counters stay.
func TestErase_UsageCountersAndOtherAccountsKeys(t *testing.T) {
	e, st, rdb, mr := newRig(t)
	ctx := context.Background()
	_, other := mint(t.Context(), t, rdb, "yves")
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
	_, plaintext := mint(t.Context(), t, rdb, "john")

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
	_, plaintext := mint(t.Context(), t, rdb, "john")
	if _, err := e.FinishBySlug(context.Background(), "john"); err == nil {
		t.Fatal("FinishBySlug accepted a slug that was never erased")
	}
	if !authenticates(rdb, plaintext) {
		t.Error("refused FinishBySlug still deleted keys")
	}
}

// TestErase_PostCommitFailureIsMarkedCommitted — a cleanup failure after
// the commit must be distinguishable from a pre-commit failure, so no
// caller tells the user to retry an erasure that already happened.
func TestErase_PostCommitFailureIsMarkedCommitted(t *testing.T) {
	e, st, _, _ := newRig(t)
	st.plan.OwnerEmails = []string{"owner@b.example"}
	st.renameErr = errors.New("pg gone")
	rep, err := e.Erase(context.Background(), st.plan.AccountID, platform.ActorUser)
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("err = %v, want ErrCleanupIncomplete", err)
	}
	if !strings.Contains(err.Error(), "-finish-slug john") {
		t.Errorf("err = %q, want the finish command", err)
	}
	if !st.erased || len(rep.Plan.OwnerEmails) != 1 {
		t.Errorf("report = %+v, want the committed plan", rep)
	}

	e, st, _, _ = newRig(t)
	st.eraseErr = errors.New("tx failed")
	if _, err := e.Erase(context.Background(), st.plan.AccountID, platform.ActorUser); err == nil ||
		errors.Is(err, ErrCleanupIncomplete) {
		t.Errorf("pre-commit failure = %v, want a plain error", err)
	}
}

// A transient post-commit failure is retried in place: once the account is gone a repeat Erase reports
// AlreadyErased, so an unretried blip would leave live key records behind an "erased" answer.
func TestErase_RetriesATransientCleanupFailure(t *testing.T) {
	e, st, rdb, _ := newRig(t)
	_, plaintext := mint(context.Background(), t, rdb, "john")
	st.renameFailures = 1
	if _, err := e.Erase(context.Background(), st.plan.AccountID, platform.ActorUser); err != nil {
		t.Fatalf("Erase = %v, want the retry to finish the cleanup", err)
	}
	if len(st.renames) != 2 || st.renames[0] != st.renames[1] {
		t.Errorf("renames = %v, want two attempts with one erased subject", st.renames)
	}
	if authenticates(rdb, plaintext) {
		t.Error("erased account's key still authenticates")
	}
}

// The HTTP path answers a committed erasure inside the request timeout even when cleanup never succeeds.
func TestEraseForRequest_BoundsAFailingCleanup(t *testing.T) {
	e, st, _, _ := newRig(t)
	e.cleanupTimeout = 0
	st.renameErr = errors.New("pg gone")
	start := time.Now()
	rep, err := e.EraseForRequest(context.Background(), st.plan.AccountID, platform.ActorUser)
	if elapsed := time.Since(start); elapsed > requestCleanupTimeout+2*time.Second {
		t.Errorf("EraseForRequest took %v, want about %v", elapsed, requestCleanupTimeout)
	}
	if !errors.Is(err, ErrCleanupIncomplete) || !st.erased || rep.Plan.Slug != "john" {
		t.Fatalf("err = %v, erased = %v, report = %+v; want committed with incomplete cleanup", err, st.erased, rep)
	}
}
