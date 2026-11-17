//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingDeployFixture is the real BLNT factory `deploy` event.
const lendingDeployFixture = "../fixtures/blnt/985d7eda4b610f28103edcb28faa52408169f455ac9a880c4a39f58ecc0c2135/deploy_factory_65134362_db1ff1407551_0_CAKLFW.json"

func runLendingWrite(t *testing.T, addr string, from, to uint32) error {
	t.Helper()
	return chops.Run([]string{
		"ch-entry-history", "-lending-only", "-ch-addr", addr, "-write", "-lock-file", filepath.Join(t.TempDir(), "lending.lock"),
		"-from", strconv.FormatUint(uint64(from), 10), "-to", strconv.FormatUint(uint64(to), 10),
	})
}

// seedLendingDeploy writes the real deploy event at ledger seq, plus the same event announcing the yield pool so the
// registry holds every known pool, and returns the pool the real event announces.
func seedLendingDeploy(t *testing.T, ctx context.Context, addr string, seq uint32) string {
	t.Helper()
	raw, err := os.ReadFile(lendingDeployFixture)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		TxHash string   `json:"tx_hash"`
		Topics []string `json:"topics"`
		Value  string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var body xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(f.Value, &body); err != nil || body.Address == nil || body.Address.ContractId == nil {
		t.Fatalf("deploy body %q is not a contract address: %v", f.Value, err)
	}
	pool := strkey.MustEncode(strkey.VersionByteContract, body.Address.ContractId[:])
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer func() { _ = sink.Close(ctx) }()
	closeTime := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	yieldID := xdr.ContractId(strkey.MustDecode(strkey.VersionByteContract, blnt.MainnetYieldBlentPool))
	yieldBody, err := xdr.MarshalBase64(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &yieldID},
	})
	if err != nil {
		t.Fatal(err)
	}
	deploy := func(idx uint32, body string) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: seq, CloseTime: closeTime, TxHash: f.TxHash, EventIndex: idx,
			ContractID: blnt.MainnetPoolFactory, EventType: "contract", TopicCount: 1, Topic0Sym: "deploy",
			TopicsXDR: f.Topics, DataXDR: body, InSuccessfulCall: 1,
		}
	}
	// The sink always writes the ledger row; it matches entryHistorySeedLedgers' so the duplicate collapses.
	ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
		LedgerSeq: seq, CloseTime: closeTime,
		LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
		TxCount: 1, TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
	}, Events: []chstore.ContractEventRow{deploy(0, f.Value), deploy(1, yieldBody)}}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add deploy: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush deploy: %v", err)
	}
	return pool
}

// lendingKeyB64 is the key_xdr of pool's persistent storage key k.
func lendingKeyB64(t *testing.T, pool xdr.ContractId, k xdr.ScVal) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pool},
		Key:      k, Durability: xdr.ContractDataDurabilityPersistent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// lendingHistory lists key's rows under FINAL as "hour ledger entry", checking each is pool's under protocol blnt.
