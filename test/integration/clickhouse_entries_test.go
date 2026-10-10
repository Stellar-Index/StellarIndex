//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
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

// convergeDB is a scratch database so the check never mutates the shared
// `stellar` lake the other integration tests read.
const (
	convergeDB    = "stellar_converge"
	convergeProbe = convergeDB + ".converge_probe"
)

var (
	convergeStellarRef    = regexp.MustCompile(`\bstellar\.`)
	convergeCreateDB      = regexp.MustCompile(`(?i)^CREATE\s+DATABASE\s+IF\s+NOT\s+EXISTS\s+stellar$`)
	convergeAlterTable    = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(\S+)\s+(.*)$`)
	convergeAddIfMissing  = regexp.MustCompile(`(?is)^ADD\s+(COLUMN|INDEX)\s+IF\s+NOT\s+EXISTS\s+(\w+)\s`)
	convergeUnconditional = regexp.MustCompile(`(?is)^(ADD\s+(COLUMN|INDEX)|DROP\s+INDEX|MODIFY\s+COLUMN)\s`)
)

// convergeSchema is table -> "column x" / "index y" -> full definition.
type convergeSchema map[string]map[string]string

// TestClickHouseAlterMigrationsConvergeWithFreshSchema pins that every
// ALTER-bearing deploy/clickhouse/*.sql artifact (the hand-applied upgrade
// path r1 took) lands on the same column and skip-index definitions that
// tier1_schema.sql's CREATE TABLE gives a fresh provision.
//
//   - Unconditional ALTERs (DROP+ADD INDEX) execute against the fresh schema,
//     so applying every artifact must change no tier-1 definition.
//   - `IF NOT EXISTS` ALTERs are no-ops there, which hides their definitions.
//     Each is replayed against a structural copy of its table with its
//     objects dropped, and must rebuild exactly the table's definitions.
//
// Ordinal column position is not compared: it depends on the order the files
// were run, and ch-schema-drift.sh checks it against the live lake.
func TestClickHouseAlterMigrationsConvergeWithFreshSchema(t *testing.T) {
	ctx := context.Background()
	conn := dialClickHouse(t, ctx, "default")
	dropConvergeDB(t, ctx, conn)
	t.Cleanup(func() { dropConvergeDB(t, context.Background(), conn) })

	convergeApply(t, ctx, conn, "tier1_schema.sql")
	fresh := convergeSnapshot(t, ctx, conn, "")

	files := convergeAlterFiles(t)
	if len(files) == 0 {
		t.Fatal("found no ALTER-bearing deploy/clickhouse/*.sql files; the glob or the filter is broken")
	}
	for _, f := range files {
		convergeApply(t, ctx, conn, f)
	}
	migrated := convergeSnapshot(t, ctx, conn, "")
	for table := range fresh {
		convergeDiff(t, "tier-1 table "+table+" after every ALTER artifact", fresh[table], migrated[table])
	}

	replayed := 0
	for _, f := range files {
		replayed += convergeReplayConditionalAdds(t, ctx, conn, f)
	}
	t.Logf("checked %d ALTER artifacts (%d IF NOT EXISTS statements replayed) against %d fresh tier-1 tables",
		len(files), replayed, len(fresh))
}

func dropConvergeDB(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+convergeDB+" SYNC"); err != nil {
		t.Fatalf("drop %s: %v", convergeDB, err)
	}
}

// convergeStatements reads a deploy artifact and re-points it at convergeDB.
func convergeStatements(t *testing.T, name string) []string {
	t.Helper()
	stmts, err := clickHouseDeployStatements(name)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range stmts {
		if convergeCreateDB.MatchString(s) {
			stmts[i] = "CREATE DATABASE IF NOT EXISTS " + convergeDB
			continue
		}
		stmts[i] = convergeStellarRef.ReplaceAllString(s, convergeDB+".")
	}
	return stmts
}

func convergeApply(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	for _, s := range convergeStatements(t, name) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.96q: %v", name, s, err)
		}
	}
}

// convergeAlterFiles lists every deploy/clickhouse artifact other than the
// fresh schema that carries an ALTER TABLE statement, so a new one is covered
// without registering it here.
func convergeAlterFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(clickHouseDeployDir(), "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		name := filepath.Base(p)
		if name == "tier1_schema.sql" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range splitSQLStatements(string(raw)) {
			if convergeAlterTable.MatchString(s) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// convergeReplayConditionalAdds replays each of name's all-`IF NOT EXISTS`
// ALTER statements against a copy of its table (`CREATE TABLE … AS`, so no
// materialized view pins the columns) from which the statement's objects were
// dropped, and requires the copy to come back identical to the table. It fails
// on any clause it does not model rather than leaving it unchecked.
func convergeReplayConditionalAdds(t *testing.T, ctx context.Context, conn driver.Conn, name string) int {
	t.Helper()
	replayed := 0
	for _, s := range convergeStatements(t, name) {
		m := convergeAlterTable.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		strip := convergeStripStatements(t, name, m[2])
		if len(strip) == 0 {
			continue
		}
		convergeExec(t, ctx, conn, name, "DROP TABLE IF EXISTS "+convergeProbe+" SYNC")
		convergeExec(t, ctx, conn, name, "CREATE TABLE "+convergeProbe+" AS "+m[1])
		for _, st := range strip {
			convergeExec(t, ctx, conn, name, "ALTER TABLE "+convergeProbe+" "+st)
		}
		convergeExec(t, ctx, conn, name, "ALTER TABLE "+convergeProbe+" "+m[2])
		want := convergeSnapshot(t, ctx, conn, strings.TrimPrefix(m[1], convergeDB+"."))
		got := convergeSnapshot(t, ctx, conn, strings.TrimPrefix(convergeProbe, convergeDB+"."))
		convergeDiff(t, fmt.Sprintf("%s: %.60q replayed onto %s", name, s, m[1]),
			convergeOnly(want), convergeOnly(got))
		convergeExec(t, ctx, conn, name, "DROP TABLE IF EXISTS "+convergeProbe+" SYNC")
		replayed++
	}
	return replayed
}

// convergeStripStatements returns the DROP clauses that undo body's
// `ADD … IF NOT EXISTS` clauses, indexes first so a column's own index never
// blocks its drop. It returns nil for a statement of unconditional clauses
// (the fresh-schema pass already executed those) and fails on a mix.
func convergeStripStatements(t *testing.T, name, body string) []string {
	t.Helper()
	var indexes, columns []string
	unconditional := 0
	for _, clause := range convergeSplitClauses(body) {
		add := convergeAddIfMissing.FindStringSubmatch(clause + " ")
		switch {
		case add != nil && strings.EqualFold(add[1], "INDEX"):
			indexes = append(indexes, "DROP INDEX IF EXISTS "+add[2])
		case add != nil:
			columns = append(columns, "DROP COLUMN IF EXISTS "+add[2])
		case convergeUnconditional.MatchString(clause + " "):
			unconditional++
		default:
			t.Fatalf("%s: ALTER clause %.80q is not one this convergence check models; extend it", name, clause)
		}
	}
	if unconditional > 0 && len(indexes)+len(columns) > 0 {
		t.Fatalf("%s: ALTER mixes IF NOT EXISTS and unconditional clauses; split it so each half is checked", name)
	}
	return append(indexes, columns...)
}

// convergeSplitClauses splits an ALTER body on the commas between clauses,
// ignoring those nested inside parentheses (`Decimal(38, 7)`).
func convergeSplitClauses(body string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(body[start:]))
}

func convergeExec(t *testing.T, ctx context.Context, conn driver.Conn, name, stmt string) {
	t.Helper()
	if err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("%s: %.96q: %v", name, stmt, err)
	}
}

// convergeOnly returns the single table of a one-table snapshot.
func convergeOnly(s convergeSchema) map[string]string {
	for _, defs := range s {
		return defs
	}
	return nil
}

// convergeSnapshot reads the column and skip-index definitions of convergeDB,
// or of one table in it when table is non-empty.
func convergeSnapshot(t *testing.T, ctx context.Context, conn driver.Conn, table string) convergeSchema {
	t.Helper()
	out := convergeSchema{}
	put := func(tbl, key, def string) {
		if out[tbl] == nil {
			out[tbl] = map[string]string{}
		}
		out[tbl][key] = def
	}
	const filter = ` WHERE database = ? AND (? = '' OR table = ?)`
	cols, err := conn.Query(ctx, `SELECT table, name, type, default_kind, default_expression, compression_codec
		FROM system.columns`+filter, convergeDB, table, table)
	if err != nil {
		t.Fatalf("query system.columns: %v", err)
	}
	for cols.Next() {
		var tbl, name, typ, kind, expr, codec string
		if err := cols.Scan(&tbl, &name, &typ, &kind, &expr, &codec); err != nil {
			t.Fatalf("scan system.columns: %v", err)
		}
		put(tbl, "column "+name, strings.Join([]string{typ, kind, expr, codec}, " | "))
	}
	cols.Close()
	idx, err := conn.Query(ctx, `SELECT table, name, type_full, expr, granularity
		FROM system.data_skipping_indices`+filter, convergeDB, table, table)
	if err != nil {
		t.Fatalf("query system.data_skipping_indices: %v", err)
	}
	defer idx.Close()
	for idx.Next() {
		var tbl, name, typ, expr string
		var gran uint64
		if err := idx.Scan(&tbl, &name, &typ, &expr, &gran); err != nil {
			t.Fatalf("scan system.data_skipping_indices: %v", err)
		}
		put(tbl, "index "+name, fmt.Sprintf("%s | %s | GRANULARITY %d", typ, expr, gran))
	}
	if len(out) == 0 {
		t.Fatalf("snapshot of %s %q is empty; the schema did not apply", convergeDB, table)
	}
	return out
}

// convergeDiff reports every column or index whose definition differs.
func convergeDiff(t *testing.T, step string, want, got map[string]string) {
	t.Helper()
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var drift []string
	for k := range keys {
		if w, g := want[k], got[k]; w != g {
			drift = append(drift, fmt.Sprintf("%s: fresh %q, via ALTER %q", k, w, g))
		}
	}
	sort.Strings(drift)
	if len(drift) > 0 {
		t.Errorf("%s: %d definition(s) differ:\n  %s", step, len(drift), strings.Join(drift, "\n  "))
	}
}

// TestClickHouseAssetEntryChanges pins the asset entry-history read: a
// re-derived row collapses to its newest version before RMT merges, keyset
// pages never repeat or skip a row, nothing above the derive watermark is
// served, and an Int128 balance above 2^64 reaches the wire exact.
func TestClickHouseAssetEntryChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		asset  = "AECTEST-" + issuer
		base   = uint32(170_000_000)
		wm     = base + 10
	)
	t.Cleanup(func() {
		bg := context.Background()
		_ = raw.Exec(bg, `DELETE FROM stellar.asset_entry_changes WHERE asset = ? SETTINGS mutations_sync = 2`, asset)
		_ = raw.Exec(bg, `DELETE FROM stellar.entry_history_watermark WHERE name IN ('entry_history', ?) SETTINGS mutations_sync = 2`,
			chstore.EntryHistoryBackfillMarker)
	})
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.asset_entry_changes`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.asset_entry_changes`) })

	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	old, newer := at, at.Add(time.Hour)
	insert := func(rows ...[]any) {
		t.Helper()
		b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.asset_entry_changes
			(asset, ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
			 entry_type, change_type, changed, account, balance, fields, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		for _, r := range rows {
			if err := b.Append(r...); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	row := func(ledger uint32, op int32, role, entryType string, bal *big.Int, fields string, ing time.Time) []any {
		return []any{
			asset, ledger, at, "aec-tx", op, uint32(0), role, uint32(0),
			entryType, "updated",
			[]string{"balance"},
			"GHOLDER", bal, fields, ing,
		}
	}
	insert(
		row(base+1, -1, "holder", "trustline", big128, `{"balance":"big"}`, old),
		row(base+2, 0, "claimable", "claimable_balance", big.NewInt(7), `{"v":"old"}`, old),
		row(base+3, 1, "selling", "offer", big.NewInt(9), `{}`, old),
		row(base+20, 0, "holder", "trustline", big.NewInt(1), `{}`, old),
	)
	// Later derives of the claimable row, in their own part so they are not yet merged.
	// Enough stale copies that no window proves its page, forcing the floored exact read.
	rederives := [][]any{row(base+2, 0, "claimable", "claimable_balance", big.NewInt(8), `{"v":"new"}`, newer)}
	for i := 0; i < 12; i++ {
		rederives = append(rederives, row(base+2, 0, "claimable", "claimable_balance", big.NewInt(7), `{"v":"old"}`, old.Add(time.Duration(i)*time.Second)))
	}
	insert(rederives...)

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	var seen []uint32
	var cur chstore.AssetEntryChangeCursor
	for page := 0; page < 5; page++ {
		got, err := er.AssetEntryChanges(ctx, asset, 1, cur, wm)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		r := got[0]
		if r.Ledger == base+2 && (r.Balance.Int64() != 8 || r.Fields != `{"v":"new"}`) {
			t.Fatalf("claimable row = %+v, want the newest derive", r)
		}
		if r.Ledger == base+1 && r.Balance.Cmp(big128) != 0 {
			t.Fatalf("holder balance = %s, want 2^100", r.Balance)
		}
		seen = append(seen, r.Ledger)
		cur = chstore.AssetEntryChangeCursor{Ledger: r.Ledger, TxHash: r.TxHash, OpIndex: r.OpIndex, ChangeIndex: r.ChangeIndex, Role: r.Role}
	}
	if len(seen) != 3 || seen[0] != base+3 || seen[1] != base+2 || seen[2] != base+1 {
		t.Fatalf("paged ledgers = %v, want [%d %d %d] (deduped, ceilinged at %d)", seen, base+3, base+2, base+1, wm)
	}

	if err := raw.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('entry_history', ?), (?, ?)`,
		wm, chstore.EntryHistoryBackfillMarker, wm); err != nil {
		t.Fatalf("watermark insert: %v", err)
	}
	if got, thru, err := er.EntryHistoryCoverage(ctx); err != nil || got < wm || thru < wm {
		t.Fatalf("coverage = %d, %d, %v; want both at least %d", got, thru, err, wm)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/AECTEST:" + issuer + "/entry-changes?limit=5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data struct {
			Asset         string `json:"asset"`
			ThroughLedger uint32 `json:"through_ledger"`
			LowerBound    bool   `json:"lower_bound"`
			Changes       []struct {
				Ledger  uint32          `json:"ledger"`
				OpIndex int32           `json:"op_index"`
				Amount  string          `json:"amount"`
				Entry   json.RawMessage `json:"entry"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d decode %v", resp.StatusCode, err)
	}
	d := body.Data
	if d.Asset != asset || d.LowerBound || d.ThroughLedger < wm || len(d.Changes) != 3 {
		t.Fatalf("HTTP view = %+v, want 3 changes of %s through %d, not a lower bound", d, asset, wm)
	}
	if last := d.Changes[2]; last.Amount != big128.String() || last.OpIndex != -1 || string(last.Entry) != `{"balance":"big"}` {
		t.Fatalf("oldest change = %+v, want the exact 2^100 tx-level holder row", last)
	}
}

// TestLedgerEntriesCurrent_SameLedgerLastChangeWins is the end-to-end proof of
// the intra-ledger tie-break against real ClickHouse: when one storage key is
// changed twice within the SAME ledger (here update-then-remove), the
// current-state projection stellar.ledger_entries_current must FINAL-resolve to
// the LAST change (the removal), never resurrect the before-image.
//
// Topology exercised: InsertEntryChanges → stellar.ledger_entry_changes → the
// ledger_entries_current_mv materialized view → stellar.ledger_entries_current
// (ReplacingMergeTree on version = ledger_seq<<32 | intra_ledger_seq) → FINAL.
//
// The two rows are inserted in the ADVERSARIAL order [removed, updated] on
// purpose: with a plain ReplacingMergeTree(ledger_seq) both rows tie on
// version, and ClickHouse keeps the LAST-inserted (the 'updated' before-image)
// — i.e. this exact test goes RED on a plain-ledger_seq schema. The composite version
// makes the removal (intra_ledger_seq 6 > 5) win regardless of insert order.
func TestLedgerEntriesCurrent_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		key    = "c24c-update-then-remove-key"
		ledger = uint32(70_000_000)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Canonical walk order in the ledger: the update (intra_ledger_seq 5) comes
	// before the removal (intra_ledger_seq 6). Rows are handed to the writer in
	// the REVERSE (adversarial) order so a ledger_seq-only version would keep
	// the wrong one.
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 6, ChangeType: "removed", EntryType: "account", KeyXDR: key,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 5, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			EntryXDR: "before-image-should-not-win",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var (
		gotChangeType string
		gotVersion    uint64
		gotSeq        uint32
	)
	const q = `SELECT change_type, version, intra_ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND key_xdr = ?`
	if err := conn.QueryRow(ctx, q, key).Scan(&gotChangeType, &gotVersion, &gotSeq); err != nil {
		t.Fatalf("read ledger_entries_current FINAL: %v", err)
	}

	// The last intra-ledger change (the removal, seq 6) must be the survivor.
	if gotChangeType != "removed" {
		t.Errorf("FINAL current-state change_type = %q, want \"removed\" (the deleted entry was RESURRECTED — same-ledger tie resolved to a stale before-image)", gotChangeType)
	}
	if gotSeq != 6 {
		t.Errorf("winning intra_ledger_seq = %d, want 6 (the LAST change in the ledger must win)", gotSeq)
	}
	// version = (ledger_seq << 32) | intra_ledger_seq — the monotonic composite.
	if want := (uint64(ledger) << 32) | 6; gotVersion != want {
		t.Errorf("winning version = %d, want %d (ledger_seq<<32 | intra_ledger_seq)", gotVersion, want)
	}
}

// Integration coverage for `stellarindex-ops supply seed-claimable-balances`'s
// lake reader. The unit tests in internal/storage/clickhouse cover the Go-side
// reduction exhaustively; what only a real server can prove is the SQL — the
// PREWHERE on entry_type, the single argMax over a TUPLE of every projected
// column, and the tuple's within-ledger ordering — which is exactly where the
// SAC seed's tie-break bug lived.
//
// Every test scopes its assertions to its OWN claimable ids, because the
// reader is deliberately network-wide (no watched set) and the shared test
// schema carries other suites' entry-change rows.
//
// What ids CANNOT scope is the walk itself: the reader steps the whole lake,
// min(ledger_seq) to max(ledger_seq), in 250k-ledger windows, so its cost is
// set by the highest ledger ANY test in the process left behind. One fixture
// at ledger 4,000,000,000 made every walk here ~16,000 empty windows (53-58 s
// each unloaded, five walks in this file) and breached the 5-minute deadline
// under machine load. The fixtures that seed up there now remove
// their rows; cbsSeedsByID's window check turns any recurrence into an
// immediate, named failure instead of a load-dependent timeout, and
// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures pins the two
// known offenders in any shard layout and any order.

const (
	cbsIssuer   = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	cbsAssetKey = "AQUA:" + cbsIssuer
	// Well below the live claimable observer's floor (ledger 63,301,831)
	// — the population this seed exists to
	// recover.
	cbsPreFloorLedger = uint32(33_000_000)

	// cbsWalkWindowLedgers is the reader's initial window width
	// (claimableSeedLedgerWindow, internal/storage/clickhouse).
	cbsWalkWindowLedgers = uint64(250_000)
	// cbsMaxWalkWindows is the most windows a seed walk over the SHARED test
	// lake may take: 2,000 windows = a 500M-ledger span, ~8x mainnet's real
	// tip (~60M, ~240 windows) and over twice the suite's highest legitimate
	// fixture (217M, ~870 windows). At the ~3.5 ms an empty window costs,
	// that is ~7 s.
	cbsMaxWalkWindows = uint64(2_000)
	// cbsWalkBudget is the wall-clock ceiling on ONE walk of a bounded lake:
	// ~4x the worst walk cbsMaxWalkWindows admits, so machine load alone
	// cannot breach it, and well under the 53 s one walk took with the
	// 4,000,000,000-ledger fixture in the lake.
	cbsWalkBudget = 30 * time.Second
)

func cbsID(t *testing.T, tag byte) [32]byte {
	t.Helper()
	var id [32]byte
	// Spread the tag so ids differ in the first byte (emit's sort key) and
	// can't collide with another suite's fixtures.
	id[0], id[1], id[31] = 0xC1, tag, tag
	return id
}

func cbsAsset(t *testing.T, code string) xdr.Asset {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, cbsIssuer)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	var ac xdr.AssetCode4
	copy(ac[:], code)
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: ac,
			Issuer:    xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)},
		},
	}
}

func cbsKeyXDR(t *testing.T, id [32]byte) string {
	t.Helper()
	h := xdr.Hash(id)
	b64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.LedgerKeyClaimableBalance{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
		},
	})
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

func cbsEntryXDR(t *testing.T, id [32]byte, asset xdr.Asset, amount int64, lastMod uint32) string {
	t.Helper()
	h := xdr.Hash(id)
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastMod),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeClaimableBalance,
			ClaimableBalance: &xdr.ClaimableBalanceEntry{
				BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
				Claimants: []xdr.Claimant{},
				Asset:     asset,
				Amount:    xdr.Int64(amount),
			},
		},
	})
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

// cbsSeedsByID runs the reader and indexes what it emitted by claimable id,
// so a test can assert on its own fixtures without caring what else the shared
// schema holds.
func cbsSeedsByID(t *testing.T, ctx context.Context, addr string, assets map[string]struct{}) map[string]chstore.ClaimableBalanceSeed {
	t.Helper()
	if windows, culprit := cbsWalkWindows(t, ctx); windows > cbsMaxWalkWindows {
		t.Fatalf("the shared lake spans %d seed windows (limit %d): %s. Some fixture in this process left a far-future ledger in stellar.ledger_entry_changes; this walk would take minutes and time out under load. Remove it in that test's Cleanup (purgeLakeFixtureLedgers)",
			windows, cbsMaxWalkWindows, culprit)
	}
	out := map[string]chstore.ClaimableBalanceSeed{}
	if _, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, assets, nil, chstore.SeedWalk{}, func(s chstore.ClaimableBalanceSeed) error {
		out[s.ClaimableID] = s
		return nil
	}); err != nil {
		t.Fatalf("StreamClaimableBalanceSeeds: %v", err)
	}
	return out
}

func cbsHex(id [32]byte) string { return xdr.Hash(id).HexString() }

// cbsWalkWindows reports how many initial-width windows a seed walk over the
// shared lake would take right now, and names the row holding the top of the
// range so an over-long walk can be traced to the fixture that caused it.
// Window count — not elapsed time — is the quantity asserted before a walk:
// it is exact and independent of machine load.
func cbsWalkWindows(t *testing.T, ctx context.Context) (uint64, string) {
	t.Helper()
	conn := dialClickHouse(t, ctx, "stellar")
	var rows uint64
	var lo, hi uint32
	if err := conn.QueryRow(ctx, `SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entry_changes`).Scan(&rows, &lo, &hi); err != nil {
		t.Fatalf("read lake ledger bounds: %v", err)
	}
	if rows == 0 {
		return 0, "empty lake"
	}
	var entryType, txHash string
	if err := conn.QueryRow(ctx, `SELECT toString(entry_type), tx_hash FROM stellar.ledger_entry_changes
		WHERE ledger_seq = ? ORDER BY tx_hash LIMIT 1`, hi).Scan(&entryType, &txHash); err != nil {
		t.Fatalf("read the lake's top row: %v", err)
	}
	windows := uint64(hi-lo)/cbsWalkWindowLedgers + 1
	return windows, fmt.Sprintf("ledgers [%d, %d], top row entry_type=%s tx_hash=%q", lo, hi, entryType, txHash)
}

// TestClaimableSeed_RecoversPreFloorBalance is the headline case: a claimable
// balance created long before the live observer existed, never claimed, is
// recovered from the append-log with its exact asset, amount and TRUE
// last-modified ledger. Without this reader that balance would not
// appear in claimable_observations, under-reading AQUA's supply.
func TestClaimableSeed_RecoversPreFloorBalance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x01)
	amount := int64(4_500_000_000_000)
	closeTime := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: cbsPreFloorLedger, CloseTime: closeTime, TxHash: "cbs01", OpIndex: 0, ChangeIndex: 0,
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
		KeyXDR:   cbsKeyXDR(t, id),
		EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), amount, cbsPreFloorLedger),
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]
	if !ok {
		t.Fatal("the pre-floor claimable balance was not recovered — the seed does not close the gap it exists for")
	}
	if got.AssetKey != cbsAssetKey {
		t.Errorf("AssetKey = %q, want %q", got.AssetKey, cbsAssetKey)
	}
	if got.Balance.Cmp(big.NewInt(amount)) != 0 {
		t.Errorf("Balance = %s, want %d", got.Balance, amount)
	}
	if got.LedgerSeq != cbsPreFloorLedger {
		t.Errorf("LedgerSeq = %d, want %d (seeding at the run's position instead of the entry's true ledger lets a live observation lose the at-or-before pick)", got.LedgerSeq, cbsPreFloorLedger)
	}
	if !got.CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime = %v, want %v — observed_at is both the hypertable partition column and part of the PK", got.CloseTime, closeTime)
	}
}

// TestClaimableSeed_ClaimedBalanceIsNotSeeded — a balance claimed in a LATER
// window must not be seeded. This is the cross-window half of the reduction:
// the removal and the creation are resolved by separate server-side queries
// and reconciled in Go.
func TestClaimableSeed_ClaimedBalanceIsNotSeeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x02)
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	claimed := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbs02a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			// A claim: stellar-core emits the pre-image STATE and then the
			// REMOVED. Both are in the lake; the removal must win.
			LedgerSeq: 48_000_000, CloseTime: claimed, TxHash: "cbs02b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "state", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			LedgerSeq: 48_000_000, CloseTime: claimed, TxHash: "cbs02b", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 2, ChangeType: "removed", EntryType: "claimable_balance",
			KeyXDR: cbsKeyXDR(t, id),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]; ok {
		t.Errorf("a CLAIMED balance was seeded (%+v) — classic supply would over-report it forever", got)
	}
}

// TestClaimableSeed_SameLedgerRemovalCoherence is the same-ledger tie-break on the
// real server. A claimable balance created AND claimed inside ONE ledger is an
// ordinary pattern (one transaction can do both), so ledger_seq alone cannot
// order the changes. With independent per-column argMax aggregates ClickHouse
// may resolve the tie differently per column — entry_xdr from the live change,
// change_type from the removal — and the removed-entry skip never fires,
// resurrecting a claimed balance into classic supply. One argMax over a tuple
// of every projected column, keyed on the full within-ledger identity tuple,
// makes that structurally impossible.
func TestClaimableSeed_SameLedgerRemovalCoherence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x03)
	const ledger = uint32(36_000_000)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs03a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 10, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 123_456, ledger),
		},
		{
			// Different tx, LATER in the ledger's canonical walk. Only
			// intra_ledger_seq ranks these correctly: tx_hash "cbs03b" >
			// "cbs03a" here, so this test would also pass on the weaker
			// lexical tie-break — the intra_ledger_seq gap is what makes it
			// true by construction rather than by fixture luck.
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs03b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 11, ChangeType: "removed", EntryType: "claimable_balance",
			KeyXDR: cbsKeyXDR(t, id),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]; ok {
		t.Errorf("same-ledger create-then-claim seeded a live balance (%+v) — the deleted entry was RESURRECTED", got)
	}
}

// TestClaimableSeed_NativeAndAssetScope — native (XLM) claimable balances are
// never seeded (Algorithm 1 does not read claimable_observations, and the live
// observer declines them), and -assets scoping filters classic ones. The
// default (nil) scope must include every classic asset.
func TestClaimableSeed_NativeAndAssetScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	nativeID, aquaID, usdcID := cbsID(t, 0x04), cbsID(t, 0x05), cbsID(t, 0x06)
	ct := time.Date(2022, 9, 9, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(38_000_000)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs04", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, nativeID),
			EntryXDR: cbsEntryXDR(t, nativeID, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 100, ledger),
		},
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs05", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, aquaID),
			EntryXDR: cbsEntryXDR(t, aquaID, cbsAsset(t, "AQUA"), 200, ledger),
		},
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs06", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, usdcID),
			EntryXDR: cbsEntryXDR(t, usdcID, cbsAsset(t, "USDC"), 300, ledger),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	all := cbsSeedsByID(t, ctx, addr, nil)
	if _, seeded := all[cbsHex(nativeID)]; seeded {
		t.Error("a NATIVE claimable balance was seeded; it belongs to Algorithm 1 and the live observer skips it")
	}
	if _, seeded := all[cbsHex(aquaID)]; !seeded {
		t.Error("default scope missed a classic AQUA balance — the default must cover EVERY classic credit asset")
	}
	if _, seeded := all[cbsHex(usdcID)]; !seeded {
		t.Error("default scope missed a classic USDC balance — the default must cover EVERY classic credit asset")
	}

	scoped := cbsSeedsByID(t, ctx, addr, map[string]struct{}{cbsAssetKey: {}})
	if _, seeded := scoped[cbsHex(aquaID)]; !seeded {
		t.Error("-assets scope dropped the asset it was scoped to")
	}
	if got, seeded := scoped[cbsHex(usdcID)]; seeded {
		t.Errorf("-assets scope leaked an out-of-scope asset: %+v", got)
	}
}

// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures is the
// regression test for that. The seed reader walks the process-shared lake from
// min(ledger_seq) to max(ledger_seq), so a fixture another test leaves at a
// far-future ledger is paid for by every walk that follows it in the process.
// Two tests seed up there on purpose — TestBlendPoolReserves_SameLedgerLastChangeWins
// (ledger 4,000,000,000) and TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead
// (3,999,900,000 and up) — and each walk after them took ~16,000 empty windows,
// 53-58 s unloaded, against a 5-minute deadline the two-walk test above
// breached on a loaded machine.
//
// Which shard and which order those tests land in is an accident of the
// sorted test list, so this test does not depend on it: it RUNS both as
// subtests, lets their Cleanups fire, and then asserts on the lake they left
// behind — first the window count (exact, load-independent), then one real
// walk against a wall-clock budget. Without the purge in either seeder the
// window count is ~16,000 and the walk exhausts cbsWalkBudget.
func TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures(t *testing.T) {
	addr := clickhouseAddr(t)

	if !t.Run("argmax-fixture", TestBlendPoolReserves_SameLedgerLastChangeWins) ||
		!t.Run("blend504-fixture", TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead) {
		t.Fatal("a high-ledger fixture test failed; the lake it left behind says nothing about its cleanup")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cbsWalkBudget)
	defer cancel()

	windows, top := cbsWalkWindows(t, ctx)
	if windows > cbsMaxWalkWindows {
		t.Errorf("after the high-ledger fixtures finished the lake spans %d seed windows, want <= %d (%s) — a fixture outlived its test",
			windows, cbsMaxWalkWindows, top)
	}

	start := time.Now()
	_, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, nil, chstore.SeedWalk{}, func(chstore.ClaimableBalanceSeed) error { return nil })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seed walk over %d windows failed after %s (budget %s): %v", windows, elapsed.Round(time.Millisecond), cbsWalkBudget, err)
	}
	t.Logf("seed walk: %d windows in %s (budget %s)", windows, elapsed.Round(time.Millisecond), cbsWalkBudget)
}

// TestSACSeed_ArchivedHolderRetractedAtArchivalLedger drives both SAC seed
// readers against a real ClickHouse: a watched Balance entry written at
// lastWrite whose TTL lapsed at liveUntil must come back as a tombstone at
// liveUntil+1 carrying that ledger's stellar.ledgers close time — not at
// lastWrite, where the seed's top-of-ledger upsert would overwrite the genuine
// observation and zero the holder across [lastWrite, archival).
func TestSACSeed_ArchivedHolderRetractedAtArchivalLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		lastWrite = uint32(31_100_000)
		liveUntil = uint32(31_200_000)
		tipLedger = uint32(31_300_000)
		asset     = "ARCH:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	archivalClose := time.Date(2025, 3, 1, 12, 0, 5, 0, time.UTC)

	sac, sacContract := fhSyntheticContractAddr(t, 0xE5)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xE6)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: lastWrite, CloseTime: time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC),
			TxHash: "arch-seed-it", OpIndex: 0, ChangeIndex: 0,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(5_000_000)), lastWrite),
		},
		ttlChangeRow(keyXDR, lastWrite, 1, liveUntil, 48),
		// Lifts the lake tip (the seed's as-of ledger) past liveUntil.
		ttlChangeRow(ttlGovernedKeyXDR("arch-seed-tip"), tipLedger, 1, tipLedger+1_000_000, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)`)
	if err != nil {
		t.Fatalf("prepare ledgers: %v", err)
	}
	if err := lb.Append(liveUntil+1, archivalClose, fmt.Sprintf("%064d", liveUntil+1), "00", uint32(22)); err != nil {
		t.Fatalf("append ledger: %v", err)
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send ledgers: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  sacFullHistoryUnverified,
	}
	for name, stream := range readers {
		var got []chstore.SACBalanceSeed
		if err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				got = append(got, s)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: emitted %d seeds for the archived holder, want 1 tombstone: %+v", name, len(got), got)
		}
		s := got[0]
		if !s.IsRemoval || s.Balance.Sign() != 0 {
			t.Errorf("%s: emitted %+v, want an IsRemoval zero-balance tombstone", name, s)
		}
		if s.LedgerSeq != liveUntil+1 {
			t.Errorf("%s: tombstone LedgerSeq = %d, want archival ledger %d (not last write %d)", name, s.LedgerSeq, liveUntil+1, lastWrite)
		}
		if !s.CloseTime.Equal(archivalClose) {
			t.Errorf("%s: tombstone CloseTime = %v, want %v from stellar.ledgers", name, s.CloseTime, archivalClose)
		}
	}
}

