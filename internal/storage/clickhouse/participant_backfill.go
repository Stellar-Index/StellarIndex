package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/StellarIndex/stellar-index/internal/xdrjson"
)

// operationParticipantRows derives the stellar.operation_participants rows for a
// single operation — the ONE participant-derivation shared by the live lake
// extractor (extract.go's extractOps) and the ch-participant-backfill, so the
// two can never drift (ADR-0038 Phase B account history).
//
// It returns one row per NON-source G-account the op body touches, as decoded
// by xdrjson.ParticipantAccounts: the payment / path-payment / account-merge
// destination, the allow-trust / set-trust-line-flags trustor, the clawback
// `from`, and any account-address argument of a Soroban invoke. Muxed (M-)
// destinations resolve to their underlying G. The op's own resolved
// source_account is deliberately EXCLUDED: it is already the full-history
// operations.source_account lake column, and the account-history reader UNIONs
// the two arms on the invariant that an op is sourced XOR has the account as a
// non-source participant (explorer_reader.AccountOperations). Writing a source
// row here would double-count that op for its own source account.
//
// Not captured (a property of the shared derivation, so live-forward and
// historical stay consistent — NOT a backfill-specific gap): asset ISSUERS
// (they render as "CODE-ISSUER", not a bare strkey — an issuer is not a
// counterparty in the received-activity sense), and the counterparties of op
// types xdrjson doesn't field-decode yet (create-claimable-balance claimants,
// sponsorship targets). Extending either is a live-path change, out of scope
// for a re-derive that must reproduce live output byte-for-byte.
//
// A malformed body_xdr returns the decode error; callers soft-skip + count it
// (resilient like the extractor). The deterministic, deduplicated, sorted
// output makes a re-derive idempotent against the ReplacingMergeTree.
func operationParticipantRows(bodyB64, opSource string, ledger uint32, closeTime time.Time, txHash string, txIndex, opIndex uint32) ([]OperationParticipantRow, error) {
	accts, err := xdrjson.ParticipantAccounts(bodyB64)
	if err != nil {
		return nil, err
	}
	if len(accts) == 0 {
		return nil, nil
	}
	rows := make([]OperationParticipantRow, 0, len(accts))
	for _, acct := range accts {
		if acct == opSource {
			continue // covered by operations.source_account — see the doc above
		}
		rows = append(rows, OperationParticipantRow{
			Account:   acct,
			LedgerSeq: ledger,
			CloseTime: closeTime,
			TxHash:    txHash,
			TxIndex:   txIndex,
			OpIndex:   opIndex,
		})
	}
	return rows, nil
}

// ParticipantBackfillStats accumulates what a BackfillOperationParticipants run
// scanned + wrote (or, in dry-run, WOULD write).
type ParticipantBackfillStats struct {
	OpsScanned   uint64 // operation rows read from stellar.operations
	Participants uint64 // non-source participant rows written (or would-write in dry-run)
	DecodeErrors uint64 // op bodies that failed to decode (soft-skipped)
}

// participantInsertBatch is how many participant rows accumulate before a
// batch INSERT is sent. Bounds the write-side buffer independently of the read
// window, so a dense post-Soroban window never materialises in the heap.
const participantInsertBatch = 50_000

