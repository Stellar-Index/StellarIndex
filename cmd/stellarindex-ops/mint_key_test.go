package main

import (
	"strings"
	"testing"
)

// TestMintKey_RejectsMalformedIdentifier — input-validation
// (audit-2026-07-23). -identifier's own help text documents a
// kebab-case slug shape ("e.g. customer-acme-corp"); prior to this
// fix only non-emptiness was enforced, so anything else (spaces,
// uppercase, control characters, an unbounded length) passed
// straight through to store.Create and got persisted. This asserts
// the CLI actually rejects out-of-shape identifiers BEFORE reaching
// config load / Redis — flag validation must fail fast on args
// alone, no config file or network required for these cases to be
// caught.
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
			err := mintKey(args)
			if err == nil {
				t.Fatalf("mintKey(-identifier=%q): expected an error, got nil", tc.identifier)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("mintKey(-identifier=%q): error = %q, want substring %q",
					tc.identifier, err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestMintKey_AcceptsWellFormedIdentifier_PastValidation confirms the
// positive case: a documented-shape identifier clears the new checks
// and the function proceeds to the NEXT gate (tier / config load),
// not the identifier/label validation — i.e. the pattern isn't
// accidentally rejecting the very shape it documents as valid.
func TestMintKey_AcceptsWellFormedIdentifier_PastValidation(t *testing.T) {
	err := mintKey([]string{
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
	err := mintKey([]string{
		"-config", "/nonexistent.toml",
		"-identifier", "customer-acme-corp",
		"-label", strings.Repeat("a", mintKeyLabelMaxLen+1),
	})
	if err == nil || !strings.Contains(err.Error(), "-label must be <=") {
		t.Errorf("expected an over-length -label to be rejected, got: %v", err)
	}
}

// mint-key refuses, before any config / Redis / Postgres access, a mint
// with no recorded reason, an operator key without acknowledgement, and a
// budget or scope outside what every other mint surface accepts.
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
			err := mintKey(append(append([]string{}, base...), tc.extra...))
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("mintKey(%v) = %v, want an error containing %q", tc.extra, err, tc.wantSubstr)
			}
		})
	}

	// Acknowledged operator mint with a reason clears validation and fails
	// only at config load.
	err := mintKey(append(append([]string{}, base...), "-reason", "r", "-tier", "operator", "-confirm-operator", "-scopes", "admin"))
	if err == nil || strings.Contains(err.Error(), "-reason") || strings.Contains(err.Error(), "-confirm-operator") ||
		strings.Contains(err.Error(), "scope") {
		t.Fatalf("valid operator mint rejected by validation: %v", err)
	}
}

func TestUpgradeKey_RequiresReasonAndCeiling(t *testing.T) {
	base := []string{"-config", "/nonexistent.toml", "-key-id", "kid_x", "-actor", "alice"}
	if err := upgradeKey(append(append([]string{}, base...), "-rate-limit-per-min", "5000")); err == nil ||
		!strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("upgrade-key without -reason = %v, want -reason is required", err)
	}
	if err := upgradeKey(append(append([]string{}, base...), "-reason", "r", "-rate-limit-per-min", "10000000")); err == nil ||
		!strings.Contains(err.Error(), "must be in [0, 100000]") {
		t.Errorf("upgrade-key above the ceiling = %v, want a bound error", err)
	}
}
