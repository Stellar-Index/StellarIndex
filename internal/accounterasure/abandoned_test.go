package accounterasure

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
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
		mint(ctx, t, rdb, st.plan.Slug)
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

// An erasure whose cleanup never finished must fail the sweep, so the reaper's failure alert fires,
// even when the last attempt died on the cleanup deadline.
func TestSweepAbandonedRegistrations_IncompleteCleanupFailsTheSweep(t *testing.T) {
	e, st, _, _ := newRig(t)
	st.renameErr = fmt.Errorf("rename: %w", context.DeadlineExceeded)
	n, err := e.SweepAbandonedRegistrations(context.Background(), &fakeLister{ids: []uuid.UUID{st.plan.AccountID}}, time.Now())
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("err = %v, want ErrCleanupIncomplete", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v matches context.DeadlineExceeded; the reaper would drop it as a shutdown", err)
	}
	if n != 1 || !st.erased {
		t.Errorf("erased = %d, store erased = %v; want 1, true", n, st.erased)
	}
}

// sweepStore serves one plan per account; RenameUsageSubjects fails for the slug last planned when renameErr names it.
type sweepStore struct {
	plans     map[uuid.UUID]postgresstore.ErasurePlan
	planErr   map[uuid.UUID]error
	renameErr map[string]error
	current   string
	erased    []uuid.UUID
}

func (s *sweepStore) PlanErasure(_ context.Context, id uuid.UUID) (postgresstore.ErasurePlan, error) {
	if err := s.planErr[id]; err != nil {
		return postgresstore.ErasurePlan{}, err
	}
	s.current = s.plans[id].Slug
	return s.plans[id], nil
}

func (s *sweepStore) EraseAccount(_ context.Context, req postgresstore.ErasureRequest) (postgresstore.ErasureCounts, error) {
	s.erased = append(s.erased, req.Plan.AccountID)
	return postgresstore.ErasureCounts{}, nil
}

func (s *sweepStore) RenameUsageSubjects(context.Context, []string, string) (int64, error) {
	return 0, s.renameErr[s.current]
}

func (s *sweepStore) SlugErased(context.Context, string) (bool, error) { return false, nil }

// A hard error on a later account must not drop an earlier account's incomplete cleanup, and each
// incomplete cleanup is logged with what an operator needs to finish it.
func TestSweepAbandonedRegistrations_HardErrorKeepsIncomplete(t *testing.T) {
	e, _, _, _ := newRig(t)
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	st := &sweepStore{
		plans: map[uuid.UUID]postgresstore.ErasurePlan{
			first:  {AccountID: first, Slug: "first-slug"},
			second: {AccountID: second, Slug: "second-slug"},
		},
		planErr:   map[uuid.UUID]error{third: errors.New("plan read failed")},
		renameErr: map[string]error{"first-slug": errors.New("pg gone")},
	}
	var logs bytes.Buffer
	e.Store, e.Logger = st, slog.New(slog.NewTextHandler(&logs, nil))

	n, err := e.SweepAbandonedRegistrations(context.Background(),
		&fakeLister{ids: []uuid.UUID{first, second, third}}, time.Now())
	if !errors.Is(err, ErrCleanupIncomplete) || !strings.Contains(err.Error(), first.String()) ||
		!strings.Contains(err.Error(), "plan read failed") {
		t.Fatalf("err = %v, want the first account's incomplete cleanup and the hard error", err)
	}
	if n != 2 || len(st.erased) != 2 || st.erased[1] != second {
		t.Errorf("erased = %d, store erased = %v; want both the first and second account", n, st.erased)
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "level=ERROR") {
			line = l
		}
	}
	if !strings.Contains(line, "account_id="+first.String()) || !strings.Contains(line, "finish_slug=first-slug") {
		t.Errorf("error log = %q, want account_id and finish_slug of the first account", line)
	}
}
