package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// stubTopAccountsExplorer serves a fixed pool-wide Positions read, or the reads in seq in turn (the last repeating), and
// counts the reads.
type stubTopAccountsExplorer struct {
	*stubBLNTExplorer
	positions clickhouse.LendingPoolPositions
	seq       []clickhouse.LendingPoolPositions
	reads     atomic.Int32
}

func (e *stubTopAccountsExplorer) LendingPoolPositions(context.Context, string) (clickhouse.LendingPoolPositions, error) {
	n := int(e.reads.Add(1))
	if len(e.seq) > 0 {
		return e.seq[min(n, len(e.seq))-1], nil
	}
	return e.positions, nil
}

// withPositions is the fixture's read with accounts replacing its own.
func withPositions(p clickhouse.LendingPoolPositions, accounts ...clickhouse.LendingPoolPosition) clickhouse.LendingPoolPositions {
	p.Accounts = accounts
	return p
}

// topReserve is a reserve whose b_supply and d_supply are the totals the fixture's positions must sum to.
func topReserve(pool, asset string, bRate int64, bSupply, dSupply *big.Int) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Pool: pool, Asset: asset, Decimals: 7, DecimalsFound: true,
		Data: blend.ReserveData{
			BRate: big.NewInt(bRate), DRate: big.NewInt(1_000_000_000_000),
			IRMod: new(big.Int), BSupply: bSupply, DSupply: dSupply, BackstopCredit: new(big.Int), Version: blend.PoolV2,
		},
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(1_000 * xlm), BorrowedUnderlying: big.NewInt(500 * xlm)},
	}
}

