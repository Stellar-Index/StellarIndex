//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Runs the operator runbook SQL of deploy/clickhouse/account_activity.sql (Step-2 backfill, Step-3 verify) on ClickHouse.
// A skipped backfill window silently hides history behind the readers' hard watermark bound, and earlier verify shapes
// were blind to a later-window gap: an account active in both windows (later only as NON-SOURCE participant)
func TestClickHouseAccountActivityBackfillVerifySeesGapInLaterWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	rb := loadAccountActivityRunbook(t)

	// Two adjacent windows of the runbook's own grid, far from other tests' ledgers. Rows go straight into the three source
	// tables with no stellar.ledgers row, so no other test's tip anchor moves.
	const (
		win1 = uint32(120_000_002)
		win2 = uint32(122_000_002)
		l1   = win1 + 500
		l2   = win2 + 700
	)
	sampled := f112Accounts(t, ctx, conn, rb.sampleMod, true, 3)
	both, late, early := sampled[0], sampled[1], sampled[2]
	unsampled := f112Accounts(t, ctx, conn, rb.sampleMod, false, 1)[0]

	seedF112Ledger(t, ctx, conn, l1, both, nil)
	seedF112Ledger(t, ctx, conn, l1+1, early, nil)
	seedF112Ledger(t, ctx, conn, l2, late, []string{both})
	seedF112Ledger(t, ctx, conn, l2+1, unsampled, nil)

	// The live MVs just covered every fixture account: both windows clean.
	// (Also the isolation check — a stray row in these windows fails here.)
	for _, w := range []uint32{win1, win2} {
		if n := rb.verify(t, ctx, conn, w, rb.sampleMod); n != 0 {
			t.Fatalf("MV-covered fixture: verify(window %d) = %d, want 0", w, n)
		}
	}

	// Pre-MV history: the source rows exist, the watermark rows do not.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": "2"}))
	if err := conn.Exec(syncCtx,
		`ALTER TABLE stellar.account_activity DELETE WHERE account_id IN (?, ?, ?, ?)`,
		both, late, early, unsampled); err != nil {
		t.Fatalf("drop fixture watermarks: %v", err)
	}

	// Step 2 covers window 1 and SKIPS window 2.
	rb.backfill(t, ctx, conn, win1)

	if n := rb.verify(t, ctx, conn, win1, rb.sampleMod); n != 0 {
		t.Errorf("verify(backfilled window %d) = %d, want 0", win1, n)
	}
	// `both` now has a watermark in window 1, BELOW its window-2 participant
	// row (the reader would hide that row); `late` has none at all.
	if wm := f112Watermark(t, ctx, conn, both); wm != l1 {
		t.Fatalf("fixture: watermark(both) = %d after backfilling window 1 only, want %d", wm, l1)
	}
	if n := rb.verify(t, ctx, conn, win2, rb.sampleMod); n != 2 {
		t.Errorf("verify(SKIPPED window %d) = %d, want 2 (one too-LOW watermark, one missing) — a verify "+
			"that returns 0 here cannot fail for a gap outside the first window (F112)", win2, n)
	}
	// AA_MOD=1 is the exhaustive check: it also counts the unsampled account.
	if n := rb.verify(t, ctx, conn, win2, 1); n != 3 {
		t.Errorf("exhaustive verify(SKIPPED window %d) = %d, want 3", win2, n)
	}

	// Re-run the skipped window: the gap closes and the verify says so.
	rb.backfill(t, ctx, conn, win2)
	for _, mod := range []uint64{rb.sampleMod, 1} {
		if n := rb.verify(t, ctx, conn, win2, mod); n != 0 {
			t.Errorf("verify(window %d, 1-in-%d) = %d after re-running it, want 0", win2, mod, n)
		}
	}
	if wm := f112Watermark(t, ctx, conn, both); wm != l2 {
		t.Errorf("watermark(both) = %d after the full backfill, want %d (its participant row)", wm, l2)
	}
}

// accountActivityRunbook is the executable SQL of the runbook, still carrying
// the shell variables the operator's loop expands.
type accountActivityRunbook struct {
	inserts   []string
	verifySQL string
	sampleMod uint64
}

var (
	f112InsertRE = regexp.MustCompile(`(?s)"\s*(INSERT INTO stellar\.account_activity.*?)"`)
	f112VerifyRE = regexp.MustCompile(`(?s)-q "\s*(SELECT countIf\(wm < truth\).*?)" </dev/null`)
	f112ModRE    = regexp.MustCompile(`AA_MOD="\$\{AA_MOD:-(\d+)\}"`)
)

func loadAccountActivityRunbook(t *testing.T) accountActivityRunbook {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", "account_activity.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "--   "); ok {
			code = append(code, rest)
		}
	}
	text := strings.Join(code, "\n")

	var rb accountActivityRunbook
	for _, m := range f112InsertRE.FindAllStringSubmatch(text, -1) {
		rb.inserts = append(rb.inserts, m[1])
	}
	if len(rb.inserts) != 3 {
		t.Fatalf("runbook Step 2 carries %d backfill INSERTs, want 3 (operations, transactions, "+
			"operation_participants)", len(rb.inserts))
	}
	v := f112VerifyRE.FindAllStringSubmatch(text, -1)
	if len(v) != 1 {
		t.Fatalf("runbook Step 3 carries %d per-window verify queries, want 1 — without one the verify "+
			"cannot fail for a gap in a later window (F112)", len(v))
	}
	rb.verifySQL = v[0][1]
	m := f112ModRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("runbook Step 3 does not declare its default AA_MOD")
	}
	if rb.sampleMod, err = strconv.ParseUint(m[1], 10, 64); err != nil || rb.sampleMod == 0 {
		t.Fatalf("runbook default AA_MOD %q is not a positive integer", m[1])
	}
	return rb
}

// expand does what the operator's bash loop does to the embedded SQL.
func (accountActivityRunbook) expand(t *testing.T, sql string, window uint32, mod uint64) string {
	t.Helper()
	out := strings.NewReplacer(
		"$((W + 2000000))", strconv.FormatUint(uint64(window)+2_000_000, 10),
		"$W", strconv.FormatUint(uint64(window), 10),
		"$AA_MOD", strconv.FormatUint(mod, 10),
	).Replace(sql)
	if strings.Contains(out, "$") {
		t.Fatalf("runbook SQL still carries a shell variable after expansion:\n%s", out)
	}
	return out
}

func (rb accountActivityRunbook) backfill(t *testing.T, ctx context.Context, conn driver.Conn, window uint32) {
	t.Helper()
	for _, ins := range rb.inserts {
		if err := conn.Exec(ctx, rb.expand(t, ins, window, rb.sampleMod)); err != nil {
			t.Fatalf("runbook backfill INSERT (window %d): %v", window, err)
		}
	}
}

func (rb accountActivityRunbook) verify(t *testing.T, ctx context.Context, conn driver.Conn, window uint32, mod uint64) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, rb.expand(t, rb.verifySQL, window, mod)).Scan(&n); err != nil {
		t.Fatalf("runbook verify (window %d): %v", window, err)
	}
	return n
}

// f112Accounts returns n fixture account ids that are inside (or outside)
// the runbook's 1-in-mod hash sample, as ClickHouse itself computes it.
func f112Accounts(t *testing.T, ctx context.Context, conn driver.Conn, mod uint64, inSample bool, n int) []string {
	t.Helper()
	cmp := "="
	if !inSample {
		cmp = "!="
	}
	rows, err := conn.Query(ctx, fmt.Sprintf(
		`SELECT a FROM (SELECT concat('GTESTF112BACKFILLVERIFY', leftPad(toString(number), 33, 'A')) AS a
		                FROM numbers(100000))
		 WHERE cityHash64(a) %% %d %s 0 ORDER BY a LIMIT %d`, mod, cmp, n))
	if err != nil {
		t.Fatalf("pick fixture accounts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan fixture account: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fixture accounts: %v", err)
	}
	if len(out) != n {
		t.Fatalf("picked %d fixture accounts (inSample=%v), want %d", len(out), inSample, n)
	}
	return out
}

