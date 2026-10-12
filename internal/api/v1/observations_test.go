package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkObservationTrade builds a native/fiat:USD trade with a specific
// source and timestamp — fixture builder for observations tests.
func mkObservationTrade(source string, ts time.Time, base, quote int64) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source:      source,
		Ledger:      uint32(ts.Unix() % 1_000_000),
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

func TestObservations_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestObservations_RejectsTierParams — ADR-0018 URL discipline:
// granularity (closed-bucket) and window_seconds (tip) must NOT be
// accepted on /v1/observations.
func TestObservations_RejectsTierParams(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	for _, q := range []string{"granularity=1m", "window_seconds=5"} {
		resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD&"+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q → %d, want 400", q, resp.StatusCode)
			continue
		}
		body, _ := readAll(resp)
		if !strings.Contains(body, "invalid-observations-param") {
			t.Errorf("query %q error type missing: %s", q, body)
		}
	}
}

func TestObservations_MissingAsset_Returns400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestObservations_IdentityPair_Returns400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestObservations_UnknownSource400 — `?source=` naming a source absent from the in-memory
// `external.Registry` returns 400, not an empty page, so a typo cannot look like "this source
// has no trades for the pair". /v1/markets has the same fail-fast guard.
func TestObservations_UnknownSource400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD&source=fake-venue")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/unknown-source" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestObservations_HappyPath_AllSources — every source's most-recent
// observation flows through to the response, ordered as the reader
// returned them. Single-source flag false (multiple sources).
func TestObservations_HappyPath_AllSources(t *testing.T) {
	now := time.Unix(1745000000, 0).UTC()
	hist := &stubHistoryReader{
		observations: []canonical.Trade{
			mkObservationTrade("soroswap", now.Add(-2*time.Second), 1, 100),
			mkObservationTrade("phoenix", now.Add(-5*time.Second), 2, 250),
			mkObservationTrade("sdex", now.Add(-1*time.Second), 1, 105),
		},
	}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"source":"soroswap"`,
		`"source":"phoenix"`,
		`"source":"sdex"`,
		`"stale":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	// Flags{SingleSource:false} ships with `omitempty`, so the
	// negative assertion is "the flag must NOT be present" (its
	// presence would mean single_source=true).
	if strings.Contains(body, `"single_source":true`) {
		t.Errorf("single_source should not be set with multiple sources: %s", body)
	}
}

// A stored one-side-zero SDEX fill is served with "price": null, never
// "0": the key stays present and the priceable neighbour is unaffected.
func TestObservations_ZeroLegRowRendersNullPrice(t *testing.T) {
	now := time.Unix(1745000000, 0).UTC()
	hist := &stubHistoryReader{
		observations: []canonical.Trade{
			mkObservationTrade("sdex", now.Add(-1*time.Second), 5_000_000_000, 0),
			mkObservationTrade("soroswap", now.Add(-2*time.Second), 1, 100),
		},
	}
	tsv := startHTTPTest(t, v1.New(v1.Options{History: hist}).Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"quote_amount":"0","price":null`,
		`"price":"100.0000000000"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"price":"0"`) {
		t.Errorf("zero-leg row rendered a zero price: %s", body)
	}
}

// TestObservations_SourceFilter — ?source=phoenix returns only that
// source's row. Reader receives the filter so the SQL-side narrowing
// happens (tests that the handler forwards it).
func TestObservations_SourceFilter(t *testing.T) {
	now := time.Unix(1745000000, 0).UTC()
	hist := &stubHistoryReader{
		observations: []canonical.Trade{
			mkObservationTrade("soroswap", now.Add(-2*time.Second), 1, 100),
			mkObservationTrade("phoenix", now.Add(-5*time.Second), 2, 250),
		},
	}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&source=phoenix")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"source":"phoenix"`) {
		t.Errorf("expected phoenix row: %s", body)
	}
	if strings.Contains(body, `"source":"soroswap"`) {
		t.Errorf("source filter leaked soroswap: %s", body)
	}
	if !strings.Contains(body, `"single_source":true`) {
		t.Errorf("single_source not set with one row: %s", body)
	}
	if hist.lastCall.sourceFilter != "phoenix" {
		t.Errorf("reader sourceFilter = %q, want phoenix", hist.lastCall.sourceFilter)
	}
}

