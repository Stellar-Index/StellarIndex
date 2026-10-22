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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The derive writes the sponsorship lifecycle as a third output of the same pass: a sponsored trustline's
// creation, its transfer to another sponsor and its removal become started, revoked+started and ended rows.
func TestChEntryHistory_WritesSponsorshipEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const base = uint32(160_910_000)
	// Accounts of this test alone: the lake is shared with the entry-changes read tests, whose per-account feeds
	// would otherwise pick up these rows through the index.
	account := func(fill byte) string {
		return strkey.MustEncode(strkey.VersionByteAccountID, []byte(strings.Repeat(string(fill), 32)))
	}
	var (
		alice  = account(0x5a)
		bob    = account(0x5b)
		carol  = account(0x5c)
		issuer = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"
	)
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.entry_history_watermark`); err != nil {
		t.Fatalf("truncate watermark: %v", err)
	}
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 1, true)

	trustline := func(sponsor string) xdr.LedgerEntry {
		e := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
			AccountId: xdr.MustAddress(bob), Asset: xdr.MustNewCreditAsset("EURC", issuer).ToTrustLineAsset(), Balance: 5, Limit: 1_000_000,
		}}}
		if sponsor != "" {
			e.Ext = xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(sponsor)}}
		}
		return e
	}
	row := func(ledger uint32, change uint32, changeType string, e xdr.LedgerEntry, withEntry bool) chstore.LedgerEntryChangeRow {
		t.Helper()
		k, err := e.LedgerKey()
		if err != nil {
			t.Fatalf("ledger key: %v", err)
		}
		r := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), TxHash: "ab" + strconv.FormatUint(uint64(ledger), 10),
			OpIndex: 0, ChangeIndex: change, ChangeType: changeType, EntryType: "trustline", AccountID: bob, Asset: "EURC:" + issuer,
		}
		if r.KeyXDR, err = xdr.MarshalBase64(k); err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		if withEntry {
			if r.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
				t.Fatalf("marshal entry: %v", err)
			}
		}
		return r
	}
	src := []chstore.LedgerEntryChangeRow{
		row(base, 0, "created", trustline(alice), true),
		row(base+1, 0, "state", trustline(alice), true),
		row(base+1, 1, "updated", trustline(carol), true),
		row(base+2, 0, "state", trustline(carol), true),
		row(base+2, 1, "removed", trustline(carol), false),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, src, 0); err != nil {
		t.Fatalf("seed ledger_entry_changes: %v", err)
	}
	args := []string{
		"ch-entry-history", "-ch-addr", addr, "-write",
		"-from", strconv.FormatUint(uint64(base), 10), "-to", strconv.FormatUint(uint64(base+2), 10),
	}
	runEntryIndexWrite(t, addr, base, base+2)
	if err := chops.Run(args); err != nil {
		t.Fatalf("ch-entry-history: %v", err)
	}

	type spRow struct {
		ledger                          uint32
		sponsor, sponsored, kind, keyID string
		reserves                        uint8
		event                           string
	}
	rows, err := conn.Query(ctx, `
		SELECT ledger, sponsor, sponsored, kind, key_id, reserves, event
		FROM stellar.sponsorship_events FINAL
		WHERE ledger BETWEEN ? AND ? ORDER BY ledger, change_index, event`, base, base+2)
	if err != nil {
		t.Fatalf("query sponsorship_events: %v", err)
	}
	var got []spRow
	for rows.Next() {
		var r spRow
		if err := rows.Scan(&r.ledger, &r.sponsor, &r.sponsored, &r.kind, &r.keyID, &r.reserves, &r.event); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	_ = rows.Close()
	eurc := "EURC-" + issuer
	want := []spRow{
		{base, alice, bob, "trustline", eurc, 1, "started"},
		{base + 1, alice, bob, "trustline", eurc, 1, "revoked"},
		{base + 1, carol, bob, "trustline", eurc, 1, "started"},
		{base + 2, carol, bob, "trustline", eurc, 1, "ended"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sponsorship_events = %+v, want %+v", got, want)
	}
	if wm, err := chstore.SponsorshipWatermark(ctx, addr); err != nil || wm != base+2 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+2)
	}

	rd, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("explorer reader: %v", err)
	}
	defer func() { _ = rd.Close() }()
	sponsored := chstore.SponsorshipSideSponsored
	page, err := rd.AccountSponsorships(ctx, bob, sponsored, false, chstore.SponsorshipKey{}, base+2, 10)
	if err != nil {
		t.Fatalf("account sponsorships: %v", err)
	}
	if len(page) != 2 || page[0].Sponsor != alice || page[1].Sponsor != carol || len(page[0].Events) != 2 ||
		len(page[1].Events) != 2 || page[1].Events[1].Event != "ended" || page[0].Reserves != 1 {
		t.Fatalf("sponsorships of bob = %+v, want alice then carol with two events each", page)
	}
	inForce, err := rd.AccountSponsorships(ctx, bob, sponsored, true, chstore.SponsorshipKey{}, base+1, 10)
	if err != nil || len(inForce) != 1 || inForce[0].Sponsor != carol || len(inForce[0].Events) != 1 {
		t.Fatalf("in force at %d = %+v (%v), want carol alone with its start", base+1, inForce, err)
	}
	after := chstore.SponsorshipKey{Sponsor: alice, Sponsored: bob, Kind: "trustline", KeyID: eurc}
	rest, err := rd.AccountSponsorships(ctx, bob, sponsored, false, after, base+2, 10)
	if err != nil || len(rest) != 1 || rest[0].Sponsor != carol {
		t.Fatalf("page after alice = %+v (%v), want carol", rest, err)
	}
	tot, err := rd.SponsorshipTotals(ctx, carol, chstore.SponsorshipSideSponsor, base+1)
	if err != nil || tot != (chstore.SponsorshipTotals{InForce: 1, InForceReserves: 1, Started: 1}) {
		t.Fatalf("carol totals at %d = %+v (%v)", base+1, tot, err)
	}
	tot, err = rd.SponsorshipTotals(ctx, bob, sponsored, base+2)
	if err != nil || tot != (chstore.SponsorshipTotals{Started: 2, Revoked: 1, Ended: 1}) {
		t.Fatalf("bob totals = %+v (%v)", tot, err)
	}
	if err := chops.Run([]string{"ch-entry-history", "-ch-addr", addr, "-proof", bob + "," + alice}); err != nil {
		t.Fatalf("ch-entry-history -proof: %v", err)
	}
}

// entryHistorySeedLedgers writes stellar.ledgers rows declaring txCount transactions each, so the watermark's
// entry-change coverage gate applies, and (withOps) one stellar.operations row per tx-bearing ledger under the
// "ab<ledger>" transaction the entry-change seeds use, so the sponsorship derive's operations-coverage gate passes.
func entryHistorySeedLedgers(t *testing.T, ctx context.Context, addr string, seqs []uint32, txCount uint32, withOps bool) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, seq := range seqs {
		closeTime := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TxCount: txCount, TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if withOps && txCount > 0 {
			ext.Ops = []chstore.OperationRow{{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: "ab" + strconv.FormatUint(uint64(seq), 10),
				OpType: "OperationTypePayment", SourceAccount: "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7",
			}}
		}
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

// runEntryIndexWrite builds the entry index over [from, to]: the ceiling every sponsorship -write is clamped to.
func runEntryIndexWrite(t *testing.T, addr string, from, to uint32) {
	t.Helper()
	err := chops.Run([]string{
		"ch-entry-index", "-ch-addr", addr, "-write", "-lock-file", filepath.Join(t.TempDir(), "entry-index.lock"),
		"-from", strconv.FormatUint(uint64(from), 10), "-to", strconv.FormatUint(uint64(to), 10),
	})
	if err != nil {
		t.Fatalf("ch-entry-index -write [%d,%d]: %v", from, to, err)
	}
}

// -write never carries sponsorship events past the entry index the account route proves them against: the run clamps
// its range to the entry_index watermark, and the watermark advance itself refuses a window the index does not cover.
func TestChEntryHistory_RefusesAheadOfEntryIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const base = uint32(160_930_000)
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.entry_history_watermark`); err != nil {
		t.Fatalf("truncate watermark: %v", err)
	}
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 0, true)

	if err := runEntryHistoryWrite(addr, base, base+2); err != nil {
		t.Fatalf("ch-entry-history -write with no entry index: %v, want a no-op", err)
	}
	if wm, err := chstore.SponsorshipWatermark(ctx, addr); err != nil || wm != 0 {
		t.Fatalf("watermark = %d (%v) with no entry index, want 0", wm, err)
	}
	err := chstore.SetSponsorshipWatermark(ctx, addr, base, base+2)
	if !errors.Is(err, chstore.ErrSponsorshipAheadOfEntryIndex) {
		t.Fatalf("SetSponsorshipWatermark [%d,%d] with no entry index: err = %v, want ErrSponsorshipAheadOfEntryIndex", base, base+2, err)
	}
	if wm, err := chstore.SponsorshipWatermark(ctx, addr); err != nil || wm != 0 {
		t.Fatalf("watermark = %d (%v) after a refused advance, want 0", wm, err)
	}
}

