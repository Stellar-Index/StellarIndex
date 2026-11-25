//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
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

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingDeployFixture is the real BLNT factory `deploy` event.
const lendingDeployFixture = "../fixtures/blnt/985d7eda4b610f28103edcb28faa52408169f455ac9a880c4a39f58ecc0c2135/deploy_factory_65134362_db1ff1407551_0_CAKLFW.json"

func runLendingWrite(t *testing.T, addr string, from, to uint32) error {
	t.Helper()
	return chops.Run([]string{
		"ch-entry-history", "-lending-only", "-ch-addr", addr, "-write", "-lock-file", filepath.Join(t.TempDir(), "lending.lock"),
		// A first run starts at from as a host's starts at its -floor-ledger: the shared lake begins at another test's range.
		"-floor-ledger", strconv.FormatUint(uint64(from), 10),
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
	// The count proof reads the watermark's hour from stellar.ledgers, so each ledger closes when its changes do.
	for i, ts := range []time.Time{at(10, 10), at(10, 50), at(11, 5), at(11, 30)} {
		if err := conn.Exec(ctx, `ALTER TABLE stellar.ledgers UPDATE close_time = ? WHERE ledger_seq = ? SETTINGS mutations_sync = 2`,
			ts, base+uint32(i)); err != nil {
			t.Fatalf("align ledger %d close time: %v", base+uint32(i), err)
		}
	}
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
	// Without -from the count proof walks every 100k-ledger window from the Blend factory genesis (~1.2k queries,
	// ~17 s each run on a CI runner) over a lake that holds nothing below base.
	prove := []string{"ch-entry-history", "-lending-only", "-prove", "-ch-addr", addr, "-from", strconv.FormatUint(uint64(base), 10)}
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
	// A row past the watermark in an hour that does not hold the watermark cannot hide that hour's missing row.
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_state_hourly
		(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2027-02-10 10:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 10:50:00', 'UTC'), 'A2')`,
		pool, key, base+4); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), "row counts disagree") {
		t.Fatalf("proof with a missing hour under a row past the watermark: err = %v, want a row-count mismatch", err)
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE ledger_seq > ? SETTINGS mutations_sync = 2`, base+3); err != nil {
		t.Fatal(err)
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
	proveLendingBoundedCases(t, ctx, conn, addr, pool)
	// The yield pool gains a live reserve the table has never seen: the proof names it.
	yieldID := xdr.ContractId(strkey.MustDecode(strkey.VersionByteContract, blnt.MainnetYieldBlentPool))
	var asset xdr.ContractId
	asset[0] = 0x5a
	create(base+4, at(12, 20), resDataKeyB64(t, yieldID, asset), "Y1")
	if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), blnt.MainnetYieldBlentPool+" has no ResData") {
		t.Fatalf("proof with a live reserve absent from the table: err = %v, want the yield pool named", err)
	}
}

// proveLendingBoundedCases covers keys changed since the watermark, whose state there the proof folds from a bounded
// lake read: a tampered row, a key the history lacks, a later change the history missed, and keys left not compared.
// It leaves the table consistent again.
func proveLendingBoundedCases(t *testing.T, ctx context.Context, conn driver.Conn, addr, pool string) {
	t.Helper()
	const base = lendingProofBase
	// Each change its own transaction: a shared (ledger, tx, op, change) identity would collapse under FINAL.
	var n uint32
	create := func(seq uint32, ts time.Time, k, e string) {
		n++
		insertEntryChanges(t, ctx, chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: ts, TxHash: "b0" + strconv.FormatUint(uint64(n), 10), ChangeType: "created",
			EntryType: "contract_data", KeyXDR: k, EntryXDR: e, IntraLedgerSeq: 100 + n,
		})
	}
	at := func(h, m int) time.Time { return time.Date(2027, 2, 10, h, m, 0, 0, time.UTC) }
	prove := []string{"ch-entry-history", "-lending-only", "-prove", "-ch-addr", addr, "-from", strconv.FormatUint(uint64(base), 10)}
	exec := func(q string, args ...any) {
		t.Helper()
		if err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rederive := func() {
		t.Helper()
		exec(`ALTER TABLE stellar.lending_state_hourly DELETE WHERE ledger_seq > ? OR entry_xdr = 'TAMPER' SETTINGS mutations_sync = 2`, base+3)
		if err := runLendingWrite(t, addr, base, base+3); err != nil {
			t.Fatalf("re-derive [%d,%d]: %v", base, base+3, err)
		}
		if err := chops.Run(prove); err != nil {
			t.Fatalf("proof after re-deriving: %v", err)
		}
	}
	wantMismatch := func(what string) {
		t.Helper()
		if err := chops.Run(prove); err == nil || !strings.Contains(err.Error(), "disagree with the lake") {
			t.Fatalf("proof with %s: err = %v, want a state mismatch", what, err)
		}
	}
	poolID := xdr.ContractId(strkey.MustDecode(strkey.VersionByteContract, pool))
	reserve := func(b byte) string {
		var a xdr.ContractId
		a[0] = b
		return resDataKeyB64(t, poolID, a)
	}

	// A key unchanged since the watermark whose latest row is wrong.
	k1 := reserve(0x5b)
	create(base+1, at(10, 50), k1, "K1")
	rederive()
	exec(`INSERT INTO stellar.lending_state_hourly (contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2027-02-10 11:00:00', 'UTC'), ?, 1, toDateTime('2027-02-10 11:05:00', 'UTC'), 'TAMPER')`, pool, k1, base+2)
	wantMismatch("a wrong row for a key unchanged since the watermark")
	rederive()

	// A key the lake changed at or below the watermark and again after it, absent from the history.
	k2 := reserve(0x5c)
	create(base+1, at(10, 50), k2, "K2")
	create(base+4, at(12, 10), k2, "K2b")
	wantMismatch("a key changed since the watermark that the history lacks")
	rederive()

	// A key whose history misses a change at or below the watermark, and that changed again after it.
	k3 := reserve(0x5d)
	create(base, at(10, 10), k3, "K3")
	create(base+2, at(11, 5), k3, "K3b")
	rederive()
	create(base+4, at(12, 10), k3, "K3c")
	exec(`ALTER TABLE stellar.lending_state_hourly DELETE WHERE key_xdr = ? AND hour = toDateTime('2027-02-10 11:00:00', 'UTC')
		SETTINGS mutations_sync = 2`, k3)
	wantMismatch("a missed change at or below the watermark")
	rederive()

	// Not compared: a key whose latest row lies deeper below the watermark than the proof reads, and a backstop key
	// (deploy ledger unknown) with no row at or below the watermark. The rest still passes; the run is INCOMPLETE.
	backstopID := xdr.ContractId(strkey.MustDecode(strkey.VersionByteContract, blnt.MainnetBackstop))
	balance := func(b byte) string {
		var a xdr.ContractId
		a[0] = b
		sym := xdr.ScSymbol("PoolBalance")
		addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &a}
		vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, {Type: xdr.ScValTypeScvAddress, Address: &addr}}
		return lendingKeyB64(t, backstopID, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec})
	}
	b1, b2 := balance(0x61), balance(0x62)
	create(base+1, at(10, 50), b1, "B1")
	rederive()
	create(base+4, at(12, 10), b2, "B2")
	// The row outside -from is left out of the count proof.
	k4 := reserve(0x5e)
	exec(`INSERT INTO stellar.lending_state_hourly (contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blnt', ?, toDateTime('2026-11-01 00:00:00', 'UTC'), ?, 1, toDateTime('2026-11-01 00:00:00', 'UTC'), 'K4')`,
		pool, k4, base-1_000_000)
	create(base+4, at(12, 10), k4, "K4b")
	err := chops.Run(append(prove, "-from", strconv.FormatUint(uint64(base), 10)))
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE: 2 keys not compared") {
		t.Fatalf("proof with keys beyond its read bound: err = %v, want INCOMPLETE naming 2 keys", err)
	}
	exec(`ALTER TABLE stellar.lending_state_hourly DELETE WHERE key_xdr = ? SETTINGS mutations_sync = 2`, k4)
	for _, tbl := range []string{"ledger_entry_changes", "ledger_entries_current"} {
		exec(`ALTER TABLE stellar.`+tbl+` DELETE WHERE key_xdr = ? SETTINGS mutations_sync = 2`, b2)
	}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof once every key is comparable: %v", err)
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
	stmts, err := clickHouseDeployStatements("lending_state.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Later tests share the table: converge its version even when this test fails after the downgrade below.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		for _, s := range stmts {
			if err := conn.Exec(cctx, s); err != nil {
				t.Errorf("re-apply lending_state.sql: %.72q: %v", s, err)
			}
		}
	})
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