// TestObservations_AggregateLatest — collapse to the single newest
// trade across sources. The wire shape stays an array (length 1).
func TestObservations_AggregateLatest(t *testing.T) {
	base := time.Unix(1745000000, 0).UTC()
	hist := &stubHistoryReader{
		observations: []canonical.Trade{
			mkObservationTrade("soroswap", base.Add(-10*time.Second), 1, 100),
			// Newest — should win the collapse.
			mkObservationTrade("phoenix", base.Add(-1*time.Second), 2, 250),
			mkObservationTrade("sdex", base.Add(-5*time.Second), 1, 105),
		},
	}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&aggregate=latest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"source":"phoenix"`) {
		t.Errorf("collapse didn't pick newest source phoenix: %s", body)
	}
	if strings.Contains(body, `"source":"soroswap"`) || strings.Contains(body, `"source":"sdex"`) {
		t.Errorf("collapse leaked older sources: %s", body)
	}
}

// TestObservations_AggregateInvalid — anything other than "latest"
// or empty is a 400 — the handler rejects unknowns rather than
// silently ignoring (keeps the surface honest).
func TestObservations_AggregateInvalid(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD&aggregate=median")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestObservations_EmptyArrayWhenNoData: a pair with no trades returns 200 and an empty array, not 404; "no
// observations" is a legitimate steady state for divergence-detection pollers.
func TestObservations_EmptyArrayWhenNoData(t *testing.T) {
	hist := &stubHistoryReader{observations: nil}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on empty pair", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected empty data array: %s", body)
	}
	if !strings.Contains(body, `"stale":false`) {
		t.Errorf("stale flag should be false on observations: %s", body)
	}
}

// TestObservations_EmptyHintsTriangulationWhenAvailable: a triangulated pair has no raw rows though /v1/price serves it,
// so an empty unfiltered result probes the triangulation cache and sets `triangulated: true` to point consumers at
// /v1/price.
func TestObservations_EmptyHintsTriangulationWhenAvailable(t *testing.T) {
	hist := &stubHistoryReader{observations: nil}
	looker := &stubTriangulatedPriceLooker{
		value:          "0.16800000000000000000",
		isTriangulated: true,
		found:          true,
	}
	srv := v1.New(v1.Options{History: hist, Triangulated: looker})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected empty data array: %s", body)
	}
	if !strings.Contains(body, `"triangulated":true`) {
		t.Errorf("expected triangulated=true hint when empty + cache-hit: %s", body)
	}
}

// TestObservations_EmptyDoesNotHintWhenSourceFiltered confirms the triangulation hint never
// fires for a source-filtered query, which asks "did THIS venue see a trade?"; triangulated
// values are not attributable to any single source.
func TestObservations_EmptyDoesNotHintWhenSourceFiltered(t *testing.T) {
	hist := &stubHistoryReader{observations: nil}
	looker := &stubTriangulatedPriceLooker{
		value:          "0.16800000000000000000",
		isTriangulated: true,
		found:          true,
	}
	srv := v1.New(v1.Options{History: hist, Triangulated: looker})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD&source=sdex")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"triangulated":false`) {
		t.Errorf("source-filtered empty result must keep triangulated=false: %s", body)
	}
}

// TestObservations_InternalError — reader error propagates as 500
// without leaking the underlying message.
func TestObservations_InternalError(t *testing.T) {
	hist := &stubHistoryReader{err: errors.New("hypertable timeout")}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if strings.Contains(body, "hypertable timeout") {
		t.Errorf("internal error leaked: %s", body)
	}
}