// seedF112Ledger writes one transaction + one operation sourced by `source`
// at `seq`, naming `participants` as non-source participants of that op.
func seedF112Ledger(t *testing.T, ctx context.Context, conn driver.Conn, seq uint32, source string, participants []string) {
	t.Helper()
	at := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	hash := fmt.Sprintf("%064d", seq)
	if err := conn.Exec(ctx,
		`INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account) VALUES (?, ?, ?, 0, ?)`,
		seq, at, hash, source); err != nil {
		t.Fatalf("seed transaction %d: %v", seq, err)
	}
	if err := conn.Exec(ctx,
		`INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		 VALUES (?, ?, ?, 0, 0, 'OperationTypePayment', ?, 'Ym9keQ==')`,
		seq, at, hash, source); err != nil {
		t.Fatalf("seed operation %d: %v", seq, err)
	}
	for _, p := range participants {
		if err := conn.Exec(ctx,
			`INSERT INTO stellar.operation_participants (account, ledger_seq, close_time, tx_hash, tx_index, op_index)
			 VALUES (?, ?, ?, ?, 0, 0)`,
			p, seq, at, hash); err != nil {
			t.Fatalf("seed participant %d: %v", seq, err)
		}
	}
}

func f112Watermark(t *testing.T, ctx context.Context, conn driver.Conn, account string) uint32 {
	t.Helper()
	var wm uint32
	if err := conn.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, account).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	return wm
}

// Creator and sponsor boards are ORDER BY rank, so ?account= reads get no primary-key help: both EXCHANGE halves must
// carry the bloom_filter skip index, and on a multi-granule board it must actually drop granules.
func TestAccountBoardKeyedReadsPruneOnTheSkipIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")

	for _, tc := range []struct{ table, col, index string }{
		{"account_creators_rollup", "creator", "idx_creators_rollup_creator"},
		{"account_sponsors_rollup", "sponsor", "idx_sponsors_rollup_sponsor"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			for _, table := range []string{tc.table, tc.table + "_staging"} {
				var idxType, idxExpr string
				if err := conn.QueryRow(ctx, `SELECT type, expr FROM system.data_skipping_indices
					WHERE database = 'stellar' AND table = ? AND name = ?`, table, tc.index).
					Scan(&idxType, &idxExpr); err != nil {
					t.Fatalf("stellar.%s has no skip index %s — the keyed board read full-scans: %v", table, tc.index, err)
				}
				if idxType != "bloom_filter" || idxExpr != tc.col {
					t.Fatalf("stellar.%s %s = %s(%s), want bloom_filter(%s)", table, tc.index, idxType, idxExpr, tc.col)
				}
			}

			// A private clone inherits the definition without disturbing the
			// live board other tests read; 40,000 rows is five granules.
			probe := "stellar." + tc.table + "_keyed_probe"
			for _, q := range []string{
				`DROP TABLE IF EXISTS ` + probe,
				`CREATE TABLE ` + probe + ` AS stellar.` + tc.table,
				fmt.Sprintf(`INSERT INTO %s (rank, %s)
					SELECT toUInt32(number + 1), concat('GKEYEDPROBE', toString(number)) FROM numbers(40000)`, probe, tc.col),
			} {
				if err := conn.Exec(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			t.Cleanup(func() { _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS `+probe) }) //nolint:contextcheck // cleanup outlives the test context

			plan := explain(t, ctx, conn, fmt.Sprintf(`EXPLAIN indexes = 1 SELECT count() FROM %s
				WHERE %s = 'GKEYEDPROBE31000'`, probe, tc.col))
			selected, initial := skipIndexGranules(t, plan, tc.index)
			if initial < 2 || selected >= initial {
				t.Fatalf("%s dropped no granules (%d/%d) for a one-row keyed read:\n%s", tc.index, selected, initial, plan)
			}
		})
	}
}

// The operator files' ALTERs on a pre-index board must land the index on both EXCHANGE halves, and be idempotent when run
// twice.
func TestAccountBoardIndexUpgradeAppliesToALegacyBoard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	_, thisFile, _, _ := runtime.Caller(0)

	for _, tc := range []struct{ file, table, index string }{
		{"account_creators_rollup.sql", "account_creators_rollup", "idx_creators_rollup_creator"},
		{"account_sponsors_rollup.sql", "account_sponsors_rollup", "idx_sponsors_rollup_sponsor"},
	} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		alters := map[string]string{}
		for _, stmt := range splitSQLStatements(string(raw)) {
			if f := strings.Fields(stmt); len(f) > 2 && f[0] == "ALTER" && f[1] == "TABLE" {
				alters[strings.TrimPrefix(f[2], "stellar.")] = stmt
			}
		}
		for _, target := range []string{tc.table, tc.table + "_staging"} {
			stmt, ok := alters[target]
			if !ok {
				t.Fatalf("%s carries no ALTER for stellar.%s — an existing host keeps an unindexed board", tc.file, target)
			}
			legacy := target + "_legacy_probe"
			for _, q := range []string{
				`DROP TABLE IF EXISTS stellar.` + legacy,
				`CREATE TABLE stellar.` + legacy + ` AS stellar.` + target,
				`ALTER TABLE stellar.` + legacy + ` DROP INDEX ` + tc.index,
			} {
				if err := conn.Exec(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			t.Cleanup(func() { _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS stellar.`+legacy) })
			if n := skipIndexCount(t, ctx, conn, legacy, tc.index); n != 0 {
				t.Fatalf("legacy probe stellar.%s still has %s", legacy, tc.index)
			}
			rewritten := strings.Replace(stmt, "stellar."+target, "stellar."+legacy, 1)
			for range 2 {
				if err := conn.Exec(ctx, rewritten); err != nil {
					t.Fatalf("%s's ALTER for %s failed on a legacy board: %v\n%s", tc.file, target, err, rewritten)
				}
			}
			if n := skipIndexCount(t, ctx, conn, legacy, tc.index); n != 1 {
				t.Fatalf("after %s's ALTER, stellar.%s has %d %s indices, want 1", tc.file, legacy, n, tc.index)
			}
		}
	}
}