// Int256 amounts above 2^200 (and negative ones) round-trip exactly as *big.Int, a re-derive of the same
// (contract_id, asset, hour) with a later derived_at wins under FINAL, and sum() stays exact past Int128.
func TestLendingInterestHourly_Int256RoundTripAndNewestDeriveWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	stmts, err := clickHouseDeployStatements("lending_interest.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("apply lending_interest.sql over tier1: %.72q: %v", s, err)
		}
	}
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.lending_interest_hourly`); err != nil {
		t.Fatal(err)
	}

	const pool, assetA, assetB = "CPOOL", "CASSETA", "CASSETB"
	hour := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	p200 := new(big.Int).Lsh(big.NewInt(1), 200)
	stale := new(big.Int).Add(p200, big.NewInt(1))
	fresh := new(big.Int).Add(p200, big.NewInt(987_654_321))
	freshResidual := new(big.Int).Neg(new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 201), big.NewInt(7)))
	other := new(big.Int).Lsh(big.NewInt(1), 130)
	insert := func(asset string, gross, residual *big.Int, derivedAt time.Time) {
		t.Helper()
		b, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.lending_interest_hourly
			(contract_id, protocol, asset, hour, first_ledger, last_ledger, gross, backstop_residual, derived_at)`)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Append(pool, "blnt", asset, hour, uint32(100), uint32(200), gross, residual, derivedAt); err != nil {
			t.Fatal(err)
		}
		if err := b.Send(); err != nil {
			t.Fatalf("insert %s: %v", asset, err)
		}
	}
	first := time.Date(2027, 3, 1, 11, 0, 0, 0, time.UTC)
	// The stale derive is inserted last: only the derived_at version, not insert order, can make the newer one win.
	insert(assetA, fresh, freshResidual, first.Add(time.Millisecond))
	insert(assetA, stale, big.NewInt(0), first)
	insert(assetB, other, big.NewInt(0), first)

	var gross, residual big.Int
	if err := conn.QueryRow(ctx, `SELECT gross, backstop_residual FROM stellar.lending_interest_hourly FINAL
		WHERE contract_id = ? AND asset = ? AND hour = ?`, pool, assetA, hour).Scan(&gross, &residual); err != nil {
		t.Fatal(err)
	}
	if gross.Cmp(fresh) != 0 || residual.Cmp(freshResidual) != 0 {
		t.Fatalf("FINAL row = (%s, %s), want the newest derive (%s, %s)", &gross, &residual, fresh, freshResidual)
	}
	var total big.Int
	if err := conn.QueryRow(ctx, `SELECT sum(gross) FROM stellar.lending_interest_hourly FINAL
		WHERE contract_id = ?`, pool).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if want := new(big.Int).Add(fresh, other); total.Cmp(want) != 0 {
		t.Fatalf("sum(gross) FINAL = %s, want %s", &total, want)
	}
}

// liI128 is a non-negative decimal as an i128 ScVal.
func liI128(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 || n.BitLen() > 127 {
		t.Fatalf("i128 %q", s)
	}
	lo := new(big.Int).And(n, new(big.Int).SetUint64(^uint64(0)))
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{
		Hi: xdr.Int64(new(big.Int).Rsh(n, 64).Uint64()), Lo: xdr.Uint64(lo.Uint64()), //nolint:gosec // BitLen <= 127
	}}
}

// liReserve is one BLNT ResData value, both as the stored ScVal and as the decoded struct the expected sums use.
type liReserve struct {
	dRate, bRate, bc string
	lastTime         uint64
}

const liBSupply, liDSupply = "30000000000000000000", "20000000000000000000" // > 2^64: no narrowing survives

func (r liReserve) val(t *testing.T) xdr.ScVal {
	lt := xdr.Uint64(r.lastTime)
	return lendingSymMap(map[string]xdr.ScVal{
		"b_rate": liI128(t, r.bRate), "b_supply": liI128(t, liBSupply), "backstop_credit": liI128(t, r.bc),
		"d_rate": liI128(t, r.dRate), "d_supply": liI128(t, liDSupply), "ir_mod": liI128(t, "10000000"),
		"last_time": {Type: xdr.ScValTypeScvU64, U64: &lt},
	}, "b_rate", "b_supply", "backstop_credit", "d_rate", "d_supply", "ir_mod", "last_time")
}

func (r liReserve) data() blend.ReserveData {
	n := func(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
	return blend.ReserveData{
		DRate: n(r.dRate), BRate: n(r.bRate), IRMod: n("10000000"), BSupply: n(liBSupply), DSupply: n(liDSupply),
		BackstopCredit: n(r.bc), LastTime: r.lastTime, Version: blend.PoolV2,
	}
}

func liFeesVal(t *testing.T, asset xdr.ContractId, credit, carry string) xdr.ScVal {
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &asset}
	m := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr},
		Val: lendingSymMap(map[string]xdr.ScVal{"carry": liI128(t, carry), "credit": liI128(t, credit)}, "carry", "credit"),
	}}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// liHour is one hour's expected sums: an independent blnt.AccrueBetween chain over its pairs.
type liHour struct {
	gross, fee, backstop, intended, realised, creditDelta big.Int
}

