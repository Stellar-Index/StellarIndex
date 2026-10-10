package v1_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stubHistoryReader implements v1.HistoryReader with a static slice.
type stubHistoryReader struct {
	trades []canonical.Trade
	// tradesByPair, when non-nil, overrides `trades` for
	// TradesInRangeAfter based on the queried pair; tradesPairs records
	// every pair that method was asked for, in order. See its godoc.
	tradesByPair map[string][]canonical.Trade
	tradesPairs  []string
	// observations is the per-source fixture returned by
	// LatestTradePerSource. Distinct from `trades` so observations
	// tests don't have to share state with TradesInRange tests.
	observations []canonical.Trade
	points       []v1.HistoryPoint
	// pointsByPair, when non-nil, overrides `points` based on the
	// query pair. Used by the stablecoin-fallback regression
	// test where the literal XLM/fiat:USD pair returns empty and a
	// proxied XLM/USDC-G… pair carries the fixture.
	pointsByPair map[string][]v1.HistoryPoint
	// twapPoints / twapPointsByPair back TWAPPointsInRange (the
	// /v1/chart?price_type=twap path). Kept separate from `points` so
	// twap tests don't collide with the vwap-chart fixtures.
	twapPoints       []v1.HistoryPoint
	twapPointsByPair map[string][]v1.HistoryPoint
	twapErr          error
	// ohlcBars is the static fixture returned by OHLCSeries. Tests
	// that want per-call behaviour override via ohlcSeriesFn.
	ohlcBars []v1.OHLCSeriesBar
	// ohlcByPair, when non-nil, keys the OHLC fixture on the requested
	// pair and honours the window — a bar comes back only when its
	// bucket lies in [from, to) — the series twin of tradesByPair.
	// ohlcPairs records every pair OHLCSeries was asked for, in order,
	// so a walk's reach and its ordering can be pinned.
	ohlcByPair map[string][]v1.OHLCSeriesBar
	ohlcPairs  []string
	// ohlcSeriesFn, when non-nil, overrides ohlcBars and lets tests
	// inject error/per-call behaviour.
	ohlcSeriesFn func(ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int) ([]v1.OHLCSeriesBar, error)
	lastCall     struct {
		from, to     time.Time
		limit        int
		afterTs      time.Time
		afterLedger  uint32
		afterTxHash  string
		afterSource  string
		afterOpIndex uint32
		granularity  string
		sourceFilter string
		ohlcInterval string
	}
	// lpsMu guards lastCall.sourceFilter: computeObservations scans the
	// alias spellings concurrently.
	lpsMu sync.Mutex
	// pointsErr is set by tests that want to drive the
	// since-inception handler to a specific error code (e.g.
	// ErrUnknownGranularity).
	pointsErr error
	err       error
}

// TradesInRange returns the fixture rows that BELONG to the requested pair,
// i.e. those whose own Trade.Pair matches it — what a real store does.
//
// The filter is load-bearing since the fiat point path was unified the fiat point path onto
// the series constituent set: `?quote=fiat:USD` now fans out over all ~18
// entries of usdPeggedConstituents, and a pair-blind stub would hand the
// same fixture back once per constituent and multiply every volume /
// trade-count assertion by the fan-out width. Fixtures that leave Pair
// unset are "unspecified" and match any request, so the pair-agnostic
// callers (observations, cursor plumbing) are unaffected.
func (r *stubHistoryReader) TradesInRange(_ context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	r.lastCall.from = from
	r.lastCall.to = to
	r.lastCall.limit = limit
	if r.err != nil {
		return nil, r.err
	}
	return tradesForPair(r.trades, pair), nil
}

// tradesForPair is the stub's pair predicate — see
// [stubHistoryReader.TradesInRange]. Returns the input slice untouched when
// no row declares a pair, so an all-unset fixture keeps its identity (and
// nil stays nil).
func tradesForPair(trades []canonical.Trade, pair canonical.Pair) []canonical.Trade {
	var zero canonical.Pair
	matched := make([]canonical.Trade, 0, len(trades))
	declared := false
	for _, t := range trades {
		if t.Pair == zero {
			continue
		}
		declared = true
		if t.Pair.Equal(pair) {
			matched = append(matched, t)
		}
	}
	if !declared {
		return trades
	}
	return matched
}

// TradesInRangeAfter: the stub ignores the cursor (tests construct
// their own trade slices per-assertion) but records it so cursor
// tests can verify the handler forwarded it. Every queried pair is
// appended to tradesPairs so alias tests can pin the read order.
//
// tradesByPair, when non-nil, keys the fixture on the requested pair —
// the trades twin of pointsByPair, used by the alias regression test
// where the literal pair is empty and the crypto:XLM spelling carries
// the rows.
//
// The flat `trades` fixture is filtered by [tradesForPair], the same
// predicate the TradesInRange sibling applies, because the handler now
// reads each pair in BOTH stored directions: a row belongs to the ONE
// orientation it declares, and a pair-blind answer would hand the same
// fixture back for a market's flip and double every page. Fixtures that
// leave Pair unset stay unspecified and match any request.
func (r *stubHistoryReader) TradesInRangeAfter(_ context.Context, pair canonical.Pair, from, to, afterTs time.Time, afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int) ([]canonical.Trade, error) {
	r.lastCall.from = from
	r.lastCall.to = to
	r.lastCall.limit = limit
	r.lastCall.afterTs = afterTs
	r.lastCall.afterLedger = afterLedger
	r.lastCall.afterTxHash = afterTxHash
	r.lastCall.afterSource = afterSource
	r.lastCall.afterOpIndex = afterOpIndex
	r.tradesPairs = append(r.tradesPairs, pair.String())
	if r.err != nil {
		return nil, r.err
	}
	if r.tradesByPair != nil {
		return r.tradesByPair[pair.String()], nil
	}
	return tradesForPair(r.trades, pair), nil
}

// HistoryPoints stub records the granularity + returns the
// pre-set fixture (or pointsErr). Honours pointsByPair when set,
// otherwise falls back to the global `points` slice.
func (r *stubHistoryReader) HistoryPoints(_ context.Context, pair canonical.Pair, granularity string, _ int) ([]v1.HistoryPoint, error) {
	r.lastCall.granularity = granularity
	if r.pointsErr != nil {
		return nil, r.pointsErr
	}
	if r.pointsByPair != nil {
		return r.pointsByPair[pair.String()], nil
	}
	return r.points, nil
}

// HistoryPointsInRange stub mirrors HistoryPoints — same fixture,
// same error path. Records the from/to so chart tests can assert
// the timeframe→window mapping.
func (r *stubHistoryReader) HistoryPointsInRange(_ context.Context, _ canonical.Pair, granularity string, from, to time.Time, _ int) ([]v1.HistoryPoint, error) {
	r.lastCall.granularity = granularity
	r.lastCall.from = from
	r.lastCall.to = to
	if r.pointsErr != nil {
		return nil, r.pointsErr
	}
	return r.points, nil
}

// TWAPPointsInRange stub records the (snapped) granularity + window
// and returns the twap fixture. Honours twapPointsByPair when set,
// else the global twapPoints slice; twapErr drives error paths.
func (r *stubHistoryReader) TWAPPointsInRange(_ context.Context, pair canonical.Pair, granularity string, from, to time.Time, _ int) ([]v1.HistoryPoint, error) {
	r.lastCall.granularity = granularity
	r.lastCall.from = from
	r.lastCall.to = to
	if r.twapErr != nil {
		return nil, r.twapErr
	}
	if r.twapPointsByPair != nil {
		return r.twapPointsByPair[pair.String()], nil
	}
	return r.twapPoints, nil
}

