package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/RatesEngine/rates-engine/internal/api/v1"
	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// stubPriceReader implements v1.PriceReader.
type stubPriceReader struct {
	// Lookup keyed on "<base>/<quote>".
	snapshots map[string]v1.PriceSnapshot
	stale     map[string]bool
	sources   map[string][]string
	// recent is the per-pair history returned by RecentClosedSnapshots.
	// Same key shape as `snapshots`. Empty/missing yields []v1.PriceSnapshot{}
	// (no observations) — matches the production reader's contract.
	recent map[string][]v1.PriceSnapshot
	err    error
}

func (r *stubPriceReader) LatestPrice(_ context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if r.err != nil {
		return v1.PriceSnapshot{}, nil, false, r.err
	}
	key := a.String() + "/" + q.String()
	snap, ok := r.snapshots[key]
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return snap, r.sources[key], r.stale[key], nil
}

func (r *stubPriceReader) RecentClosedSnapshots(_ context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	if r.err != nil {
		return nil, r.err
	}
	key := a.String() + "/" + q.String()
	rows, ok := r.recent[key]
	if !ok {
		return []v1.PriceSnapshot{}, nil
	}
	if n < len(rows) {
		rows = rows[:n]
	}
	return rows, nil
}

func TestPrice_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "price-unavailable") {
		t.Errorf("error type missing: %s", body)
	}
}

func TestPrice_MissingAssetParam(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPrice_InvalidAssetReturns400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=garbage-format")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPrice_IdentityPairReturns400(t *testing.T) {
	// XLM / XLM is always 1 — reject as a bad request.
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "identity-price") {
		t.Errorf("error type missing: %s", body)
	}
}

func TestPrice_HappyPath(t *testing.T) {
	snap := v1.PriceSnapshot{
		AssetID:    "native",
		Quote:      "fiat:USD",
		Price:      "0.1242",
		PriceType:  "last_trade",
		ObservedAt: time.Unix(1745000000, 0).UTC(),
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap},
		sources:   map[string][]string{"native/fiat:USD": {"sdex"}},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Envelope shape: {"data":{"price":"0.1242",...},"flags":{"stale":false,...},"sources":["sdex"]}
	for _, s := range []string{
		`"price":"0.1242"`,
		`"price_type":"last_trade"`,
		`"stale":false`,
		`"sources":["sdex"]`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q: %s", s, body)
		}
	}
}

func TestPrice_StaleFlagSet(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.1242", PriceType: "last_trade"},
		},
		stale: map[string]bool{"native/fiat:USD": true},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("stale flag not set: %s", body)
	}
}

// stubDivergenceLooker is a minimal v1.DivergenceLooker for tests.
// firing controls what DivergenceFiringFor returns; err is the
// surfaced error (nil = clean response).
type stubDivergenceLooker struct {
	firing bool
	err    error
	calls  int
}

func (s *stubDivergenceLooker) DivergenceFiringFor(_ context.Context, _ canonical.Asset) (bool, error) {
	s.calls++
	return s.firing, s.err
}

// TestPrice_DivergenceFires — when the lookup says the warning is
// firing for this asset, flags.divergence_warning is true.
func TestPrice_DivergenceFires(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.07", PriceType: "vwap"},
		},
	}
	div := &stubDivergenceLooker{firing: true}
	srv := v1.New(v1.Options{Prices: reader, Divergence: div})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"divergence_warning":true`) {
		t.Errorf("divergence_warning not set: %s", body)
	}
	if div.calls != 1 {
		t.Errorf("divergence lookup calls = %d, want 1", div.calls)
	}
}

// TestPrice_DivergenceClean — when the lookup says no warning,
// flags.divergence_warning stays false.
func TestPrice_DivergenceClean(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.07", PriceType: "vwap"},
		},
	}
	div := &stubDivergenceLooker{firing: false}
	srv := v1.New(v1.Options{Prices: reader, Divergence: div})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"divergence_warning":false`) {
		t.Errorf("divergence_warning expected false: %s", body)
	}
}

// TestPrice_DivergenceErrorIsBestEffort — a divergence lookup error
// must NOT cause the price endpoint to fail. Flag stays at default
// (false); the price still flows.
func TestPrice_DivergenceErrorIsBestEffort(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.07", PriceType: "vwap"},
		},
	}
	div := &stubDivergenceLooker{err: errors.New("redis exploded")}
	srv := v1.New(v1.Options{Prices: reader, Divergence: div})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — divergence error must NOT fail the price call", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"divergence_warning":false`) {
		t.Errorf("flag expected default false on lookup error: %s", body)
	}
}

