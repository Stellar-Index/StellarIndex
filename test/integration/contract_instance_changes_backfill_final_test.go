//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The instance backfill reads ledger_entry_changes, a
// ReplacingMergeTree(ingested_at), and a corrected re-ingest leaves the stale
// part beside the fix until a merge. Without FINAL both rows ride one INSERT
// into contract_instance_changes, tie on its DEFAULT now() version, and the
// pre-fix wasm_hash can be the one code-history serves. The stale part is
// written LAST so an unmerged read hands it to the target last.
func TestContractInstanceBackfill_ReadsCorrectedSourceRowOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const ledger = uint32(72_719_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("gh719-instance-backfill-final-c1"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	stale, corrected := t356Hash(0x20), t356Hash(0x21)
	instKey, correctedEntry := t356InstanceKeyAndEntry(t, cid, corrected)
	_, staleEntry := t356InstanceKeyAndEntry(t, cid, stale)

	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), TxHash: "gh719-upgrade",
		OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
		KeyXDR: instKey, EntryXDR: correctedEntry,
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// The same source key, one hour OLDER by version: the pre-fix row a
	// merge would discard, still sitting in its own part.
	if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
		SELECT * REPLACE (? AS entry_xdr, ingested_at - INTERVAL 1 HOUR AS ingested_at)
		FROM stellar.ledger_entry_changes WHERE ledger_seq = ? AND tx_hash = ?`,
		staleEntry, ledger, row.TxHash); err != nil {
		t.Fatalf("insert stale part: %v", err)
	}
	var parts uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.ledger_entry_changes
		WHERE ledger_seq = ? AND tx_hash = ?`, ledger, row.TxHash).Scan(&parts); err != nil || parts != 2 {
		t.Fatalf("unmerged source rows = %d (%v), want 2 (the fix and its stale twin)", parts, err)
	}

	// Drop what the MV wrote at insert time, so the target holds only the
	// backfill's output.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE
		WHERE contract_hash = ? SETTINGS mutations_sync = 2`, contractHash); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := chstore.BackfillContractInstanceChanges(ctx, addr, ledger, ledger, 10, t.Logf); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var got []string
	q, err := conn.Query(ctx, `SELECT wasm_hash FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ? AND ledger_seq = ?`, contractHash, ledger)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	for q.Next() {
		var h string
		if err := q.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, h)
	}
	_ = q.Close()
	want := hex.EncodeToString(corrected[:])
	if len(got) != 1 || got[0] != want {
		t.Fatalf("backfilled wasm_hash = %v, want exactly the corrected [%s] (stale %s must not survive)",
			got, want, hex.EncodeToString(stale[:]))
	}
}
