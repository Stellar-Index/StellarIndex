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

// Soroban archives (evicts) a contract_data entry once its TTL lapses. The
// entry stops being part of live ledger state, but the lake still holds its
// last-known value forever — so any reader that takes "the newest
// contract_data row for this key" as current state will serve an archived
// balance indefinitely.
//
// That is not hypothetical. It is why PHO served +157% against Horizon
// (2026-07-28): `supply seed-sac-balances` wrote 122,148,204 PHO across 39
// contract holders whose entries had been archived since 2024-11/2025-03,
// while our LIVE observer's rows matched Horizon to 0.009%. Four of the five
// keys behind the largest balance had `live_until` in the 54.4M–56.5M range
// against a tip of 63.68M.
//
// The signal to separate them is already in the lake: every contract_data
// entry has a companion TTL entry carrying `liveUntilLedgerSeq`.
//
// HOW the signal is read changed in v0.21.4. The classifier used to scan
// `ledger_entries_current WHERE entry_type = 'ttl'` (586M rows) per batch,
// extracting liveUntilLedgerSeq from the WIDE entry_xdr column for every ttl
// row. Six production attempts failed across four mechanisms (IN-list parse
// cap, two client-pin OOMs, thread fan-out amplification, and terminally an
// OOM of the query's own 8 GiB pin inside AggregatingTransform); the design
// was wrong, not the tuning. The extraction now happens ONCE per TTL change,
// at ingest, into the slim `stellar.ttl_live_until` projection
// (key_hash → live_until, ReplacingMergeTree(version), ~20-30 GB vs 590 GB),
// and this reader is a primary-key lookup bounded by construction. The scan
// path is DELETED, not dormant — deployments without the projection get
// [errTTLLiveUntilTableMissing], never a silent fallback.

// ttlKeyHashOffset/Len locate the 32-byte key hash inside a decoded TTL
// LedgerKey. The key is 36 bytes: a 4-byte LedgerEntryType discriminant
// (TTL = 9) followed by sha256(LedgerKey) of the entry it governs.
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
// liveUntilLedgerSeq therefore starts at byte 40 (0-indexed). Since v0.21.4
// the extraction itself lives in SQL DDL — the `ttl_live_until` materialized
// view in deploy/clickhouse/tier1_schema.sql and the operator artifact
// deploy/clickhouse/ttl_live_until.sql — guarded on the exact decoded lengths
// so an unrecognised wire shape is SKIPPED (→ key absent → [TTLUnknown] →
// entry kept), never misread as archived. These constants remain the Go-side
// statement of that layout; ttl_liveness_test.go asserts both DDL files
// against them so the three cannot drift apart.
const (
	ttlEntryLen         = 48
	ttlLiveUntilOffset0 = 40
)

// ttlLivenessBatchSize caps how many key hashes ride in one IN list.
// 1,500 keys ≈ 105 KiB of query text (each key renders as
// `unhex('<64-hex>'), ` ≈ 70 bytes), safely inside ClickHouse's 256 KiB
// default max_query_size. The original 5,000 produced ~350 KiB and
// failed the parse cap on the first production run (2026-07-29) — the
// tool must fit DEFAULT server limits, not depend on a users.d raise.
const ttlLivenessBatchSize = 1_500

// errTTLLiveUntilTableMissing is returned when the slim projection this
// reader depends on has not been provisioned. Refusing loudly is deliberate:
// the pre-v0.21.4 scan path is deleted, and silently degrading every key to
// TTLUnknown would make the archived-balance filter a no-op that looks like
// "nothing was archived" rather than like a misconfiguration.
var errTTLLiveUntilTableMissing = errors.New(
	"clickhouse: stellar.ttl_live_until does not exist — apply deploy/clickhouse/ttl_live_until.sql " +
		"(table + materialized view, then its Step-2 windowed backfill) before running TTL-liveness reads; " +
		"there is no scan fallback")

// TTLKeyHash returns the TTL key hash governing the ledger entry whose
// base64-encoded LedgerKey is keyXDR — i.e. sha256 over the DECODED key
// bytes, which is exactly how stellar-core derives LedgerKeyTtl.keyHash.
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
	// TTLUnknown — no TTL entry was found, or its wire shape was
	// unrecognised. NOT a licence to drop the entry: callers must keep it.
	// Entry types that carry no TTL at all (classic entries) land here.
	TTLUnknown TTLLiveness = iota
	// TTLLive — liveUntilLedgerSeq is at or beyond the reference ledger.
	TTLLive
	// TTLArchived — liveUntilLedgerSeq has lapsed. The entry is no longer
	// part of live ledger state and its last-known value must not be
	// reported as current.
	TTLArchived
)