func lendingHistory(t *testing.T, ctx context.Context, conn driver.Conn, pool, key string) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT contract_id, protocol, key_xdr, toString(hour), ledger_seq, entry_xdr
		FROM stellar.lending_state_hourly FINAL WHERE key_xdr = ? ORDER BY hour`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var c, p, k, h, e string
		var seq uint32
		if err := rows.Scan(&c, &p, &k, &h, &seq, &e); err != nil {
			t.Fatal(err)
		}
		if c != pool || p != chstore.LendingBLNT || k != key {
			t.Errorf("row %s/%s/%s: want %s/%s", c, p, k, pool, chstore.LendingBLNT)
		}
		got = append(got, h+" "+strconv.FormatUint(uint64(seq), 10)+" "+e)
	}
	return got
}

// The hour's last change wins across windows that split the hour, a re-run changes nothing, pre-images and
// position keys are never captured, a hole is refused, and the proof passes until an hour's row is missing or
// tampered, or a registered pool with live reserves has none in the table.
func TestChEntryHistoryLending_HourlyLastChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	const base = lendingProofBase
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	asset[0] = 0x5a
	key := resDataKeyB64(t, poolID, asset)
	uncaptured := key[:60] + "x" // shares the pool's key head, matches no prefix
	configSym := xdr.ScSymbol("ResConfig")
	assetAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &asset}
	configVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &configSym}, {Type: xdr.ScValTypeScvAddress, Address: &assetAddr}}
	configKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &configVec})
	instanceKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})

	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2, base + 3}, 1, false)
	at := func(h, m int) time.Time { return time.Date(2027, 2, 10, h, m, 0, 0, time.UTC) }
	change := func(seq uint32, ts time.Time, ct, k, e string, intra uint32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: ts, TxHash: "1e" + strconv.FormatUint(uint64(seq), 10),
			ChangeType: ct, EntryType: "contract_data", KeyXDR: k, EntryXDR: e, IntraLedgerSeq: intra,
		}
	}
	insertEntryChanges(t, ctx,
		change(base, at(10, 10), "created", key, "A1", 1),
		change(base, at(10, 10), "created", uncaptured, "P1", 2),
		change(base, at(10, 10), "created", configKey, "C1", 3),
		change(base, at(10, 10), "created", instanceKey, "I1", 4),
		change(base+1, at(10, 50), "updated", key, "A2", 1),
		change(base+1, at(10, 50), "state", key, "PRE", 5),
		change(base+2, at(11, 5), "updated", key, "A3", 1),
		change(base+3, at(11, 30), "removed", key, "", 1),
	)

	if err := runLendingWrite(t, addr, base, base); err != nil {
		t.Fatalf("window [%d,%d]: %v", base, base, err)
	}
	if err := runLendingWrite(t, addr, base+1, base+3); err != nil {
		t.Fatalf("window [%d,%d]: %v", base+1, base+3, err)
	}
	if err := runLendingWrite(t, addr, base, base+3); err != nil {
		t.Fatalf("re-run [%d,%d]: %v", base, base+3, err)
	}
	if wm, err := chstore.LendingStateWatermark(ctx, addr); err != nil || wm != base+3 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+3)
	}

	got := lendingHistory(t, ctx, conn, pool, key)
	want := []string{"2027-02-10 10:00:00 " + strconv.FormatUint(uint64(base+1), 10) + " A2", "2027-02-10 11:00:00 " + strconv.FormatUint(uint64(base+3), 10) + " "}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("history under FINAL = %q, want %q", got, want)
	}

	if err := chstore.SetLendingStateWatermark(ctx, addr, base+4, base+6, 0); !errors.Is(err, chstore.ErrEntryHistoryHole) {
		t.Fatalf("advance over an absent window: err = %v, want ErrEntryHistoryHole", err)
	}

	proveLendingCases(t, ctx, conn, addr, pool, key, func(seq uint32, ts time.Time, k, e string) {
		insertEntryChanges(t, ctx, change(seq, ts, "created", k, e, 1))
	})
}

// lendingProofBase is TestChEntryHistoryLending_HourlyLastChange's first ledger.
const lendingProofBase = uint32(171_330_000)

// proveLendingCases runs -prove over the table TestChEntryHistoryLending_HourlyLastChange derived through base+3;
// create inserts one created change.
func proveLendingCases(t *testing.T, ctx context.Context, conn driver.Conn, addr, pool, key string,
	create func(seq uint32, ts time.Time, key, entry string),
) {
	t.Helper()
	const base = lendingProofBase
	at := func(h, m int) time.Time { return time.Date(2027, 2, 10, h, m, 0, 0, time.UTC) }
	prove := []string{"ch-entry-history", "-lending-only", "-prove", "-ch-addr", addr}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof on a correct table: %v", err)
	}
	// A key changed above the watermark is checked against its entry changes folded up to the watermark.
	create(base+4, at(12, 10), key, "A5")
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof with a key changed above the watermark: %v", err)
	}
	// A missing hour that is not a key's latest row fails the per-(key, hour) count; re-deriving restores it.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE key_xdr = ? AND hour = toDateTime('2027-02-10 10:00:00', 'UTC')
		SETTINGS mutations_sync = 2`, key); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), "row counts disagree") {
		t.Fatalf("proof with a missing middle hour: err = %v, want a row-count mismatch", err)
	}
	if err := runLendingWrite(t, addr, base, base+3); err != nil {
		t.Fatalf("re-derive [%d,%d]: %v", base, base+3, err)
	}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof after re-deriving the missing hour: %v", err)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_state_hourly
		(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2027-02-10 12:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 12:00:00', 'UTC'), 'TAMPER')`,
		pool, key, base+3); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err == nil {
		t.Fatal("proof passed over a tampered history row")
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE entry_xdr = 'TAMPER' SETTINGS mutations_sync = 2`); err != nil {
		t.Fatal(err)
	}
	// A wrong row at or below the watermark is still compared when a later hour holds a row past it.
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_state_hourly
		(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2027-02-10 11:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 11:30:00', 'UTC'), 'TAMPER'),
		       (?, 'blnt', ?, toDateTime('2027-02-10 12:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 12:10:00', 'UTC'), 'A5')`,
		pool, key, base+3, pool, key, base+4); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), "disagree with the lake") {
		t.Fatalf("proof with a tampered row below a row past the watermark: err = %v, want a state mismatch", err)
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE entry_xdr = 'TAMPER' OR ledger_seq > ?
		SETTINGS mutations_sync = 2`, base+3); err != nil {
		t.Fatal(err)
	}
	if err := runLendingWrite(t, addr, base, base+3); err != nil {
		t.Fatalf("re-derive [%d,%d]: %v", base, base+3, err)
	}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof after re-deriving the tampered hour: %v", err)
	}
	// Rows written past the watermark by a window it refused, one replacing the watermark's own hour: not a mismatch.
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_state_hourly
		(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2027-02-10 11:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 11:59:00', 'UTC'), 'LATE'),
		       (?, 'blnt', ?, toDateTime('2027-02-10 12:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 12:10:00', 'UTC'), 'A5')`,
		pool, key, base+4, pool, key, base+4); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof with history rows past the watermark: %v", err)
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE ledger_seq > ? SETTINGS mutations_sync = 2`, base+3); err != nil {
		t.Fatal(err)
	}
	// A merge may already have dropped the hour's row the late one replaced.
	if err := runLendingWrite(t, addr, base, base+3); err != nil {
		t.Fatalf("re-derive [%d,%d]: %v", base, base+3, err)
	}
	// The yield pool gains a live reserve the table has never seen: the proof names it.
	yieldID := xdr.ContractId(strkey.MustDecode(strkey.VersionByteContract, blnt.MainnetYieldBlentPool))
	var asset xdr.ContractId
	asset[0] = 0x5a
	create(base+4, at(12, 20), resDataKeyB64(t, yieldID, asset), "Y1")
	if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), blnt.MainnetYieldBlentPool+" has no ResData") {
		t.Fatalf("proof with a live reserve absent from the table: err = %v, want the yield pool named", err)
	}
}

// Two transactions changing one key in its hour's last ledger: with intra-ledger order the later wins; without it the
// window is refused and the watermark stays.
func TestChEntryHistoryLending_SameLedgerOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_340_000)
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	asset[0] = 0x5b
	key := resDataKeyB64(t, poolID, asset)
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1}, 1, false)
	change := func(seq uint32, h int, tx, e string, intra uint32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 2, 10, h, 10, 0, 0, time.UTC), TxHash: tx,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: key, EntryXDR: e, IntraLedgerSeq: intra,
		}
	}
	// tx_hash order would pick "bb"; apply order picks "aa".
	insertEntryChanges(t, ctx,
		change(base, 13, "aa", "LATE", 2), change(base, 13, "bb", "EARLY", 1),
		change(base+1, 14, "aa", "X", 0), change(base+1, 14, "bb", "Y", 0),
	)
	if err := runLendingWrite(t, addr, base, base); err != nil {
		t.Fatalf("ordered window: %v", err)
	}
	if got := lendingHistory(t, ctx, conn, pool, key); len(got) != 1 || got[0] != "2027-02-10 13:00:00 "+strconv.FormatUint(uint64(base), 10)+" LATE" {
		t.Fatalf("ordered history = %q, want the later transaction's entry", got)
	}
	err := runLendingWrite(t, addr, base+1, base+1)
	if err == nil || !strings.Contains(err.Error(), "1 keys have several transactions without intra-ledger order") {
		t.Fatalf("unordered window: err = %v, want a refusal naming the count", err)
	}
	if wm, err := chstore.LendingStateWatermark(ctx, addr); err != nil || wm != base {
		t.Fatalf("watermark = %d (%v), want it held at %d", wm, err, base)
	}
	if got := lendingHistory(t, ctx, conn, pool, key); len(got) != 1 {
		t.Fatalf("history after the refused window = %q, want only the ordered hour", got)
	}
}

// A re-derived ledger keeps its superseded rows until a merge: rows with no intra-ledger order must not refuse the
// window, and a superseded row numbered by an older walk must not outrank the current last change.
func TestChEntryHistoryLending_SupersededRowsAreIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_350_000)
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	asset[0] = 0x5c
	key := resDataKeyB64(t, poolID, asset)
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1}, 1, false)
	closeAt := func(h int) time.Time { return time.Date(2027, 2, 11, h, 10, 0, 0, time.UTC) }
	stale := func(seq uint32, h int, tx, e string, intra uint32) {
		t.Helper()
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
			(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, ingested_at, intra_ledger_seq)
			VALUES (?, ?, ?, 0, 0, 'updated', 'contract_data', ?, ?, '2026-01-01 00:00:00', ?)`,
			seq, closeAt(h), tx, key, e, intra); err != nil {
			t.Fatalf("insert superseded row: %v", err)
		}
	}
	fresh := func(seq uint32, h int, tx, e string, intra uint32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: closeAt(h), TxHash: tx,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: key, EntryXDR: e, IntraLedgerSeq: intra,
		}
	}
	stale(base, 13, "aa", "A-UNORDERED", 0)
	stale(base, 13, "bb", "B-UNORDERED", 0)
	stale(base+1, 14, "aa", "A-OLD-WALK", 9)
	insertEntryChanges(t, ctx,
		fresh(base, 13, "aa", "A", 1), fresh(base, 13, "bb", "B", 2),
		fresh(base+1, 14, "aa", "A2", 1), fresh(base+1, 14, "bb", "B2", 2),
	)
	if err := runLendingWrite(t, addr, base, base+1); err != nil {
		t.Fatalf("window holding superseded rows: %v", err)
	}
	want := []string{
		"2027-02-11 13:00:00 " + strconv.FormatUint(uint64(base), 10) + " B",
		"2027-02-11 14:00:00 " + strconv.FormatUint(uint64(base+1), 10) + " B2",
	}
	if got := lendingHistory(t, ctx, conn, pool, key); !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
}

