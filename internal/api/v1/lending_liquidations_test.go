package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
// asset and of a deep one and repaying 100 d-tokens of the deep one; every asset trades at 0.50 an hour before, on $1,000.
func liqRouteServer(t *testing.T) (*testServerImpl, *stubLiquidationReader, string, string) {
	t.Helper()
	return liqRouteServerWith(t, "1000", nil)
}

// liqRouteServerWith is liqRouteServer with the pricing bucket's USD volume and the operator's source exclusions.
func liqRouteServerWith(t *testing.T, volumeUSD string, excl []timescale.SourceExclusion) (*testServerImpl, *stubLiquidationReader, string, string) {
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
	pts := []v1.HistoryPoint{{Bucket: bucket, VWAP: "0.50", VolumeUSD: &volumeUSD, Sources: []string{"soroswap"}}}
	opts := v1.Options{
		Explorer: liqRouteExplorer{&stubExplorerReader{}}, Lending: lr,
		History:            &stubHistoryReader{points: pts, twapPoints: pts},
		VerifiedCurrencies: newTestCatalogue(t),
		Substance:          pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thin: true}}, pricingguard.SubstanceGateOptions{}),
	}
	if excl != nil {
		opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: excl}, pricingguard.SourceExclusionGateOptions{})
	}
	return startHTTPTest(t, v1.New(opts).Handler()), lr, thin, deep
}

// liqRouteFirstFill fetches one fill through the route.
func liqRouteFirstFill(t *testing.T, ts *testServerImpl) map[string]any {
	t.Helper()
	status, body := fetch(t, ts.URL+"/v1/lending/pools/"+liqTestPool+"/liquidations?limit=1")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	fills, _ := decodeMap(t, body)["data"].(map[string]any)["liquidations"].([]any)
	if len(fills) != 1 {
		t.Fatalf("liquidations = %v, want 1", fills)
	}
	f, _ := fills[0].(map[string]any)
	return f
}

// TestLendingPoolLiquidations_ThinBucketAtFill: the asset's market today clears the substance gate, but the bucket that would
// price the fill traded under the floor, so the deep legs are null with price_thin_at_fill and no side or profit is claimed.
func TestLendingPoolLiquidations_ThinBucketAtFill(t *testing.T) {
	ts, _, _, deep := liqRouteServerWith(t, "999.99", nil)
	f := liqRouteFirstFill(t, ts)
	if f["lot_usd"] != nil || f["bid_usd"] != nil || f["filler_profit_usd"] != nil {
		t.Fatalf("lot_usd %v bid_usd %v profit %v, want all null", f["lot_usd"], f["bid_usd"], f["filler_profit_usd"])
	}
	for _, l := range append(f["lot"].([]any), f["bid"].([]any)...) {
		if leg, _ := l.(map[string]any); leg["asset"] == deep && (leg["usd"] != nil || leg["reason"] != "price_thin_at_fill") {
			t.Errorf("deep leg = %v, want usd null with reason price_thin_at_fill", leg)
		}
	}
}

// liqProxyFailReader serves the asset's own USD series and fails every stablecoin-proxy read, so the chart walk ends degraded
// with the canonical buckets already merged.
type liqProxyFailReader struct {
	*stubHistoryReader
}

func (r liqProxyFailReader) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, n int) ([]v1.HistoryPoint, error) {
	if p.Quote.Type != canonical.AssetFiat {
		return nil, context.DeadlineExceeded
	}
	return r.stubHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, n)
}

