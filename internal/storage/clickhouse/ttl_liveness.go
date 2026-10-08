package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Soroban archives a contract_data entry once its TTL lapses, but the lake keeps its last
// value forever, so "newest contract_data row for this key" serves an archived balance
// indefinitely. The signal is the companion TTL entry's `liveUntilLedgerSeq`.
//
// It is extracted once per TTL change, at ingest, into the slim `stellar.ttl_live_until`
// projection (key_hash -> live_until, ReplacingMergeTree(version)); this reader is a bounded
// primary-key lookup. Scanning ledger_entries_current for ttl rows (586M rows, wide
// entry_xdr) OOMs, so there is no fallback: a missing projection gives
// [errTTLLiveUntilTableMissing].

// ttlKeyHashOffset/Len locate the 32-byte key hash in a decoded TTL LedgerKey: a 4-byte
// LedgerEntryType discriminant (TTL = 9) then sha256 of the governed LedgerKey.
const (
	ttlKeyHashOffset = 4
	ttlKeyHashLen    = 32
	ttlLedgerKeyLen  = ttlKeyHashOffset + ttlKeyHashLen
)

// ttlEntryLen is the decoded size of a TTL LedgerEntry:
//
//	lastModifiedLedgerSeq (4) | data.type (4) | keyHash (32) |
//	liveUntilLedgerSeq (4)    | ext.v (4)     = 48
//
// so liveUntilLedgerSeq starts at byte 40. The extraction lives in SQL DDL
// (deploy/clickhouse/tier1_schema.sql, ttl_live_until.sql), guarded on these exact lengths so
// an unrecognised shape is skipped (-> [TTLUnknown], entry kept), never misread as archived.
// ttl_liveness_test.go asserts both DDL files against these constants.
const (
	ttlEntryLen         = 48
	ttlLiveUntilOffset0 = 40
)

// ttlLivenessBatchSize caps key hashes per IN list: 1,500 keys is ~105 KiB of query text,
// inside ClickHouse's default 256 KiB max_query_size (5,000 failed the parse cap).
const ttlLivenessBatchSize = 1_500

// errTTLLiveUntilTableMissing means the slim projection is not provisioned. Refusing loudly is
// deliberate: degrading every key to TTLUnknown would make the archived-balance filter a
// silent no-op.
var errTTLLiveUntilTableMissing = errors.New(
	"clickhouse: stellar.ttl_live_until does not exist — apply deploy/clickhouse/ttl_live_until.sql " +
		"(table + materialized view, then its Step-2 windowed backfill) before running TTL-liveness reads; " +
		"there is no scan fallback")

