package keys

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestRunMintKey_RecordsAuditRow: a CLI mint writes the same durable key.mint row as the HTTP path (staff actor, target, who, why, grants).
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

// TestRunMintKey_AuditFailureRevokesKey: no record, no credential; a mint whose audit row fails is revoked and its plaintext never returned.
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

// TestRunUpgradeKey_AuditsAndRollsBack: upgrade-key records old and new budget, and a failed audit row rolls the budget back.
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

// Without -write upgrade-key only reads the key: the budget stays put.
func TestPreviewUpgradeKey_ChangesNothing(t *testing.T) {
	ctx := context.Background()
	store := newKeyTestStore(t)
	rec, _, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: "customer-acme", Label: "Acme", RateLimitPerMin: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := previewUpgradeKey(ctx, store, upgradeKeyOpts{keyID: rec.KeyID, rateLimit: 50000}); err != nil {
		t.Fatalf("previewUpgradeKey: %v", err)
	}
	got, err := store.GetByKeyID(ctx, rec.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimitPerMin != 1000 {
		t.Errorf("preview changed the budget to %d", got.RateLimitPerMin)
	}
}

// TestMintKey_RejectsMalformedIdentifier: the documented kebab-case slug shape must be enforced at the CLI boundary, before config load or Redis, so out-of-shape values never reach store.Create.
func TestMintKey_RejectsMalformedIdentifier(t *testing.T) {
	baseArgs := []string{"-config", "/nonexistent.toml", "-label", "Acme Corp"}

	cases := []struct {
		name       string
		identifier string
		wantSubstr string
	}{
		{"empty", "", "-identifier is required"},
		{"whitespace_only", "   ", "-identifier is required"},
		{"contains_space", "customer acme corp", "kebab-case slug"},
		{"uppercase", "Customer-Acme-Corp", "kebab-case slug"},
		{"leading_hyphen", "-customer-acme", "kebab-case slug"},
		{"double_hyphen", "customer--acme", "kebab-case slug"},
		{"control_chars", "customer\x00acme", "kebab-case slug"},
		{"too_long", strings.Repeat("a", mintKeyIdentifierMaxLen+1), "must be <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, baseArgs...), "-identifier", tc.identifier)
			err := Mint(args)
			if err == nil {
				t.Fatalf("Mint(-identifier=%q): expected an error, got nil", tc.identifier)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Mint(-identifier=%q): error = %q, want substring %q",
					tc.identifier, err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestMintKey_AcceptsWellFormedIdentifier_PastValidation: a documented-shape identifier clears validation and reaches the next gate (tier / config load).
func TestMintKey_AcceptsWellFormedIdentifier_PastValidation(t *testing.T) {
	err := Mint([]string{
		"-config", "/nonexistent.toml",
		"-identifier", "customer-acme-corp",
		"-label", "Acme Corp - production",
		"-reason", "onboarding", "-actor", "alice",
	})
	if err == nil {
		t.Fatal("expected an error (config file does not exist), got nil")
	}
	if strings.Contains(err.Error(), "kebab-case slug") || strings.Contains(err.Error(), "-identifier") ||
		strings.Contains(err.Error(), "-label") {
		t.Errorf("well-formed identifier/label incorrectly rejected by input validation: %v", err)
	}
}

// TestMintKey_RejectsOverlongLabel.
func TestMintKey_RejectsOverlongLabel(t *testing.T) {
	err := Mint([]string{
		"-config", "/nonexistent.toml",
		"-identifier", "customer-acme-corp",
		"-label", strings.Repeat("a", mintKeyLabelMaxLen+1),
	})
	if err == nil || !strings.Contains(err.Error(), "-label must be <=") {
		t.Errorf("expected an over-length -label to be rejected, got: %v", err)
	}
}

// mint-key refuses, before any config / Redis / Postgres access, a mint with no reason, an operator key without acknowledgement, or a budget or scope outside what other mint surfaces accept.
func TestMintKey_RequiresReasonAckAndBounds(t *testing.T) {
	base := []string{"-config", "/nonexistent.toml", "-identifier", "customer-acme", "-label", "Acme", "-actor", "alice"}
	cases := []struct {
		name       string
		extra      []string
		wantSubstr string
	}{
		{"no-reason", nil, "-reason is required"},
		{"blank-reason", []string{"-reason", "   "}, "-reason is required"},
		{"operator-unacknowledged", []string{"-reason", "r", "-tier", "operator"}, "-confirm-operator"},
		{"rate-above-ceiling", []string{"-reason", "r", "-rate-limit-per-min", "10000000"}, "must be in [0, 100000]"},
		{"negative-rate", []string{"-reason", "r", "-rate-limit-per-min", "-5"}, "must be in [0, 100000]"},
		{"wildcard-scope", []string{"-reason", "r", "-scopes", "*"}, "unknown key scope"},
		{"unknown-scope", []string{"-reason", "r", "-scopes", "read,superuser"}, "unknown key scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Mint(append(append([]string{}, base...), tc.extra...))
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Mint(%v) = %v, want an error containing %q", tc.extra, err, tc.wantSubstr)
			}
		})
	}

	// Acknowledged operator mint with a reason clears validation and fails
	// only at config load.
	err := Mint(append(append([]string{}, base...), "-reason", "r", "-tier", "operator", "-confirm-operator", "-scopes", "admin"))
	if err == nil || strings.Contains(err.Error(), "-reason") || strings.Contains(err.Error(), "-confirm-operator") ||
		strings.Contains(err.Error(), "scope") {
		t.Fatalf("valid operator mint rejected by validation: %v", err)
	}
}

// Without -write mint-key previews and never reaches the key store: Redis points at a closed port, so a minting run would fail on the ping.
func TestMintKey_DryRunMintsNothing(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "closed-redis.toml")
	if err := os.WriteFile(cfg, []byte("[storage]\nredis_addr = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Mint([]string{"-config", cfg, "-identifier", "customer-acme", "-label", "Acme", "-actor", "alice", "-reason", "r"})
	if err != nil {
		t.Fatalf("dry run reached a store: %v", err)
	}
}

// TestUpgradeKey_HelpMatchesParser: the parser rejects a negative -rate-limit-per-min and treats 0 as "reset to tier default", so the help must not say to pass -1.
func TestUpgradeKey_HelpMatchesParser(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	helpErr := Upgrade([]string{"-h"})
	os.Stderr = orig
	_ = w.Close()
	usage, _ := io.ReadAll(r)
	if !errors.Is(helpErr, flag.ErrHelp) {
		t.Fatalf("Upgrade(-h) = %v, want flag.ErrHelp", helpErr)
	}
	if strings.Contains(string(usage), "-1") {
		t.Errorf("help advertises -1, which the parser rejects:\n%s", usage)
	}

	negErr := Upgrade([]string{"-config", "/nonexistent.toml", "-key-id", "kid_x", "-rate-limit-per-min", "-1"})
	if negErr == nil || !strings.Contains(negErr.Error(), "must be >= 0") {
		t.Errorf("negative -rate-limit-per-min must be refused, got: %v", negErr)
	}
}

func TestUpgradeKey_WritesOnlyWithWriteFlag(t *testing.T) {
	base := []string{"-config", "c.toml", "-key-id", "kid_x", "-rate-limit-per-min", "5000", "-reason", "r", "-actor", "alice"}
	for _, tc := range []struct {
		extra []string
		write bool
	}{{nil, false}, {[]string{"-dry-run"}, false}, {[]string{"-write"}, true}} {
		opts, err := parseUpgradeKeyFlags(append(append([]string{}, base...), tc.extra...))
		if err != nil {
			t.Fatalf("%v: %v", tc.extra, err)
		}
		if opts.write != tc.write {
			t.Errorf("%v: write = %v, want %v", tc.extra, opts.write, tc.write)
		}
	}
}

func TestUpgradeKey_RequiresReasonAndCeiling(t *testing.T) {
	base := []string{"-config", "/nonexistent.toml", "-key-id", "kid_x", "-actor", "alice"}
	if err := Upgrade(append(append([]string{}, base...), "-rate-limit-per-min", "5000")); err == nil ||
		!strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("upgrade-key without -reason = %v, want -reason is required", err)
	}
	if err := Upgrade(append(append([]string{}, base...), "-reason", "r", "-rate-limit-per-min", "10000000")); err == nil ||
		!strings.Contains(err.Error(), "must be in [0, 100000]") {
		t.Errorf("upgrade-key above the ceiling = %v, want a bound error", err)
	}
}
