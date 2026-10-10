package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// stubLendingReader is the in-memory test seam.
type stubLendingReader struct {
	pools   []timescale.BlendPoolSummary
	assets  []string
	configs map[string]blend.ReserveConfig
	version *blend.PoolVersion // nil → PoolV2, the generation the reserve fixtures are scaled for
	err     error
}

func (r *stubLendingReader) BlendPoolVersion(_ context.Context, _ string) (blend.PoolVersion, error) {
	if r.version == nil {
		return blend.PoolV2, r.err
	}
	return *r.version, r.err
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
				NetSupplied30d: ptr("1000"),
				NetBorrowed30d: ptr("400"),
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
		t.Errorf("Protocol = %q, want \"blend\" (handler hard-codes it; Blend is the only lending protocol integrated)", first.Protocol)
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
	if !first.LastSeen.Time().Equal(lastSeen) {
		t.Errorf("LastSeen = %v, want %v", first.LastSeen, lastSeen)
	}
	if first.NetSupplied30d == nil || *first.NetSupplied30d != "1000" ||
		first.NetBorrowed30d == nil || *first.NetBorrowed30d != "400" {
		t.Errorf("net-flow = (supplied=%v, borrowed=%v), want (1000, 400)", first.NetSupplied30d, first.NetBorrowed30d)
	}
	if first.Utilization30dPct == nil || *first.Utilization30dPct != 40 {
		t.Errorf("Utilization30dPct = %v, want 40 (400/1000)", first.Utilization30dPct)
	}
	// Second pool's flows span several assets (nil from the store) →
	// both flows serialise as null and utilisation is omitted.
	if env.Data[1].Utilization30dPct != nil {
		t.Errorf("Utilization30dPct (pool 2) = %v, want nil (multi-asset window)", *env.Data[1].Utilization30dPct)
	}
	if !strings.Contains(body, `"net_supplied_30d":null`) || !strings.Contains(body, `"net_borrowed_30d":null`) {
		t.Errorf("multi-asset pool must serialise its net flows as null, got: %s", body)
	}
}

// TestLendingPools_CrossAssetFlowsHaveNoUtilization pins that a pool
// whose window flows cross reserve assets never gets a utilisation
// figure: 1e13 XLM stroops supplied and 1e12 USDC units borrowed are
// not "10% utilised" — the store withholds the sums and the handler
// must not synthesise a ratio from anything else.
func TestLendingPools_CrossAssetFlowsHaveNoUtilization(t *testing.T) {
	reader := &stubLendingReader{pools: []timescale.BlendPoolSummary{{
		Pool:     "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD",
		LastSeen: time.Date(2026, 5, 9, 10, 15, 52, 0, time.UTC),
	}}}
	srv := v1.New(v1.Options{Lending: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if strings.Contains(body, "utilization_30d_pct") {
		t.Errorf("cross-asset pool served a utilisation figure: %s", body)
	}
	if !strings.Contains(body, `"net_supplied_30d":null`) {
		t.Errorf("cross-asset pool must serve null net flows, got: %s", body)
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

// TestLendingPoolReserves_AssetsTimeoutReturns503 and its sibling below
// pin the behaviour: the reserves handler's 15s ceiling must surface as a
// RETRYABLE 503 + `lending-timeout`, exactly like handleLendingPools in
// the same file already does — not a 500.
//
// A 500 tells clients "broken, don't retry", costs an availability
// point in the sla-probe's 5xx accounting, and is not even a status the
// OpenAPI spec declares for this path (it declares 400 + 503 only).
//
// Proven red against a handler without the mapping: status 500, no
// `lending-timeout` in the body.
func TestLendingPoolReserves_AssetsTimeoutReturns503(t *testing.T) {
	pool := mkCStrkey(t, 7)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{},
		Lending:  &stubLendingReader{err: context.DeadlineExceeded},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (BlendPoolAssets deadline is retryable capacity, not an internal fault)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "lending-timeout") {
		t.Errorf("expected `lending-timeout` problem type in body, got: %s", body)
	}
}

// Same contract at the second reader hop: the contract_data scan that
// decodes reserve state.
func TestLendingPoolReserves_ReservesTimeoutReturns503(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 20)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{err: context.DeadlineExceeded},
		Lending:  &stubLendingReader{assets: []string{asset}},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (BlendPoolReserves deadline is retryable capacity)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "lending-timeout") {
		t.Errorf("expected `lending-timeout` problem type in body, got: %s", body)
	}
}

// A NON-deadline storage error must still be a 500 — the fix maps the
// timeout branch only, it does not blanket-downgrade real faults.
func TestLendingPoolReserves_StorageErrorStays500(t *testing.T) {
	pool := mkCStrkey(t, 7)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{},
		Lending:  &stubLendingReader{err: errors.New("storage broke")},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a non-deadline storage error", resp.StatusCode)
	}
}