// A window whose stellar.operations rows cover fewer ledgers than stellar.ledgers declares tx-bearing (a changes-only
// backfill range) is refused before any row is read: the candidate read would silently miss signer transitions.
func TestChEntryHistory_RefusesOperationsShortfall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const base = uint32(160_940_000)
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 1, false)

	err := chops.Run([]string{
		"ch-entry-history", "-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10), "-to", strconv.FormatUint(uint64(base+2), 10),
	})
	if !errors.Is(err, chstore.ErrSponsorshipOperationsShortfall) {
		t.Fatalf("ch-entry-history over tx-bearing ledgers without operations rows: err = %v, want ErrSponsorshipOperationsShortfall", err)
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
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 0, true)
	runEntryIndexWrite(t, addr, base, base+2)

	if err := runEntryHistoryWrite(addr, base, base); err != nil {
		t.Fatalf("ch-entry-history -write [%d,%d]: %v", base, base, err)
	}
	if wm, err := chstore.SponsorshipWatermark(ctx, addr); err != nil || wm != base {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base)
	}

	err := runEntryHistoryWrite(addr, base+2, base+2)
	if !errors.Is(err, chstore.ErrEntryHistorySkippedPrefix) {
		t.Fatalf("ch-entry-history -write skipping %d: err = %v, want ErrEntryHistorySkippedPrefix", base+1, err)
	}
	if wm, err := chstore.SponsorshipWatermark(ctx, addr); err != nil || wm != base {
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

// Every ALTER-bearing deploy/clickhouse/*.sql artifact (the hand-applied upgrade path) must land on the same columns and
// skip indexes as tier1_schema.sql's CREATE TABLE. Unconditional ALTERs run against the fresh schema and must change
// nothing; `IF NOT EXISTS` ALTERs are replayed on a stripped structural copy and must rebuild it.
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

// Replays each all-`IF NOT EXISTS` ALTER against a `CREATE TABLE ... AS` copy (no MV pinning columns) with the statement's
// objects dropped, and requires the copy to match the table again. Fails on any clause it does not model.
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

// Returns the DROP clauses that undo body's `ADD ... IF NOT EXISTS` clauses, indexes first so a column's index never
// blocks its drop. nil for unconditional statements (the fresh-schema pass ran those); fails on a mix.
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

// Serves the asset entry-change feed end to end: lake rows seeded for one asset, the index built by ch-entry-index,
// then GET /v1/assets/{asset_id}/entry-changes walked through its cursor, and the holder's own account feed from the
// same rows. A claimable balance that changes in a ledger where a trustline of the asset also changed is served too.
func TestClickHouseAssetEntryChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	// An isolated ledger range nothing else in the suite writes to.
	const base = uint32(161_200_000)
	const (
		holder = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
		other  = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
		issuer = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
		asset  = "AECT-" + issuer
	)
	// The entry-index watermark must start empty: the builder refuses a window that starts above watermark+1, and the
	// index test later in this file asserts the watermark it leaves.
	resetWatermark := func() { _ = raw.Exec(context.Background(), `TRUNCATE TABLE stellar.entry_history_watermark`) }
	resetWatermark()
	t.Cleanup(func() {
		bg := context.Background()
		_ = raw.Exec(bg, `DELETE FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN ? AND ? SETTINGS mutations_sync = 2`, base, base+2)
		for _, table := range []string{"account_entry_ledgers", "asset_entry_ledgers"} {
			_ = raw.Exec(bg, `DELETE FROM stellar.`+table+` WHERE ledger BETWEEN ? AND ? SETTINGS mutations_sync = 2`, base, base+2)
		}
		resetWatermark()
	})
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 1, true)

	trustline := func(balance int64) xdr.LedgerEntry {
		return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
			AccountId: xdr.MustAddress(holder), Asset: xdr.MustNewCreditAsset("AECT", issuer).ToTrustLineAsset(),
			Balance: xdr.Int64(balance), Limit: 1_000_000,
		}}}
	}
	claimable := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{3}},
		Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(other), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}}},
		Asset: xdr.MustNewCreditAsset("AECT", issuer), Amount: 70,
	}}}
	row := func(ledger, seq uint32, changeType, entryType, owner, assetID string, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
		t.Helper()
		k, err := e.LedgerKey()
		if err != nil {
			t.Fatalf("ledger key: %v", err)
		}
		r := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC), TxHash: "ae" + strconv.FormatUint(uint64(ledger), 10),
			OpIndex: 0, ChangeIndex: seq, IntraLedgerSeq: seq, ChangeType: changeType, EntryType: entryType, AccountID: owner, Asset: assetID,
		}
		if r.KeyXDR, err = xdr.MarshalBase64(k); err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		if r.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		return r
	}
	// The watermark proof wants a change in every tx-bearing ledger of the window; base+2 carries only an unrelated account.
	account := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
		AccountId: xdr.MustAddress(other), Balance: 500, SeqNum: 9, Thresholds: xdr.Thresholds{1, 0, 0, 0},
	}}}
	src := []chstore.LedgerEntryChangeRow{
		row(base, 0, "created", "trustline", holder, asset, trustline(10)),
		row(base+1, 3, "updated", "trustline", holder, asset, trustline(25)),
		row(base+1, 4, "created", "claimable_balance", "", "", claimable),
		row(base+2, 0, "updated", "account", other, "", account),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, src, 0); err != nil {
		t.Fatalf("seed ledger_entry_changes: %v", err)
	}
	if err := chops.Run([]string{
		"ch-entry-index", "-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10), "-to", strconv.FormatUint(uint64(base+2), 10), "-write",
	}); err != nil {
		t.Fatalf("ch-entry-index: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)

	type change struct {
		Ledger         uint32 `json:"ledger"`
		IntraLedgerSeq uint32 `json:"intra_ledger_seq"`
		Role           string `json:"role"`
		EntryType      string `json:"entry_type"`
		ChangeType     string `json:"change_type"`
		Account        string `json:"account"`
		Amount         string `json:"amount"`
	}
	type page struct {
		Data struct {
			Asset         string   `json:"asset"`
			Changes       []change `json:"changes"`
			NextCursor    string   `json:"next_cursor"`
			ThroughLedger uint32   `json:"through_ledger"`
			LowerBound    bool     `json:"lower_bound"`
			CoverageNote  string   `json:"coverage_note"`
		} `json:"data"`
	}
	get := func(url string) page {
		t.Helper()
		resp, err := http.Get(ts.URL + url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out page
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d decode %v", url, resp.StatusCode, err)
		}
		return out
	}

	// Walk the asset feed two changes at a time; the cursor must cross the ledger boundary and end cleanly.
	first := get("/v1/assets/AECT:" + issuer + "/entry-changes?limit=2")
	d := first.Data
	if d.Asset != asset || !d.LowerBound || d.ThroughLedger != base+2 || !strings.Contains(d.CoverageNote, "offers are not served") {
		t.Fatalf("asset page = %+v, want %s through %d as a lower bound", d, asset, base+2)
	}
	all := append([]change(nil), d.Changes...)
	for cursor, hops := d.NextCursor, 0; cursor != ""; hops++ {
		if hops > 3 {
			t.Fatalf("cursor walk did not end: %d hops", hops)
		}
		next := get("/v1/assets/AECT:" + issuer + "/entry-changes?limit=2&cursor=" + cursor)
		all = append(all, next.Data.Changes...)
		cursor = next.Data.NextCursor
	}
	want := []change{
		{Ledger: base + 1, IntraLedgerSeq: 4, Role: "claimable", EntryType: "claimable_balance", ChangeType: "created", Amount: "70"},
		{Ledger: base + 1, IntraLedgerSeq: 3, Role: "holder", EntryType: "trustline", ChangeType: "updated", Account: holder, Amount: "25"},
		{Ledger: base, IntraLedgerSeq: 0, Role: "holder", EntryType: "trustline", ChangeType: "created", Account: holder, Amount: "10"},
	}
	if !slices.Equal(all, want) {
		t.Fatalf("asset feed = %+v, want %+v", all, want)
	}

	// The holder's own feed is located through the account index built from the same rows.
	acct := get("/v1/accounts/" + holder + "/entry-changes?limit=10")
	if n := len(acct.Data.Changes); n != 2 || acct.Data.Changes[0].Ledger != base+1 || acct.Data.Changes[1].Ledger != base ||
		acct.Data.Changes[0].Role != "owner" || acct.Data.Changes[0].EntryType != "trustline" {
		t.Fatalf("account feed = %+v, want the two trustline changes of %s as owner, newest first", acct.Data.Changes, holder)
	}
}

// When one storage key changes twice in the SAME ledger (update then remove), ledger_entries_current FINAL must resolve to
// the LAST change (the removal), never the before-image. Rows are inserted in the ADVERSARIAL order [removed, updated]:
func TestLedgerEntriesCurrent_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		key    = "c24c-update-then-remove-key"
		ledger = uint32(70_000_000)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Canonical ledger order: update (intra 5) before removal (intra 6). Rows are written in REVERSE so a ledger_seq-only
	// version would keep the wrong one.
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

// Lake reader behind `supply seed-claimable-balances`. Only a real server proves the SQL: PREWHERE on entry_type, one
// argMax over a TUPLE of every projected column, and its within-ledger ordering. Assertions are scoped to each test's OWN
// ids since the reader is network-wide;

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
	// The most windows a seed walk over the SHARED lake may take: 2,000 (500M-ledger span, ~8x mainnet's tip and over twice
	// the suite's highest legitimate fixture, 217M). At ~3.5 ms per empty window that is ~7 s.
	cbsMaxWalkWindows = uint64(2_000)
	// Wall-clock ceiling for ONE walk of a bounded lake: ~4x the worst walk cbsMaxWalkWindows admits, so load alone cannot
	// breach it, and well under the 53 s a walk took with the 4e9 fixture present.
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

// Reports how many initial-width windows a seed walk would take now, and names the row holding the range top so an over-
// long walk traces to its fixture. Window count, not elapsed time, is asserted: it is exact and load-independent.
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

// Headline case: a claimable balance created long before the live observer existed and never claimed is recovered from the
// append-log with exact asset, amount and TRUE last-modified ledger; otherwise it would be missing from
// claimable_observations and under-read AQUA's supply.
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

// A balance claimed in a LATER window must not be seeded (cross-window reduction: removal and creation come from separate
// server-side queries reconciled in Go).
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

// Same-ledger tie-break on the real server: a balance created AND claimed in one ledger is ordinary, so ledger_seq cannot
// order the changes. Independent per-column argMax can take entry_xdr from the live change and change_type from the
// removal, so the removed-entry skip never fires and a claimed balance is resurrected;
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
			// Different tx, LATER in the ledger's canonical walk. Only intra_ledger_seq ranks it correctly (tx_hash "cbs03b" >
			// "cbs03a" would also pass a lexical tie-break), so the intra_ledger_seq gap makes this true by construction.
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

// Native (XLM) claimable balances are never seeded (Algorithm 1 does not read claimable_observations and the live observer
// declines them); -assets scoping filters classic ones; the nil scope includes every classic asset.
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

// Regression for walk cost: a fixture another test leaves at a far-future ledger is paid for by every later seed walk. It
// RUNS TestBlendPoolReserves_SameLedgerLastChangeWins (4e9) and ..._CurrentStateProjectionBoundsTheRead (3,999,900,000+)
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

// Both SAC seed readers: a watched Balance entry whose TTL lapsed before the lake tip is an archived (evicted,
// restorable) entry. It stays served at its last write, the policy the live sac_balances observer applies to evicted
// keys, so neither reader may turn it into a tombstone.
func TestSACSeed_ArchivedHolderStaysServed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		lastWrite = uint32(31_100_000)
		liveUntil = uint32(31_200_000)
		tipLedger = uint32(31_300_000)
		asset     = "ARCH:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	lastWriteClose := time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)

	sac, sacContract := fhSyntheticContractAddr(t, 0xE5)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xE6)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: lastWrite, CloseTime: lastWriteClose,
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

	watched := map[string]string{sac: asset}
	for name, stream := range sacSeedReaders() {
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
			t.Fatalf("%s: emitted %d seeds for the archived holder, want 1 live balance: %+v", name, len(got), got)
		}
		s := got[0]
		if s.IsRemoval || s.Balance.Cmp(big.NewInt(5_000_000)) != 0 {
			t.Errorf("%s: emitted %+v, want the last written balance 5000000 and no tombstone", name, s)
		}
		if s.LedgerSeq != lastWrite || !s.CloseTime.Equal(lastWriteClose) {
			t.Errorf("%s: emitted at ledger %d / %v, want the last write %d / %v", name, s.LedgerSeq, s.CloseTime, lastWrite, lastWriteClose)
		}
	}
}

