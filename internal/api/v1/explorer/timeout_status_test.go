package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin C-F1 (audit-2026-07-23): a lake read that blows
// explorerReadTimeout must be served as a 503 `…-timeout` problem+json, NOT the
// `errors/internal` 500 every explorer handler used to emit. The live symptom
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

func (r *timeoutReader) ContractInteractions(context.Context, string, int, uint32) ([]clickhouse.ContractEdgeRow, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) ContractCodeHistory(context.Context, string) ([]clickhouse.ContractCodeVersion, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error) {
	return nil, context.DeadlineExceeded
}

func (r *timeoutReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return nil, context.DeadlineExceeded
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

func (r *timeoutReader) BlendPoolReserves(context.Context, string, []string, map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
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

// TestExplorerReads_DeadlineMapsTo503 is the C-F1 regression guard. For every
// lake-backed explorer handler, a read that returns context.DeadlineExceeded
// must produce 503 + a `…-timeout` problem type. Against the un-fixed code every
// case yields 500 `errors/internal`.
func TestExplorerReads_DeadlineMapsTo503(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		pathVals map[string]string
		call     func(h *Handler, w http.ResponseWriter, r *http.Request)
		wantType string
	}{
		{
			"ContractCodeHistory", "/v1/contracts/" + validTestContract + "/code-history",
			map[string]string{"contract_id": validTestContract},
			(*Handler).ContractCodeHistory,
			"https://api.stellarindex.io/errors/contract-code-history-timeout",
		},
		{
			"ContractInteractions", "/v1/contracts/" + validTestContract + "/interactions",
			map[string]string{"contract_id": validTestContract},
			(*Handler).ContractInteractions,
			"https://api.stellarindex.io/errors/contract-interactions-timeout",
		},
		{
			"ContractsList", "/v1/contracts", nil, (*Handler).ContractsList,
			"https://api.stellarindex.io/errors/contracts-timeout",
		},
		{
			"ContractDetail", "/v1/contracts/" + validTestContract,
			map[string]string{"contract_id": validTestContract},
			(*Handler).ContractDetail,
			"https://api.stellarindex.io/errors/contract-detail-timeout",
		},
		{
			"ContractWasm", "/v1/contracts/" + validTestContract + "/wasm",
			map[string]string{"contract_id": validTestContract},
			(*Handler).ContractWasm,
			"https://api.stellarindex.io/errors/contract-wasm-timeout",
		},
		{
			"LedgersList", "/v1/ledgers", nil, (*Handler).LedgersList,
			"https://api.stellarindex.io/errors/ledgers-timeout",
		},
		{
			"LedgerDetail", "/v1/ledgers/42",
			map[string]string{"seq": "42"},
			(*Handler).LedgerDetail,
			"https://api.stellarindex.io/errors/ledger-detail-timeout",
		},
		{
			"LedgerTransactions", "/v1/ledgers/42/transactions",
			map[string]string{"seq": "42"},
			(*Handler).LedgerTransactions,
			"https://api.stellarindex.io/errors/ledger-transactions-timeout",
		},
		{
			"OperationsByLedger", "/v1/operations?ledger=42", nil, (*Handler).Operations,
			"https://api.stellarindex.io/errors/operations-timeout",
		},
		{
			"OperationsDirectory", "/v1/operations", nil, (*Handler).Operations,
			"https://api.stellarindex.io/errors/operations-timeout",
		},
		{
			"NetworkThroughput", "/v1/network/throughput", nil, (*Handler).NetworkThroughput,
			"https://api.stellarindex.io/errors/network-throughput-timeout",
		},
		{
			"TxDetail", "/v1/tx/" + validTestTxHash,
			map[string]string{"hash": validTestTxHash},
			(*Handler).TxDetail,
			"https://api.stellarindex.io/errors/tx-detail-timeout",
		},
		{
			"AccountTransactions", "/v1/accounts/" + validTestAccount + "/transactions",
			map[string]string{"g_strkey": validTestAccount},
			(*Handler).AccountTransactions,
			"https://api.stellarindex.io/errors/account-transactions-timeout",
		},
		{
			"AccountOperations", "/v1/accounts/" + validTestAccount + "/operations",
			map[string]string{"g_strkey": validTestAccount},
			(*Handler).AccountOperations,
			"https://api.stellarindex.io/errors/account-operations-timeout",
		},
		{
			"AccountState", "/v1/accounts/" + validTestAccount,
			map[string]string{"g_strkey": validTestAccount},
			(*Handler).AccountState,
			"https://api.stellarindex.io/errors/account-state-timeout",
		},
		{
			"AccountMovements", "/v1/accounts/" + validTestAccount + "/movements",
			map[string]string{"g_strkey": validTestAccount},
			(*Handler).AccountMovements,
			"https://api.stellarindex.io/errors/account-movements-timeout",
		},
		{
			"AssetHolders", "/v1/assets/native/holders",
			map[string]string{"asset_id": "native"},
			(*Handler).AssetHolders,
			"https://api.stellarindex.io/errors/asset-holders-timeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec problemRecord
			h := newTimeoutHandler(&rec)

			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			for k, v := range tc.pathVals {
				req.SetPathValue(k, v)
			}
			w := httptest.NewRecorder()
			tc.call(h, w, req)

			if !rec.written {
				t.Fatalf("%s: no problem+json written — the handler swallowed the deadline", tc.name)
			}
			if rec.status != http.StatusServiceUnavailable {
				t.Fatalf("%s: status = %d (%q / %q), want 503 — a read deadline is not an internal error (C-F1)",
					tc.name, rec.status, rec.typeURL, rec.title)
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s: response code = %d, want 503", tc.name, w.Code)
			}
			if rec.typeURL != tc.wantType {
				t.Fatalf("%s: problem type = %q, want %q", tc.name, rec.typeURL, tc.wantType)
			}
			if !strings.Contains(rec.title, "timed out") {
				t.Fatalf("%s: title = %q, want a 'timed out' headline", tc.name, rec.title)
			}
			// The detail must name the budget that was blown, so an operator
			// reading a 503 knows it was the 8s explorer ceiling and not some
			// upstream proxy timeout.
			if !strings.Contains(rec.detail, explorerReadTimeout.String()) {
				t.Fatalf("%s: detail = %q, want it to name the %v read budget",
					tc.name, rec.detail, explorerReadTimeout)
			}
		})
	}
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
