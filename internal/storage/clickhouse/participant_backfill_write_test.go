package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ── write-side doubles ──────────────────────────────────────────────────────

// stubBatch records the rows appended to one prepared batch and whether it was
// sent. The embedded interface panics on any method this write path should
// never touch.
type stubBatch struct {
	driver.Batch
	rows    [][]any
	sent    bool
	sendErr error
}

func (b *stubBatch) Append(v ...any) error {
	row := make([]any, len(v))
	copy(row, v)
	b.rows = append(b.rows, row)
	return nil
}

func (b *stubBatch) Send() error {
	b.sent = true
	return b.sendErr
}

// batchConn is a write-capable connection double: it records every
// PrepareBatch statement and hands back a stubBatch per call, so a test can
// count FLUSHES (one batch per flush) as well as rows.
//
// It composes with stubConn for the read half — backfillParticipantWindow
// takes its read and write connections separately, which is exactly what makes
// the write side testable without a server.
type batchConn struct {
	driver.Conn
	statements []string
	batches    []*stubBatch
	prepareErr error
	sendErr    error
}

func (c *batchConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.statements = append(c.statements, query)
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	b := &stubBatch{sendErr: c.sendErr}
	c.batches = append(c.batches, b)
	return b, nil
}

// rowsSent totals the rows across every batch that was actually Send()-ed.
func (c *batchConn) rowsSent() int {
	n := 0
	for _, b := range c.batches {
		if b.sent {
			n += len(b.rows)
		}
	}
	return n
}

func participantRows(n int) []OperationParticipantRow {
	out := make([]OperationParticipantRow, n)
	for i := range out {
		out[i] = OperationParticipantRow{
			Account:   testDest,
			LedgerSeq: uint32(i), //nolint:gosec // small test index
			CloseTime: time.Unix(1_700_000_000, 0).UTC(),
			TxHash:    "aa",
			TxIndex:   1,
			OpIndex:   0,
		}
	}
	return out
}

// ── participantWriter ───────────────────────────────────────────────────────

