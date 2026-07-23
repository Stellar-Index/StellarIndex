package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin C3-1 (audit-2026-07-16): every explorer handler that reads
// the shared 8-conn ClickHouse pool (ExplorerReader) MUST bound the read in a
// request-scoped context.WithTimeout(explorerReadTimeout) so a handful of slow
// unauthenticated requests can't hold every connection open and wedge every
// lake-backed endpoint (the server WriteTimeout does not cancel an in-flight
// query). The regression these guard against is a handler passing raw
// r.Context() — which, absent middleware, carries NO deadline — straight to the
// reader.

// deadlineProbe records the deadline on the FIRST reader call a handler makes
// (all of a handler's reads share the same context, and the first sees the full
// budget). A handler that wraps its reads in context.WithTimeout hands the
// reader a context whose Deadline() is set ~explorerReadTimeout out; the
// un-fixed handler hands it r.Context(), which has no deadline.
type deadlineProbe struct {
	mu      sync.Mutex
	sawCall bool
	hasDL   bool
	budget  time.Duration
}

func (p *deadlineProbe) record(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sawCall {
		return
	}
	p.sawCall = true
	if dl, ok := ctx.Deadline(); ok {
		p.hasDL = true
		p.budget = time.Until(dl)
	}
}

// capReader is a full ExplorerReader that records the context deadline on every
// call and returns harmless zero values, so a handler runs to (or past) its
// first lake read without needing real data.
type capReader struct{ probe *deadlineProbe }

func (r *capReader) RecentLedgers(ctx context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) LedgerBySeq(ctx context.Context, _ uint32) (clickhouse.LedgerHeader, bool, error) {
	r.probe.record(ctx)
	return clickhouse.LedgerHeader{}, false, nil
}

