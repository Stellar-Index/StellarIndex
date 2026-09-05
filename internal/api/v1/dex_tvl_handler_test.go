package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// countingAquariusReader is a fallible reserve reader that counts every
// read, so a test can prove the drill-down never reads per request.
type countingAquariusReader struct {
	pools []timescale.AquariusPoolReserve
	err   error
	calls atomic.Int32
}

func (s *countingAquariusReader) LatestAquariusReserves(context.Context, int) ([]timescale.AquariusPoolReserve, error) {
	s.calls.Add(1)
	return s.pools, s.err
}

// countingTVLPricer is the stub pricer with a call counter, so a test
// can prove the drill-down consults no price tier per request. The
// price rule stays in one place — this only counts the way through it.
type countingTVLPricer struct {
	calls atomic.Int32
}

func (p *countingTVLPricer) USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error) {
	p.calls.Add(1)
	return stubTVLPricerT{}.USDPriceAt(ctx, asset, at)
}

func aquariusPool(id string, ledger uint32, rawXLM int64) timescale.AquariusPoolReserve {
	return timescale.AquariusPoolReserve{
		ContractID: id,
		ObservedAt: time.Now(),
		Ledger:     ledger,
		Legs: []timescale.AquariusReserveLeg{
			{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(rawXLM))},
		},
	}
}

// protocolTVLEnvelope is the wire shape the tests decode; kept local so
// the test asserts JSON field names rather than Go struct tags.
type protocolTVLEnvelope struct {
	Data struct {
		Protocol       string `json:"protocol"`
		CarriedForward bool   `json:"carried_forward"`
		TVL            struct {
			TVLUSD        string `json:"tvl_usd"`
			PoolsTotal    int    `json:"pools_total"`
			UnpricedPools int    `json:"unpriced_pools"`
			AsOf          string `json:"as_of"`
		} `json:"tvl"`
		Pools []struct {
			Pool       string `json:"pool"`
			TVLUSD     string `json:"tvl_usd"`
			Priced     bool   `json:"priced"`
			AsOfLedger uint32 `json:"as_of_ledger"`
			Legs       []struct {
				Token    string `json:"token"`
				Reserve  string `json:"reserve"`
				Asset    string `json:"asset"`
				Basis    string `json:"basis"`
				USD      string `json:"usd"`
				Excluded string `json:"excluded"`
			} `json:"legs"`
		} `json:"pools"`
	} `json:"data"`
	Sources []string `json:"sources"`
	Flags   struct {
		Stale bool `json:"stale"`
	} `json:"flags"`
}