// TestSACSeed_UncoveredTTLStillEmits: TTL coverage is not a precondition of the seed. A watched Balance entry with no
// stellar.ttl_live_until row is emitted as a live holder by both readers, with no coverage error.
func TestSACSeed_UncoveredTTLStillEmits(t *testing.T) {
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
	for name, stream := range sacSeedReaders() {
		var got []chstore.SACBalanceSeed
		err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				got = append(got, s)
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: err = %v, want nil (TTL coverage is not a seed precondition)", name, err)
		}
		if len(got) != 1 || got[0].IsRemoval || got[0].Balance.Cmp(big.NewInt(9_000_000)) != 0 {
			t.Errorf("%s: emitted %+v, want one live 9000000 balance for the uncovered holder", name, got)
		}
	}
}

// sacSeedReaders names both SAC seed readers for the policy tests above.
func sacSeedReaders() map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error {
	return map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  sacFullHistoryUnverified,
	}
}

// sacFullHistoryUnverified is the full-history reader over the synthetic test
// lake, which carries stellar.ledgers rows only where a fixture needs them.
func sacFullHistoryUnverified(ctx context.Context, addr string, watched map[string]string, fn func(chstore.SACBalanceSeed) error) error {
	_, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, fn)
	return err
}

// Reproduces the PHO/BLND shape (docs/architecture/supply-pipeline.md, dormant contract-held SAC balances): a pool
// contract's Balance entry last written before ledger_entries_current's ~62M coverage floor. The row goes into
// ledger_entry_changes and its MV mirror is then deleted, giving the floor's end state.
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
	// A structurally valid stand-in for the dormant pool holder: what matters is that it is a CONTRACT address (not a
	// G-account) holding the SAC Balance(Address) entry, the incident's shape.
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
	// The live ledger_entries_current_mv mirrors the inserted row (a fresh schema has the MV before any insert, unlike r1
	// where it came after ~62M of backfilled history). Delete the mirror synchronously to reproduce the floor's end state.
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