// TestSACSeed_UncoveredTTLRefuses: a watched Balance entry with no
// stellar.ttl_live_until row (an unbackfilled projection) must fail both
// readers — never be emitted as a live holder under a clean-looking pass.
func TestSACSeed_UncoveredTTLRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		written = uint32(31_400_000)
		asset   = "NOTTL:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	sac, sacContract := fhSyntheticContractAddr(t, 0xF1)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xF2)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: written, CloseTime: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		TxHash: "nottl-seed-it", OpIndex: 0, ChangeIndex: 0,
		ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
		EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(9_000_000)), written),
	}}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  sacFullHistoryUnverified,
	}
	for name, stream := range readers {
		var emitted int
		err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				emitted++
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "no stellar.ttl_live_until row") {
			t.Errorf("%s: err = %v, want the TTL-coverage refusal", name, err)
		}
		if emitted != 0 {
			t.Errorf("%s: emitted %d seeds for the uncovered holder, want 0", name, emitted)
		}
	}
}

// sacFullHistoryUnverified is the full-history reader over the synthetic test
// lake, which carries stellar.ledgers rows only where a fixture needs them.
func sacFullHistoryUnverified(ctx context.Context, addr string, watched map[string]string, fn func(chstore.SACBalanceSeed) error) error {
	_, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, fn)
	return err
}