// A V1 pool's ResData rates carry 9 decimals, not V2's 12: the handler
// must hand the reader the pool's own generation so supplied / borrowed
// / TVL are not served 1000x low.
func TestLendingPoolReserves_PassesPoolVersionToReader(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 20)
	v1Pool := blend.PoolV1
	explorer := &stubExplorerReader{}
	srv := v1.New(v1.Options{
		Explorer: explorer,
		Lending:  &stubLendingReader{assets: []string{asset}, version: &v1Pool},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(explorer.reserveCalls) != 1 || explorer.reserveCalls[0] != blend.PoolV1 {
		t.Errorf("BlendPoolReserves called with versions %v, want [PoolV1]", explorer.reserveCalls)
	}
}

// A pool with no known V1/V2 lineage has no knowable rate scale: its
// reserves are withheld (empty, no TVL) and the lake is never read.
func TestLendingPoolReserves_UnknownLineageWithheld(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 20)
	unknown := blend.PoolVersionUnknown
	explorer := &stubExplorerReader{reserves: []clickhouse.BlendReserveState{{
		Pool: pool, Asset: asset, Decimals: 7,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(1), BorrowedUnderlying: big.NewInt(0)},
	}}}
	srv := v1.New(v1.Options{
		Explorer: explorer,
		Lending:  &stubLendingReader{assets: []string{asset}, version: &unknown},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.LendingPoolReservesView `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Reserves) != 0 || env.Data.TVLUSD != nil {
		t.Errorf("unknown-lineage pool served reserves=%d tvl=%v, want none", len(env.Data.Reserves), env.Data.TVLUSD)
	}
	if len(explorer.reserveCalls) != 0 {
		t.Errorf("unknown-lineage pool read the lake with versions %v", explorer.reserveCalls)
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

// barrierPriceStall caps how long one parked LatestPrice waits when the
// barrier is never reached. It bounds the RED run (a serial handler pays it
// once per reserve, then fails the peak assertion) without letting the test
// hang; the GREEN run never waits at all, because the fan-out saturates the
// barrier and releases it.
const barrierPriceStall = 200 * time.Millisecond

// barrierPriceReader is a v1.PriceReader that parks every LatestPrice call
// until `want` of them are in flight AT ONCE, recording the peak it observed.
//
// This is the instrument that detects a serial fan-out. The route's cost is a
// per-reserve DB fan-out, so the only thing separating a serial handler
// from a bounded-parallel one is whether those reads OVERLAP — and a stub that answers
// instantly makes a serial walk and a parallel one look identical. Parking
// each call turns concurrency into an observable: peak 1 is a serial loop,
// peak len(reserves) is the bounded fan-out.
type barrierPriceReader struct {
	want int

	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int

	release   chan struct{}
	closeOnce sync.Once
}

func newBarrierPriceReader(want int) *barrierPriceReader {
	return &barrierPriceReader{want: want, release: make(chan struct{})}
}

func (r *barrierPriceReader) LatestPrice(_ context.Context, _, _ canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	saturated := r.inFlight >= r.want
	r.mu.Unlock()

	if saturated {
		r.closeOnce.Do(func() { close(r.release) })
	}
	select {
	case <-r.release:
	case <-time.After(barrierPriceStall):
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	// A miss is the honest answer for a reserve token with no price feed, and
	// it is also the path that costs the most round-trips in production.
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (r *barrierPriceReader) RecentClosedSnapshots(_ context.Context, _, _ canonical.Asset, _ int) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

func (r *barrierPriceReader) observed() (peak, calls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak, r.calls
}

// TestLendingPoolReserves_PricingFansOutBounded is the regression guard
// on the handler half.
//
// /v1/lending/pools/{pool}/reserves timed out at 12.1s on the largest Blend
// pool while a SMALL pool answered in 9.31s — 78% of the same ceiling. That
// spread is the signature of a per-row cost: the handler priced each reserve
// in turn, and every buildReserveView is one or more USD-price round-trips.
// The route's latency therefore carried a term linear in the reserve count,
// and the largest pool merely crossed the line first.
//
// The guard asserts the PROPERTY, not a duration: every reserve's pricing read
// must be in flight at the same time (up to the fan-out cap), which is exactly
// what a serial `for range states` cannot do. A stopwatch assertion would be a
// function of the box; and the existing exclusion guard
// (dex_tvl_exclusion_claims_internal_test.go) only ever proved the path was
// REGISTERED, which is why this shipped.
//
// Proven RED against a serial handler: peak concurrency 1 over 6 calls,
// the run taking 6 × barrierPriceStall.
func TestLendingPoolReserves_PricingFansOutBounded(t *testing.T) {
	pool := mkCStrkey(t, 7)

	const reserveCount = 6
	assets := make([]string, reserveCount)
	states := make([]clickhouse.BlendReserveState, reserveCount)
	for i := range assets {
		assets[i] = mkCStrkey(t, byte(20+i))
		states[i] = clickhouse.BlendReserveState{
			Pool:     pool,
			Asset:    assets[i],
			Decimals: 7,
			Metrics: blend.ReserveMetrics{
				SuppliedUnderlying: big.NewInt(int64(1_000_000 * (i + 1))),
				BorrowedUnderlying: big.NewInt(int64(400_000 * (i + 1))),
				UtilizationPct:     40,
			},
		}
	}

	prices := newBarrierPriceReader(reserveCount)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{reserves: states},
		Lending:  &stubLendingReader{assets: assets},
		Prices:   prices,
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}

	var env struct {
		Data v1.LendingPoolReservesView `json:"data"`
	}
	mustDecode(t, resp, &env)

	peak, calls := prices.observed()
	if calls == 0 {
		t.Fatal("the handler never consulted the price reader — the fixture no longer exercises the per-reserve pricing path, so the concurrency assertion below would be vacuous")
	}
	if peak < reserveCount {
		t.Errorf("peak concurrent price reads = %d over %d calls, want %d — the per-reserve pricing is SERIAL, so the route's latency grows with the reserve count (#504)",
			peak, calls, reserveCount)
	}

	// The fan-out must not disturb the answer: every reserve present, in the
	// reader's order, carrying its own exact figures.
	if len(env.Data.Reserves) != reserveCount {
		t.Fatalf("len(reserves) = %d, want %d", len(env.Data.Reserves), reserveCount)
	}
	for i, rv := range env.Data.Reserves {
		if rv.Asset != assets[i] {
			t.Errorf("reserve %d asset = %q, want %q — the fan-out reordered the response", i, rv.Asset, assets[i])
		}
		if want := states[i].Metrics.SuppliedUnderlying.String(); rv.Supplied != want {
			t.Errorf("reserve %d supplied = %q, want %q — a fan-out slot landed on the wrong index", i, rv.Supplied, want)
		}
	}
	// Nothing priced (every lookup missed), so TVL is withheld rather than
	// reported as zero.
	if env.Data.TVLUSD != nil {
		t.Errorf("tvl_usd = %q, want null when no reserve priced", *env.Data.TVLUSD)
	}
}

// The 8s-ceiling comment on handleLendingPools must not cite specific issue
// numbers from the cold-path-protection series. Those
// numbers resolve to OTHER endpoints (pools, sources, coins, chart, history,
// oracle) — none of them lending — and no PR by any of those numbers exists
// in this repo's remote. A reader following the reference lands on unrelated
// content instead of lending's own fix. The comment must describe the
// pattern without citing numbers that don't point back to lending.
func TestLendingPoolsCommentDoesNotCiteUnrelatedIssueNumbers(t *testing.T) {
	src, err := os.ReadFile("lending.go")
	if err != nil {
		t.Fatalf("read lending.go: %v", err)
	}
	for _, stale := range []string{"#1082", "#1099", "#1104"} {
		if strings.Contains(string(src), stale) {
			t.Errorf("lending.go still cites %s, which resolves to an unrelated endpoint's fix, not lending's own", stale)
		}
	}
}

// assetKeyedPriceReader prices only the base assets it names, in USD; every
// other read is an honest miss.
type assetKeyedPriceReader struct {
	usd map[string]string
}

func (r *assetKeyedPriceReader) LatestPrice(_ context.Context, base, _ canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if p, ok := r.usd[base.String()]; ok {
		return v1.PriceSnapshot{Price: p}, []string{"sdex"}, false, nil
	}
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (r *assetKeyedPriceReader) RecentClosedSnapshots(_ context.Context, _, _ canonical.Asset, _ int) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

// TestLendingPoolReserves_PartialTVLIsLowerBound pins the money invariant on
// the pool's tvl_usd: a sum that leaves out an unpriced reserve is served
// with lower_bound=true, and a sum over every reserve is not.
//
// RED without the fix: lower_bound absent on the partial sum.
func TestLendingPoolReserves_PartialTVLIsLowerBound(t *testing.T) {
	pool := mkCStrkey(t, 9)
	priced, unpriced := mkCStrkey(t, 40), mkCStrkey(t, 41)
	reserve := func(asset string, supplied int64) clickhouse.BlendReserveState {
		return clickhouse.BlendReserveState{
			Pool: pool, Asset: asset, Decimals: 7, DecimalsFound: true,
			Metrics: blend.ReserveMetrics{
				SuppliedUnderlying: big.NewInt(supplied),
				BorrowedUnderlying: big.NewInt(0),
			},
		}
	}
	cases := []struct {
		name      string
		usd       map[string]string
		wantTVL   string
		wantLower bool
	}{
		// 1 token at $2 priced; the other reserve's 500 tokens excluded.
		{"one reserve unpriced", map[string]string{priced: "2"}, "2.00", true},
		{"every reserve priced", map[string]string{priced: "2", unpriced: "3"}, "1502.00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
					reserve(priced, 10_000_000), reserve(unpriced, 5_000_000_000),
				}},
				Lending: &stubLendingReader{assets: []string{priced, unpriced}},
				Prices:  &assetKeyedPriceReader{usd: tc.usd},
			})
			resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			mustDecode(t, resp, &env)
			var tvl *string
			if err := json.Unmarshal(env.Data["tvl_usd"], &tvl); err != nil || tvl == nil || *tvl != tc.wantTVL {
				t.Fatalf("tvl_usd = %s, want %q", env.Data["tvl_usd"], tc.wantTVL)
			}
			raw, present := env.Data["lower_bound"]
			var lower bool
			if !present || json.Unmarshal(raw, &lower) != nil || lower != tc.wantLower {
				t.Errorf("lower_bound = %s (present=%v), want %v — a tvl_usd that excludes an unpriced reserve must be marked a lower bound",
					raw, present, tc.wantLower)
			}
		})
	}
}

// TestLendingPools_CacheUnavailable503 — The handler had a
// 503 timeout path and a 500 fallthrough; MISCONF now lands on the
// cache-unavailable 503 instead of the generic 500.
func TestLendingPools_CacheUnavailable503(t *testing.T) {
	reader := &stubLendingReader{err: miscOnfErr}
	srv := v1.New(v1.Options{Lending: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/lending/pools")
	assertCacheUnavailable(t, resp)
}

// TestLendingPoolReserves_NoConfigNeverInventsDecimals pins CA2-A14-harden-3:
// a reserve whose rate config is missing must not be USD-valued with the
// placeholder exponent 7. Every reserve is priced at $2 per whole token.
//
//   - eighteen: an 18-dp Soroban token with no config — valued with the
//     contract's own declared decimals (3 tokens = $6.00), not 10^11 × that.
//   - unknown: no config and no declared decimals — USD withheld and left
//     out of tvl_usd rather than published off by an unknown power of ten.
//   - xlmSAC: the native SAC with no config — 7 by protocol, still priced.
func TestLendingPoolReserves_NoConfigNeverInventsDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	eighteen := mkCStrkey(t, 40)
	unknown := mkCStrkey(t, 41)
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	states := []clickhouse.BlendReserveState{
		reserveState(pool, eighteen, 7, false, new(big.Int).Mul(big.NewInt(3), e18), e18),
		reserveState(pool, unknown, 7, false, big.NewInt(50_000_000), big.NewInt(10_000_000)),
		reserveState(pool, xlmSAC, 7, false, big.NewInt(30_000_000), big.NewInt(10_000_000)),
	}
	// The stored price is the RAW smallest-unit ratio; the confirmed 18
	// normalises eighteen's by 10^11 to $2 per whole token.
	prices := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
		usdKey(t, eighteen): {Price: "0.00000000002"},
		usdKey(t, unknown):  {Price: "2"},
		"native/fiat:USD":   {Price: "2"},
	}}
	srv := v1.New(v1.Options{
		Explorer:            &stubExplorerReader{reserves: states},
		Lending:             &stubLendingReader{assets: []string{eighteen, unknown, xlmSAC}},
		Prices:              prices,
		TokenDecimals:       newPerContractDecimals(map[string]uint32{eighteen: 18}),
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, eighteen, 18),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 3 {
		t.Fatalf("len(reserves) = %d, want 3", len(got.Reserves))
	}

	r18 := got.Reserves[0]
	if strOrNil(r18.SuppliedUSD) != "6.00" || strOrNil(r18.BorrowedUSD) != "2.00" || r18.Decimals != 18 {
		t.Errorf("18-dp reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 18",
			strOrNil(r18.SuppliedUSD), strOrNil(r18.BorrowedUSD), r18.Decimals)
	}
	ru := got.Reserves[1]
	if ru.SuppliedUSD != nil || ru.BorrowedUSD != nil {
		t.Errorf("unknown-decimals reserve: supplied_usd=%s borrowed_usd=%s, want both null",
			strOrNil(ru.SuppliedUSD), strOrNil(ru.BorrowedUSD))
	}
	if ru.Supplied != "50000000" || ru.Borrowed != "10000000" {
		t.Errorf("unknown-decimals reserve token amounts = %s/%s, want exact 50000000/10000000", ru.Supplied, ru.Borrowed)
	}
	rx := got.Reserves[2]
	if strOrNil(rx.SuppliedUSD) != "6.00" || strOrNil(rx.BorrowedUSD) != "2.00" || rx.Decimals != 7 {
		t.Errorf("native SAC reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 7",
			strOrNil(rx.SuppliedUSD), strOrNil(rx.BorrowedUSD), rx.Decimals)
	}
	if strOrNil(got.TVLUSD) != "12.00" {
		t.Errorf("tvl_usd = %s, want 12.00 (18-dp 6.00 + SAC 6.00; the unknown-decimals reserve excluded)", strOrNil(got.TVLUSD))
	}
	if !got.LowerBound {
		t.Error("lower_bound = false, want true: tvl_usd excludes the unknown-decimals reserve")
	}
}

// TestLendingPoolReserves_ConfigDecimalsAreAuthoritative: a captured reserve
// config's decimals price the reserve as before; the lake is not consulted.
func TestLendingPoolReserves_ConfigDecimalsAreAuthoritative(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 42)
	dec := newPerContractDecimals(map[string]uint32{asset: 18})
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			reserveState(pool, asset, 6, true, big.NewInt(3_000_000), big.NewInt(1_000_000)),
		}},
		Lending: &stubLendingReader{assets: []string{asset}},
		// Raw ratio 20, normalised by the confirmed 6 to $2 per whole token.
		Prices:              &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdKey(t, asset): {Price: "20"}}},
		TokenDecimals:       dec,
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, asset, 6),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if strOrNil(rv.SuppliedUSD) != "6.00" || strOrNil(rv.BorrowedUSD) != "2.00" || rv.Decimals != 6 {
		t.Errorf("configured reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 6",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD), rv.Decimals)
	}
	if dec.wasConsulted(asset) {
		t.Error("lake token decimals consulted for a reserve whose config already declares them")
	}
}

// TestLendingPoolReserves_UnpricedReserveReportsDeclaredDecimals: with no
// config and no USD price, the reserve's decimals field still carries the
// token's declared exponent (clients scale supplied/borrowed with it), not 7.
func TestLendingPoolReserves_UnpricedReserveReportsDeclaredDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 43)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			reserveState(pool, asset, 7, false, e18, e18),
		}},
		Lending:       &stubLendingReader{assets: []string{asset}},
		Prices:        &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{}},
		TokenDecimals: newPerContractDecimals(map[string]uint32{asset: 18}),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if rv.Decimals != 18 || rv.SuppliedUSD != nil || got.TVLUSD != nil {
		t.Errorf("unpriced reserve: decimals=%d supplied_usd=%s tvl_usd=%s, want 18 / <nil> / <nil>",
			rv.Decimals, strOrNil(rv.SuppliedUSD), strOrNil(got.TVLUSD))
	}
}

// TestLendingPoolReserves_PriceScaleMustMatchReserveDecimals: the USD price
// is normalised through the nonstandard-decimals projection (else 7), so a
// 6-decimal reserve with no projection row carries the RAW ratio. Dividing
// its amounts by 10^6 would publish ten times the value (30.00 for 3 tokens
// at a raw 1, where the truth is 3.00) — the reserve's USD figures are
// withheld instead and it leaves tvl_usd. With the row, it is valued.
func TestLendingPoolReserves_PriceScaleMustMatchReserveDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 44)
	newSrv := func(nd *v1.NonstandardDecimalsCache) *v1.Server {
		return v1.New(v1.Options{
			Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
				reserveState(pool, asset, 6, true, big.NewInt(30_000_000), big.NewInt(10_000_000)),
			}},
			Lending:             &stubLendingReader{assets: []string{asset}},
			Prices:              &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdKey(t, asset): {Price: "1"}}},
			NonstandardDecimals: nd,
		})
	}

	got := getReserves(t, newSrv(nil), pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if rv.SuppliedUSD != nil || rv.BorrowedUSD != nil || got.TVLUSD != nil {
		t.Errorf("no projection row: supplied_usd=%s borrowed_usd=%s tvl_usd=%s, want all null",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD), strOrNil(got.TVLUSD))
	}
	if rv.Decimals != 6 || rv.Supplied != "30000000" {
		t.Errorf("decimals=%d supplied=%s, want 6 / 30000000 served either way", rv.Decimals, rv.Supplied)
	}

	got = getReserves(t, newSrv(nonstandardDecimalsCacheWith(t, asset, 6)), pool)
	rv = got.Reserves[0]
	if strOrNil(rv.SuppliedUSD) != "3.00" || strOrNil(rv.BorrowedUSD) != "1.00" {
		t.Errorf("confirmed 6: supplied_usd=%s borrowed_usd=%s, want 3.00 / 1.00",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD))
	}
}