// TestParticipantWriter_FlushesOnBothPaths is the writer-flush contract, and
// it has TWO halves that fail independently:
//
//   - the FULL-BATCH path: crossing participantInsertBatch sends mid-stream, so
//     a dense window never materialises in the heap (the whole reason the
//     threshold exists);
//   - the END-OF-STREAM path: whatever is left in the buffer when the window
//     ends is sent too. A writer that only flushed on the threshold would drop
//     the final partial batch of EVERY window — up to 49,999 participant rows
//     each — while the job reported the full would-write count.
func TestParticipantWriter_FlushesOnBothPaths(t *testing.T) {
	ctx := t.Context()
	conn := &batchConn{}
	w := &participantWriter{conn: conn, buf: make([]OperationParticipantRow, 0, participantInsertBatch)}

	// One row short of the threshold: nothing sent yet.
	if err := w.add(ctx, participantRows(participantInsertBatch-1)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(conn.batches) != 0 {
		t.Fatalf("flushed after %d rows, want no flush below the %d threshold",
			participantInsertBatch-1, participantInsertBatch)
	}

	// Crossing the threshold flushes exactly once, for exactly the buffer.
	if err := w.add(ctx, participantRows(1)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(conn.batches) != 1 {
		t.Fatalf("full-batch path produced %d batches, want 1", len(conn.batches))
	}
	if !conn.batches[0].sent {
		t.Error("the full batch was prepared but never sent")
	}
	if got := len(conn.batches[0].rows); got != participantInsertBatch {
		t.Errorf("full batch carried %d rows, want %d", got, participantInsertBatch)
	}

	// A tail below the threshold sits in the buffer …
	if err := w.add(ctx, participantRows(7)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(conn.batches) != 1 {
		t.Fatalf("a %d-row tail triggered a flush; want it buffered until end of stream", 7)
	}
	// … until the end-of-stream flush.
	if err := w.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(conn.batches) != 2 {
		t.Fatalf("end-of-stream flush produced %d batches, want 2", len(conn.batches))
	}
	if got := len(conn.batches[1].rows); got != 7 {
		t.Errorf("tail batch carried %d rows, want 7 — the final partial batch must not be dropped", got)
	}
	if total, want := conn.rowsSent(), participantInsertBatch+7; total != want {
		t.Errorf("wrote %d rows in total, want %d (every row added must reach the server)", total, want)
	}
}

// TestParticipantWriter_FlushIsIdempotentAndSkipsEmpty — the window loop calls
// flush unconditionally at end of stream, including for an EMPTY window (a
// sparse ledger range with no participants). Preparing and sending an empty
// batch there is a pointless round trip against a table this job hammers.
func TestParticipantWriter_FlushIsIdempotentAndSkipsEmpty(t *testing.T) {
	ctx := t.Context()
	conn := &batchConn{}
	w := &participantWriter{conn: conn}

	if err := w.flush(ctx); err != nil {
		t.Fatalf("flush(empty): %v", err)
	}
	if len(conn.statements) != 0 {
		t.Fatalf("flushing an empty buffer prepared %d batches, want 0", len(conn.statements))
	}

	if err := w.add(ctx, participantRows(3)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := w.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// A second flush must not re-send the rows the first one already wrote:
	// the buffer is truncated on flush, so double-writing would double-count
	// the job's own output on any path that flushes twice.
	if err := w.flush(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := conn.rowsSent(); got != 3 {
		t.Errorf("wrote %d rows across two flushes, want 3 — the buffer must be cleared on flush", got)
	}
}

// TestParticipantWriter_DryRunWritesNothing — dry run is the operator's
// pre-flight cost probe. It must not prepare a batch at all: writeConn is nil
// on that path, so any attempt to use it is a nil dereference in production.
func TestParticipantWriter_DryRunWritesNothing(t *testing.T) {
	ctx := t.Context()
	w := &participantWriter{conn: nil, dryRun: true}
	if err := w.add(ctx, participantRows(participantInsertBatch+10)); err != nil {
		t.Fatalf("dry-run add: %v", err)
	}
	if err := w.flush(ctx); err != nil {
		t.Fatalf("dry-run flush: %v", err)
	}
	if len(w.buf) != 0 {
		t.Errorf("dry-run left %d rows buffered, want the buffer released", len(w.buf))
	}
}

// TestParticipantWriter_SendFailurePropagates — a failed flush must abort the
// window so the operator resumes from its start. Swallowing it would report a
// completed window whose rows were never written.
func TestParticipantWriter_SendFailurePropagates(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("too many parts")
	conn := &batchConn{sendErr: boom}
	w := &participantWriter{conn: conn}
	if err := w.add(ctx, participantRows(1)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := w.flush(ctx); err == nil {
		t.Fatal("flush swallowed a Send failure")
	}

	prepFail := &batchConn{prepareErr: errors.New("table is read-only")}
	w2 := &participantWriter{conn: prepFail}
	if err := w2.add(ctx, participantRows(1)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := w2.flush(ctx); err == nil {
		t.Fatal("flush swallowed a PrepareBatch failure")
	}
}

// TestInsertParticipantBatch_MatchesLiveSinkColumnOrder — the backfill and the
// live extractor write the SAME table, and Append is POSITIONAL. If the two
// column lists ever diverge, the backfill silently writes ledger_seq into
// tx_index (and so on) for the whole of history, with no error anywhere. The
// live statement is Sink.flushParticipants'.
func TestInsertParticipantBatch_MatchesLiveSinkColumnOrder(t *testing.T) {
	const liveStatement = "INSERT INTO stellar.operation_participants (account, ledger_seq, close_time, tx_hash, tx_index, op_index)"

	conn := &batchConn{}
	row := OperationParticipantRow{
		Account:   testDest,
		LedgerSeq: 60_000_001,
		CloseTime: time.Unix(1_700_000_000, 0).UTC(),
		TxHash:    "abc",
		TxIndex:   4,
		OpIndex:   2,
	}
	if err := insertParticipantBatch(t.Context(), conn, []OperationParticipantRow{row}); err != nil {
		t.Fatalf("insertParticipantBatch: %v", err)
	}
	if len(conn.statements) != 1 {
		t.Fatalf("prepared %d statements, want 1", len(conn.statements))
	}
	if conn.statements[0] != liveStatement {
		t.Fatalf("backfill INSERT statement has drifted from Sink.flushParticipants:\n backfill: %s\n live:     %s",
			conn.statements[0], liveStatement)
	}
	// The appended tuple must be in that declared column order.
	want := []any{row.Account, row.LedgerSeq, row.CloseTime, row.TxHash, row.TxIndex, row.OpIndex}
	if got := conn.batches[0].rows[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("appended tuple = %v, want %v (positional Append must follow the column list)", got, want)
	}
}

// ── window read/decode/write loop ───────────────────────────────────────────

// opRow builds one stellar.operations row in backfillParticipantWindow's
// projection order.
func opRow(ledger uint32, txHash string, txIndex, opIndex uint32, source, body string) []any {
	return []any{ledger, time.Unix(1_700_000_000, 0).UTC(), txHash, txIndex, opIndex, source, body}
}

func paymentToDest(t *testing.T) string {
	t.Helper()
	return opBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(testDest),
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      xdr.Int64(1),
	})
}

// TestBackfillParticipantWindow_ScansDecodesAndWrites drives the whole window
// loop: read rows, derive participants, batch-write, report stats.
func TestBackfillParticipantWindow_ScansDecodesAndWrites(t *testing.T) {
	body := paymentToDest(t)
	read := &stubConn{respond: func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "stellar.operations") {
			t.Fatalf("unexpected read query: %s", q)
		}
		return &stubRows{data: [][]any{
			opRow(100, "aa", 0, 0, testSource, body),
			opRow(101, "bb", 1, 0, testSource, body),
			// The op's OWN source as destination: contributes NO participant
			// row (it is already the operations.source_account column, and
			// writing it would double-count the op for its own account).
			opRow(102, "cc", 0, 0, testDest, body),
		}}, nil
	}}
	write := &batchConn{}

	stats, err := backfillParticipantWindow(t.Context(), read, write, 100, 102, false)
	if err != nil {
		t.Fatalf("backfillParticipantWindow: %v", err)
	}
	if stats.OpsScanned != 3 {
		t.Errorf("OpsScanned = %d, want 3", stats.OpsScanned)
	}
	if stats.Participants != 2 {
		t.Errorf("Participants = %d, want 2 (the self-sourced op contributes none)", stats.Participants)
	}
	if stats.DecodeErrors != 0 {
		t.Errorf("DecodeErrors = %d, want 0", stats.DecodeErrors)
	}
	// End-of-stream flush actually happened, and wrote what the stats claim.
	if got := write.rowsSent(); got != int(stats.Participants) {
		t.Errorf("wrote %d rows but reported %d participants — the two must agree", got, stats.Participants)
	}
}

// TestBackfillParticipantWindow_BindsTheWindowInOrder pins the read query's
// predicate and its arguments together. A `>= ? AND <= ?` whose args arrive
// (hi, lo) reads an EMPTY window and the job reports a clean zero-participant
// pass over a range it never looked at.
func TestBackfillParticipantWindow_BindsTheWindowInOrder(t *testing.T) {
	read := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	if _, err := backfillParticipantWindow(t.Context(), read, nil, 60_000_000, 60_249_999, true); err != nil {
		t.Fatalf("backfillParticipantWindow: %v", err)
	}
	if len(read.queries) != 1 {
		t.Fatalf("issued %d read queries, want 1 per window", len(read.queries))
	}
	q := read.queries[0]
	if !strings.Contains(q, "WHERE ledger_seq >= ? AND ledger_seq <= ?") {
		t.Errorf("window predicate missing or reshaped:\n%s", q)
	}
	if got, want := read.args[0], []any{uint32(60_000_000), uint32(60_249_999)}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound args = %v, want %v (lo then hi, matching the clause order)", got, want)
	}
	// No ORDER BY and no FINAL: either engages a sort/merge transform over the
	// window, which is the documented OOM class for this scan. Insert order is
	// irrelevant — the target ReplacingMergeTree re-sorts by its own key.
	if strings.Contains(q, "ORDER BY") {
		t.Errorf("window read gained an ORDER BY (MergeSortingTransform, the sdex-reconcile OOM class):\n%s", q)
	}
	if strings.Contains(q, "FINAL") {
		t.Errorf("window read gained FINAL (partition-scale merge; duplicates here are harmless):\n%s", q)
	}
	// The projection must be exactly the seven columns scanOpParticipants
	// scans, in order — Scan is positional.
	for _, col := range []string{"ledger_seq", "close_time", "tx_hash", "tx_index", "op_index", "source_account", "body_xdr"} {
		if !strings.Contains(q, col) {
			t.Errorf("window read no longer projects %q:\n%s", col, q)
		}
	}
}

// TestBackfillParticipantWindow_SoftSkipsUndecodableBodies — one malformed
// body must not lose the rest of the window (a lake body decoded once at
// ingest, so a failure here is vanishingly rare but must be survivable). It is
// COUNTED, so the operator sees it rather than it vanishing.
func TestBackfillParticipantWindow_SoftSkipsUndecodableBodies(t *testing.T) {
	body := paymentToDest(t)
	read := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{
			opRow(1, "aa", 0, 0, testSource, body),
			opRow(2, "bb", 0, 0, testSource, "!!!not-xdr!!!"),
			opRow(3, "cc", 0, 0, testSource, body),
		}}, nil
	}}
	write := &batchConn{}

	stats, err := backfillParticipantWindow(t.Context(), read, write, 1, 3, false)
	if err != nil {
		t.Fatalf("a single malformed body aborted the window: %v", err)
	}
	if stats.OpsScanned != 3 {
		t.Errorf("OpsScanned = %d, want 3 — the bad row is scanned, not skipped from the count", stats.OpsScanned)
	}
	if stats.DecodeErrors != 1 {
		t.Errorf("DecodeErrors = %d, want 1 — a soft-skipped row must be visible to the operator", stats.DecodeErrors)
	}
	if stats.Participants != 2 {
		t.Errorf("Participants = %d, want 2 — the rows after the bad one must still be derived", stats.Participants)
	}
	if got := write.rowsSent(); got != 2 {
		t.Errorf("wrote %d rows, want 2", got)
	}
}

