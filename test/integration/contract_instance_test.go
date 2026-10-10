//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The instance backfill reads ledger_entry_changes, a
// ReplacingMergeTree(ingested_at), and a corrected re-ingest leaves the stale
// part beside the fix until a merge. Without FINAL both rows ride one INSERT
// into contract_instance_changes, tie on its DEFAULT now() version, and the
// stale wasm_hash can be the one code-history serves. The stale part is
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
	// The same source key, one hour OLDER by version: the stale row a
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

// contract_instance_changes keyed (contract_hash, ledger_seq,
// change_index), and change_index restarts per TRANSACTION. Two transactions
// writing one contract's instance in the same ledger — both at change_index 0
// — collapsed to one row on merge, so a same-ledger upgrade vanished from the
// code history and the survivor was the last INSERTED, not the ledger-final
// write. This drives the shipped DDL (tier1 + the r1 migration's v2), its MV,
// the backfill INSERT and both indexed reader queries on a real ClickHouse.
//
// Red against the old key: after OPTIMIZE … FINAL only one ledger-L row
// survives, so the count assertions fail and the history loses a version.

const (
	t356Ledger0 = uint32(72_356_001) // deploy: executable h0
	t356Ledger  = uint32(72_356_050) // tx A installs h1, then tx B installs h2
)

func t356Hash(b byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func t356InstanceKeyAndEntry(t *testing.T, cid xdr.ContractId, wasm xdr.Hash) (string, string) {
	t.Helper()
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	inst := xdr.ScContractInstance{Executable: xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm,
	}}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
		},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return k, e
}

func t356CodeKeyAndEntry(t *testing.T, wasm xdr.Hash) (string, string) {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetContractCode(wasm); err != nil {
		t.Fatalf("contract_code key: %v", err)
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: wasm, Code: []byte("\x00asm\x01\x00\x00\x00")},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal code key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal code entry: %v", err)
	}
	return k, e
}

// applyDeployStatements executes every statement of one deploy/clickhouse
// artifact — for the migration, that is its Step-1 CREATEs only; the later
// steps are operator-run comments.
func applyDeployStatements(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, s := range splitSQLStatements(string(raw)) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.80q: %v", name, s, err)
		}
	}
}