// TestLendingPoolLiquidations_PartialWalkIsUnread: a walk that stopped early may be missing the bucket nearest the fill, so no
// leg is valued from what it did merge; each says price_unread and no side total is claimed.
func TestLendingPoolLiquidations_PartialWalkIsUnread(t *testing.T) {
	_, lr, _, deep := liqRouteServer(t)
	vol := "1000"
	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	pts := []v1.HistoryPoint{{Bucket: bucket, VWAP: "0.50", VolumeUSD: &vol, Sources: []string{"soroswap"}}}
	ts := startHTTPTest(t, v1.New(v1.Options{
		Explorer: liqRouteExplorer{&stubExplorerReader{}}, Lending: lr, VerifiedCurrencies: newTestCatalogue(t),
		History: liqProxyFailReader{&stubHistoryReader{points: pts, twapPoints: pts}},
	}).Handler())
	f := liqRouteFirstFill(t, ts)
	if f["bid_usd"] != nil || f["filler_profit_usd"] != nil {
		t.Fatalf("bid_usd %v profit %v, want null from a partial walk", f["bid_usd"], f["filler_profit_usd"])
	}
	for _, l := range append(f["lot"].([]any), f["bid"].([]any)...) {
		if leg, _ := l.(map[string]any); leg["asset"] == deep && (leg["usd"] != nil || leg["reason"] != "price_unread") {
			t.Errorf("deep leg = %v, want usd null with reason price_unread", leg)
		}
	}
}

// liqTodayWalkFailReader degrades only the today's-market walk. The fills' walk reads through liqReadBefore and so always passes a
// bound; an asset's today walk runs after its fills' walk and opens with an unbounded canonical read, which arms proxy failures
// for that asset. The canonical buckets still merge, so the walk ends partial, not empty.
type liqTodayWalkFailReader struct {
	*stubHistoryReader
	mu    sync.Mutex
	armed map[string]bool
}

func (r *liqTodayWalkFailReader) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, n int) ([]v1.HistoryPoint, error) {
	r.mu.Lock()
	if p.Quote.Type == canonical.AssetFiat && to.IsZero() {
		r.armed[p.Base.String()] = true
	}
	fail := p.Quote.Type != canonical.AssetFiat && r.armed[p.Base.String()]
	r.mu.Unlock()
	if fail {
		return nil, context.DeadlineExceeded
	}
	return r.stubHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, n)
}

// TestLendingPoolLiquidations_PartialTodayWalkIsUnread: the fills' walk is complete but the walk judging today's market stopped
// early, so whether today's book is thin is unknown; no leg is valued and each says price_unread.
func TestLendingPoolLiquidations_PartialTodayWalkIsUnread(t *testing.T) {
	_, lr, _, deep := liqRouteServer(t)
	vol := "1000"
	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	pts := []v1.HistoryPoint{{Bucket: bucket, VWAP: "0.50", VolumeUSD: &vol, Sources: []string{"soroswap"}}}
	ts := startHTTPTest(t, v1.New(v1.Options{
		Explorer: liqRouteExplorer{&stubExplorerReader{}}, Lending: lr, VerifiedCurrencies: newTestCatalogue(t),
		History: &liqTodayWalkFailReader{stubHistoryReader: &stubHistoryReader{points: pts, twapPoints: pts}, armed: map[string]bool{}},
	}).Handler())
	f := liqRouteFirstFill(t, ts)
	if f["bid_usd"] != nil || f["filler_profit_usd"] != nil {
		t.Fatalf("bid_usd %v profit %v, want null when today's walk is partial", f["bid_usd"], f["filler_profit_usd"])
	}
	seen := false
	for _, l := range append(f["lot"].([]any), f["bid"].([]any)...) {
		if leg, _ := l.(map[string]any); leg["asset"] == deep {
			seen = true
			if leg["usd"] != nil || leg["reason"] != "price_unread" {
				t.Errorf("deep leg = %v, want usd null with reason price_unread", leg)
			}
		}
	}
	if !seen {
		t.Fatal("no leg in the deep asset; the fixture no longer exercises the walk")
	}
}

// liqRangeRecorder records the [from, to) of every hourly series read.
type liqRangeRecorder struct {
	*stubHistoryReader
	spans [][2]time.Time
}