func skipIndexCount(t *testing.T, ctx context.Context, conn driver.Conn, table, index string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM system.data_skipping_indices
		WHERE database = 'stellar' AND table = ? AND name = ?`, table, index).Scan(&n); err != nil {
		t.Fatalf("count %s on %s: %v", index, table, err)
	}
	return n
}

// Activity-watermark bound on AccountOperations. Must not hide data: the bounded read returns EXACTLY the account's rows,
// incl. a NON-SOURCE participant op above its last sourced activity (the watermark is max over ALL roles, not min), and it
// must prune (EXPLAIN ESTIMATE of the reverse primary-key scan reads fewer operations rows with the bound).
func TestClickHouseAccountActivityWatermarkBoundedOps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		idle  = "GTEST_WM31_IDLE_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		other = "GTEST_WM31_OTHER_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAA"
		decoy = "GTEST_WM31_DECOY_TIP_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"

		sourcedLo   = uint32(6_210_001)  // idle's sourced ops: 3 consecutive ledgers from here
		participant = uint32(6_310_007)  // idle is a NON-SOURCE participant here (postdates sourced)
		tip         = uint32(82_310_001) // decoy activity in a far-higher partition (the "tip")
	)
	// Shared-ClickHouse isolation: the decoy `tip` seq is ~82M, so a per-second offset would steal the GLOBAL max(close_time)
	// tip that TestNetworkThroughput_DedupsReingestedLedger reserves in 2027. Offset in MILLISECONDS to stay inside
	// 2026/05/01; assertions key on ledger_seq, so same-second rows are fine.
	closeAt := func(seq uint32) time.Time {
		return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq-sourcedLo) * time.Millisecond)
	}
	hash := func(seq uint32) string { return fmt.Sprintf("%064d", seq) }

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	addLedger := func(seq uint32, txSource, opSource string, participants []string) {
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeAt(seq), LedgerHash: "aa", PrevHash: "bb",
				ProtocolVersion: 22, TxCount: 1, OpCount: 1,
			},
			Txs: []chstore.TransactionRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, SourceAccount: txSource, Successful: 1,
			}},
			Ops: []chstore.OperationRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, OpIndex: 0, OpType: "OperationTypePayment", SourceAccount: opSource, BodyXDR: "Ym9keQ==",
			}},
		}
		for _, p := range participants {
			ext.Participants = append(ext.Participants, chstore.OperationParticipantRow{
				Account: p, LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq), TxIndex: 0, OpIndex: 0,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}

	// The idle account's own sourced activity, far below the tip.
	for i := uint32(0); i < 3; i++ {
		addLedger(sourcedLo+i, idle, idle, nil)
	}
	// A LATER op sourced by someone else in which idle only participates —
	// the row a sourced-only watermark would hide.
	addLedger(participant, other, other, []string{idle})
	// Decoy tip activity in a far-higher partition: the granules the
	// unbounded resolve walks through and the bounded one must skip.
	for i := uint32(0); i < 3; i++ {
		addLedger(tip+i, decoy, decoy, nil)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")

	// (2) The MV-maintained watermark must be the max over ALL roles: the
	// participant ledger, not the higher sourced ledger.
	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, idle).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm != participant {
		t.Fatalf("watermark = %d, want %d — account_activity must cover EVERY role the ops query "+
			"filters on (a source-only watermark = %d would bound below the participant row and HIDE it)",
			wm, participant, sourcedLo+2)
	}

	// (1) Bounded read loses nothing: newest-first, participant row first, then the three sourced rows; equality with the
	// seeded truth IS bounded-vs-unbounded equality since the watermark row exists.
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, _, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	wantSeqs := []uint32{participant, sourcedLo + 2, sourcedLo + 1, sourcedLo}
	if len(rows) != len(wantSeqs) {
		got := make([]uint32, len(rows))
		for i, r := range rows {
			got[i] = r.Seq
		}
		t.Fatalf("AccountOperations returned %d rows %v, want %d %v — a missing row means the "+
			"watermark bound HID history (the data-hiding failure the #31 invariant forbids)",
			len(rows), got, len(wantSeqs), wantSeqs)
	}
	for i, want := range wantSeqs {
		if rows[i].Seq != want {
			t.Fatalf("row %d at ledger %d, want %d (newest first)", i, rows[i].Seq, want)
		}
	}

	// Pagination across the bound: cursor below the participant row must
	// serve the sourced rows, nothing lost at the seam.
	page2, _, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{Ledger: participant, A: 0, B: 0})
	if err != nil {
		t.Fatalf("AccountOperations page 2: %v", err)
	}
	if len(page2) != 3 || page2[0].Seq != sourcedLo+2 {
		t.Fatalf("cursor page returned %d rows (first seq %v), want the 3 sourced rows from %d",
			len(page2), page2, sourcedLo+2)
	}

	// (3) The bound prunes: EXPLAIN ESTIMATE of the reverse primary-key scan must read strictly fewer operations rows with it.
	// The scan shape is asserted, not the IN-arm: on a toy dataset set-based index analysis resolves the IN to exact granules
	// either way, which does not save the live table; the value is partition + primary-key range pruning.
	armSQL := func(bound string) string {
		where := ""
		if bound != "" {
			where = ` WHERE ` + bound
		}
		return `EXPLAIN ESTIMATE SELECT ledger_seq, tx_index, op_index FROM stellar.operations` + where + `
			ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT 50`
	}
	opsRowsEstimate := func(sql string) uint64 {
		res, err := raw.Query(ctx, sql)
		if err != nil {
			t.Fatalf("EXPLAIN ESTIMATE: %v", err)
		}
		defer func() { _ = res.Close() }()
		var total uint64
		for res.Next() {
			var db, table string
			var parts, nrows, marks uint64
			if err := res.Scan(&db, &table, &parts, &nrows, &marks); err != nil {
				t.Fatalf("scan EXPLAIN ESTIMATE: %v", err)
			}
			if table == "operations" {
				total += nrows
			}
		}
		if err := res.Err(); err != nil {
			t.Fatalf("EXPLAIN ESTIMATE rows: %v", err)
		}
		return total
	}
	unbounded := opsRowsEstimate(armSQL(""))
	bounded := opsRowsEstimate(armSQL(fmt.Sprintf("ledger_seq <= %d", wm)))
	if bounded >= unbounded {
		t.Fatalf("bounded arm reads %d operations rows, unbounded %d — the watermark bound must "+
			"prune the tip partitions (granule-bounded scan is the whole point of #31)", bounded, unbounded)
	}
}

// operations/transactions are ReplacingMergeTree(ingested_at), so a re-derive leaves stale and corrected rows in separate
// parts. `LIMIT 1 BY` picks by part order; only FINAL applies the engine's rule (highest ingested_at, last insert on a
// same-second tie), as /v1/tx/{hash} serves. Keys cover both part orders and the tie.
func TestClickHouseAccountHistoryServesNewestUnmergedVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		account = "GTEST_GH1141_REDERIVE_ACCOUNT_AAAAAAAAAAAAAAAAAAAAA"
		seq     = uint32(6_420_001)
	)
	// Far below the throughput test's global close_time tip (see
	// account_activity_watermark_test.go).
	closeTime := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	older, newer := closeTime.Add(10*time.Second), closeTime.Add(20*time.Second)
	type version struct {
		ingestedAt time.Time
		fresh      bool
	}
	// Per tx_index: the version written to part 1, then to part 2.
	cases := [][2]version{
		{{older, false}, {newer, true}}, // re-derive lands later, higher version
		{{newer, true}, {older, false}}, // higher version sits in the EARLIER part
		{{older, false}, {older, true}}, // same-second re-derive: last insert wins
	}
	memo := func(fresh bool) string {
		if fresh {
			return "fresh-post-rederive"
		}
		return "stale-pre-rederive"
	}
	fee := func(fresh bool) int64 {
		if fresh {
			return 200
		}
		return 100
	}
	body := func(fresh bool) string {
		if fresh {
			return "ZnJlc2g="
		}
		return "c3RhbGU="
	}
	txHash := func(i int) string { return fmt.Sprintf("%064d", 1141_000+i) }

	raw := dialClickHouse(t, ctx, "stellar")
	for _, tbl := range []string{"stellar.transactions", "stellar.operations"} {
		if err := raw.Exec(ctx, "SYSTEM STOP MERGES "+tbl); err != nil {
			t.Fatalf("SYSTEM STOP MERGES %s: %v", tbl, err)
		}
		t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES "+tbl) })
	}

	// One INSERT per part per table → two un-merged parts each.
	for part := 0; part < 2; part++ {
		tb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			 operation_count, successful, result_code, memo_type, memo, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare tx batch: %v", err)
		}
		ob, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operations
			(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare op batch: %v", err)
		}
		for i, c := range cases {
			v := c[part]
			if err := tb.Append(seq, closeTime, txHash(i), uint32(i), account, fee(v.fresh), int64(1000),
				uint16(1), uint8(1), int32(0), "MemoTypeMemoText", memo(v.fresh), v.ingestedAt); err != nil {
				t.Fatalf("append tx: %v", err)
			}
			if err := ob.Append(seq, closeTime, txHash(i), uint32(i), uint32(0), "OperationTypePayment",
				account, body(v.fresh), v.ingestedAt); err != nil {
				t.Fatalf("append op: %v", err)
			}
		}
		if err := tb.Send(); err != nil {
			t.Fatalf("send tx: %v", err)
		}
		if err := ob.Send(); err != nil {
			t.Fatalf("send op: %v", err)
		}
	}

	// Precondition: both versions of every key are physically present, or
	// this test proves nothing about FINAL.
	for _, q := range []string{
		`SELECT count() FROM stellar.transactions WHERE ledger_seq = ? GROUP BY ledger_seq, tx_index`,
		`SELECT count() FROM stellar.operations WHERE ledger_seq = ? GROUP BY ledger_seq, tx_index, op_index`,
	} {
		rows, err := raw.Query(ctx, q, seq)
		if err != nil {
			t.Fatalf("count versions: %v", err)
		}
		keys := 0
		for rows.Next() {
			var n uint64
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan versions: %v", err)
			}
			if n != 2 {
				t.Fatalf("%s: a key has %d row(s), want 2 un-merged versions", q, n)
			}
			keys++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("count versions rows: %v", err)
		}
		_ = rows.Close()
		if keys != len(cases) {
			t.Fatalf("%s: %d key(s), want %d", q, keys, len(cases))
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	txs, _, err := er.AccountTransactions(ctx, account, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	if len(txs) != len(cases) {
		t.Fatalf("AccountTransactions returned %d rows, want %d (one per key): %+v", len(txs), len(cases), txs)
	}
	for _, tx := range txs {
		if tx.Memo != memo(true) || tx.FeeCharged != fee(true) {
			t.Errorf("AccountTransactions tx_index=%d served memo=%q fee=%d, want the newest version memo=%q fee=%d",
				tx.TxIndex, tx.Memo, tx.FeeCharged, memo(true), fee(true))
		}
	}

	ops, _, err := er.AccountOperations(ctx, account, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(ops) != len(cases) {
		t.Fatalf("AccountOperations returned %d rows, want %d (one per key): %+v", len(ops), len(cases), ops)
	}
	for _, op := range ops {
		if op.BodyXDR != body(true) {
			t.Errorf("AccountOperations tx_index=%d served body_xdr=%q, want the newest version %q",
				op.TxIndex, op.BodyXDR, body(true))
		}
	}
}

// distinct_sponsored_total must count each sponsored account once across the WHOLE board, not once per sponsor. Two
// sponsors of the SAME account give per-sponsor distinct_sponsored 1 each (summing to the buggy 2); the true count is 1.
func TestSponsorsRollup_DistinctSponsoredTotalIsGlobal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const ledger = uint32(50)
	sponsorA := gAccountFromSeed(t, 0x41)
	sponsorB := gAccountFromSeed(t, 0x42)
	sponsored := gAccountFromSeed(t, 0x43)
	// Early close time: a later one becomes the lake's max(close_time) and
	// shifts NetworkThroughput's window off TestNetworkThroughput_* fixtures.
	closeTime := time.Date(2024, 1, 1, 0, 0, 50, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	txHashA := "sponsdistinctaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txHashB := "sponsdistinctbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Txs: []chstore.TransactionRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0,
				SourceAccount: sponsorA, FeeCharged: 100, MaxFee: 100, OperationCount: 2, Successful: 1,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1,
				SourceAccount: sponsorB, FeeCharged: 100, MaxFee: 100, OperationCount: 2, Successful: 1,
			},
		},
		Ops: []chstore.OperationRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeBeginSponsoringFutureReserves", SourceAccount: sponsorA,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0, OpIndex: 1,
				OpType: "OperationTypeEndSponsoringFutureReserves", SourceAccount: sponsored,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1, OpIndex: 0,
				OpType: "OperationTypeBeginSponsoringFutureReserves", SourceAccount: sponsorB,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1, OpIndex: 1,
				OpType: "OperationTypeEndSponsoringFutureReserves", SourceAccount: sponsored,
			},
		},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	if err := chstore.RunSponsorsRollup(ctx, addr, t.Logf); err != nil {
		t.Fatalf("RunSponsorsRollup: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	board, ok, err := reader.AccountSponsors(ctx, 10, "")
	if err != nil {
		t.Fatalf("AccountSponsors: %v", err)
	}
	if !ok {
		t.Fatal("AccountSponsors: no board")
	}
	if got, want := board.DistinctSponsoredTotal, int64(1); got != want {
		t.Fatalf("DistinctSponsoredTotal = %d, want %d (one account sponsored by two sponsors "+
			"counts once across the whole board, not once per sponsor)", got, want)
	}

	// A keyed miss still reports the cycle's time, never the zero time.
	miss, ok, err := reader.AccountSponsors(ctx, 10, gAccountFromSeed(t, 0x44))
	if err != nil || !ok {
		t.Fatalf("AccountSponsors(keyed miss) = ok %v, err %v", ok, err)
	}
	if len(miss.Board) != 0 {
		t.Fatalf("keyed miss Board = %+v, want empty", miss.Board)
	}
	assertCycleTime(t, miss.ComputedAt, board.ComputedAt)
}

// assertCycleTime pins a keyed miss's computed_at to the same cycle as the
// board's: the stats and board rows are written seconds apart.
func assertCycleTime(t *testing.T, got, board time.Time) {
	t.Helper()
	if got.IsZero() {
		t.Fatalf("keyed miss ComputedAt is the zero time; want the cycle's (%s)", board)
	}
	if d := got.Sub(board); d < -time.Minute || d > time.Minute {
		t.Fatalf("keyed miss ComputedAt = %s, want the board cycle's %s", got, board)
	}
}

// legacyAccountOperationsSQL is the pre-rewrite AccountOperations page query, frozen as a differential oracle: the pruning
// rewrite must serve identical pages without reading one operations granule per key the account ever touched.
func legacyAccountOperationsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
	}
	boundClause := ` AND ledger_seq <= ?`
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr FROM stellar.operations
		WHERE (ledger_seq, tx_index, op_index) IN (
		  SELECT ledger_seq, tx_index, op_index FROM (
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		            WHERE source_account = ? AND op_index != 4294967295)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.operation_participants WHERE account = ?)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// legacyAccountTransactionsSQL is AccountTransactions' page query as it stood
// before the same rewrite (origin/main 6762d0a1, accountTransactionsQuery with
// txCols expanded) — the differential oracle for the transactions walk.
func legacyAccountTransactionsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index) < (?, ?)`
	}
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, source_account,
	fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo FROM stellar.transactions
		WHERE (ledger_seq, tx_index) IN (
		  SELECT ledger_seq, tx_index FROM (
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.ops_by_source WHERE source_account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.operation_participants WHERE account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// AccountOperations rewrite proof (explorer 503s for an account with 11,925 sourced + 26,064 participant ops). The old `pk
// IN (SELECT ...)` arms cost one granule PER KEY before the LIMIT; the fix pages ops_by_source/operation_participants
// directly. Differential walk vs frozen legacy SQL (transactions:
func TestClickHouseAccountOperationsPageBoundedByPageSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		hot  = "GTEST_PKP_HOT_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		fill = "GTEST_PKP_FILLER_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		base = uint32(9_500_001) // partitions 9..11; nowhere near other tests' ledgers
		n    = 2_000_000
		// Sourced every `stride` ledgers (one key per 8192-row granule);
		// participant on the same stride at `partOff`; overlap every
		// `overlapStride` (a sourced op HOT also participates in).
		stride  = 10_000
		partOff = 5_000
		txOff   = 2_500
	)
	// close_time: ms offsets keep the whole fixture inside 2026/05/01 so
	// it never becomes the global close_time tip another test anchors on
	// (see account_activity_watermark_test.go).
	closeExpr := `toDateTime('2026-05-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	seqExpr := fmt.Sprintf(`toUInt32(%d + number)`, base)
	hashExpr := `lpad(toString(` + seqExpr + `), 64, '0')`

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.80q: %v", q, err)
		}
	}
	hotOp := fmt.Sprintf(`number %% %d = 0`, stride)
	hotPart := fmt.Sprintf(`(number %% %d = %d OR number %% %d = %d)`, stride, partOff, stride, txOff)
	hotTx := fmt.Sprintf(`(number %% %d = 0 OR number %% %d = %d)`, stride, stride, txOff)
	insertOps := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
			(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
			SELECT %s, %s, %s, 0, 0, 'OperationTypePayment', if(%s, '%s', '%s'), 'Ym9keQ=='
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotOp, hot, fill, n, where))
	}
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, if(%s, '%s', '%s'), 100, 200, 1, 1, 0, 'none', ''
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotTx, hot, fill, n, where))
	}
	insertParts := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_participants
			(account, ledger_seq, close_time, tx_hash, tx_index, op_index)
			SELECT '%s', %s, %s, %s, 0, 0 FROM numbers(%d) WHERE %s AND %s`,
			hot, seqExpr, closeExpr, hashExpr, n, hotPart, where))
	}
	insertOps(`1`)
	insertTxs(`1`)
	insertParts(`1`)
	// Re-ingested duplicate PARTS near the bottom of the range (MVs
	// re-fire, so ops_by_source and account_activity get duplicates too).
	dup := fmt.Sprintf(`number < %d`, 3*stride)
	insertOps(dup)
	insertTxs(dup)
	insertParts(dup)

	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, hot).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm == 0 {
		t.Fatal("fixture: account_activity watermark missing — the reader would take the unbounded path and the legacy oracle (bounded) would not be the same query")
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	legacyPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.OpRow {
		t.Helper()
		args := []any{hot, wm}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, hot, wm)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountOperationsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.OpRow
		for rows.Next() {
			var r chstore.OpRow
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.OpIndex, &r.OpType, &r.SourceAccount, &r.BodyXDR); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	legacyTxPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.TxSummary {
		t.Helper()
		args := []any{hot}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, hot)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountTransactionsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy tx page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.TxSummary
		for rows.Next() {
			var r chstore.TxSummary
			var ok uint8
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.SourceAccount,
				&r.FeeCharged, &r.MaxFee, &r.OperationCount, &ok, &r.ResultCode, &r.MemoType, &r.Memo); err != nil {
				t.Fatalf("legacy tx scan: %v", err)
			}
			r.Successful = ok != 0
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy tx rows: %v", err)
		}
		return out
	}

	// (1) Full-history differential walk, page size 7.
	const pageSize = 7
	type key [3]uint32
	older := func(k, prev key) bool {
		return k[0] < prev[0] || (k[0] == prev[0] && (k[1] < prev[1] || (k[1] == prev[1] && k[2] < prev[2])))
	}
	// walk pages `name` through reader and legacy until the reader serves an empty page, asserting page-for-page equality and
	// returning the distinct keys. All pages are `pageSize` long but the last: a non-final short page makes a client stop.
	walk := func(name string, page func(cur chstore.ExplorerCursor) (got, want []key)) map[key]bool {
		t.Helper()
		var (
			cur   chstore.ExplorerCursor
			seen  = map[key]bool{}
			pages int
			last  key
			short int // pages shorter than pageSize; only the final one may be
		)
		for {
			got, want := page(cur)
			if len(got) > 0 {
				if short > 0 {
					t.Fatalf("%s page %d (cursor %+v) follows a SHORT page — a non-final short page means a client stopping on it truncates history", name, pages, cur)
				}
				if len(got) < pageSize {
					short++
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%s page %d (cursor %+v): reader %d rows, legacy %d rows", name, pages, cur, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s page %d row %d differs (cursor %+v): reader %v legacy %v", name, pages, i, cur, got[i], want[i])
				}
				if seen[got[i]] {
					t.Fatalf("%s page %d row %d: key %v served twice (dedupe / cursor regression)", name, pages, i, got[i])
				}
				if (pages > 0 || i > 0) && !older(got[i], last) {
					t.Fatalf("%s page %d row %d: key %v not strictly older than previous %v", name, pages, i, got[i], last)
				}
				seen[got[i]] = true
				last = got[i]
			}
			if len(got) == 0 {
				break
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1], B: last[2]}
		}
		t.Logf("%s: %d distinct keys over %d pages", name, len(seen), pages)
		return seen
	}

	opKeys := func(rows []chstore.OpRow) []key {
		out := make([]key, len(rows))
		for i, r := range rows {
			out[i] = key{r.Seq, r.TxIndex, r.OpIndex}
		}
		return out
	}
	opsSeen := walk("operations", func(cur chstore.ExplorerCursor) ([]key, []key) {
		got, _, err := er.AccountOperations(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountOperations (cursor %+v): %v", cur, err)
		}
		want := legacyPage(ctx, pageSize, cur)
		// Full-row equality (close_time, hash, type, source, body), not
		// just keys — the hydration must serve the same wide row.
		for i := range got {
			if i < len(want) && got[i] != want[i] {
				t.Fatalf("operations row differs (cursor %+v):\n reader %+v\n legacy %+v", cur, got[i], want[i])
			}
		}
		return opKeys(got), opKeys(want)
	})
	if want := 3 * (n / stride); len(opsSeen) != want {
		t.Fatalf("operations: %d distinct ops, want %d (sourced %d + participant %d + participant-on-hot-sourced-tx %d)",
			len(opsSeen), want, n/stride, n/stride, n/stride)
	}
	// The tx listing's arms legitimately overlap. Without dedupe an overlap key took TWO merge LIMIT slots, the page came back
	// SHORT, and the handler (next_cursor only on a FULL page) truncated the client's walk. The reader now dedupes at the
	// merge, so compare the whole walk (no row gained, lost, reordered or repeated)
	txWalk := func(name string, page func(cur chstore.ExplorerCursor) []chstore.TxSummary) (rows []chstore.TxSummary, nonFinalShort int) {
		t.Helper()
		var (
			cur       chstore.ExplorerCursor
			seen      = map[key]bool{}
			pages     int
			last      key
			prevShort bool
		)
		for {
			got := page(cur)
			if len(got) > 0 && prevShort {
				nonFinalShort++
			}
			if len(got) == 0 {
				break
			}
			prevShort = len(got) < pageSize
			for i, r := range got {
				k := key{r.Seq, r.TxIndex, 0}
				if seen[k] {
					t.Fatalf("%s page %d row %d: tx %v served twice (dedupe / cursor regression)", name, pages, i, k)
				}
				if (pages > 0 || i > 0) && !older(k, last) {
					t.Fatalf("%s page %d row %d: tx %v not strictly older than previous %v", name, pages, i, k, last)
				}
				seen[k] = true
				last = k
				rows = append(rows, r)
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1]}
		}
		t.Logf("%s: %d txs over %d pages (%d non-final short pages)", name, len(rows), pages, nonFinalShort)
		return rows, nonFinalShort
	}

	readerTxs, readerShort := txWalk("transactions (reader)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		got, _, err := er.AccountTransactions(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountTransactions (cursor %+v): %v", cur, err)
		}
		return got
	})
	legacyTxs, legacyShort := txWalk("transactions (legacy)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		return legacyTxPage(ctx, pageSize, cur)
	})
	if readerShort != 0 {
		t.Fatalf("transactions: %d non-final SHORT pages from the reader — the handler withholds next_cursor on a short page, so a client stops there with older history unreached (#290)", readerShort)
	}
	if legacyShort == 0 {
		t.Fatal("fixture no longer reproduces #290: the pre-fix query text served no non-final short page, so the fullness assertion above is vacuous — the arms must still overlap (the txOff txs)")
	}
	if len(readerTxs) != len(legacyTxs) {
		t.Fatalf("transactions: reader walked %d txs, legacy %d — the merge dedupe must change page BOUNDARIES, never the rows served", len(readerTxs), len(legacyTxs))
	}
	for i := range readerTxs {
		if readerTxs[i] != legacyTxs[i] {
			t.Fatalf("transactions row %d differs:\n reader %+v\n legacy %+v", i, readerTxs[i], legacyTxs[i])
		}
	}
	if want := 3 * (n / stride); len(readerTxs) != want {
		t.Fatalf("transactions: %d distinct txs, want %d (sourced-with-op %d + participant-only %d + sourced-tx-and-participant %d)",
			len(readerTxs), want, n/stride, n/stride, n/stride)
	}

	// A cursor set MID-page (not at a page boundary): the next page must
	// start exactly at the following row, identically on both paths.
	first, _, err := er.AccountOperations(ctx, hot, 10, chstore.ExplorerCursor{})
	if err != nil || len(first) != 10 {
		t.Fatalf("first page: %v (%d rows)", err, len(first))
	}
	mid := chstore.ExplorerCursor{Ledger: first[3].Seq, A: first[3].TxIndex, B: first[3].OpIndex}
	fromMid, _, err := er.AccountOperations(ctx, hot, 10, mid)
	if err != nil {
		t.Fatalf("mid-page cursor: %v", err)
	}
	legacyMid := legacyPage(ctx, 10, mid)
	if len(fromMid) != 10 || fromMid[0] != first[4] || len(legacyMid) != 10 || legacyMid[0] != first[4] {
		t.Fatalf("mid-page cursor %+v: reader first row %+v, legacy first row %+v, want %+v",
			mid, fromMid[0], legacyMid[0], first[4])
	}

	// (2) read_rows: one page of 50 via each path.
	const limit = 50
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyPage(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)), limit, chstore.ExplorerCursor{})
	// The reader issues several statements per page (two windowed key arms and
	// the hydration), so its cost is the SUM over every statement it ran,
	// found through the log_comment this context stamps on each of them.
	readerTag := uuid.NewString()
	readerCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": readerTag}))
	if _, _, err := er.AccountOperations(readerCtx, hot, limit, chstore.ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations read_rows page: %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)
	readRows := func(where string, args ...any) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND `+where+`
			ORDER BY event_time_microseconds DESC LIMIT 1`, append([]any{t0}, args...)...).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", where, err)
		}
		return rr
	}
	legacyRead := readRows(`query_id = ?`, legacyID)
	var readerRead uint64
	if err := raw.QueryRow(ctx, `SELECT sum(read_rows) FROM system.query_log
		WHERE type = 'QueryFinish' AND event_time >= ? AND log_comment = ?`, t0, readerTag).Scan(&readerRead); err != nil {
		t.Fatalf("query_log (reader statements): %v", err)
	}
	if readerRead == 0 {
		t.Fatal("no reader statement found in query_log — the log_comment tag did not reach the server")
	}
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d hot op keys, one per 8192-row granule; page %d)",
		legacyRead, readerRead, len(opsSeen), limit)
	if legacyRead < uint64(n/stride)*4096 {
		t.Fatalf("fixture no longer reproduces the pathology: legacy read %d rows, expected ≥ one granule per hot key (~%d) — the differential still holds but the read_rows bound below would be vacuous",
			legacyRead, (n/stride)*8192)
	}
	if readerRead*4 > legacyRead {
		t.Fatalf("reader read %d rows vs legacy %d — the page must be bounded by the page size (≤ 3×limit granules), not by the account's history",
			readerRead, legacyRead)
	}
}

// A failed classic tx still writes operation_participants rows, so anyone could plant a row in any account's history for a
// fee. Listings must drop participant-only rows of failed txs but keep the account's own failed txs and all successes; the
// 20 failed spam txs sit NEWEST so the participant arm must page past them.
func TestClickHouseAccountHistoryHidesFailedParticipantTxs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		victim   = "GTEST_INV2697_VICTIM_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		spammer  = "GTEST_INV2697_SPAMMER_AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		base     = uint32(7_269_701)
		spamFrom = base + 10
		spamN    = 20
	)
	// Far below the throughput test's global close_time tip (see
	// account_activity_watermark_test.go).
	closeTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	fixture := []participantFixtureTx{
		{base, true, spammer, spammer, true},     // successful payment to victim: visible
		{base + 1, false, victim, victim, false}, // victim's own failed tx: visible (sourced)
		{base + 2, false, victim, spammer, true}, // victim's own failed tx, op sourced by another: visible
	}
	for i := uint32(0); i < spamN; i++ {
		fixture = append(fixture, participantFixtureTx{spamFrom + i, false, spammer, spammer, true}) // hidden
	}
	insertParticipantFixture(t, ctx, victim, closeTime, fixture)

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	want := []uint32{base + 2, base + 1, base}

	t.Run("transactions", func(t *testing.T) {
		txs, _, err := er.AccountTransactions(ctx, victim, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountTransactions: %v", err)
		}
		assertSeqs(t, "AccountTransactions page", txSeqs(txs), want)

		var walked []uint32
		cur := chstore.ExplorerCursor{}
		for page := 0; page < len(want)+2; page++ {
			txs, _, err := er.AccountTransactions(ctx, victim, 1, cur)
			if err != nil {
				t.Fatalf("AccountTransactions limit-1 page %d: %v", page, err)
			}
			if len(txs) == 0 {
				break
			}
			walked = append(walked, txs[0].Seq)
			cur = chstore.ExplorerCursor{Ledger: txs[0].Seq, A: txs[0].TxIndex}
		}
		assertSeqs(t, "AccountTransactions limit-1 walk", walked, want)
	})

	t.Run("operations", func(t *testing.T) {
		ops, _, err := er.AccountOperations(ctx, victim, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountOperations: %v", err)
		}
		assertSeqs(t, "AccountOperations page", opSeqs(ops), want)

		var walked []uint32
		cur := chstore.ExplorerCursor{}
		for page := 0; page < len(want)+2; page++ {
			ops, _, err := er.AccountOperations(ctx, victim, 1, cur)
			if err != nil {
				t.Fatalf("AccountOperations limit-1 page %d: %v", page, err)
			}
			if len(ops) == 0 {
				break
			}
			walked = append(walked, ops[0].Seq)
			cur = chstore.ExplorerCursor{Ledger: ops[0].Seq, A: ops[0].TxIndex, B: ops[0].OpIndex}
		}
		assertSeqs(t, "AccountOperations limit-1 walk", walked, want)
	})

	t.Run("op-type counts", func(t *testing.T) {
		counts, err := er.AccountOperationTypeCounts(ctx, victim)
		if err != nil {
			t.Fatalf("AccountOperationTypeCounts: %v", err)
		}
		var total int64
		for _, c := range counts {
			total += c.Count
		}
		if total != int64(len(want)) {
			t.Fatalf("AccountOperationTypeCounts total = %d, want %d (failed participant-only ops counted): %+v",
				total, len(want), counts)
		}
	})

	// account_op_types must answer what the join answers once marked. The source arm is its MV; the participant arm is written
	// here as the sink writes it (visible participant rows only), since the fixture bypasses the sink.
	t.Run("op-type counts from account_op_types", func(t *testing.T) {
		joined := map[string][]chstore.OpTypeCount{}
		for _, acct := range []string{victim, spammer} {
			c, err := er.AccountOperationTypeCounts(ctx, acct)
			if err != nil {
				t.Fatalf("join counts(%s): %v", acct, err)
			}
			joined[acct] = c
		}
		raw := dialClickHouse(t, ctx, "stellar")
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_op_types (account, ledger_seq, tx_index, op_index, op_type)
			SELECT p.account, p.ledger_seq, p.tx_index, p.op_index, o.op_type
			FROM stellar.operation_participants AS p
			JOIN stellar.operations AS o USING (ledger_seq, tx_index, op_index)
			JOIN stellar.transactions AS t USING (ledger_seq, tx_index)
			WHERE p.ledger_seq BETWEEN ? AND ? AND (t.successful = 1 OR t.source_account = p.account)`,
			base, spamFrom+spamN); err != nil {
			t.Fatalf("write participant arm: %v", err)
		}
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_op_types_coverage (covered_from, covered_to) VALUES (2, ?)`, spamFrom+spamN); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		t.Cleanup(func() { _ = raw.Exec(context.Background(), `TRUNCATE TABLE stellar.account_op_types_coverage`) })

		marked, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("new explorer reader: %v", err)
		}
		t.Cleanup(func() { _ = marked.Close() })
		for _, acct := range []string{victim, spammer} {
			got, err := marked.AccountOperationTypeCounts(ctx, acct)
			if err != nil {
				t.Fatalf("indexed counts(%s): %v", acct, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(joined[acct]) {
				t.Errorf("%s: account_op_types counts %+v, join counts %+v", acct, got, joined[acct])
			}
		}
	})

	// The spammer sourced every spam tx (and the op in base+2); its own
	// history keeps them all.
	t.Run("spammer keeps its own failed txs", func(t *testing.T) {
		txs, _, err := er.AccountTransactions(ctx, spammer, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountTransactions(spammer): %v", err)
		}
		if len(txs) != spamN+2 {
			t.Fatalf("spammer has %d txs, want %d", len(txs), spamN+2)
		}
	})
}

// participantFixtureTx is one single-op transaction of a participant fixture.
type participantFixtureTx struct {
	seq         uint32
	ok          bool
	txSource    string
	opSource    string
	participant bool // the op names the fixture's participant as a non-source participant
}

// insertParticipantFixture writes txs to stellar.transactions, operations and
// operation_participants in the sink's flush order.
func insertParticipantFixture(t *testing.T, ctx context.Context, participant string, closeTime time.Time, txs []participantFixtureTx) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	tb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.transactions
		(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
		 operation_count, successful, result_code, memo_type, memo)`)
	if err != nil {
		t.Fatalf("prepare tx batch: %v", err)
	}
	ob, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)`)
	if err != nil {
		t.Fatalf("prepare op batch: %v", err)
	}
	pb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operation_participants
		(account, ledger_seq, close_time, tx_hash, tx_index, op_index)`)
	if err != nil {
		t.Fatalf("prepare participant batch: %v", err)
	}
	for _, f := range txs {
		hash := fmt.Sprintf("%064d", f.seq)
		successful, code := uint8(1), int32(0)
		if !f.ok {
			successful, code = 0, -1
		}
		if err := tb.Append(f.seq, closeTime, hash, uint32(0), f.txSource, int64(100), int64(100),
			uint16(1), successful, code, "MemoTypeMemoNone", ""); err != nil {
			t.Fatalf("append tx: %v", err)
		}
		if err := ob.Append(f.seq, closeTime, hash, uint32(0), uint32(0), "OperationTypePayment",
			f.opSource, "Ym9keQ=="); err != nil {
			t.Fatalf("append op: %v", err)
		}
		if f.participant {
			if err := pb.Append(participant, f.seq, closeTime, hash, uint32(0), uint32(0)); err != nil {
				t.Fatalf("append participant: %v", err)
			}
		}
	}
	// Ingest order (Sink.Flush): transactions, operations, participants.
	if err := tb.Send(); err != nil {
		t.Fatalf("send tx: %v", err)
	}
	if err := ob.Send(); err != nil {
		t.Fatalf("send op: %v", err)
	}
	if err := pb.Send(); err != nil {
		t.Fatalf("send participant: %v", err)
	}
}