func t356CountRows(t *testing.T, ctx context.Context, conn driver.Conn, table, contractHash string, ledger uint32) uint64 {
	t.Helper()
	if err := conn.Exec(ctx, "OPTIMIZE TABLE stellar."+table+" FINAL"); err != nil {
		t.Fatalf("optimize %s: %v", table, err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM stellar."+table+
		" FINAL WHERE contract_hash = ? AND ledger_seq = ?", contractHash, ledger).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestContractInstanceChanges_SameLedgerTransactionsBothSurvive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	applyDeployStatements(t, ctx, conn, "contract_instance_changes_tx_key.sql")
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS stellar.contract_instance_changes_v2_mv")
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS stellar.contract_instance_changes_v2")
	})

	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("t356-two-tx-same-ledger-contract"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("strkey: %v", err)
	}
	h0, h1, h2 := t356Hash(0x10), t356Hash(0x11), t356Hash(0x12)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	_, entry2 := t356InstanceKeyAndEntry(t, cid, h2)
	codeKey, codeEntry := t356CodeKeyAndEntry(t, h2)
	closeTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 4, ChangeType: "created", EntryType: "contract_code", KeyXDR: codeKey, EntryXDR: codeEntry,
		},
		// Tx B is ledger-final (intra 9) and is handed to the writer FIRST,
		// so an insertion-order survivor would be tx A's h1.
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry2,
		},
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// The MVs (canonical and the migration's v2) keep both same-ledger writes.
	for _, table := range []string{chstore.ContractInstanceChangesTable, chstore.ContractInstanceChangesV2Table} {
		if n := t356CountRows(t, ctx, conn, table, contractHash, t356Ledger); n != 2 {
			t.Fatalf("%s via MV: %d rows at ledger %d, want 2 (one per transaction)", table, n, t356Ledger)
		}
	}

	// The backfill INSERT, into the migration's target, re-derives the same.
	if err := conn.Exec(ctx, "TRUNCATE TABLE stellar.contract_instance_changes_v2"); err != nil {
		t.Fatalf("truncate v2: %v", err)
	}
	if err := chstore.BackfillContractInstanceChangesInto(ctx, addr, chstore.ContractInstanceChangesV2Table,
		t356Ledger0, t356Ledger, 1000, t.Logf); err != nil {
		t.Fatalf("backfill v2: %v", err)
	}
	if n := t356CountRows(t, ctx, conn, chstore.ContractInstanceChangesV2Table, contractHash, t356Ledger); n != 2 {
		t.Fatalf("backfill: %d rows at ledger %d, want 2", n, t356Ledger)
	}
	var ils []uint32
	var txs []string
	q, err := conn.Query(ctx, `SELECT intra_ledger_seq, tx_hash FROM stellar.contract_instance_changes_v2 FINAL
		WHERE contract_hash = ? AND ledger_seq = ? ORDER BY intra_ledger_seq`, contractHash, t356Ledger)
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	for q.Next() {
		var s uint32
		var h string
		if err := q.Scan(&s, &h); err != nil {
			t.Fatalf("scan v2: %v", err)
		}
		ils, txs = append(ils, s), append(txs, h)
	}
	_ = q.Close()
	if len(ils) != 2 || ils[0] != 4 || ils[1] != 9 || txs[0] != "t356-tx-a" || txs[1] != "t356-tx-b" {
		t.Fatalf("backfilled (intra_ledger_seq, tx_hash) = %v %v, want [4 9] [t356-tx-a t356-tx-b]", ils, txs)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	hist, err := er.ContractCodeHistory(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []string{hex.EncodeToString(h0[:]), hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:])}
	if len(hist) != len(want) {
		t.Fatalf("code history = %+v, want hashes %v (the same-ledger h1 must not vanish)", hist, want)
	}
	for i, v := range hist {
		if v.WasmHash != want[i] {
			t.Fatalf("code history[%d] = %s, want %s (ledger-final h2 last): %+v", i, v.WasmHash, want[i], hist)
		}
	}

	info, err := er.ContractWasm(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractWasm: %v", err)
	}
	if info.WasmHash != want[2] {
		t.Fatalf("ContractWasm hash = %s, want the ledger-final %s", info.WasmHash, want[2])
	}
}

// The genesis watermark is written by Set...Watermark (ordering-guarded) and
// consumed by ContractCodeHistory: an index miss skips the ledger_entry_changes
// scan only under a covering mark. Runs the real INSERT and SELECT.
func TestContractInstanceGenesisWatermark_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	const name = chstore.ContractInstanceChangesTable
	clearMark := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		if err := conn.Exec(cctx, `ALTER TABLE stellar.entry_history_watermark DELETE WHERE name = ? SETTINGS mutations_sync = 2`, name); err != nil {
			t.Fatalf("clear mark: %v", err)
		}
	}
	clearMark()
	t.Cleanup(clearMark)

	const ledger = uint32(72_720_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("inv2188-watermark-roundtrip-c001"))
	cid := xdr.ContractId(cidRaw)
	strk, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	instKey, entry := t356InstanceKeyAndEntry(t, cid, t356Hash(0x31))
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC), TxHash: "inv2188-tx",
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// Leave the index non-empty (so it is "available") but without this
	// contract: an index miss the writer's mark alone can authorise.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ? SETTINGS mutations_sync = 2`,
		hex.EncodeToString(cidRaw[:])); err != nil {
		t.Fatalf("clear MV row: %v", err)
	}
	other, h33 := t356Hash(0x32), t356Hash(0x33)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contract_instance_changes
		(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)
		VALUES (?, 1, 'inv2188-other', 0, 1, now(), 0, ?)`, hex.EncodeToString(h33[:]), hex.EncodeToString(other[:])); err != nil {
		t.Fatalf("seed index row: %v", err)
	}

	history := func() int {
		er, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = er.Close() }()
		h, err := er.ContractCodeHistory(ctx, strk)
		if err != nil {
			t.Fatalf("ContractCodeHistory: %v", err)
		}
		return len(h)
	}
	if n := history(); n != 1 {
		t.Fatalf("no mark: history = %d versions, want 1 (the scan must run)", n)
	}
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 500); err != nil {
		t.Fatal(err)
	}
	if n := history(); n != 0 {
		t.Fatalf("covering mark: history = %d versions, want 0 (scan skipped)", n)
	}
	// A later, lower mark must not replace the higher one.
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 300); err != nil {
		t.Fatal(err)
	}
	var rows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.entry_history_watermark WHERE name = ?`, name).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("mark rows = %d (%v), want 1: a lower thru must record nothing", rows, err)
	}
}

