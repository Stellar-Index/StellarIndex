package forex

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ECB's daily file, trimmed. Rates read "1 EUR = X <currency>".
const ecbDailyXML = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <gesmes:subject>Reference rates</gesmes:subject>
  <Cube>
    <Cube time="2026-08-27">
      <Cube currency="USD" rate="1.2500"/>
      <Cube currency="GBP" rate="0.8500"/>
      <Cube currency="JPY" rate="160.00"/>
    </Cube>
  </Cube>
</gesmes:Envelope>`

func ecbServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestWorker builds a Worker whose PRIMARY deterministically fails,
// by pointing the real client at an endpoint that always 500s. That is
// the state the fallback exists for, and it avoids mocking massive's
// wire format just to exercise the failover.
func newTestWorker(t *testing.T) *Worker {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	return &Worker{
		client:       NewClient("test-key").WithBase(dead.URL),
		cache:        NewCache(),
		logger:       discardLogger(),
		guards:       map[string]*rateGuard{},
		activeSource: fxSource,
	}
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// The rebase is the whole point of the adapter: ECB quotes against EUR,
// the worker's contract is units-per-USD. Getting this inverted would
// mis-price every fiat pair simultaneously, so pin the arithmetic.
func TestECBProvider_RebasesEURQuotesOntoUSD(t *testing.T) {
	srv := ecbServer(t, ecbDailyXML, http.StatusOK)
	rates, publishedAt, err := ECBProvider{Endpoint: srv.URL}.LatestUSDRates(context.Background())
	if err != nil {
		t.Fatalf("LatestUSDRates: %v", err)
	}

	// 1 EUR = 1.25 USD, so 1 USD = 0.8 EUR.
	if !closeTo(rates["EUR"], 0.8) {
		t.Errorf("EUR = %v, want 0.8 (1/1.25)", rates["EUR"])
	}
	// GBP: 0.85 per EUR / 1.25 USD per EUR = 0.68 per USD.
	if !closeTo(rates["GBP"], 0.68) {
		t.Errorf("GBP = %v, want 0.68", rates["GBP"])
	}
	// JPY: 160 / 1.25 = 128.
	if !closeTo(rates["JPY"], 128) {
		t.Errorf("JPY = %v, want 128", rates["JPY"])
	}
	// USD against itself is 1, not the EUR rate.
	if !closeTo(rates["USD"], 1) {
		t.Errorf("USD = %v, want 1", rates["USD"])
	}
	if publishedAt.Format("2006-01-02") != "2026-08-27" {
		t.Errorf("publishedAt = %v, want the cube's date", publishedAt)
	}
}

// Without a USD cube there is no anchor to rebase on. Failing is the
// only safe answer — inventing one would silently mis-scale everything.
func TestECBProvider_NoUSDRateIsAnError(t *testing.T) {
	const noUSD = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <Cube><Cube time="2026-08-27"><Cube currency="GBP" rate="0.8500"/></Cube></Cube>
</gesmes:Envelope>`
	srv := ecbServer(t, noUSD, http.StatusOK)
	if _, _, err := (ECBProvider{Endpoint: srv.URL}).LatestUSDRates(context.Background()); !errors.Is(err, ErrNoUSDAnchor) {
		t.Fatalf("err = %v, want ErrNoUSDAnchor", err)
	}
}

func TestECBProvider_UpstreamErrorPropagates(t *testing.T) {
	srv := ecbServer(t, "upstream exploded", http.StatusInternalServerError)
	if _, _, err := (ECBProvider{Endpoint: srv.URL}).LatestUSDRates(context.Background()); err == nil {
		t.Fatal("want an error on HTTP 500, got nil")
	}
}

// stubProvider is a RateProvider whose behaviour the test dictates.
type stubProvider struct {
	name  string
	rates map[string]float64
	err   error
	calls int
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) LatestUSDRates(context.Context) (map[string]float64, time.Time, error) {
	s.calls++
	if s.err != nil {
		return nil, time.Time{}, s.err
	}
	return s.rates, time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), nil
}