// TestPrice_NoDivergenceLookerLeavesFlagFalse — when no
// DivergenceLooker is wired (typical pre-launch / no-Redis state),
// the flag never fires and no lookup is attempted.
func TestPrice_NoDivergenceLookerLeavesFlagFalse(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.07", PriceType: "vwap"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"divergence_warning":false`) {
		t.Errorf("flag should default to false without a looker: %s", body)
	}
}

func TestPrice_DefaultQuoteIsUSD(t *testing.T) {
	// Omit quote param — handler defaults to fiat:USD.
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.12"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestPrice_NotFoundReturns404(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPrice_InternalErrorReturns500(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: errors.New("db timeout")}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	// Body must NOT leak the underlying error message.
	body, _ := readAll(resp)
	if strings.Contains(body, "db timeout") {
		t.Errorf("internal error leaked to client: %s", body)
	}
}

// ─── LastTradeToSnapshot ─────────────────────────────────────────

// TestVWAP1mToSnapshot is the CAGG-served counterpart to
// TestLastTradeToSnapshot. Confirms the snapshot's ObservedAt
// reflects the END of the 1-minute window (not its start) and the
// VWAP string is passed through unchanged from the prices_1m row.
func TestVWAP1mToSnapshot(t *testing.T) {
	bucketStart := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	got := v1.VWAP1mToSnapshot("native", "fiat:USD", "0.123456789", bucketStart)

	if got.AssetID != "native" {
		t.Errorf("AssetID = %q, want native", got.AssetID)
	}
	if got.Quote != "fiat:USD" {
		t.Errorf("Quote = %q, want fiat:USD", got.Quote)
	}
	if got.Price != "0.123456789" {
		t.Errorf("Price = %q, want pass-through of NUMERIC text 0.123456789", got.Price)
	}
	if got.PriceType != "vwap" {
		t.Errorf("PriceType = %q, want vwap (CAGG-served path)", got.PriceType)
	}
	if got.WindowSeconds != 60 {
		t.Errorf("WindowSeconds = %d, want 60 (1-minute CAGG)", got.WindowSeconds)
	}
	wantObserved := bucketStart.Add(60 * time.Second)
	if !got.ObservedAt.Equal(wantObserved) {
		t.Errorf("ObservedAt = %v, want %v (END of window, not start)", got.ObservedAt, wantObserved)
	}
}

func TestLastTradeToSnapshot(t *testing.T) {
	usdc, _ := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	pair, _ := canonical.NewPair(canonical.NativeAsset(), usdc)

	// 100 XLM @ 12.42 USDC = 1e9 base stroops, 12_420_000 quote stroops.
	// Ratio = 12_420_000 / 1_000_000_000 = 0.01242 in stroop-units.
	// At decimals=7 we get a str with 7 fractional digits.
	tr := canonical.Trade{
		Source:      "sdex",
		Ledger:      52_430_001,
		TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:     0,
		Timestamp:   time.Unix(1745000000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420_000)),
	}

	snap := v1.LastTradeToSnapshot(tr, 7)
	if snap.AssetID != "native" {
		t.Errorf("asset = %q", snap.AssetID)
	}
	if snap.PriceType != "last_trade" {
		t.Errorf("price_type = %q", snap.PriceType)
	}
	// 12_420_000 / 1_000_000_000 scaled to 7 decimals =
	// (12_420_000 * 10^7) / 1_000_000_000 = 12_420_000 / 100 = 124_200
	// → "0.0124200"
	if snap.Price != "0.0124200" {
		t.Errorf("price = %q, want 0.0124200", snap.Price)
	}
	if snap.ObservedAt != tr.Timestamp {
		t.Errorf("timestamp lost")
	}
}

func TestLastTradeToSnapshot_zeroDecimals(t *testing.T) {
	tr := canonical.Trade{
		Source: "sdex", Ledger: 1, TxHash: "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex: 0, Timestamp: time.Now(),
		Pair:        mustPair(canonical.NativeAsset(), mustClassicTest("USDC", testUSDCIssuer)),
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420)),
	}
	snap := v1.LastTradeToSnapshot(tr, 0)
	if snap.Price != "12" { // 12420 / 1000 = 12 with no decimals
		t.Errorf("price = %q, want 12", snap.Price)
	}
}

// helper
func mustPair(base, quote canonical.Asset) canonical.Pair {
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		panic(err)
	}
	return p
}

func mustClassicTest(code, issuer string) canonical.Asset {
	a, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		panic(err)
	}
	return a
}
