package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type stubHistoryExplorer struct {
	*stubExplorerReader
	reserve  clickhouse.LendingReserveHistory
	backstop clickhouse.LendingBackstopHistory
	err      error
	q        *clickhouse.LendingHistoryQuery
	tiered   bool
}

func (e *stubHistoryExplorer) LendingReserveHistory(_ context.Context, _, _ string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingReserveHistory, error) {
	e.q = &q
	return e.reserve, e.err
}

func (e *stubHistoryExplorer) LendingBackstopHistory(_ context.Context, _ string, tiered bool, q clickhouse.LendingHistoryQuery) (clickhouse.LendingBackstopHistory, error) {
	e.q, e.tiered = &q, tiered
	return e.backstop, e.err
}

func getLendingHistory(t *testing.T, srv *v1.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestLendingReserveHistory_ValidationAndErrors(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	base := "/v1/lending/pools/" + pool + "/reserves/" + asset + "/history"
	v2 := blend.PoolV2
	cases := []struct {
		name   string
		path   string
		err    error
		status int
		typ    string
	}{
		{"bad interval", base + "?interval=1w", nil, 400, "invalid-interval"},
		{"hourly over 83 days", base + "?interval=1h&from=2026-01-01T00:00:00Z&to=2026-03-25T00:00:01Z", nil, 400, "invalid-window"},
		{"daily over 1998 days", base + "?from=2020-01-01T00:00:00Z&to=2025-06-21T00:00:01Z", nil, 400, "invalid-window"},
		{"no state ever recorded", base, clickhouse.ErrLendingReserveNotFound, 404, "not-found"},
		{"bad from", base + "?from=yesterday", nil, 400, "invalid-time"},
		{"bad asset", "/v1/lending/pools/" + pool + "/reserves/USDC/history", nil, 400, "invalid-asset"},
		{"too many points", base, clickhouse.ErrLendingHistoryTooManyPoints, 400, "invalid-window"},
		{"not derived", base, clickhouse.ErrLendingHistoryNotReady, 503, "history-unavailable"},
	}
	for _, tc := range cases {
		srv := v1.New(v1.Options{Explorer: &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, err: tc.err}, Lending: &stubLendingReader{version: &v2}})
		rec := getLendingHistory(t, srv, tc.path)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), "/errors/"+tc.typ) {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	// An explorer without the history read is the same not-ready 503, never an empty 200.
	rec := getLendingHistory(t, v1.New(v1.Options{Explorer: &stubExplorerReader{}}), base)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "history-unavailable") {
		t.Fatalf("no history reader: %d %s", rec.Code, rec.Body)
	}
	// The widest windows themselves are served.
	for _, q := range []string{"?interval=1h&from=2026-01-01T00:00:00Z&to=2026-03-25T00:00:00Z", "?from=2020-01-01T00:00:00Z&to=2025-06-21T00:00:00Z"} {
		srv := v1.New(v1.Options{Explorer: &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}}, Lending: &stubLendingReader{version: &v2}})
		if rec := getLendingHistory(t, srv, base+q); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
}

func TestLendingHistory_UnknownPoolIsNotFound(t *testing.T) {
	pool := mkCStrkey(t, 1)
	unknown := blend.PoolVersionUnknown
	for _, path := range []string{"/reserves/" + mkCStrkey(t, 2) + "/history", "/backstop/history"} {
		ex := &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}}
		srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &unknown}})
		rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+path)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "/errors/not-found") || ex.q != nil {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestLendingReserveHistory_FromRoundsDownToItsBucket(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	v2 := blend.PoolV2
	for _, tc := range []struct{ query, want string }{
		{"?interval=1h&from=2027-03-01T10:30:00Z&to=2027-03-02T00:00:00Z", "2027-03-01T10:00:00Z"},
		{"?from=2027-03-01T10:30:00Z&to=2027-03-05T00:00:00Z", "2027-03-01T00:00:00Z"},
	} {
		ex := &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}}
		rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}}),
			"/v1/lending/pools/"+pool+"/reserves/"+asset+"/history"+tc.query)
		var body struct{ Data struct{ From string } }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.query, rec.Code, rec.Body)
		}
		if body.Data.From != tc.want || ex.q.From.UTC().Format(time.RFC3339) != tc.want {
			t.Errorf("%s: from = %s, query from = %v, want %s", tc.query, body.Data.From, ex.q.From, tc.want)
		}
	}
}

