//go:build integration

package integration_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestChEntryHistory_OneDecodePassWritesBothProjections drives
// `ch-entry-history` through chops.Run against the real tier-1 DDL: a dry run
// writes nothing, a -write run fills the account- and asset-keyed tables from
// one pass and advances the watermark, and an unmerged duplicate in the source
// collapses to one target row.
func TestChEntryHistory_OneDecodePassWritesBothProjections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// An isolated ledger range nothing else in the suite writes to.
	const base = uint32(160_900_000)
	const (
		alice  = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
		bob    = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
		issuer = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
		usdc   = "USDC-" + issuer
	)
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.entry_history_watermark`); err != nil {
		t.Fatalf("truncate watermark: %v", err)
	}
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 1)

	account := func(balance, seq int64) xdr.LedgerEntry {
		return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
			AccountId: xdr.MustAddress(alice), Balance: xdr.Int64(balance), SeqNum: xdr.SequenceNumber(seq),
			Thresholds: xdr.Thresholds{1, 0, 0, 0},
		}}}
	}
	trustline := func(balance int64) xdr.LedgerEntry {
		return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
			AccountId: xdr.MustAddress(bob), Asset: xdr.MustNewCreditAsset("USDC", issuer).ToTrustLineAsset(),
			Balance: xdr.Int64(balance), Limit: 1_000_000,
		}}}
	}
	closeTime := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	row := func(ledger uint32, op int32, change uint32, changeType, entryType string, key xdr.LedgerEntry, withEntry bool) chstore.LedgerEntryChangeRow {
		t.Helper()
		k, err := key.LedgerKey()
		if err != nil {
			t.Fatalf("ledger key: %v", err)
		}
		r := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "ee" + strconv.FormatUint(uint64(ledger), 10),
			OpIndex: op, ChangeIndex: change, ChangeType: changeType, EntryType: entryType,
		}
		if r.KeyXDR, err = xdr.MarshalBase64(k); err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		if withEntry {
			if r.EntryXDR, err = xdr.MarshalBase64(key); err != nil {
				t.Fatalf("marshal entry: %v", err)
			}
		}
		return r
	}
	feePre, feePost := account(1000, 7), account(900, 8)
	tlFunded, tlEmpty := trustline(10), trustline(0)
	src := []chstore.LedgerEntryChangeRow{
		// base: a tx-level fee charge.
		row(base, -1, 0, "state", "account", feePre, true),
		row(base, -1, 1, "updated", "account", feePost, true),
		// base+1: bob opens a funded USDC trustline.
		row(base+1, 0, 0, "created", "trustline", tlFunded, true),
		// base+2: bob empties and removes it.
		row(base+2, 0, 0, "state", "trustline", tlEmpty, true),
		row(base+2, 0, 1, "removed", "trustline", tlEmpty, false),
	}
	// The fee charge goes in twice, as separate inserts: an unmerged duplicate
	// (one batch would have its change_index collision-resolved instead).
	for _, batch := range [][]chstore.LedgerEntryChangeRow{src, src[:2]} {
		if _, err := chstore.InsertEntryChanges(ctx, addr, batch, 0); err != nil {
			t.Fatalf("seed ledger_entry_changes: %v", err)
		}
	}

	run := func(write bool) {
		t.Helper()
		args := []string{
			"ch-entry-history", "-ch-addr", addr,
			"-from", strconv.FormatUint(uint64(base), 10), "-to", strconv.FormatUint(uint64(base+2), 10),
		}
		if write {
			args = append(args, "-write")
		}
		if err := chops.Run(args); err != nil {
			t.Fatalf("ch-entry-history (write=%v): %v", write, err)
		}
	}
	count := func(table string) uint64 {
		t.Helper()
		var n uint64
		if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.`+table+` FINAL WHERE ledger BETWEEN ? AND ?`, base, base+2).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	run(false)
	if a, s := count("account_entry_changes"), count("asset_entry_changes"); a+s != 0 {
		t.Fatalf("dry run wrote %d account / %d asset rows, want none", a, s)
	}

	run(true)
	if wm, err := chstore.EntryHistoryWatermark(ctx, addr); err != nil || wm != base+2 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+2)
	}

	type accRow struct {
		account, role, changeType, asset, balance string
		changed                                   []string
	}
	rows, err := conn.Query(ctx, `
		SELECT account, role, change_type, asset, toString(balance), changed
		FROM stellar.account_entry_changes FINAL
		WHERE ledger BETWEEN ? AND ? ORDER BY ledger, change_index`, base, base+2)
	if err != nil {
		t.Fatalf("query account_entry_changes: %v", err)
	}
	var got []accRow
	for rows.Next() {
		var r accRow
		if err := rows.Scan(&r.account, &r.role, &r.changeType, &r.asset, &r.balance, &r.changed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	_ = rows.Close()
	want := []accRow{
		{alice, "owner", "updated", "native", "900", []string{"balance", "seq_num"}},
		{bob, "owner", "created", usdc, "10", []string{}},
		{bob, "owner", "removed", usdc, "0", []string{}},
	}
	if len(got) != len(want) {
		t.Fatalf("account_entry_changes = %+v, want %+v (the duplicate fee charge must collapse)", got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.account != w.account || g.role != w.role || g.changeType != w.changeType || g.asset != w.asset ||
			g.balance != w.balance || !slices.Equal(g.changed, w.changed) {
			t.Fatalf("account_entry_changes[%d] = %+v, want %+v", i, g, w)
		}
	}

	var holderRows uint64
	var lastBalance string
	if err := conn.QueryRow(ctx, `
		SELECT count(), toString(argMax(balance, ledger))
		FROM stellar.asset_entry_changes FINAL
		WHERE asset = ? AND role = 'holder' AND account = ? AND ledger BETWEEN ? AND ?`,
		usdc, bob, base, base+2).Scan(&holderRows, &lastBalance); err != nil {
		t.Fatalf("query asset_entry_changes: %v", err)
	}
	if holderRows != 2 || lastBalance != "0" {
		t.Fatalf("asset_entry_changes %s holder rows for %s = %d (latest balance %s), want 2 ending at 0", usdc, bob, holderRows, lastBalance)
	}
}

// entryHistorySeedLedgers writes stellar.ledgers rows declaring txCount
// transactions each, so the watermark's entry-change coverage gate applies.
func entryHistorySeedLedgers(t *testing.T, ctx context.Context, addr string, seqs []uint32, txCount uint32) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, seq := range seqs {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TxCount: txCount, TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush ledger seed: %v", err)
	}
}

func runEntryHistoryWrite(addr string, from, to uint32) error {
	return chops.Run([]string{
		"ch-entry-history", "-ch-addr", addr, "-write",
		"-from", strconv.FormatUint(uint64(from), 10), "-to", strconv.FormatUint(uint64(to), 10),
	})
}

// TestChEntryHistory_WatermarkRefusesUncoveredWindow proves -write never
// advances over tx-bearing ledgers stellar.ledger_entry_changes does not
// cover — here none at all, plus one holding only a snapshot seed row (empty
// tx_hash) — so the run stops and resumes once entry changes are backfilled.
func TestChEntryHistory_WatermarkRefusesUncoveredWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const base = uint32(160_910_000)
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.entry_history_watermark`); err != nil {
		t.Fatalf("truncate watermark: %v", err)
	}
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 3)

	seedEntry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
		AccountId: xdr.MustAddress("GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"), Balance: 1, Thresholds: xdr.Thresholds{1, 0, 0, 0},
	}}}
	key, err := seedEntry.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	snap := chstore.LedgerEntryChangeRow{
		LedgerSeq: base + 1, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC),
		OpIndex: -1, ChangeType: "state", EntryType: "account",
	}
	if snap.KeyXDR, err = xdr.MarshalBase64(key); err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if snap.EntryXDR, err = xdr.MarshalBase64(seedEntry); err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{snap}, 0); err != nil {
		t.Fatalf("seed snapshot row: %v", err)
	}

	for _, w := range [][2]uint32{{base, base + 2}, {base + 1, base + 1}} {
		err := runEntryHistoryWrite(addr, w[0], w[1])
		if !errors.Is(err, chstore.ErrEntryHistoryEntryChangeShortfall) {
			t.Fatalf("ch-entry-history -write [%d,%d] over uncovered ledgers: err = %v, want ErrEntryHistoryEntryChangeShortfall", w[0], w[1], err)
		}
		if wm, err := chstore.EntryHistoryWatermark(ctx, addr); err != nil || wm != 0 {
			t.Fatalf("watermark = %d (%v) after a refused advance over [%d,%d], want 0", wm, err, w[0], w[1])
		}
	}
}

// TestChEntryHistory_WatermarkRefusesSkippedPrefix proves a -write window
// starting above watermark+1 is refused: advancing would mark the ledgers
// between them derived when they never were.
func TestChEntryHistory_WatermarkRefusesSkippedPrefix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const base = uint32(160_920_000)
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.entry_history_watermark`); err != nil {
		t.Fatalf("truncate watermark: %v", err)
	}
	// tx_count 0: no entry changes are owed, so only the prefix rule can refuse.
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 0)

	if err := runEntryHistoryWrite(addr, base, base); err != nil {
		t.Fatalf("ch-entry-history -write [%d,%d]: %v", base, base, err)
	}
	if wm, err := chstore.EntryHistoryWatermark(ctx, addr); err != nil || wm != base {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base)
	}

	err := runEntryHistoryWrite(addr, base+2, base+2)
	if !errors.Is(err, chstore.ErrEntryHistorySkippedPrefix) {
		t.Fatalf("ch-entry-history -write skipping %d: err = %v, want ErrEntryHistorySkippedPrefix", base+1, err)
	}
	if wm, err := chstore.EntryHistoryWatermark(ctx, addr); err != nil || wm != base {
		t.Fatalf("watermark = %d (%v) after a refused advance, want %d", wm, err, base)
	}
}