func decodeProtocolTVL(t *testing.T, resp *http.Response) protocolTVLEnvelope {
	t.Helper()
	var env protocolTVLEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func decodeProblem(t *testing.T, resp *http.Response) v1.Problem {
	t.Helper()
	var p v1.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return p
}

// GET /v1/protocols/{name}/tvl is the per-pool drill-down behind the
// `tvl` block on /v1/protocols (#338): every pool the protocol figure
// was summed from, every reserve leg of each, the served-price identity
// each leg was valued under, and — for a leg that contributed nothing —
// the reason it was excluded rather than a silent zero.
func TestHandleProtocolTVL_ServesThePoolBreakdown(t *testing.T) {
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{
		AquariusReserves: stubTVLAquariusReader{pools: []timescale.AquariusPoolReserve{{
			ContractID: "CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV",
			ObservedAt: time.Now(),
			Ledger:     62_900_000,
			Legs: []timescale.AquariusReserveLeg{
				// 40 XLM-SAC raw units at the 1e7 anchor scale × $0.25 = $10.
				{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(400_000_000))},
				// A position whose token address never resolved: excluded, never zero-valued.
				{TokenIndex: 1, Token: "", Reserve: canonical.NewAmount(big.NewInt(42))},
			},
		}}},
		Pricer: stubTVLPricerT{},
	})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("cache refresh: %v", err)
	}
	srv := v1.New(v1.Options{DEXTVL: cache})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/protocols/aquarius/tvl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/protocols/aquarius/tvl = %d, want 200", resp.StatusCode)
	}
	// The test server runs without a CDN, so only the client-tier half of
	// the short current-state band is emitted; the CDN variant is pinned
	// in middleware/cachecontrol_test.go.
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=30" {
		t.Errorf("Cache-Control = %q, want the short current-state band", got)
	}
	env := decodeProtocolTVL(t, resp)
	d := env.Data
	if d.Protocol != "aquarius" || d.TVL.TVLUSD != "10.00" || d.TVL.PoolsTotal != 1 || d.TVL.UnpricedPools != 1 {
		t.Errorf("data = %+v, want aquarius 10.00 over 1 pool with 1 unpriced", d)
	}
	if d.CarriedForward || env.Flags.Stale {
		t.Error("a protocol computed this cycle is neither carried_forward nor stale")
	}
	if len(env.Sources) != 1 || env.Sources[0] != "aquarius" {
		t.Errorf("sources = %v, want [aquarius]", env.Sources)
	}
	if len(d.Pools) != 1 {
		t.Fatalf("pools = %+v, want exactly the one aquarius pool", d.Pools)
	}
	p := d.Pools[0]
	if p.Pool != "CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV" || p.TVLUSD != "10.00" || p.Priced || p.AsOfLedger != 62_900_000 {
		t.Errorf("pool = %+v, want 10.00, unpriced, as_of_ledger 62900000", p)
	}
	if len(p.Legs) != 2 {
		t.Fatalf("legs = %+v, want both reserve legs", p.Legs)
	}
	xlm, unresolved := p.Legs[0], p.Legs[1]
	if xlm.Token != canonical.XLMSacContractID || xlm.Reserve != "400000000" || xlm.Asset != "native" ||
		xlm.Basis != "served_usd_price" || xlm.USD != "10.00" || xlm.Excluded != "" {
		t.Errorf("XLM leg = %+v, want valued at 10.00 under asset native via the served price", xlm)
	}
	if unresolved.Token != "" || unresolved.Reserve != "42" || unresolved.USD != "" || unresolved.Basis != "" ||
		unresolved.Excluded != "unresolved_token" {
		t.Errorf("unresolved leg = %+v, want excluded=unresolved_token and NO usd", unresolved)
	}

	// The tvl block is the SAME one the directory row publishes for the
	// same snapshot — the two surfaces must be checkable against each
	// other.
	rowResp := mustGet(t, ts.URL+"/v1/protocols")
	var rows struct {
		Data v1.ProtocolsView `json:"data"`
	}
	mustDecode(t, rowResp, &rows)
	aq := protocolRow(t, rows.Data.Protocols, "aquarius")
	if aq.TVL == nil || aq.TVL.TVLUSD != d.TVL.TVLUSD || aq.TVL.AsOf != d.TVL.AsOf {
		t.Errorf("directory row tvl = %+v, drill-down tvl = %+v — same snapshot, must match", aq.TVL, d.TVL)
	}
}

