// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
)

// cancelTestAccountStore is a minimal [RegisterAccountCreator] double,
// local to this file (package v1, so it can see r.Context() plumbing
// the external v1_test fakes can't exercise deterministically).
type cancelTestAccountStore struct {
	mu            sync.Mutex
	created       []platform.Account
	suspendCalls  int
	suspendID     uuid.UUID
	suspendReason string
}

func (f *cancelTestAccountStore) Create(_ context.Context, a platform.Account) (platform.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.ID = uuid.New()
	f.created = append(f.created, a)
	return a, nil
}

func (f *cancelTestAccountStore) Suspend(_ context.Context, id uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspendCalls++
	f.suspendID = id
	f.suspendReason = reason
	return nil
}

// cancelTestKeyStore is a minimal [platform.APIKeyStore] double whose
// Create returns the context error when the context is already done —
// standing in for a Postgres INSERT that observes a canceled ctx, same
// as a real *sql.DB call would.
type cancelTestKeyStore struct{}

func (cancelTestKeyStore) Create(ctx context.Context, k platform.APIKey, _ int) (platform.APIKey, error) {
	if err := ctx.Err(); err != nil {
		return platform.APIKey{}, err
	}
	return k, nil
}

func (cancelTestKeyStore) Get(_ context.Context, _ string) (platform.APIKey, error) {
	return platform.APIKey{}, platform.ErrNotFound
}

func (cancelTestKeyStore) GetByHash(_ context.Context, _ []byte) (platform.APIKey, error) {
	return platform.APIKey{}, platform.ErrNotFound
}

func (cancelTestKeyStore) ListForAccount(_ context.Context, _ uuid.UUID) ([]platform.APIKey, error) {
	return nil, nil
}
func (cancelTestKeyStore) Update(_ context.Context, _ platform.APIKey) error { return nil }
func (cancelTestKeyStore) Revoke(_ context.Context, _ string, _ uuid.UUID, _ string) error {
	return nil
}

func (cancelTestKeyStore) TouchUsage(_ context.Context, _ string, _ net.IP, _ string) error {
	return nil
}

// cancelTestMirror is a minimal [KeyMirror] double that records
// whether the context it was handed for a rollback was ALREADY dead —
// the GH-977 tell: a rollback on the same canceled request context
// fails immediately, the same way a real Redis SCAN does.
type cancelTestMirror struct {
	mu               sync.Mutex
	createCalls      int
	revokeCalls      int
	revokeSawDeadCtx bool
}

func (m *cancelTestMirror) CreateWithSecret(_ context.Context, _ auth.MirroredKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	return nil
}

func (m *cancelTestMirror) RevokeKeyByID(ctx context.Context, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokeCalls++
	if ctx.Err() != nil {
		m.revokeSawDeadCtx = true
		return ctx.Err()
	}
	return nil
}

// TestHandleRegister_ClientCancelDuringKeyCreate is the GH-977
// regression: a client disconnecting between the Redis mirror write
// and the Postgres management-row INSERT must still (a) quarantine
// the now-orphaned account for the signup-reaper, and (b) roll the
// mirror back on a context that is NOT already dead — otherwise the
// rollback fails the same way the Postgres write just did, and a
// live, unlistable credential outlives the request that minted it.
//
// RED on the pre-fix code: the `errors.Is(err, context.Canceled)`
// branch in handleRegister returns before suspendRegisterOrphan runs
// (suspendCalls stays 0), and mintRegisterKey's rollback reuses the
// same canceled ctx (revokeSawDeadCtx is true).
func TestHandleRegister_ClientCancelDuringKeyCreate(t *testing.T) {
	accounts := &cancelTestAccountStore{}
	mirror := &cancelTestMirror{}
	s := New(Options{
		RegisterAccounts: accounts,
		APIKeyBudgets: APIKeyBudgetStores{
			Platform:    cancelTestKeyStore{},
			RedisMirror: mirror,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone by the time Platform.Create runs

	r := httptest.NewRequest(http.MethodPost, "/v1/register", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	s.handleRegister(w, r)

	if len(accounts.created) != 1 {
		t.Fatalf("accounts created = %d, want 1", len(accounts.created))
	}
	if mirror.createCalls != 1 {
		t.Fatalf("mirror.CreateWithSecret called %d times, want 1", mirror.createCalls)
	}

	// (a) The orphan account must be quarantined even though the
	// mint error is context.Canceled.
	if accounts.suspendCalls != 1 {
		t.Errorf("accounts.Suspend called %d times, want 1 — a canceled mint must still quarantine the orphan account (GH-977)", accounts.suspendCalls)
	}
	if accounts.suspendCalls == 1 {
		if accounts.suspendID != accounts.created[0].ID {
			t.Errorf("suspended account id = %s, want %s", accounts.suspendID, accounts.created[0].ID)
		}
		if !strings.HasPrefix(accounts.suspendReason, signupreaper.SignupRaceReasonPrefix) {
			t.Errorf("suspend reason = %q, want prefix %q", accounts.suspendReason, signupreaper.SignupRaceReasonPrefix)
		}
	}

	// (b) The mirror rollback must run on a context that is not
	// already canceled.
	if mirror.revokeCalls != 1 {
		t.Fatalf("mirror.RevokeKeyByID called %d times, want 1", mirror.revokeCalls)
	}
	if mirror.revokeSawDeadCtx {
		t.Error("mirror rollback ran on the SAME canceled request context — it will fail exactly as the Postgres write just did (GH-977)")
	}
}