// TestSACFullHistorySeed_RecoversDormantPoolHolder reproduces the exact
// PHO/BLND VERDICT shape (docs/architecture/
// supply-pipeline.md "Dormant contract-held SAC balances"): a pool
// contract's SAC Balance(Address) entry whose last write predates the
// ClickHouse ledger_entries_current current-state MV's ~62M coverage
// floor. It writes the row into stellar.ledger_entry_changes (the raw
// append-log a real ch-backfill would have populated) and then
// synchronously deletes the mirrored row that the LIVE
// ledger_entries_current_mv trigger writes on every insert
// (fhSuppressFromCurrentState) — reproducing the FLOOR'S END STATE
// (a row present in the raw append-log but absent from the current-state
// projection) deterministically in a fresh test schema, where the real
// mechanism (the MV having been created strictly after some historical
// rows already existed on r1) can't be replicated because the test
// schema always creates the MV before any row is inserted. Then asserts:
//
//  1. StreamSACBalanceSeeds (the default, current-state-backed reader)
//     finds NOTHING for the dormant holder — reproducing the bug.
//  2. StreamSACBalanceSeedsFullHistory (the -full-history reader, reading
//     ledger_entry_changes directly) DOES find it — proving the fix.
func TestSACFullHistorySeed_RecoversDormantPoolHolder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32" // PHO SAC wrapper (real mainnet id)
		asset = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	// Well below the ~62,000,000 current-state floor — this is the ledger
	// the dormant pool contract actually acquired the SAC token at.
	const dormantLedger = uint32(41_500_000)
	closeTime := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)

	dormantBalance, ok := new(big.Int).SetString("599880000000000000000", 10) // ~6e20, matches the incident's magnitude
	if !ok {
		t.Fatal("bad test fixture: dormantBalance parse failed")
	}

	sacContract := fhContractScAddr(t, sac)
	// A synthetic-but-structurally-valid contract address standing in for
	// the dormant Phoenix/Blend pool holder — its own identity is
	// incidental to the test; what matters is that it's a CONTRACT
	// address (not a G-account) holding the SAC's Balance(Address) entry,
	// the exact shape the incident's pool holders had.
	poolAddr, poolContract := fhSyntheticContractAddr(t, 0xA1)
	balanceKey := fhBalanceKey(t, poolContract)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)
	entryXDR := fhEntryXDR(t, sacContract, balanceKey, fhI128Val(dormantBalance), dormantLedger)

	row := chstore.LedgerEntryChangeRow{
		LedgerSeq:   dormantLedger,
		CloseTime:   closeTime,
		TxHash:      "",
		OpIndex:     -1,
		ChangeIndex: 1,
		ChangeType:  "created",
		EntryType:   "contract_data",
		KeyXDR:      keyXDR,
		EntryXDR:    entryXDR,
	}
	written, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0)
	if err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	if written != 1 {
		t.Fatalf("InsertEntryChanges wrote %d rows, want 1", written)
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{fhLiveTTLRow(keyXDR, dormantLedger)}, 0); err != nil {
		t.Fatalf("InsertEntryChanges (ttl): %v", err)
	}
	// The live ledger_entries_current_mv mirrors the row we just inserted
	// (a fresh test schema always has the MV in place before any insert —
	// unlike r1, where it was created after ~62M-worth of ch-backfilled
	// history already existed). Synchronously delete the mirrored copy to
	// reproduce the floor's actual end state.
	fhSuppressFromCurrentState(t, ctx, addr, keyXDR)

	watched := map[string]string{sac: asset}

	// (1) The default current-state-backed reader sees NOTHING — the
	// mirrored row was suppressed above, reproducing "this Balance entry
	// is absent from ledger_entries_current".
	var currentStateFound int
	if err := chstore.StreamSACBalanceSeeds(ctx, addr, watched, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == poolAddr {
			currentStateFound++
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeeds: %v", err)
	}
	if currentStateFound != 0 {
		t.Errorf("StreamSACBalanceSeeds (current-state) found %d rows for the dormant pool holder, want 0 (test fixture didn't touch ledger_entries_current — if this fires, the fixture itself is wrong, not the reader)", currentStateFound)
	}

	// (2) The full-history reader recovers it directly from the append-log.
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == poolAddr {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("StreamSACBalanceSeedsFullHistory did not find the dormant pool holder — the fix did not recover it")
	}
	if got.ContractID != sac {
		t.Errorf("ContractID = %q, want %q", got.ContractID, sac)
	}
	if got.AssetKey != asset {
		t.Errorf("AssetKey = %q, want %q", got.AssetKey, asset)
	}
	if got.Balance.Cmp(dormantBalance) != 0 {
		t.Errorf("Balance = %s, want %s (i128 truncated?)", got.Balance, dormantBalance)
	}
	if got.LedgerSeq != dormantLedger {
		t.Errorf("LedgerSeq = %d, want %d", got.LedgerSeq, dormantLedger)
	}
}

