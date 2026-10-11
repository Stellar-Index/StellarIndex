package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// operationParticipantRows derives the stellar.operation_participants rows for one
// operation: the ONE derivation shared by the live lake extractor (extractOps) and
// ch-participant-backfill, so the two cannot drift (ADR-0038 Phase B).
//
// It returns one row per NON-source G-account the op body touches, as decoded by
// xdrjson.ParticipantAccounts. Soroban InvokeContract ops contribute NO
// participant: their args and auth entries are attacker-controllable at decode time.
//
// The op's own source_account is EXCLUDED: it is already in operations.source_account,
// and the account-history reader UNIONs the two arms on the invariant that an op is
// sourced XOR has the account as a non-source participant
// (explorer_reader.AccountOperations); a source row would double-count it.
//
// Asset ISSUERS and counterparties of op types xdrjson doesn't field-decode yet are
// not captured, so live and historical stay consistent; a re-derive must reproduce
// live output.
//
// A malformed body_xdr returns the decode error; callers soft-skip and count it.
// Sorted, deduplicated output makes a re-derive idempotent under the RMT.
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
	Participants uint64 // non-source participant rows derived; all written (or would-write in dry-run) only when the run returned nil
	DecodeErrors uint64 // op bodies that failed to decode (soft-skipped)
}

// participantInsertBatch is how many participant rows accumulate before a
// batch INSERT is sent. Bounds the write-side buffer independently of the read
// window, so a dense post-Soroban window never materialises in the heap.
const participantInsertBatch = 50_000

// BackfillOperationParticipants fills stellar.operation_participants (the
// NON-source side of ADR-0038 Phase B account history) for the inclusive
// [from,to] ledger range by re-deriving participants from
// stellar.operations.body_xdr — a CH-INTERNAL job, not a Galexie re-walk.
// Each op is decoded with the SAME operationParticipantRows the live
// extractor uses, so output is byte-identical to live capture.
//
// Windowed + resumable like BackfillTxHashIndex: progress and the exact
// resume point are logged after each window, and re-running a window is
// idempotent (ReplacingMergeTree keyed on (account, ledger_seq, tx_index,
// op_index)). Memory is bounded: rows are STREAMED and flushed in fixed
// batches.
//
// dryRun decodes + counts the participants that WOULD be written but writes
// nothing.
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
		hi := ledgerWindowHi(lo, to, window)
		wStart := time.Now()
		ws, werr := backfillParticipantWindow(ctx, readConn, writeConn, lo, hi, dryRun)
		stats.OpsScanned += ws.OpsScanned
		stats.Participants += ws.Participants
		stats.DecodeErrors += ws.DecodeErrors
		if werr != nil {
			return stats, fmt.Errorf("clickhouse: participant window [%d,%d]: %w — resume with -from %d -to %d", lo, hi, werr, lo, to)
		}
		logf("window [%d,%d] done in %s (total %s; ops=%d %s participants=%d decode-errors=%d; resume point -from %d -to %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second),
			ws.OpsScanned, verb, ws.Participants, ws.DecodeErrors, hi+1, to)
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
	// Identity from the environment; see ops_auth.go.
	auth, err := chAuth()
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: auth,
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
