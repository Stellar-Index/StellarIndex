package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubLiquidationReader pages fixed fills newest first, honouring the cursor's time, and records the cursors it was given.
type stubLiquidationReader struct {
	stubLendingReader
	fills   []timescale.LendingLiquidationFill
	cursors []timescale.LendingConfigCursor
}

func (r *stubLiquidationReader) LendingLiquidations(_ context.Context, _ string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingLiquidationFill, error) {
	r.cursors = append(r.cursors, cur)
	var out []timescale.LendingLiquidationFill
	for _, f := range r.fills {
		if (!cur.IsSet() || f.Ts.Before(cur.Ts)) && len(out) < limit {
			out = append(out, f)
		}
	}
	return out, nil
}

// liqRouteExplorer serves every fill transaction's reserve state at b_rate = d_rate = 1.0.
type liqRouteExplorer struct {
	*stubExplorerReader
}

func (liqRouteExplorer) LendingReservesAtTxs(_ context.Context, _, _ string, assets []string, txs []clickhouse.LendingTxRef) (map[clickhouse.LendingTxReserve]blend.ReserveData, error) {
	one := func() *big.Int { return big.NewInt(1_000_000_000_000) }
	out := map[clickhouse.LendingTxReserve]blend.ReserveData{}
	for _, tx := range txs {
		for _, a := range assets {
			out[clickhouse.LendingTxReserve{LendingTxRef: tx, Asset: a}] = blend.ReserveData{Version: blend.PoolV2, BRate: one(), DRate: one()}
		}
	}
	return out, nil
}

// liqRouteServer: a V2 pool with two user-liquidation fills 30 and 40 minutes ago, each taking 100 b-tokens of a thin-market
// asset and of a deep one and repaying 100 d-tokens of the deep one; every asset trades at 0.50 an hour before.
func liqRouteServer(t *testing.T) (*testServerImpl, *stubLiquidationReader, string, string) {
	t.Helper()
	thin, deep := mkCStrkey(t, 3), mkCStrkey(t, 4)
	now := time.Now().UTC()
	fill := func(ago time.Duration, tx string) timescale.LendingLiquidationFill {
		return timescale.LendingLiquidationFill{
			LendingEventPos: timescale.LendingEventPos{Ts: now.Add(-ago).Truncate(time.Second), Ledger: 100, TxHash: tx},
			User:            mkCStrkey(t, 6), Filler: mkCStrkey(t, 7), FillPercent: "100", HasAmounts: true,
			Lot: map[string]string{thin: "1000000000", deep: "1000000000"}, Bid: map[string]string{deep: "1000000000"},
		}
	}
	lr := &stubLiquidationReader{fills: []timescale.LendingLiquidationFill{fill(30*time.Minute, liqTxA), fill(40*time.Minute, liqTxB)}}
	bucket := now.Add(-2 * time.Hour).Truncate(time.Hour)
	pts := []v1.HistoryPoint{{Bucket: bucket, VWAP: "0.50"}}
	srv := v1.New(v1.Options{
		Explorer: liqRouteExplorer{&stubExplorerReader{}}, Lending: lr,
		History:            &stubHistoryReader{points: pts, twapPoints: pts},
		VerifiedCurrencies: newTestCatalogue(t),
		Substance:          pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thin: true}}, pricingguard.SubstanceGateOptions{}),
	})
	return startHTTPTest(t, srv.Handler()), lr, thin, deep
}

// TestLendingPoolLiquidations_ThinLotLegWithheld: a lot asset whose only market is thin gets no USD (price_thin_market), so the
// lot side is the deep leg's value as a lower bound and no profit is claimed; the deep bid is valued in full.
func TestLendingPoolLiquidations_ThinLotLegWithheld(t *testing.T) {
	ts, _, thin, _ := liqRouteServer(t)
	status, body := fetch(t, ts.URL+"/v1/lending/pools/"+liqTestPool+"/liquidations?limit=1")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	data, _ := decodeMap(t, body)["data"].(map[string]any)
	fills, _ := data["liquidations"].([]any)
	if len(fills) != 1 {
		t.Fatalf("liquidations = %v, want 1", data["liquidations"])
	}
	f, _ := fills[0].(map[string]any)
	if f["lot_usd"] != "50.00" || f["lot_usd_lower_bound"] != true || f["filler_profit_usd"] != nil || f["bid_usd"] != "50.00" {
		t.Fatalf("lot_usd %v lower_bound %v profit %v bid_usd %v, want 50.00 true null 50.00",
			f["lot_usd"], f["lot_usd_lower_bound"], f["filler_profit_usd"], f["bid_usd"])
	}
	for _, l := range f["lot"].([]any) {
		leg, _ := l.(map[string]any)
		if leg["asset"] == thin && (leg["usd"] != nil || leg["reason"] != "price_thin_market") {
			t.Errorf("thin lot leg = %v, want usd null with reason price_thin_market", leg)
		}
	}
}

// TestLendingPoolLiquidations_Route: the route through the real router — envelope, cache policy, a cursor that resumes after
// the first page, limit bounds, and 404 for a contract no lending lineage knows.
func TestLendingPoolLiquidations_Route(t *testing.T) {
	ts, lr, _, _ := liqRouteServer(t)
	base := ts.URL + "/v1/lending/pools/" + liqTestPool + "/liquidations"
	resp, err := http.Get(base + "?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") == "" {
		t.Fatalf("status %d cache-control %q, want 200 with a cache policy", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	_, body := fetch(t, base+"?limit=1")
	env := decodeMap(t, body)
	data, _ := env["data"].(map[string]any)
	page, _ := env["pagination"].(map[string]any)
	next, _ := page["next"].(string)
	if data["pool"] != liqTestPool || data["valued_at"] != "event_time" || next == "" {
		t.Fatalf("pool %v valued_at %v next %q, want the pool, event_time and a next cursor", data["pool"], data["valued_at"], next)
	}
	status, body := fetch(t, base+"?limit=1&cursor="+url.QueryEscape(next))
	if status != http.StatusOK {
		t.Fatalf("second page status %d: %s", status, body)
	}
	second, _ := decodeMap(t, body)["data"].(map[string]any)["liquidations"].([]any)
	got := lr.cursors[len(lr.cursors)-1]
	if len(second) != 1 || second[0].(map[string]any)["tx_hash"] != liqTxB || got.TxHash != liqTxA || got.Ledger != 100 {
		t.Fatalf("second page %v from cursor %+v, want fill bb after aa", second, got)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?cursor=not-a-cursor"} {
		if status, _ := fetch(t, base+q); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, status)
		}
	}
	unknown := blend.PoolVersionUnknown
	lr.version, lr.fills = &unknown, nil
	if status, body := fetch(t, base); status != http.StatusNotFound {
		t.Errorf("unknown pool: status %d (%s), want 404", status, body)
	}
}

// liqTestPool is any well-formed pool contract id; the stubs answer for every pool.
const liqTestPool = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"

// liqTxA and liqTxB are the two fills' transaction hashes (a cursor carries a 64-hex hash).
var liqTxA, liqTxB = strings.Repeat("a", 64), strings.Repeat("b", 64)