// Participant arm's multi-chunk visibility SQL and query budget: 1,500 participant txs span several visibility chunks and
// the 9,000-tx failed run exceeds one request's budget, ending pages short at a scan frontier. Walking pages as the
// handler does must yield exactly the successful txs, newest first, none skipped or repeated.
func TestClickHouseAccountHistoryParticipantBulkFailedTxs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		victim  = "GTEST_INV2697_BULKVICTIM_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		spammer = "GTEST_INV2697_BULKSPAM_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		base    = uint32(7_270_000)
		failedN = 9000
		mixedN  = 1500
		limit   = 200
	)
	closeTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	fixture := []participantFixtureTx{{base, true, spammer, spammer, true}}
	for i := uint32(1); i <= failedN; i++ {
		fixture = append(fixture, participantFixtureTx{base + i, false, spammer, spammer, true})
	}
	for i := uint32(1); i <= mixedN; i++ {
		fixture = append(fixture, participantFixtureTx{base + failedN + i, i%3 == 0, spammer, spammer, true})
	}
	var want []uint32
	for i := len(fixture) - 1; i >= 0; i-- {
		if fixture[i].ok {
			want = append(want, fixture[i].seq)
		}
	}
	insertParticipantFixture(t, ctx, victim, closeTime, fixture)

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	// walk pages like the handler: resume when set, else the last row of a
	// full page, else stop.
	walk := func(t *testing.T, page func(chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error)) {
		t.Helper()
		var got []uint32
		var cur chstore.ExplorerCursor
		resumes := 0
		for n := 0; n < 200; n++ {
			seqs, last, resume, err := page(cur)
			if err != nil {
				t.Fatalf("page %d: %v", n, err)
			}
			if n == 0 && len(seqs) != limit {
				t.Fatalf("first page has %d rows, want a full %d from two visibility chunks", len(seqs), limit)
			}
			got = append(got, seqs...)
			switch {
			case resume.IsSet():
				resumes++
				cur = resume
			case len(seqs) == limit:
				cur = last
			default:
				if resumes == 0 {
					t.Fatal("no page stopped at a scan frontier; the budget path did not run")
				}
				assertSeqs(t, "bulk walk", got, want)
				return
			}
		}
		t.Fatal("walk did not end")
	}

	t.Run("transactions", func(t *testing.T) {
		walk(t, func(cur chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error) {
			txs, resume, err := er.AccountTransactions(ctx, victim, limit, cur)
			var last chstore.ExplorerCursor
			if len(txs) > 0 {
				last = chstore.ExplorerCursor{Ledger: txs[len(txs)-1].Seq, A: txs[len(txs)-1].TxIndex}
			}
			return txSeqs(txs), last, resume, err
		})
	})
	t.Run("operations", func(t *testing.T) {
		walk(t, func(cur chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error) {
			ops, resume, err := er.AccountOperations(ctx, victim, limit, cur)
			var last chstore.ExplorerCursor
			if n := len(ops); n > 0 {
				last = chstore.ExplorerCursor{Ledger: ops[n-1].Seq, A: ops[n-1].TxIndex, B: ops[n-1].OpIndex}
			}
			return opSeqs(ops), last, resume, err
		})
	})
}