// TestSACFullHistorySeed_LatestWriteWins proves the server-side
// `ORDER BY key_xdr, ledger_seq DESC LIMIT 1 BY key_xdr` reduction picks
// the HIGHEST-ledger write per storage key, not an arbitrary one — the
// same "latest wins" guarantee ledger_entries_current's
// ReplacingMergeTree(ledger_seq) provides, reproduced over the raw
// append-log which can (and does, under ch-backfill re-derive / live
// capture) hold multiple historical writes to the same key.
func TestSACFullHistorySeed_LatestWriteWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" // BLND SAC wrapper (real mainnet id)
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	)
	closeTimeOld := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	closeTimeNew := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xB2)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	oldBal := big.NewInt(1_000_000)
	newBal := big.NewInt(2_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 30_000_000, CloseTime: closeTimeOld, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(oldBal), 30_000_000),
		},
		{
			LedgerSeq: 45_000_000, CloseTime: closeTimeNew, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(newBal), 45_000_000),
		},
		fhLiveTTLRow(keyXDR, 45_000_000),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == holder {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("StreamSACBalanceSeedsFullHistory found no row for the test holder")
	}
	if got.Balance.Cmp(newBal) != 0 {
		t.Errorf("Balance = %s, want %s (the LOWER-ledger write won — latest-wins reduction is broken)", got.Balance, newBal)
	}
	if got.LedgerSeq != 45_000_000 {
		t.Errorf("LedgerSeq = %d, want 45000000", got.LedgerSeq)
	}
}

