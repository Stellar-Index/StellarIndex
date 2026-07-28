package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
// liveUntilLedgerSeq therefore starts at byte 40 (0-indexed). The layout is
// asserted rather than assumed — see [ttlLiveUntilExpr].
const (
	ttlEntryLen         = 48
	ttlLiveUntilOffset0 = 40
)

// ttlLiveUntilExpr is the ClickHouse expression yielding a TTL entry's
// liveUntilLedgerSeq. XDR integers are big-endian and reinterpretAsUInt32 is
// little-endian, hence the reverse().
//
// It is guarded on the decoded length: a TTL entry whose wire shape is not
// exactly 48 bytes yields 0, which [ClassifyTTLLiveness] treats as UNKNOWN
// rather than as expired. A protocol change that alters this layout must
// degrade to "cannot prove archived", never to "prove archived".
var ttlLiveUntilExpr = fmt.Sprintf(
	`if(length(base64Decode(entry_xdr)) = %d, reinterpretAsUInt32(reverse(substring(base64Decode(entry_xdr), %d, 4))), 0)`,
	ttlEntryLen, ttlLiveUntilOffset0+1, // ClickHouse substring is 1-indexed
)

// ttlLivenessBatchSize caps how many key hashes ride in one IN list.
const ttlLivenessBatchSize = 5_000

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
// It reads `ledger_entries_current` rather than the raw change log: that table
// is already deduped to one row per key, so this is a bounded lookup instead
// of a scan over every TTL change in history.
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

	q := fmt.Sprintf(`
		SELECT lower(hex(substring(base64Decode(key_xdr), %d, %d))) AS key_hash,
		       max(%s) AS live_until
		FROM stellar.ledger_entries_current
		WHERE entry_type = 'ttl'
		  AND length(base64Decode(key_xdr)) = %d
		  AND substring(base64Decode(key_xdr), %d, %d) IN (%s)
		GROUP BY key_hash`,
		ttlKeyHashOffset+1, ttlKeyHashLen, // 1-indexed
		ttlLiveUntilExpr,
		ttlLedgerKeyLen,
		ttlKeyHashOffset+1, ttlKeyHashLen,
		strings.Join(placeholders, ", "),
	)

	rows, err := conn.Query(ctx, q, args...)
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
		// max() over a guarded expression: 0 means every candidate row had
		// an unrecognised shape, which stays UNKNOWN.
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