// TestBackfillParticipantWindow_DryRunCountsWithoutWriting — dryRun passes a
// nil write connection, so this also proves nothing on the write path is
// touched.
func TestBackfillParticipantWindow_DryRunCountsWithoutWriting(t *testing.T) {
	body := paymentToDest(t)
	read := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{
			opRow(1, "aa", 0, 0, testSource, body),
			opRow(2, "bb", 0, 0, testSource, body),
		}}, nil
	}}
	stats, err := backfillParticipantWindow(t.Context(), read, nil, 1, 2, true)
	if err != nil {
		t.Fatalf("dry-run window: %v", err)
	}
	if stats.Participants != 2 {
		t.Errorf("Participants = %d, want 2 — dry run still COUNTS what it would write", stats.Participants)
	}
}

// TestBackfillParticipantWindow_TruncatedStreamIsAnError — a window whose read
// dies part-way must fail, so the caller's error names it and the operator
// re-runs it. Reporting success would leave a permanent hole in
// operation_participants that nothing later detects: the job is idempotent and
// resumable precisely so a failed window can be redone.
func TestBackfillParticipantWindow_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("memory limit exceeded mid-scan")
	read := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{
			data:      [][]any{opRow(1, "aa", 0, 0, testSource, paymentToDest(t))},
			streamErr: truncated,
		}, nil
	}}
	_, err := backfillParticipantWindow(t.Context(), read, &batchConn{}, 1, 2, false)
	if err == nil {
		t.Fatal("a truncated window read reported success")
	}
	if !errors.Is(err, truncated) {
		t.Errorf("err = %v, want it to wrap %v", err, truncated)
	}
}

