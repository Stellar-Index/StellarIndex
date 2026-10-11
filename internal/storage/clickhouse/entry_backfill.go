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

// seedIntraLedgerSeq is the intra_ledger_seq a snapshot/seed backfill row stamps: math.MaxUint32,
// the top of the per-ledger position space. A snapshot 'state' row is the authoritative final
// state as of LastModifiedLedgerSeq (ADR-0034), so it sits at the END of that ledger's order: a
// live change can never overwrite it in ledger_entries_current's version comparison, while a
// re-snapshot (equal sentinel) stays corrective. Mirrors timescale.SeedIntraLedgerSeq; a local
// const so the lake layer does not depend on the served-tier package.
const seedIntraLedgerSeq = uint32(0xFFFFFFFF) // math.MaxUint32

// SnapshotEntryRow builds a ledger_entry_changes row from a live snapshot entry (Post-only),
// populating owner/asset/balance with the SAME helpers the indexer uses, so account-state,
// asset-holder and supply reads treat backfilled rows identically. Keyed on the entry's real
// LedgerKey + LastModifiedLedgerSeq so key_xdr lookups match and newest-wins ordering holds.
// ok=false on a marshal error (skip the one entry, never abort the backfill).
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
		// ChangeIndex MUST be per-key unique within a ledger_seq: snapshot rows share tx_hash=""
		// and op_index=-1, so a constant would collapse every same-ledger entry to ONE survivor
		// under the ReplacingMergeTree ORDER BY. crc32(key) is deterministic (idempotent re-runs)
		// but only the BASE value: InsertEntryChanges resolves residual collisions in the batch.
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
		// version comparison, so it stamps the top-of-space sentinel.
		IntraLedgerSeq: seedIntraLedgerSeq,
	}, true
}

// isSnapshotSeedRow reports whether a ledger_entry_changes row has
// SnapshotEntryRow's shape. Live rows never match: a fee-phase `state` row
// carries its tx hash, and an eviction (also tx-less) is `removed`.
func isSnapshotSeedRow(txHash string, opIndex int32, changeType string) bool {
	return txHash == "" && opIndex == -1 && changeType == "state"
}

// entryBackfillChunk bounds each INSERT batch so a large backfill streams in
// fixed-memory chunks rather than one giant batch.
const entryBackfillChunk = 20_000

// InsertEntryChanges batch-inserts rows directly into stellar.ledger_entry_changes, bypassing the
// Sink's per-ledger commit-marker flow: it writes NO stellar.ledgers row, so it never advances
// the completeness watermark, which is correct for snapshot entries. Idempotent under the
// ReplacingMergeTree. Returns the number written. throttle pauses between chunks; keep it
// non-zero for large backfills so the insert and the ledger_entries_current MV don't spike the
// live serving CH.
//
// Resolves ChangeIndex collisions across the whole batch first (resolveChangeIndexCollisions),
// since SnapshotEntryRow's crc32(key) base is only collision-free in the common case.
func InsertEntryChanges(ctx context.Context, addr string, rows []LedgerEntryChangeRow, throttle time.Duration) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if err := resolveChangeIndexCollisions(rows); err != nil {
		return 0, err
	}
	// Identity from the environment; see ops_auth.go.
	auth, err := chAuth()
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
// (ledger_seq, tx_hash, op_index) group in rows, the tuple prefix the ReplacingMergeTree ORDER BY
// shares change_index with. A 32-bit crc32(key) collides often enough in a multi-million-row
// backfill to silently drop a row at merge time. Colliding rows are walked in KeyXDR-sorted
// order, never arrival order, and re-hashed with an incrementing salt until free, so the outcome
// depends only on the SET of keys and a resumed backfill stays idempotent.
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