// TestSACFullHistorySeed_SameLedgerRemovalCoherence proves: when one
// storage key is BOTH present and removed within the SAME
// ledger, the full-history seed must resolve the single genuine latest change
// coherently (every projected column from that one row) so the removed-entry
// skip fires and the deleted balance is NOT resurrected.
//
// change_index is only a per-transaction counter (extract_entry_changes.go),
// so ledger_seq does not order intra-ledger changes — the intra-ledger order
// is (op_index, change_index). The prior query took four INDEPENDENT
// argMax(col, ledger_seq) aggregates, which ClickHouse resolves per-column on
// a ledger_seq tie: it could pair the present row's entry_xdr with the removed
// row's change_type (or vice versa), so the removed skip below missed and the
// pre-removal before-image balance leaked into the SAC supply seed. The tuple
// argMax (ledger_seq, tx_hash, op_index, change_index) forces all columns from
// the one latest row, here the op_index=1 'removed' change → holder skipped.
func TestSACFullHistorySeed_SameLedgerRemovalCoherence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	const ledger = uint32(50_000_000)
	closeTime := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xC3)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	// A non-zero before-image balance on BOTH rows: if the query resolves
	// change_type incoherently and misses the removal, this is exactly the
	// value that would be resurrected into the supply seed.
	beforeImage := big.NewInt(100_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{ // present earlier in the ledger (op 0)
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 0, ChangeIndex: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(beforeImage), ledger),
		},
		{ // removed later in the SAME ledger (op 1) — the genuine latest state
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 1, ChangeIndex: 1,
			ChangeType: "removed", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(beforeImage), ledger),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	// A retraction tombstone (IsRemoval=true, Balance=0) for this holder is
	// the CORRECT outcome: the removal is the genuine latest
	// intra-ledger state and must overwrite any prior observation. Only a
	// live, nonzero-balance emission would mean the deleted balance was
	// RESURRECTED — that's what this test guards against.
	var resurrected, tombstones int
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder != holder {
			return nil
		}
		if seed.IsRemoval && seed.Balance.Sign() == 0 && seed.LedgerSeq == ledger {
			tombstones++
		} else {
			resurrected++
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if resurrected != 0 {
		t.Errorf("removed-in-same-ledger holder was emitted %d time(s) with a live balance — the deleted balance was RESURRECTED (column-incoherent argMax tie): the latest intra-ledger change is 'removed'", resurrected)
	}
	if tombstones != 1 {
		t.Errorf("emitted %d removal tombstones at ledger %d for the removed holder, want exactly 1", tombstones, ledger)
	}
}

// TestSACFullHistorySeed_SameLedgerRecreateWins is the positive complement:
// removed early then RE-CREATED later in the same ledger → the holder IS
// emitted, with the re-created balance (the latest intra-ledger change wins,
// resolved by op_index within the tied ledger_seq).
func TestSACFullHistorySeed_SameLedgerRecreateWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	)
	const ledger = uint32(55_000_000)
	closeTime := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xD4)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	recreated := big.NewInt(777_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{ // removed early (op 0)
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "bb", OpIndex: 0, ChangeIndex: 1,
			ChangeType: "removed", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(big.NewInt(1)), ledger),
		},
		{ // re-created later in the SAME ledger (op 2) — the genuine latest state
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "bb", OpIndex: 2, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(recreated), ledger),
		},
		fhLiveTTLRow(keyXDR, ledger),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == holder {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("re-created-in-same-ledger holder was NOT emitted — the removal at op 0 incoherently won over the re-create at op 2")
	}
	if got.Balance.Cmp(recreated) != 0 {
		t.Errorf("Balance = %s, want %s (a stale intra-ledger change won)", got.Balance, recreated)
	}
}