// The mark is gated on the view's identity and age from system.tables; run
// that lookup for real against a table with a view and one without.
func TestContractInstanceGenesisWatermark_StartState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	st, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil {
		t.Fatalf("ReadInstanceBackfillStart: %v", err)
	}
	if !st.MVExists || st.MVUUID == "" || st.MVModified.IsZero() || st.MVAge < 0 {
		t.Fatalf("start state = %+v, want the canonical view's identity and age", st)
	}
	again, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil || again.MVUUID != st.MVUUID || !again.MVModified.Equal(st.MVModified) {
		t.Fatalf("re-read = %+v (%v), want the same view identity as %+v", again, err, st)
	}
	v2, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesV2Table)
	if err != nil || v2.MVExists {
		t.Fatalf("v2 start state = %+v (%v), want no view", v2, err)
	}
}

// instanceFixture returns a deterministic C-strkey and the base64 LedgerKey of
// its persistent ScvLedgerKeyContractInstance entry — the key the explorer's
// contract routes classify.
func instanceFixture(t *testing.T, tag string) (contractStrkey, instanceKeyXDR string) {
	t.Helper()
	sum := sha256.Sum256([]byte("contract-instance-state-it-" + tag))
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, sum[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cid := xdr.ContractId(sum)
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	instanceKeyXDR, err = xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	return contractStrkey, instanceKeyXDR
}

// TestContractInstanceState_TTLRowAndAbsence runs the explorer's instance read
// against a real ClickHouse: a TTL row for the instance key yields Known plus
// the newest live_until (so an archived instance is judged archived), and a
// contract with no lake evidence at all yields Known=false without error.
func TestContractInstanceState_TTLRowAndAbsence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	archivedContract, archivedKey := instanceFixture(t, "archived")
	neverDeployed, _ := instanceFixture(t, "never-deployed")
	lapsedAt := ttlAsOf - 1_000_000

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archivedKey, 72_100_001, 1, lapsedAt-5, 48),
		ttlChangeRow(archivedKey, 72_100_002, 1, lapsedAt, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = er.Close() }()

	st, err := er.ContractInstanceState(ctx, archivedContract)
	if err != nil {
		t.Fatalf("ContractInstanceState(archived): %v", err)
	}
	if !st.Known || st.LiveUntil != lapsedAt {
		t.Fatalf("archived instance state = %+v, want Known with LiveUntil %d", st, lapsedAt)
	}
	if v := chstore.TTLVerdictAt(st.LiveUntil, ttlAsOf); v != chstore.TTLArchived {
		t.Errorf("verdict = %v, want TTLArchived", v)
	}

	st, err = er.ContractInstanceState(ctx, neverDeployed)
	if err != nil {
		t.Fatalf("ContractInstanceState(never deployed): %v", err)
	}
	if st.Known || st.LiveUntil != 0 {
		t.Errorf("never-deployed state = %+v, want zero", st)
	}
}
