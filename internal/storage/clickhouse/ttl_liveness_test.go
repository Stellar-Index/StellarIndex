package clickhouse

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTTLKeyHashMatchesCoreDerivation pins stellar-core's LedgerKeyTtl.keyHash: sha256 over the DECODED
// LedgerKey bytes. Hashing the base64 text fails silently: every lookup misses, every entry comes back
// TTLUnknown, and the filter degrades to a no-op that looks like "nothing archived".
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

// ttlDDLFiles returns the canonical schema and the operator migration artifact carrying the TTL extraction.
// Both must match the Go layout constants; a drifted offset silently corrupts the projection at ingest.
func ttlDDLFiles(t *testing.T) map[string]string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	out := make(map[string]string, 2)
	for _, rel := range []string{
		filepath.Join("deploy", "clickhouse", "tier1_schema.sql"),
		filepath.Join("deploy", "clickhouse", "ttl_live_until.sql"),
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read DDL %s: %v", rel, err)
		}
		out[rel] = string(raw)
	}
	return out
}

// TestTTLLiveUntilDDLIsShapeGuarded: the MV must insert only rows with exact 36/48-byte key/entry lengths, so
// a layout change yields no row rather than a garbage liveUntil that deletes live balances. It must also use
// tryBase64Decode, since a decode throw inside the MV would fail the source INSERT and block ingest.
func TestTTLLiveUntilDDLIsShapeGuarded(t *testing.T) {
	wantExprs := []string{
		// live_until: 4 bytes at decoded offset 40 (0-indexed) = 41
		// (ClickHouse substring is 1-indexed), reversed for big-endian XDR
		// vs little-endian reinterpretAsUInt32.
		fmt.Sprintf("reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), %d, 4)))", ttlLiveUntilOffset0+1),
		// key_hash: the sha256(LedgerKey) bytes after the 4-byte type
		// discriminant, 1-indexed.
		fmt.Sprintf("substring(tryBase64Decode(key_xdr), %d, %d)", ttlKeyHashOffset+1, ttlKeyHashLen),
		// The exact-length fail-open guards.
		fmt.Sprintf("length(tryBase64Decode(key_xdr)) = %d", ttlLedgerKeyLen),
		fmt.Sprintf("length(tryBase64Decode(entry_xdr)) = %d", ttlEntryLen),
		// The composite ReplacingMergeTree version (same as
		// ledger_entries_current).
		"bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq",
		// Source filter.
		"entry_type = 'ttl'",
	}
	for rel, ddl := range ttlDDLFiles(t) {
		for _, expr := range wantExprs {
			if !strings.Contains(ddl, expr) {
				t.Errorf("%s: missing extraction expression %q", rel, expr)
			}
		}
		// The throwing decode must not appear anywhere in the TTL MV files'
		// executable extraction. tier1_schema.sql legitimately never uses
		// base64Decode; the artifact must not reintroduce it either.
		if rel == "deploy/clickhouse/ttl_live_until.sql" {
			for _, line := range strings.Split(ddl, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") {
					continue // runbook/verify examples in comments may differ
				}
				if strings.Contains(line, "base64Decode(") && !strings.Contains(line, "tryBase64Decode(") {
					t.Errorf("%s: executable DDL uses throwing base64Decode: %q", rel, line)
				}
			}
		}
	}
}

// TestTTLEntryLayoutConstants documents the wire layout behind the offsets: TTL LedgerKey is 36 bytes
// (type 4 + keyHash 32); TTL LedgerEntry is 48 (lastModified 4, type 4, keyHash 32, liveUntil 4, ext 4).
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

// TestTTLLivenessQueryIsSlimLookup pins the query shape: a bounded primary-key lookup over the
// slim projection, latest state via argMax(live_until, version), thread/memory guard rails kept,
// and no scan of ledger_entries_current or per-row entry_xdr decode (that design OOM'd).
func TestTTLLivenessQueryIsSlimLookup(t *testing.T) {
	q := ttlLivenessBatchQuery([]string{"unhex(?)", "unhex(?)"})

	for _, want := range []string{
		"FROM stellar.ttl_live_until",
		"argMax(live_until, version)",
		"key_hash IN (unhex(?), unhex(?))",
		"GROUP BY key_hash",
		"max_threads = 4",
		"max_memory_usage = 8000000000",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("batch query missing %q:\n%s", want, q)
		}
	}
	for _, banned := range []string{"ledger_entries_current", "entry_xdr", "base64Decode"} {
		if strings.Contains(q, banned) {
			t.Errorf("batch query resurrects the deleted scan path (%q):\n%s", banned, q)
		}
	}
}

// TestTTLMissingTableErrorNamesTheArtifact pins fail-loud behaviour: an unprovisioned deployment gets an
// error naming what to apply, not a silent all-UNKNOWN result that reads as "nothing was archived".
func TestTTLMissingTableErrorNamesTheArtifact(t *testing.T) {
	msg := errTTLLiveUntilTableMissing.Error()
	if !strings.Contains(msg, "deploy/clickhouse/ttl_live_until.sql") {
		t.Errorf("missing-table error does not name the DDL artifact: %q", msg)
	}
	if !strings.Contains(msg, "stellar.ttl_live_until") {
		t.Errorf("missing-table error does not name the table: %q", msg)
	}
}

// TestTTLVerdictAt pins the one liveness rule every TTL reader shares: live
// through live_until inclusive, archived after, and a zero never archives.
func TestTTLVerdictAt(t *testing.T) {
	cases := []struct {
		liveUntil, asOf uint32
		want            TTLLiveness
	}{
		{0, 64_000_000, TTLUnknown},
		{54_400_000, 64_277_149, TTLArchived},
		{64_277_148, 64_277_149, TTLArchived},
		{64_277_149, 64_277_149, TTLLive},
		{70_000_000, 64_277_149, TTLLive},
	}
	for _, tc := range cases {
		if got := TTLVerdictAt(tc.liveUntil, tc.asOf); got != tc.want {
			t.Errorf("TTLVerdictAt(%d, %d) = %d, want %d", tc.liveUntil, tc.asOf, got, tc.want)
		}
	}
}
