package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The ClickHouse-down failure mode, pinned at the handler seam: the chaos
// suite's docker-compose dev stack carries no ClickHouse, so this is where a
// lake outage is exercised. Every lake-backed route must fail loudly with a
// 5xx problem, never answer 200 with an empty page.

var errLakeDown = errors.New("dial tcp 127.0.0.1:9300: connect: connection refused")

// downReader is an ExplorerReader whose every read fails the way the
// clickhouse-go pool does when the server is unreachable.
type downReader struct{}

func (downReader) RecentLedgers(context.Context, int, uint32) ([]clickhouse.LedgerHeader, error) {
	return nil, errLakeDown
}

func (downReader) LedgerBySeq(context.Context, uint32) (clickhouse.LedgerHeader, bool, error) {
	return clickhouse.LedgerHeader{}, false, errLakeDown
}

func (downReader) LedgerTransactions(context.Context, uint32, int) ([]clickhouse.TxSummary, error) {
	return nil, errLakeDown
}

func (downReader) OperationsByLedger(context.Context, uint32, int) ([]clickhouse.OpRow, error) {
	return nil, errLakeDown
}

func (downReader) RecentOperations(context.Context, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return nil, errLakeDown
}

func (downReader) OperationTypeStats(context.Context, uint32) ([]clickhouse.OpTypeCount, error) {
	return nil, errLakeDown
}

func (downReader) NetworkThroughput(context.Context, int) ([]clickhouse.ThroughputBucket, error) {
	return nil, errLakeDown
}

func (downReader) BlendPoolReserves(context.Context, string, blend.PoolVersion, []string, map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	return nil, errLakeDown
}

func (downReader) TransactionByHash(context.Context, string) (clickhouse.TxSummary, bool, error) {
	return clickhouse.TxSummary{}, false, errLakeDown
}

func (downReader) OperationsByTx(context.Context, uint32, string) ([]clickhouse.OpRow, error) {
	return nil, errLakeDown
}

func (downReader) OperationResultsByTx(context.Context, uint32, string) (map[uint32]clickhouse.OpResult, error) {
	return nil, errLakeDown
}

func (downReader) TxOutcomesByHash(context.Context, []uint32, []string) (map[string]clickhouse.TxOutcome, error) {
	return nil, errLakeDown
}

func (downReader) EventsByTx(context.Context, uint32, string) ([]clickhouse.EventSummary, error) {
	return nil, errLakeDown
}

func (downReader) ContractEventsRecent(context.Context, string, int, clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	return nil, errLakeDown
}

func (downReader) ContractWasm(context.Context, string) (clickhouse.ContractWasmInfo, error) {
	return clickhouse.ContractWasmInfo{}, errLakeDown
}

func (downReader) ContractInstanceState(context.Context, string) (clickhouse.ContractInstanceState, error) {
	return clickhouse.ContractInstanceState{}, errLakeDown
}

func (downReader) RecentContracts(context.Context, int, uint32) ([]clickhouse.ContractDirectoryRow, error) {
	return nil, errLakeDown
}

func (downReader) ContractInteractions(context.Context, string, int, uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	return nil, 0, errLakeDown
}

func (downReader) ContractCodeHistory(context.Context, string) ([]clickhouse.ContractCodeVersion, error) {
	return nil, errLakeDown
}

func (downReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error) {
	return nil, errLakeDown
}

func (downReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return nil, errLakeDown
}

func (downReader) AccountOperationTypeCounts(context.Context, string) ([]clickhouse.OpTypeCount, error) {
	return nil, errLakeDown
}

func (downReader) AccountState(context.Context, string) (clickhouse.AccountState, error) {
	return clickhouse.AccountState{}, errLakeDown
}

func (downReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, errLakeDown
}

func (downReader) AssetHolders(context.Context, string, int) ([]clickhouse.AssetHolder, int64, error) {
	return nil, 0, errLakeDown
}

func (downReader) AccountsByWealth(context.Context, []string, []float64, int) ([]clickhouse.AccountWealth, error) {
	return nil, errLakeDown
}

func (downReader) AccountsByWealthCached(context.Context, []string, []float64, int) ([]clickhouse.AccountWealth, string, time.Time, bool) {
	return nil, "", time.Time{}, false
}

func (downReader) SoroswapPairReserves(context.Context, []string) (map[string]clickhouse.SoroswapPairState, error) {
	return nil, errLakeDown
}