// BackfillOperationParticipants fills stellar.operation_participants (the
// NON-source side of ADR-0038 Phase B account history) for the inclusive
// [from,to] ledger range by re-deriving participants from
// stellar.operations.body_xdr — a CH-INTERNAL job, NOT a multi-day Galexie
// re-walk (BACKLOG #59). operation_participants captures live-forward only;
// stellar.operations holds the full genesis→tip history WITH the op body, so
// every historical participant set is derivable in the lake. Each op is decoded
// with the SAME operationParticipantRows the live extractor uses, so the output
// is byte-identical to what live capture would have written.
//
// Windowed + resumable like BackfillTxHashIndex: one streaming
// read-decode-insert pass per `window` ledgers, progress + the exact resume
// point logged after each window (via logf), and re-running a window is
// idempotent (ReplacingMergeTree keyed on (account, ledger_seq, tx_index,
// op_index)). Memory is bounded: operation rows are STREAMED from the read
// connection and participant rows are flushed to the write connection in fixed
// batches; neither the read window nor the write buffer materialises whole.
//
// dryRun decodes + counts the participants that WOULD be written but writes
// nothing — the operator's pre-flight cost probe.
func BackfillOperationParticipants(ctx context.Context, addr string, from, to, window uint32, dryRun bool, logf func(format string, args ...any)) (ParticipantBackfillStats, error) {
	var stats ParticipantBackfillStats
	if from == 0 || to < from || window == 0 {
		return stats, fmt.Errorf("clickhouse: participant backfill: need 0 < from <= to and window > 0 (got from=%d to=%d window=%d)", from, to, window)
	}
	readConn, err := openRead(ctx, addr)
	if err != nil {
		return stats, err
	}
	defer func() { _ = readConn.Close() }()

	var writeConn driver.Conn
	if !dryRun {
		writeConn, err = openParticipantWrite(ctx, addr)
		if err != nil {
			return stats, err
		}
		defer func() { _ = writeConn.Close() }()
	}

	verb := "wrote"
	if dryRun {
		verb = "would-write"
	}
	start := time.Now()
	for lo := from; ; {
		hi := to
		if rem := to - lo; rem >= window { // window fits without uint overflow
			hi = lo + window - 1
		}
		wStart := time.Now()
		ws, werr := backfillParticipantWindow(ctx, readConn, writeConn, lo, hi, dryRun)
		stats.OpsScanned += ws.OpsScanned
		stats.Participants += ws.Participants
		stats.DecodeErrors += ws.DecodeErrors
		if werr != nil {
			return stats, fmt.Errorf("clickhouse: participant window [%d,%d]: %w — resume with -from %d", lo, hi, werr, lo)
		}
		logf("window [%d,%d] done in %s (total %s; ops=%d %s participants=%d decode-errors=%d; resume point -from %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second),
			ws.OpsScanned, verb, ws.Participants, ws.DecodeErrors, hi+1)
		if hi >= to {
			return stats, nil
		}
		lo = hi + 1
	}
}

