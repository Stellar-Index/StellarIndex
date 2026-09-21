package clickhouse

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// seedIntraLedgerSeq is the intra_ledger_seq a snapshot/seed backfill row
// stamps: math.MaxUint32, the top of the per-ledger position space. A snapshot
// 'state' row is the authoritative reconstructed FINAL state of its entry as of
// LastModifiedLedgerSeq (ADR-0034), so it must sit at the END of that ledger's
// intra-ledger order — a live per-ledger change (a much smaller position) can
// never overwrite it in ledger_entries_current's version comparison, while a
// re-snapshot (equal sentinel) stays corrective. The live extract counter
// cannot reach this value (it would require ~4.3e9 entry-changes in one
// ledger). Mirrors timescale.SeedIntraLedgerSeq (migration 0111, C2-6); kept a
// local const so the lake layer takes no dependency on the served-tier package.
const seedIntraLedgerSeq = uint32(0xFFFFFFFF) // math.MaxUint32

// SnapshotEntryRow builds a ledger_entry_changes row from a live snapshot entry
// (Post-only), populating owner/asset/balance with the SAME helpers the indexer
// uses for its own writes — so account-state + asset-holder + supply reads work
// identically against backfilled rows. Keyed on the entry's real LedgerKey +
// LastModifiedLedgerSeq (so the readers' key_xdr lookups match and newest-wins
// ordering stays correct). change_type="state", op_index=-1, intra_ledger_seq =
// seedIntraLedgerSeq (authoritative final state for its ledger). ok=false on a
// marshal error (skip the one entry, never abort the backfill).
func SnapshotEntryRow(post *xdr.LedgerEntry, closeTime time.Time) (LedgerEntryChangeRow, bool) {
	key, err := post.LedgerKey()
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	keyB64, err := xdr.MarshalBase64(key)
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	entryB64, err := xdr.MarshalBase64(post)
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	owner, asset := ownerAndAsset(key)
	return LedgerEntryChangeRow{
		LedgerSeq: uint32(post.LastModifiedLedgerSeq), //nolint:gosec // ledger seq fits uint32
		CloseTime: closeTime,
		OpIndex:   -1,
		// ChangeIndex MUST be per-key unique within a ledger_seq: the
		// table is ReplacingMergeTree ORDER BY (ledger_seq, tx_hash,
		// op_index, change_index), and snapshot rows all share
		// tx_hash="" + op_index=-1. With ChangeIndex=0 every snapshot
		// entry modified in the same ledger collapsed to ONE arbitrary
		// survivor at merge time — the 2026-07-03 site audit measured
		// >55% of the 48M-entry Phase-C snapshot already destroyed
		// (blast radius: account-state, trustline, supply, and wasm
		// readers). crc32(key) is deterministic, so re-runs stay
		// idempotent per key instead of duplicating — but a 32-bit
		// checksum still collides between two DIFFERENT keys sharing a
		// ledger_seq often enough in a multi-million-row backfill to
		// reproduce the same collapse. This is only the BASE value:
		// InsertEntryChanges resolves any residual collision across the
		// whole write batch before it reaches ClickHouse.
		ChangeIndex: crc32.ChecksumIEEE([]byte(keyB64)),
		ChangeType:  "state",
		EntryType:   entryTypeName(post.Data.Type),
		KeyXDR:      keyB64,
		EntryXDR:    entryB64,
		AccountID:   owner,
		Asset:       asset,
		Balance:     entryBalance(*post),
		// A reconstructed snapshot is the ledger's final state for this key —
		// it must win any same-ledger live change in ledger_entries_current's
		// version comparison (C2-4c), so it stamps the top-of-space sentinel.
		IntraLedgerSeq: seedIntraLedgerSeq,
	}, true
}

// entryBackfillChunk bounds each INSERT batch so a large backfill streams in
// fixed-memory chunks rather than one giant batch.
const entryBackfillChunk = 20_000