func txSeqs(txs []chstore.TxSummary) []uint32 {
	out := make([]uint32, len(txs))
	for i, tx := range txs {
		out[i] = tx.Seq
	}
	return out
}

func opSeqs(ops []chstore.OpRow) []uint32 {
	out := make([]uint32, len(ops))
	for i, op := range ops {
		out[i] = op.Seq
	}
	return out
}

func assertSeqs(t *testing.T, what string, got, want []uint32) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: ledgers %v, want %v (failed participant-only txs must be absent)", what, got, want)
	}
}

// accountsByWealthQuery (native_xlm basis) with balances 2^62+1 and 2^62 stroops, indistinguishable as float64:
// NativeStroops must be exact and the larger first via the native_stroops tiebreak, since the float ranking key ties.
func TestClickHouseAccountsByWealthNativeExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		bigger  = "GTEST_WEALTH_EXACT_BIGGER_AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		smaller = "GTEST_WEALTH_EXACT_SMALLER_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		ledger  = uint32(77_900_001)
	)
	balances := map[string]int64{bigger: 1<<62 + 1, smaller: 1 << 62}
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare ledger_entries_current: %v", err)
	}
	// Insert the smaller first so insertion order cannot pass for ranking.
	for _, acct := range []string{smaller, bigger} {
		if err := b.Append("account", "wealth-exact-"+acct, acct, "", balances[acct],
			"updated", ledger, at, ""); err != nil {
			t.Fatalf("append account: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send accounts: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	rows, err := er.AccountsByWealth(ctx, []string{"native"}, []string{"1"}, 500)
	if err != nil {
		t.Fatalf("AccountsByWealth: %v", err)
	}

	pos := map[string]int{}
	for i, w := range rows {
		want, ours := balances[w.AccountID]
		if !ours {
			continue
		}
		pos[w.AccountID] = i
		if got := w.NativeStroops.BigInt(); !got.IsInt64() || got.Int64() != want {
			t.Errorf("%s NativeStroops = %s, want exactly %d", w.AccountID, got, want)
		}
	}
	if len(pos) != 2 {
		t.Fatalf("ranking returned %d of the 2 seeded accounts (%d rows)", len(pos), len(rows))
	}
	if pos[bigger] > pos[smaller] {
		t.Errorf("bigger balance ranked at %d, after smaller at %d; native_stroops must break the float tie",
			pos[bigger], pos[smaller])
	}
}

// accountsByWealthQuery (usd basis): 10 XLM at 0.1 plus 3 TOK at 0.333333333333333333 is exactly 1.999999999999999999; a
// float64 sum returns 2 or 1.9999999999999998, which the endpoint would serve as an exact decimal string.
func TestClickHouseAccountsByWealthUSDExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		holder = "GTEST_WEALTH_USD_EXACT_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		tok    = "TOK-GTEST_WEALTH_USD_EXACT_ISSUER_AAAAAAAAAAAAAAAAAAAAAA"
		ledger = uint32(77_900_002)
	)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare ledger_entries_current: %v", err)
	}
	if err := b.Append("account", "wealth-usd-exact-acct", holder, "", int64(100_000_000),
		"updated", ledger, at, ""); err != nil {
		t.Fatalf("append account: %v", err)
	}
	if err := b.Append("trustline", "wealth-usd-exact-tl", holder, tok, int64(30_000_000),
		"updated", ledger, at, ""); err != nil {
		t.Fatalf("append trustline: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send entries: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	rows, err := er.AccountsByWealth(ctx, []string{"native", tok}, []string{"0.1", "0.333333333333333333"}, 500)
	if err != nil {
		t.Fatalf("AccountsByWealth: %v", err)
	}
	want, _ := new(big.Rat).SetString("1.999999999999999999")
	for _, w := range rows {
		if w.AccountID != holder {
			continue
		}
		if w.Value == nil || w.Value.Cmp(want) != 0 {
			t.Fatalf("%s value = %v, want exactly %s", holder, w.Value, want.FloatString(18))
		}
		return
	}
	t.Fatalf("ranking returned %d rows without the seeded holder", len(rows))
}

// Same-ledger tie-break: readers folding to the latest entry per key with a single-column ledger_seq tie-break pick an
// ARBITRARY row (stale value or resurrected removal); they must order by (ledger_seq, intra_ledger_seq) or the
// materialized `version` so the LAST change wins.

// argMax(balance, (ledger_seq, intra_ledger_seq)): the stale change (intra 8) sorts FIRST in the base table's ORDER BY, so
// argMax over ledger_seq alone, which keeps the first row on a tie, returns it; the later change (intra 9) must win.
func TestQueryAccountBalance_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		account  = "c24c-sibling-account-balance-GTEST"
		ledger   = uint32(71_000_001)
		staleBal = int64(100)
		finalBal = int64(200)
	)
	// Valid base64: lake-wide scans (the SAC full-history seed) base64Decode every key_xdr.
	key := base64.StdEncoding.EncodeToString([]byte("c24c-account-balance-same-ledger-key"))
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		// The LATER change (intra 9, op 1) — the final balance. Handed to the
		// writer first (adversarial), but sorts LAST in the table.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "acctbal", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			AccountID: account, Balance: finalBal,
		},
		// The EARLIER change (intra 8, op 0) — a stale mid-ledger balance. Sorts
		// FIRST, so a ledger_seq-only argMax keeps it.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "acctbal", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			AccountID: account, Balance: staleBal,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	snap, found, err := chstore.QueryAccountBalance(ctx, addr, account)
	if err != nil {
		t.Fatalf("QueryAccountBalance: %v", err)
	}
	if !found {
		t.Fatalf("QueryAccountBalance: account not found (the two seeded rows should count)")
	}
	if snap.Stroops != finalBal {
		t.Errorf("Stroops = %d, want %d (same-ledger tie resolved to a stale mid-ledger balance instead of the LAST change)", snap.Stroops, finalBal)
	}
	if snap.AtLedger != ledger {
		t.Errorf("AtLedger = %d, want %d", snap.AtLedger, ledger)
	}
}