// backfillParticipantWindow streams one [lo,hi] ledger window of stellar.operations,
// decodes each op's participant set, and (unless dryRun) batch-inserts the
// non-source participants. Returns the per-window stats.
func backfillParticipantWindow(ctx context.Context, readConn, writeConn driver.Conn, lo, hi uint32, dryRun bool) (ParticipantBackfillStats, error) {
	var stats ParticipantBackfillStats
	// No ORDER BY / no FINAL: a plain range scan streams with bounded memory. An
	// ORDER BY would engage MergeSortingTransform (the sdex-reconcile OOM class,
	// StreamSDEXOps' "NO FINAL" note); insert order is irrelevant because the
	// ReplacingMergeTree re-sorts by its own ORDER BY key, and duplicate rows
	// from un-merged parts are harmless (idempotent re-derive).
	const q = `SELECT ledger_seq, close_time, tx_hash, tx_index, op_index, source_account, body_xdr
		FROM stellar.operations
		WHERE ledger_seq >= ? AND ledger_seq <= ?`
	rows, err := readConn.Query(ctx, q, lo, hi)
	if err != nil {
		return stats, fmt.Errorf("query operations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	w := &participantWriter{conn: writeConn, dryRun: dryRun, buf: make([]OperationParticipantRow, 0, participantInsertBatch)}
	for rows.Next() {
		prs, err := scanOpParticipants(rows)
		if errors.Is(err, errOpBodyDecode) {
			stats.OpsScanned++
			stats.DecodeErrors++ // soft-skip one malformed body, keep the window going
			continue
		}
		if err != nil {
			return stats, err
		}
		stats.OpsScanned++
		stats.Participants += uint64(len(prs))
		if err := w.add(ctx, prs); err != nil {
			return stats, err
		}
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("stream operations: %w", err)
	}
	if err := w.flush(ctx); err != nil {
		return stats, err
	}
	return stats, nil
}

// errOpBodyDecode wraps an operation whose body_xdr failed to decode. The
// backfill soft-skips + counts these (resilient like the live extractor) rather
// than aborting the window — a lake op body decoded once at ingest, so a failure
// here is vanishingly rare, but one bad row must not lose the rest of a window.
var errOpBodyDecode = errors.New("clickhouse: operation body decode failed")

// scanOpParticipants reads one operation row and derives its non-source
// participants. A decode failure is returned wrapped in errOpBodyDecode (a
// soft-skip signal); any other error is a fatal row-read error.
func scanOpParticipants(rows driver.Rows) ([]OperationParticipantRow, error) {
	var (
		ledger    uint32
		closeTime time.Time
		txHash    string
		txIndex   uint32
		opIndex   uint32
		source    string
		bodyXDR   string
	)
	if err := rows.Scan(&ledger, &closeTime, &txHash, &txIndex, &opIndex, &source, &bodyXDR); err != nil {
		return nil, fmt.Errorf("scan op: %w", err)
	}
	prs, derr := operationParticipantRows(bodyXDR, source, ledger, closeTime.UTC(), txHash, txIndex, opIndex)
	if derr != nil {
		return nil, fmt.Errorf("%w (ledger %d tx %s op %d): %v", errOpBodyDecode, ledger, txHash, opIndex, derr) //nolint:errorlint // wrap the sentinel + annotate; the inner cause is informational only
	}
	return prs, nil
}

// participantWriter buffers participant rows and flushes them to ClickHouse in
// fixed batches (a no-op in dry-run). It keeps the per-op-row loop in
// backfillParticipantWindow flat.
type participantWriter struct {
	conn   driver.Conn // nil in dry-run
	dryRun bool
	buf    []OperationParticipantRow
}

// add appends an op's participant rows, flushing once the batch threshold is
// crossed. A single op contributes only a few rows, so the buffer overshoots
// participantInsertBatch by at most that handful.
func (w *participantWriter) add(ctx context.Context, rows []OperationParticipantRow) error {
	w.buf = append(w.buf, rows...)
	if len(w.buf) >= participantInsertBatch {
		return w.flush(ctx)
	}
	return nil
}

func (w *participantWriter) flush(ctx context.Context) error {
	if w.dryRun || len(w.buf) == 0 {
		w.buf = w.buf[:0]
		return nil
	}
	if err := insertParticipantBatch(ctx, w.conn, w.buf); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

// insertParticipantBatch sends one native batch of participant rows, matching
// the live Sink.flushParticipants column order exactly.
func insertParticipantBatch(ctx context.Context, conn driver.Conn, rows []OperationParticipantRow) error {
	b, err := conn.PrepareBatch(ctx, "INSERT INTO stellar.operation_participants (account, ledger_seq, close_time, tx_hash, tx_index, op_index)")
	if err != nil {
		return fmt.Errorf("prepare operation_participants: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(r.Account, r.LedgerSeq, r.CloseTime, r.TxHash, r.TxIndex, r.OpIndex); err != nil {
			return fmt.Errorf("append participant %s/%s/%d: %w", r.Account, r.TxHash, r.OpIndex, err)
		}
	}
	return wrapSend(b.Send(), "operation_participants")
}

// openParticipantWrite dials ClickHouse for the participant backfill's batch
// INSERTs — the cheap-append write class (a finite execution ceiling), distinct
// from openRead's heavy-FINAL read class. Kept separate from the streaming read
// pool so a flush never contends with the open read cursor.
func openParticipantWrite(ctx context.Context, addr string) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
		Settings: clickhouse.Settings{
			// A generous-but-finite time ceiling: this is the WRITE path (cheap
			// appends into a ReplacingMergeTree), mirroring the live Sink.
			"max_execution_time": 300,
		},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open write %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping write %s: %w", addr, err)
	}
	return conn, nil
}

// MinParticipantLedger returns the lowest ledger present in
// stellar.operation_participants — the live-capture floor. ok=false when the
// table is empty (no live capture yet), so the caller can fall back to the lake
// tip. The historical backfill targets [genesis, floor-1] by default.
func MinParticipantLedger(ctx context.Context, addr string) (uint32, bool, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	var (
		cnt uint64
		lo  uint64
	)
	if err := conn.QueryRow(ctx,
		`SELECT toUInt64(count()), toUInt64(min(ledger_seq)) FROM stellar.operation_participants`).Scan(&cnt, &lo); err != nil {
		return 0, false, fmt.Errorf("clickhouse: min participant ledger: %w", err)
	}
	if cnt == 0 {
		return 0, false, nil // empty table → min() is a meaningless 0
	}
	return uint32(lo), true, nil
}
