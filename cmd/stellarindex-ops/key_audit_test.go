package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

type recordingAudit struct {
	entries []platform.AuditEntry
	err     error
}

func (r *recordingAudit) Append(_ context.Context, e platform.AuditEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

func newKeyTestStore(t *testing.T) *auth.RedisAPIKeyStore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return auth.NewRedisAPIKeyStore(rdb)
}

func auditMeta(t *testing.T, e platform.AuditEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Metadata, &m); err != nil {
		t.Fatalf("audit metadata: %v", err)
	}
	return m
}

// A CLI mint lands the same durable key.mint row the HTTP path writes:
// staff actor, the minted key as target, who, why, and what was granted.
func TestRunMintKey_RecordsAuditRow(t *testing.T) {
	ctx := context.Background()
	store := newKeyTestStore(t)
	audit := &recordingAudit{}
	opts := mintKeyOpts{
		identifier: "customer-acme", label: "Acme", actor: "alice", reason: "onboarding 1234",
		tier: auth.TierOperator, rateLimit: 5000,
	}
	rec, plaintext, err := runMintKey(ctx, store, audit, opts)
	if err != nil || plaintext == "" {
		t.Fatalf("runMintKey = (%q, %v)", plaintext, err)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "key.mint" || e.ActorKind != platform.ActorStaff || e.TargetKind != "api_key" || e.TargetID != rec.KeyID {
		t.Errorf("audit entry = %+v, want key.mint / staff / api_key / %s", e, rec.KeyID)
	}
	m := auditMeta(t, e)
	for k, want := range map[string]any{
		"actor": "alice", "reason": "onboarding 1234", "tier": "operator",
		"target_identifier": "customer-acme", "full_access": true, "rate_limit_per_min": float64(5000), "expires_at": "never",
	} {
		if m[k] != want {
			t.Errorf("audit metadata[%s] = %v, want %v", k, m[k], want)
		}
	}
}

// No record, no credential: a mint whose audit row fails is revoked and
// its plaintext never returned.
func TestRunMintKey_AuditFailureRevokesKey(t *testing.T) {
	ctx := context.Background()
	store := newKeyTestStore(t)
	audit := &recordingAudit{err: errors.New("postgres down")}
	opts := mintKeyOpts{identifier: "customer-acme", label: "Acme", actor: "alice", reason: "r", tier: auth.TierAPIKey}
	_, plaintext, err := runMintKey(ctx, store, audit, opts)
	if err == nil || plaintext != "" {
		t.Fatalf("audit failure: runMintKey = (plaintext %q, err %v), want no plaintext and an error", plaintext, err)
	}
	keys, lerr := store.ListKeysForIdentifier(ctx, "customer-acme")
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, k := range keys {
		if k.RevokedAt.IsZero() {
			t.Errorf("unaudited key %s is still live", k.KeyID)
		}
	}
}

// upgrade-key records the old and new budget; a failed audit row rolls the
// budget back.
func TestRunUpgradeKey_AuditsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	store := newKeyTestStore(t)
	rec, _, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: "customer-acme", Label: "Acme", RateLimitPerMin: 1000})
	if err != nil {
		t.Fatal(err)
	}

	audit := &recordingAudit{}
	opts := upgradeKeyOpts{keyID: rec.KeyID, actor: "alice", reason: "partner deal", rateLimit: 50000}
	if _, err := runUpgradeKey(ctx, store, audit, opts); err != nil {
		t.Fatalf("runUpgradeKey: %v", err)
	}
	if len(audit.entries) != 1 || audit.entries[0].Action != "key.ratelimit.update" || audit.entries[0].TargetID != rec.KeyID {
		t.Fatalf("audit rows = %+v, want one key.ratelimit.update for %s", audit.entries, rec.KeyID)
	}
	m := auditMeta(t, audit.entries[0])
	if m["from_rate_limit_per_min"] != float64(1000) || m["to_rate_limit_per_min"] != float64(50000) || m["actor"] != "alice" || m["reason"] != "partner deal" {
		t.Errorf("audit metadata = %v", m)
	}

	failing := &recordingAudit{err: errors.New("postgres down")}
	opts.rateLimit = 90000
	if _, err := runUpgradeKey(ctx, store, failing, opts); err == nil {
		t.Fatal("audit failure: runUpgradeKey returned nil")
	}
	got, err := store.GetByKeyID(ctx, rec.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimitPerMin != 50000 {
		t.Errorf("after a failed audit the budget is %d, want it rolled back to 50000", got.RateLimitPerMin)
	}
}
