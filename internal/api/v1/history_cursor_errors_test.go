package v1

import (
	"encoding/base64"
	"strings"
	"testing"
)

// decodeHistoryCursor has eight failure modes; the round-trip
// test in history_cursor_internal_test.go pins the success path.
// This file pins each error branch so a refactor can't silently
// loosen any of them — a hand-crafted cursor with the wrong shape
// MUST be rejected, otherwise pagination accepts attacker-supplied
// values and serves wrong-looking pages.

func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestDecodeHistoryCursor_badBase64(t *testing.T) {
	_, err := decodeHistoryCursor("!!!not-base64!!!")
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("expected base64-tagged error, got %v", err)
	}
}

func TestDecodeHistoryCursor_wrongPartCount(t *testing.T) {
	for _, raw := range []string{
		"",         // empty
		"only:two", // 2 parts
		"a:b:c",    // 3 parts
		"a:b:c:d",  // 4 parts (missing op_index)
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := decodeHistoryCursor(encodeRaw(raw))
			if err == nil {
				t.Errorf("expected error for %q, got nil", raw)
			}
		})
	}
}

func TestDecodeHistoryCursor_badTimestamp(t *testing.T) {
	bad := encodeRaw("not-a-number:1:src:00:0")
	_, err := decodeHistoryCursor(bad)
	if err == nil || !strings.Contains(err.Error(), "ts") {
		t.Errorf("expected ts-tagged error, got %v", err)
	}
}

func TestDecodeHistoryCursor_badLedger(t *testing.T) {
	bad := encodeRaw("0:not-a-number:src:" + strings.Repeat("0", 64) + ":0")
	_, err := decodeHistoryCursor(bad)
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Errorf("expected ledger-tagged error, got %v", err)
	}
}

func TestDecodeHistoryCursor_emptySource(t *testing.T) {
	// Empty source weakens the full-PK comparison into a partial
	// one — must be rejected. Same-ledger page-skip bug returns
	// otherwise.
	bad := encodeRaw("0:1::" + strings.Repeat("0", 64) + ":0")
	_, err := decodeHistoryCursor(bad)
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Errorf("expected source-tagged error, got %v", err)
	}
}

func TestDecodeHistoryCursor_badTxHash(t *testing.T) {
	for _, txHash := range []string{
		"too-short", // not 64 chars
		"NOTLOWERHEX0000000000000000000000000000000000000000000000000000", // uppercase
		strings.Repeat("z", 64), // 64 chars but not hex
	} {
		t.Run(txHash, func(t *testing.T) {
			bad := encodeRaw("0:1:src:" + txHash + ":0")
			_, err := decodeHistoryCursor(bad)
			if err == nil || !strings.Contains(err.Error(), "tx_hash") {
				t.Errorf("expected tx_hash-tagged error, got %v", err)
			}
		})
	}
}

func TestDecodeHistoryCursor_badOpIndex(t *testing.T) {
	bad := encodeRaw("0:1:src:" + strings.Repeat("0", 64) + ":not-a-number")
	_, err := decodeHistoryCursor(bad)
	if err == nil || !strings.Contains(err.Error(), "op_index") {
		t.Errorf("expected op_index-tagged error, got %v", err)
	}
}

// isLowerHex64: pinning the helper directly so a refactor can't
// silently widen the predicate (e.g., accept uppercase or shorten).
func TestIsLowerHex64(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("0", 64), true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("f", 64), true},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"", false},                      // empty
		{strings.Repeat("0", 63), false}, // too short
		{strings.Repeat("0", 65), false}, // too long
		{strings.Repeat("g", 64), false}, // non-hex char
		{strings.Repeat("A", 64), false}, // uppercase rejected
		{"0000000000000000000000000000000000000000000000000000000000000-00", false}, // dash
	}
	for _, tc := range cases {
		if got := isLowerHex64(tc.in); got != tc.want {
			t.Errorf("isLowerHex64(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
