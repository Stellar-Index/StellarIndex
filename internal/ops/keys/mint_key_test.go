package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