func (r *liqRangeRecorder) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, n int) ([]v1.HistoryPoint, error) {
	r.spans = append(r.spans, [2]time.Time{from, to})
	return r.stubHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, n)
}

// TestLendingPoolLiquidations_ReadStopsAtNewestFill: a page of fills three days old reads its pricing series only up to the
// bucket after the newest fill, while today's market is read on its own and still refuses the thin asset.
func TestLendingPoolLiquidations_ReadStopsAtNewestFill(t *testing.T) {
	thin, deep := mkCStrkey(t, 3), mkCStrkey(t, 4)
	newest := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	lr := &stubLiquidationReader{fills: []timescale.LendingLiquidationFill{{
		LendingEventPos: timescale.LendingEventPos{Ts: newest, Ledger: 100, TxHash: liqTxA},
		User:            mkCStrkey(t, 6), Filler: mkCStrkey(t, 7), FillPercent: "100", HasAmounts: true,
		Lot: map[string]string{thin: "1000000000", deep: "1000000000"}, Bid: map[string]string{deep: "1000000000"},
	}}}
	vol := "1000"
	pts := []v1.HistoryPoint{{Bucket: newest.Add(-2 * time.Hour).Truncate(time.Hour), VWAP: "0.50", VolumeUSD: &vol}}
	rec := &liqRangeRecorder{stubHistoryReader: &stubHistoryReader{points: pts, twapPoints: pts}}
	ts := startHTTPTest(t, v1.New(v1.Options{
		Explorer: liqRouteExplorer{&stubExplorerReader{}}, Lending: lr, History: rec, VerifiedCurrencies: newTestCatalogue(t),
		Substance: pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thin: true}}, pricingguard.SubstanceGateOptions{}),
	}).Handler())
	f := liqRouteFirstFill(t, ts)
	if f["bid_usd"] != "50.00" || f["lot_usd"] != "50.00" || f["lot_usd_lower_bound"] != true {
		t.Fatalf("bid_usd %v lot_usd %v lower_bound %v, want 50.00 50.00 true (thin asset refused today)", f["bid_usd"], f["lot_usd"], f["lot_usd_lower_bound"])
	}
	bound, bounded, today := newest.Add(time.Hour), 0, 0
	for _, sp := range rec.spans {
		from, to := sp[0], sp[1]
		switch {
		case from.After(bound):
			today++ // today's market, read from a day ago
		case to.IsZero() || to.After(bound):
			t.Errorf("a fill series read [%s, %s) runs past the bucket after the newest fill (%s)", from, to, bound)
		case to.Equal(bound):
			bounded++
		}
	}
	if bounded == 0 || today == 0 {
		t.Fatalf("reads %v: want fill reads bounded at %s and a separate read of today's market", rec.spans, bound)
	}
}

// TestLendingPoolLiquidations_ExcludedSourceDoesNotValueFill: the only bucket that could price the fills was fed by a source
// the operator excluded over its span, so no leg is valued from it (price_withheld) — the route's source-exclusion proof.
func TestLendingPoolLiquidations_ExcludedSourceDoesNotValueFill(t *testing.T) {
	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	excl := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: bucket.Add(-time.Hour), Until: bucket.Add(2 * time.Hour)}}
	ts, _, _, deep := liqRouteServerWith(t, "1000", excl)
	f := liqRouteFirstFill(t, ts)
	if f["bid_usd"] != nil || f["filler_profit_usd"] != nil {
		t.Fatalf("bid_usd %v profit %v, want null", f["bid_usd"], f["filler_profit_usd"])
	}
	for _, l := range f["bid"].([]any) {
		if leg, _ := l.(map[string]any); leg["asset"] == deep && (leg["usd"] != nil || leg["price_usd"] != nil || leg["reason"] != "price_withheld") {
			t.Errorf("deep bid leg = %v, want no price and reason price_withheld", leg)
		}
	}
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
