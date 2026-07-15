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

// stubAggregatorsReader is the in-memory test seam for
// /v1/aggregators. Records the `since` bound so the window test can
// pin the 24h contract.
type stubAggregatorsReader struct {
	rows  []timescale.AggregatorRollupRow
	err   error
	since time.Time
}

func (r *stubAggregatorsReader) AggregatorRollup(_ context.Context, since time.Time) ([]timescale.AggregatorRollupRow, error) {
	r.since = since
	if r.err != nil {
		return nil, r.err
	}
	return r.rows, nil
}

// TestAggregators_503WhenReaderNil pins the feature-gated-reader
// degradation — same contract as the sibling registry endpoints.
func TestAggregators_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/aggregators")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
}

// TestAggregators_HappyPath pins the wire shape the explorer
// /aggregators page reads verbatim, including the null-volume
// semantics (null = no USD valuation, NOT zero volume) and the
// zero-stat vault row.
func TestAggregators_HappyPath(t *testing.T) {
	vol := "18211.4052710000000000"
	lastAt := time.Date(2026, 7, 4, 21, 58, 11, 0, time.UTC)
	reader := &stubAggregatorsReader{
		rows: []timescale.AggregatorRollupRow{
			{
				ContractID:   "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
				Name:         "soroswap-router",
				Kind:         "router",
				ProtocolSlug: "soroswap",
				RoutedTrades: 342,
				RoutedVolume: &vol,
				LastRoutedAt: &lastAt,
			},
			{
				ContractID:   "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
				Name:         "defindex-vault-usdc-autocompound",
				Kind:         "aggregator-vault",
				ProtocolSlug: "defindex",
			},
		},
	}
	srv := v1.New(v1.Options{Aggregators: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/aggregators")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.AggregatorRow `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(env.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(env.Data))
	}

	router := env.Data[0]
	if router.Name != "soroswap-router" || router.Kind != "router" || router.Protocol != "soroswap" {
		t.Errorf("router row = %+v, want soroswap-router/router/soroswap", router)
	}
	if router.RoutedTrades24h != 342 {
		t.Errorf("RoutedTrades24h = %d, want 342", router.RoutedTrades24h)
	}
	if router.RoutedVolume24hUSD == nil || *router.RoutedVolume24hUSD != vol {
		t.Errorf("RoutedVolume24hUSD = %v, want %q", router.RoutedVolume24hUSD, vol)
	}
	if router.LastRoutedAt == nil || !router.LastRoutedAt.Equal(lastAt) {
		t.Errorf("LastRoutedAt = %v, want %v", router.LastRoutedAt, lastAt)
	}

	vault := env.Data[1]
	if vault.Kind != "aggregator-vault" {
		t.Errorf("vault Kind = %q, want aggregator-vault", vault.Kind)
	}
	if vault.RoutedTrades24h != 0 || vault.RoutedVolume24hUSD != nil || vault.LastRoutedAt != nil {
		t.Errorf("vault stats = (%d, %v, %v), want zero/null", vault.RoutedTrades24h, vault.RoutedVolume24hUSD, vault.LastRoutedAt)
	}

	// The raw JSON must carry explicit nulls (not omit the keys) so
	// clients can distinguish "no valuation" without probing.
	if !strings.Contains(body, `"routed_volume_24h_usd":null`) {
		t.Errorf("vault volume should serialize as explicit null; body=%s", body)
	}

	// Window contract: since ≈ now-24h.
	wantSince := time.Now().UTC().Add(-24 * time.Hour)
	if d := reader.since.Sub(wantSince); d < -time.Minute || d > time.Minute {
		t.Errorf("rollup since = %v, want ≈ %v", reader.since, wantSince)
	}
}

// TestAggregators_NotesHonestDegrade pins the ROADMAP #11/#29 coverage
// caveats: a router row always carries a "these cases can't be told
// apart" note, an auto_discovered (evidence-only, unverified) router
// row carries the stronger "not yet attributed" note, and a vault row
// carries no note at all (Notes is a router-kind-only concept).
func TestAggregators_NotesHonestDegrade(t *testing.T) {
	reader := &stubAggregatorsReader{
		rows: []timescale.AggregatorRollupRow{
			{
				ContractID:   "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
				Name:         "soroswap-router",
				Kind:         "router",
				ProtocolSlug: "soroswap",
			},
			{
				ContractID:     "CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH",
				Name:           "soroswap-router-aggregator-exec",
				Kind:           "router",
				ProtocolSlug:   "unattributed",
				AutoDiscovered: true,
			},
			{
				ContractID:   "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
				Name:         "defindex-vault-usdc-autocompound",
				Kind:         "aggregator-vault",
				ProtocolSlug: "defindex",
			},
		},
	}
	srv := v1.New(v1.Options{Aggregators: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/aggregators")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.AggregatorRow `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(env.Data) != 3 {
		t.Fatalf("len(data) = %d, want 3", len(env.Data))
	}

	byName := map[string]v1.AggregatorRow{}
	for _, row := range env.Data {
		byName[row.Name] = row
	}

	router := byName["soroswap-router"]
	if len(router.Notes) != 1 {
		t.Errorf("soroswap-router Notes = %v, want exactly 1 (the shared-bucket caveat)", router.Notes)
	}

	exec := byName["soroswap-router-aggregator-exec"]
	if len(exec.Notes) != 2 {
		t.Errorf("aggregator-exec Notes = %v, want exactly 2 (unverified + not-yet-attributed)", exec.Notes)
	}

	vault := byName["defindex-vault-usdc-autocompound"]
	if vault.Notes != nil {
		t.Errorf("vault Notes = %v, want nil (Notes is router-kind only)", vault.Notes)
	}
}

// TestAggregators_500OnReaderError pins the upstream-failure path.
func TestAggregators_500OnReaderError(t *testing.T) {
	srv := v1.New(v1.Options{Aggregators: &stubAggregatorsReader{err: errors.New("pg down")}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/aggregators")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}