// The server-side `ORDER BY key_xdr, ledger_seq DESC LIMIT 1 BY key_xdr` reduction must pick the HIGHEST-ledger write per
// key, matching ledger_entries_current's ReplacingMergeTree(ledger_seq), over a raw append-log that can hold several
// writes to one key (ch-backfill re-derive, live capture).
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

// A key both present and removed in the SAME ledger must resolve to the single latest change coherently (every column from
// one row) so the removed-entry skip fires and the balance is not resurrected. change_index is only a per- transaction
// counter, so intra-ledger order is (op_index, change_index); independent argMax(col, ledger_seq)
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
	// A retraction tombstone (IsRemoval=true, Balance=0) is the CORRECT outcome: the removal is the latest intra-ledger state.
	// Only a live nonzero-balance emission would mean the deleted balance was resurrected.
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

// Positive complement: removed early then RE-CREATED later in the same ledger, the holder IS emitted with the re-created
// balance (latest intra-ledger change wins, resolved by op_index within the tied ledger_seq).
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

// Synchronously deletes the row matching keyXDR from ledger_entries_current to reproduce the coverage floor's end state
// (row in ledger_entry_changes, absent from the projection). mutations_sync=2 blocks until the row is gone.
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

// Builds a deterministic, checksum-valid C-strkey and matching xdr.ScAddress from a tag byte (via strkey.Encode) for
// stand-in pool/holder addresses whose identity does not matter.
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