// Degradation is explicit and typed, never an empty list dressed as a
// figure: unknown name → 404; known protocol with no derivation → 404
// naming the standing reason; no cache / cold cache → 503.
func TestHandleProtocolTVL_Refusals(t *testing.T) {
	cold := v1.NewDEXTVLCache(v1.DEXTVLSources{})
	warm := v1.NewDEXTVLCache(v1.DEXTVLSources{
		AquariusReserves: stubTVLAquariusReader{pools: []timescale.AquariusPoolReserve{aquariusPool(
			"CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV", 1, 400_000_000)}},
		Pricer: stubTVLPricerT{},
	})
	if err := warm.Refresh(context.Background()); err != nil {
		t.Fatalf("warm refresh: %v", err)
	}

	cases := []struct {
		name       string
		cache      *v1.DEXTVLCache
		path       string
		status     int
		typeSuffix string
		detail     string
	}{
		{"unknown protocol", warm, "/v1/protocols/nope/tvl", http.StatusNotFound, "protocol-not-found", "unknown protocol name"},
		{"known, no derivation (standing exclusion)", warm, "/v1/protocols/sdex/tvl", http.StatusNotFound, "protocol-tvl-not-derived", "order book"},
		{"known, derivation not wired", warm, "/v1/protocols/soroswap/tvl", http.StatusNotFound, "protocol-tvl-not-derived", "not wired"},
		{"no cache wired", nil, "/v1/protocols/aquarius/tvl", http.StatusServiceUnavailable, "dex-tvl-unavailable", "hasn't wired"},
		{"cache not yet refreshed", cold, "/v1/protocols/aquarius/tvl", http.StatusServiceUnavailable, "dex-tvl-unavailable", "not completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpTestServer(t, v1.New(v1.Options{DEXTVL: tc.cache}))
			resp := mustGet(t, ts.URL+tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.status)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q on a problem, want no-store", got)
			}
			p := decodeProblem(t, resp)
			if !strings.HasSuffix(p.Type, tc.typeSuffix) || !strings.Contains(p.Detail, tc.detail) {
				t.Errorf("problem = %+v, want type …/%s with detail containing %q", p, tc.typeSuffix, tc.detail)
			}
		})
	}
}

// A protocol whose reserve read failed this cycle is served with its
// previous figure AND pools, labelled carried_forward with flags.stale
// — the headline total refuses that figure, and the drill-down is where
// to see what was refused.
func TestHandleProtocolTVL_CarriedForwardIsLabelledStale(t *testing.T) {
	reader := &countingAquariusReader{pools: []timescale.AquariusPoolReserve{aquariusPool(
		"CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV", 7, 400_000_000)}}
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{AquariusReserves: reader, Pricer: stubTVLPricerT{}})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	reader.err = errors.New("lake unavailable")
	if err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("second refresh should report the read failure")
	}
	ts := httpTestServer(t, v1.New(v1.Options{DEXTVL: cache}))

	resp := mustGet(t, ts.URL+"/v1/protocols/aquarius/tvl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a carried figure is served, labelled", resp.StatusCode)
	}
	env := decodeProtocolTVL(t, resp)
	if !env.Data.CarriedForward || !env.Flags.Stale {
		t.Errorf("carried_forward = %v, flags.stale = %v; want both true", env.Data.CarriedForward, env.Flags.Stale)
	}
	if len(env.Data.Pools) != 1 || env.Data.Pools[0].TVLUSD != "10.00" {
		t.Errorf("pools = %+v, want the previous cycle's pool carried with the figure", env.Data.Pools)
	}
}