// fhSuppressFromCurrentState synchronously deletes the row matching
// keyXDR from stellar.ledger_entries_current — used to reproduce, in a
// fresh test schema, the end state of the real current-state coverage
// floor (a row present in ledger_entry_changes but absent from the
// current-state projection). mutations_sync=2 makes the ALTER TABLE
// DELETE block until the mutation (and any dependent replica/merge work)
// completes, so the row is guaranteed gone before the test reads it.
func fhSuppressFromCurrentState(t *testing.T, ctx context.Context, addr, keyXDR string) {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:     []string{addr},
		Auth:     clickhouse.Auth{Database: "stellar"},
		Settings: clickhouse.Settings{"mutations_sync": "2"},
	})
	if err != nil {
		t.Fatalf("open clickhouse for suppress: %v", err)
	}
	defer func() { _ = conn.Close() }()
	const q = `ALTER TABLE stellar.ledger_entries_current DELETE WHERE entry_type = 'contract_data' AND key_xdr = $1`
	if err := conn.Exec(ctx, q, keyXDR); err != nil {
		t.Fatalf("suppress mirrored row from ledger_entries_current: %v", err)
	}
}

// ─── XDR fixture helpers (mirror internal/storage/clickhouse's
// sac_balance_seed_test.go — duplicated here because that package's test
// helpers aren't exported across the package boundary) ────────────────

func fhContractScAddr(t *testing.T, cAddr string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cAddr, err)
	}
	var cid [32]byte
	copy(cid[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
}

// fhBalanceKey builds the `Vec(Symbol("Balance"), Address(holder))` key
// for a CONTRACT holder (a pool address) — the shape a Phoenix/Blend pool
// contract's own SAC balance entry uses.
func fhBalanceKey(t *testing.T, holder xdr.ScAddress) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol("Balance")
	symSV := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	addrSV := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &holder}
	vec := xdr.ScVec{symSV, addrSV}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