// Coverage leg on both full-history seed readers: a walk asked to verify the lake must refuse, before emitting anything, a
// range whose stellar.ledgers rows are missing (the shared lake has entry changes at ledgers with no ledgers row, so the
// hole is real). Without it both emit the holder/balance as current state, letting a LiveSink drop elect a pre-hole value.
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

// TrustlineAssetsAfter's pool-share exclusion must match only the two spellings TrustLineAssetID emits ("pool:<hex>" and
// bare "pool"), never a real credit asset that merely starts with "pool" ("poolX" is a valid, case-sensitive code). `NOT
// startsWith(asset, 'pool')` would silently drop "poolX-GISSUER..."; this test goes RED on it.
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

	// "ooo" sorts after every uppercase-coded asset other tests seed but before "pool", "pool:..." and "poolX...", so the page
	// captures all three fixture rows regardless of unrelated rows in the shared container.
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

// legacyBlendReservesSQL is the pre-rewrite BlendPoolReserves lookup (a 250,000-ledger windowed fold over
// ledger_entry_changes), frozen as a differential oracle: the rewrite must agree row for row on every reserve the window
// could see, without reading one granule per pool WRITE in that window.
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

// Live proof for /v1/lending/pools/{pool}/reserves (503 at 12.1s on the largest pool). The legacy fold read every rewrite
// of a reserve's ResData key in a 250k-ledger window, so cost tracked pool write activity; the fix reads
// ledger_entries_current (sort key (entry_type, key_xdr)).
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

		// Far above every other fixture, so this test's top (base+churn) is the lake's max(ledger_seq) and the frozen oracle's
		// 250k window covers the fixture in any shard and order. The other fixture up here (4e9) is within 250k above base and
		// removes its rows when done; asserted below, not assumed.
		base  = uint32(3_999_900_000)
		churn = 3_000
		// Far below the window: the legacy shape cannot see these two.
		quietLedger = uint32(1_000_000)

		// TTL verdicts are judged against max(ledger_seq) at compute time, which the suite shares. These two sit far either side
		// of any tip this test can run under (base+churn, or 4e9 while the argmax fixture is live), so verdicts hold in any order.
		liveUntilLive     = uint32(4_294_000_000)
		liveUntilArchived = uint32(1_500_000_000)

		staleBRate    = uint64(1_000_000_000_000) // 1.0 at 12 decimals
		finalBRate    = uint64(2_000_000_000_000) // 2.0 — the winning write
		quietBRate    = uint64(3_000_000_000_000) // 3.0
		archivedBRate = uint64(4_000_000_000_000) // 4.0 — must never be served
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Everything seeded at [base, base+churn] is removed when the test finishes: rows this high are max(ledger_seq), which
	// would cost each later whole-lake seed walk ~16,000 empty windows. The quiet + archived rows at quietLedger stay: they
	// are below any realistic tip and bound nothing.
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
		// The TTL entries that separate quiet from archived: ttlChangeRow renders the lake's TTL change for a governed key and the
		// ttl_live_until_mv view turns it into the projection ClassifyTTLLiveness reads.
		ttlChangeRow(quietKey, base+1, 1, liveUntilLive, 48),
		ttlChangeRow(archivedKey, base+1, 1, liveUntilArchived, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Collapse ledger_entries_current to its steady state: production merges keep ~one row per live key, but a fresh container
	// holds every version in unmerged parts, so read_rows would measure merge lag, not query shape. FINAL gives the same
	// answer.
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

	// The two OLD reserves: same projection, opposite verdicts. Dropping the 250k window retired a capture-window caveat AND
	// an accidental staleness bound. Quiet-but-live is ADMITTED (untouched for months is not dead; the old shape reported it
	// absent).
	q, ok := byAsset[quietStr]
	if !ok {
		t.Error("assetQuiet absent from the reader's result — a QUIET reserve is not a dead one; reading the current-state projection is supposed to retire the 250k-ledger capture-window caveat, and the archived-entry drop must not over-reach into live-but-old entries")
	} else if want := new(big.Int).SetUint64(quietBRate); q.Data.BRate == nil || q.Data.BRate.Cmp(want) != 0 {
		t.Errorf("assetQuiet b_rate = %v, want %d", q.Data.BRate, quietBRate)
	}

	// Positively archived: ABSENT. The projection still holds its last value and nothing in the SQL distinguishes it from
	// assetQuiet; only the TTL verdict does. Skipping it would price a dead pool's reserves into tvl_usd with
	// flags.stale=false.
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

// Drives `ch-entry-index` via chops.Run on the real tier-1 DDL: a dry run writes nothing; -write fills both index tables
// from one window, including the sponsor row found through the SQL base64-tail predicate, and advances the entry_index
// watermark.
func TestChEntryIndex_WriteFillsBothIndexes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// An isolated ledger range nothing else in the suite writes to.
	const base = uint32(161_100_000)
	const (
		alice  = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
		bob    = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
		issuer = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
		usdc   = "USDC-" + issuer
	)
	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2}, 1, true)

	account := func(id string, balance, seq int64) xdr.LedgerEntry {
		return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
			AccountId: xdr.MustAddress(id), Balance: xdr.Int64(balance), SeqNum: xdr.SequenceNumber(seq),
			Thresholds: xdr.Thresholds{1, 0, 0, 0},
		}}}
	}
	trustline := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(bob), Asset: xdr.MustNewCreditAsset("USDC", issuer).ToTrustLineAsset(),
		Balance: 10, Limit: 1_000_000,
	}}}
	claimable := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{2}},
		Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(alice), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}}},
		Asset: xdr.MustNewCreditAsset("USDC", issuer), Amount: 70,
	}}}
	sponsored := account(bob, 50, 1)
	sponsored.Ext = xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(alice)}}

	row := func(ledger uint32, change uint32, changeType, entryType, owner, asset string, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
		t.Helper()
		k, err := e.LedgerKey()
		if err != nil {
			t.Fatalf("ledger key: %v", err)
		}
		r := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC), TxHash: "ei" + strconv.FormatUint(uint64(ledger), 10),
			OpIndex: 0, ChangeIndex: change, ChangeType: changeType, EntryType: entryType, AccountID: owner, Asset: asset,
		}
		if r.KeyXDR, err = xdr.MarshalBase64(k); err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		if r.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		return r
	}
	src := []chstore.LedgerEntryChangeRow{
		// base: alice's account changes (two changes, one owner row).
		row(base, 0, "state", "account", alice, "", account(alice, 1000, 7)),
		row(base, 1, "updated", "account", alice, "", account(alice, 900, 8)),
		// base+1: bob opens a USDC trustline.
		row(base+1, 0, "created", "trustline", bob, usdc, trustline),
		// base+2: a claimable balance for alice, and bob's account sponsored by alice.
		row(base+2, 0, "created", "claimable_balance", "", "", claimable),
		row(base+2, 1, "updated", "account", bob, "", sponsored),
	}
	// base+2: an earlier claimable balance is claimed; the removal row carries its key and no entry.
	claimed := claimable
	claimed.Data.ClaimableBalance = &xdr.ClaimableBalanceEntry{BalanceId: xdr.ClaimableBalanceId{
		Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{3},
	}, Asset: xdr.MustNewNativeAsset(), Amount: 5}
	removal := row(base+2, 2, "removed", "claimable_balance", "", "", claimed)
	removal.EntryXDR = ""
	src = append(src, removal)
	if _, err := chstore.InsertEntryChanges(ctx, addr, src, 0); err != nil {
		t.Fatalf("seed ledger_entry_changes: %v", err)
	}

	run := func(write bool) {
		t.Helper()
		args := []string{
			"ch-entry-index", "-ch-addr", addr,
			"-from", strconv.FormatUint(uint64(base), 10), "-to", strconv.FormatUint(uint64(base+2), 10),
		}
		if write {
			args = append(args, "-write")
		}
		if err := chops.Run(args); err != nil {
			t.Fatalf("ch-entry-index (write=%v): %v", write, err)
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
	if a, s := count("account_entry_ledgers"), count("asset_entry_ledgers"); a+s != 0 {
		t.Fatalf("dry run wrote %d account / %d asset rows, want none", a, s)
	}
	run(true)
	run(true) // idempotent: the second window collapses on FINAL
	if a, s := count("account_entry_ledgers"), count("asset_entry_ledgers"); a != 5 || s != 1 {
		t.Fatalf("write run left %d account / %d asset rows, want 5 / 1", a, s)
	}
	type roleRow struct {
		Account string
		Role    string
		Ledger  uint32
	}
	rows, err := conn.Query(ctx, `SELECT account, role, ledger FROM stellar.account_entry_ledgers FINAL WHERE ledger BETWEEN ? AND ? ORDER BY ledger, role, account`, base, base+2)
	if err != nil {
		t.Fatalf("read account index: %v", err)
	}
	defer rows.Close()
	var got []roleRow
	for rows.Next() {
		var r roleRow
		if err := rows.Scan(&r.Account, &r.Role, &r.Ledger); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	raw := func(g string) string {
		t.Helper()
		b, err := strkey.Decode(strkey.VersionByteAccountID, g)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := []roleRow{
		{raw(alice), "owner", base},
		{raw(bob), "owner", base + 1},
		{raw(bob), "owner", base + 2},
		{raw(alice), "claimant", base + 2},
		{raw(alice), "sponsor", base + 2},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("account index rows = %+v, want %+v", got, want)
	}
	var created, updated, removed uint32
	if err := conn.QueryRow(ctx, `SELECT n_created, n_updated, n_removed FROM stellar.asset_entry_ledgers FINAL WHERE asset = ? AND ledger = ?`, usdc, base+1).Scan(&created, &updated, &removed); err != nil {
		t.Fatalf("read asset index: %v", err)
	}
	if created != 1 || updated != 0 || removed != 0 {
		t.Fatalf("asset counts = %d/%d/%d, want 1/0/0", created, updated, removed)
	}
	createdKey := src[3].KeyXDR
	var claimKey, claimEntry string
	var claimLedger uint32
	if err := conn.QueryRow(ctx, `SELECT key_xdr, ledger, entry_xdr FROM stellar.account_claimables FINAL WHERE account = ?`, raw(alice)).Scan(&claimKey, &claimLedger, &claimEntry); err != nil {
		t.Fatalf("read account_claimables: %v", err)
	}
	if claimKey != createdKey || claimLedger != base+2 || claimEntry != src[3].EntryXDR {
		t.Fatalf("account_claimables row = (%s, %d), want alice's balance %s at %d with its entry", claimKey, claimLedger, createdKey, base+2)
	}
	var removedAt uint32
	if err := conn.QueryRow(ctx, `SELECT ledger FROM stellar.claimable_removals FINAL WHERE key_xdr = ?`, removal.KeyXDR).Scan(&removedAt); err != nil {
		t.Fatalf("read claimable_removals: %v", err)
	}
	if removedAt != base+2 {
		t.Fatalf("claimable_removals ledger = %d, want %d", removedAt, base+2)
	}
	// alice once held the claimed balance too; the reader must drop it through claimable_removals.
	if err := conn.Exec(ctx, `INSERT INTO stellar.account_claimables (account, key_xdr, ledger, close_time, entry_xdr) VALUES (?, ?, ?, ?, '')`,
		raw(alice), removal.KeyXDR, base+1, time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("seed claimed balance: %v", err)
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	scan, err := er.IndexedClaimableBalances(ctx, raw(alice), 10)
	if err != nil {
		t.Fatalf("IndexedClaimableBalances: %v", err)
	}
	if scan.Keys != 1 || scan.Live[0].Ledger != base+2 || !slices.Contains(scan.Live[0].Claimants, alice) {
		t.Fatalf("indexed claimables = %+v, want only alice's live balance at %d", scan.Live, base+2)
	}
	wm, err := chstore.EntryIndexWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if wm != base+2 {
		t.Fatalf("entry_index watermark = %d, want %d", wm, base+2)
	}
}

// Both axes of the fix: update-then-update in one ledger must serve the final b_rate, and update-then-remove must drop the
// key (the removal participates and is dropped via HAVING on the winning change_type, instead of the earlier update being
// resurrected). The reader gets both from ledger_entries_current FINAL;
func TestBlendPoolReserves_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A very high ledger so the fixture sits at the lake's tip, but it must NOT outlive the test: max(ledger_seq) bounds every
	// whole-lake walker, so a surviving row costs each walk ~16,000 empty windows and breaches the 5-minute deadline under
	// load. purgeLakeFixtureLedgers removes it synchronously.
	const ledger = uint32(4_000_000_000)
	purgeLakeFixtureLedgers(t, addr, ledger, ledger)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	const poolSeed, assetValSeed, assetGoneSeed = byte(0xB1), byte(0xB2), byte(0xB3)
	pool := contractIDFromSeed(poolSeed)
	assetVal := contractIDFromSeed(assetValSeed)
	assetGone := contractIDFromSeed(assetGoneSeed)
	poolStr := mustContractStrkey(t, poolSeed)
	assetValStr := mustContractStrkey(t, assetValSeed)
	assetGoneStr := mustContractStrkey(t, assetGoneSeed)

	const (
		staleBRate = uint64(1_000_000_000_000) // 1.0 at 12 decimals — a mid-ledger rate
		finalBRate = uint64(2_000_000_000_000) // 2.0 — the last change in the ledger
	)

	rows := []chstore.LedgerEntryChangeRow{
		// reserveVal: LATER update (op 1, intra 9) — the final b_rate. Written
		// first (adversarial); sorts last.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendval", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetVal), EntryXDR: resDataEntryB64(t, pool, assetVal, ledger, finalBRate),
		},
		// reserveVal: EARLIER update (op 0, intra 8) — a stale b_rate. Sorts
		// first, so a ledger_seq-only argMax keeps it.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendval", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetVal), EntryXDR: resDataEntryB64(t, pool, assetVal, ledger, staleBRate),
		},
		// reserveGone: the removal (op 1, intra 9) — the LAST change; the key must
		// drop out.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendgone", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "removed", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetGone), EntryXDR: "",
		},
		// reserveGone: the earlier update (op 0, intra 8) — a live entry the
		// naive `entry_xdr != ''` filter would resurrect.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendgone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetGone), EntryXDR: resDataEntryB64(t, pool, assetGone, ledger, staleBRate),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	states, err := reader.BlendPoolReserves(ctx, poolStr, blend.PoolV2, []string{assetValStr, assetGoneStr}, nil)
	if err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}

	byAsset := make(map[string]chstore.BlendReserveState, len(states))
	for _, s := range states {
		byAsset[s.Asset] = s
	}

	got, ok := byAsset[assetValStr]
	if !ok {
		t.Fatalf("reserveVal absent from result; want present with the final b_rate")
	}
	if want := new(big.Int).SetUint64(finalBRate); got.Data.BRate == nil || got.Data.BRate.Cmp(want) != 0 {
		t.Errorf("reserveVal b_rate = %v, want %d (same-ledger tie resolved to a stale mid-ledger reserve state)", got.Data.BRate, finalBRate)
	}
	if _, present := byAsset[assetGoneStr]; present {
		t.Errorf("reserveGone present in result; want ABSENT (its last same-ledger change was a removal — the pre-fix query resurrected it)")
	}
}