// OHLCSeries stub: when ohlcSeriesFn is non-nil, delegate (lets
// tests inject per-call behaviour); otherwise return r.ohlcBars.
// Records interval + from/to/limit on lastCall so tests can
// assert the handler forwarded the correct args.
func (r *stubHistoryReader) OHLCSeries(ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int) ([]v1.OHLCSeriesBar, error) {
	r.lastCall.ohlcInterval = interval
	r.lastCall.from = from
	r.lastCall.to = to
	r.lastCall.limit = limit
	r.ohlcPairs = append(r.ohlcPairs, pair.String())
	if r.ohlcSeriesFn != nil {
		return r.ohlcSeriesFn(ctx, pair, interval, from, to, limit)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.ohlcByPair != nil {
		var out []v1.OHLCSeriesBar
		for _, b := range r.ohlcByPair[pair.String()] {
			if !b.T.Time().Before(from) && b.T.Time().Before(to) {
				out = append(out, b)
			}
		}
		return out, nil
	}
	return r.ohlcBars, nil
}

// LastInterval / LastFrom / LastTo / LastLimit are public-test
// accessors for the OHLC-series call-tracking fields. Keep
// lastCall internal so tests in other files can't accidentally
// mutate it.
func (r *stubHistoryReader) LastInterval() string { return r.lastCall.ohlcInterval }
func (r *stubHistoryReader) LastFrom() time.Time  { return r.lastCall.from }
func (r *stubHistoryReader) LastTo() time.Time    { return r.lastCall.to }
func (r *stubHistoryReader) LastLimit() int       { return r.lastCall.limit }

// LatestTradePerSource stub: returns r.observations (per-source
// fixture distinct from the full r.trades slice) so observations
// tests can drive the handler without polluting other history-test
// fixtures. Honors sourceFilter — restricts to the matching entry.
func (r *stubHistoryReader) LatestTradePerSource(_ context.Context, _ canonical.Pair, sourceFilter string) ([]canonical.Trade, error) {
	r.lpsMu.Lock()
	r.lastCall.sourceFilter = sourceFilter
	r.lpsMu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if sourceFilter == "" {
		return r.observations, nil
	}
	out := make([]canonical.Trade, 0, 1)
	for _, t := range r.observations {
		if t.Source == sourceFilter {
			out = append(out, t)
		}
	}
	return out, nil
}

func mkHistTrade(price int64) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(price)),
	}
}

func TestHistory_503WhenReaderNil(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{}))

	for _, path := range []string{
		"/v1/history?base=native&quote=fiat:USD",
		"/v1/history/since-inception?asset=native",
	} {
		resp := mustGet(t, ts.URL+path)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (no reader wired)", path, resp.StatusCode)
		}
	}
}

// `asset=` is accepted as an
// alias for `base=` on endpoints that flow through parseBaseQuote,
// so clients copying /v1/price URLs into /v1/history (or twap/vwap/
// ohlc) don't hit a 400 on their first try. Pin both halves of the
// new contract: alias works for valid input, AND mixing both is a
// 400 with a self-explanatory message.
func TestHistory_AssetParamAcceptedAsBaseAlias(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?asset=native&quote=fiat:USD")
	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("asset= alias rejected (400); want it accepted as base= alias. body=%s", string(body))
	}
}

func TestHistory_BaseAndAssetBothPresent_Rejected(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (both base+asset)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mutually exclusive") {
		t.Errorf("body should mention 'mutually exclusive'; got %q", string(body))
	}
}

func TestHistory_BadRequest400(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{}}))

	fromAfterTo := url.Values{
		"base": {"native"}, "quote": {"fiat:USD"},
		"from": {"2026-04-23T12:00:00Z"}, "to": {"2026-04-23T11:00:00Z"},
	}.Encode()
	const ok = "/v1/history?base=native&quote=fiat:USD"
	cases := map[string]string{
		"missing base":                  "/v1/history?quote=fiat:USD",
		"bad base":                      "/v1/history?base=garbage&quote=fiat:USD",
		"missing quote":                 "/v1/history?base=native",
		"bad quote":                     "/v1/history?base=native&quote=garbage",
		"invalid from":                  ok + "&from=yesterday",
		"from after to":                 "/v1/history?" + fromAfterTo,
		"limit zero":                    ok + "&limit=0",
		"limit over max":                ok + "&limit=10001",
		"limit negative":                ok + "&limit=-5",
		"limit not numeric":             ok + "&limit=abc",
		"since-inception missing asset": "/v1/history/since-inception",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			resp := mustGet(t, ts.URL+path)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestHistory_ReturnsTrades(t *testing.T) {
	reader := &stubHistoryReader{
		trades: []canonical.Trade{mkHistTrade(100), mkHistTrade(101)},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(env.Data))
	}
	if env.Data[0].Source != "soroswap" {
		t.Errorf("source = %q", env.Data[0].Source)
	}
	if env.Data[0].BaseAsset != "native" || env.Data[0].QuoteAsset != "fiat:USD" {
		t.Errorf("pair fields wrong: %+v", env.Data[0])
	}
	if env.Data[0].Price == nil || *env.Data[0].Price == "" {
		t.Error("price missing")
	}
	if reader.lastCall.limit != 50 {
		t.Errorf("limit threaded to reader = %d, want 50", reader.lastCall.limit)
	}
}

// TestHistory_RoutedViaFieldPassthrough pins the additive routed_via
// wire field (migration 0025 Phase B): populated when the trade
// carries router attribution, and OMITTED (not empty-string) for
// direct trades so pre-existing consumers see byte-identical rows.
func TestHistory_RoutedViaFieldPassthrough(t *testing.T) {
	routed := mkHistTrade(100)
	routed.RoutedVia = "soroswap-router"
	direct := mkHistTrade(101)
	reader := &stubHistoryReader{trades: []canonical.Trade{routed, direct}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(env.Data))
	}
	if env.Data[0].RoutedVia != "soroswap-router" {
		t.Errorf("routed trade RoutedVia = %q, want soroswap-router", env.Data[0].RoutedVia)
	}
	if env.Data[1].RoutedVia != "" {
		t.Errorf("direct trade RoutedVia = %q, want empty", env.Data[1].RoutedVia)
	}
	// omitempty contract: exactly one routed_via key in the payload.
	if n := strings.Count(string(body), `"routed_via"`); n != 1 {
		t.Errorf("routed_via key count = %d, want 1 (omitted on direct trades); body=%s", n, body)
	}
}

func TestHistory_DefaultWindowIs1Hour(t *testing.T) {
	// When neither from nor to is set, the handler should compute a
	// 1-hour window ending ~now. Check the window duration rather
	// than absolute times (to minimize test-clock flakiness).
	reader := &stubHistoryReader{trades: nil}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	_ = mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD")

	if reader.lastCall.from.IsZero() || reader.lastCall.to.IsZero() {
		t.Fatal("handler didn't pass from/to to reader")
	}
	dur := reader.lastCall.to.Sub(reader.lastCall.from)
	if dur != time.Hour {
		t.Errorf("default window = %v, want 1h", dur)
	}
}

// A full page (rows == limit) emits a next cursor; a short page means the
// window is exhausted and emits none.
func TestHistory_NextCursorOnlyWhenPageFull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limit    string
		rows     int
		wantNext bool
	}{
		{"page full", "2", 2, true},
		{"page short", "50", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trades []canonical.Trade
			for i := 0; i < tc.rows; i++ {
				trades = append(trades, mkHistTrade(int64(100+i)))
			}
			ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: trades}}))

			resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit="+tc.limit)
			var env struct {
				Pagination *struct {
					Next string `json:"next"`
				} `json:"pagination"`
			}
			mustDecode(t, resp, &env)
			gotNext := env.Pagination != nil && env.Pagination.Next != ""
			if gotNext != tc.wantNext {
				t.Errorf("next cursor present = %v, want %v (pagination=%+v)", gotNext, tc.wantNext, env.Pagination)
			}
		})
	}
}

