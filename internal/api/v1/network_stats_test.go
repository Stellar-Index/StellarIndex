package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubNetworkStatsReader is the in-memory test seam — same pattern
// as the per-handler stubs elsewhere in this package. Captures the
// last-call context for the upstream-error path test.
type stubNetworkStatsReader struct {
	stats timescale.NetworkStats
	err   error
}

func (r *stubNetworkStatsReader) GetNetworkStats(_ context.Context) (timescale.NetworkStats, error) {
	if r.err != nil {
		return timescale.NetworkStats{}, r.err
	}
	return r.stats, nil
}

// TestNetworkStats_503WhenReaderNil pins feature-gated degradation: /v1/network/stats backs the explorer's home strip,
// so 503 lets the strip hide rather than render zeroes.
func TestNetworkStats_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestNetworkStats_HappyPath pins the wire shape the explorer's HomeNetworkStrip reads verbatim; Volume24hUSD is a
// pointer-to-string per ADR-0003.
func TestNetworkStats_HappyPath(t *testing.T) {
	vol := "3958193034.60"
	reader := &stubNetworkStatsReader{
		stats: timescale.NetworkStats{
			Volume24hUSD:    &vol,
			MarketsCount24h: 22158,
			AssetsIndexed:   86114,
			LatestLedger:    62484113,
		},
	}
	srv := v1.New(v1.Options{NetworkStats: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.NetworkStats `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.Volume24hUSD == nil || *env.Data.Volume24hUSD != vol {
		t.Errorf("Volume24hUSD = %v, want %q", env.Data.Volume24hUSD, vol)
	}
	if env.Data.MarketsCount24h != 22158 {
		t.Errorf("MarketsCount24h = %d, want 22158", env.Data.MarketsCount24h)
	}
	if env.Data.AssetsIndexed != 86114 {
		t.Errorf("AssetsIndexed = %d, want 86114", env.Data.AssetsIndexed)
	}
	if env.Data.LatestLedger != 62484113 {
		t.Errorf("LatestLedger = %d, want 62484113", env.Data.LatestLedger)
	}
	// Source counts come from the in-memory external.Registry —
	// can't pin exact values (they grow when sources land) but the
	// invariant is exchanges ≤ total and total > 0.
	if env.Data.TotalSources <= 0 {
		t.Errorf("TotalSources = %d, want > 0", env.Data.TotalSources)
	}
	if env.Data.ExchangeSources < 0 || env.Data.ExchangeSources > env.Data.TotalSources {
		t.Errorf("ExchangeSources = %d should be in [0, %d]",
			env.Data.ExchangeSources, env.Data.TotalSources)
	}
}

// TestNetworkStats_NullVolumeOmitted pins omitempty: with no USD-equivalent trades in 24h the volume field is absent,
// so callers can tell "no data" from "0".
func TestNetworkStats_NullVolumeOmitted(t *testing.T) {
	reader := &stubNetworkStatsReader{
		stats: timescale.NetworkStats{
			Volume24hUSD:    nil, // no USD-equivalent trades
			MarketsCount24h: 0,
			AssetsIndexed:   86114,
			LatestLedger:    62484113,
		},
	}
	srv := v1.New(v1.Options{NetworkStats: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if strings.Contains(body, `"volume_24h_usd"`) {
		t.Errorf("volume_24h_usd should be absent (omitempty), got: %s", body)
	}
}

// TestNetworkStats_ReaderError500: a storage failure surfaces as a 500 problem+json so the explorer's error branch fires
// instead of a "data: null" success.
func TestNetworkStats_ReaderError500(t *testing.T) {
	reader := &stubNetworkStatsReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{NetworkStats: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "network-stats-error") {
		t.Errorf("error type missing: %s", body)
	}
}

// stubStaleNetworkStatsReader implements the unexported stale-aware capability structurally (the handler type-asserts
// the method set), exercising the honest-freshness path without the real TTL cache.
type stubStaleNetworkStatsReader struct {
	stats      timescale.NetworkStats
	observedAt time.Time
	stale      bool
}

func (r *stubStaleNetworkStatsReader) GetNetworkStats(_ context.Context) (timescale.NetworkStats, error) {
	return r.stats, nil
}

func (r *stubStaleNetworkStatsReader) GetNetworkStatsAt(_ context.Context) (timescale.NetworkStats, time.Time, bool, error) {
	return r.stats, r.observedAt, r.stale, nil
}

// TestNetworkStats_HonestStaleAsOf pins that an SWR-stale value yields flags.stale=true and as_of equal to its real
// observation time, never stale:false / as_of=now. Mirrors the /v1/markets contract.
func TestNetworkStats_HonestStaleAsOf(t *testing.T) {
	observed := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	reader := &stubStaleNetworkStatsReader{
		stats:      timescale.NetworkStats{MarketsCount24h: 100, AssetsIndexed: 200, LatestLedger: 300},
		observedAt: observed,
		stale:      true,
	}
	srv := v1.New(v1.Options{NetworkStats: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
		AsOf time.Time `json:"as_of"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if !env.Flags.Stale {
		t.Errorf("flags.stale = false, want true (SWR stale-serve must not assert fresh); body=%s", body)
	}
	if !env.AsOf.Equal(observed) {
		t.Errorf("as_of = %s, want the served value's observed-at %s (never now over aged data)", env.AsOf, observed)
	}
}

// TestNetworkStats_FreshServeNotStale is the other side: a fresh serve
// stamps stale=false and an as_of at the served value's recent observed-at
// (here still non-zero and NOT flagged stale).
func TestNetworkStats_FreshServeNotStale(t *testing.T) {
	observed := time.Date(2026, 8, 25, 11, 59, 0, 0, time.UTC)
	reader := &stubStaleNetworkStatsReader{
		stats:      timescale.NetworkStats{MarketsCount24h: 1},
		observedAt: observed,
		stale:      false,
	}
	srv := v1.New(v1.Options{NetworkStats: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/network/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	body, _ := readAll(resp)
	_ = json.NewDecoder(strings.NewReader(body)).Decode(&env)
	if env.Flags.Stale {
		t.Errorf("flags.stale = true on a fresh serve, want false; body=%s", body)
	}
}
