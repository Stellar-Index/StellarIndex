package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// stubTopAccountsExplorer serves a fixed pool-wide Positions read and counts the reads.
type stubTopAccountsExplorer struct {
	*stubBLNTExplorer
	positions clickhouse.LendingPoolPositions
	reads     atomic.Int32
}

func (e *stubTopAccountsExplorer) LendingPoolPositions(context.Context, string) (clickhouse.LendingPoolPositions, error) {
	e.reads.Add(1)
	return e.positions, nil
}

func topReserve(pool, asset string, bRate int64) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Pool: pool, Asset: asset, Decimals: 7, DecimalsFound: true,
		Data: blend.ReserveData{
			BRate: big.NewInt(bRate), DRate: big.NewInt(1_000_000_000_000),
			IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
		},
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(1_000 * xlm), BorrowedUnderlying: big.NewInt(500 * xlm)},
	}
}

// topAccountsFixture is a Blend pool with XLM (priced $2, b_rate 1.1) and an unpriced reserve, and four accounts: one holding
// 2^64 XLM b-tokens, one 300 XLM, one only the unpriced token (more units than anyone), one a borrower.
func topAccountsFixture(t *testing.T) (pool, sac, unpriced string, ex *stubTopAccountsExplorer, srv *v1.Server) {
	t.Helper()
	pool, sac, unpriced = mkCStrkey(t, 1), nativeSAC(t), mkCStrkey(t, 5)
	ex = &stubTopAccountsExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			topReserve(pool, sac, 1_100_000_000_000), topReserve(pool, unpriced, 1_000_000_000_000),
		}}},
		positions: clickhouse.LendingPoolPositions{Readable: true, Accounts: []clickhouse.LendingPoolPosition{
			{Account: "GA", Supply: map[string]*big.Int{sac: bigStr(t, "18446744073709551616")}, Ledger: 10},
			{Account: "GB", Supply: map[string]*big.Int{sac: big.NewInt(300 * xlm)}, Ledger: 11},
			{Account: "GC", Supply: map[string]*big.Int{unpriced: bigStr(t, "99999999999999999999999")}, Ledger: 12},
			{Account: "GD", Supply: map[string]*big.Int{sac: new(big.Int)}, Borrow: map[string]*big.Int{sac: big.NewInt(50 * xlm)}, Ledger: 13},
		}},
	}
	srv = v1.New(v1.Options{
		Explorer: ex, Lending: &stubLendingReader{assets: []string{sac, unpriced}},
		Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {Price: "2"}}},
	})
	return pool, sac, unpriced, ex, srv
}

func getTopAccounts(t *testing.T, srv *v1.Server, pool, query string, wantStatus int) (v1.LendingTopAccountsView, v1.Flags) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/top-accounts"+query, nil))
	if rec.Code != wantStatus {
		t.Fatalf("%s: status = %d, want %d: %s", query, rec.Code, wantStatus, rec.Body)
	}
	var body struct {
		Data  v1.LendingTopAccountsView
		Flags v1.Flags
	}
	if wantStatus == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body.Data, body.Flags
}

// TestLendingTopAccounts_RanksByUSDAndExcludesUnpriced: without asset, rows rank by value_usd; the unpriced reserve's
// position is left out (never compared by token amount, though it holds the most units) and the ranking is a lower bound
// naming it. Amounts above 2^63 stay exact strings at the stored rate.
func TestLendingTopAccounts_RanksByUSDAndExcludesUnpriced(t *testing.T) {
	pool, sac, unpriced, _, srv := topAccountsFixture(t)
	got, _ := getTopAccounts(t, srv, pool, "?side=supply", http.StatusOK)
	if got.Ranking != "value_usd" || got.Protocol != "blend" || got.Accounts != 3 || !got.LowerBound {
		t.Fatalf("view = %+v", got)
	}
	if len(got.Rows) != 2 || got.Rows[0].Account != "GA" || got.Rows[1].Account != "GB" || got.Rows[0].Rank != 1 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	a := got.Rows[0]
	if a.Tokens != "18446744073709551616" || a.Amount != "20291418481080506777" || strOrNull(a.ValueUSD) != "4058283696216.10" ||
		a.Asset != sac || a.Decimals == nil || *a.Decimals != 7 || a.LastModifiedLedger != 10 {
		t.Fatalf("top row = %+v", a)
	}
	if b := got.Rows[1]; b.Amount != "3300000000" || strOrNull(b.ValueUSD) != "660.00" || strOrNull(b.ShareOfReserve) != "0.330000" {
		t.Fatalf("second row = %+v", b)
	}
	if len(got.Excluded) != 1 || strOrNull(got.Excluded[0].Asset) != unpriced || got.Excluded[0].Reason != "unpriced" {
		t.Fatalf("excluded = %+v", got.Excluded)
	}
}