// The drill-down reads only the in-process snapshot: no reserve reader
// and no price tier is touched per request, so it needs no budget of
// its own and its cost is bounded by serialisation alone. That is a
// claim about what the handler TOUCHES, so it is pinned by counting
// rather than by a clock — over a run of requests against a snapshot
// far larger than production's pool count (r1 serves a few hundred
// Aquarius pools) the reserve read and the price lookups both stand
// still, and every response carries the same data block byte for byte
// (only the envelope's as_of stamp moves). What the snapshot costs to
// serialise is measured by BenchmarkHandleProtocolTVL_LargeSnapshot,
// on a quiet machine: a wall-clock ceiling in the unit suite measures
// how loaded the runner is, not what the handler does. The 5 s ceiling
// that used to sit here spent 1.1 s of itself under -race on an idle
// machine and breached at 5.3 s under CPU load, on one unchanged
// commit.
func TestHandleProtocolTVL_ServesFromMemory(t *testing.T) {
	const pools = 5_000
	reader := &countingAquariusReader{}
	for i := 0; i < pools; i++ {
		reader.pools = append(reader.pools, aquariusPool(fmt.Sprintf("POOL-%05d", i), uint32(i+1), int64(i)*10_000_000))
	}
	pricer := &countingTVLPricer{}
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{AquariusReserves: reader, Pricer: pricer})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	reads, prices := reader.calls.Load(), pricer.calls.Load()
	if reads != 1 {
		t.Fatalf("reader calls after refresh = %d, want 1", reads)
	}
	if prices == 0 {
		t.Fatal("the refresh priced nothing, so a standing price count below would prove nothing")
	}
	ts := httpTestServer(t, v1.New(v1.Options{DEXTVL: cache}))

	// A handful of requests, not a crowd: the counters are read after
	// each one and a handler that read per request moves them on the
	// first, so further repetitions buy nothing but runtime. The twenty
	// this loop used to make were a sample size for the clock, and went
	// with it.
	const requests = 5
	var first []byte
	for i := 0; i < requests; i++ {
		body := getProtocolTVL(t, ts.URL+"/v1/protocols/aquarius/tvl")
		if got := reader.calls.Load(); got != reads {
			t.Fatalf("request %d moved the reserve read count to %d, want still %d — the handler must never read the store per request", i, got, reads)
		}
		if got := pricer.calls.Load(); got != prices {
			t.Fatalf("request %d moved the price lookup count to %d, want still %d — the handler must never price per request", i, got, prices)
		}
		data := protocolTVLData(t, body)
		if i == 0 {
			first = data
			var env protocolTVLEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(env.Data.Pools) != pools || env.Data.TVL.PoolsTotal != pools {
				t.Fatalf("pools served = %d (pools_total %d), want %d", len(env.Data.Pools), env.Data.TVL.PoolsTotal, pools)
			}
			continue
		}
		if !bytes.Equal(data, first) {
			t.Fatalf("request %d served a data block of %d bytes against the first request's %d, diverging at byte %d — every request serves the one snapshot, unchanged",
				i, len(data), len(first), firstDifference(data, first))
		}
	}
}

// getProtocolTVL issues one drill-down request and returns the whole
// body, reading it to completion — an abandoned body leaves the server
// writing into a socket nobody drains and costs the next request a
// fresh connection.
func getProtocolTVL(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	return body
}

// protocolTVLData returns the `data` object exactly as it was written,
// so two responses can be compared byte for byte: the envelope around
// it carries an as_of stamped at write time, which differs per
// response and says nothing about the snapshot underneath.
func protocolTVLData(t *testing.T, body []byte) []byte {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatal("response carried no data block")
	}
	return env.Data
}

// firstDifference reports the offset two data blocks diverge at, so a
// failure names where they parted rather than only that they did.
func firstDifference(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// BenchmarkHandleProtocolTVL_LargeSnapshot — informal perf guard over
// the drill-down's only per-request cost, serialising the snapshot. It
// is where the latency half of the test above went: a quiet machine
// reports a number a regression moves by a multiple (a handler that
// re-derived or re-read per request is orders out), where the same
// number inside the gate reported how busy the runner was. Reference
// on an idle arm64 laptop without -race: 1.72 ms/op over a 1.12 MiB
// body, for a snapshot an order of magnitude past the few hundred
// pools r1 serves — 8,700x under the 15 s request deadline. Not
// gated in CI; run manually via
// `go test ./internal/api/v1/ -run xxx -bench HandleProtocolTVL`.
func BenchmarkHandleProtocolTVL_LargeSnapshot(b *testing.B) {
	const pools = 5_000
	reader := &countingAquariusReader{}
	for i := 0; i < pools; i++ {
		reader.pools = append(reader.pools, aquariusPool(fmt.Sprintf("POOL-%05d", i), uint32(i+1), int64(i)*10_000_000))
	}
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{AquariusReserves: reader, Pricer: stubTVLPricerT{}})
	if err := cache.Refresh(context.Background()); err != nil {
		b.Fatalf("refresh: %v", err)
	}
	h := v1.New(v1.Options{
		DEXTVL: cache,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/protocols/aquarius/tvl", nil))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200", rec.Code)
		}
	}
}