// observationsCallTracker is a HistoryReader that counts LatestTradePerSource calls and serves
// `rows` back. The count asserts the fast-path short-circuit guards: a quote that never
// reaches storage can never return the observation a venue actually recorded for it.
type observationsCallTracker struct {
	calls atomic.Int64
	rows  []canonical.Trade
}

func (h *observationsCallTracker) LatestTradePerSource(_ context.Context, _ canonical.Pair, _ string) ([]canonical.Trade, error) {
	h.calls.Add(1)
	return h.rows, nil
}

func (h *observationsCallTracker) TradesInRange(_ context.Context, _ canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	return nil, nil
}

func (h *observationsCallTracker) TradesInRangeAfter(_ context.Context, _ canonical.Pair, _, _, _ time.Time, _ uint32, _, _ string, _ uint32, _ int) ([]canonical.Trade, error) {
	return nil, nil
}

func (h *observationsCallTracker) HistoryPoints(_ context.Context, _ canonical.Pair, _ string, _ int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (h *observationsCallTracker) HistoryPointsInRange(_ context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (h *observationsCallTracker) TWAPPointsInRange(_ context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (h *observationsCallTracker) OHLCSeries(_ context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
	return nil, nil
}

// TestObservations_FiatUSDQuoteReachesStorage: `fiat:USD` is the default quote and a real stored quote (CEX connectors
// write against it), so a memory fast path would make the default quote never return observations.
func TestObservations_FiatUSDQuoteReachesStorage(t *testing.T) {
	hist := &observationsCallTracker{
		rows: []canonical.Trade{
			mkObservationTrade("sdex", time.Unix(1_772_000_000, 0).UTC(), 100, 18),
		},
	}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fiat:USD status=%d, want 200", resp.StatusCode)
	}
	if n := hist.calls.Load(); n == 0 {
		t.Fatal("storage NOT queried for the default fiat:USD quote — the #29 short-circuit is back")
	}
	body, _ := readAll(resp)
	if strings.Contains(body, `"data":[]`) {
		t.Fatalf("fiat:USD returned an empty array despite a stored observation: %s", body)
	}
	if !strings.Contains(body, `"source":"sdex"`) {
		t.Errorf("sdex observation missing from the fiat:USD response: %s", body)
	}
}

// TestObservations_RealCexQuotesHitStorage pins that CEX connectors write genuine trades
// quoted in crypto:USDT / crypto:BTC / fiat:EUR, so those quotes must query storage; an
// all-fiat/all-crypto short-circuit would silently suppress live CEX observations.
func TestObservations_RealCexQuotesHitStorage(t *testing.T) {
	for _, q := range []string{"crypto:USDT", "crypto:BTC", "fiat:EUR"} {
		hist := &observationsCallTracker{}
		srv := v1.New(v1.Options{History: hist})
		tsv := startHTTPTest(t, srv.Handler())

		// A non-zero call count is the whole assertion: the handler consulted storage rather than answering the quote family
		// from memory. What storage holds is a separate concern.
		_ = mustGet(t, tsv.URL+"/v1/observations?asset=native&quote="+q)
		if n := hist.calls.Load(); n == 0 {
			t.Errorf("quote=%s: storage NOT queried — real CEX quote is still being short-circuited (F-1325)", q)
		}
	}
}

// /v1/observations accepts `base=` as
// alias for `asset=` so URLs from /v1/twap don't 400 on first try.
func TestObservations_BaseParamAcceptedAsAssetAlias(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, tsv.URL+"/v1/observations?base=native&quote=fiat:USD")
	if resp.StatusCode == http.StatusBadRequest {
		t.Errorf("base= alias rejected (400); want it accepted as asset= alias")
	}
}

func TestObservations_BothAssetAndBase400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	tsv := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (both asset+base)", resp.StatusCode)
	}
}

