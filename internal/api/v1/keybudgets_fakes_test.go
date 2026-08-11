// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net"
	"sync"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// Shared fakes for the tier-clamp (key-budget) seam. These lived in
// stripe_webhook_test.go until the Stripe integration was removed
// (2026-08-10, the platform went free); the admin clamp tests are the
// remaining consumers.

// fakeSelfServiceKeyManager is the test double for
// [v1.SelfServiceKeyManager]. Records every UpdateRateLimit call so
// assertions can confirm the handler called the right key with the
// right budget.
type fakeSelfServiceKeyManager struct {
	mu      sync.Mutex
	keys    map[string][]auth.APIKeyRecord // identifier → keys
	updates []keyBudgetUpdateCall
}

type keyBudgetUpdateCall struct {
	keyID     string
	rateLimit int
}

func (f *fakeSelfServiceKeyManager) ListKeysForIdentifier(_ context.Context, identifier string) ([]auth.APIKeyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[identifier], nil
}

func (f *fakeSelfServiceKeyManager) UpdateRateLimit(_ context.Context, keyID string, rateLimit int) (auth.APIKeyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, keyBudgetUpdateCall{keyID: keyID, rateLimit: rateLimit})
	return auth.APIKeyRecord{KeyID: keyID, RateLimitPerMin: rateLimit}, nil
}

// recordingAuditSink captures every Append call so tests can assert
// the durable audit row was written (or simulate an audit-DB blip
// via err).
type recordingAuditSink struct {
	entries []platform.AuditEntry
	err     error
}

func (r *recordingAuditSink) Append(_ context.Context, e platform.AuditEntry) error {
	r.entries = append(r.entries, e)
	return r.err
}

// fakePlatformAPIKeysForBridge is the [platform.APIKeyStore] test
// double for the clamp seam. ListForAccount returns the seeded
// slice; Update records every call so assertions can confirm which
// keys were rewritten.
type fakePlatformAPIKeysForBridge struct {
	mu      sync.Mutex
	byAcct  map[uuid.UUID][]platform.APIKey
	updates []platform.APIKey
}

func (f *fakePlatformAPIKeysForBridge) ListForAccount(_ context.Context, accountID uuid.UUID) ([]platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]platform.APIKey, len(f.byAcct[accountID]))
	copy(out, f.byAcct[accountID])
	return out, nil
}

func (f *fakePlatformAPIKeysForBridge) Update(_ context.Context, k platform.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, k)
	// Reflect the update back into the source-of-truth slice
	// so a subsequent ListForAccount returns the new value.
	for i := range f.byAcct[k.AccountID] {
		if f.byAcct[k.AccountID][i].ID == k.ID {
			f.byAcct[k.AccountID][i] = k
			return nil
		}
	}
	return nil
}

func (*fakePlatformAPIKeysForBridge) Create(_ context.Context, _ platform.APIKey, _ int) (platform.APIKey, error) {
	panic("unused")
}

func (*fakePlatformAPIKeysForBridge) Get(_ context.Context, _ string) (platform.APIKey, error) {
	panic("unused")
}

func (*fakePlatformAPIKeysForBridge) GetByHash(_ context.Context, _ []byte) (platform.APIKey, error) {
	panic("unused")
}

func (*fakePlatformAPIKeysForBridge) Revoke(_ context.Context, _ string, _ uuid.UUID, _ string) error {
	panic("unused")
}

func (*fakePlatformAPIKeysForBridge) TouchUsage(_ context.Context, _ string, _ net.IP, _ string) error {
	panic("unused")
}

// fakeKeyCacheInvalidator records the hex hashes it was asked to
// evict (and can be made to fail) so the X6 read-through
// split-brain tests can assert which keys got invalidated.
type fakeKeyCacheInvalidator struct {
	mu          sync.Mutex
	invalidated []string
	err         error
}

func (f *fakeKeyCacheInvalidator) InvalidateCachedKey(_ context.Context, hexHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, hexHash)
	return f.err
}

func (f *fakeKeyCacheInvalidator) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.invalidated))
	copy(out, f.invalidated)
	return out
}
