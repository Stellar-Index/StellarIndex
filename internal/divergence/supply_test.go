package divergence

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
)

// The live 2026-07-07 Stellar Dashboard /lumens body (see
// docs/methodology/xlm-circulating-supply.md). Note it does NOT carry
// an explicit circulatingSupply field — the reference derives it as
// total − mandate − upgradeReserve − feePool = 34,066,264,765.22 XLM.
const dashboardLumensBody = `{
  "updatedAt": "2026-07-07T01:06:15.982Z",
  "originalSupply": "100000000000",
  "inflationLumens": "5443902087.3472865",
  "burnedLumens": "55442115247.4347092",
  "totalSupply": "50001786839.9125773",
  "upgradeReserve": "258885847.5135956",
  "feePool": "10099167.2831740",
  "sdfMandate": "15666537059.8967597",
  "_details": "https://www.stellar.org/developers/guides/lumen-supply-metrics.html"
}`

const wantDashboardCirculating = 34_066_264_765.22 // total − mandate − upgradeReserve − feePool

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustParseAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

// ─── Stellar Dashboard reference ─────────────────────────────────────

func TestStellarDashboardReference_DerivesCirculating(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, dashboardLumensBody)
	}))
	defer srv.Close()

	// Pin the clock just after the fixture's updatedAt (2026-07-07T01:06:15Z)
	// so the CS-089 staleness gate reads the historical snapshot as fresh
	// rather than depending on wall time.
	fixedNow := time.Date(2026, 7, 7, 1, 30, 0, 0, time.UTC)
	ref := NewStellarDashboardReference(StellarDashboardOptions{
		BaseURL: srv.URL,
		NowFn:   func() time.Time { return fixedNow },
	})
	got, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
	if err != nil {
		t.Fatalf("LookupCirculatingSupply: %v", err)
	}
	if gotPath != "/lumens" {
		t.Errorf("path = %q, want /lumens", gotPath)
	}
	if math.Abs(got-wantDashboardCirculating) > 1.0 {
		t.Errorf("circulating = %.2f, want ~%.2f", got, wantDashboardCirculating)
	}
	if ref.Name() != "stellar-dashboard" {
		t.Errorf("Name() = %q", ref.Name())
	}
}

func TestStellarDashboardReference_PrefersExplicitField(t *testing.T) {
	const body = `{"totalSupply":"50000000000","sdfMandate":"15000000000","upgradeReserve":"250000000","feePool":"10000000","circulatingSupply":"34740000000"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	ref := NewStellarDashboardReference(StellarDashboardOptions{BaseURL: srv.URL})
	got, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
	if err != nil {
		t.Fatalf("LookupCirculatingSupply: %v", err)
	}
	// Explicit circulatingSupply (34.74B) must win over the component
	// formula (which would give 34.74B here too — pick a value that
	// differs to prove the field is used).
	if math.Abs(got-34_740_000_000) > 1.0 {
		t.Errorf("circulating = %.2f, want 34740000000 (explicit field)", got)
	}
}

// TestSupplyReferences_StalenessGate (MNY-22 / CS-089) — a FROZEN
// supply upstream must read as "reference unavailable", never drive a
// supply divergence. Both references expose an upstream publication
// time (`/lumens`.updatedAt, `/coins/{id}`.market_data.last_updated);
// a figure older than the 24h default is excluded.
func TestSupplyReferences_StalenessGate(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Hour).Format(time.RFC3339)  // within 24h
	stale := now.Add(-48 * time.Hour).Format(time.RFC3339) // 2× the 24h default

	t.Run("dashboard_fresh_served", func(t *testing.T) {
		body := `{"updatedAt":"` + fresh + `","totalSupply":"50000000000","sdfMandate":"15000000000","upgradeReserve":"250000000","feePool":"10000000"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		ref := NewStellarDashboardReference(StellarDashboardOptions{BaseURL: srv.URL, NowFn: func() time.Time { return now }})
		if _, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset()); err != nil {
			t.Fatalf("fresh dashboard: %v", err)
		}
	})

	t.Run("dashboard_stale_excluded", func(t *testing.T) {
		body := `{"updatedAt":"` + stale + `","totalSupply":"50000000000","sdfMandate":"15000000000","upgradeReserve":"250000000","feePool":"10000000"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		ref := NewStellarDashboardReference(StellarDashboardOptions{BaseURL: srv.URL, NowFn: func() time.Time { return now }})
		_, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
		if !errors.Is(err, ErrSupplyUnavailable) {
			t.Fatalf("stale dashboard err = %v, want ErrSupplyUnavailable", err)
		}
	})

	t.Run("coingecko_stale_excluded", func(t *testing.T) {
		body := `{"market_data":{"circulating_supply":34066264765.0,"last_updated":"` + stale + `"}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		ref := NewCoinGeckoSupplyReference(CoinGeckoSupplyOptions{BaseURL: srv.URL, NowFn: func() time.Time { return now }})
		_, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
		if !errors.Is(err, ErrSupplyUnavailable) {
			t.Fatalf("stale coingecko err = %v, want ErrSupplyUnavailable", err)
		}
	})
}

