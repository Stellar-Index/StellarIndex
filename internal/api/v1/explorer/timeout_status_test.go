package explorer

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin the behaviour: a lake read that blows
// explorerReadTimeout must be served as a 503 `…-timeout` problem+json, NOT the
// `errors/internal` 500 every explorer handler would otherwise emit. The live symptom
// was GET /v1/contracts/{id}/code-history returning
// `500 {"type":".../errors/internal","title":"Internal error"}` at exactly 8.0s
// for every contract (API log: `explorer ContractCodeHistory failed
// err="context deadline exceeded"`), which told callers "we broke" when the
// truthful answer was "this exceeded our time budget" — and buried a capacity
// problem in the same bucket as real server bugs for the 5xx SLA probe.
//
// 503 (not 504) is the codebase's existing convention for a server-side read
// deadline — see writeReadTimeout's doc comment and v1's handlerTimedOut call
// sites — and every explorer route already declares 503 in the OpenAPI spec.

// timeoutReader is a capReader whose lake reads all fail the way ClickHouse
// fails one that outran its context: the driver propagates the cancellation and
// the error unwraps to context.DeadlineExceeded (exactly what the production log
// line showed). Only the methods the tested handlers call are overridden; the
// rest fall through to capReader's harmless zero values.
type timeoutReader struct{ *capReader }

func (r *timeoutReader) RecentLedgers(context.Context, int, uint32) ([]clickhouse.LedgerHeader, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) LedgerBySeq(context.Context, uint32) (clickhouse.LedgerHeader, bool, error) {
	return clickhouse.LedgerHeader{}, false, context.DeadlineExceeded
}

func (r *timeoutReader) LedgerTransactions(context.Context, uint32, int) ([]clickhouse.TxSummary, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) OperationsByLedger(context.Context, uint32, int) ([]clickhouse.OpRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) RecentOperations(context.Context, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) NetworkThroughput(context.Context, int) ([]clickhouse.ThroughputBucket, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) TransactionByHash(context.Context, string) (clickhouse.TxSummary, bool, error) {
	return clickhouse.TxSummary{}, false, context.DeadlineExceeded
}

func (r *timeoutReader) ContractEventsRecent(context.Context, string, int, clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) ContractWasm(context.Context, string) (clickhouse.ContractWasmInfo, error) {
	return clickhouse.ContractWasmInfo{}, context.DeadlineExceeded
}

func (r *timeoutReader) RecentContracts(context.Context, int, uint32) ([]clickhouse.ContractDirectoryRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) ContractInteractions(context.Context, string, int, uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	return nil, 0, context.DeadlineExceeded
}

func (r *timeoutReader) ContractCodeHistory(context.Context, string) ([]clickhouse.ContractCodeVersion, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, clickhouse.ExplorerCursor, error) {
	return nil, clickhouse.ExplorerCursor{}, context.DeadlineExceeded
}

func (r *timeoutReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, clickhouse.ExplorerCursor, error) {
	return nil, clickhouse.ExplorerCursor{}, context.DeadlineExceeded
}

func (r *timeoutReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, context.DeadlineExceeded
}

func (r *timeoutReader) AssetHolders(context.Context, string, int) ([]clickhouse.AssetHolder, int64, error) {
	return nil, 0, context.DeadlineExceeded
}

func (r *timeoutReader) AccountMovements(context.Context, string, int, clickhouse.AccountMovementCursor, clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) BlendPoolReserves(context.Context, string, blend.PoolVersion, []string, map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	return nil, context.DeadlineExceeded
}

// problemRecord is the problem+json a handler asked WriteProblem to emit.
type problemRecord struct {
	typeURL string
	title   string
	status  int
	detail  string
	written bool
}

// newTimeoutHandler wires a Handler over timeoutReader with a WriteProblem that
// records the full problem shape (newProbeHandler's stub keeps only the status).
func newTimeoutHandler(rec *problemRecord) *Handler {
	h := newProbeHandler(&timeoutReader{capReader: &capReader{probe: &deadlineProbe{}}}, nil)
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		*rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}
	return h
}

// TestReadTimedOut covers both arms of the deadline predicate — mirrors
// TestHandlerTimedOut in internal/api/v1/envelope_test.go, which pins the same
// contract for the v1-package handlers.
func TestReadTimedOut(t *testing.T) {
	t.Run("wrapped DeadlineExceeded on a live context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if !readTimedOut(ctx, context.DeadlineExceeded) {
			t.Error("readTimedOut(live ctx, DeadlineExceeded) = false, want true")
		}
	})

	t.Run("driver-phrased error on an expired context", func(t *testing.T) {
		// ClickHouse can return its own server-side cancellation text rather
		// than something that unwraps to context.DeadlineExceeded; the per-call
		// context is the reliable signal.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		chErr := errors.New("code: 159, message: Timeout exceeded: elapsed 8.001 seconds")
		if !readTimedOut(ctx, chErr) {
			t.Error("readTimedOut(deadlined ctx, driver timeout text) = false, want true")
		}
	})

	t.Run("client cancellation is not a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if readTimedOut(ctx, context.Canceled) {
			t.Error("readTimedOut(cancelled ctx, Canceled) = true, want false")
		}
	})

	t.Run("plain storage error on a live context is not a timeout", func(t *testing.T) {
		if readTimedOut(context.Background(), errors.New("storage broke")) {
			t.Error("readTimedOut(live ctx, plain err) = true, want false")
		}
	})
}

func (r *timeoutReader) ContractStats(context.Context) (clickhouse.ContractStats, error) {
	return clickhouse.ContractStats{}, context.DeadlineExceeded
}

func (r *timeoutReader) ContractTypes(context.Context, []string) (map[string]bool, error) {
	return nil, context.DeadlineExceeded
}