// The core failover: a failing primary hands off to the standby, and the
// SOURCE LABEL follows — so stellarindex_external_fx_last_quote_unix
// shows which feed is actually live instead of always claiming massive.
func TestFetchRates_FallsBackAndReportsItsSource(t *testing.T) {
	fb := &stubProvider{name: "ecb", rates: map[string]float64{"EUR": 0.8}}
	w := newTestWorker(t)
	w.fallbacks = []RateProvider{fb}

	rates, _, source, err := w.fetchRates(context.Background())
	if err != nil {
		t.Fatalf("fetchRates: %v", err)
	}
	if source != "ecb" {
		t.Fatalf("source = %q, want ecb", source)
	}
	if !closeTo(rates["EUR"], 0.8) {
		t.Errorf("EUR = %v, want the fallback's rate", rates["EUR"])
	}
	if fb.calls != 1 {
		t.Errorf("fallback calls = %d, want 1", fb.calls)
	}
}

// The degraded path: when every source fails, the primary's error is
// what surfaces — that is the one an operator needs to act on.
func TestFetchRates_AllSourcesFailReturnsPrimaryError(t *testing.T) {
	fb := &stubProvider{name: "ecb", err: errors.New("ecb down")}
	w := newTestWorker(t)
	w.fallbacks = []RateProvider{fb}

	_, _, _, err := w.fetchRates(context.Background())
	if err == nil {
		t.Fatal("want an error when every source fails")
	}
	if fb.calls != 1 {
		t.Errorf("fallback calls = %d, want 1", fb.calls)
	}
}

// Cancellation is shutdown, not a source outage: it must short-circuit
// rather than burn through every fallback on the way out.
func TestFetchRates_CancelledContextSkipsFallbacks(t *testing.T) {
	fb := &stubProvider{name: "ecb", rates: map[string]float64{"EUR": 0.8}}
	w := newTestWorker(t)
	w.fallbacks = []RateProvider{fb}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := w.fetchRates(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fb.calls != 0 {
		t.Errorf("fallback calls = %d, want 0 — cancellation must not fan out", fb.calls)
	}
}

func TestWithFallbacks_RegistersInOrder(t *testing.T) {
	a := &stubProvider{name: "a"}
	b := &stubProvider{name: "b"}
	w := newTestWorker(t).WithFallbacks(a, b)
	if len(w.fallbacks) != 2 || w.fallbacks[0].Name() != "a" || w.fallbacks[1].Name() != "b" {
		t.Fatalf("fallbacks = %v, want [a b] in order", w.fallbacks)
	}
}

// The default source must remain the primary so a worker with no
// fallbacks stamps exactly what it did before this change.
func TestWorker_DefaultActiveSourceIsPrimary(t *testing.T) {
	if got := newTestWorker(t).activeSource; got != fxSource {
		t.Fatalf("activeSource = %q, want %q", got, fxSource)
	}
}

// A zero-value Worker (struct literal, as several sibling tests build)
// must stamp the PRIMARY, not an empty string. Regression guard: routing
// the label through w.activeSource directly broke
// TestPersistSnapshot_stampsLivenessGaugeOnCommittedWrite and
// TestAcceptRate_HistoryConflictVetoReclassifiesWhenStuck, because both
// build &Worker{...} and would have written source="".
func TestSourceLabel_ZeroValueWorkerFallsBackToPrimary(t *testing.T) {
	if got := (&Worker{}).sourceLabel(); got != fxSource {
		t.Fatalf("sourceLabel() = %q, want %q", got, fxSource)
	}
	if got := (&Worker{activeSource: "ecb"}).sourceLabel(); got != "ecb" {
		t.Fatalf("sourceLabel() = %q, want ecb", got)
	}
}