// TestLendingTopAccounts_AssetFilterRanksByAmountAndKeepsUnpricedRows: within one reserve rows rank by amount; a row whose
// price is unavailable keeps its amount with value_usd null and the reason set, and the ranking is complete.
func TestLendingTopAccounts_AssetFilterRanksByAmountAndKeepsUnpricedRows(t *testing.T) {
	pool, _, unpriced, _, srv := topAccountsFixture(t)
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+unpriced, http.StatusOK)
	if got.Ranking != "amount" || got.LowerBound || got.Accounts != 1 || len(got.Rows) != 1 {
		t.Fatalf("view = %+v", got)
	}
	if r := got.Rows[0]; r.Account != "GC" || r.Amount != "99999999999999999999999" || r.ValueUSD != nil || strOrNull(r.UsdReason) != "unpriced" {
		t.Fatalf("row = %+v", r)
	}
}

// TestLendingTopAccounts_BorrowSideLimitAndCache: the borrow side reads d-tokens only; limit caps rows; a second request is
// served from the per-pool cache without a second lake read.
func TestLendingTopAccounts_BorrowSideLimitAndCache(t *testing.T) {
	pool, _, _, ex, srv := topAccountsFixture(t)
	got, _ := getTopAccounts(t, srv, pool, "?side=borrow", http.StatusOK)
	if got.Accounts != 1 || len(got.Rows) != 1 || got.Rows[0].Account != "GD" || got.Rows[0].Amount != "500000000" || got.LowerBound {
		t.Fatalf("borrow = %+v", got)
	}
	got, _ = getTopAccounts(t, srv, pool, "?side=supply&limit=1", http.StatusOK)
	if len(got.Rows) != 1 || got.Rows[0].Account != "GA" {
		t.Fatalf("limit=1 rows = %+v", got.Rows)
	}
	if n := ex.reads.Load(); n != 1 {
		t.Fatalf("lake reads = %d, want 1 across cached requests", n)
	}
}

// TestLendingTopAccounts_PartialReadIsALowerBound: a truncated read or an unreadable entry makes rows and accounts a floor.
func TestLendingTopAccounts_PartialReadIsALowerBound(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	ex.positions.Truncated, ex.positions.Unreadable = true, 2
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if !got.LowerBound || len(got.Excluded) != 2 || got.Excluded[0].Reason != "positions_truncated" ||
		got.Excluded[1].Reason != "positions_unreadable" || len(got.Rows) != 2 {
		t.Fatalf("view = %+v", got)
	}
}

// TestLendingTopAccounts_BLNTPool: a pool with BLNT lineage is served as protocol blnt through the same rate conversion.
func TestLendingTopAccounts_BLNTPool(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	unknown := blend.PoolVersionUnknown
	ex := &stubTopAccountsExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}, data: map[string]blnt.ReserveData{
			asset: {
				BRate: big.NewInt(2_000_000_000_000), DRate: big.NewInt(1_000_000_000_000), IRMod: new(big.Int),
				BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int),
			},
		}},
		positions: clickhouse.LendingPoolPositions{Readable: true, Accounts: []clickhouse.LendingPoolPosition{
			{Account: "GA", Supply: map[string]*big.Int{asset: big.NewInt(7)}},
		}},
	}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubBLNTLendingReader{
		stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{asset},
	}})
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+asset, http.StatusOK)
	if got.Protocol != "blnt" || len(got.Rows) != 1 || got.Rows[0].Amount != "14" || got.Rows[0].Tokens != "7" {
		t.Fatalf("view = %+v", got)
	}
}

// TestLendingTopAccounts_Validation: malformed side, limit, asset or pool is a 400; a pool of no known lineage is a 404, as
// /emissions; an explorer without the pool-wide positions read is a 503.
func TestLendingTopAccounts_Validation(t *testing.T) {
	pool, _, _, _, srv := topAccountsFixture(t)
	for _, q := range []string{"", "?side=lend", "?side=supply&limit=0", "?side=supply&limit=101", "?side=supply&limit=x", "?side=supply&asset=USDC"} {
		getTopAccounts(t, srv, pool, q, http.StatusBadRequest)
	}
	getTopAccounts(t, srv, "not-a-pool", "?side=supply", http.StatusBadRequest)

	unknown := blend.PoolVersionUnknown
	ex := &stubTopAccountsExplorer{stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}}}
	srv404 := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &unknown}})
	getTopAccounts(t, srv404, pool, "?side=supply", http.StatusNotFound)

	srv503 := v1.New(v1.Options{Explorer: &stubExplorerReader{}, Lending: &stubLendingReader{}})
	getTopAccounts(t, srv503, pool, "?side=supply", http.StatusServiceUnavailable)
}