// aliasKeyedHistoryReader returns observations only for an exact pair
// string — the fixture for the alias fan-in regression below.
type aliasKeyedHistoryReader struct {
	stubHistoryReader
	byPair map[string][]canonical.Trade
}

func (r *aliasKeyedHistoryReader) LatestTradePerSource(
	_ context.Context, pair canonical.Pair, _ string,
) ([]canonical.Trade, error) {
	return r.byPair[pair.String()], nil
}

// TestObservations_AliasFanIn: CEX observations are stored under `crypto:XLM`, so `?asset=native` must scan every alias
// spelling and merge per source. Quote is crypto:USDT because fiat:USD short-circuits to the triangulation hint.
func TestObservations_AliasFanIn(t *testing.T) {
	now := time.Unix(1745000000, 0).UTC()
	usdt, _ := canonical.ParseAsset("crypto:USDT")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	native, _ := canonical.ParseAsset("native")

	cexTrade := mkObservationTrade("soroswap", now.Add(-3*time.Second), 1, 100)
	cexTrade.Pair, _ = canonical.NewPair(cryptoXLM, usdt)
	sdexTrade := mkObservationTrade("sdex", now.Add(-1*time.Second), 1, 105)
	sdexTrade.Pair, _ = canonical.NewPair(native, usdt)

	hist := &aliasKeyedHistoryReader{byPair: map[string][]canonical.Trade{
		"native/crypto:USDT":     {sdexTrade},
		"crypto:XLM/crypto:USDT": {cexTrade},
	}}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=crypto:USDT")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{`"source":"sdex"`, `"source":"soroswap"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q — alias spelling not scanned: %s", want, body)
		}
	}
}

// rendezvousHistoryReader releases its LatestTradePerSource callers only
// once `n` of them are in flight, so alias scans issued one at a time fail.
type rendezvousHistoryReader struct {
	stubHistoryReader
	n       int32
	arrived atomic.Int32
	all     chan struct{}
}