func TestLendingReserveHistory_DefaultsAndBody(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	supplied, _ := new(big.Int).SetString("18446744073709551617", 10)
	cfg := blend.ReserveConfig{CFactor: 9_000_000, LFactor: 9_500_000, Decimals: 6}
	ex := &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, reserve: clickhouse.LendingReserveHistory{
		Coverage: clickhouse.LendingHistoryCoverage{ThroughLedger: 500, ThroughTime: time.Unix(2500, 0)},
		Protocol: clickhouse.LendingBlendV1, Undecodable: 1,
		Points: []clickhouse.LendingReservePoint{
			{CloseTime: time.Unix(1000, 0), StateLedger: 200, Removed: true},
			{CloseTime: time.Unix(2000, 0), StateLedger: 400, ConfigLedger: 300, State: clickhouse.BlendReserveState{
				Decimals: 6, DecimalsFound: true, Config: &cfg,
				Data:    blend.ReserveData{BRate: big.NewInt(1_000_000_000), DRate: big.NewInt(1_000_000_000), IRMod: big.NewInt(1), BackstopCredit: big.NewInt(0), Version: blend.PoolV1},
				Metrics: blend.ReserveMetrics{SuppliedUnderlying: supplied, BorrowedUnderlying: big.NewInt(0), HasAPR: true, BorrowAPR: 0.05, SupplyAPR: 0.04},
			}},
		},
	}}
	v2 := blend.PoolV2
	rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}}), "/v1/lending/pools/"+pool+"/reserves/"+asset+"/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if w := ex.q.To.Sub(ex.q.From); !ex.q.Daily || ex.q.MaxPoints != 2000 || w < 90*24*time.Hour || w >= 91*24*time.Hour ||
		!ex.q.From.Equal(ex.q.From.Truncate(24*time.Hour)) {
		t.Fatalf("query = %+v", ex.q)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("a response with a dropped point must not be cached: %q", cc)
	}
	var body struct{ Data v1.LendingReserveHistoryView }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	d := body.Data
	if *d.Protocol != "blend_v1" || d.Interval != "1d" || *d.RateDecimals != 9 || *d.IRModDecimals != 9 || *d.Decimals != 6 ||
		d.HistoryThroughLedger != 500 || d.HistoryThroughTime == nil || d.DroppedPoints != 1 || len(d.Points) != 2 {
		t.Fatalf("body = %+v", d)
	}
	if p := d.Points[1]; *p.Supplied != "18446744073709551617" || *p.ConfigLedger != 300 || *p.BorrowAPR != 0.05 || *p.CollateralFactor != "9000000" || !d.Points[0].Removed {
		t.Fatalf("points = %+v", d.Points)
	}
}

func TestLendingBackstopHistory_LineageAndTiers(t *testing.T) {
	pool := mkCStrkey(t, 1)
	path := "/v1/lending/pools/" + pool + "/backstop/history?interval=1h&window=72h"
	v1v := blend.PoolV1
	ex := &stubHistoryExplorer{stubExplorerReader: &stubExplorerReader{}}
	rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v1v}}), path)
	var v1Body struct{ Data v1.LendingBackstopHistoryView }
	if err := json.Unmarshal(rec.Body.Bytes(), &v1Body); err != nil || rec.Code != 200 || v1Body.Data.Available ||
		*v1Body.Data.UnavailableReason != "blend_v1_unsupported" || ex.q != nil {
		t.Fatalf("V1: %d %s", rec.Code, rec.Body)
	}

	unknown := blend.PoolVersionUnknown
	shares, _ := new(big.Int).SetString("18446744073709551616", 10)
	ex.backstop = clickhouse.LendingBackstopHistory{
		Coverage: clickhouse.LendingHistoryCoverage{ThroughLedger: 10}, Backstop: blnt.MainnetBackstop,
		Tiers: []clickhouse.LendingBackstopTierHistory{
			{Tier: "FirstLoss", Points: []clickhouse.LendingBackstopPoint{{Ledger: 9, Balance: clickhouse.BackstopBalance{Shares: shares, Tokens: big.NewInt(5), Q4W: new(big.Int).Rsh(shares, 2)}}}},
			{Tier: "SecondLoss", BeforeFirstState: true},
		},
	}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}}})
	rec = getLendingHistory(t, srv, path)
	var body struct{ Data v1.LendingBackstopHistoryView }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := body.Data
	if !ex.tiered || ex.q.Daily || ex.q.To.Sub(ex.q.From) < 72*time.Hour || ex.q.To.Sub(ex.q.From) >= 73*time.Hour || !d.Available || *d.Protocol != "blnt" || len(d.Tiers) != 2 {
		t.Fatalf("body = %+v, q = %+v", d, ex.q)
	}
	p := d.Tiers[0].Points[0]
	if *p.Shares != "18446744073709551616" || *p.Tokens != "5" || *p.QueuedPct != 25 || !d.Tiers[1].BeforeFirstState || d.Tiers[1].Points == nil {
		t.Fatalf("tiers = %+v", d.Tiers)
	}
}