func (r *capReader) LedgerTransactions(ctx context.Context, _ uint32, _ int) ([]clickhouse.TxSummary, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) RecentOperations(ctx context.Context, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationTypeStats(ctx context.Context, _ uint32) ([]clickhouse.OpTypeCount, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NetworkThroughput(ctx context.Context, _ int) ([]clickhouse.ThroughputBucket, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) BlendPoolReserves(ctx context.Context, _ string, _ []string, _ map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) TransactionByHash(ctx context.Context, _ string) (clickhouse.TxSummary, bool, error) {
	r.probe.record(ctx)
	return clickhouse.TxSummary{}, false, nil
}

func (r *capReader) OperationsByTx(ctx context.Context, _ uint32, _ string) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationResultsByTx(ctx context.Context, _ uint32, _ string) (map[uint32]int32, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) EventsByTx(ctx context.Context, _ uint32, _ string) ([]clickhouse.EventSummary, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractEventsRecent(ctx context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.ContractActivityRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractWasm(ctx context.Context, _ string) (clickhouse.ContractWasmInfo, error) {
	r.probe.record(ctx)
	return clickhouse.ContractWasmInfo{}, clickhouse.ErrContractWasmUnresolved
}

func (r *capReader) RecentContracts(ctx context.Context, _ int, _ uint32) ([]clickhouse.ContractDirectoryRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractInteractions(ctx context.Context, _ string, _ int, _ uint32) ([]clickhouse.ContractEdgeRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractCodeHistory(ctx context.Context, _ string) ([]clickhouse.ContractCodeVersion, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountTransactions(ctx context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountOperations(ctx context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountState(ctx context.Context, _ string) (clickhouse.AccountState, error) {
	r.probe.record(ctx)
	return clickhouse.AccountState{}, nil
}

// AccountStateCached records the deadline like its uncached sibling, so
// the timeout-propagation probes still see this call.
func (r *capReader) AccountStateCached(ctx context.Context, _ string) (clickhouse.AccountState, error) {
	r.probe.record(ctx)
	return clickhouse.AccountState{}, nil
}

func (r *capReader) AssetHolders(ctx context.Context, _ string, _ int) ([]clickhouse.AssetHolder, int64, error) {
	r.probe.record(ctx)
	return nil, 0, nil
}

func (r *capReader) AccountsByWealth(ctx context.Context, _ []string, _ []float64, _ int) ([]clickhouse.AccountWealth, error) {
	r.probe.record(ctx)
	return nil, nil
}

// AccountsByWealthCached records the deadline like its uncached sibling so
// the timeout-propagation probes still observe this call, and reports warm
// so handlers proceed down the normal path.
func (r *capReader) AccountsByWealthCached(ctx context.Context, _ []string, _ []float64, _ int) ([]clickhouse.AccountWealth, bool) {
	r.probe.record(ctx)
	return nil, true
}

func (r *capReader) SoroswapPairReserves(ctx context.Context, _ []string) (map[string]clickhouse.SoroswapPairState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NativeLiquidityPoolReserves(ctx context.Context, _ []string) (map[string]clickhouse.NativeLiquidityPoolState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NativeLiquidityPoolsRanked(ctx context.Context, _ int) ([]clickhouse.NativeLiquidityPoolState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) TokenDisplays(ctx context.Context, _ []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) SACClassicAssetName(ctx context.Context, _ string) (string, bool, error) {
	r.probe.record(ctx)
	return "", false, nil
}

func (r *capReader) SACAssetFromEvents(ctx context.Context, _ string) (string, bool, error) {
	r.probe.record(ctx)
	return "", false, nil
}

func (r *capReader) AccountsUnspendable(ctx context.Context, _ []string) (map[string]bool, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountMovements(ctx context.Context, _ string, _ int, _ clickhouse.AccountMovementCursor, _ clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

// capPositions is a PositionsReader that shares the same probe — the positions
// endpoint's lake dependency is Postgres (this seam), so its bounded context
// arrives here rather than at capReader.
type capPositions struct{ probe *deadlineProbe }

func (p *capPositions) BlendPositionsByUser(ctx context.Context, _ string) ([]timescale.BlendPositionFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) BlendBackstopSharesByUser(ctx context.Context, _ string) ([]timescale.BlendBackstopFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) PhoenixStakeByUser(ctx context.Context, _ string) ([]timescale.PhoenixStakeFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) DefindexVaultSharesByUser(ctx context.Context, _ string) ([]timescale.DefindexVaultFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) CreditPositionsByOwner(ctx context.Context, _ string) ([]timescale.CreditPositionFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) AquariusGaugeByUser(ctx context.Context, _ string) ([]timescale.AquariusGaugeFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

// newProbeHandler wires a Handler with the minimal seams the endpoints need,
// mirroring internal/api/v1/explorer.go's construction (ParseLimit returns the
// default, WriteJSON/WriteProblem just set a status, ClientAborted is false).
func newProbeHandler(reader ExplorerReader, positions PositionsReader) *Handler {
	return &Handler{
		Reader:         reader,
		Positions:      positions,
		PricingEnabled: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, _ *http.Request, def, _ int) (int, bool) {
			return def, true
		},
		ParseWindowDays: func(_ *http.Request, def int) int { return def },
		LakeWatermark:   func(_ context.Context) (uint32, bool, bool) { return 0, false, false },
		IsKnownSAC:      func(string) bool { return false },
		ClientAborted:   func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
			w.WriteHeader(http.StatusOK)
		},
	}
}

// validTestAccount / validTestContract are the same well-formed strkeys the
// package-v1 explorer tests use — they must pass canonical.IsAccountID /
// IsContractID so the handlers reach their lake read rather than 400ing.
const (
	validTestAccount  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	validTestContract = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	validTestTxHash   = "88526317d98b1eb5a8040123456789abcdef0123456789abcdef0123456789ab"
)

// TestExplorerReads_BoundedByReadTimeout is the C3-1 regression guard: for every
// lake-backed handler, assert the reader is invoked with a context whose
// deadline is set and sits within (0, explorerReadTimeout]. Against the un-fixed
// code (raw r.Context(), no deadline) hasDL is false and the test fails.
func TestExplorerReads_BoundedByReadTimeout(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		pathVals map[string]string
		call     func(h *Handler, w http.ResponseWriter, r *http.Request)
	}{
		{"LedgersList", "/v1/ledgers", nil, (*Handler).LedgersList},
		{"LedgerDetail", "/v1/ledgers/42", map[string]string{"seq": "42"}, (*Handler).LedgerDetail},
		{"LedgerTransactions", "/v1/ledgers/42/transactions", map[string]string{"seq": "42"}, (*Handler).LedgerTransactions},
		{"TxDetail", "/v1/tx/" + validTestTxHash, map[string]string{"hash": validTestTxHash}, (*Handler).TxDetail},
		{"ContractDetail", "/v1/contracts/" + validTestContract, map[string]string{"contract_id": validTestContract}, (*Handler).ContractDetail},
		{"ContractWasm", "/v1/contracts/" + validTestContract + "/wasm", map[string]string{"contract_id": validTestContract}, (*Handler).ContractWasm},
		{"OperationsByLedger", "/v1/operations?ledger=42", nil, (*Handler).Operations},
		{"OperationsDirectory", "/v1/operations", nil, (*Handler).Operations},
		{"NetworkThroughput", "/v1/network/throughput", nil, (*Handler).NetworkThroughput},
		{"AccountTransactions", "/v1/accounts/" + validTestAccount + "/transactions", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountTransactions},
		{"AccountOperations", "/v1/accounts/" + validTestAccount + "/operations", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountOperations},
		{"AccountMovements", "/v1/accounts/" + validTestAccount + "/movements", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountMovements},
		{"AccountState", "/v1/accounts/" + validTestAccount, map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountState},
		{"AssetHolders", "/v1/assets/native/holders", map[string]string{"asset_id": "native"}, (*Handler).AssetHolders},
		{"AccountPositions", "/v1/accounts/" + validTestAccount + "/positions", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountPositions},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &deadlineProbe{}
			h := newProbeHandler(&capReader{probe: probe}, &capPositions{probe: probe})

			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			for k, v := range tc.pathVals {
				r.SetPathValue(k, v)
			}
			tc.call(h, httptest.NewRecorder(), r)

			if !probe.sawCall {
				t.Fatalf("%s: handler never reached a lake read — test wiring is wrong", tc.name)
			}
			if !probe.hasDL {
				t.Fatalf("%s: reader received a context with NO deadline — the read is unbounded "+
					"(C3-1 pool-exhaustion DoS regression)", tc.name)
			}
			// Pin the budget to ~explorerReadTimeout: present, positive, never
			// larger than the ceiling, and close to it (distinguishes this 8s
			// read budget from any looser upstream/middleware deadline).
			if probe.budget <= 0 || probe.budget > explorerReadTimeout {
				t.Fatalf("%s: deadline budget %v not in (0, %v]", tc.name, probe.budget, explorerReadTimeout)
			}
			if probe.budget < explorerReadTimeout-2*time.Second {
				t.Fatalf("%s: deadline budget %v is smaller than the expected ~%v read ceiling",
					tc.name, probe.budget, explorerReadTimeout)
			}
		})
	}
}

// blockingReader is a capReader whose RecentLedgers blocks until its context is
// cancelled (with a generous hard fallback so an un-fixed, deadline-less run
// doesn't leak the goroutine forever).
type blockingReader struct{ *capReader }

func (b *blockingReader) RecentLedgers(ctx context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * explorerReadTimeout):
		return nil, errors.New("blocking reader hard fallback fired")
	}
}

// TestExplorerReads_ReturnWhenReadExceedsBudget proves the end-to-end anti-DoS
// property the deadline exists to deliver: a reader that would otherwise block
// indefinitely is abandoned once the handler's own read budget elapses, so the
// handler returns (and releases its pool connection) instead of hanging. Against
// the un-fixed code the reader gets a deadline-less r.Context(), never observes
// cancellation, and the handler hangs — this test then trips its budget+slack
// ceiling and fails.
func TestExplorerReads_ReturnWhenReadExceedsBudget(t *testing.T) {
	probe := &deadlineProbe{}
	h := newProbeHandler(&blockingReader{capReader: &capReader{probe: probe}}, nil)

	r := httptest.NewRequest(http.MethodGet, "/v1/ledgers", nil)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		h.LedgersList(httptest.NewRecorder(), r)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < explorerReadTimeout-2*time.Second {
			t.Fatalf("handler returned in %v — too fast to be the read deadline firing (expected ~%v); "+
				"the block/return path isn't exercising the timeout", elapsed, explorerReadTimeout)
		}
	case <-time.After(explorerReadTimeout + 3*time.Second):
		t.Fatal("handler did not return within the read budget + slack — the lake read is unbounded " +
			"(C3-1 pool-exhaustion DoS regression)")
	}
}
