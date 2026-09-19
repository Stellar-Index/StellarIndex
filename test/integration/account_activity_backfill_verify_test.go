//go:build integration

package integration_test

import (
	"context"
	"fmt"
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
)

// TestClickHouseAccountActivityBackfillVerifySeesGapInLaterWindow executes
// the SQL of deploy/clickhouse/account_activity.sql's operator runbook — the
// Step-2 backfill INSERTs and the Step-3 verify, extracted from the file, not
// copied — against a live ClickHouse (F112).
//
// stellar.account_activity feeds a HARD `ledger_seq <= watermark` bound on
// the account-history readers, so a Step-2 backfill that skipped a window
// silently hides history. Step 3 is the only thing standing between a
// partial backfill and the reader deploy, and both of its earlier shapes
// were blind to a gap outside the first window: sampling recently active
// accounts tests what the live MVs cover anyway, and sampling the accounts
// with the OLDEST last activity tests only window 1.
//
// Fixture (the one the blind shapes return 0 on): window 1 backfilled, a
// LATER window skipped, an account active in both — in the later window
// only as a NON-SOURCE participant, the role a narrowed check would miss.
// The verify must count it (> 0) and must return 0 once the skipped window
// is re-run.
func TestClickHouseAccountActivityBackfillVerifySeesGapInLaterWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	rb := loadAccountActivityRunbook(t)

	// Two adjacent windows of the runbook's own grid (2 + k*2M), far from
	// any ledger another integration test seeds. Rows go straight into the
	// three source tables — no stellar.ledgers row, so no other test's
	// "tip" anchor moves.
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
