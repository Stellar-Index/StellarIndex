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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubBackstopExplorer returns a fixed backstop state and records whether the tiered (BLNT) read was asked for.
type stubBackstopExplorer struct {
	*stubExplorerReader
	state  clickhouse.LendingBackstopState
	tiered *bool
}

func (e *stubBackstopExplorer) LendingBackstop(_ context.Context, _ string, tiered bool) (clickhouse.LendingBackstopState, error) {
	e.tiered = &tiered
	return e.state, nil
}

type stubAuctionLendingReader struct {
	stubLendingReader
	auctions  []timescale.LendingAuction
	cur       timescale.LendingConfigCursor
	truncated bool
}

func (r *stubAuctionLendingReader) LendingAuctions(_ context.Context, _ string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingAuction, bool, error) {
	r.cur = cur
	if len(r.auctions) > limit {
		return r.auctions[:limit], r.truncated, nil
	}
	return r.auctions, r.truncated, nil
}

// TestLendingPoolAuctions_LifecycleTruncatedInBody: a capped fill/delete read is disclosed in the body, not only through the
// header-only degraded flag, since a cut-off delete reads as deleted=false.
func TestLendingPoolAuctions_LifecycleTruncatedInBody(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		lr := &stubAuctionLendingReader{truncated: truncated, auctions: []timescale.LendingAuction{{
			LendingEventPos: timescale.LendingEventPos{Ts: time.Unix(1_700_000_000, 0).UTC(), Ledger: 10, TxHash: "aa"}, User: mkCStrkey(t, 6),
		}}}
		srv := v1.New(v1.Options{Lending: lr})
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 5)+"/auctions", nil))
		var body struct{ Data v1.LendingAuctionsView }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Auctions) != 1 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body)
		}
		if body.Data.LifecycleTruncated != truncated || !strings.Contains(rec.Body.String(), `"lifecycle_truncated":`) {
			t.Fatalf("truncated=%v: body %s", truncated, rec.Body)
		}
		if noStore := strings.Contains(rec.Header().Get("Cache-Control"), "no-store"); noStore != truncated {
			t.Fatalf("truncated=%v: no-store = %v", truncated, noStore)
		}
	}
}

func getBackstop(t *testing.T, srv *v1.Server, pool string) v1.LendingBackstopView {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/backstop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct{ Data v1.LendingBackstopView }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

// TestLendingPoolBackstop_BLNTTiersExactAndUnavailableTierIsNull — a BLNT pool reads the tiered backstop; amounts above
// 2^63 are exact strings, queued_pct is q4w/shares, and a tier with no entry serves nulls rather than zeros.
func TestLendingPoolBackstop_BLNTTiersExactAndUnavailableTierIsNull(t *testing.T) {
	unknown, weight := blend.PoolVersionUnknown, uint32(40)
	shares, _ := new(big.Int).SetString("18446744073709551616", 10) // 2^64
	tokens, _ := new(big.Int).SetString("9223372036854775819", 10)
	q4w, _ := new(big.Int).SetString("4611686018427387904", 10) // 2^62 → 25%
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
		Instance: &clickhouse.PoolInstance{Config: blend.PoolConfig{BstopRate: 2_000_000}, Backstop: blnt.MainnetBackstop},
		Tiers: []clickhouse.BackstopTierState{
			{Tier: "FirstLoss", Balance: &clickhouse.BackstopBalance{Shares: shares, Tokens: tokens, Q4W: q4w}, Asset: "Usdc", Token: "CUSDC", TakeRateWeight: &weight},
			{Tier: "SecondLoss"},
		},
	}}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubBLNTLendingReader{
		stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)},
	}})
	got := getBackstop(t, srv, blnt.MainnetFixedPool)
	if ex.tiered == nil || !*ex.tiered || !got.Available || got.Protocol == nil || *got.Protocol != "blnt" ||
		*got.Backstop != blnt.MainnetBackstop || got.BackstopToken != nil || *got.TakeRate != "2000000" || len(got.Tiers) != 2 {
		t.Fatalf("view = %+v", got)
	}
	ft := got.Tiers[0]
	if *ft.Tier != "FirstLoss" || *ft.Shares != "18446744073709551616" || *ft.Tokens != "9223372036854775819" ||
		*ft.QueuedShares != "4611686018427387904" || ft.QueuedPct == nil || *ft.QueuedPct != 25 || *ft.BackstopAsset != "Usdc" ||
		*ft.BackstopToken != "CUSDC" || *ft.TakeRateWeight != 40 {
		t.Fatalf("FirstLoss = %+v", ft)
	}
	if st := got.Tiers[1]; st.Shares != nil || st.Tokens != nil || st.QueuedPct != nil || st.BackstopAsset != nil || st.TakeRateWeight != nil {
		t.Fatalf("SecondLoss = %+v, want nulls", st)
	}
}

