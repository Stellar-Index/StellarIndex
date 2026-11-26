package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type stubRevenueExplorer struct {
	*stubExplorerReader
	rev   clickhouse.LendingRevenue
	err   error
	q     *clickhouse.LendingHistoryQuery
	asset string
}

func (e *stubRevenueExplorer) LendingPoolRevenue(_ context.Context, _, asset string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingRevenue, error) {
	e.q, e.asset = &q, asset
	return e.rev, e.err
}

type revenueTip struct{ at time.Time }

func (r revenueTip) LakeWatermark(context.Context) (uint32, time.Time, error) { return 60, r.at, nil }

// The stale flag follows the hourly watermark: a tip inside one derive run and the grace of the hour after the
// coverage is fresh, one past it is stale.
func TestLendingPoolRevenue_StaleFollowsTheHourlyWatermark(t *testing.T) {
	v2 := blend.PoolV2
	through := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		tip   time.Duration
		stale bool
	}{{time.Hour + 41*time.Minute, false}, {time.Hour + 43*time.Minute, true}} {
		ex := &stubRevenueExplorer{
			stubExplorerReader: &stubExplorerReader{},
			rev:                clickhouse.LendingRevenue{Covered: true, ThroughLedger: 50, ThroughHour: through, FloorLedger: 1},
		}
		srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}, LakeWatermark: revenueTip{through.Add(tc.tip)}})
		var body struct{ Flags v1.Flags }
		rec := getLendingHistory(t, srv, "/v1/lending/pools/"+mkCStrkey(t, 1)+"/revenue")
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Flags.Stale != tc.stale {
			t.Errorf("tip +%v: %d stale %v, want %v", tc.tip, rec.Code, body.Flags.Stale, tc.stale)
		}
	}
}

func TestLendingPoolRevenue_ValidationAndErrors(t *testing.T) {
	pool := mkCStrkey(t, 1)
	base := "/v1/lending/pools/" + pool + "/revenue"
	v2 := blend.PoolV2
	for _, tc := range []struct {
		name   string
		path   string
		err    error
		status int
		typ    string
	}{
		{"bad pool", "/v1/lending/pools/nope/revenue", nil, 400, "invalid-pool"},
		{"bad asset", base + "?asset=USDC", nil, 400, "invalid-asset"},
		{"bad interval", base + "?interval=1w", nil, 400, "invalid-interval"},
		{"hourly over 83 days", base + "?interval=1h&from=2026-01-01T00:00:00Z&to=2026-03-25T00:00:01Z", nil, 400, "invalid-window"},
		{"too many points", base, clickhouse.ErrLendingHistoryTooManyPoints, 400, "invalid-window"},
		{"read failed", base, errors.New("boom"), 500, "internal"},
	} {
		srv := v1.New(v1.Options{Explorer: &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, err: tc.err}, Lending: &stubLendingReader{version: &v2}})
		rec := getLendingHistory(t, srv, tc.path)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), "/errors/"+tc.typ) {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if tc.err != nil && !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Errorf("%s: an error must not be cached: %q", tc.name, rec.Header().Get("Cache-Control"))
		}
	}
	// An explorer without the revenue read is the not-ready 503, never an empty 200.
	if rec := getLendingHistory(t, v1.New(v1.Options{Explorer: &stubExplorerReader{}, Lending: &stubLendingReader{version: &v2}}), base); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no revenue reader: %d %s", rec.Code, rec.Body)
	}
	unknown := blend.PoolVersionUnknown
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}}
	if rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &unknown}}), base); rec.Code != http.StatusNotFound || ex.q != nil {
		t.Fatalf("unknown pool: %d %s", rec.Code, rec.Body)
	}
}

// A known pool with no derived interest (here Blend V1) is a 200 with no reserves, named interest_not_derived.
func TestLendingPoolRevenue_UncoveredPoolIsAnEmptyLowerBound(t *testing.T) {
	v1pool := blend.PoolV1
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}}
	rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v1pool}}),
		"/v1/lending/pools/"+mkCStrkey(t, 1)+"/revenue?interval=1h&from=2027-03-01T10:30:00Z&to=2027-03-02T05:45:00Z")
	var body struct{ Data v1.LendingRevenueView }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := body.Data
	if !d.LowerBound || len(d.Reserves) != 0 || d.Coverage.ThroughHour != nil || d.Coverage.DeriveRev == 0 {
		t.Fatalf("body = %+v", d)
	}
	var reasons []string
	for _, e := range d.Excluded {
		reasons = append(reasons, e.Kind+":"+e.Reason)
	}
	if strings.Join(reasons, ",") != "pool:interest_not_derived" {
		t.Fatalf("excluded = %v", reasons)
	}
	if time.Time(d.From).Format(time.RFC3339) != "2027-03-01T10:00:00Z" || time.Time(d.To).Format(time.RFC3339) != "2027-03-02T05:00:00Z" ||
		ex.q.Daily || ex.q.MaxPoints != 2000 {
		t.Fatalf("window %v..%v, query %+v", d.From, d.To, ex.q)
	}
}

// Reserves are keyed by token contract, so XLM is its SAC; the other spellings are rejected, never read as no reserve.
func TestLendingPoolRevenue_XLMSpellingsReadTheSACOrAreRejected(t *testing.T) {
	v2 := blend.PoolV2
	sac := mkCStrkey(t, 7)
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}})
	base := "/v1/lending/pools/" + mkCStrkey(t, 1) + "/revenue?asset="
	if rec := getLendingHistory(t, srv, base+sac); rec.Code != http.StatusOK || ex.asset != sac {
		t.Fatalf("SAC: %d, read asset %q", rec.Code, ex.asset)
	}
	for _, spelling := range []string{"native", "crypto:XLM", "XLM"} {
		if rec := getLendingHistory(t, srv, base+spelling); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", spelling, rec.Code)
		}
	}
}

func TestLendingPoolRevenue_AmountsAreExactStrings(t *testing.T) {
	v2 := blend.PoolV2
	asset := mkCStrkey(t, 2)
	huge, _ := new(big.Int).SetString("340282366920938463463374607431768211457", 10) // past u128
	s := clickhouse.NewLendingRevenueSums()
	s.Gross.Set(huge)
	s.SupplierIntended.Set(huge)
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 1, ThroughLedger: 50, ThroughHour: time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC),
		Reserves: []clickhouse.LendingRevenueReserve{{Asset: asset, Buckets: []clickhouse.LendingRevenueBucket{
			{Start: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), Sums: s}, {Start: time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC), Sums: s},
		}}},
	}}
	rec := getLendingHistory(t, v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}}),
		"/v1/lending/pools/"+mkCStrkey(t, 1)+"/revenue?asset="+asset+"&from=2027-03-01T00:00:00Z&to=2027-03-04T00:00:00Z")
	if rec.Code != http.StatusOK || ex.asset != asset {
		t.Fatalf("%d %s (asset %q)", rec.Code, rec.Body, ex.asset)
	}
	twice := new(big.Int).Lsh(huge, 1).String()
	if !strings.Contains(rec.Body.String(), `"interest_accrued":"`+twice+`"`) || !strings.Contains(rec.Body.String(), `"interest_accrued":"`+huge.String()+`"`) {
		t.Fatalf("amounts not exact decimal strings: %s", rec.Body)
	}
}