func (r *rendezvousHistoryReader) LatestTradePerSource(
	ctx context.Context, pair canonical.Pair, _ string,
) ([]canonical.Trade, error) {
	if r.arrived.Add(1) == r.n {
		close(r.all)
	}
	select {
	case <-r.all:
		t := mkObservationTrade("src:"+pair.String(), time.Unix(1745000000, 0).UTC(), 1, 100)
		return []canonical.Trade{t}, nil
	case <-time.After(2 * time.Second):
		return nil, errors.New("alias scans ran one at a time")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestObservations_AliasScansRunConcurrently — an XLM pair fans out to one
// LatestTradePerSource scan per alias spelling; run serially, a cold read
// cost the sum of every spelling's scan.
func TestObservations_AliasScansRunConcurrently(t *testing.T) {
	native, _ := canonical.ParseAsset("native")
	usdt, _ := canonical.ParseAsset("crypto:USDT")
	n := len(canonical.AssetAliases(native)) * len(canonical.AssetAliases(usdt))
	if n < 2 {
		t.Fatalf("fixture needs an aliased pair; native/crypto:USDT has %d spellings", n)
	}
	hist := &rendezvousHistoryReader{n: int32(n), all: make(chan struct{})} //nolint:gosec // small alias count
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=crypto:USDT")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := strings.Count(body, `"source":"src:`); got != n {
		t.Errorf("want one observation per alias spelling (%d), got %d: %s", n, got, body)
	}
}

// TestObservations_DivergenceCheckedStructurallyFalse: the raw per-source surface has no aggregated value to vouch for, so
// divergence_checked is false by design and the looker is never consulted. If that changes, update handleObservations
// and the OpenAPI text.
func TestObservations_DivergenceCheckedStructurallyFalse(t *testing.T) {
	now := time.Unix(1745000000, 0).UTC()
	hist := &stubHistoryReader{
		observations: []canonical.Trade{
			mkObservationTrade("soroswap", now.Add(-2*time.Second), 1, 100),
			mkObservationTrade("sdex", now.Add(-1*time.Second), 1, 105),
		},
	}
	div := &stubAliasDivergenceLooker{verdicts: map[string]struct{ firing, checked bool }{}}
	for _, a := range canonical.AssetAliases(canonical.NativeAsset()) {
		div.verdicts[a.String()] = struct{ firing, checked bool }{firing: true, checked: true}
	}
	srv := v1.New(v1.Options{History: hist, Divergence: div})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"source":"soroswap"`,
		`"divergence_checked":false`,
		`"divergence_warning":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if len(div.askedSpellings()) != 0 {
		t.Errorf("observations consulted the divergence looker for %v; the raw surface carries no verdict by design", div.askedSpellings())
	}
}

// TestObservations_ExcludesExchangeRows: exchange trade rows are not
// redistributable, so a CEX row that reaches the handler (a reader that
// ignores the storage-side filter) is dropped before the response.
func TestObservations_ExcludesExchangeRows(t *testing.T) {
	ts := time.Unix(1_772_000_000, 0).UTC()
	hist := &observationsCallTracker{rows: []canonical.Trade{
		mkObservationTrade("binance", ts, 100, 18),
		mkObservationTrade("kraken", ts, 100, 19),
		mkObservationTrade("sdex", ts, 100, 20),
	}}
	tsv := startHTTPTest(t, v1.New(v1.Options{History: hist}).Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, cex := range []string{"binance", "kraken"} {
		if strings.Contains(body, `"source":"`+cex+`"`) {
			t.Errorf("exchange row %s served raw: %s", cex, body)
		}
	}
	if !strings.Contains(body, `"source":"sdex"`) {
		t.Errorf("on-chain row missing: %s", body)
	}
}

// TestObservations_CacheUnavailable503: the fiat:USD short-circuit skips storage, so a concrete classic quote (USDC-G...)
// forces the LatestTradePerSource path that hits the cache layer.
func TestObservations_CacheUnavailable503(t *testing.T) {
	hist := &stubHistoryReader{err: miscOnfErr}
	srv := v1.New(v1.Options{History: hist})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/observations?asset=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	assertCacheUnavailable(t, resp)
}

func TestObservations_NonstandardDecimals_NormalizesPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	// base 100 tokens @9dp = 100×10^9; quote 250 @7dp = 250×10^7 → raw ratio
	// 0.025, ×K(100) = true price 2.5. (fiat:USD short-circuits observations,
	// so quote is the 7dp classic USDC.)
	tr := m2Trade(t, flaggedAsset, classicUSDC,
		"0000000000000000000000000000000000000000000000000000000000000001",
		big.NewInt(100_000_000_000), big.NewInt(2_500_000_000))
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{observations: []canonical.Trade{tr}},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/observations?asset="+flaggedAsset+"&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Guards against serving the raw ratio "0.0250000000".
	if !strings.Contains(body, `"price":"2.5000000000"`) {
		t.Errorf("/v1/observations price not normalized (want 2.5000000000): %s", body)
	}
	// Raw on-chain amounts stay smallest-unit (the ADR-0018 contract).
	if !strings.Contains(body, `"base_amount":"100000000000"`) {
		t.Errorf("/v1/observations base_amount must stay raw stroops: %s", body)
	}
}

func TestObservations_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	// native/USDC both 7dp: raw ratio 0.01626 passes through unchanged.
	tr := m2Trade(t, "native", classicUSDC,
		"0000000000000000000000000000000000000000000000000000000000000002",
		big.NewInt(1_000_000_000), big.NewInt(16_260_000))
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{observations: []canonical.Trade{tr}},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/observations?asset=native&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0162600000"`) {
		t.Errorf("7dp /v1/observations must be byte-identical (0.0162600000): %s", body)
	}
}
