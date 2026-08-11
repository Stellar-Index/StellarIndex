package dashboardauth

import (
	"bytes"
	"testing"
)

// testCodeSecret is an obvious test-only placeholder, not a real key.
var testCodeSecret = []byte("test-code-secret-placeholder-not-a-real-key")

// TestCodeForHash_AlwaysSixDigits — whatever the hash, the derived
// code must be a clean 6 ASCII digits (the historical bug was a
// 5-digit code padded with a NUL byte).
func TestCodeForHash_AlwaysSixDigits(t *testing.T) {
	g := &Generator{Secret: testCodeSecret}
	for i := 0; i < 256; i++ {
		hash := make([]byte, 32)
		for j := range hash {
			hash[j] = byte((i*7 + j*13) % 251)
		}
		code := g.CodeForHash(hash)
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

// TestCodeForHash_ShortHashIsSafe — a hash too short to derive from
// returns empty rather than producing a code.
func TestCodeForHash_ShortHashIsSafe(t *testing.T) {
	g := &Generator{Secret: testCodeSecret}
	if got := g.CodeForHash([]byte{1, 2}); got != "" {
		t.Fatalf("CodeForHash(short) = %q, want empty", got)
	}
}

// TestGeneratedCodeMatchesHash — the code emailed at mint time must be
// the same one CodeForHash re-derives from the stored hash under the
// same secret, or verify-code could never match a real token.
func TestGeneratedCodeMatchesHash(t *testing.T) {
	// Deterministic entropy so the test is stable.
	read := func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}
	g := &Generator{Read: read, Secret: testCodeSecret}
	_, hash, code, err := g.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if got := g.CodeForHash(hash); got != code {
		t.Fatalf("CodeForHash(hash) = %q, minted code = %q — must match", got, code)
	}
	if len(code) != 6 {
		t.Fatalf("minted code %q is not 6 digits", code)
	}
	// Sanity: the stored hash is the sha256 of the plaintext, 32 bytes.
	if len(hash) != 32 || bytes.Equal(hash, make([]byte, 32)) {
		t.Fatalf("unexpected hash shape: len=%d", len(hash))
	}
}

// TestCodeDerivationIsKeyed is the regression for the audit finding
// "the 6-digit code is derivable from the stored hash": the code must
// be a function of the SERVER SECRET, not of the stored hash alone.
// If a future refactor drops the key, two generators with different
// secrets would agree on every code again and this test fails.
func TestCodeDerivationIsKeyed(t *testing.T) {
	a := &Generator{Secret: []byte("secret-a-placeholder")}
	b := &Generator{Secret: []byte("secret-b-placeholder")}

	agree := 0
	const trials = 64
	for i := 0; i < trials; i++ {
		hash := make([]byte, 32)
		for j := range hash {
			hash[j] = byte((i*31 + j*17 + 5) % 253)
		}
		if a.CodeForHash(hash) == b.CodeForHash(hash) {
			agree++
		}
	}
	// Random 6-digit collisions happen at ~1e-6 per trial; even a
	// handful of agreements would mean the secret isn't load-bearing.
	if agree > 2 {
		t.Fatalf("codes agreed on %d/%d hashes across different secrets — derivation is not keyed", agree, trials)
	}

	// And the same secret must be deterministic (or verify-code breaks).
	hash := make([]byte, 32)
	for j := range hash {
		hash[j] = byte(j * 3)
	}
	if a.CodeForHash(hash) != a.CodeForHash(hash) {
		t.Fatal("same-secret derivation is not deterministic")
	}
}

// TestValidateNeverLeavesCodeDerivationUnkeyed — Config.validate()
// must refuse to run the code path without SOME secret: an empty
// secret is exactly the offline-derivable state the audit flagged.
func TestValidateNeverLeavesCodeDerivationUnkeyed(t *testing.T) {
	rig := newTestRig(t) // no explicit secret configured
	if len(rig.h.cfg.Generator.Secret) == 0 {
		t.Fatal("validate() left Generator.Secret empty — 6-digit codes are derivable from the stored hash again")
	}
}
