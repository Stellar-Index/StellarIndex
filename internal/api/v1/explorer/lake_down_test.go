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

func (downReader) RecentOperationsOfType(context.Context, int, clickhouse.ExplorerCursor, []string) (clickhouse.OpTypePage, error) {
	return clickhouse.OpTypePage{}, errLakeDown
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

func (downReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, clickhouse.ExplorerCursor, error) {
	return nil, clickhouse.ExplorerCursor{}, errLakeDown
}

func (downReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, clickhouse.ExplorerCursor, error) {
	return nil, clickhouse.ExplorerCursor{}, errLakeDown
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

func (downReader) AccountsByWealth(context.Context, []string, []string, int) ([]clickhouse.AccountWealth, error) {
	return nil, errLakeDown
}

func (downReader) AccountsByWealthCached(context.Context, []string, []string, int) (clickhouse.AccountWealthSnapshot, bool) {
	return clickhouse.AccountWealthSnapshot{}, false
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

func (downReader) AssetMovements(context.Context, string, int, clickhouse.AccountMovementCursor, uint32) ([]clickhouse.AssetMovementRow, error) {
	return nil, errLakeDown
}

func (downReader) AssetMovementsBackfilledThru(context.Context) (uint32, error) {
	return 0, errLakeDown
}

func (downReader) AssetEntryChanges(context.Context, string, int, clickhouse.AssetEntryChangeCursor, uint32) ([]clickhouse.AssetEntryChange, error) {
	return nil, errLakeDown
}

func (downReader) EntryHistoryCoverage(context.Context) (uint32, uint32, error) {
	return 0, 0, errLakeDown
}

func (downReader) Cap67MovementsWatermark(context.Context) (uint32, error) {
	return 0, errLakeDown
}

func (downReader) Cap67SupplyCoverage(context.Context) (uint32, uint32, bool, error) {
	return 0, 0, false, errLakeDown
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
		{"LedgerAt", "/v1/ledgers/at?ts=1700000000", nil, (*Handler).LedgerAt},
		{"LedgerTransactions", "/v1/ledgers/42/transactions", map[string]string{"seq": "42"}, (*Handler).LedgerTransactions},
		{"LedgerOperations", "/v1/ledgers/42/operations", map[string]string{"seq": "42"}, (*Handler).LedgerOperations},
		{"TxDetail", "/v1/tx/" + validTestTxHash, map[string]string{"hash": validTestTxHash}, (*Handler).TxDetail},
		{"ContractsList", "/v1/contracts", nil, (*Handler).ContractsList},
		{"ContractStats", "/v1/contracts/stats", nil, (*Handler).ContractStats},
		{"ContractDetail", "/v1/contracts/" + validTestContract, contract, (*Handler).ContractDetail},
		{"ContractWasm", "/v1/contracts/" + validTestContract + "/wasm", contract, (*Handler).ContractWasm},
		{"ContractInteractions", "/v1/contracts/" + validTestContract + "/interactions", contract, (*Handler).ContractInteractions},
		{"ContractCodeHistory", "/v1/contracts/" + validTestContract + "/code-history", contract, (*Handler).ContractCodeHistory},
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
		{"AssetEntryChanges", "/v1/assets/native/entry-changes", map[string]string{"asset_id": "native"}, (*Handler).AssetEntryChanges},
		{"AssetMovements", "/v1/assets/native/movements", map[string]string{"asset_id": "native"}, (*Handler).AssetMovements},
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

func (downReader) ContractStats(context.Context) (clickhouse.ContractStats, error) {
	return clickhouse.ContractStats{}, errLakeDown
}

func (downReader) ContractTypes(context.Context, []string) (map[string]bool, error) {
	return nil, errLakeDown
}

// TestExplorerReads_BoundedByReadTimeout is the regression guard: for every
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
		{"LedgerOperations", "/v1/ledgers/42/operations", map[string]string{"seq": "42"}, (*Handler).LedgerOperations},
		{"TxDetail", "/v1/tx/" + validTestTxHash, map[string]string{"hash": validTestTxHash}, (*Handler).TxDetail},
		{"ContractDetail", "/v1/contracts/" + validTestContract, map[string]string{"contract_id": validTestContract}, (*Handler).ContractDetail},
		{"ContractWasm", "/v1/contracts/" + validTestContract + "/wasm", map[string]string{"contract_id": validTestContract}, (*Handler).ContractWasm},
		{"OperationsDirectory", "/v1/operations", nil, (*Handler).Operations},
		{"NetworkThroughput", "/v1/network/throughput", nil, (*Handler).NetworkThroughput},
		{"AccountTransactions", "/v1/accounts/" + validTestAccount + "/transactions", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountTransactions},
		{"AccountOperations", "/v1/accounts/" + validTestAccount + "/operations", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountOperations},
		{"AccountMovements", "/v1/accounts/" + validTestAccount + "/movements", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountMovements},
		{"AccountState", "/v1/accounts/" + validTestAccount, map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountState},
		{"AssetHolders", "/v1/assets/native/holders", map[string]string{"asset_id": "native"}, (*Handler).AssetHolders},
		{"AssetEntryChanges", "/v1/assets/native/entry-changes", map[string]string{"asset_id": "native"}, (*Handler).AssetEntryChanges},
		{"AccountPositions", "/v1/accounts/" + validTestAccount + "/positions", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountPositions},
		{"AccountsStats", "/v1/accounts/stats", nil, (*Handler).AccountsStats},
		{"AccountCreators", "/v1/accounts/creators", nil, (*Handler).AccountCreators},
		{"AccountSponsors", "/v1/accounts/sponsors", nil, (*Handler).AccountSponsors},
		{"AccountGraph", "/v1/accounts/" + validTestAccount + "/graph", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountGraph},
		{"AccountGraphHistory", "/v1/accounts/" + validTestAccount + "/graph/history", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountGraphHistory},
		{"AccountGraphCohort", "/v1/accounts/" + validTestAccount + "/graph/cohort?relation=created", map[string]string{"g_strkey": validTestAccount}, (*Handler).AccountGraphCohort},
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
			// Pin the budget to the route's expected read ceiling: present,
			// positive, never larger than the ceiling, and close to it
			// (distinguishes the route's own budget from any looser
			// upstream/middleware deadline). Every read is request-scoped at
			// explorerReadTimeout except AssetHolders, whose cold-path scan
			// runs DETACHED on its own assetHoldersRefreshTimeout budget
			// (stale-while-revalidate) — still
			// bounded, still cancellation-observing, just not
			// request-scoped.
			wantBudget := explorerReadTimeout
			switch tc.name {
			case "AssetHolders":
				wantBudget = assetHoldersRefreshTimeout
			case "OperationsDirectory":
				// The never-computed first page is single-flighted
				// through refreshOpsDirectory, which runs the fill DETACHED
				// on its own budget (like every sibling cold-path here) so a
				// burst of concurrent first-page requests shares the one
				// read instead of each paying for its own — still bounded,
				// just not request-scoped.
				wantBudget = opsDirRefreshTimeout
			case "ContractDetail":
				// First page is SWR'd: the cold
				// compute runs DETACHED on the shared contract-detail
				// budget — still bounded, just not request-scoped.
				wantBudget = contractDetailRefreshTimeout
			case "NetworkThroughput":
				// Snapshot-served: the year-window
				// scan runs DETACHED on its own refresh budget so it
				// survives the request that kicked it — still bounded,
				// just not request-scoped.
				wantBudget = networkThroughputRefreshTimeout
			case "AccountPositions":
				// SWR'd on the shared contract-detail cache:
				// the six-fold fan-out runs DETACHED on
				// that budget — still bounded, still
				// cancellation-observing, just not request-scoped. The
				// REQUEST-side bound is unchanged (the cold wait is
				// capped by explorerReadTimeout in the handler).
				wantBudget = contractDetailRefreshTimeout
			}
			if probe.budget <= 0 || probe.budget > wantBudget {
				t.Fatalf("%s: deadline budget %v not in (0, %v]", tc.name, probe.budget, wantBudget)
			}
			if probe.budget < wantBudget-2*time.Second {
				t.Fatalf("%s: deadline budget %v is smaller than the expected ~%v read ceiling",
					tc.name, probe.budget, wantBudget)
			}
		})
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

// TestExplorerReads_DeadlineMapsTo503 is the regression guard. For every
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
			"LedgerOperations", "/v1/ledgers/42/operations",
			map[string]string{"seq": "42"},
			(*Handler).LedgerOperations,
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