// TestLendingPoolBackstop_BlendV1IsUnavailable — a V1 pool's backstop keys are unverified: served unavailable, never read.
func TestLendingPoolBackstop_BlendV1IsUnavailable(t *testing.T) {
	v1v := blend.PoolV1
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v1v}})
	got := getBackstop(t, srv, mkCStrkey(t, 4))
	if ex.tiered != nil || got.Available || got.UnavailableReason == nil || *got.UnavailableReason != "blend_v1_unsupported" ||
		got.Backstop != nil || len(got.Tiers) != 0 {
		t.Fatalf("view = %+v", got)
	}
}

// TestLendingPoolAuctions_PaginatesWithACursorTheNextPageAccepts — labels the type, carries fills and delete, and hands
// back a cursor that round-trips into the reader.
func TestLendingPoolAuctions_PaginatesWithACursorTheNextPageAccepts(t *testing.T) {
	ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pos := func(l uint32) timescale.LendingEventPos {
		return timescale.LendingEventPos{Ts: ts.Add(time.Duration(l) * time.Second), Ledger: l, TxHash: pad64hex(l)}
	}
	r := &stubAuctionLendingReader{auctions: []timescale.LendingAuction{
		{
			LendingEventPos: pos(9), AuctionType: 1, User: "U", Bid: map[string]string{"C": "18446744073709551621"}, Lot: map[string]string{},
			Fills: []timescale.LendingAuctionFill{{LendingEventPos: pos(10), Filler: "F", FillPercent: "50"}},
		},
		{LendingEventPos: pos(5), AuctionType: 3, User: "U", Deleted: &timescale.LendingEventPos{Ts: ts, Ledger: 7}},
	}}
	srv := v1.New(v1.Options{Lending: r})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 5)+"/auctions?limit=1", nil))
	var body struct {
		Data       v1.LendingAuctionsView
		Pagination struct{ Next string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	a := body.Data.Auctions
	if len(a) != 1 || a[0].AuctionTypeLabel != "bad_debt" || a[0].Bid["C"] != "18446744073709551621" || len(a[0].Fills) != 1 ||
		a[0].Fills[0].FillPercent != "50" || a[0].Deleted || body.Pagination.Next == "" {
		t.Fatalf("page = %+v next=%q", a, body.Pagination.Next)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 5)+"/auctions?cursor="+body.Pagination.Next, nil))
	if rec.Code != http.StatusOK || r.cur.Ledger != 9 || r.cur.TxHash != pad64hex(9) {
		t.Fatalf("second page status %d cursor %+v", rec.Code, r.cur)
	}
}

func pad64hex(n uint32) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = '0'
	}
	b[63] = hex[n%16]
	return string(b)
}

// TestLendingPoolAuctions_NoReaderIs503 — a lending reader without auction history, bare or behind the cache, is a 503, never an
// empty list that reads as "no auctions".
func TestLendingPoolAuctions_NoReaderIs503(t *testing.T) {
	for name, lr := range map[string]v1.LendingReader{
		"bare":   &stubLendingReader{},
		"cached": v1.NewCachedLendingReader(&stubLendingReader{}, 0),
	} {
		rec := httptest.NewRecorder()
		v1.New(v1.Options{Lending: lr}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 5)+"/auctions", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503: %s", name, rec.Code, rec.Body)
		}
	}
}