func TestHistory_CursorForwardedToReader(t *testing.T) {
	// A valid cursor decodes to the full PK tuple (ts, ledger,
	// tx_hash, op_index, source) and gets forwarded to the reader.
	// Widening the cursor to full PK (see history.go) means we must
	// also verify tx_hash and source round-trip.
	reader := &stubHistoryReader{trades: []canonical.Trade{mkHistTrade(100), mkHistTrade(101)}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// First request → get a cursor back.
	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=2")
	var env struct {
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)
	if env.Pagination == nil {
		t.Fatal("first request should have produced a cursor")
	}
	next := env.Pagination.Next

	// Second request with that cursor — reader sees every full-PK
	// component populated.
	reader.lastCall.afterTs = time.Time{}
	reader.lastCall.afterLedger = 0
	reader.lastCall.afterTxHash = ""
	reader.lastCall.afterSource = ""
	reader.lastCall.afterOpIndex = 0
	_ = mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&cursor="+next)

	last := reader.trades[len(reader.trades)-1]
	if reader.lastCall.afterTs.IsZero() {
		t.Error("cursor not decoded into afterTs")
	}
	if reader.lastCall.afterLedger != last.Ledger {
		t.Errorf("afterLedger = %d, want %d", reader.lastCall.afterLedger, last.Ledger)
	}
	if reader.lastCall.afterTxHash != last.TxHash {
		t.Errorf("afterTxHash = %q, want %q", reader.lastCall.afterTxHash, last.TxHash)
	}
	if reader.lastCall.afterSource != last.Source {
		t.Errorf("afterSource = %q, want %q", reader.lastCall.afterSource, last.Source)
	}
	if reader.lastCall.afterOpIndex != last.OpIndex {
		t.Errorf("afterOpIndex = %d, want %d", reader.lastCall.afterOpIndex, last.OpIndex)
	}
}

func TestHistory_InvalidCursor400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	// base64-encode each "raw" cursor shape below.
	b64 := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	lowerHex64 := "fadefadefadefadefadefadefadefadefadefadefadefadefadefadefadefade"
	uppercaseHex := "FADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADEFADE"

	for _, bad := range []string{
		"not-base64!!!",
		"dGVzdA", // base64 of "test" — no colon separator
		// Empty source — would degenerate the full-PK cursor back
		// into the (ts, ledger)-only shape that loses rows sharing a
		// ledger.
		b64("100:1::" + lowerHex64 + ":0"),
		// Bad tx_hash format (63 chars, missing one).
		b64("100:1:soroswap:" + lowerHex64[:63] + ":0"),
		// Uppercase hex tx_hash (canonical form is lowercase).
		b64("100:1:soroswap:" + uppercaseHex + ":0"),
	} {
		resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&cursor="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cursor=%q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestHistory_EmptyListReturnsEmptyArray(t *testing.T) {
	// No trades in the window → empty array, not null.
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	var parsed struct {
		Data []v1.TradeRow `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if parsed.Data == nil {
		t.Error("empty result should be [] not null")
	}
}

// TestHistory_NativeReadsCryptoXLMAlias pins the alias fan-in on the raw
// trade feed. The handler keyed its trades scan on the LITERAL pair, so
// `?base=native&quote=fiat:USD` served an empty page while the identical
// window under `?base=crypto:XLM` returned a full one and /v1/vwap, which
// loops the aliases, reported a live trade population for the pair the
// same hour. XLM's three canonical spellings are disjoint venue
// populations (the on-chain decoders stamp `native`, the CEX parsers
// `crypto:XLM`), and every sibling read path already loops
// canonical.AssetAliases.
//
// The literal form must still be read FIRST — a populated pair must not
// be overtaken by an alias — and the rows carry the spelling they were
// stored under, so a client can always see which form answered.
func TestHistory_NativeReadsCryptoXLMAlias(t *testing.T) {
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	aliasPair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatal(err)
	}
	cex := mkHistTrade(100)
	cex.Source = "coinbase"
	cex.Pair = aliasPair

	// Only the crypto:XLM spelling has rows; native/fiat:USD is empty.
	reader := &stubHistoryReader{tradesByPair: map[string][]canonical.Trade{
		aliasPair.String(): {cex},
	}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1 — ?base=native must reach the crypto:XLM trades", len(env.Data))
	}
	if env.Data[0].BaseAsset != "crypto:XLM" || env.Data[0].QuoteAsset != "fiat:USD" {
		t.Errorf("row pair = %s/%s, want crypto:XLM/fiat:USD (the spelling it was stored under)",
			env.Data[0].BaseAsset, env.Data[0].QuoteAsset)
	}
	if len(reader.tradesPairs) == 0 || reader.tradesPairs[0] != "native/fiat:USD" {
		t.Errorf("first scan = %v, want native/fiat:USD (the literal form leads)", reader.tradesPairs)
	}
}

// TestHistory_PaginationUnionsInterleavedAliasForms pins
// CA2-A04-harden-7: with two alias forms of the SAME asset both
// populated and their rows genuinely interleaved in time, a first-hit
// gate (whichever alias form's page came back non-empty on THAT
// request) serves the leading form until it drains, then falls through
// to the sibling form with a cursor already past that sibling's earlier
// rows — losing them permanently, not just serving them out of order.
//
// Six rows: native/usdc at sec 10, 20, 90; crypto:XLM/usdc at sec 30,
// 40, 50 — interleaved so the native form's LAST row (sec 90) sits after
// every crypto:XLM row. Draining at limit=1 must still return the full
// union in strict timestamp order; a first-hit gate would return
// only the three native rows (10, 20, 90) and silently drop all three
// crypto:XLM rows once the native form drains past them.
func TestHistory_PaginationUnionsInterleavedAliasForms(t *testing.T) {
	t.Parallel()
	native := mustParseAsset(t, "native")
	xlmAlias := mustParseAsset(t, "crypto:XLM")
	usdc := mustParseAsset(t, usdcClassicID)

	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "1a", native, usdc, 1, 1),
		storedTrade(t, "sdex", 20, "2a", native, usdc, 1, 1),
		storedTrade(t, "coinbase", 30, "1b", xlmAlias, usdc, 1, 1),
		storedTrade(t, "coinbase", 40, "2b", xlmAlias, usdc, 1, 1),
		storedTrade(t, "coinbase", 50, "3b", xlmAlias, usdc, 1, 1),
		storedTrade(t, "sdex", 90, "3a", native, usdc, 1, 1),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(native, usdc, 1))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d — the crypto:XLM form's rows must not be "+
			"dropped once the native form drains past them", len(served), sizes, len(rows))
	}
	wantTxSuffix := []string{"1a", "2a", "1b", "2b", "3b", "3a"}
	for i, row := range served {
		if !strings.HasSuffix(row.TxHash, wantTxSuffix[i]) {
			t.Errorf("row %d tx_hash = %s, want suffix %q — the union must be in strict timestamp order",
				i, row.TxHash, wantTxSuffix[i])
		}
		if i > 0 && !served[i-1].Timestamp.Time().Before(row.Timestamp.Time()) {
			t.Errorf("row %d ts %s is not after row %d ts %s", i, row.Timestamp, i-1, served[i-1].Timestamp)
		}
	}
}

// ─── /v1/history/since-inception ────────────────────────────────

// TestSpecDeclares404OnScamWithholdingRoutes pins that both
// /history/since-inception and /chart return a 404 price-withheld when
// seriesWithheldForScam fires (see handleHistorySinceInception and
// handleChart), but neither route declared a 404 in the OpenAPI
// contract — a spec-driven client would treat it as endpoint-not-found
// and fall back elsewhere, the same harm as on the price
// stream.
func TestSpecDeclares404OnScamWithholdingRoutes(t *testing.T) {
	specPath := filepath.Join(moduleRoot(t), "openapi", "stellar-index.v1.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/history/since-inception", "/chart"} {
		op, ok := spec.Paths[path]["get"]
		if !ok {
			t.Fatalf("spec has no GET %s", path)
		}
		if _, ok := op.Responses["404"]; !ok {
			t.Errorf("GET %s does not declare 404, but seriesWithheldForScam serves one", path)
		}
	}
}

// TestHistory_BelowCoverageFloorIsFlagged — /v1/history returns raw
// trades, not buckets, and its empty page carries the identical
// ambiguity. The annotation rides on the ENVELOPE precisely so this
// surface, whose `data` is a bare array, can carry it too.
func TestHistory_BelowCoverageFloorIsFlagged(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := emptyHistoryServer(t, probe)

	resp := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, resp, &env)
	if !env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = false on an empty below-floor trade window")
	}
	if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want %s", env.CoverageFrom, xlmCoverageFloor)
	}
}

// TestHistory_DrainedCursorPageIsNotProbed — an empty page reached by
// PAGINATION means "you have all the rows", not "there is nothing
// here", and the cursor shadows `from` so the window the signal would
// describe is not the window that was read. Neither probe nor flag.
func TestHistory_DrainedCursorPageIsNotProbed(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := emptyHistoryServer(t, probe)

	// One full page first, to obtain a server-minted cursor.
	first := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z&limit=1")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", first.StatusCode)
	}
	if calls, _, _, _ := probe.snapshot(); calls != 1 {
		t.Fatalf("setup: probe calls = %d on the first empty page, want 1", calls)
	}

	second := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z&cursor="+drainedHistoryCursor)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("cursor page status = %d, want 200", second.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, second, &env)
	if env.Flags.OutsideCoverage || env.CoverageFrom != nil {
		t.Errorf("drained cursor page carried a coverage signal: outside=%v from=%v",
			env.Flags.OutsideCoverage, env.CoverageFrom)
	}
	if calls, _, _, _ := probe.snapshot(); calls != 1 {
		t.Errorf("probe calls = %d, want the cursor page to add none", calls)
	}
}

// TestHistory_ReverseStoredMarketCarriesTheFloor pins that correspondence.
func TestHistory_ReverseStoredMarketCarriesTheFloor(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		// Stored as USDC/AQUA only.
		probeKey(usdc, aqua): pegFloor2021,
	}}
	ts := emptyHistoryServer(t, probe)
	const window = "&from=2019-01-01T00:00:00Z&to=2019-02-01T00:00:00Z"

	getHistory := func(t *testing.T, pairQS string) coverageMeta {
		t.Helper()
		resp := mustGet(t, ts.URL+"/v1/history?"+pairQS+window)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var env coverageMeta
		mustDecode(t, resp, &env)
		return env
	}

	t.Run("requested orientation is not the stored one: the floor", func(t *testing.T) {
		env := getHistory(t, "base="+aquaClassicID+"&quote="+usdcClassicID)
		assertCoverage(t, env, &pegFloor2021, true)
		if probe.stored() != 0 {
			t.Errorf("stored-orientation probes = %d, want 0 — /v1/history reads both directions, so it must not probe one", probe.stored())
		}
	})
	t.Run("requested orientation is the stored one: the same floor", func(t *testing.T) {
		env := getHistory(t, "base="+usdcClassicID+"&quote="+aquaClassicID)
		assertCoverage(t, env, &pegFloor2021, true)
		if probe.stored() != 0 {
			t.Errorf("stored-orientation probes = %d, want 0", probe.stored())
		}
	})
	t.Run("one probe spans every market the page read reaches", func(t *testing.T) {
		// A FRESH probe, so the span under test is the one read this
		// request made — a union across the two orientations above would
		// cover both markets even under a per-orientation probe, and
		// would assert nothing.
		fresh := &coverageFloorProbe{byPair: map[string]time.Time{probeKey(usdc, aqua): pegFloor2021}}
		freshTS := httpTestServer(t, v1.New(v1.Options{
			History:       &stubHistoryReader{},
			CoverageFloor: fresh,
		}))
		resp := mustGet(t, freshTS.URL+"/v1/history?base="+aquaClassicID+"&quote="+usdcClassicID+window)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var env coverageMeta
		mustDecode(t, resp, &env)

		reads := fresh.probedReads()
		if len(reads) != 1 {
			t.Fatalf("probes = %d, want 1 — the raw-trade page has one entry in its set", len(reads))
		}
		span := map[string]bool{}
		for _, k := range probeSpanKeys(reads[0].pair, reads[0].span) {
			span[k] = true
		}
		for _, want := range []string{probeKey(usdc, aqua), probeKey(aqua, usdc)} {
			if !span[want] {
				t.Errorf("%s is outside the span of the one probe (%s), but the page read reads it",
					want, reads[0].span)
			}
		}
	})
	t.Run("the series surface reports the same floor", func(t *testing.T) {
		env := ohlcCoverageGetPair(t, ts, "base="+aquaClassicID+"&quote="+usdcClassicID,
			"2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z")
		assertCoverage(t, env.coverageMeta, &pegFloor2021, true)
	})
}

// /v1/history: base_decimals is stamped on every row from the same read.
func TestHistory_DecimalsReadFailed_Returns503(t *testing.T) {
	pair, err := canonical.NewPair(mustParseAsset(t, decTestContract), mustParseAsset(t, "native"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	tr := mkHistTrade(100)
	tr.Pair = pair
	reader := &stubHistoryReader{trades: []canonical.Trade{tr}}
	srv := v1.New(v1.Options{History: reader, TokenDecimals: &countingDecStub{err: errDecimalsRead}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base="+decTestContract+"&quote=native&limit=50")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 rather than base_decimals 7 on a failed read: %s",
			resp.StatusCode, body)
	}
}

// TestHistory_PerSideDecimals: the additive base_decimals/quote_decimals
// fields resolve the Soroban token's declared decimals() for the base
// side (via the TokenDecimals reader) and default 7 for a native/classic
// quote — resolved ONCE per request and stamped on every row.
func TestHistory_PerSideDecimals(t *testing.T) {
	// The fixture declares the pair it is requested under: the page read
	// walks both stored directions, and the stub answers a pair the way
	// the trades table does — a row belongs to the ONE orientation it
	// carries. The decimals assertions below are unchanged by that.
	pair, err := canonical.NewPair(mustParseAsset(t, decTestContract), mustParseAsset(t, "native"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	first, second := mkHistTrade(100), mkHistTrade(101)
	first.Pair, second.Pair = pair, pair
	reader := &stubHistoryReader{trades: []canonical.Trade{first, second}}
	stub := &decStub{d: 6, found: true}
	srv := v1.New(v1.Options{History: reader, TokenDecimals: stub})
	ts := httpTestServer(t, srv)

	// base = Soroban token (6 decimals per stub), quote = native (7).
	resp := mustGet(t, ts.URL+"/v1/history?base="+decTestContract+"&quote=native&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(env.Data))
	}
	for i, row := range env.Data {
		if row.BaseDecimals != 6 {
			t.Errorf("row %d base_decimals = %d, want 6 (Soroban resolved)", i, row.BaseDecimals)
		}
		if row.QuoteDecimals != 7 {
			t.Errorf("row %d quote_decimals = %d, want 7 (native default)", i, row.QuoteDecimals)
		}
	}
	// Resolved once per side (not per row) — the reader is consulted for
	// the Soroban base contract.
	if stub.gotContract != decTestContract {
		t.Errorf("consulted contract = %q, want %q", stub.gotContract, decTestContract)
	}
	// Both decimals keys present on /v1/history rows (2 rows × 2 keys).
	if n := strings.Count(string(body), `"base_decimals"`); n != 2 {
		t.Errorf("base_decimals key count = %d, want 2; body=%s", n, body)
	}
}

// TestHistory_ClassicNeverConsultsDecimals: a classic/native pair resolves
// to 7 on both sides WITHOUT consulting the token-decimals reader (a wrong
// overlay would mis-scale every amount on the markets page).
func TestHistory_ClassicDecimalsNoReaderConsult(t *testing.T) {
	reader := &stubHistoryReader{trades: []canonical.Trade{mkHistTrade(100)}}
	stub := &decStub{d: 18, found: true} // would lie if consulted
	srv := v1.New(v1.Options{History: reader, TokenDecimals: stub})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1", len(env.Data))
	}
	if env.Data[0].BaseDecimals != 7 || env.Data[0].QuoteDecimals != 7 {
		t.Errorf("native/fiat decimals = (%d,%d), want (7,7)",
			env.Data[0].BaseDecimals, env.Data[0].QuoteDecimals)
	}
	if stub.gotContract != "" {
		t.Errorf("classic/native pair consulted the decimals reader (contract %q)", stub.gotContract)
	}
}

// TestHistory_OffChainSourceDecimals is the regression test for the
// off-chain decimals divisor.
//
// base_decimals/quote_decimals are documented as THE divisor for turning
// a row's raw amount into whole units, and they were resolved once per
// page from the ASSET. But the scale an amount was stamped at is a
// property of the CONNECTOR, not the asset: the CEX parsers stamp 1e8
// and the FX pollers 1e6, while the asset resolver returns 7 for
// anything non-Soroban. A page mixes both — /v1/history for a
// crypto/fiat pair returns sdex rows beside coinbase rows — so a
// consumer following the documented conversion overstated every CEX
// trade by exactly 10x. `price` is scale-invariant, so nothing in the
// response contradicted it.
//
// Failure shape: coinbase rows served base_decimals 7
// against a parser that stamps 8.
func TestHistory_OffChainSourceDecimals(t *testing.T) {
	onChain := mkHistTrade(100) // Source: soroswap
	cex := mkHistTrade(101)
	cex.Source = "coinbase"
	fx := mkHistTrade(102)
	fx.Source = "exchangeratesapi"

	reader := &stubHistoryReader{trades: []canonical.Trade{onChain, cex, fx}}
	srv := v1.New(v1.Options{History: reader, TokenDecimals: &decStub{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 3 {
		t.Fatalf("got %d rows, want 3", len(env.Data))
	}

	for i, want := range []int{7, 8, 6} { // soroswap / coinbase / exchangeratesapi
		got := env.Data[i]
		if got.BaseDecimals != want || got.QuoteDecimals != want {
			t.Errorf("row %d (source %q) decimals = (%d,%d), want (%d,%d) — the scale is a property of the connector, not the asset",
				i, got.Source, got.BaseDecimals, got.QuoteDecimals, want, want)
		}
	}
}

// TestHistory_OrientationRendering pins, to the value, how a stored row is
// re-expressed in the requested orientation (AQUA/USDC).
//
//   - reverse-stored: the market exists only as USDC/AQUA. The
//     legs and amounts swap, `price` is the exact reciprocal,
//     and both directions are read, requested first. The 7:1 row would expose
//     a float round-trip: 1/(1/7) is not 7 in binary floating point.
//   - stored-orientation: a row already held as asked is served untouched.
//   - zero-leg: a flipped one-side-zero row (SDEX rounding fill) makes the
//     zero leg the denominator and renders "price": null without dropping or
//     poisoning the page.
//   - exact-at-scale: magnitudes where a float, or a reciprocal taken as a
//     division rather than a swap, drifts. Only the final render rounds: ten
//     fractional digits, extended only when the first significant digit lies
//     beyond the tenth place, so a positive price is never an all-zero string.
func TestHistory_OrientationRendering(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	type wantRow struct{ base, quote, price string }
	for _, tc := range []struct {
		name      string
		rows      []canonical.Trade
		want      []wantRow
		wantReads bool
	}{
		{
			name: "reverse-stored",
			rows: []canonical.Trade{
				storedTrade(t, "sdex", 10, "a1", usdc, aqua, 7, 1), // 7 USDC units bought 1 AQUA unit
				storedTrade(t, "sdex", 20, "a2", usdc, aqua, 1, 3), // 1 USDC unit bought 3 AQUA units
			},
			want:      []wantRow{{"1", "7", "7.0000000000"}, {"3", "1", "0.3333333333"}},
			wantReads: true,
		},
		{
			name: "stored-orientation",
			rows: []canonical.Trade{storedTrade(t, "sdex", 10, "b1", aqua, usdc, 7, 1)},
			want: []wantRow{{"7", "1", "0.1428571428"}},
		},
		{
			name: "zero-leg",
			rows: []canonical.Trade{
				storedTrade(t, "sdex", 10, "5a", usdc, aqua, 5, 0),
				storedTrade(t, "sdex", 20, "5b", usdc, aqua, 5, 1),
			},
			want: []wantRow{{"0", "5", "null"}, {"1", "5", "5.0000000000"}},
		},
		{
			name: "exact-at-scale",
			rows: []canonical.Trade{
				storedTradeAmounts(t, "sdex", 10, "4a", usdc, aqua, "1000000000000000001", "3"),
				storedTradeAmounts(t, "sdex", 20, "4b", usdc, aqua, "3", "1000000000000000001"),
				storedTradeAmounts(t, "sdex", 30, "4c", usdc, aqua, "7", "1"),
				storedTradeAmounts(t, "sdex", 40, "4d", usdc, aqua, "1", "3"),
			},
			want: []wantRow{
				{"3", "1000000000000000001", "333333333333333333.6666666666"},
				{"1000000000000000001", "3", "0.000000000000000002999999999999"},
				{"1", "7", "7.0000000000"},
				{"3", "1", "0.3333333333"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &orientedTradeStore{rows: tc.rows}
			ts := httpTestServer(t, v1.New(v1.Options{History: store}))

			page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
			if len(page.Data) != len(tc.want) {
				t.Fatalf("rows = %d, want %d — a degenerate or reverse-stored row must not drop the page", len(page.Data), len(tc.want))
			}
			for i, w := range tc.want {
				got := page.Data[i]
				if got.BaseAsset != aqua.String() || got.QuoteAsset != usdc.String() {
					t.Errorf("row %d pair = %s/%s, want %s/%s", i, got.BaseAsset, got.QuoteAsset, aqua, usdc)
				}
				if got.BaseAmount != w.base || got.QuoteAmount != w.quote || priceString(got.Price) != w.price {
					t.Errorf("row %d = %s/%s @ %s, want %s/%s @ %s",
						i, got.BaseAmount, got.QuoteAmount, priceString(got.Price), w.base, w.quote, w.price)
				}
			}
			if tc.wantReads {
				pairs := store.readPairs()
				if len(pairs) != 2 || pairs[0] != aqua.String()+"/"+usdc.String() || pairs[1] != usdc.String()+"/"+aqua.String() {
					t.Errorf("reads = %v, want the requested orientation then its flip", pairs)
				}
			}
		})
	}
}

// TestHistory_BothDirectionsMergeInKeysetOrder pins the merge on a
// market the decoder recorded in both directions — the shape /v1/ohlc
// folds in SQL. Every row appears once, in the endpoint's order, each
// re-expressed by the orientation it was STORED in.
func TestHistory_BothDirectionsMergeInKeysetOrder(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTrade(t, "sdex", 10, "c1", aqua, usdc, 4, 1),  // requested-side
		storedTrade(t, "sdex", 20, "c2", usdc, aqua, 1, 5),  // flipped
		storedTrade(t, "sdex", 30, "c3", aqua, usdc, 8, 1),  // requested-side
		storedTrade(t, "sdex", 40, "c4", usdc, aqua, 1, 10), // flipped
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 4 {
		t.Fatalf("rows = %d, want 4 — both stored directions belong to one market", len(page.Data))
	}
	wantAmounts := [][2]string{{"4", "1"}, {"5", "1"}, {"8", "1"}, {"10", "1"}}
	for i, row := range page.Data {
		if row.BaseAsset != aqua.String() || row.QuoteAsset != usdc.String() {
			t.Errorf("row %d pair = %s/%s, want %s/%s", i, row.BaseAsset, row.QuoteAsset, aqua, usdc)
		}
		if row.BaseAmount != wantAmounts[i][0] || row.QuoteAmount != wantAmounts[i][1] {
			t.Errorf("row %d amounts = %s/%s, want %s/%s",
				i, row.BaseAmount, row.QuoteAmount, wantAmounts[i][0], wantAmounts[i][1])
		}
	}
	for i := 1; i < len(page.Data); i++ {
		if !page.Data[i-1].Timestamp.Time().Before(page.Data[i].Timestamp.Time()) {
			t.Errorf("row %d ts %s is not after row %d ts %s — the merge must hold the keyset order",
				i, page.Data[i].Timestamp, i-1, page.Data[i-1].Timestamp)
		}
	}
	if page.Pagination != nil {
		t.Errorf("pagination = %+v, want absent — the window is drained", page.Pagination)
	}
}

// TestHistory_PageBoundaryAcrossDirections is the boundary case: both
// directions hold rows immediately after every cursor, so each page cut
// falls between the two streams. The drain must serve every row exactly
// once, in order, and must not report the window drained while rows
// remain in the other direction.
func TestHistory_PageBoundaryAcrossDirections(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	// Strictly interleaved: odd seconds requested-side, even flipped.
	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "d1", aqua, usdc, 1, 11),
		storedTrade(t, "sdex", 11, "d2", usdc, aqua, 12, 1),
		storedTrade(t, "sdex", 12, "d3", aqua, usdc, 1, 13),
		storedTrade(t, "sdex", 13, "d4", usdc, aqua, 14, 1),
		storedTrade(t, "sdex", 14, "d5", aqua, usdc, 1, 15),
		storedTrade(t, "sdex", 15, "d6", usdc, aqua, 16, 1),
		storedTrade(t, "sdex", 16, "d7", aqua, usdc, 1, 17),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d — a page boundary between the two directions must not drop a row",
			len(served), sizes, len(rows))
	}
	for i, row := range served {
		if row.BaseAsset != aqua.String() || row.QuoteAsset != usdc.String() {
			t.Errorf("row %d pair = %s/%s, want %s/%s", i, row.BaseAsset, row.QuoteAsset, aqua, usdc)
		}
		if i > 0 && !served[i-1].Timestamp.Time().Before(row.Timestamp.Time()) {
			t.Errorf("row %d ts %s is not after row %d ts %s — the drain must hold one order across pages",
				i, row.Timestamp, i-1, served[i-1].Timestamp)
		}
	}
	assertServedExactlyOnce(t, served, rows, sizes)
}

// TestHistory_PageBoundaryWithinOneLedger tightens the boundary onto a
// single (ts, ledger): both directions hold rows there, so the cut is
// decided by tx_hash — the component the merge and the database order
// identically — rather than by time.
func TestHistory_PageBoundaryWithinOneLedger(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "e1", aqua, usdc, 1, 21),
		storedTrade(t, "sdex", 10, "e2", usdc, aqua, 22, 1),
		storedTrade(t, "sdex", 10, "e3", aqua, usdc, 1, 23),
		storedTrade(t, "sdex", 10, "e4", usdc, aqua, 24, 1),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 1))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d", len(served), sizes, len(rows))
	}
	for i, row := range served {
		wantSuffix := fmt.Sprintf("e%d", i+1)
		if !strings.HasSuffix(row.TxHash, wantSuffix) {
			t.Errorf("row %d tx = %s, want the one ending %s — one ledger's rows order by tx_hash across both directions",
				i, row.TxHash, wantSuffix)
		}
	}
}

// TestHistory_PageIsNotCutThroughATieGroup pins the one comparison the
// merge deliberately refuses to make.
//
// Two rows can share (ts, ledger, tx_hash, op_index) and differ only in
// `source` — the trades primary key allows it — and `source` is the one
// keyset component whose Go byte order can disagree with the database's
// collation. Cutting a page between two such rows would mint a cursor
// the database may order AFTER the row that was dropped, and that row
// would never be served. So the cut moves off the group, and the page
// comes back SHORT with a cursor rather than full and lossy.
func TestHistory_PageIsNotCutThroughATieGroup(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	tied := storedTrade(t, "blend_emitter", 20, "f2", aqua, usdc, 1, 31)
	tiedFlip := storedTrade(t, "blenda", 20, "f2", usdc, aqua, 32, 1)
	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "f1", aqua, usdc, 1, 30),
		tied,
		tiedFlip,
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// limit 2 would cut between the two tied rows.
	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 2))
	if len(page.Data) != 1 {
		t.Fatalf("first page = %d rows, want 1 — the page must stop before the tie group, not inside it", len(page.Data))
	}
	if page.Pagination == nil {
		t.Fatalf("pagination absent on a short page that has rows behind it — the cursor must not be inferred from the page length")
	}

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d", len(served), sizes, len(rows))
	}
	assertServedExactlyOnce(t, served, rows, sizes)
}

// TestHistory_OverLimitTieGroupIsCompletedBeforeItIsServed: `limit`
// validates to [1, 10000], so a caller can ask for limit=1 against a
// three-source group. The flipped row's source sorts ABOVE the stored row
// a short read leaves unfetched, so a page that serves the group without
// completing it mints a cursor past that row. Limit 2 repeats it one size
// up so limit=1 cannot be read as a boundary artefact.
func TestHistory_OverLimitTieGroupIsCompletedBeforeItIsServed(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	for _, tc := range []struct {
		name  string
		limit int
		rows  []canonical.Trade
		// firstPageWhole pins that the group is completed, then served on one page.
		firstPageWhole bool
	}{
		{"limit 1", 1, []canonical.Trade{
			storedTrade(t, "aaa_src", 10, "1e", aqua, usdc, 1, 41),
			storedTrade(t, "bbb_src", 10, "1e", aqua, usdc, 1, 42),
			storedTrade(t, "ccc_src", 10, "1e", usdc, aqua, 43, 1),
		}, true},
		{"limit 2", 2, []canonical.Trade{
			storedTrade(t, "aaa_src", 10, "2f", aqua, usdc, 1, 51),
			storedTrade(t, "bbb_src", 10, "2f", aqua, usdc, 1, 52),
			storedTrade(t, "ccc_src", 10, "2f", aqua, usdc, 1, 53), // unfetched at limit=2
			storedTrade(t, "ddd_src", 10, "2f", usdc, aqua, 54, 1), // flipped, highest source
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := httpTestServer(t, v1.New(v1.Options{History: &orientedTradeStore{rows: tc.rows}}))
			served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, tc.limit))
			assertServedExactlyOnce(t, served, tc.rows, sizes)
			if tc.firstPageWhole && (len(sizes) == 0 || sizes[0] != len(tc.rows)) {
				t.Errorf("first page = %v, want the whole group (%d rows) on one page", sizes, len(tc.rows))
			}
		})
	}
}

// TestHistory_TieGroupSweepLosesNothing walks every arrangement of four
// rows over two tie groups, both orientations, at limits 1..3 — the
// shapes a hand-built fixture keeps missing. Every row must be served
// EXACTLY once: losing one is the failure the merge exists to prevent,
// and repeating one silently inflates any total a client accumulates
// across pages.
func TestHistory_TieGroupSweepServesEachRowOnce(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)
	sources := []string{"s1", "s2", "s3", "s4"}

	store := &orientedTradeStore{}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))
	for mask := 0; mask < 256; mask++ {
		for limit := 1; limit <= 3; limit++ {
			rows := make([]canonical.Trade, 0, len(sources))
			for k, src := range sources {
				sec, tx := int64(10), "1c"
				if mask&(1<<(k+4)) != 0 {
					sec, tx = 20, "2d"
				}
				if mask&(1<<k) != 0 {
					rows = append(rows, storedTrade(t, src, sec, tx, usdc, aqua, 100, 1))
					continue
				}
				rows = append(rows, storedTrade(t, src, sec, tx, aqua, usdc, 1, 100))
			}
			store.reset(rows, nil)
			served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, limit))

			if !assertServedExactlyOnce(t, served, rows, sizes) {
				t.Errorf("mask=%d limit=%d", mask, limit)
			}
		}
	}
	// Exactly-once, not "few": a page ending on a complete tie group resumes
	// past it by key, so a client summing volume across pages is never inflated.
}

// TestHistory_FlippedRowNonstandardDecimals pins the pairing between the
// flipped trade slice and the request's legs.
//
// normalizeTradeRowPrices is handed the trades the page returned — now
// re-expressed — while `base` and `quote` are the REQUEST's legs. If
// that pairing were inverted the correction would run 10^-2 instead of
// 10^+2 and the price would read 0.00025 rather than 2.5, on the one
// surface whose whole point is the per-trade number.
func TestHistory_FlippedRowNonstandardDecimals(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flagged := mustParseAsset(t, flaggedAsset)
	usd := mustParseAsset(t, "fiat:USD")
	// Stored the other way round: 250 USD (7dp) bought 100 tokens (9dp).
	stored := storedTradeAmounts(t, "aquarius", 10, "3a", usd, flagged, "2500000000", "100000000000")
	ts := httpTestServer(t, v1.New(v1.Options{
		History:             &orientedTradeStore{rows: []canonical.Trade{stored}},
		NonstandardDecimals: cache,
	}))

	page := getHistoryPage(t, ts, orientationQuery(flagged, usd, 50))
	if len(page.Data) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Data))
	}
	got := page.Data[0]
	if got.BaseAmount != "100000000000" || got.QuoteAmount != "2500000000" {
		t.Errorf("amounts = %s/%s, want 100000000000/2500000000", got.BaseAmount, got.QuoteAmount)
	}
	if priceString(got.Price) != "2.5000000000" {
		t.Errorf("price = %s, want 2.5000000000 — 250 USD over 100 tokens, corrected for a 9dp base against a 7dp quote", priceString(got.Price))
	}
}

// TestHistory_PastGroupCursorPointingBeyondTheWindowTerminates drives
// the case the arithmetic makes possible: a cursor stepped past a
// complete group that is then asked about a window holding nothing after
// it. The page must come back empty, carry NO cursor, and stop —
// re-serving the group, or handing back a cursor to be asked again,
// would be an endless drain.
//
// A client narrowing `to` between pages is what produces it: the first
// request spans both groups, so the cursor is minted past the first; the
// second replays that cursor over a window that ends before the second
// group exists.
func TestHistory_PastGroupCursorPointingBeyondTheWindowTerminates(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	// A complete two-source group at +10s, then one later row at +30s so
	// the first page ends on the group with rows still behind it.
	//
	// The FLIPPED row's source sorts BELOW the stored row's, which is the
	// arrangement that makes the two orderings disagree: the merge puts
	// the requested orientation first on a tie, so a last-row cursor
	// would name `aaa_src` and the database would hand `zzz_src` back on
	// the next page. Reverse these two and the case tests nothing.
	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTrade(t, "zzz_src", 10, "c1", aqua, usdc, 1, 61),
		storedTrade(t, "aaa_src", 10, "c1", usdc, aqua, 62, 1),
		storedTrade(t, "ccc_src", 30, "c2", aqua, usdc, 1, 63),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	wide := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 2))
	if len(wide.Data) != 2 {
		t.Fatalf("first page = %d rows, want the 2-row group", len(wide.Data))
	}
	if wide.Pagination == nil {
		t.Fatalf("first page carried no cursor, but a row remains behind it")
	}

	// Same cursor, window clipped to end before the +30s row.
	narrow := url.Values{
		"base":   {aqua.String()},
		"quote":  {usdc.String()},
		"from":   {orientationWindowStart.Format(time.RFC3339)},
		"to":     {orientationWindowStart.Add(20 * time.Second).Format(time.RFC3339)},
		"limit":  {"2"},
		"cursor": {wide.Pagination.Next},
	}.Encode()
	page := getHistoryPage(t, ts, narrow)
	if len(page.Data) != 0 {
		t.Errorf("page past the end = %d rows, want 0 — the cursor steps past the group, and nothing follows it in this window", len(page.Data))
	}
	if page.Pagination != nil {
		t.Errorf("page past the end carried a cursor (%q), want none — the drain must stop", page.Pagination.Next)
	}
	// And the same cursor over the ORIGINAL window still reaches the row
	// it was minted to reach: stepping past a group must not step past
	// anything else.
	rest, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	assertServedExactlyOnce(t, rest, store.rows, sizes)
}

// TestHistory_PastGroupCursorDeclinesToWrapOpIndex pins the guard. At
// the maximum op_index the step would wrap to zero — a cursor pointing
// at the START of the transaction, which re-serves it forever — so the
// read keeps the last-row cursor instead. That cursor can repeat a row;
// it cannot loop, and it cannot lose one.
func TestHistory_PastGroupCursorDeclinesToWrapOpIndex(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	rows := []canonical.Trade{
		storedTrade(t, "zzz_src", 10, "d1", aqua, usdc, 1, 71),
		storedTrade(t, "aaa_src", 10, "d1", usdc, aqua, 72, 1),
		storedTrade(t, "ccc_src", 30, "d2", aqua, usdc, 1, 73),
	}
	for i := range rows {
		rows[i].OpIndex = math.MaxUint32
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// drainHistory fails the test if the drain does not terminate, which
	// is the shape a wrapped cursor takes.
	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	seen := map[string]int{}
	for _, r := range served {
		seen[r.Source]++
	}
	for _, r := range rows {
		if seen[r.Source] == 0 {
			t.Errorf("source %s never served (pages %v) — the fallback cursor must not skip", r.Source, sizes)
		}
	}
}

// TestHistory_TieGroupLargerThanAnyPageIsServedWhole is the case a
// bounded completion budget got wrong.
//
// An over-limit tie group is completed by re-reading the truncated
// direction, and that re-read goes straight to the store's maximum
// rather than climbing a ladder. A ladder has a top, and a group past
// the top fell to a branch that cut THROUGH the group and minted a
// last-row cursor — so the flipped row whose source sorts below every
// stored source in the group was excluded by the database's own
// predicate and never served on any page. The branch is gone; this is
// the fixture that would bring it back.
func TestHistory_TieGroupLargerThanAnyPageIsServedWhole(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)

	// One group: 300 rows in the requested orientation plus a single
	// flipped row whose source sorts BELOW all of them. Far past any
	// plausible page size, and past any ladder that could bound the
	// completion.
	rows := make([]canonical.Trade, 0, 301)
	rows = append(rows, storedTrade(t, "aaa_low", 10, "0c", usdc, aqua, 100, 1))
	for i := 0; i < 300; i++ {
		rows = append(rows, storedTrade(t, fmt.Sprintf("s%03d", i), 10, "0c", aqua, usdc, 1, 100))
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// limit=1 is client-reachable, and it is the smallest page that can
	// hold this group — which is to say it cannot, so the page runs to
	// the group's upper edge.
	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 1))
	assertServedExactlyOnce(t, served, rows, sizes)
	if len(sizes) == 0 || sizes[0] != len(rows) {
		t.Errorf("first page = %v, want the whole %d-row group — a group is never split across a page edge",
			sizes, len(rows))
	}
	if store.maxRead <= len(rows) {
		t.Errorf("largest read asked of the store = %d, want more than the %d-row group — the completion re-read must outrun the group in one go",
			store.maxRead, len(rows))
	}
}

// TestHistory_ExactlyOnceUnderEitherSourceCollation is the property the
// spec now promises, checked against a model of the store rather than
// against a shape.
//
// The merge never compares `source`, because Go's byte order and the
// database's collation can disagree on it. The way to test that claim is
// to make the model disagree: run every drain twice, once with the
// source column ordered ascending and once DESCENDING, and require the
// same answer both times.
//
// What the reversed half catches, stated as the mutation that proves
// it rather than as a claim: order the merge's ties by Go `source` and
// resume at the last served row. That is exactly-once under the
// ascending model — Go's order and the database's agree there, so the
// last row of a group is also the database's last — and it serves rows
// twice under the reversed one. The ascending half alone passes it. Any
// design that reaches its resume point through a source comparison has
// that shape, including the "resume at the group's lowest source"
// alternative this endpoint deliberately does not use.
//
// It does NOT catch every source comparison. Ordering ties by source
// while still resuming PAST the group is harmless, because the cursor
// does not depend on where inside a group the page ended — which is
// the point of stepping past it.
func TestHistory_ExactlyOnceUnderEitherSourceCollation(t *testing.T) {
	t.Parallel()
	aqua, usdc := aquaUSDC(t)
	// Names whose byte order and reversed order disagree, and which
	// carry the `-` and `_` a non-C collation weighs oddly.
	sources := []string{"a-x", "a_x", "blend", "blend_emitter", "sdex", "soroswap-router", "zz", "m1"}
	txs := []string{"0a", "0b", "0c"}
	secs := []int64{10, 20}

	collations := []struct {
		name string
		less func(a, b string) bool
	}{
		{"ascending", func(a, b string) bool { return a < b }},
		{"descending", func(a, b string) bool { return a > b }},
	}
	drains := 0
	for _, coll := range collations {
		// ONE server, one store, re-pointed at each fixture.
		store := &orientedTradeStore{}
		ts := httpTestServer(t, v1.New(v1.Options{History: store}))
		// Same seed per collation, so the two runs see identical
		// fixtures and any difference is the collation alone.
		rng := rand.New(rand.NewSource(20260905))
		for iter := 0; iter < 250; iter++ {
			held := map[string]bool{}
			rows := make([]canonical.Trade, 0, 8)
			for k := 0; k < 1+rng.Intn(8); k++ {
				src := sources[rng.Intn(len(sources))]
				sec := secs[rng.Intn(len(secs))]
				tx := txs[rng.Intn(len(txs))]
				// The primary key is (source, ledger, tx_hash, op_index,
				// ts) and holds no asset column, so one identity exists
				// in ONE orientation only. A fixture that broke that
				// would be testing something the store cannot produce.
				id := fmt.Sprintf("%s|%d|%s", src, sec, tx)
				if held[id] {
					continue
				}
				held[id] = true
				if rng.Intn(2) == 0 {
					rows = append(rows, storedTrade(t, src, sec, tx, aqua, usdc, 1, 100))
					continue
				}
				rows = append(rows, storedTrade(t, src, sec, tx, usdc, aqua, 100, 1))
			}
			if len(rows) == 0 {
				continue
			}
			for limit := 1; limit <= 5; limit++ {
				store.reset(rows, coll.less)
				served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, limit))
				drains++

				if !assertServedExactlyOnce(t, served, rows, sizes) {
					t.Fatalf("collation=%s iter=%d limit=%d rows=%d", coll.name, iter, limit, len(rows))
				}
			}
		}
	}
	t.Logf("%d drains across both source collations, every row served exactly once", drains)
}

func TestHistory_SourceFilterRestrictsRows(t *testing.T) {
	rd := historySourceFixture()
	ts := httpTestServer(t, v1.New(v1.Options{History: rd}))
	base := ts.URL + "/v1/history?base=native&quote=fiat:USD&from=2026-02-25T00:00:00Z&to=2026-03-02T00:00:00Z"

	resp := mustGet(t, base+"&source=sdex")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := historyRowSources(t, resp)
	if len(got) == 0 {
		t.Fatal("no rows for source=sdex")
	}
	for _, s := range got {
		if s != "sdex" {
			t.Errorf("source=sdex returned a %q row", s)
		}
	}
	if rd.gotSource != "sdex" {
		t.Errorf("reader saw source %q, want sdex", rd.gotSource)
	}

	if all := historyRowSources(t, mustGet(t, base)); len(all) < 2 {
		t.Errorf("unfiltered request returned %d rows, want both sources", len(all))
	}
}

func TestHistory_SourceFilterValidation(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: historySourceFixture()}))
	for _, c := range []struct{ source, problem string }{
		{"nope-not-a-source", "unknown-source"},
		{"coingecko", "off-chain-source-filter"},
		{"binance", "off-chain-source-filter"},
	} {
		resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&source="+c.source)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("source=%s: status = %d, want 400", c.source, resp.StatusCode)
		}
		if !strings.Contains(string(body), c.problem) {
			t.Errorf("source=%s: body lacks %q: %s", c.source, c.problem, body)
		}
	}
}

func TestHistory_SourceFilterUnsupportedReader503(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{}}))
	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&source=sdex")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHistory_NonstandardDecimals_Normalizes proves /v1/history
// does not decline — it reads exclusively from raw trades (TradesInRangeAfter),
// so the per-row Price field is corrected instead.
func TestHistory_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	xlmUSD, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, err := canonical.NewPair(xlmUSD, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// base 100 * 10^9 (9dp), quote 250 * 10^7 (7dp fiat) → true price
	// 250/100 = 2.5.
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2_500_000_000)),
	}
	reader := &stubHistoryReader{trades: []canonical.Trade{trade}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/history?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (raw-trade path is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"2.5000000000"`) {
		t.Errorf("body missing normalized price 2.5000000000: %s", body)
	}
}
