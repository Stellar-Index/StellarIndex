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

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/sources/blend"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// stubLendingReader is the in-memory test seam.
type stubLendingReader struct {
	pools   []timescale.BlendPoolSummary
	assets  []string
	configs map[string]blend.ReserveConfig
	err     error
}

func (r *stubLendingReader) ListBlendPools(_ context.Context) ([]timescale.BlendPoolSummary, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.pools, nil
}

func (r *stubLendingReader) BlendPoolAssets(_ context.Context, _ string) ([]string, error) {
	return r.assets, r.err
}

func (r *stubLendingReader) BlendReserveConfigs(_ context.Context, _ string) (map[string]blend.ReserveConfig, error) {
	return r.configs, r.err
}

// TestLendingPools_EmptyArrayWhenReaderNil — feature-gated reader.
// 200 + empty array (NOT 503) so the explorer's /lending page can
// render an empty state without an error toast.
func TestLendingPools_EmptyArrayWhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected `\"data\":[]` in body, got: %s", body)
	}
}

// TestLendingPools_HappyPath threads a populated stub and pins the
// per-row wire shape the explorer's /lending page reads (Protocol
// is hard-coded "blend" today; surfaces in the row regardless of
// what the storage struct's name field says, since the storage
// type doesn't have a Protocol field).
func TestLendingPools_HappyPath(t *testing.T) {
	lastSeen := time.Date(2026, 5, 9, 10, 15, 52, 0, time.UTC)
	reader := &stubLendingReader{
		pools: []timescale.BlendPoolSummary{
			{
				Pool:           "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD",
				Auctions24h:    30,
				AuctionsTotal:  5687,
				UniqueUsers30d: 4,
				LastSeen:       lastSeen,
				NetSupplied30d: "1000",
				NetBorrowed30d: "400",
			},
			{
				Pool:           "CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS",
				Auctions24h:    2,
				AuctionsTotal:  1544,
				UniqueUsers30d: 3,
				LastSeen:       lastSeen.Add(-1 * time.Hour),
			},
		},
	}
	srv := v1.New(v1.Options{Lending: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.LendingPool `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(env.Data) != 2 {
		t.Fatalf("len = %d, want 2 (body=%s)", len(env.Data), body)
	}
	first := env.Data[0]
	if first.Protocol != "blend" {
		t.Errorf("Protocol = %q, want \"blend\" (handler hard-codes per-PR-comment-#1110)", first.Protocol)
	}
	if first.Pool != "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD" {
		t.Errorf("Pool = %q", first.Pool)
	}
	if first.AuctionsTotal != 5687 || first.Auctions24h != 30 {
		t.Errorf("auction counts = (24h=%d, total=%d)", first.Auctions24h, first.AuctionsTotal)
	}
	if first.UniqueUsers30d != 4 {
		t.Errorf("UniqueUsers30d = %d", first.UniqueUsers30d)
	}
	if !first.LastSeen.Equal(lastSeen) {
		t.Errorf("LastSeen = %v, want %v", first.LastSeen, lastSeen)
	}
	if first.NetSupplied30d != "1000" || first.NetBorrowed30d != "400" {
		t.Errorf("net-flow = (supplied=%q, borrowed=%q), want (1000, 400)", first.NetSupplied30d, first.NetBorrowed30d)
	}
	if first.Utilization30dPct == nil || *first.Utilization30dPct != 40 {
		t.Errorf("Utilization30dPct = %v, want 40 (400/1000)", first.Utilization30dPct)
	}
	// Second pool has no net-flow fields set → utilisation omitted.
	if env.Data[1].Utilization30dPct != nil {
		t.Errorf("Utilization30dPct (pool 2) = %v, want nil (no net supply)", *env.Data[1].Utilization30dPct)
	}
}

// TestLendingPools_ReaderError500 — storage error surfaces as a 500
// problem+json. The handler logs at ERROR (production grep target).
func TestLendingPools_ReaderError500(t *testing.T) {
	reader := &stubLendingReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{Lending: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestLendingPools_TimeoutReturns503 — the 8s deadline fires when
// the per-pool aggregates take too long. Returns 503 with a
// `lending-timeout` problem type so callers can retry rather than
// treat it as an opaque internal error.
func TestLendingPools_TimeoutReturns503(t *testing.T) {
	reader := &stubLendingReader{err: context.DeadlineExceeded}
	srv := v1.New(v1.Options{Lending: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "lending-timeout") {
		t.Errorf("expected `lending-timeout` problem type in body, got: %s", body)
	}
}

// TestLendingPools_NilSliceFromReaderMarshalsAsEmptyArray —
// regression guard: a reader that returns (nil, nil) shouldn't
// surface as `data: null`. The handler's `make([]LendingPool, 0)`
// path keeps the wire-shape consistent.
func TestLendingPools_NilSliceFromReaderMarshalsAsEmptyArray(t *testing.T) {
	reader := &stubLendingReader{pools: nil}
	srv := v1.New(v1.Options{Lending: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected `\"data\":[]`, got: %s", body)
	}
}

// TestLendingPoolReserves_Watermark pins ADR-0041 Decision 4 on the
// Blend per-reserve current-state read: `as_of_ledger` carries the
// (cached) lake watermark, `flags.stale` fires when its close time
// trails now beyond the threshold (10min here), and the exact reserve
// amounts survive the disclosure add unchanged.
func TestLendingPoolReserves_Watermark(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 20)
	explorer := &stubExplorerReader{
		reserves: []clickhouse.BlendReserveState{{
			Pool:     pool,
			Asset:    asset,
			Decimals: 7,
			Metrics: blend.ReserveMetrics{
				SuppliedUnderlying: big.NewInt(1_000_000),
				BorrowedUnderlying: big.NewInt(400_000),
				UtilizationPct:     40,
			},
		}},
	}
	lending := &stubLendingReader{assets: []string{asset}}
	srv := v1.New(v1.Options{
		Explorer:      explorer,
		Lending:       lending,
		LakeWatermark: &wmStub{ledger: 63_500_000, closedAt: time.Now().Add(-10 * time.Minute)},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.LendingPoolReservesView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AsOfLedger != 63_500_000 {
		t.Errorf("as_of_ledger = %d, want 63500000", env.Data.AsOfLedger)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale should fire for a 10-minute-old watermark")
	}
	// The disclosure add must not perturb the exact i128 reserve figures.
	if len(env.Data.Reserves) != 1 || env.Data.Reserves[0].Supplied != "1000000" || env.Data.Reserves[0].Borrowed != "400000" {
		t.Errorf("reserves = %+v, want one reserve supplied=1000000 borrowed=400000", env.Data.Reserves)
	}
}