// A lake re-derive that renumbers a ledger's intra order can give the hour's true last change a lower intra than the
// stale row already in the table. On a table converged by lending_state.sql the replayed row still replaces it.
func TestChEntryHistoryLending_ReplayDisplacesRenumberedRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{
		`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`,
		// The live host's table before lending_state.sql converged its version.
		`ALTER TABLE stellar.lending_state_hourly MODIFY COLUMN version UInt64 MATERIALIZED bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq`,
	} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_360_000)
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	asset[0] = 0x5d
	key := resDataKeyB64(t, poolID, asset)
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base}, 1, false)
	closeAt := time.Date(2027, 2, 12, 9, 10, 0, 0, time.UTC)
	oldWalk := func(tx, e string, intra uint32) {
		t.Helper()
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
			(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, ingested_at, intra_ledger_seq)
			VALUES (?, ?, ?, 0, 0, 'updated', 'contract_data', ?, ?, '2026-01-01 00:00:00', ?)`,
			base, closeAt, tx, key, e, intra); err != nil {
			t.Fatalf("insert old-walk row: %v", err)
		}
	}
	oldWalk("aa", "MID", 57)
	oldWalk("bb", "END", 12)
	if err := runLendingWrite(t, addr, base, base); err != nil {
		t.Fatalf("first derive: %v", err)
	}
	row := func(e string) []string {
		return []string{"2027-02-12 09:00:00 " + strconv.FormatUint(uint64(base), 10) + " " + e}
	}
	if got := lendingHistory(t, ctx, conn, pool, key); !slices.Equal(got, row("MID")) {
		t.Fatalf("history before the re-derive = %q, want %q", got, row("MID"))
	}

	stmts, err := clickHouseDeployStatements("lending_state.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("apply lending_state.sql over the existing table: %.72q: %v", s, err)
		}
	}
	// The re-derived walk keeps each change's identity and moves the stale one before the real last change.
	ts := chstore.LedgerEntryChangeRow{LedgerSeq: base, CloseTime: closeAt, ChangeType: "updated", EntryType: "contract_data", KeyXDR: key}
	mid, end := ts, ts
	mid.TxHash, mid.EntryXDR, mid.IntraLedgerSeq = "aa", "MID", 5
	end.TxHash, end.EntryXDR, end.IntraLedgerSeq = "bb", "END", 12
	insertEntryChanges(t, ctx, mid, end)
	if err := runLendingWrite(t, addr, base, base); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := lendingHistory(t, ctx, conn, pool, key); !slices.Equal(got, row("END")) {
		t.Fatalf("history after the replay = %q, want %q", got, row("END"))
	}
}