func TestStellarDashboardReference_NonXLMUnsupported(t *testing.T) {
	ref := NewStellarDashboardReference(StellarDashboardOptions{BaseURL: "http://unused.invalid"})
	_, err := ref.LookupCirculatingSupply(context.Background(),
		mustParseAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"))
	if !errors.Is(err, ErrAssetUnsupported) {
		t.Fatalf("err = %v, want ErrAssetUnsupported", err)
	}
}

func TestStellarDashboardReference_RateLimitedDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ref := NewStellarDashboardReference(StellarDashboardOptions{BaseURL: srv.URL})
	_, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
	if !errors.Is(err, ErrSupplyUnavailable) {
		t.Fatalf("err = %v, want ErrSupplyUnavailable on HTTP 429", err)
	}
}

// ─── CoinGecko supply reference ──────────────────────────────────────

func TestCoinGeckoSupplyReference_ParsesMarketData(t *testing.T) {
	const body = `{"id":"stellar","market_data":{"circulating_supply":34066264765.0,"total_supply":50001786839.0}}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	ref := NewCoinGeckoSupplyReference(CoinGeckoSupplyOptions{BaseURL: srv.URL})
	got, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset())
	if err != nil {
		t.Fatalf("LookupCirculatingSupply: %v", err)
	}
	if !strings.HasPrefix(gotPath, "/coins/stellar") {
		t.Errorf("path = %q, want /coins/stellar…", gotPath)
	}
	if math.Abs(got-34_066_264_765.0) > 1.0 {
		t.Errorf("circulating = %.2f, want 34066264765", got)
	}
}

func TestCoinGeckoSupplyReference_UnsupportedAsset(t *testing.T) {
	ref := NewCoinGeckoSupplyReference(CoinGeckoSupplyOptions{BaseURL: "http://unused.invalid"})
	_, err := ref.LookupCirculatingSupply(context.Background(), mustParseAsset(t, "crypto:DOGE"))
	if !errors.Is(err, ErrAssetUnsupported) {
		t.Fatalf("err = %v, want ErrAssetUnsupported for asset absent from idMap", err)
	}
}

// ─── scaleServedSupply ───────────────────────────────────────────────

func TestScaleServedSupply(t *testing.T) {
	tests := []struct {
		name     string
		raw      *big.Int
		decimals int
		want     float64
		wantOK   bool
	}{
		{"xlm stroops → tokens", bigIntStr(t, "340763839035000000"), 7, 34_076_383_903.5, true},
		{"nil", nil, 7, 0, false},
		{"negative", big.NewInt(-1), 7, 0, false},
		{"zero", big.NewInt(0), 7, 0, false},
		{"decimals out of range", big.NewInt(1), 39, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := scaleServedSupply(tt.raw, tt.decimals)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > 1.0 {
				t.Errorf("got = %.2f, want %.2f", got, tt.want)
			}
		})
	}
}

// ─── SupplyService.Tick ──────────────────────────────────────────────

// fakeReader implements ServedSupplyReader.
type fakeReader struct {
	circulating *big.Int
	err         error
}

func (f fakeReader) LatestCirculatingSupply(context.Context, string) (ServedSupply, error) {
	if f.err != nil {
		return ServedSupply{}, f.err
	}
	return ServedSupply{Circulating: f.circulating, LedgerSequence: 42, ObservedAt: time.Now()}, nil
}

// fakeReference implements SupplyReference with a canned value/error.
type fakeReference struct {
	name string
	val  float64
	err  error
}

func (f fakeReference) Name() string { return f.name }
func (f fakeReference) LookupCirculatingSupply(context.Context, canonical.Asset) (float64, error) {
	return f.val, f.err
}

// captureEmitter records everything the service emits.
type captureEmitter struct {
	ratios    map[string]float64 // "asset|reference" → ratio
	outcomes  []SupplyOutcomeKind
	durations []SupplyOutcomeKind
}

func newCaptureEmitter() *captureEmitter {
	return &captureEmitter{ratios: map[string]float64{}}
}

func (c *captureEmitter) Ratio(asset, reference string, ratio float64) {
	c.ratios[asset+"|"+reference] = ratio
}
func (c *captureEmitter) Outcome(kind SupplyOutcomeKind) { c.outcomes = append(c.outcomes, kind) }
func (c *captureEmitter) Duration(kind SupplyOutcomeKind, _ float64) {
	c.durations = append(c.durations, kind)
}

// xlmStroops for 34,076,383,903.5 XLM — our served figure, ~0.03% above
// the Dashboard's 34.066B (the Fee-Pool noise floor).
func servedXLM(t *testing.T) *big.Int { return bigIntStr(t, "340763839035000000") }

func newTestService(t *testing.T, reader ServedSupplyReader, emitter SupplyEmitter, refs ...SupplyReference) *SupplyService {
	t.Helper()
	svc, err := NewSupplyService(SupplyServiceOptions{
		References: refs,
		Reader:     reader,
		Emitter:    emitter,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewSupplyService: %v", err)
	}
	return svc
}

func TestSupplyService_OKWithinNoiseFloor(t *testing.T) {
	em := newCaptureEmitter()
	svc := newTestService(t,
		fakeReader{circulating: servedXLM(t)}, em,
		fakeReference{name: "stellar-dashboard", val: wantDashboardCirculating})
	svc.Tick(context.Background())

	if len(em.outcomes) != 1 || em.outcomes[0] != SupplyOutcomeOK {
		t.Fatalf("outcomes = %v, want [ok] (0.03%% is under the 1%% threshold)", em.outcomes)
	}
	got := em.ratios["native|stellar-dashboard"]
	if got <= 0 || got > 0.01 {
		t.Errorf("ratio = %g, want a small positive value under the 1%% threshold", got)
	}
	if len(em.durations) != 1 || em.durations[0] != SupplyOutcomeOK {
		t.Errorf("durations = %v, want [ok]", em.durations)
	}
}

func TestSupplyService_DivergentOverThreshold(t *testing.T) {
	em := newCaptureEmitter()
	// Reference reports ~29.6B — the stale figure the manual re-flag
	// keeps comparing against; ~13% below our 34.08B → fires.
	svc := newTestService(t,
		fakeReader{circulating: servedXLM(t)}, em,
		fakeReference{name: "stellar-dashboard", val: 29_600_000_000})
	svc.Tick(context.Background())

	if len(em.outcomes) != 1 || em.outcomes[0] != SupplyOutcomeDivergent {
		t.Fatalf("outcomes = %v, want [divergent]", em.outcomes)
	}
	if em.ratios["native|stellar-dashboard"] <= 0.01 {
		t.Errorf("ratio = %g, want > 0.01", em.ratios["native|stellar-dashboard"])
	}
}

func TestSupplyService_NoReferenceGracefulDegrade(t *testing.T) {
	em := newCaptureEmitter()
	// Every reference is dark (429 → ErrSupplyUnavailable) or doesn't
	// publish the asset (ErrAssetUnsupported). Must NOT be a divergence.
	svc := newTestService(t,
		fakeReader{circulating: servedXLM(t)}, em,
		fakeReference{name: "coingecko", err: ErrSupplyUnavailable},
		fakeReference{name: "stellar-dashboard", err: ErrAssetUnsupported})
	svc.Tick(context.Background())

	if len(em.outcomes) != 1 || em.outcomes[0] != SupplyOutcomeNoReference {
		t.Fatalf("outcomes = %v, want [no_reference]", em.outcomes)
	}
	if len(em.ratios) != 0 {
		t.Errorf("ratios = %v, want none (no reference responded)", em.ratios)
	}
}

func TestSupplyService_RefreshErrorOnBootstrap(t *testing.T) {
	em := newCaptureEmitter()
	svc := newTestService(t,
		fakeReader{err: ErrNoServedSupply}, em,
		fakeReference{name: "stellar-dashboard", val: wantDashboardCirculating})
	svc.Tick(context.Background())

	if len(em.outcomes) != 1 || em.outcomes[0] != SupplyOutcomeRefreshError {
		t.Fatalf("outcomes = %v, want [refresh_error]", em.outcomes)
	}
	if len(em.ratios) != 0 {
		t.Errorf("ratios = %v, want none (no served figure to compare)", em.ratios)
	}
}

func TestNewSupplyService_RejectsNoReference(t *testing.T) {
	_, err := NewSupplyService(SupplyServiceOptions{
		Reader:  fakeReader{circulating: big.NewInt(1)},
		Emitter: newCaptureEmitter(),
		Logger:  discardLogger(),
	})
	if err == nil {
		t.Fatal("want error when no references configured")
	}
}

// obsEmitter forwards to the real obs collectors, mirroring the
// aggregator's obsSupplyDivergenceEmitter — used to prove the duration
// histogram actually advances (the wave-100 obstest pattern).
type obsEmitter struct{}

func (obsEmitter) Ratio(asset, reference string, ratio float64) {
	obs.SupplyDivergenceRatio.WithLabelValues(asset, reference).Set(ratio)
}

func (obsEmitter) Outcome(kind SupplyOutcomeKind) {
	obs.SupplyDivergenceTotal.WithLabelValues(string(kind)).Inc()
}

func (obsEmitter) Duration(kind SupplyOutcomeKind, seconds float64) {
	obs.SupplyDivergenceDurationSeconds.WithLabelValues(string(kind)).Observe(seconds)
}

func TestSupplyService_EmitsDurationHistogram(t *testing.T) {
	svc := newTestService(t,
		fakeReader{circulating: servedXLM(t)}, obsEmitter{},
		fakeReference{name: "stellar-dashboard", val: wantDashboardCirculating})

	before := obstest.HistogramSampleCount(t, obs.SupplyDivergenceDurationSeconds, "outcome", "ok")
	svc.Tick(context.Background())
	after := obstest.HistogramSampleCount(t, obs.SupplyDivergenceDurationSeconds, "outcome", "ok")
	if after <= before {
		t.Errorf("duration histogram did not advance for outcome=ok (before=%d after=%d)", before, after)
	}
}

func bigIntStr(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big.Int literal %q", s)
	}
	return n
}