// fhSyntheticContractAddr builds a structurally-valid, deterministic
// C-strkey + matching xdr.ScAddress from a single tag byte (via
// strkey.Encode, so it's always checksum-valid — no hand-typed strkeys
// to get wrong). Used for stand-in pool/holder addresses whose specific
// identity doesn't matter to the test.
func fhSyntheticContractAddr(t *testing.T, tag byte) (string, xdr.ScAddress) {
	t.Helper()
	var raw [32]byte
	raw[0] = tag
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode(contract, tag=%#x): %v", tag, err)
	}
	cid := xdr.ContractId(raw)
	return s, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

// fhSyntheticAccountAddr is the G-address analogue of
// [fhSyntheticContractAddr].
func fhSyntheticAccountAddr(t *testing.T, tag byte) (string, xdr.ScAddress) {
	t.Helper()
	var raw [32]byte
	raw[0] = tag
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode(account, tag=%#x): %v", tag, err)
	}
	pk := xdr.Uint256(raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	return s, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
}

func fhI128Val(amount *big.Int) xdr.ScVal {
	lo := new(big.Int).And(amount, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(amount, 64)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: xdr.Int64(hi.Int64()), Lo: xdr.Uint64(lo.Uint64())},
	}
}

// fhLiveTTLRow is the TTL change keeping a fixture Balance entry live past any
// lake tip another test can raise: the SAC seed refuses a watched key with no
// TTL row. The tx hash is per key so two fixtures' TTL rows never collapse.
func fhLiveTTLRow(keyXDR string, ledger uint32) chstore.LedgerEntryChangeRow {
	row := ttlChangeRow(keyXDR, ledger, 1, 4_000_000_000, 48)
	row.TxHash = "fh-ttl-" + keyXDR[len(keyXDR)-24:]
	return row
}

func fhKeyXDR(t *testing.T, contract xdr.ScAddress, key xdr.ScVal) string {
	t.Helper()
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   contract,
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

func fhEntryXDR(t *testing.T, contract xdr.ScAddress, key, val xdr.ScVal, lastMod uint32) string {
	t.Helper()
	le := xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastMod),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   contract,
				Key:        key,
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

// TestSeedWalk_VerifyLakeRefusesAHole pins the coverage leg on both
// full-history seed readers: a walk asked to verify the lake must refuse,
// before emitting anything, a range whose stellar.ledgers rows are missing.
// The shared test lake carries entry changes at ledgers no fixture wrote a
// ledgers row for, so the hole here is real. Without the check both readers
// emit the holder / balance below as current state, which is what let a
// LiveSink drop elect a pre-hole value and stamp it full_history.
func TestSeedWalk_VerifyLakeRefusesAHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" // BLND SAC wrapper (real mainnet id)
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
		at    = uint32(31_000_000)
	)
	closeTime := time.Date(2021, 7, 1, 0, 0, 0, 0, time.UTC)
	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xC7)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)
	cbID := cbsID(t, 0x71)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: at, CloseTime: closeTime, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(big.NewInt(7_000_000)), at),
		},
		fhLiveTTLRow(keyXDR, at),
		{
			LedgerSeq: at, CloseTime: closeTime, TxHash: "slv01", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, cbID),
			EntryXDR: cbsEntryXDR(t, cbID, cbsAsset(t, "AQUA"), 5_000, at),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	walk := chstore.SeedWalk{VerifyLake: true}

	var sacEmitted int
	ev, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, map[string]string{sac: asset}, walk, func(s chstore.SACBalanceSeed) error {
		if s.Holder == holder {
			sacEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("SAC full-history: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if sacEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("SAC full-history: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", sacEmitted, ev.LakeVerifiedThrough)
	}

	var cbEmitted int
	ev, err = chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, nil, walk, func(s chstore.ClaimableBalanceSeed) error {
		if s.ClaimableID == cbsHex(cbID) {
			cbEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("claimable: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if cbEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("claimable: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", cbEmitted, ev.LakeVerifiedThrough)
	}
}

// TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares is the
// executing proof of CA2-A14-correct-5: TrustlineAssetsAfter's pool-share
// exclusion must match only the two spellings TrustLineAssetID emits
// ("pool:<hex>" and the bare "pool"), never a real credit-asset string that
// merely starts with the substring "pool" — asset codes are case-sensitive
// and "poolX" etc. are valid Stellar asset codes (internal/canonical/asset.go
// validateClassicAssetCode).
//
// `NOT startsWith(asset, 'pool')` would drop "poolX-GISSUER..." silently
// alongside the real pool-share rows; this test goes RED on that predicate.
func TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(70_100_000)
		realAsset = "poolX-GCA14POOLPREFIXTESTISSUERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-real", Asset: realAsset,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-2", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolhex", Asset: "pool:c24deadbeef",
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-3", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolbare", Asset: "pool",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	scanner, err := chstore.NewHoldingsScanner(ctx, addr)
	if err != nil {
		t.Fatalf("NewHoldingsScanner: %v", err)
	}
	defer func() { _ = scanner.Close() }()

	// "ooo" sorts after every uppercase-coded asset other tests seed
	// (ASCII uppercase < lowercase) but strictly before "pool", "pool:..."
	// and "poolX...", so this page captures all three fixture rows without
	// depending on how many unrelated rows the shared container holds.
	seeds, err := scanner.TrustlineAssetsAfter(ctx, "ooo", 50)
	if err != nil {
		t.Fatalf("TrustlineAssetsAfter: %v", err)
	}

	var gotReal, gotPoolHex, gotPoolBare bool
	for _, s := range seeds {
		switch s.Asset {
		case realAsset:
			gotReal = true
		case "pool:c24deadbeef":
			gotPoolHex = true
		case "pool":
			gotPoolBare = true
		}
	}

	if !gotReal {
		t.Errorf("TrustlineAssetsAfter dropped real asset %q — the pool-prefix exclusion over-matched a credit asset code starting with %q", realAsset, "pool")
	}
	if gotPoolHex {
		t.Errorf("TrustlineAssetsAfter returned the pool:<hex> share row %q — pool-share exclusion regressed", "pool:c24deadbeef")
	}
	if gotPoolBare {
		t.Errorf("TrustlineAssetsAfter returned the bare pool fallback row — pool-share exclusion regressed")
	}
}

// legacyBlendReservesSQL is the BlendPoolReserves lookup as it stood
// before the current-state rewrite, frozen here as the differential oracle: a
// 250,000-ledger windowed fold over stellar.ledger_entry_changes, resolving
// the latest entry per key itself. The rewrite must agree with it row for row
// on every reserve the window could see, while no longer reading one granule
// per WRITE the pool made inside that window.
func legacyBlendReservesSQL() string {
	return `SELECT key_xdr, argMax(entry_xdr, (ledger_seq, intra_ledger_seq)) AS latest_xdr
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data'
		  AND ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledger_entry_changes) - ?
		  AND key_xdr IN (?)
		GROUP BY key_xdr
		HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'`
}

// legacyBlendWindowLedgers is the frozen oracle's window width.
const legacyBlendWindowLedgers = uint32(250_000)

// TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead is the
// live-ClickHouse proof for (`/v1/lending/pools/{pool}/reserves` →
// 503 lending-timeout at 12.1s on the largest Blend pool; 9.31s — 78% of the
// same 12s budget — on a SMALL one).
//
// Pathology: the reserve lookup folded the latest entry per key out of
// stellar.ledger_entry_changes across a 250,000-ledger (~14-day) window. A
// Blend ResData entry is REWRITTEN on nearly every pool interaction, so one
// reserve's key alone matches tens of thousands of rows scattered through
// that window's granules (tens of thousands of rows for the busiest mainnet pool's USDC
// reserve). The legacy read's cost was therefore a function of pool
// WRITE ACTIVITY, not of how many reserves were asked for — a multi-second
// floor under EVERY pool.
//
// The fix reads stellar.ledger_entries_current, whose sort key IS
// (entry_type, key_xdr): ~one row per requested key, the same shape the three
// sibling pool-state readers (Soroswap / Phoenix / Comet) have always used.
//
// Removing that window, however, also removed a bound it was serving by
// ACCIDENT. An archived (TTL-lapsed) Soroban entry has had no writes since it
// lapsed, so the narrow window dropped it as a side effect of being narrow,
// while ledger_entries_current keeps its last-known value forever. Reading the
// projection without an explicit staleness bound hands a DEAD pool's final
// reserves to the handler, which prices them at today's USD rate into
// `tvl_usd` and stamps the current watermark with flags.stale=false — a
// fabricated TVL where a naive read returned an empty reserve list. So the
// read is paired with the same archived-entry drop the three sibling readers
// carry, and this test pins BOTH halves: quiet-but-live must answer, archived
// must not. Conflating those two is the bug this fixture exists to prevent.
//
// Fixture — one pool, five reserves chosen to cover every axis:
//
//   - assetHot: ResData rewritten across `churn` ledgers inside the window,
//     the LAST write carrying a distinct b_rate. This is the pathology: the
//     legacy fold must read every one of those writes to find the winner.
//   - assetGone: written, then REMOVED as its final change. Must be absent
//     from BOTH paths — the removed-key drop is the semantics the old
//     `HAVING argMax(change_type, ...) != 'removed'` carried, and that the
//     empty-entry_xdr filter over FINAL must preserve.
//   - assetTied: two writes in the SAME ledger, the stale one sorting FIRST
//     in the base table. Pins the intra-ledger tie-break through the new path:
//     the projection's version is (ledger_seq << 32) | intra_ledger_seq, so
//     FINAL keeps the LAST intra-ledger change, exactly as the old composite
//     argMax did.
//   - assetQuiet: a single write far BELOW the legacy window, with a TTL
//     entry that is still LIVE. Quiet is not dead: the old shape reported it
//     absent ("consistent with captured window") and the projection answers.
//     The genuine coverage win — and it must SURVIVE the archived drop.
//   - assetArchived: written in the same old ledger as assetQuiet, but with a
//     LAPSED TTL. As visible to the projection as assetQuiet is; the ONLY
//     thing separating the two is the TTL verdict. Must be ABSENT, or the
//     route publishes a dead pool's reserves as current liquidity.
//
// The test then:
//
//  1. DIFFERENTIAL: the frozen legacy SQL and the reader must agree on the
//     winning entry for every reserve the window could see (hot, tied), agree
//     on the removal (gone absent from both), and differ on the two OLD
//     reserves — quiet ADMITTED where the legacy window could not see it,
//     archived still absent but now for a stated reason rather than as a
//     lucky side effect of the scan bound.
//  2. READ-ROWS: both paths measured via system.query_log. The legacy shape
//     reads at least the pool's whole in-window write history; the new one
//     must read a small multiple of the key count. Red-proof: with the old
//     query text in the reader the two figures are equal and the bound
//     below fails.
func TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		poolSeed     = byte(0xC0)
		hotSeed      = byte(0xC1)
		goneSeed     = byte(0xC2)
		tiedSeed     = byte(0xC3)
		quietSeed    = byte(0xC4)
		archivedSeed = byte(0xC5)

		// Far above every other fixture, so this test's own top (base+churn)
		// is the lake's max(ledger_seq) and the frozen oracle's 250k window
		// therefore covers the fixture — in any shard and any test order.
		// It does not lean on another test: the one other fixture up here
		// (4_000_000_000, argmax_intra_ledger_seq_readers_test.go) is within
		// 250k above base, so the window holds with it present too, and that
		// test removes its rows when it finishes. Asserted below rather than
		// assumed. This test removes its own rows the same way (see the
		// purgeLakeFixtureLedgers call below).
		base  = uint32(3_999_900_000)
		churn = 3_000
		// Far below the window: the legacy shape cannot see these two.
		quietLedger = uint32(1_000_000)

		// TTL verdicts are judged against max(ledger_seq) at compute time,
		// which the suite shares. These two sit far either side of any tip
		// this test can run under (its own top, base+churn, or 4e9 while
		// the argmax fixture is live), so the verdicts hold in any order.
		liveUntilLive     = uint32(4_294_000_000)
		liveUntilArchived = uint32(1_500_000_000)

		staleBRate    = uint64(1_000_000_000_000) // 1.0 at 12 decimals
		finalBRate    = uint64(2_000_000_000_000) // 2.0 — the winning write
		quietBRate    = uint64(3_000_000_000_000) // 3.0
		archivedBRate = uint64(4_000_000_000_000) // 4.0 — must never be served
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Everything seeded at [base, base+churn] is removed again when this test
	// finishes. Rows this high are the table's max(ledger_seq), which bounds
	// every whole-lake walker in the process: left behind, they cost each
	// later claimable-balance / SAC full-history seed walk ~16,000 empty
	// 250k-ledger windows. The quiet + archived rows at quietLedger
	// stay — they are below any realistic tip and bound nothing.
	purgeLakeFixtureLedgers(t, addr, base, base+churn)

	pool := contractIDFromSeed(poolSeed)
	poolStr := mustContractStrkey(t, poolSeed)
	hot, gone, tied := contractIDFromSeed(hotSeed), contractIDFromSeed(goneSeed), contractIDFromSeed(tiedSeed)
	quiet, archived := contractIDFromSeed(quietSeed), contractIDFromSeed(archivedSeed)
	hotStr := mustContractStrkey(t, hotSeed)
	goneStr := mustContractStrkey(t, goneSeed)
	tiedStr := mustContractStrkey(t, tiedSeed)
	quietStr := mustContractStrkey(t, quietSeed)
	archivedStr := mustContractStrkey(t, archivedSeed)

	hotKey := resDataKeyB64(t, pool, hot)
	goneKey := resDataKeyB64(t, pool, gone)
	tiedKey := resDataKeyB64(t, pool, tied)
	quietKey := resDataKeyB64(t, pool, quiet)
	archivedKey := resDataKeyB64(t, pool, archived)

	hotFinalEntry := resDataEntryB64(t, pool, hot, base+churn, finalBRate)
	tiedFinalEntry := resDataEntryB64(t, pool, tied, base+churn, finalBRate)
	quietEntry := resDataEntryB64(t, pool, quiet, quietLedger, quietBRate)
	archivedEntry := resDataEntryB64(t, pool, archived, quietLedger, archivedBRate)

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}

	// The hot reserve's churn: `churn` writes of the STALE state, one per
	// ledger, in ONE insert so they land together. This is what the legacy
	// fold has to read in full to find the winner.
	mustExec(fmt.Sprintf(`INSERT INTO stellar.ledger_entry_changes
		(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, intra_ledger_seq)
		SELECT toUInt32(%d + number), toDateTime('2024-01-01 00:00:00', 'UTC'), lpad(toString(number), 64, '0'), 0, 0,
		       'updated', 'contract_data', '%s', '%s', 1
		FROM numbers(%d)`, base, hotKey, resDataEntryB64(t, pool, hot, base, staleBRate), churn))

	rows := []chstore.LedgerEntryChangeRow{
		// assetHot: the winning write, one ledger past the churn.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-hot", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: hotKey, EntryXDR: hotFinalEntry,
		},
		// assetGone: a live write, then a REMOVAL as the final change.
		{
			LedgerSeq: base + 10, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: resDataEntryB64(t, pool, gone, base+10, staleBRate),
		},
		{
			LedgerSeq: base + 11, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "removed", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: "",
		},
		// assetTied: the LATER same-ledger change (intra 9), written first so
		// it sorts LAST in the base table's ORDER BY — a ledger_seq-only
		// tie-break keeps the stale row below instead.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: tiedFinalEntry,
		},
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: resDataEntryB64(t, pool, tied, base+churn, staleBRate),
		},
		// assetQuiet + assetArchived: one write each, far below the legacy
		// window, IDENTICAL in every respect the projection can see. Only
		// their TTL entries (seeded below) differ.
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-quiet", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: quietKey, EntryXDR: quietEntry,
		},
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-archived", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: archivedKey, EntryXDR: archivedEntry,
		},
		// The TTL entries that separate them. ttlChangeRow (ttl_liveness_test.go)
		// renders the lake's TTL change for a governed key; the
		// ttl_live_until_mv materialized view turns it into the slim projection
		// ClassifyTTLLiveness reads.
		ttlChangeRow(quietKey, base+1, 1, liveUntilLive, 48),
		ttlChangeRow(archivedKey, base+1, 1, liveUntilArchived, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Collapse ledger_entries_current to its steady state. In production
	// background merges keep the projection at ~one row per live key, which is
	// what makes the PK-prefix probe cheap; a container that was seeded
	// seconds ago has every version still sitting in unmerged parts, so
	// without this the read_rows figures below would measure merge lag rather
	// than query shape. FINAL returns the same ANSWER either way — this only
	// removes the fixture's own artefact.
	mustExec(`OPTIMIZE TABLE stellar.ledger_entries_current FINAL`)

	// The oracle derives its window from the table's global max ledger, which
	// the whole suite shares. Prove the fixture is actually inside it, or the
	// differential below would be comparing against an empty legacy answer.
	var maxLedger uint32
	if err := raw.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledger_entry_changes`).Scan(&maxLedger); err != nil {
		t.Fatalf("read max ledger: %v", err)
	}
	if maxLedger < base+churn || maxLedger-legacyBlendWindowLedgers >= base {
		t.Fatalf("fixture outside the frozen oracle's window: max(ledger_seq)=%d, window starts at %d, fixture spans [%d, %d] — the legacy answer would be empty and every comparison below vacuous",
			maxLedger, maxLedger-legacyBlendWindowLedgers, base, base+churn)
	}
	if quietLedger > maxLedger-legacyBlendWindowLedgers {
		t.Fatalf("the quiet + archived reserves (ledger %d) landed INSIDE the legacy window (starts %d) — they exist to separate the retired capture-window caveat from the staleness bound, so they must sit below it",
			quietLedger, maxLedger-legacyBlendWindowLedgers)
	}
	// The TTL verdicts are judged at the lake tip; assert the fixture's two
	// live_until values still straddle it, or the archived assertion below
	// would pass for the wrong reason (or the quiet one fail spuriously).
	if liveUntilArchived >= maxLedger {
		t.Fatalf("assetArchived's live_until (%d) is at/above the lake tip (%d) — it would classify LIVE and the archived-drop assertion would be vacuous", liveUntilArchived, maxLedger)
	}
	if liveUntilLive < maxLedger {
		t.Fatalf("assetQuiet's live_until (%d) is below the lake tip (%d) — it would classify ARCHIVED and the quiet-survives assertion would be testing the opposite property", liveUntilLive, maxLedger)
	}

	keys := []string{hotKey, goneKey, tiedKey, quietKey, archivedKey}
	legacyWinners := func(qctx context.Context) map[string]string {
		t.Helper()
		out := map[string]string{}
		rs, err := raw.Query(qctx, legacyBlendReservesSQL(), legacyBlendWindowLedgers, keys)
		if err != nil {
			t.Fatalf("legacy blend reserves: %v", err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var k, entry string
			if err := rs.Scan(&k, &entry); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out[k] = entry
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	assets := []string{hotStr, goneStr, tiedStr, quietStr, archivedStr}

	// (1) Differential.
	legacy := legacyWinners(ctx)
	states, err := reader.BlendPoolReserves(ctx, poolStr, blend.PoolV2, assets, nil)
	if err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}
	byAsset := make(map[string]chstore.BlendReserveState, len(states))
	for _, s := range states {
		byAsset[s.Asset] = s
	}

	// The fixture must actually reproduce the pathology the oracle models:
	// the legacy path has to see the hot + tied reserves, and must NOT see
	// the removed one.
	for _, want := range []struct {
		key, label, entry string
	}{
		{hotKey, "assetHot", hotFinalEntry},
		{tiedKey, "assetTied", tiedFinalEntry},
	} {
		got, ok := legacy[want.key]
		if !ok {
			t.Fatalf("legacy oracle did not resolve %s — the fixture no longer reproduces the pre-fix behaviour, so the agreement below is vacuous", want.label)
		}
		if got != want.entry {
			t.Fatalf("legacy oracle resolved %s to an unexpected write — the fixture's winner is not the one seeded as final", want.label)
		}
	}
	if _, present := legacy[goneKey]; present {
		t.Fatal("legacy oracle resolved assetGone — its final change is a removal; the fixture is wrong")
	}
	for _, tc := range []struct{ key, label string }{{quietKey, "assetQuiet"}, {archivedKey, "assetArchived"}} {
		if _, present := legacy[tc.key]; present {
			t.Fatalf("legacy oracle resolved %s from ledger %d — both old reserves must sit below the window, or the difference assertions mean nothing", tc.label, quietLedger)
		}
	}

	// Reader vs oracle: same answer for everything the window could see.
	for _, tc := range []struct {
		asset, label string
		wantBRate    uint64
	}{
		{hotStr, "assetHot", finalBRate},
		{tiedStr, "assetTied", finalBRate},
	} {
		got, ok := byAsset[tc.asset]
		if !ok {
			t.Errorf("%s absent from the reader's result; the legacy path resolved it", tc.label)
			continue
		}
		want := new(big.Int).SetUint64(tc.wantBRate)
		if got.Data.BRate == nil || got.Data.BRate.Cmp(want) != 0 {
			t.Errorf("%s b_rate = %v, want %d — the projection resolved a different write than the frozen fold did", tc.label, got.Data.BRate, tc.wantBRate)
		}
	}
	if _, present := byAsset[goneStr]; present {
		t.Error("assetGone present in the reader's result; want ABSENT — its final change was a removal, and `entry_xdr != ''` over FINAL must drop it exactly as the old HAVING did")
	}

	// The two OLD reserves — identical to the projection, opposite verdicts.
	// This pair is the whole point: dropping the 250k-ledger window retired a
	// capture-window caveat AND removed an accidental staleness bound, and the
	// fix must land on the right side of both.
	//
	// Quiet-but-live: ADMITTED. This is the coverage win — a reserve nobody has
	// touched in months is not a dead one, and the old shape reported it absent.
	q, ok := byAsset[quietStr]
	if !ok {
		t.Error("assetQuiet absent from the reader's result — a QUIET reserve is not a dead one; reading the current-state projection is supposed to retire the 250k-ledger capture-window caveat, and the archived-entry drop must not over-reach into live-but-old entries")
	} else if want := new(big.Int).SetUint64(quietBRate); q.Data.BRate == nil || q.Data.BRate.Cmp(want) != 0 {
		t.Errorf("assetQuiet b_rate = %v, want %d", q.Data.BRate, quietBRate)
	}

	// Positively archived: ABSENT. The projection still holds this entry's
	// last-known value and the query returns it — nothing in the SQL can tell
	// it apart from assetQuiet. Only the TTL verdict can, and if it is not
	// consulted the handler prices a dead pool's reserves at today's USD rate
	// into tvl_usd and publishes it with flags.stale=false.
	if got, present := byAsset[archivedStr]; present {
		t.Errorf("assetArchived present in the reader's result (b_rate=%v) — its TTL lapsed at ledger %d against a lake tip of %d, so its last-known reserves are NOT current liquidity. The pre-#504 250k-ledger window dropped it as a side effect of being narrow; removing that window without an explicit archived-entry drop publishes a fabricated TVL for a dead pool",
			got.Data.BRate, liveUntilArchived, maxLedger)
	}

	// (2) read_rows, one measured call down each path.
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyWinners(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)))
	readerID := uuid.NewString()
	if _, err := reader.BlendPoolReserves(clickhouse.Context(ctx, clickhouse.WithQueryID(readerID)), poolStr, blend.PoolV2, assets, nil); err != nil {
		t.Fatalf("BlendPoolReserves (measured): %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)

	readRows := func(id string) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND query_id = ?
			ORDER BY event_time_microseconds DESC LIMIT 1`, t0, id).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", id, err)
		}
		return rr
	}
	legacyRead, readerRead := readRows(legacyID), readRows(readerID)
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d in-window writes to ONE reserve key, %d keys probed)",
		legacyRead, readerRead, churn, len(keys))

	if legacyRead < uint64(churn) {
		t.Fatalf("fixture no longer reproduces the pathology: the legacy shape read %d rows, expected at least the pool's %d in-window writes — the bound below would be vacuous",
			legacyRead, churn)
	}
	// The probe reads granules covering the requested keys, not the pool's
	// write history. Generous by design: what must NOT hold is the two
	// figures tracking each other.
	if readerRead*10 > legacyRead {
		t.Errorf("reader read %d rows vs legacy %d — the reserve lookup must be bounded by the KEY COUNT, not by how often the pool was written to (#504)",
			readerRead, legacyRead)
	}
}