func (downReader) NativeLiquidityPoolReserves(context.Context, []string) (map[string]clickhouse.NativeLiquidityPoolState, error) {
	return nil, errLakeDown
}

func (downReader) NativeLiquidityPoolsRanked(context.Context, int) ([]clickhouse.NativeLiquidityPoolState, error) {
	return nil, errLakeDown
}

func (downReader) TokenDisplays(context.Context, []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	return nil, errLakeDown
}

func (downReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return "", false, errLakeDown
}

func (downReader) SACAssetFromEvents(context.Context, string) (string, bool, error) {
	return "", false, errLakeDown
}

func (downReader) AccountsUnspendable(context.Context, []string) (map[string]bool, error) {
	return nil, errLakeDown
}

func (downReader) AccountMovements(context.Context, string, int, clickhouse.AccountMovementCursor, clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	return nil, errLakeDown
}

func (downReader) Cap67MovementsWatermark(context.Context) (uint32, error) {
	return 0, errLakeDown
}

func (downReader) AccountsStats(context.Context) (clickhouse.AccountsStats, bool, error) {
	return clickhouse.AccountsStats{}, false, errLakeDown
}

func (downReader) AccountCreators(context.Context, int, string) (clickhouse.AccountCreators, bool, error) {
	return clickhouse.AccountCreators{}, false, errLakeDown
}

func (downReader) AccountSponsors(context.Context, int, string) (clickhouse.AccountSponsors, bool, error) {
	return clickhouse.AccountSponsors{}, false, errLakeDown
}

func (downReader) AccountGraph(context.Context, string, string, int, string) (clickhouse.AccountGraph, bool, error) {
	return clickhouse.AccountGraph{}, false, errLakeDown
}

func (downReader) AccountGraphHistory(context.Context, string) (clickhouse.AccountGraphHistory, bool, error) {
	return clickhouse.AccountGraphHistory{}, false, errLakeDown
}

func (downReader) AccountCohort(context.Context, string, string) (clickhouse.AccountCohort, bool, error) {
	return clickhouse.AccountCohort{}, false, errLakeDown
}

func (downReader) ContractActivitySummaryFor(context.Context, string, int) (clickhouse.ContractActivitySummary, bool, error) {
	return clickhouse.ContractActivitySummary{}, false, errLakeDown
}

type lakeDownOutcome struct {
	problemStatus int
	wroteJSON     bool
}

func newLakeDownHandler(out *lakeDownOutcome) *Handler {
	h := newProbeHandler(downReader{}, &capPositions{probe: &deadlineProbe{}})
	h.LookupUSDPrice = func(context.Context, canonical.Asset) (string, bool) { return "", false }
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
		out.problemStatus = status
		w.WriteHeader(status)
	}
	h.WriteJSON = func(w http.ResponseWriter, _ any, _ bool) {
		out.wroteJSON = true
		w.WriteHeader(http.StatusOK)
	}
	return h
}

type lakeDownCase struct {
	name     string
	target   string
	pathVals map[string]string
	call     func(h *Handler, w http.ResponseWriter, r *http.Request)
}

// notLakeBacked names the route handlers whose hard dependency is not the
// lake, so a ClickHouse outage is not theirs to surface.
var notLakeBacked = map[string]string{
	"Search":           "classifies the query string; no store read",
	"DirectoryLookup":  "reads h.Directory",
	"AccountTrades":    "reads h.Trades (Postgres)",
	"AccountPositions": "folds from h.Positions (Postgres); the lake only decorates asset labels",
}