// End-to-end: a claimable balance seeded live, then claimed while the observer wasn't recording, must stop counting
// toward classic supply after a re-seed. A seed that only added rows would sum it forever.
func TestClaimableSeed_RetractsServedClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	claimed, kept := cbsID(t, 0x61), cbsID(t, 0x62)
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	claimedAt := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	// The served tier as an earlier (add-only) seed left it: both live.
	for _, id := range [][32]byte{claimed, kept} {
		if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
			ClaimableID: cbsHex(id), AssetKey: cbsAssetKey, Ledger: 34_000_000, ObservedAt: created,
			Balance: big.NewInt(999), IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
		}); err != nil {
			t.Fatalf("InsertClaimableObservation: %v", err)
		}
	}
	// A removed row for an id nobody serves as live must not be listed.
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: cbsHex(cbsID(t, 0x63)), AssetKey: cbsAssetKey, Ledger: 35_000_000, ObservedAt: created,
		Balance: big.NewInt(0), IsRemoval: true,
	}); err != nil {
		t.Fatalf("InsertClaimableObservation (removal): %v", err)
	}

	served, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations: %v", err)
	}
	if len(served) != 2 || served[cbsHex(claimed)] != (timescale.LiveClaimable{AssetKey: cbsAssetKey, Ledger: 34_000_000}) {
		t.Fatalf("served live set = %+v, want exactly the two live balances at 34000000", served)
	}

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr61a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
			EntryXDR: cbsEntryXDR(t, claimed, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			LedgerSeq: 48_000_000, CloseTime: claimedAt, TxHash: "cbr61b", IntraLedgerSeq: 1,
			ChangeType: "removed", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
		},
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr62a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, kept),
			EntryXDR: cbsEntryXDR(t, kept, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	servedKeys := map[string]string{}
	for id, row := range served {
		servedKeys[id] = row.AssetKey
	}
	var rows []timescale.ClaimableObservation
	if _, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, servedKeys, chstore.SeedWalk{}, func(s chstore.ClaimableBalanceSeed) error {
		if _, ours := served[s.ClaimableID]; ours {
			rows = append(rows, timescale.ClaimableObservation{
				ClaimableID: s.ClaimableID, AssetKey: s.AssetKey, Ledger: s.LedgerSeq, ObservedAt: s.CloseTime,
				Balance: s.Balance, IsRemoval: s.IsRemoval, IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamClaimableBalanceSeeds: %v", err)
	}
	if err := store.InsertClaimableObservationBatch(ctx, rows); err != nil {
		t.Fatalf("InsertClaimableObservationBatch: %v", err)
	}

	sum, err := store.SumClaimableBalancesAtOrBefore(ctx, cbsAssetKey, 50_000_000)
	if err != nil {
		t.Fatalf("SumClaimableBalancesAtOrBefore: %v", err)
	}
	if sum.Cmp(big.NewInt(999)) != 0 {
		t.Errorf("served claimable sum after re-seed = %s, want 999 (the claimed balance retracted, the live one kept)", sum)
	}
	after, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations (after): %v", err)
	}
	if _, still := after[cbsHex(claimed)]; still || len(after) != 1 {
		t.Errorf("served live set after re-seed = %+v, want only the unclaimed balance", after)
	}
}