// ClassifyTTLLiveness resolves, for each base64 LedgerKey in keyXDRs, whether
// the entry is still live as of asOfLedger.
//
// It reads the slim `stellar.ttl_live_until` projection (key_hash →
// live_until, MV-maintained at ingest) — a primary-key lookup whose cost
// scales with the batch, not with the lake. The projection must exist:
// [errTTLLiveUntilTableMissing] otherwise.
//
// Keys with no TTL row come back [TTLUnknown], and the contract is that
// callers KEEP those. Dropping an entry we merely failed to resolve would
// understate supply, which is the same class of error as the phantom balances
// this exists to remove — and a silent over-drop is far harder to notice than
// a residual over-count. Only a positive, parsed, lapsed liveUntilLedgerSeq
// justifies exclusion.
// A whole-network seed resolves tens of thousands of keys (USDC alone carries
// 48,505 seeded holders), far past what one IN list should carry, so the work
// is split into [ttlLivenessBatchSize] chunks.
func ClassifyTTLLiveness(ctx context.Context, conn driver.Conn, keyXDRs []string, asOfLedger uint32) (map[string]TTLLiveness, error) {
	out := make(map[string]TTLLiveness, len(keyXDRs))
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
		if err := classifyTTLLivenessBatch(ctx, conn, keyXDRs[start:end], asOfLedger, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ensureTTLLiveUntilTable probes for the projection and refuses with a
// deploy-pointing error when it is absent. One tiny metadata query per
// ClassifyTTLLiveness call (not per batch), before any work is done.
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

// ttlLivenessBatchQuery renders the per-batch lookup. argMax(live_until,
// version) keeps the LATEST TTL state per key (version = (ledger_seq<<32) |
// intra_ledger_seq, matching the table's ReplacingMergeTree version — the
// GROUP BY makes the read correct over not-yet-merged duplicate versions
// without FINAL).
//
// SETTINGS rationale: the lookup is bounded by construction (≤ batch-size
// primary-key probes over three tiny columns), so these pins are guard rails,
// not load-bearing tuning — carried over from the scan era (2026-07-29,
// measured on r1: unpinned max_threads fanned a read out to 40× its
// single-digit-MiB cost) so that a future layout or planner shift fails THIS
// query loudly instead of starving the shared host.
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

// classifyTTLLivenessBatch resolves one bounded chunk of keys into out.
func classifyTTLLivenessBatch(ctx context.Context, conn driver.Conn, keyXDRs []string, asOfLedger uint32, out map[string]TTLLiveness) error {
	// hash -> the key(s) it governs. Distinct keys cannot collide under
	// sha256, but the same key may legitimately appear twice in the input.
	byHash := make(map[string][]string, len(keyXDRs))
	args := make([]any, 0, len(keyXDRs))
	placeholders := make([]string, 0, len(keyXDRs))
	for _, k := range keyXDRs {
		out[k] = TTLUnknown
		h, err := TTLKeyHash(k)
		if err != nil {
			// An undecodable key cannot be proven archived. Leave it
			// TTLUnknown so the caller keeps it.
			continue
		}
		if _, seen := byHash[h]; !seen {
			placeholders = append(placeholders, "unhex(?)")
			args = append(args, h)
		}
		byHash[h] = append(byHash[h], k)
	}
	if len(placeholders) == 0 {
		return nil
	}

	rows, err := conn.Query(ctx, ttlLivenessBatchQuery(placeholders), args...)
	if err != nil {
		return fmt.Errorf("clickhouse: ClassifyTTLLiveness: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyHash   string
			liveUntil uint32
		)
		if err := rows.Scan(&keyHash, &liveUntil); err != nil {
			return fmt.Errorf("clickhouse: ClassifyTTLLiveness scan: %w", err)
		}
		// The MV's length guards mean a malformed shape is never inserted,
		// so 0 cannot arise from misparsing — but a literal stored 0 still
		// cannot prove anything, and fail-open says UNKNOWN over a guess.
		if liveUntil == 0 {
			continue
		}
		verdict := TTLLive
		if liveUntil < asOfLedger {
			verdict = TTLArchived
		}
		for _, k := range byHash[strings.ToLower(keyHash)] {
			out[k] = verdict
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: ClassifyTTLLiveness stream: %w", err)
	}
	return nil
}
