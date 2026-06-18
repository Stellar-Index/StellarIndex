package dashboardauth

import (
	"bytes"
	"testing"
)

// TestCodeFromHash_AlwaysSixDigits guards the latent bug where the
// code was derived from 3 hash bytes → 5 base32 chars, leaving
// numericFromBase32 to pad the 6th position with a NUL byte. The
// emailed "6-digit code" was really 5 digits + \x00; once users type
// it, it must be a clean 6 ASCII digits.
func TestCodeFromHash_AlwaysSixDigits(t *testing.T) {
	for i := 0; i < 256; i++ {
		hash := make([]byte, 32)
		for j := range hash {
			hash[j] = byte((i*7 + j*13) % 251)
		}
		code := CodeFromHash(hash)
		if len(code) != 6 {
			t.Fatalf("hash seed %d: len(code) = %d, want 6 (%q)", i, len(code), code)
		}
		for k := 0; k < len(code); k++ {
			if code[k] < '0' || code[k] > '9' {
				t.Fatalf("hash seed %d: code %q has non-digit at %d", i, code, k)
			}
		}
	}
}

// TestCodeFromHash_ShortHashIsSafe — a hash too short to derive from
// returns empty rather than panicking on the slice.
func TestCodeFromHash_ShortHashIsSafe(t *testing.T) {
	if got := CodeFromHash([]byte{1, 2}); got != "" {
		t.Fatalf("CodeFromHash(short) = %q, want empty", got)
	}
}

// TestGeneratedCodeMatchesHash — the code emailed at mint time must be
// the same one CodeFromHash re-derives from the stored hash, or
// verify-code could never match a real token.
func TestGeneratedCodeMatchesHash(t *testing.T) {
	// Deterministic entropy so the test is stable.
	read := func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}
	_, hash, code, err := generateMagicLinkToken(read)
	if err != nil {
		t.Fatalf("generateMagicLinkToken: %v", err)
	}
	if got := CodeFromHash(hash); got != code {
		t.Fatalf("CodeFromHash(hash) = %q, minted code = %q — must match", got, code)
	}
	if len(code) != 6 {
		t.Fatalf("minted code %q is not 6 digits", code)
	}
	// Sanity: the stored hash is the sha256 of the plaintext, 32 bytes.
	if len(hash) != 32 || bytes.Equal(hash, make([]byte, 32)) {
		t.Fatalf("unexpected hash shape: len=%d", len(hash))
	}
}