// lakeFixturePartitionLedgers mirrors stellar.ledger_entry_changes'
// `PARTITION BY intDiv(ledger_seq, 1000000)` (deploy/clickhouse/tier1_schema.sql).
const lakeFixturePartitionLedgers = uint32(1_000_000)

// Registers a t.Cleanup deleting every ledger_entry_changes row in [from, to] and failing the test if any survive. For
// fixtures far above any chain tip: the table's max(ledger_seq) is global state that unique keys cannot scope, and walkers
// step min to max. The range must be the caller's OWN;
func purgeLakeFixtureLedgers(t *testing.T, addr string, from, to uint32) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		conn := dialClickHouse(t, ctx, "stellar")

		const countQ = `SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN ? AND ?`
		var before, after uint64
		if err := conn.QueryRow(ctx, countQ, from, to).Scan(&before); err != nil {
			t.Errorf("purge lake fixture [%d, %d]: count before: %v", from, to, err)
			return
		}
		for part := from / lakeFixturePartitionLedgers; part <= to/lakeFixturePartitionLedgers; part++ {
			q := fmt.Sprintf(`ALTER TABLE stellar.ledger_entry_changes DELETE IN PARTITION %d
				WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, part, from, to)
			if err := conn.Exec(ctx, q); err != nil {
				t.Errorf("purge lake fixture [%d, %d] in partition %d: %v", from, to, part, err)
				return
			}
		}
		if err := conn.QueryRow(ctx, countQ, from, to).Scan(&after); err != nil {
			t.Errorf("purge lake fixture [%d, %d]: count after: %v", from, to, err)
			return
		}
		if after != 0 {
			t.Errorf("purge lake fixture [%d, %d]: %d of %d rows survived the delete — every later whole-lake walk in this process will step to ledger %d",
				from, to, after, before, to)
			return
		}
		t.Logf("purged lake fixture [%d, %d]: %d of %d rows deleted", from, to, before-after, before)
	})
}

// argMax(entry_xdr, version) over ledger_entries_current: two same-ledger changes to one pool key differ only in
// intra_ledger_seq (hence `version`); a plain argMax over ledger_seq ties and can serve stale reserves.
func TestNativeLiquidityPoolsRanked_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger   = uint32(72_000_001)
		staleRes = int64(111_0000000)
		finalRes = int64(222_0000000)
	)
	// Valid base64: lake-wide scans (the SAC full-history seed) base64Decode every key_xdr.
	key := base64.StdEncoding.EncodeToString([]byte("c24c-native-lp-same-ledger-key"))
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	var poolID [32]byte
	poolID[0] = 0xB4
	wantPoolStrkey, err := strkey.Encode(strkey.VersionByteLiquidityPool, poolID[:])
	if err != nil {
		t.Fatalf("encode pool strkey: %v", err)
	}

	// Two separate inserts make two un-merged parts (optimize_on_insert would collapse duplicates within one block). A
	// ledger_seq tie resolves to the LAST-created part on the pinned image, so seeding FINAL first and STALE last makes a
	// plain query serve stale reserves, while argMax by `version` keeps the higher-version row regardless of part order.
	final := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: closeTime, TxHash: "lpfinal", OpIndex: 1, ChangeIndex: 0,
		IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "liquidity_pool", KeyXDR: key,
		EntryXDR: lpEntryB64(t, poolID, finalRes),
	}}
	stale := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: closeTime, TxHash: "lpstale", OpIndex: 0, ChangeIndex: 0,
		IntraLedgerSeq: 3, ChangeType: "updated", EntryType: "liquidity_pool", KeyXDR: key,
		EntryXDR: lpEntryB64(t, poolID, staleRes),
	}}
	if _, err := chstore.InsertEntryChanges(ctx, addr, final, 0); err != nil {
		t.Fatalf("InsertEntryChanges (final): %v", err)
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, stale, 0); err != nil {
		t.Fatalf("InsertEntryChanges (stale): %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	ranked, err := reader.NativeLiquidityPoolsRanked(ctx, 0)
	if err != nil {
		t.Fatalf("NativeLiquidityPoolsRanked: %v", err)
	}

	var found *chstore.NativeLiquidityPoolState
	for i := range ranked {
		if ranked[i].PoolStrkey == wantPoolStrkey {
			found = &ranked[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("seeded pool %s absent from ranked result", wantPoolStrkey)
	}
	if want := big.NewInt(finalRes); found.ReserveA == nil || found.ReserveA.Cmp(want) != 0 {
		t.Errorf("ReserveA = %v, want %d (same-ledger tie resolved to a stale mid-ledger reserve instead of the LAST change)", found.ReserveA, finalRes)
	}
}

// --- fixture builders (scaffolding only — assertions go through the real readers) ---

// contractIDFromSeed fills a 32-byte contract id with a single seed byte,
// consistent with mustContractStrkey(t, seed) (pg_sources_misc_test.go) so
// the id and its C-strkey refer to the same contract.
func contractIDFromSeed(seed byte) xdr.ContractId {
	var id xdr.ContractId
	id[0] = seed
	return id
}

// resDataKey mirrors the unexported clickhouse.poolDataKeyXDR key ScVal for a
// Blend PoolDataKey::ResData(asset) entry: Vec[Symbol("ResData"), Address(asset)].
func resDataKey(pool, asset xdr.ContractId) xdr.ScVal {
	a := asset
	sym := xdr.ScSymbol("ResData")
	assetAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &a}
	vec := &xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &assetAddr},
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
}

// resDataKeyB64 is the base64 LedgerKey the reader matches on (key_xdr column) —
// identical to clickhouse.poolDataKeyXDR(pool, "ResData", asset).
func resDataKeyB64(t *testing.T, pool, asset xdr.ContractId) string {
	t.Helper()
	p := pool
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &p},
			Key:        resDataKey(pool, asset),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("marshal ResData key: %v", err)
	}
	return b64
}

// resDataEntryB64 builds a Blend ResData contract_data LedgerEntry whose value is
// the ScMap blend.DecodeReserveData expects, carrying the given b_rate.
func resDataEntryB64(t *testing.T, pool, asset xdr.ContractId, ledger uint32, bRate uint64) string {
	t.Helper()
	i128 := func(v uint64) xdr.ScVal {
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(v)}}
	}
	sym := func(s string) xdr.ScVal {
		ss := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &ss}
	}
	u64 := func(v uint64) xdr.ScVal {
		u := xdr.Uint64(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
	}
	// Symbol-sorted map entries (canonical ScMap order).
	mp := &xdr.ScMap{
		{Key: sym("b_rate"), Val: i128(bRate)},
		{Key: sym("b_supply"), Val: i128(0)},
		{Key: sym("backstop_credit"), Val: i128(0)},
		{Key: sym("d_rate"), Val: i128(1_000_000_000_000)},
		{Key: sym("d_supply"), Val: i128(0)},
		{Key: sym("ir_mod"), Val: i128(10_000_000)},
		{Key: sym("last_time"), Val: u64(0)},
	}
	val := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
	p := pool
	entry := xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(ledger),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &p},
				Key:        resDataKey(pool, asset),
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal ResData entry: %v", err)
	}
	return b64
}

// lpEntryB64 builds a classic ConstantProduct liquidity_pool LedgerEntry with the
// given ReserveA (native / USDC pair) — the shape nativeLPStateFromEntry decodes.
func lpEntryB64(t *testing.T, poolID [32]byte, reserveA int64) string {
	t.Helper()
	var issuer xdr.Uint256
	copy(issuer[:], []byte("c24c-lp-usdc-issuer-seed--------"))
	var code xdr.AssetCode4
	copy(code[:], "USDC")
	assetB := xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: code, Issuer: xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &issuer}},
	}
	var pid xdr.PoolId
	copy(pid[:], poolID[:])
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeLiquidityPool,
			LiquidityPool: &xdr.LiquidityPoolEntry{
				LiquidityPoolId: pid,
				Body: xdr.LiquidityPoolEntryBody{
					Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
					ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
						Params:                   xdr.LiquidityPoolConstantProductParameters{AssetA: xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, AssetB: assetB, Fee: 30},
						ReserveA:                 xdr.Int64(reserveA),
						ReserveB:                 1_000,
						TotalPoolShares:          1_000,
						PoolSharesTrustLineCount: 5,
					},
				},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal LP entry: %v", err)
	}
	return b64
}