func (h *liHour) add(t *testing.T, prev, next liReserve, prevFee, nextFee blnt.ProtocolFee) {
	t.Helper()
	a, err := blnt.AccrueBetween(prev.data(), next.data(), prevFee, nextFee, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	h.gross.Add(&h.gross, a.Gross)
	h.fee.Add(&h.fee, a.ProtocolFee)
	h.backstop.Add(&h.backstop, a.Backstop)
	h.intended.Add(&h.intended, a.SupplierIntended)
	h.realised.Add(&h.realised, a.SupplierRealised)
	h.creditDelta.Add(&h.creditDelta, new(big.Int).Sub(next.data().BackstopCredit, prev.data().BackstopCredit))
}

func liVersions(t *testing.T, ctx context.Context, addr string, lo, hi uint32, keys []chstore.LendingInterestKey) []lendinginterest.Version {
	t.Helper()
	var out []lendinginterest.Version
	if err := chstore.LendingInterestVersions(ctx, addr, lo, hi, keys, func(v lendinginterest.Version) error {
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatalf("versions [%d,%d]: %v", lo, hi, err)
	}
	return out
}

// A synthetic BLNT lake goes through the storage path the interest pass will run: keys, versions, seeds, Fold,
// insert, and the watermark. The stored sums equal an independent blnt.AccrueBetween chain; a superseded lake row
// is collapsed by FINAL; a re-derive deletes an hour it no longer writes. The read refuses a corrupt reserve, a
// renumbering leftover, a deleted ResData and a lake missing a tx-bearing ledger, and reads a deleted ProtocolFees as
// empty. Keys, seeds and the watermark refuse to pass lending_state's; the watermark refuses to start or rest
// mid-hour and to leave a registry pool uncovered, records its floor, and never moves down.
func TestLendingInterest_StoragePathOverASyntheticLake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{
		`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`,
		`TRUNCATE TABLE stellar.lending_interest_hourly`,
	} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_370_000)
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	asset[0] = 0x5e
	assetStr := strkey.MustEncode(strkey.VersionByteContract, asset[:])
	resKey := resDataKeyB64(t, poolID, asset)
	instKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
	feeSym := xdr.ScSymbol("ProtocolFees")
	feeVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &feeSym}}
	feeKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &feeVec})

	at := func(h, m int) time.Time { return time.Date(2027, 2, 13, h, m, 0, 0, time.UTC) }
	closes := []time.Time{
		at(10, 10), at(10, 50), at(11, 5), at(11, 40), at(12, 10), at(12, 20), at(13, 5), at(14, 5), at(15, 5), at(16, 5), at(17, 5),
	}
	seqs := make([]uint32, len(closes))
	for i := range closes {
		seqs[i] = base + uint32(i)
	}
	// base-1 closes in hour 9, so base starts an hour.
	entryHistorySeedLedgers(t, ctx, addr, append([]uint32{base - 1}, seqs...), 1, false)
	for i, ts := range append([]time.Time{at(9, 50)}, closes...) {
		if err := conn.Exec(ctx, `ALTER TABLE stellar.ledgers UPDATE close_time = ? WHERE ledger_seq = ? SETTINGS mutations_sync = 2`,
			ts, base-1+uint32(i)); err != nil {
			t.Fatalf("align ledger %d close time: %v", base-1+uint32(i), err)
		}
	}

	r0 := liReserve{"1000000000000", "1000000000000", "0", 1000}
	r1 := liReserve{"1000100000000", "1000050000000", "500", 2000}
	r2 := liReserve{"1000300000000", "1000150000000", "1500", 3000}
	r3 := liReserve{"1000600000000", "1000300000000", "3000", 4000}
	r4 := liReserve{"1001000000000", "1000500000000", "4500", 5000}
	entry := func(v xdr.ScVal) string { return lendingPoolEntryB64(t, poolID, v) }
	change := func(i int, intra, idx uint32, ct, k, e string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seqs[i], CloseTime: closes[i], TxHash: "7" + strconv.Itoa(i), ChangeIndex: idx,
			ChangeType: ct, EntryType: "contract_data", KeyXDR: k, EntryXDR: e, IntraLedgerSeq: intra,
		}
	}
	insertEntryChanges(t, ctx,
		change(0, 1, 0, "created", instKey, entry(lendingPoolInstanceVal(asset))),
		change(0, 2, 1, "created", resKey, entry(r0.val(t))),
		change(0, 3, 2, "created", feeKey, entry(liFeesVal(t, asset, "0", "0"))),
		change(1, 1, 0, "updated", resKey, entry(r1.val(t))),
		change(1, 2, 1, "updated", feeKey, entry(liFeesVal(t, asset, "1000", "5"))),
		change(2, 1, 0, "updated", resKey, entry(r2.val(t))),
		// The lake writes no eviction row for a persistent key; P23 auto-restore stores the old value back.
		change(4, 1, 0, "restored", resKey, entry(r2.val(t))),
		change(5, 1, 0, "updated", resKey, entry(r3.val(t))),
		change(6, 1, 0, "updated", resKey, entry(r4.val(t))),
		change(7, 1, 0, "updated", resKey, entry(lendingPoolInstanceVal(asset))),
		// A renumbering leftover: one change at two walk positions.
		change(8, 1, 0, "updated", resKey, entry(r4.val(t))),
		change(8, 5, 3, "updated", resKey, entry(r4.val(t))),
		change(9, 1, 0, "removed", resKey, ""),
		change(10, 1, 0, "removed", feeKey, ""),
	)
	// Ledger base+3's transaction touches none of the pool's keys.
	unrelated := change(3, 1, 0, "updated", "AAAAunrelated", "")
	unrelated.EntryType = "account"
	insertEntryChanges(t, ctx, unrelated)
	// A superseded copy of ledger base+2's change (same identity, older ingest) that would not decode.
	if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
		(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, ingested_at, intra_ledger_seq)
		VALUES (?, ?, '72', 0, 0, 'updated', 'contract_data', ?, 'STALE', '2026-01-01 00:00:00', 9)`, seqs[2], closes[2], resKey); err != nil {
		t.Fatalf("insert superseded row: %v", err)
	}
	if err := runLendingWrite(t, addr, base, base+6); err != nil {
		t.Fatalf("lending state [%d,%d]: %v", base, base+6, err)
	}

	pools := []chstore.LendingContract{{ID: pool, Protocol: chstore.LendingBLNT, Deployed: base}}
	opts := lendinginterest.Options{Protocols: map[string]lendinginterest.Protocol{pool: lendinginterest.BLNT}}
	// lending_state stops at base+6: a reserve first written past it would be missing from the list.
	if _, err := chstore.LendingInterestKeys(ctx, addr, pools, base+7); !errors.Is(err, chstore.ErrLendingInterestAheadOfState) {
		t.Fatalf("keys past lending_state: err = %v, want ErrLendingInterestAheadOfState", err)
	}
	keys, err := chstore.LendingInterestKeys(ctx, addr, pools, base+2)
	if err != nil {
		t.Fatal(err)
	}
	var gotKeys []string
	for _, k := range keys {
		gotKeys = append(gotKeys, k.Key)
		if k.Kind == lendinginterest.KindReserve && k.Asset != assetStr {
			t.Errorf("reserve key asset = %s, want %s", k.Asset, assetStr)
		}
	}
	if len(keys) != 4 || keys[0].Key != resKey || !slices.Contains(gotKeys, instKey) || keys[3].Key != feeKey {
		t.Fatalf("keys = %q, want ResData, both instance keys, ProtocolFees", gotKeys)
	}

	// Window 1: [base, base+2], committing hour 10 from an empty start.
	seeds, err := chstore.LendingInterestSeeds(ctx, addr, pools, keys, at(10, 0), base)
	if err != nil {
		t.Fatal(err)
	}
	if f := seeds.Fees[pool]; len(seeds.Reserves) != 0 || len(seeds.Configs) != 0 || len(f) != 1 || f[0] == nil || len(f[0]) != 0 {
		t.Fatalf("seeds before the pool's first hour = %+v, want only an explicit empty fee map", seeds)
	}
	vs := liVersions(t, ctx, addr, base, base+2, keys)
	if len(vs) != 6 {
		t.Fatalf("versions [%d,%d] = %d, want 6 (the superseded row collapsed)", base, base+2, len(vs))
	}
	rows1, carry, err := lendinginterest.Fold(seeds.Seeds, vs, at(11, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, rows1, 1); err != nil {
		t.Fatal(err)
	}
	// A first window starting mid-hour would claim the hour's earlier ledgers.
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+1, base+1, pools); !errors.Is(err, chstore.ErrLendingInterestNotHourStart) {
		t.Fatalf("first advance from mid-hour: err = %v, want ErrLendingInterestNotHourStart", err)
	}
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base, base+1, pools); err != nil {
		t.Fatalf("watermark at hour 10's last ledger: %v", err)
	}
	var floor uint32
	if err := conn.QueryRow(ctx, `SELECT max(thru_ledger) FROM stellar.entry_history_watermark WHERE name = 'lending_interest_floor'`).
		Scan(&floor); err != nil || floor != base {
		t.Fatalf("floor = %d (%v), want %d", floor, err, base)
	}
	if _, err := chstore.LendingInterestSeeds(ctx, addr, pools, keys, at(15, 0), base+7); !errors.Is(err, chstore.ErrLendingInterestAheadOfState) {
		t.Fatalf("seeds before an hour lending_state has not finished: err = %v, want ErrLendingInterestAheadOfState", err)
	}

	// Window 2 resumes from lending_state_hourly's seeds at base+2, not from the carry.
	keys, err = chstore.LendingInterestKeys(ctx, addr, pools, base+6)
	if err != nil {
		t.Fatal(err)
	}
	seeds, err = chstore.LendingInterestSeeds(ctx, addr, pools, keys, at(11, 0), base+2)
	if err != nil {
		t.Fatal(err)
	}
	fees1 := blnt.ProtocolFee{Credit: big.NewInt(1000), Carry: big.NewInt(5)}
	rk := lendinginterest.ReserveKey{Pool: pool, Asset: assetStr}
	if r := seeds.Reserves[rk]; len(r) != 1 || r[0].DRate.Cmp(r1.data().DRate) != 0 || len(seeds.Configs[pool]) != 1 ||
		seeds.Configs[pool][0] != 2_000_000 || seeds.Fees[pool][0][assetStr].Credit.Cmp(fees1.Credit) != 0 {
		t.Fatalf("seeds before hour 11 = %+v, want r1, bstop 2000000 and the fees of base+1", seeds)
	}
	vs = liVersions(t, ctx, addr, base+2, base+6, keys)
	if len(vs) != 4 || vs[1].Ledger != base+4 || vs[1].Reserve.DRate.Cmp(r2.data().DRate) != 0 {
		t.Fatalf("versions [%d,%d] = %+v, want 4 with the restore at %d", base+2, base+6, vs, base+4)
	}
	rows2, _, err := lendinginterest.Fold(seeds.Seeds, vs, at(13, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	viaCarry, _, err := lendinginterest.Fold(carry, vs[1:], at(13, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaCarry) != len(rows2) || len(rows2) != 2 || viaCarry[0].Gross.Cmp(rows2[0].Gross) != 0 || viaCarry[1].Gross.Cmp(rows2[1].Gross) != 0 {
		t.Fatalf("resume from seeds = %+v, from the carry = %+v; want the same two hours", rows2, viaCarry)
	}
	// Re-deriving hours 11-13: an earlier derive's hour-13 row that this one no longer writes is deleted.
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_interest_hourly (contract_id, protocol, asset, hour, first_ledger,
		last_ledger, derived_at) VALUES (?, 'blnt', ?, ?, ?, ?, '2026-01-01 00:00:00')`, pool, assetStr, at(13, 0), base+6, base+6); err != nil {
		t.Fatal(err)
	}
	cutoff, err := chstore.ServerNow(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, rows2, 1); err != nil {
		t.Fatal(err)
	}
	if err := chstore.DeleteSupersededLendingInterestRows(ctx, addr, pools, at(11, 0), at(13, 0), cutoff); err != nil {
		t.Fatal(err)
	}

	fees0 := blnt.ProtocolFee{Credit: big.NewInt(0), Carry: big.NewInt(0)}
	var want [3]liHour
	want[0].add(t, r0, r1, fees0, fees1)
	want[1].add(t, r1, r2, fees1, fees1)
	want[2].add(t, r2, r2, fees1, fees1)
	want[2].add(t, r2, r3, fees1, fees1)
	type ledgers struct{ first, last, versions, accrued uint32 }
	wantLedgers := []ledgers{{base, base + 1, 2, 1}, {base + 2, base + 2, 1, 1}, {base + 4, base + 5, 2, 2}}
	rows, err := conn.Query(ctx, `SELECT hour, first_ledger, last_ledger, versions, accrued_versions, gross, protocol_fee, backstop,
		supplier_intended, supplier_realised, backstop_credit_delta, residual_explained, explained_versions, derive_rev, protocol
		FROM stellar.lending_interest_hourly FINAL WHERE contract_id = ? AND asset = ? ORDER BY hour`, pool, assetStr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	i := 0
	for ; rows.Next(); i++ {
		var hour time.Time
		var l ledgers
		var got liHour
		var explained big.Int
		var explainedN uint32
		var rev uint16
		var proto string
		if err := rows.Scan(&hour, &l.first, &l.last, &l.versions, &l.accrued, &got.gross, &got.fee, &got.backstop, &got.intended,
			&got.realised, &got.creditDelta, &explained, &explainedN, &rev, &proto); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatalf("extra row at %s", hour)
		}
		w := &want[i]
		if !hour.Equal(at(10+i, 0)) || l != wantLedgers[i] || rev != 1 || proto != "blnt" || explained.Sign() != 0 || explainedN != 0 {
			t.Errorf("row %d = %s %+v rev %d %s, want %s %+v rev 1 blnt", i, hour, l, rev, proto, at(10+i, 0), wantLedgers[i])
		}
		for _, c := range []struct {
			name      string
			got, want *big.Int
		}{
			{"gross", &got.gross, &w.gross},
			{"protocol_fee", &got.fee, &w.fee},
			{"backstop", &got.backstop, &w.backstop},
			{"supplier_intended", &got.intended, &w.intended},
			{"supplier_realised", &got.realised, &w.realised},
			{"backstop_credit_delta", &got.creditDelta, &w.creditDelta},
		} {
			if c.got.Cmp(c.want) != 0 {
				t.Errorf("hour %d %s = %s, want %s", 10+i, c.name, c.got, c.want)
			}
		}
	}
	if err := rows.Err(); err != nil || i != len(want) {
		t.Fatalf("read %d rows (%v), want %d", i, err, len(want))
	}
	if want[0].gross.Sign() <= 0 || want[1].fee.Sign() <= 0 {
		t.Fatalf("degenerate fixture: hour 10 gross %s, hour 11 fee %s", &want[0].gross, &want[1].fee)
	}

	err = chstore.LendingInterestVersions(ctx, addr, base+7, base+7, keys, func(lendinginterest.Version) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ledger "+strconv.FormatUint(uint64(base+7), 10)) || !strings.Contains(err.Error(), resKey) {
		t.Fatalf("corrupt reserve read: err = %v, want a refusal naming ledger %d and the key", err, base+7)
	}
	refusal := func(seq uint32, want string) {
		t.Helper()
		err := chstore.LendingInterestVersions(ctx, addr, seq, seq, keys, func(lendinginterest.Version) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "ledger "+strconv.FormatUint(uint64(seq), 10)) || !strings.Contains(err.Error(), want) {
			t.Errorf("read of ledger %d: err = %v, want a refusal naming it and %q", seq, err, want)
		}
	}
	refusal(base+8, "renumbering leftover")
	refusal(base+9, "deleted its ResData")
	if vs := liVersions(t, ctx, addr, base+10, base+10, keys); len(vs) != 1 || vs[0].Removed || vs[0].Fees == nil || len(vs[0].Fees) != 0 {
		t.Errorf("deleted ProtocolFees = %+v, want one version holding an empty map", vs)
	}

	// The lake losing a tx-bearing ledger's changes (a ch-rebuild or orphan delete part-way) refuses both the read
	// and the advance; the same window passes once they are back.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.ledger_entry_changes DELETE WHERE ledger_seq = ? SETTINGS mutations_sync = 2`, base+3); err != nil {
		t.Fatal(err)
	}
	if err := chstore.LendingInterestVersions(ctx, addr, base+2, base+6, keys, func(lendinginterest.Version) error { return nil }); !errors.Is(err, chstore.ErrEntryHistoryEntryChangeShortfall) {
		t.Errorf("read over a lost ledger: err = %v, want ErrEntryHistoryEntryChangeShortfall", err)
	}
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+2, base+5, pools); !errors.Is(err, chstore.ErrEntryHistoryEntryChangeShortfall) {
		t.Errorf("advance over a lost ledger: err = %v, want ErrEntryHistoryEntryChangeShortfall", err)
	}
	insertEntryChanges(t, ctx, unrelated)

	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+2, base+4, pools); !errors.Is(err, chstore.ErrLendingInterestNotHourEnd) {
		t.Fatalf("mid-hour advance: err = %v, want ErrLendingInterestNotHourEnd", err)
	}
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+2, base+7, pools); !errors.Is(err, chstore.ErrLendingInterestAheadOfState) {
		t.Fatalf("advance past lending_state: err = %v, want ErrLendingInterestAheadOfState", err)
	}

	// A pool added to the registry after the watermark passed its deploy ledger refuses the advance until a bounded
	// per-pool backfill covers it; one deployed past the watermark needs none.
	late := strkey.MustEncode(strkey.VersionByteContract, append([]byte{0x77}, make([]byte, 31)...))
	future := strkey.MustEncode(strkey.VersionByteContract, append([]byte{0x78}, make([]byte, 31)...))
	lateC := chstore.LendingContract{ID: late, Protocol: chstore.LendingBlendV2, Deployed: base}
	all := append(slices.Clone(pools), lateC, chstore.LendingContract{ID: future, Protocol: chstore.LendingBlendV2, Deployed: base + 3})
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+2, base+5, all); !errors.Is(err, chstore.ErrLendingInterestPoolUncovered) {
		t.Fatalf("advance with an uncovered pool: err = %v, want ErrLendingInterestPoolUncovered", err)
	}
	gaps, err := chstore.LendingInterestUncoveredPools(ctx, addr, all)
	if err != nil || len(gaps) != 1 || gaps[0] != (chstore.LendingInterestPoolGap{Pool: late, Deployed: base}) {
		t.Fatalf("uncovered pools = %+v (%v), want only %s", gaps, err, late)
	}
	if err := chstore.SetLendingInterestPoolWatermark(ctx, addr, lateC, base+1, base+1); !errors.Is(err, chstore.ErrLendingInterestPoolUncovered) {
		t.Fatalf("pool backfill starting past its deploy ledger: err = %v, want ErrLendingInterestPoolUncovered", err)
	}
	if err := chstore.SetLendingInterestPoolWatermark(ctx, addr, lateC, base, base+1); err != nil {
		t.Fatalf("pool backfill: %v", err)
	}
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base+2, base+5, all); err != nil {
		t.Fatalf("advance to hour 12's last ledger: %v", err)
	}
	if gaps, err := chstore.LendingInterestUncoveredPools(ctx, addr, all); err != nil || len(gaps) != 0 {
		t.Fatalf("uncovered pools after the advance = %+v (%v), want none", gaps, err)
	}
	if err := chstore.SetLendingInterestWatermark(ctx, addr, base, base+1, pools); err != nil {
		t.Fatalf("re-set below the watermark: %v", err)
	}
	if wm, err := chstore.LendingInterestWatermark(ctx, addr); err != nil || wm != base+5 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+5)
	}
}

