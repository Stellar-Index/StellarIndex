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

// ttlDDLFiles returns the two DDL files that carry the TTL extraction since
// v0.21.4 moved it out of Go query text: the canonical fresh-deployment schema
// and the operator migration artifact. Both must stay in lockstep with the Go
// layout constants — the extraction runs at INGEST now, so a drifted offset
// would corrupt the projection silently rather than fail a query.
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

// TestTTLLiveUntilDDLIsShapeGuarded pins the fail-open property of the
// liveUntil extraction in BOTH DDL files (canonical schema + operator
// artifact), asserted against the Go layout constants. The MV inserts only
// rows whose decoded key/entry lengths are exactly 36/48 bytes, so a future
// protocol change to the TTLEntry layout yields NO row (→ key absent →
// TTLUnknown → entry KEPT) rather than a garbage ledger number that would
// read as "archived" and silently delete live balances.
//
// It also pins tryBase64Decode over base64Decode: inside a materialized view
// a decode THROW fails the whole source INSERT into ledger_entry_changes —
// i.e. blocks ingest — while try* + the length guards skip the row.
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
		// ledger_entries_current, audit-2026-07-16 C2-4c).
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

// TestTTLLivenessQueryIsSlimLookup pins the v0.21.4 query shape: a bounded
// primary-key lookup over the slim projection, latest state via
// argMax(live_until, version), thread/memory guard rails retained — and,
// critically, NO scan of ledger_entries_current and NO per-row entry_xdr
// decode (the design that OOM'd six production runs on 2026-07-29).
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

// TestTTLMissingTableErrorNamesTheArtifact pins the fail-loud contract: with
// the scan path deleted, an unprovisioned deployment must get an error that
// tells the operator exactly what to apply — not a silent all-UNKNOWN
// degradation that reads as "nothing was archived".
func TestTTLMissingTableErrorNamesTheArtifact(t *testing.T) {
	msg := errTTLLiveUntilTableMissing.Error()
	if !strings.Contains(msg, "deploy/clickhouse/ttl_live_until.sql") {
		t.Errorf("missing-table error does not name the DDL artifact: %q", msg)
	}
	if !strings.Contains(msg, "stellar.ttl_live_until") {
		t.Errorf("missing-table error does not name the table: %q", msg)
	}
}
