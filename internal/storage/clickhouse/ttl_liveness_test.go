package clickhouse

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestTTLKeyHashMatchesCoreDerivation pins the derivation stellar-core uses
// for LedgerKeyTtl.keyHash: sha256 over the DECODED LedgerKey bytes.
//
// Hashing the base64 TEXT instead of the decoded bytes is the easy mistake
// here, and it fails silently — every lookup simply misses, every entry comes
// back TTLUnknown, and the filter degrades to a no-op that looks like "nothing
// was archived" rather than like a bug.
func TestTTLKeyHashMatchesCoreDerivation(t *testing.T) {
	raw := []byte{0x00, 0x00, 0x00, 0x06, 0xDE, 0xAD, 0xBE, 0xEF}
	keyXDR := base64.StdEncoding.EncodeToString(raw)

	got, err := TTLKeyHash(keyXDR)
	if err != nil {
		t.Fatalf("TTLKeyHash: %v", err)
	}
	want := sha256.Sum256(raw)
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("TTLKeyHash = %s, want %s (sha256 of the DECODED key)", got, hex.EncodeToString(want[:]))
	}

	// Guard the specific wrong answer: hashing the base64 text.
	overText := sha256.Sum256([]byte(keyXDR))
	if got == hex.EncodeToString(overText[:]) {
		t.Error("TTLKeyHash hashed the base64 TEXT, not the decoded bytes")
	}
}

func TestTTLKeyHashRejectsUndecodableKey(t *testing.T) {
	if _, err := TTLKeyHash("not!valid!base64"); err == nil {
		t.Error("TTLKeyHash accepted an undecodable key; callers rely on the error to leave it TTLUnknown (kept)")
	}
}

// TestTTLLiveUntilExprIsShapeGuarded pins the fail-open property of the
// liveUntil extraction. The offsets are asserted against a decoded length of
// exactly 48 bytes, so a future protocol change to the TTLEntry layout yields
// 0 (→ TTLUnknown → entry KEPT) rather than a garbage ledger number that would
// read as "archived" and silently delete live balances.
func TestTTLLiveUntilExprIsShapeGuarded(t *testing.T) {
	if !strings.Contains(ttlLiveUntilExpr, "length(base64Decode(entry_xdr)) = 48") {
		t.Errorf("liveUntil expression is not length-guarded: %s", ttlLiveUntilExpr)
	}
	// liveUntilLedgerSeq sits at decoded byte 40 (0-indexed); ClickHouse
	// substring is 1-indexed, so the expression must say 41.
	if !strings.Contains(ttlLiveUntilExpr, "substring(base64Decode(entry_xdr), 41, 4)") {
		t.Errorf("liveUntil offset is wrong (want 1-indexed 41): %s", ttlLiveUntilExpr)
	}
	// XDR is big-endian, reinterpretAsUInt32 is little-endian.
	if !strings.Contains(ttlLiveUntilExpr, "reverse(") {
		t.Errorf("liveUntil expression must reverse for big-endian XDR: %s", ttlLiveUntilExpr)
	}
}

// TestTTLEntryLayoutConstants documents the wire layout the offsets encode, so
// a reader can check them against the XDR without running a query:
//
//	TTL LedgerKey  = type(4) + keyHash(32)                          = 36
//	TTL LedgerEntry= lastModified(4) + type(4) + keyHash(32)
//	                 + liveUntil(4) + ext(4)                        = 48
func TestTTLEntryLayoutConstants(t *testing.T) {
	if ttlLedgerKeyLen != 36 {
		t.Errorf("ttlLedgerKeyLen = %d, want 36", ttlLedgerKeyLen)
	}
	if ttlEntryLen != 48 {
		t.Errorf("ttlEntryLen = %d, want 48", ttlEntryLen)
	}
	if ttlLiveUntilOffset0+4+4 != ttlEntryLen {
		t.Errorf("liveUntil offset %d + 4 + ext 4 != entry length %d", ttlLiveUntilOffset0, ttlEntryLen)
	}
	if ttlKeyHashOffset+ttlKeyHashLen != ttlLedgerKeyLen {
		t.Errorf("key hash offset %d + len %d != key length %d", ttlKeyHashOffset, ttlKeyHashLen, ttlLedgerKeyLen)
	}
}
