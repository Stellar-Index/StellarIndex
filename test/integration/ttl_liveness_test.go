//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests prove the v0.21.4 TTL-liveness path end-to-end against a real
// ClickHouse: rows inserted into stellar.ledger_entry_changes flow through the
// ttl_live_until_mv materialized view (applied from tier1_schema.sql by the
// harness) into the slim stellar.ttl_live_until projection, and
// ClassifyTTLLiveness resolves verdicts from THAT table — the
// ledger_entries_current scan path no longer exists.

// ttlAsOf is the reference ledger for verdicts. Fixture ledgers sit far above
// any other test's ranges to keep the shared container's keys disjoint.
const ttlAsOf = uint32(72_500_000)

// ttlGovernedKeyXDR builds a distinctive base64 LedgerKey for the governed
// (e.g. contract_data) entry — the input shape callers hand to
// ClassifyTTLLiveness.
func ttlGovernedKeyXDR(tag string) string {
	return base64.StdEncoding.EncodeToString([]byte("ttl-liveness-it-governed-" + tag))
}

// ttlChangeRow renders the lake's TTL change for the entry governed by
// governedXDR: KeyXDR is the 36-byte TTL LedgerKey (type=00000009 |
// sha256(decoded governed key)), EntryXDR the TTLEntry
// (lastModified(4) | data.type(4) | keyHash(32) | liveUntil(4) | ext(4)),
// truncated/padded to entryLen so malformed shapes can be seeded too.
func ttlChangeRow(governedXDR string, ledger, intra, liveUntil uint32, entryLen int) chstore.LedgerEntryChangeRow {
	rawKey, err := base64.StdEncoding.DecodeString(governedXDR)
	if err != nil {
		panic(fmt.Sprintf("fixture governed key must be valid base64: %v", err))
	}
	keyHash := sha256.Sum256(rawKey)

	ttlKey := make([]byte, 0, 36)
	ttlKey = append(ttlKey, 0x00, 0x00, 0x00, 0x09) // LedgerEntryType TTL = 9
	ttlKey = append(ttlKey, keyHash[:]...)

	full := make([]byte, 48)
	binary.BigEndian.PutUint32(full[0:4], ledger) // lastModifiedLedgerSeq
	binary.BigEndian.PutUint32(full[4:8], 9)      // data.type = TTL
	copy(full[8:40], keyHash[:])
	binary.BigEndian.PutUint32(full[40:44], liveUntil) // XDR big-endian
	// full[44:48] = ext.v 0
	entry := full
	if entryLen != len(full) {
		entry = make([]byte, entryLen)
		copy(entry, full)
	}

	return chstore.LedgerEntryChangeRow{
		LedgerSeq:      ledger,
		CloseTime:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		TxHash:         "ttl-liveness-it-" + governedXDR[:8],
		OpIndex:        0,
		ChangeIndex:    0,
		ChangeType:     "updated",
		EntryType:      "ttl",
		KeyXDR:         base64.StdEncoding.EncodeToString(ttlKey),
		EntryXDR:       base64.StdEncoding.EncodeToString(entry),
		IntraLedgerSeq: intra,
	}
}

func TestClassifyTTLLiveness_SlimProjectionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	var (
		archived  = ttlGovernedKeyXDR("archived")  // lapsed long ago
		boundary  = ttlGovernedKeyXDR("boundary")  // live_until == asOf → LIVE (not yet lapsed)
		extended  = ttlGovernedKeyXDR("extended")  // lapsed then bumped in a later ledger → LIVE
		sameLedgr = ttlGovernedKeyXDR("sameledger") // bumped twice in ONE ledger → intra tie-break
		malformed = ttlGovernedKeyXDR("malformed") // 47-byte entry → MV skips → UNKNOWN
		absent    = ttlGovernedKeyXDR("absent")    // no TTL row at all → UNKNOWN
	)

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archived, 72_000_001, 1, ttlAsOf-1_000_000, 48),
		ttlChangeRow(boundary, 72_000_002, 1, ttlAsOf, 48),
		// extended: the LATER (winning) change is handed to the writer FIRST —
		// adversarial insert order; argMax(live_until, version) must not care.
		ttlChangeRow(extended, 72_000_100, 1, ttlAsOf+5_000_000, 48),
		ttlChangeRow(extended, 72_000_003, 1, ttlAsOf-2_000_000, 48),
		// sameLedgr: two changes in ONE ledger. Only intra_ledger_seq (the low
		// 32 bits of the RMT version) separates them; the later one extends.
		ttlChangeRow(sameLedgr, 72_000_004, 7, ttlAsOf+3_000_000, 48),
		ttlChangeRow(sameLedgr, 72_000_004, 6, ttlAsOf-3_000_000, 48),
		ttlChangeRow(malformed, 72_000_005, 1, ttlAsOf+1_000_000, 47),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	keys := []string{archived, boundary, extended, sameLedgr, malformed, absent, "not!valid!base64"}
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, keys, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness: %v", err)
	}

	want := map[string]chstore.TTLLiveness{
		archived:           chstore.TTLArchived,
		boundary:           chstore.TTLLive,
		extended:           chstore.TTLLive,
		sameLedgr:          chstore.TTLLive,
		malformed:          chstore.TTLUnknown, // fail-open: unrecognised shape never proves archived
		absent:             chstore.TTLUnknown,
		"not!valid!base64": chstore.TTLUnknown, // undecodable input key stays kept, no error
	}
	if len(got) != len(want) {
		t.Errorf("got %d verdicts, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("verdict[%s] = %v, want %v", k, got[k], w)
		}
	}
}

// TestClassifyTTLLiveness_MissingProjectionFailsLoud proves the no-fallback
// contract on a real server: with stellar.ttl_live_until absent, the
// classifier returns an error naming the DDL artifact instead of silently
// degrading every key to TTLUnknown (which would read as "nothing archived").
// The table is renamed away and restored — integration tests in this package
// run sequentially, and no ledger_entry_changes insert happens in the window
// (the MV would otherwise error on its missing target).
func TestClassifyTTLLiveness_MissingProjectionFailsLoud(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until TO stellar.ttl_live_until_it_hidden"); err != nil {
		t.Fatalf("hide ttl_live_until: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until_it_hidden TO stellar.ttl_live_until"); err != nil {
			t.Fatalf("restore ttl_live_until (container state now broken for later tests): %v", err)
		}
	}
	defer restore()

	_, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err == nil {
		t.Fatal("ClassifyTTLLiveness succeeded without the projection; the deleted scan path must not have a silent fallback")
	}
	if !strings.Contains(err.Error(), "deploy/clickhouse/ttl_live_until.sql") {
		t.Errorf("error does not point the operator at the DDL artifact: %v", err)
	}

	// Restore, then prove the guard clears: absent keys resolve UNKNOWN, no error.
	restore()
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness after restore: %v", err)
	}
	if got[ttlGovernedKeyXDR("missing-table")] != chstore.TTLUnknown {
		t.Errorf("expected TTLUnknown for a key with no TTL row, got %v", got[ttlGovernedKeyXDR("missing-table")])
	}
}