// liBLNTPoolWASM is the audited BLNT pool hash (docs/operations/wasm-audits/blnt.md).
const liBLNTPoolWASM = "0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356"

// liInstanceVal is lendingPoolInstanceVal running WASM hash hexHash.
func liInstanceVal(t *testing.T, oracle xdr.ContractId, hexHash string) xdr.ScVal {
	t.Helper()
	b, err := hex.DecodeString(hexHash)
	if err != nil || len(b) != 32 {
		t.Fatalf("wasm hash %q", hexHash)
	}
	var h xdr.Hash
	copy(h[:], b)
	v := lendingPoolInstanceVal(oracle)
	v.Instance.Executable = xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &h}
	return v
}

// liRun runs the command; -floor-ledger goes only to runs that read it (the state pass or -interest-init), since an
// interest-only run refuses it.
func liRun(t *testing.T, addr string, base uint32, args ...string) error {
	t.Helper()
	cmd := []string{"ch-entry-history", "-lending-only", "-ch-addr", addr, "-write", "-lock-file", filepath.Join(t.TempDir(), "lending.lock")}
	if !slices.Contains(args, "-interest-only") || slices.Contains(args, "-interest-init") {
		cmd = append(cmd, "-floor-ledger", strconv.FormatUint(uint64(base), 10))
	}
	return chops.Run(append(cmd, args...))
}