// InsertEntryChanges batch-inserts rows directly into
// stellar.ledger_entry_changes, bypassing the Sink's per-ledger commit-marker
// flow: it writes NO stellar.ledgers row, so it never advances the completeness
// watermark (which keys off a ledgers row meaning "this ledger is fully
// durable"). That makes it the correct path for backfilling historical /
// snapshot entries — e.g. the state-snapshot contract-code + instance backfill
// (DATA-TRUTH-PLAN G1) — into the append-log the WASM + account-state readers
// query (and which the ledger_entries_current MV folds into current state).
// Idempotent under the table's ReplacingMergeTree. Returns the number written.
// throttle is a pause between chunks — keep it non-zero for large backfills so
// the insert + the ledger_entries_current MV don't spike the live serving CH.
//
// Resolves any ChangeIndex collision across the whole batch before writing
// (resolveChangeIndexCollisions) — SnapshotEntryRow's crc32(key) base value is
// only collision-free in the common case; this is the chokepoint that makes
// it collision-free in fact for every row that reaches ClickHouse.
func InsertEntryChanges(ctx context.Context, addr string, rows []LedgerEntryChangeRow, throttle time.Duration) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if err := resolveChangeIndexCollisions(rows); err != nil {
		return 0, err
	}
	// Ops-batch identity from the environment (2026-08-28 r1 incident;
	// see ops_auth.go) — CH `default` user when unset.
	auth, err := opsAuth()
	if err != nil {
		return 0, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: auth,
		// A finite ceiling — this is the cheap append path, not a heavy read.
		Settings:        clickhouse.Settings{"max_execution_time": 600},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return 0, fmt.Errorf("clickhouse: open %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Ping(ctx); err != nil {
		return 0, fmt.Errorf("clickhouse: ping %s: %w", addr, err)
	}

	written := 0
	for start := 0; start < len(rows); start += entryBackfillChunk {
		end := start + entryBackfillChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := insertEntryChunk(ctx, conn, rows[start:end]); err != nil {
			return written, err
		}
		written += end - start
		if throttle > 0 && end < len(rows) {
			select {
			case <-time.After(throttle):
			case <-ctx.Done():
				return written, ctx.Err()
			}
		}
	}
	return written, nil
}

// insertEntryChunk sends one prepared batch of entry-change rows.
func insertEntryChunk(ctx context.Context, conn clickhouse.Conn, chunk []LedgerEntryChangeRow) error {
	b, err := conn.PrepareBatch(ctx,
		"INSERT INTO stellar.ledger_entry_changes (ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, account_id, asset, balance, intra_ledger_seq)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare entry-changes batch: %w", err)
	}
	for _, r := range chunk {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex,
			r.ChangeType, r.EntryType, r.KeyXDR, r.EntryXDR, r.AccountID, r.Asset, r.Balance, r.IntraLedgerSeq); err != nil {
			return fmt.Errorf("clickhouse: append entry-change: %w", err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("clickhouse: send entry-changes batch: %w", err)
	}
	return nil
}

// maxChangeIndexProbeAttempts bounds resolveChangeIndexCollisions' re-hash
// loop. A real collision resolves within a handful of probes; this is a
// fail-closed ceiling, not a realistic count.
const maxChangeIndexProbeAttempts = 1 << 20

// resolveChangeIndexCollisions makes ChangeIndex unique within every
// (ledger_seq, tx_hash, op_index) group in rows — the exact tuple prefix the
// table's ReplacingMergeTree ORDER BY shares change_index with. A 32-bit
// crc32(key) base value collides between two DIFFERENT keys often enough in a
// multi-million-row backfill (2026-07-03 site audit, SnapshotEntryRow's doc
// comment) to silently drop one of them at merge time.
//
// Colliding rows within a group are walked in KeyXDR-sorted order — never
// arrival order — and re-hashed with an incrementing salt until free. Sorting
// by key means the outcome depends only on the SET of keys sharing the group,
// so re-running the same rows in any order (an interrupted backfill resumed)
// resolves to the same final indices and stays idempotent rather than
// duplicating.
func resolveChangeIndexCollisions(rows []LedgerEntryChangeRow) error {
	type groupKey struct {
		ledgerSeq uint32
		txHash    string
		opIndex   int32
	}
	groups := make(map[groupKey][]int, len(rows))
	for i := range rows {
		k := groupKey{rows[i].LedgerSeq, rows[i].TxHash, rows[i].OpIndex}
		groups[k] = append(groups[k], i)
	}
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		if err := resolveGroupCollisions(rows, idxs); err != nil {
			return err
		}
	}
	return nil
}

// resolveGroupCollisions assigns each row in idxs (all sharing one
// ledger_seq/tx_hash/op_index group) a ChangeIndex distinct from every other
// row in the group, mutating rows in place.
func resolveGroupCollisions(rows []LedgerEntryChangeRow, idxs []int) error {
	sort.Slice(idxs, func(a, b int) bool {
		return rows[idxs[a]].KeyXDR < rows[idxs[b]].KeyXDR
	})
	used := make(map[uint32]bool, len(idxs))
	for _, i := range idxs {
		ci := rows[i].ChangeIndex
		for attempt := uint32(0); used[ci]; attempt++ {
			if attempt >= maxChangeIndexProbeAttempts {
				return fmt.Errorf("clickhouse: no unique change_index found for key %s (ledger_seq %d) after %d probes",
					rows[i].KeyXDR, rows[i].LedgerSeq, attempt)
			}
			ci = changeIndexProbe(rows[i].KeyXDR, attempt)
		}
		used[ci] = true
		rows[i].ChangeIndex = ci
	}
	return nil
}

// changeIndexProbe re-hashes key with an incrementing salt — the fallback
// discriminator when the base crc32(key) collides with another key in the
// same group.
func changeIndexProbe(key string, attempt uint32) uint32 {
	buf := make([]byte, len(key)+4)
	copy(buf, key)
	binary.BigEndian.PutUint32(buf[len(key):], attempt+1)
	return crc32.ChecksumIEEE(buf)
}
