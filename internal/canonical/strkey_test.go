package canonical

import (
	"errors"
	"testing"
)

// Real CRC-valid strkeys for canonical pubnet entities. Picked
// because they're well-known and stable: USDC issuer (Circle),
// XLM SAC contract on pubnet, and the testnet root account.
//
// We don't need many — the goal is to prove the CRC check fires
// + admits the round-trip cases the test cases expect.
const (
	validAccountG  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" // USDC issuer
	validContractC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // XLM SAC
)

func TestIsAccountID_AcceptsRealStrkey(t *testing.T) {
	if !IsAccountID(validAccountG) {
		t.Errorf("IsAccountID(%q) = false, want true (real Circle USDC issuer)", validAccountG)
	}
}

func TestIsContractID_AcceptsRealStrkey(t *testing.T) {
	if !IsContractID(validContractC) {
		t.Errorf("IsContractID(%q) = false, want true (real XLM SAC)", validContractC)
	}
}

func TestIsAccountID_RejectsCRCMismatch(t *testing.T) {
	// Same length + prefix as a valid G-strkey, but the body is
	// wrong → CRC fails. The old regex-only validator would let
	// this through; the SDK-backed one rejects.
	mutated := mutateLastChar(validAccountG)
	if mutated == validAccountG {
		t.Fatalf("mutateLastChar produced no change for %q", validAccountG)
	}
	if IsAccountID(mutated) {
		t.Errorf("IsAccountID(%q) = true, want false (CRC mismatch)", mutated)
	}
}

func TestIsContractID_RejectsCRCMismatch(t *testing.T) {
	mutated := mutateLastChar(validContractC)
	if mutated == validContractC {
		t.Fatalf("mutateLastChar produced no change for %q", validContractC)
	}
	if IsContractID(mutated) {
		t.Errorf("IsContractID(%q) = true, want false (CRC mismatch)", mutated)
	}
}

func TestIsAccountID_RejectsWrongPrefix(t *testing.T) {
	// A valid C-strkey isn't a valid G-strkey even though both are
	// 56-char base32 — version-byte mismatch.
	if IsAccountID(validContractC) {
		t.Errorf("IsAccountID(%q) = true, want false (C-prefix is contract, not account)", validContractC)
	}
}

func TestIsContractID_RejectsWrongPrefix(t *testing.T) {
	if IsContractID(validAccountG) {
		t.Errorf("IsContractID(%q) = true, want false (G-prefix is account, not contract)", validAccountG)
	}
}

func TestIsAccountID_RejectsTooShort(t *testing.T) {
	if IsAccountID("GA5ZSEJY") {
		t.Error("IsAccountID accepted a too-short string")
	}
	if IsAccountID("") {
		t.Error("IsAccountID accepted empty string")
	}
}

func TestValidateAccountID_ErrorWrapsSentinel(t *testing.T) {
	// Callers downstream do errors.Is(err, ErrInvalidStrkey).
	err := validateAccountID("garbage")
	if err == nil {
		t.Fatal("validateAccountID(garbage) returned nil error")
	}
	if !errors.Is(err, ErrInvalidStrkey) {
		t.Errorf("validateAccountID error doesn't wrap ErrInvalidStrkey: %v", err)
	}
}

// SEP-23 test vector: the M-strkey for GA7QYNF7...SGZ with muxed id 0.
func TestMuxedAccountID_ResolvesUnderlyingAccount(t *testing.T) {
	const m = "MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK"
	const want = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	got, ok := MuxedAccountID(m)
	if !ok || got != want {
		t.Fatalf("MuxedAccountID(%q) = (%q, %v), want (%q, true)", m, got, ok, want)
	}
	for _, bad := range []string{want, mutateLastChar(m), "", "not-a-strkey"} {
		if g, ok := MuxedAccountID(bad); ok {
			t.Errorf("MuxedAccountID(%q) = (%q, true), want ok=false", bad, g)
		}
	}
}

// mutateLastChar swaps the final character with an adjacent valid
// base32 character so the result is the same length + prefix but a
// different payload — guaranteed to fail CRC verification.
func mutateLastChar(s string) string {
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	swap := byte('A')
	if last == 'A' {
		swap = 'B'
	}
	return s[:len(s)-1] + string(swap)
}