func liU(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// The real command path over a synthetic BLNT lake: the hourly timer run leaves the interest table empty until
// -interest-init; init and two timer runs write rows equal to an independent blnt.AccrueBetween chain with the
// watermark on an hour end, and a run with nothing new writes nothing; a -from re-derive rewrites its hours and
// deletes one it no longer writes; an upgrade to an unaudited WASM refuses and holds the watermark.
func TestChEntryHistoryLending_InterestPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{
		`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`,
		`TRUNCATE TABLE stellar.lending_interest_hourly`,
	} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_500_000)
	pool := seedLendingDeploy(t, ctx, addr, base)
	var poolID, yieldID, asset xdr.ContractId
	copy(poolID[:], strkey.MustDecode(strkey.VersionByteContract, pool))
	copy(yieldID[:], strkey.MustDecode(strkey.VersionByteContract, blnt.MainnetYieldBlentPool))
	asset[0] = 0x5f
	assetStr := strkey.MustEncode(strkey.VersionByteContract, asset[:])
	resKey := resDataKeyB64(t, poolID, asset)
	instKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
	yieldInst := lendingKeyB64(t, yieldID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
	feeSym := xdr.ScSymbol("ProtocolFees")
	feeVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &feeSym}}
	feeKey := lendingKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &feeVec})

	at := func(h, m int) time.Time { return time.Date(2027, 2, 20, h, m, 0, 0, time.UTC) }
	closes := []time.Time{
		at(10, 10), at(10, 50), at(11, 5), at(11, 40), at(12, 10), at(12, 50), at(13, 5), at(13, 30),
		at(14, 5), at(14, 30), at(15, 5), at(15, 30), at(16, 5), at(16, 30), at(17, 5),
	}
	seqs := []uint32{base - 1}
	for i := range closes {
		seqs = append(seqs, base+uint32(i))
	}
	entryHistorySeedLedgers(t, ctx, addr, seqs, 1, false)
	for i, ts := range append([]time.Time{at(9, 50)}, closes...) {
		if err := conn.Exec(ctx, `ALTER TABLE stellar.ledgers UPDATE close_time = ? WHERE ledger_seq = ? SETTINGS mutations_sync = 2`,
			ts, seqs[i]); err != nil {
			t.Fatal(err)
		}
	}
	// The replay gate reads the instance index only once it is genesis-complete.
	if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`,
		chstore.ContractInstanceChangesTable, base+100); err != nil {
		t.Fatal(err)
	}

	rs := []liReserve{
		{"1000000000000", "1000000000000", "0", 1000},
		{"1000100000000", "1000050000000", "500", 2000},
		{"1000300000000", "1000150000000", "1500", 3000},
		{"1000600000000", "1000300000000", "3000", 4000},
		{"1001000000000", "1000500000000", "4500", 5000},
		{"1001500000000", "1000750000000", "7000", 6000},
		{"1002100000000", "1001050000000", "9500", 7000},
	}
	// Reserve versions at these ledger offsets; the fee entry each writes is the chain's own expectation.
	at6 := []int{0, 1, 2, 4, 6, 8, 10}
	entry := func(v xdr.ScVal) string { return lendingPoolEntryB64(t, poolID, v) }
	change := func(i int, intra, idx uint32, ct, k, e string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: base + uint32(i), CloseTime: closes[i], TxHash: "8" + strconv.Itoa(i), ChangeIndex: idx,
			ChangeType: ct, EntryType: "contract_data", KeyXDR: k, EntryXDR: e, IntraLedgerSeq: intra,
		}
	}
	rows := []chstore.LedgerEntryChangeRow{
		change(0, 1, 0, "created", instKey, entry(liInstanceVal(t, asset, liBLNTPoolWASM))),
		change(0, 1, 1, "created", yieldInst, lendingPoolEntryB64(t, yieldID, liInstanceVal(t, asset, liBLNTPoolWASM))),
		change(0, 2, 2, "created", resKey, entry(rs[0].val(t))),
		change(0, 3, 3, "created", feeKey, entry(liFeesVal(t, asset, "0", "0"))),
	}
	var want [6]liHour // hours 10..15
	fee := blnt.ProtocolFee{Credit: big.NewInt(0), Carry: big.NewInt(0)}
	for j := 1; j < len(rs); j++ {
		a, err := blnt.AccrueBetween(rs[j-1].data(), rs[j].data(), fee, fee, 2_000_000)
		if err != nil {
			t.Fatal(err)
		}
		next := a.ExpectedFees
		want[closes[at6[j]].Hour()-10].add(t, rs[j-1], rs[j], fee, next)
		rows = append(rows, change(at6[j], 1, 0, "updated", resKey, entry(rs[j].val(t))),
			change(at6[j], 2, 1, "updated", feeKey, entry(liFeesVal(t, asset, next.Credit.String(), next.Carry.String()))))
		fee = next
	}
	insertEntryChanges(t, ctx, rows...)
	for _, i := range []int{3, 5, 7, 9, 11, 12, 14} { // every tx-bearing ledger holds a change
		u := change(i, 1, 0, "updated", "AAAAunrelated"+strconv.Itoa(i), "")
		u.EntryType = "account"
		insertEntryChanges(t, ctx, u)
	}
	// liDSupply makes every gross·rate whole units, so the carry stays 0 here; TestCheckFeeCarry moves it.
	if fee.Credit.Sign() == 0 {
		t.Fatalf("degenerate fixture: final fee entry %+v", fee)
	}

	// Rows under FINAL and the newest derive: a run that rewrote anything moves the latter.
	count := func() (n uint64, newest time.Time) {
		t.Helper()
		if err := conn.QueryRow(ctx, `SELECT count(), max(derived_at) FROM stellar.lending_interest_hourly FINAL
			WHERE contract_id IN (?, ?)`, pool, blnt.MainnetYieldBlentPool).Scan(&n, &newest); err != nil {
			t.Fatal(err)
		}
		return n, newest
	}
	wm := func() uint32 {
		t.Helper()
		w, err := chstore.LendingInterestWatermark(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	// The hourly timer's run: the state pass runs, the interest pass is not initialised and writes nothing.
	if err := liRun(t, addr, base, "-from", liU(base), "-to", liU(base+7)); err != nil {
		t.Fatalf("state pass: %v", err)
	}
	if n, _ := count(); n != 0 || wm() != 0 {
		t.Fatalf("uninitialised timer run: %d rows, watermark %d; want none", n, wm())
	}
	if err := liRun(t, addr, base, "-interest-only", "-interest-init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if w := wm(); w != base+7 {
		t.Fatalf("watermark after init = %d, want %d (hour 13's last ledger)", w, base+7)
	}
	// Two timer runs: the first derives hour 14, the second has nothing new.
	var newest [2]time.Time
	for i := range 2 {
		if err := liRun(t, addr, base, "-to", liU(base+9)); err != nil {
			t.Fatalf("timer run: %v", err)
		}
		var n uint64
		if n, newest[i] = count(); n != 5 {
			t.Fatalf("rows after timer run %d = %d, want 5", i+1, n)
		}
	}
	if !newest[0].Equal(newest[1]) {
		t.Fatalf("the second timer run rewrote rows (newest derive %s, then %s); want a no-op", newest[0], newest[1])
	}
	if w := wm(); w != base+9 {
		t.Fatalf("watermark after the timer runs = %d, want %d", w, base+9)
	}
	checkLiRows(t, ctx, conn, pool, assetStr, at(10, 0), want[:5], []uint32{base, base + 2, base + 4, base + 6, base + 8},
		[]uint32{base + 1, base + 2, base + 4, base + 6, base + 8})

	// Re-derive hours 11-12 from mid-hour: an earlier derive's row of an asset no longer written there is deleted.
	if err := conn.Exec(ctx, `INSERT INTO stellar.lending_interest_hourly (contract_id, protocol, asset, hour, first_ledger,
		last_ledger, derived_at) VALUES (?, 'blnt', 'CSTALE', ?, ?, ?, '2026-01-01 00:00:00')`, pool, at(12, 0), base+4, base+4); err != nil {
		t.Fatal(err)
	}
	if err := liRun(t, addr, base, "-interest-only", "-from", liU(base+3), "-to", liU(base+5)); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	var stale uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.lending_interest_hourly FINAL WHERE asset = 'CSTALE'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("superseded row count = %d (%v), want 0", stale, err)
	}
	checkLiRows(t, ctx, conn, pool, assetStr, at(10, 0), want[:5], []uint32{base, base + 2, base + 4, base + 6, base + 8},
		[]uint32{base + 1, base + 2, base + 4, base + 6, base + 8})
	if w := wm(); w != base+9 {
		t.Fatalf("watermark after the re-derive = %d, want %d (it never moves down)", w, base+9)
	}

	liStaleStateCases(t, ctx, conn, addr, base, pool, assetStr, rs, want[5], count)
	checkLiRows(t, ctx, conn, pool, assetStr, at(10, 0), want[:], []uint32{base, base + 2, base + 4, base + 6, base + 8, base + 10},
		[]uint32{base + 1, base + 2, base + 4, base + 6, base + 8, base + 10})
	// The lake's last image in a seed's own ledger rewritten (another tx, a later intra, another value): the instance's
	// latest change is its seed ledger; the reserve changed since its hour-14 seed.
	var oracle xdr.ContractId
	oracle[0] = 0x6e
	instRow := change(0, 9, 0, "updated", instKey, entry(liInstanceVal(t, oracle, liBLNTPoolWASM)))
	resRow := change(8, 3, 0, "updated", resKey, entry(rs[4].val(t)))
	instRow.TxHash, resRow.TxHash = "9r0", "9r8"
	liSeedImageCases(t, ctx, conn, addr, base, []chstore.LedgerEntryChangeRow{instRow, resRow}, count)
	checkLiRows(t, ctx, conn, pool, assetStr, at(10, 0), want[:], []uint32{base, base + 2, base + 4, base + 6, base + 8, base + 10},
		[]uint32{base + 1, base + 2, base + 4, base + 6, base + 8, base + 10})
	liProveCases(t, ctx, conn, addr, base, pool, assetStr, resKey, at)

	// An upgrade to unaudited code refuses the window naming the hash; the watermark holds.
	const unaudited = "1111111111111111111111111111111111111111111111111111111111111111"
	insertEntryChanges(t, ctx, change(13, 1, 0, "updated", instKey, entry(liInstanceVal(t, asset, unaudited))))
	err := liRun(t, addr, base, "-to", liU(base+13))
	if err == nil || !strings.Contains(err.Error(), unaudited) {
		t.Fatalf("run over the upgrade: err = %v, want a refusal naming %s", err, unaudited)
	}
	if w := wm(); w != base+11 {
		t.Fatalf("watermark after the refusal = %d, want %d", w, base+11)
	}

	// A pool lending_state never derived (no instance row) refuses its window, naming the replay, though it has no
	// reserve key to miss.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE contract_id = ? SETTINGS mutations_sync = 2`,
		blnt.MainnetYieldBlentPool); err != nil {
		t.Fatal(err)
	}
	err = liRun(t, addr, base, "-interest-only", "-from", liU(base+10), "-to", liU(base+11))
	// The lake half of the message depends on the registry's deploy ledger, the lowest any test in the shared lake wrote.
	if err == nil || !strings.Contains(err.Error(), "lending_state") || !strings.Contains(err.Error(), "pool "+blnt.MainnetYieldBlentPool) {
		t.Fatalf("run over an underived pool: err = %v, want the refusal naming it", err)
	}
}