// TTLKeyHash returns the TTL key hash for the entry whose base64 LedgerKey is keyXDR: sha256
// over the decoded key bytes, as stellar-core derives LedgerKeyTtl.keyHash.
func TTLKeyHash(keyXDR string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(keyXDR)
	if err != nil {
		return "", fmt.Errorf("clickhouse: TTLKeyHash: decode key_xdr: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TTLLiveness is the verdict for one entry key.
type TTLLiveness int

const (
	// TTLUnknown: no TTL entry found, or its wire shape was unrecognised. Callers must keep
	// the entry. Classic entries (no TTL) land here.
	TTLUnknown TTLLiveness = iota
	// TTLLive — liveUntilLedgerSeq is at or beyond the reference ledger.
	TTLLive
	// TTLArchived: liveUntilLedgerSeq has lapsed; the last-known value must not be reported
	// as current.
	TTLArchived
)

// ClassifyTTLLiveness resolves whether each base64 LedgerKey in keyXDRs is live as of
// asOfLedger, via a primary-key lookup of `stellar.ttl_live_until` (cost scales with the
// batch). Missing projection: [errTTLLiveUntilTableMissing].
//
// Keys with no TTL row come back [TTLUnknown] and callers KEEP them: dropping an unresolved
// entry understates supply, and a silent over-drop is harder to notice than an over-count.
// Only a parsed, lapsed liveUntilLedgerSeq justifies exclusion. Seeds resolve tens of
// thousands of keys, so work is chunked by [ttlLivenessBatchSize].
func ClassifyTTLLiveness(ctx context.Context, conn driver.Conn, keyXDRs []string, asOfLedger uint32) (map[string]TTLLiveness, error) {
	liveUntil, err := resolveTTLLiveUntil(ctx, conn, keyXDRs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TTLLiveness, len(keyXDRs))
	for _, k := range keyXDRs {
		out[k] = TTLVerdictAt(liveUntil[k], asOfLedger)
	}
	return out, nil
}

// TTLVerdictAt is the liveness rule every TTL reader shares: live through liveUntilLedgerSeq
// inclusive, archived once asOfLedger passes it. A zero live_until proves nothing: UNKNOWN,
// never a guessed archival.
func TTLVerdictAt(liveUntil, asOfLedger uint32) TTLLiveness {
	switch {
	case liveUntil == 0:
		return TTLUnknown
	case liveUntil < asOfLedger:
		return TTLArchived
	default:
		return TTLLive
	}
}

// resolveTTLLiveUntil returns the latest parsed liveUntilLedgerSeq for each
// base64 LedgerKey in keyXDRs that has a nonzero TTL row. Keys with no row, a
// zero row, or an undecodable key are absent from the result.
func resolveTTLLiveUntil(ctx context.Context, conn driver.Conn, keyXDRs []string) (map[string]uint32, error) {
	out := make(map[string]uint32, len(keyXDRs))
	if len(keyXDRs) == 0 {
		return out, nil
	}
	if err := ensureTTLLiveUntilTable(ctx, conn); err != nil {
		return nil, err
	}
	for start := 0; start < len(keyXDRs); start += ttlLivenessBatchSize {
		end := start + ttlLivenessBatchSize
		if end > len(keyXDRs) {
			end = len(keyXDRs)
		}
		batch, err := ttlLiveUntilBatch(ctx, conn, keyXDRs[start:end])
		if err != nil {
			return nil, fmt.Errorf("clickhouse: ClassifyTTLLiveness: %w", err)
		}
		for k, lu := range batch {
			out[k] = lu
		}
	}
	return out, nil
}

// ensureTTLLiveUntilTable probes for the projection and refuses with a deploy-pointing error
// when absent; one metadata query per ClassifyTTLLiveness call.
func ensureTTLLiveUntilTable(ctx context.Context, conn driver.Conn) error {
	var exists uint8
	if err := conn.QueryRow(ctx, "EXISTS TABLE stellar.ttl_live_until").Scan(&exists); err != nil {
		return fmt.Errorf("clickhouse: ClassifyTTLLiveness: probe stellar.ttl_live_until: %w", err)
	}
	if exists == 0 {
		return errTTLLiveUntilTableMissing
	}
	return nil
}

// ttlLivenessBatchQuery renders the per-batch lookup. argMax(live_until, version) keeps the
// latest TTL state per key (version = (ledger_seq<<32) | intra_ledger_seq, the table's RMT
// version), correct over un-merged duplicates without FINAL.
//
// The SETTINGS pins are guard rails (unpinned max_threads fanned a read out 40x its cost), so
// a future layout shift fails this query loudly instead of starving the host.
func ttlLivenessBatchQuery(placeholders []string) string {
	return fmt.Sprintf(`
		SELECT lower(hex(key_hash)) AS key_hash_hex,
		       argMax(live_until, version) AS live_until
		FROM stellar.ttl_live_until
		WHERE key_hash IN (%s)
		GROUP BY key_hash
		SETTINGS max_threads = 4,
		         max_memory_usage = 8000000000`,
		strings.Join(placeholders, ", "),
	)
}

// ttlLiveUntilBatch returns the newest non-zero live_until in stellar.ttl_live_until per key
// of one chunk; keys with no row, an undecodable key, or a stored 0 are absent.
func ttlLiveUntilBatch(ctx context.Context, conn driver.Conn, keyXDRs []string) (map[string]uint32, error) {
	// hash -> the key(s) it governs; the same key may appear twice in the input.
	byHash := make(map[string][]string, len(keyXDRs))
	args := make([]any, 0, len(keyXDRs))
	placeholders := make([]string, 0, len(keyXDRs))
	for _, k := range keyXDRs {
		h, err := TTLKeyHash(k)
		if err != nil {
			// An undecodable key cannot be proven archived: leave it unresolved (TTLUnknown).
			continue
		}
		if _, seen := byHash[h]; !seen {
			placeholders = append(placeholders, "unhex(?)")
			args = append(args, h)
		}
		byHash[h] = append(byHash[h], k)
	}
	out := make(map[string]uint32, len(keyXDRs))
	if len(placeholders) == 0 {
		return out, nil
	}

	rows, err := conn.Query(ctx, ttlLivenessBatchQuery(placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyHash   string
			liveUntil uint32
		)
		if err := rows.Scan(&keyHash, &liveUntil); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		// The MV's length guards mean 0 cannot come from misparsing, and a stored 0 still
		// proves nothing: UNKNOWN over a guess.
		if liveUntil == 0 {
			continue
		}
		for _, k := range byHash[strings.ToLower(keyHash)] {
			out[k] = liveUntil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	return out, nil
}