// TestBackfillParticipantWindow_QueryFailurePropagates.
func TestBackfillParticipantWindow_QueryFailurePropagates(t *testing.T) {
	boom := errors.New("no such table")
	read := &stubConn{respond: func(string) (driver.Rows, error) { return nil, boom }}
	if _, err := backfillParticipantWindow(t.Context(), read, nil, 1, 2, true); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

// TestScanOpParticipants_SoftSkipSentinel pins the signal
// backfillParticipantWindow branches on. A decode failure must be
// errors.Is-matchable as errOpBodyDecode; anything else is fatal. Losing the
// sentinel turns every malformed body into an aborted window.
func TestScanOpParticipants_SoftSkipSentinel(t *testing.T) {
	bad := &stubRows{data: [][]any{opRow(9, "zz", 0, 0, testSource, "not-xdr")}}
	if !bad.Next() {
		t.Fatal("stub has no row")
	}
	_, err := scanOpParticipants(bad)
	if !errors.Is(err, errOpBodyDecode) {
		t.Fatalf("err = %v, want errors.Is(err, errOpBodyDecode) — the soft-skip branch keys on it", err)
	}

	good := &stubRows{data: [][]any{opRow(9, "zz", 0, 0, testSource, paymentToDest(t))}}
	if !good.Next() {
		t.Fatal("stub has no row")
	}
	rows, err := scanOpParticipants(good)
	if err != nil {
		t.Fatalf("scanOpParticipants(valid): %v", err)
	}
	if len(rows) != 1 || rows[0].Account != testDest {
		t.Fatalf("derived %+v, want one row for %s", rows, testDest)
	}
	// close_time is normalised to UTC on the way into the row.
	if rows[0].CloseTime.Location() != time.UTC {
		t.Errorf("CloseTime location = %v, want UTC", rows[0].CloseTime.Location())
	}
}