// liStaleStateCases runs the lending_state replay interleaving against the interest pass at watermark base+9: with
// lending_state's rows of hours 13-14 deleted and not yet replayed, the timer's interest run for hour 15 seeds from
// hour 12's row; the lake proof refuses it, writing nothing. The replay then arms a redo: the timer's pass skips until
// a re-derive from the replay's first ledger reaches the tip, after which hour 15 holds the true gross.
func liStaleStateCases(t *testing.T, ctx context.Context, conn driver.Conn, addr string, base uint32, pool, asset string,
	rs []liReserve, hour15 liHour, count func() (uint64, time.Time),
) {
	t.Helper()
	wm := func() uint32 {
		w, err := chstore.LendingInterestWatermark(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE ledger_seq BETWEEN ? AND ? SETTINGS mutations_sync = 2`,
		base+6, base+8); err != nil {
		t.Fatal(err)
	}
	zero := blnt.ProtocolFee{Credit: big.NewInt(0), Carry: big.NewInt(0)}
	staleSum, err := blnt.AccrueBetween(rs[3].data(), rs[6].data(), zero, zero, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := count()
	err = liRun(t, addr, base, "-to", liU(base+11)) // the timer: state resumes to base+11, then the interest pass
	if err == nil || !errors.Is(err, chstore.ErrLendingInterestSeedUnproved) {
		var got big.Int
		_ = conn.QueryRow(ctx, `SELECT gross FROM stellar.lending_interest_hourly FINAL WHERE contract_id = ? AND asset = ? AND hour = ?`,
			pool, asset, time.Date(2027, 2, 20, 15, 0, 0, 0, time.UTC)).Scan(&got)
		if got.Cmp(staleSum.Gross) == 0 {
			t.Errorf("repro: hour 15 written with the stale-seed gross %s (true %s), watermark %d", &got, &hour15.gross, wm())
		}
		t.Fatalf("interest run over deleted lending_state rows: err = %v, hour 15 gross %s; want ErrLendingInterestSeedUnproved", err, &got)
	}
	if n, _ := count(); n != before || wm() != base+9 {
		t.Fatalf("refused run: %d rows (was %d), watermark %d; want nothing written", n, before, wm())
	}

	// The replay restores the rows and arms the redo; the timer's pass then skips.
	if err := liRun(t, addr, base, "-from", liU(base+6), "-to", liU(base+9)); err != nil {
		t.Fatalf("lending_state replay: %v", err)
	}
	if err := liRun(t, addr, base, "-to", liU(base+11)); err != nil || wm() != base+9 {
		t.Fatalf("timer under the redo: %v, watermark %d; want a quiet skip at %d", err, wm(), base+9)
	}
	if err := liRun(t, addr, base, "-interest-only", "-from", liU(base+7)); err == nil || !strings.Contains(err.Error(), "re-derive them first") {
		t.Fatalf("re-derive from past the replay's first ledger: %v, want a refusal", err)
	}
	if err := liRun(t, addr, base, "-interest-only", "-from", liU(base+6)); err != nil || wm() != base+11 {
		t.Fatalf("re-derive from the replay: %v, watermark %d; want %d", err, wm(), base+11)
	}
	if redo, pending, err := chstore.LendingInterestPendingRedo(ctx, addr); err != nil || pending {
		t.Fatalf("redo after the re-derive reached the watermark: %+v pending %t (%v)", redo, pending, err)
	}
}

// liSeedImageCases adds each row to the lake, as a lake re-derive that renumbers the seed's ledger leaves it, without
// replaying lending_state: the seed's ledger still matches the lake's last change, its (ledger, intra) and image do
// not, so a re-derive of hour 15 refuses and writes nothing. Each row is removed after. Then a redo armed below the
// interest floor is armed at the floor, and a re-derive from the floor clears it.
func liSeedImageCases(t *testing.T, ctx context.Context, conn driver.Conn, addr string, base uint32,
	rows []chstore.LedgerEntryChangeRow, count func() (uint64, time.Time),
) {
	t.Helper()
	for _, r := range rows {
		insertEntryChanges(t, ctx, r)
		n, newest := count()
		err := liRun(t, addr, base, "-interest-only", "-from", liU(base+10), "-to", liU(base+11))
		if !errors.Is(err, chstore.ErrLendingInterestSeedUnproved) {
			t.Fatalf("re-derive over a rewritten seed image at ledger %d: err = %v, want ErrLendingInterestSeedUnproved", r.LedgerSeq, err)
		}
		if m, nw := count(); m != n || !nw.Equal(newest) {
			t.Fatalf("refused re-derive wrote rows: %d (was %d), newest derive %s (was %s)", m, n, nw, newest)
		}
		if err := conn.Exec(ctx, `ALTER TABLE stellar.ledger_entry_changes DELETE WHERE ledger_seq = ? AND key_xdr = ? AND tx_hash = ?
			SETTINGS mutations_sync = 2`, r.LedgerSeq, r.KeyXDR, r.TxHash); err != nil {
			t.Fatal(err)
		}
	}
	floor, err := chstore.LendingInterestFloor(ctx, addr)
	if err != nil || floor == 0 {
		t.Fatalf("floor %d: %v", floor, err)
	}
	if err := chstore.ArmLendingInterestRedo(ctx, addr, floor-1); err != nil {
		t.Fatal(err)
	}
	if redo, pending, err := chstore.LendingInterestPendingRedo(ctx, addr); err != nil || !pending || redo.From != floor {
		t.Fatalf("redo armed below the floor %d: %+v pending %t (%v); want it from the floor", floor, redo, pending, err)
	}
	if err := liRun(t, addr, base, "-interest-only", "-from", liU(floor)); err != nil {
		t.Fatalf("re-derive from the floor: %v", err)
	}
	if redo, pending, err := chstore.LendingInterestPendingRedo(ctx, addr); err != nil || pending {
		t.Fatalf("redo after the re-derive from the floor: %+v pending %t (%v)", redo, pending, err)
	}
}

// liProveCases runs -prove -interest over [floor, watermark]: the writer's rows prove; a newer row version with a
// wrong gross, a stale extra row and a lost lake version each fail it naming the row; a ledger with no changes left
// refuses. It deletes lake rows of ledger base+4, so it runs after every case that re-derives hour 12.
func liProveCases(t *testing.T, ctx context.Context, conn driver.Conn, addr string, base uint32, pool, assetStr, resKey string,
	at func(h, m int) time.Time,
) {
	t.Helper()
	h12 := pool + " " + assetStr + " " + at(12, 0).Format(time.RFC3339)
	out, gauge, err := liProve(t, addr)
	if err != nil || gauge != "0" || !strings.Contains(out, ": PROVED: 6 hours proved, 0 mismatches") ||
		!strings.Contains(out, "gross telescoped 6 ") {
		t.Fatalf("prove of the writer's rows: err %v gauge %s\n%s", err, gauge, out)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO stellar.lending_interest_hourly SELECT * REPLACE (gross + 1 AS gross, now64(3) AS derived_at)
		FROM stellar.lending_interest_hourly FINAL WHERE contract_id = ? AND asset = ? AND hour = ?`, pool, assetStr, at(12, 0))
	if out, gauge, err := liProve(t, addr); err == nil || gauge != "1" || !strings.Contains(out, "mismatch "+h12+" gross: stored ") {
		t.Fatalf("prove over a corrupted gross: err %v gauge %s\n%s", err, gauge, out)
	}
	if err := liRun(t, addr, base, "-interest-only", "-from", liU(base+4), "-to", liU(base+5)); err != nil {
		t.Fatalf("re-derive hour 12: %v", err)
	}
	exec(`INSERT INTO stellar.lending_interest_hourly (contract_id, protocol, asset, hour, first_ledger, last_ledger, derive_rev)
		VALUES (?, 'blnt', 'CSTALE', ?, ?, ?, 1)`, pool, at(13, 0), base+6, base+6)
	if out, _, err := liProve(t, addr); err == nil || !strings.Contains(out, "mismatch "+pool+" CSTALE "+at(13, 0).Format(time.RFC3339)+
		" row: stored present, recomputed none") {
		t.Fatalf("prove over a stale extra row: err %v\n%s", err, out)
	}
	exec(`ALTER TABLE stellar.lending_interest_hourly DELETE WHERE asset = 'CSTALE' SETTINGS mutations_sync = 2`)
	if out, _, err := liProve(t, addr); err != nil {
		t.Fatalf("prove after the repairs: %v\n%s", err, out)
	}
	// Hour 12's only reserve version (ledger base+4); its fee change keeps the ledger covered.
	exec(`ALTER TABLE stellar.ledger_entry_changes DELETE WHERE ledger_seq = ? AND key_xdr = ? SETTINGS mutations_sync = 2`, base+4, resKey)
	if out, _, err := liProve(t, addr); err == nil || !strings.Contains(out, "mismatch "+h12+" row: stored present, recomputed none") ||
		!strings.Contains(out, "the lake's versions fail the in-line identities") {
		t.Fatalf("prove over a lost lake version: err %v\n%s", err, out)
	}
	exec(`ALTER TABLE stellar.ledger_entry_changes DELETE WHERE ledger_seq = ? SETTINGS mutations_sync = 2`, base+4)
	if _, _, err := liProve(t, addr); err == nil || !strings.Contains(err.Error(), "tx-bearing ledgers") {
		t.Fatalf("prove over a ledger with no changes: err %v, want the coverage refusal", err)
	}
}

// liProve runs `ch-entry-history -lending-only -prove -interest` and returns its report and proof_failed gauge.
func liProve(t *testing.T, addr string) (out, gauge string, err error) {
	t.Helper()
	dir := t.TempDir()
	f, ferr := os.Create(filepath.Join(dir, "stdout")) //nolint:gosec // test temp dir
	if ferr != nil {
		t.Fatal(ferr)
	}
	stdout := os.Stdout
	os.Stdout = f
	err = chops.Run([]string{"ch-entry-history", "-lending-only", "-ch-addr", addr, "-prove", "-interest", "-heartbeat", filepath.Join(dir, "x.prom")})
	os.Stdout = stdout
	_ = f.Close()
	b, rerr := os.ReadFile(filepath.Join(dir, "stdout")) //nolint:gosec // test temp dir
	if rerr != nil {
		t.Fatal(rerr)
	}
	// No -prove-recent: the full range's heartbeat.
	prom, _ := os.ReadFile(filepath.Join(dir, "ops_job_ch-entry-history-lending-interest-prove-full.prom")) //nolint:gosec // test temp dir
	for _, l := range strings.Split(string(prom), "\n") {
		if strings.HasPrefix(l, "stellarindex_lending_interest_proof_failed{") {
			gauge = l[strings.LastIndex(l, " ")+1:]
		}
	}
	return string(b), gauge, err
}

// checkLiRows compares the pool's rows under FINAL with the expected per-hour chain sums and ledgers.
func checkLiRows(t *testing.T, ctx context.Context, conn driver.Conn, pool, asset string, first time.Time, want []liHour,
	firstLedgers, lastLedgers []uint32,
) {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT hour, first_ledger, last_ledger, gross, protocol_fee, backstop, supplier_intended,
		supplier_realised, backstop_credit_delta, fee_mismatches, derive_rev
		FROM stellar.lending_interest_hourly FINAL WHERE contract_id = ? AND asset = ? ORDER BY hour`, pool, asset)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	i := 0
	for ; rows.Next(); i++ {
		var hour time.Time
		var fl, ll, feeMismatch uint32
		var rev uint16
		var got liHour
		if err := rows.Scan(&hour, &fl, &ll, &got.gross, &got.fee, &got.backstop, &got.intended, &got.realised,
			&got.creditDelta, &feeMismatch, &rev); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatalf("extra row at %s", hour)
		}
		if !hour.Equal(first.Add(time.Duration(i)*time.Hour)) || fl != firstLedgers[i] || ll != lastLedgers[i] || rev != 1 || feeMismatch != 0 {
			t.Errorf("row %d: %s ledgers %d-%d rev %d fee mismatches %d", i, hour, fl, ll, rev, feeMismatch)
		}
		w := &want[i]
		for _, c := range [][2]*big.Int{
			{&got.gross, &w.gross},
			{&got.fee, &w.fee},
			{&got.backstop, &w.backstop},
			{&got.intended, &w.intended},
			{&got.realised, &w.realised},
			{&got.creditDelta, &w.creditDelta},
		} {
			if c[0].Cmp(c[1]) != 0 {
				t.Errorf("hour %s: got %s, want %s (gross, fee, backstop, intended, realised, credit delta in order)", hour, c[0], c[1])
			}
		}
	}
	if err := rows.Err(); err != nil || i != len(want) {
		t.Fatalf("read %d rows (%v), want %d", i, err, len(want))
	}
}