func lakeDownCases() []lakeDownCase {
	acct := map[string]string{"g_strkey": validTestAccount}
	contract := map[string]string{"contract_id": validTestContract}
	return []lakeDownCase{
		{"LedgersList", "/v1/ledgers", nil, (*Handler).LedgersList},
		{"LedgerDetail", "/v1/ledgers/42", map[string]string{"seq": "42"}, (*Handler).LedgerDetail},
		{"LedgerTransactions", "/v1/ledgers/42/transactions", map[string]string{"seq": "42"}, (*Handler).LedgerTransactions},
		{"TxDetail", "/v1/tx/" + validTestTxHash, map[string]string{"hash": validTestTxHash}, (*Handler).TxDetail},
		{"ContractsList", "/v1/contracts", nil, (*Handler).ContractsList},
		{"ContractDetail", "/v1/contracts/" + validTestContract, contract, (*Handler).ContractDetail},
		{"ContractWasm", "/v1/contracts/" + validTestContract + "/wasm", contract, (*Handler).ContractWasm},
		{"ContractInteractions", "/v1/contracts/" + validTestContract + "/interactions", contract, (*Handler).ContractInteractions},
		{"ContractCodeHistory", "/v1/contracts/" + validTestContract + "/code-history", contract, (*Handler).ContractCodeHistory},
		{"OperationsByLedger", "/v1/operations?ledger=42", nil, (*Handler).Operations},
		{"OperationsDirectory", "/v1/operations", nil, (*Handler).Operations},
		{"NetworkThroughput", "/v1/network/throughput", nil, (*Handler).NetworkThroughput},
		{"AccountsList", "/v1/accounts", nil, (*Handler).AccountsList},
		{"AccountsStats", "/v1/accounts/stats", nil, (*Handler).AccountsStats},
		{"AccountState", "/v1/accounts/" + validTestAccount, acct, (*Handler).AccountState},
		{"AccountTransactions", "/v1/accounts/" + validTestAccount + "/transactions", acct, (*Handler).AccountTransactions},
		{"AccountOperations", "/v1/accounts/" + validTestAccount + "/operations", acct, (*Handler).AccountOperations},
		{"AccountMovements", "/v1/accounts/" + validTestAccount + "/movements", acct, (*Handler).AccountMovements},
		{"AccountActivity", "/v1/accounts/" + validTestAccount + "/activity", acct, (*Handler).AccountActivity},
		{"AccountCreators", "/v1/accounts/" + validTestAccount + "/creators", acct, (*Handler).AccountCreators},
		{"AccountSponsors", "/v1/accounts/" + validTestAccount + "/sponsors", acct, (*Handler).AccountSponsors},
		{"AccountGraph", "/v1/accounts/" + validTestAccount + "/graph", acct, (*Handler).AccountGraph},
		{"AccountGraphHistory", "/v1/accounts/" + validTestAccount + "/graph/history", acct, (*Handler).AccountGraphHistory},
		{"AccountGraphCohort", "/v1/accounts/" + validTestAccount + "/graph/cohort?relation=created", acct, (*Handler).AccountGraphCohort},
		{"AssetHolders", "/v1/assets/native/holders", map[string]string{"asset_id": "native"}, (*Handler).AssetHolders},
	}
}

func TestExplorerReads_LakeDownFailsLoudly(t *testing.T) {
	for _, tc := range lakeDownCases() {
		t.Run(tc.name, func(t *testing.T) {
			var out lakeDownOutcome
			h := newLakeDownHandler(&out)
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			for k, v := range tc.pathVals {
				r.SetPathValue(k, v)
			}
			ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout+5*time.Second)
			defer cancel()
			tc.call(h, httptest.NewRecorder(), r.WithContext(ctx))

			if out.wroteJSON {
				t.Fatalf("%s answered 200 while every lake read failed — a ClickHouse outage is served as data", tc.name)
			}
			if out.problemStatus < 500 {
				t.Fatalf("%s: lake down produced problem status %d, want a 5xx", tc.name, out.problemStatus)
			}
		})
	}
}

// TestExplorerReads_LakeDownCoversEveryRoute fails when a route handler is
// added to Handler without joining either lakeDownCases or notLakeBacked.
func TestExplorerReads_LakeDownCoversEveryRoute(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range lakeDownCases() {
		fn := runtime.FuncForPC(reflect.ValueOf(tc.call).Pointer()).Name()
		covered[fn[strings.LastIndex(fn, ".")+1:]] = true
	}
	for name := range notLakeBacked {
		if covered[name] {
			t.Errorf("%s is both exercised as lake-backed and listed in notLakeBacked", name)
		}
	}
	routeSig := reflect.TypeOf((*Handler).LedgersList)
	ht := reflect.TypeOf(&Handler{})
	routes := map[string]bool{}
	for i := range ht.NumMethod() {
		m := ht.Method(i)
		if m.Type != routeSig {
			continue
		}
		routes[m.Name] = true
		if _, ok := notLakeBacked[m.Name]; !ok && !covered[m.Name] {
			t.Errorf("route handler %s is not in lakeDownCases: add it, or list it in notLakeBacked with the store it reads", m.Name)
		}
	}
	for name := range notLakeBacked {
		if !routes[name] {
			t.Errorf("notLakeBacked names %s, which is not a route handler on Handler", name)
		}
	}
}
