package accounterasure

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

type fakeLister struct {
	ids    []uuid.UUID
	before time.Time
}

func (f *fakeLister) ListAbandonedRegistrations(_ context.Context, before time.Time, _ int) ([]uuid.UUID, error) {
	f.before = before
	return f.ids, nil
}

// An unused registration is erased only once its credential is dead:
// a key record still in Redis, or a member user, keeps the account.
func TestSweepAbandonedRegistrations(t *testing.T) {
	ctx := context.Background()
	hash := hex.EncodeToString([]byte("register-key-hash-000000000000000"))
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	t.Run("ErasesWhenKeyRecordExpired", func(t *testing.T) {
		e, st, _, _ := newRig(t)
		st.plan.KeyHashes = []string{hash}
		l := &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}
		n, err := e.SweepAbandonedRegistrations(ctx, l, cutoff)
		if err != nil || n != 1 || !st.erased {
			t.Fatalf("erased = %d, err = %v, store erased = %v; want 1, nil, true", n, err, st.erased)
		}
		if !l.before.Equal(cutoff) {
			t.Errorf("lister cutoff = %v, want %v", l.before, cutoff)
		}
	})
	t.Run("KeepsLiveKeyRecord", func(t *testing.T) {
		e, st, rdb, _ := newRig(t)
		st.plan.KeyHashes = []string{hash}
		if err := rdb.Set(ctx, cachekeys.APIKey(hash).String(), "{}", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
		n, err := e.SweepAbandonedRegistrations(ctx, &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, cutoff)
		if err != nil || n != 0 || len(st.requests) != 0 {
			t.Fatalf("erased = %d, err = %v, erase calls = %d; want 0, nil, 0", n, err, len(st.requests))
		}
	})
	t.Run("KeepsLiveRedisOnlyKey", func(t *testing.T) {
		e, st, rdb, _ := newRig(t)
		mint(t, rdb, st.plan.Slug)
		n, err := e.SweepAbandonedRegistrations(ctx, &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, cutoff)
		if err != nil || n != 0 || len(st.requests) != 0 {
			t.Fatalf("erased = %d, err = %v, erase calls = %d; want 0, nil, 0", n, err, len(st.requests))
		}
	})
	t.Run("KeepsAccountThatGainedAMember", func(t *testing.T) {
		e, st, _, _ := newRig(t)
		st.plan.UserIDs = []uuid.UUID{uuid.New()}
		n, err := e.SweepAbandonedRegistrations(ctx, &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, cutoff)
		if err != nil || n != 0 || len(st.requests) != 0 {
			t.Fatalf("erased = %d, err = %v, erase calls = %d; want 0, nil, 0", n, err, len(st.requests))
		}
	})
	// The member check and the erase must read one plan: a member who
	// joins after a first plan must not ride into a second one that is
	// executed without being checked.
	t.Run("ChecksThePlanItExecutes", func(t *testing.T) {
		e, st, _, _ := newRig(t)
		st.afterPlan = func(f *fakeStore) { f.plan.UserIDs = []uuid.UUID{uuid.New()} }
		if _, err := e.SweepAbandonedRegistrations(ctx, &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, cutoff); err != nil {
			t.Fatal(err)
		}
		if st.plans != 1 {
			t.Errorf("plans read = %d, want 1", st.plans)
		}
		for _, req := range st.requests {
			if len(req.Plan.UserIDs) > 0 {
				t.Fatalf("executed a plan with %d members; the sweep only checked one without", len(req.Plan.UserIDs))
			}
		}
	})
	t.Run("RefusesWithoutRedis", func(t *testing.T) {
		e, st, _, _ := newRig(t)
		e.Redis = nil
		if _, err := e.SweepAbandonedRegistrations(ctx, &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, cutoff); err == nil {
			t.Fatal("sweep without Redis returned no error; it cannot prove a key dead")
		}
		if len(st.requests) != 0 {
			t.Fatal("sweep without Redis erased an account")
		}
	})
}