// topAccountsFixture is a Blend pool with XLM (priced $2, b_rate 1.1) and an unpriced reserve, and four accounts: one holding
// 2^64 XLM b-tokens, one 300 XLM, one only the unpriced token (more units than anyone), one a borrower. Each reserve's
// b_supply and d_supply equal its positions' sums, so the read reconciles.
func topAccountsFixture(t *testing.T) (pool, sac, unpriced string, ex *stubTopAccountsExplorer, srv *v1.Server) {
	t.Helper()
	pool, sac, unpriced = mkCStrkey(t, 1), nativeSAC(t), mkCStrkey(t, 5)
	sacSupply := new(big.Int).Add(bigStr(t, "18446744073709551616"), big.NewInt(300*xlm))
	ex = &stubTopAccountsExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			topReserve(pool, sac, 1_100_000_000_000, sacSupply, big.NewInt(50*xlm)),
			topReserve(pool, unpriced, 1_000_000_000_000, bigStr(t, "99999999999999999999999"), new(big.Int)),
		}}},
		positions: clickhouse.LendingPoolPositions{Readable: true, Reserves: []string{sac, unpriced}, Accounts: []clickhouse.LendingPoolPosition{
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

// TestLendingTopAccounts_XLMSpellingsReadTheSACOrAreRejected: the filter is the reserve's own key, XLM's SAC, which reads every
// XLM position priced at native's USD rate; the other XLM spellings are refused with 400, never served as an empty ranking.
func TestLendingTopAccounts_XLMSpellingsReadTheSACOrAreRejected(t *testing.T) {
	pool, sac, _, _, srv := topAccountsFixture(t)
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if got.Accounts != 2 || len(got.Rows) != 2 || got.Rows[0].Account != "GA" || got.Rows[1].Account != "GB" ||
		strOrNull(got.Rows[1].ValueUSD) != "660.00" {
		t.Fatalf("SAC filter = %+v", got)
	}
	for _, spelling := range []string{"native", "crypto:XLM", "XLM"} {
		getTopAccounts(t, srv, pool, "?side=supply&asset="+spelling, http.StatusBadRequest)
	}
}

// TestLendingTopAccounts_AssetNotAReserveIs404: a well-formed asset outside the pool's readable reserve list is a 404, not
// an empty ranking; with the list unreadable the route cannot tell, and serves its lower bound.
func TestLendingTopAccounts_AssetNotAReserveIs404(t *testing.T) {
	pool, _, _, _, srv := topAccountsFixture(t)
	other := mkCStrkey(t, 9)
	getTopAccounts(t, srv, pool, "?side=supply&asset="+other, http.StatusNotFound)
	pool2, _, _, ex2, srv2 := topAccountsFixture(t)
	ex2.positions = clickhouse.LendingPoolPositions{}
	if got, _ := getTopAccounts(t, srv2, pool2, "?side=supply&asset="+other, http.StatusOK); !got.LowerBound || len(got.Rows) != 0 {
		t.Fatalf("unreadable list = %+v", got)
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

// TestLendingTopAccounts_TruncatedReadServesNoRanking: a read cut at its cap may lack the largest account, so no row is
// ranked; accounts is a floor and the response names why.
func TestLendingTopAccounts_TruncatedReadServesNoRanking(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	ex.positions.Truncated, ex.positions.Unreadable = true, 2
	for _, q := range []string{"?side=supply&asset=" + sac, "?side=supply"} {
		got, _ := getTopAccounts(t, srv, pool, q, http.StatusOK)
		if !got.LowerBound || len(got.Rows) != 0 || got.Accounts == 0 || !topExcluded(got, "null", "positions_truncated") ||
			!topExcluded(got, "null", "positions_unreadable") {
			t.Fatalf("%s: view = %+v", q, got)
		}
	}
}

func topExcluded(v v1.LendingTopAccountsView, asset, reason string) bool {
	for _, e := range v.Excluded {
		if strOrNull(e.Asset) == asset && e.Reason == reason {
			return true
		}
	}
	return false
}

// TestLendingTopAccounts_ReconciledReadIsComplete: positions summing exactly to each reserve's b_supply and d_supply are a
// complete ranking on both sides.
func TestLendingTopAccounts_ReconciledReadIsComplete(t *testing.T) {
	pool, sac, _, _, srv := topAccountsFixture(t)
	for _, q := range []string{"?side=supply&asset=" + sac, "?side=borrow&asset=" + sac, "?side=borrow"} {
		if got, _ := getTopAccounts(t, srv, pool, q, http.StatusOK); got.LowerBound || len(got.Rows) == 0 {
			t.Fatalf("%s: view = %+v", q, got)
		}
	}
}

// TestLendingTopAccounts_NoPositionsAgainstANonZeroReserveIsUnattributed: a read that finds no Positions entries while the
// reserve holds b-tokens (as a key layout the reader does not match would) is never served as a clean "0 accounts": the
// whole supply is served as unattributed and the answer is a lower bound.
func TestLendingTopAccounts_NoPositionsAgainstANonZeroReserveIsUnattributed(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	ex.positions.Accounts = nil
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if !got.LowerBound || len(got.Rows) != 0 || len(got.Excluded) != 0 || len(got.Unattributed) != 1 {
		t.Fatalf("view = %+v", got)
	}
	if u := got.Unattributed[0]; u.Asset != sac || u.Side != "supply" || u.Tokens != "18446744076709551616" ||
		u.Amount != "20291418484380506777" || u.ShareOfReserve != "1.000000" {
		t.Fatalf("unattributed = %+v", u)
	}
}

// TestLendingTopAccounts_OvercountedReserveIsNotRanked: positions summing past the reserve's b_supply cannot all be true, so
// that reserve is left out and named positions_overcounted; the other side, which reconciles, still ranks completely. A
// position holding more than the reserve's total is then never served with a share above 1.
func TestLendingTopAccounts_OvercountedReserveIsNotRanked(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	ex.positions.Accounts = append(ex.positions.Accounts, clickhouse.LendingPoolPosition{
		Account: "GE", Supply: map[string]*big.Int{sac: bigStr(t, "36893488147419103232")}, Ledger: 14,
	})
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if !got.LowerBound || len(got.Rows) != 0 || !topExcluded(got, sac, "positions_overcounted") || len(got.Unattributed) != 0 {
		t.Fatalf("view = %+v", got)
	}
	got, _ = getTopAccounts(t, srv, pool, "?side=borrow&asset="+sac, http.StatusOK)
	if got.LowerBound || len(got.Rows) != 1 {
		t.Fatalf("borrow side reconciles on d_supply alone: %+v", got)
	}
}

// TestLendingTopAccounts_ShortReserveRanksWithRankBounds: positions summing short of b_supply are ranked as a lower bound with
// the gap served; a row is marked rank_lower_bound once its amount (or, across reserves, value_usd) is below what one unread
// position could hold, and every row is when a short reserve's gap cannot be valued.
func TestLendingTopAccounts_ShortReserveRanksWithRankBounds(t *testing.T) {
	pool, sac, unpriced, ex, srv := topAccountsFixture(t)
	acc := ex.positions.Accounts
	// GB's 300 XLM of b-tokens is replaced by GF's 100: 200 XLM of b-tokens (220 XLM, $440) is not attributed.
	gf := clickhouse.LendingPoolPosition{Account: "GF", Supply: map[string]*big.Int{sac: big.NewInt(100 * xlm)}, Ledger: 15}
	ex.positions = withPositions(ex.positions, acc[0], gf, acc[2], acc[3])
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if !got.LowerBound || len(got.Excluded) != 0 || len(got.Rows) != 2 || len(got.Unattributed) != 1 {
		t.Fatalf("view = %+v", got)
	}
	if u := got.Unattributed[0]; u.Tokens != "2000000000" || u.Amount != "2200000000" || u.ShareOfReserve != "0.000000" {
		t.Fatalf("unattributed = %+v", u)
	}
	if got.Rows[0].Account != "GA" || got.Rows[0].RankLowerBound || got.Rows[1].Account != "GF" || !got.Rows[1].RankLowerBound {
		t.Fatalf("amount rank bounds = %+v", got.Rows)
	}
	got, _ = getTopAccounts(t, srv, pool, "?side=supply", http.StatusOK)
	if len(got.Rows) != 2 || got.Rows[0].RankLowerBound || !got.Rows[1].RankLowerBound || len(got.Unattributed) != 1 {
		t.Fatalf("usd rank bounds = %+v", got)
	}

	pool2, _, _, ex2, srv2 := topAccountsFixture(t)
	acc2 := ex2.positions.Accounts
	ex2.positions = withPositions(ex2.positions, acc2[0], acc2[1], acc2[3]) // the unpriced reserve's only holder unread
	got, _ = getTopAccounts(t, srv2, pool2, "?side=supply", http.StatusOK)
	if len(got.Rows) != 2 || !got.Rows[0].RankLowerBound || !got.Rows[1].RankLowerBound || len(got.Unattributed) != 1 ||
		got.Unattributed[0].Asset != unpriced {
		t.Fatalf("unpriced gap: every row is a rank floor: %+v", got)
	}
}

// TestLendingTopAccounts_MismatchRereadsOnce: a mismatch from a write between the positions and reserve reads is read again
// once, and the clean second read is the answer, cached for the full TTL.
func TestLendingTopAccounts_MismatchRereadsOnce(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	over := withPositions(ex.positions, append(slices.Clone(ex.positions.Accounts), clickhouse.LendingPoolPosition{
		Account: "GE", Supply: map[string]*big.Int{sac: big.NewInt(1)}, Ledger: 14,
	})...)
	ex.seq = []clickhouse.LendingPoolPositions{over, ex.positions}
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if got.LowerBound || len(got.Rows) != 2 || len(got.Excluded) != 0 {
		t.Fatalf("view = %+v", got)
	}
	if n := ex.reads.Load(); n != 2 {
		t.Fatalf("positions reads = %d, want 2", n)
	}
	if ttl := srv.TopAccountsTTLForTest(pool); ttl != v1.TopAccountsTTL {
		t.Fatalf("clean fill ttl = %v, want %v", ttl, v1.TopAccountsTTL)
	}
}

// TestLendingTopAccounts_PersistentMismatchCachedShort: a mismatch that survives the re-read is served, and cached only for
// the short unreconciled TTL so the next caller reads again soon.
func TestLendingTopAccounts_PersistentMismatchCachedShort(t *testing.T) {
	pool, sac, _, ex, srv := topAccountsFixture(t)
	ex.positions.Accounts = ex.positions.Accounts[1:] // GA unread: the sac reserve is short
	got, _ := getTopAccounts(t, srv, pool, "?side=supply&asset="+sac, http.StatusOK)
	if !got.LowerBound || len(got.Unattributed) != 1 {
		t.Fatalf("view = %+v", got)
	}
	if n := ex.reads.Load(); n != 2 {
		t.Fatalf("positions reads = %d, want 2", n)
	}
	if ttl := srv.TopAccountsTTLForTest(pool); ttl != v1.TopAccountsUnreconciledTTL || ttl >= v1.TopAccountsTTL {
		t.Fatalf("unreconciled fill ttl = %v, want %v", ttl, v1.TopAccountsUnreconciledTTL)
	}
}

// TestLendingTopAccounts_AsOfIsTheFillTime: the envelope's as_of dates the cached read, not the request.
func TestLendingTopAccounts_AsOfIsTheFillTime(t *testing.T) {
	pool, _, _, _, srv := topAccountsFixture(t)
	getTopAccounts(t, srv, pool, "?side=supply", http.StatusOK)
	srv.AgeTopAccountsForTest(pool, 30*time.Second)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/top-accounts?side=supply", nil))
	var body struct {
		AsOf time.Time `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if age := time.Since(body.AsOf); age < 29*time.Second {
		t.Fatalf("as_of %v is %v old, want the fill time 30s back", body.AsOf, age)
	}
}

// TestLendingTopAccounts_BLNTPool:a pool with BLNT lineage is served as protocol blnt through the same rate conversion.
func TestLendingTopAccounts_BLNTPool(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	unknown := blend.PoolVersionUnknown
	ex := &stubTopAccountsExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}, data: map[string]blnt.ReserveData{
			asset: {
				BRate: big.NewInt(2_000_000_000_000), DRate: big.NewInt(1_000_000_000_000), IRMod: new(big.Int),
				BSupply: big.NewInt(7), DSupply: new(big.Int), BackstopCredit: new(big.Int),
			},
		}},
		positions: clickhouse.LendingPoolPositions{Readable: true, Reserves: []string{asset}, Accounts: []clickhouse.LendingPoolPosition{
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
