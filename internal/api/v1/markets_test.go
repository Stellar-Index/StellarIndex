package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubMarketsReader struct {
	pairs   []v1.Market
	nextCur string
	err     error

	// lastDistinctOrder captures the order arg the handler passed
	// on the most recent DistinctPairsExt call. Lets tests assert
	// the handler's default-resolution behaviour.
	lastDistinctOrder timescale.MarketsOrder

	// lastAsset captures the asset filter the handler passed to
	// AssetMarkets — the spelling that actually reaches SQL.
	lastAsset string

	// PairMarket stub state.
	pair      v1.Market
	pairFound bool
	pairErr   error

	// Sparkline batch stub state.
	hourVolumes map[string]map[int]string
	sparkErr    error
	sparkPairs  [][2]string

	// firstPairs captures the pairs passed to FirstTradeBatch.
	firstPairs [][2]string
}

func (r *stubMarketsReader) DistinctPairsExt(_ context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.lastDistinctOrder = order
	if r.err != nil {
		return nil, "", r.err
	}
	return r.pairs, r.nextCur, nil
}

func (r *stubMarketsReader) SourceMarkets(_ context.Context, _, _ string, _ int, _ timescale.MarketsOrder) ([]v1.Market, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	return r.pairs, r.nextCur, nil
}

func (r *stubMarketsReader) AssetMarkets(_ context.Context, asset, _ string, _ int, _ timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.lastAsset = asset
	if r.err != nil {
		return nil, "", r.err
	}
	return r.pairs, r.nextCur, nil
}

func (r *stubMarketsReader) AllPools(_ context.Context, _ timescale.PoolsFilter, _ string, _ int, _ timescale.MarketsOrder) ([]v1.Pool, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	out := make([]v1.Pool, len(r.pairs))
	for i, m := range r.pairs {
		out[i] = v1.Pool{
			Source:        "test",
			Base:          m.Base,
			Quote:         m.Quote,
			LastTradeAt:   m.LastTradeAt,
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
			LastPrice:     m.LastPrice,
		}
	}
	return out, r.nextCur, nil
}

// GetPairsVolumeHistory24hBatch mirrors the producer: every requested key gets exactly 24 hourly points, oldest first,
// gaps COALESCEd to '0'. hourVolumes seeds non-zero buckets by hour index (0 = oldest).
func (r *stubMarketsReader) GetPairsVolumeHistory24hBatch(_ context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	r.sparkPairs = pairs
	if r.sparkErr != nil {
		return nil, r.sparkErr
	}
	newest := time.Now().UTC().Truncate(time.Hour)
	out := make(map[string][]timescale.PairVolumePoint, len(pairs))
	for _, p := range pairs {
		key := p[0] + "|" + p[1]
		series := make([]timescale.PairVolumePoint, 24)
		for h := range series {
			vol := "0"
			if v, ok := r.hourVolumes[key][h]; ok {
				vol = v
			}
			series[h] = timescale.PairVolumePoint{Hour: newest.Add(time.Duration(h-23) * time.Hour), VolumeUSD: vol}
		}
		out[key] = series
	}
	return out, nil
}

func (r *stubMarketsReader) PairMarket(_ context.Context, _ canonical.Asset, _ canonical.Asset) (v1.Market, bool, error) {
	if r.pairErr != nil {
		return v1.Market{}, false, r.pairErr
	}
	return r.pair, r.pairFound, nil
}

func TestMarkets_EmptyWhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Market `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 0 {
		t.Errorf("want empty list, got %d", len(env.Data))
	}
}

func TestMarkets_NilSliceFromReaderMarshalsAsEmptyArray(t *testing.T) {
	// Regression: reader returning (nil, "", nil) must not surface as
	// "data": null — OpenAPI MarketsEnvelope.data is `type: array`.
	reader := &stubMarketsReader{pairs: nil, nextCur: ""}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !bytes.Contains([]byte(body), []byte(`"data":[]`)) {
		t.Errorf("expected \"data\":[] in body, got: %s", body)
	}
}

func TestMarkets_ReturnsPairsWithCursor(t *testing.T) {
	ts1 := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubMarketsReader{
		pairs: []v1.Market{
			{Base: "native", Quote: "fiat:USD", LastTradeAt: v1.WireTime(ts1), TradeCount24h: 42},
			{Base: "native", Quote: "fiat:EUR", LastTradeAt: v1.WireTime(ts1), TradeCount24h: 10},
		},
		nextCur: "next-opaque",
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.Market `json:"data"`
		Pagination struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 {
		t.Fatalf("got %d markets, want 2", len(env.Data))
	}
	if env.Data[0].Quote != "fiat:USD" {
		t.Errorf("quote = %q", env.Data[0].Quote)
	}
	if env.Data[0].TradeCount24h != 42 {
		t.Errorf("trade_count_24h = %d, want 42", env.Data[0].TradeCount24h)
	}
	if env.Pagination.Next != "next-opaque" {
		t.Errorf("next cursor = %q, want next-opaque", env.Pagination.Next)
	}
}

// TestMarkets_IncludeSparkline pins ?include=sparkline: 24 hourly buckets per row, oldest to newest, each row with its own
// pair's zero-filled series, and volume_usd as an exact decimal string.
func TestMarkets_IncludeSparkline(t *testing.T) {
	ts1 := time.Unix(1_772_000_000, 0).UTC()
	const bigVol = "123456789012345678901234.5"
	reader := &stubMarketsReader{
		pairs: []v1.Market{
			{Base: "native", Quote: "fiat:USD", LastTradeAt: v1.WireTime(ts1)},
			{Base: "native", Quote: "fiat:EUR", LastTradeAt: v1.WireTime(ts1)},
		},
		hourVolumes: map[string]map[int]string{
			"native|fiat:USD": {0: "12.5", 23: bigVol},
			"native|fiat:EUR": {7: "3"},
		},
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	var env struct {
		Data []v1.Market `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	mustDecode(t, resp, &env)
	if reader.sparkPairs != nil || len(env.Data) != 2 || env.Data[0].VolumeHistory24h != nil {
		t.Fatalf("without ?include=sparkline: batch pairs=%v rows=%d history=%v; want no batch call and no history",
			reader.sparkPairs, len(env.Data), env.Data)
	}

	resp = mustGet(t, ts.URL+"/v1/markets?include=sparkline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d markets, want 2", len(env.Data))
	}
	want := map[string]map[int]string{
		"fiat:USD": {0: "12.5", 23: bigVol},
		"fiat:EUR": {7: "3"},
	}
	for _, m := range env.Data {
		assertSparkline(t, m.Quote, m.VolumeHistory24h, want[m.Quote])
	}
}

func assertSparkline(t *testing.T, label string, got []v1.MarketVolumeBucket, nonZero map[int]string) {
	t.Helper()
	if len(got) != 24 {
		t.Fatalf("%s: volume_history_24h has %d entries, want 24", label, len(got))
	}
	for i, b := range got {
		want, ok := nonZero[i]
		if !ok {
			want = "0"
		}
		if b.VolumeUSD != want {
			t.Errorf("%s[%d].volume_usd = %q, want %q", label, i, b.VolumeUSD, want)
		}
		if i > 0 {
			if step := time.Time(b.Hour).Sub(time.Time(got[i-1].Hour)); step != time.Hour {
				t.Errorf("%s[%d].hour is %v after its predecessor, want 1h (oldest → newest)", label, i, step)
			}
		}
	}
}

// TestMarkets_SparklineFailureIsBestEffort: a failed sparkline batch
// still serves the markets page, just without the series.
func TestMarkets_SparklineFailureIsBestEffort(t *testing.T) {
	reader := &stubMarketsReader{
		pairs:    []v1.Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 4}},
		sparkErr: errors.New("sparkline boom"),
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?include=sparkline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Market `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(reader.sparkPairs) != 1 {
		t.Fatalf("sparkline batch saw %v, want the one served pair", reader.sparkPairs)
	}
	if len(env.Data) != 1 || env.Data[0].TradeCount24h != 4 || env.Data[0].VolumeHistory24h != nil {
		t.Errorf("rows = %+v; want the one market with no volume_history_24h", env.Data)
	}
}

// swrStaleMarketsUpstream is a MarketsReader whose DistinctPairsExt succeeds on the cold fill
// then errors once failNow is set, so inside the real CachedMarketsReader it drives the SWR
// stale-serve path. Atomics keep `go test -race` clean under the background refresh.
type swrStaleMarketsUpstream struct {
	*stubMarketsReader
	calls   atomic.Int64
	failNow atomic.Bool
}

func (u *swrStaleMarketsUpstream) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	u.calls.Add(1)
	if u.failNow.Load() {
		return nil, "", errors.New("swr markets refresh boom")
	}
	return u.stubMarketsReader.DistinctPairsExt(ctx, cursor, limit, order)
}

// TestMarkets_SWRStaleServeStampsHonestFlags: an expired entry served while refresh keeps failing must report
// flags.stale=true and as_of = the rows' real observation time; a fresh serve stays stale:false.
func TestMarkets_SWRStaleServeStampsHonestFlags(t *testing.T) {
	const ttl = 40 * time.Millisecond
	up := &swrStaleMarketsUpstream{
		stubMarketsReader: &stubMarketsReader{
			pairs: []v1.Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 7}},
		},
	}
	cache := v1.NewCachedMarketsReader(up, ttl)
	srv := v1.New(v1.Options{Markets: cache})
	ts := httpTestServer(t, srv)

	type envelope struct {
		Data  []v1.Market `json:"data"`
		AsOf  time.Time   `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}

	// (a) Fresh serve — cold fill. stale:false, as_of ≈ now (the fill
	// time), data present.
	coldStart := time.Now()
	var fresh envelope
	mustDecode(t, mustGet(t, ts.URL+"/v1/markets"), &fresh)
	if len(fresh.Data) != 1 {
		t.Fatalf("fresh serve: got %d rows, want 1", len(fresh.Data))
	}
	if fresh.Flags.Stale {
		t.Errorf("fresh serve: flags.stale = true, want false")
	}
	if fresh.AsOf.Before(coldStart) || fresh.AsOf.After(time.Now()) {
		t.Errorf("fresh serve: as_of = %v, want within [%v, now] (the fill time)", fresh.AsOf, coldStart)
	}

	// Arm refresh failure, then let the entry expire so the next request
	// takes the SWR stale-serve path with a failing background refresh.
	up.failNow.Store(true)
	time.Sleep(2 * ttl)

	// (b) Stale serve. stale:true, as_of = the OLD fill time (equal to
	// the fresh serve's as_of), NOT now.
	beforeStale := time.Now()
	var stale envelope
	mustDecode(t, mustGet(t, ts.URL+"/v1/markets"), &stale)
	if len(stale.Data) != 1 {
		t.Fatalf("stale serve: got %d rows, want the old rows served (1)", len(stale.Data))
	}
	if !stale.Flags.Stale {
		t.Errorf("stale serve: flags.stale = false, want true (expired entry served while refresh fails)")
	}
	if !stale.AsOf.Equal(fresh.AsOf) {
		t.Errorf("stale serve: as_of = %v, want the OLD fill time %v (the served data's real observation time)", stale.AsOf, fresh.AsOf)
	}
	if !stale.AsOf.Before(beforeStale) {
		t.Errorf("stale serve: as_of = %v not before request time %v — as_of must be the old fill time, not now()", stale.AsOf, beforeStale)
	}

	// Confirm the SWR path (not a fresh hit) actually ran: the failing
	// background refresh was attempted.
	refreshed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if up.calls.Load() >= 2 {
			refreshed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !refreshed {
		t.Fatalf("expected a background refresh attempt (calls>=2); got %d", up.calls.Load())
	}
}

// swrStalePoolsUpstream serves one pool on the cold fill, then errors once
// failNow is set, so the cache serves the expired entry.
type swrStalePoolsUpstream struct {
	*stubMarketsReader
	failNow atomic.Bool
}

func (u *swrStalePoolsUpstream) AllPools(ctx context.Context, f timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Pool, string, error) {
	if u.failNow.Load() {
		return nil, "", errors.New("swr pools refresh boom")
	}
	return u.stubMarketsReader.AllPools(ctx, f, cursor, limit, order)
}

// /v1/pools served from an expired cache entry whose refresh keeps failing
// must report flags.stale=true and the rows' original fill time as as_of.
func TestPools_SWRStaleServeStampsHonestFlags(t *testing.T) {
	const ttl = 40 * time.Millisecond
	up := &swrStalePoolsUpstream{
		stubMarketsReader: &stubMarketsReader{
			pairs: []v1.Market{{Base: "native", Quote: "fiat:USD"}},
		},
	}
	srv := v1.New(v1.Options{Markets: v1.NewCachedMarketsReader(up, ttl)})
	ts := httpTestServer(t, srv)

	type envelope struct {
		Data  []v1.Pool `json:"data"`
		AsOf  time.Time `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}

	var fresh envelope
	mustDecode(t, mustGet(t, ts.URL+"/v1/pools"), &fresh)
	if len(fresh.Data) != 1 || fresh.Flags.Stale {
		t.Fatalf("fresh serve: rows=%d stale=%v, want 1 row, stale=false", len(fresh.Data), fresh.Flags.Stale)
	}

	up.failNow.Store(true)
	time.Sleep(2 * ttl)

	beforeStale := time.Now()
	var stale envelope
	mustDecode(t, mustGet(t, ts.URL+"/v1/pools"), &stale)
	if len(stale.Data) != 1 {
		t.Fatalf("stale serve: got %d rows, want the old rows served (1)", len(stale.Data))
	}
	if !stale.Flags.Stale {
		t.Errorf("stale serve: flags.stale = false, want true")
	}
	if !stale.AsOf.Equal(fresh.AsOf) || !stale.AsOf.Before(beforeStale) {
		t.Errorf("stale serve: as_of = %v, want the original fill time %v (before %v)", stale.AsOf, fresh.AsOf, beforeStale)
	}
}

// TestMarkets_LastTradeAtVsBucketCloseAt pins that rows ship distinct `last_trade_at` (minute-precise) and
// `bucket_close_at` (daily bucket-start); last_trade_at must not carry midnight, which skews client staleness.
func TestMarkets_LastTradeAtVsBucketCloseAt(t *testing.T) {
	// 2026-05-25T14:23:00Z — mid-bucket, NOT midnight.
	lastTrade := time.Date(2026, 5, 25, 14, 23, 0, 0, time.UTC)
	bucketClose := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	reader := &stubMarketsReader{
		pairs: []v1.Market{
			{
				Base: "native", Quote: "fiat:USD",
				LastTradeAt: v1.WireTime(lastTrade), BucketCloseAt: v1.WireTime(bucketClose), TradeCount24h: 42,
			},
		},
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	// Both timestamps must appear on the wire — daily-bucket midnight
	// AND the actual mid-bucket trade time.
	if !bytes.Contains([]byte(body), []byte(`"last_trade_at":"2026-05-25T14:23:00Z"`)) {
		t.Errorf("expected `last_trade_at` mid-bucket value in body, got: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte(`"bucket_close_at":"2026-05-25T00:00:00Z"`)) {
		t.Errorf("expected `bucket_close_at` midnight value in body, got: %s", body)
	}
	// Decode straight from the captured body so the assertions
	// above (grep) and below (parse) share one read of resp.Body —
	// mustDecode would re-read the now-drained reader and fail EOF.
	var env struct {
		Data []v1.Market `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("want 1 row, got %d", len(env.Data))
	}
	if !env.Data[0].LastTradeAt.Time().Equal(lastTrade) {
		t.Errorf("last_trade_at = %v, want %v", env.Data[0].LastTradeAt, lastTrade)
	}
	if !env.Data[0].BucketCloseAt.Time().Equal(bucketClose) {
		t.Errorf("bucket_close_at = %v, want %v", env.Data[0].BucketCloseAt, bucketClose)
	}
	if env.Data[0].LastTradeAt.Time().Equal(env.Data[0].BucketCloseAt.Time()) {
		t.Errorf("F-0065 regression: last_trade_at == bucket_close_at — the two should be distinct values")
	}
}

func TestMarkets_InvalidLimit400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	for _, bad := range []string{"0", "501", "-1", "xyz"} {
		resp := mustGet(t, ts.URL+"/v1/markets?limit="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestMarkets_ReaderError500(t *testing.T) {
	reader := &stubMarketsReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestMarkets_UnknownSource400: `?source=` naming a source absent from `external.Registry`
// returns 400, not an empty page, which a typo would make look like "this source has no trades".
// Failing fast is the contract on every listing handler here.
func TestMarkets_UnknownSource400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?source=fake-source")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/unknown-source" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestMarkets_KnownSource200 — guards the inverse: a registered
// on-chain source passes the validation gate.
func TestMarkets_KnownSource200(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?source=sdex")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestMarkets_AssetFilter_HappyPath pins `?asset=` dispatching to AssetMarkets. The stub returns the same rows for every
// method, so it asserts 200 + non-empty body; per-asset filtering is the storage layer's job.
func TestMarkets_AssetFilter_HappyPath(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{
		pairs: []v1.Market{{Base: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Quote: "native"}},
	}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Market `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Errorf("want 1 row, got %d", len(env.Data))
	}
}

// TestMarkets_InvalidAsset400 — unparseable asset_id values 400
// rather than silently returning empty. Mirrors the
// silent-empty-page guard family applied to `?source=`.
func TestMarkets_InvalidAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	// "USDC" alone (no issuer) is not a canonical asset_id.
	resp := mustGet(t, ts.URL+"/v1/markets?asset=USDC")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-asset-id" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestMarkets_SourceAndAssetTogether400 — combining the two
// filters has no defined semantics on the storage side; reject up
// front rather than silently picking one.
func TestMarkets_SourceAndAssetTogether400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets?source=sdex&asset=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/conflicting-filters" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestPools_AssetFilter_HappyPath — `/v1/pools?asset=<id>` is the
// OR-shape filter (base = X OR quote = X) that lets asset-detail
// surfaces fetch every pool touching the asset in one request.
func TestPools_AssetFilter_HappyPath(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{
		pairs: []v1.Market{{Base: "native", Quote: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}},
	}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestPools_InvalidAsset400 — silent-empty-page guard family.
func TestPools_InvalidAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?asset=USDC")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-asset-id" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestMarkets_DefaultOrderIsVolumeDesc pins the default order, 24h USD volume descending. An
// alphabetical default would surface spam tokens (`0-…`, `0TAX-…`) atop every cold listing.
// Callers paginating in lex order can still pass `?order_by=pair`.
func TestMarkets_DefaultOrderIsVolumeDesc(t *testing.T) {
	t.Run("no order_by → volume desc", func(t *testing.T) {
		reader := &stubMarketsReader{}
		srv := v1.New(v1.Options{Markets: reader})
		ts := httpTestServer(t, srv)

		resp := mustGet(t, ts.URL+"/v1/markets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if reader.lastDistinctOrder != timescale.MarketsOrderVolume24hDesc {
			t.Errorf("default order = %v, want MarketsOrderVolume24hDesc", reader.lastDistinctOrder)
		}
	})
	t.Run("explicit order_by=pair → MarketsOrderPair", func(t *testing.T) {
		reader := &stubMarketsReader{}
		srv := v1.New(v1.Options{Markets: reader})
		ts := httpTestServer(t, srv)

		resp := mustGet(t, ts.URL+"/v1/markets?order_by=pair")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if reader.lastDistinctOrder != timescale.MarketsOrderPair {
			t.Errorf("explicit pair order = %v, want MarketsOrderPair", reader.lastDistinctOrder)
		}
	})
	t.Run("explicit order_by=volume_24h_usd_desc → MarketsOrderVolume24hDesc", func(t *testing.T) {
		reader := &stubMarketsReader{}
		srv := v1.New(v1.Options{Markets: reader})
		ts := httpTestServer(t, srv)

		resp := mustGet(t, ts.URL+"/v1/markets?order_by=volume_24h_usd_desc")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if reader.lastDistinctOrder != timescale.MarketsOrderVolume24hDesc {
			t.Errorf("explicit volume order = %v, want MarketsOrderVolume24hDesc", reader.lastDistinctOrder)
		}
	})
}

// TestPools_AssetAndBaseTogether400 — asset (OR) + base/quote
// (AND) is rejected; combining the two filter shapes has no
// well-defined semantics.
func TestPools_AssetAndBaseTogether400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?asset=native&base=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/conflicting-filters" {
		t.Errorf("Type = %q", p.Type)
	}
}

// TestPools_InvalidBase400 / TestPools_InvalidQuote400: malformed base/quote filters must 400 up front rather than reach
// the trades-hypertable scan, mirroring the `?asset=` guard.
func TestPools_InvalidBase400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?base=USDC") // no issuer → not canonical
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-asset-id" {
		t.Errorf("Type = %q, want invalid-asset-id", p.Type)
	}
}

func TestPools_InvalidQuote400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?quote=USDC")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-asset-id" {
		t.Errorf("Type = %q, want invalid-asset-id", p.Type)
	}
}

// TestPools_ValidBaseQuote200 — a well-formed base/quote pair passes the
// new guard (guards against the validation rejecting legitimate input).
func TestPools_ValidBaseQuote200(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pools?base=native&quote=USDC-"+testG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func (r *stubMarketsReader) FirstTradeBatch(_ context.Context, pairs [][2]string) (map[string]time.Time, error) {
	r.firstPairs = pairs
	out := make(map[string]time.Time, len(pairs))
	for _, p := range pairs {
		out[p[0]+"|"+p[1]] = time.Unix(1_600_000_000, 0).UTC().Truncate(24 * time.Hour)
	}
	return out, nil
}

// TestMarkets_SourceFilterOmitsPairWideEnrichments: both enrichment
// readers are cross-venue, so a ?source= row — whose headline figures
// are that venue's own — must not carry them.
func TestMarkets_SourceFilterOmitsPairWideEnrichments(t *testing.T) {
	vol := "10"
	reader := &stubMarketsReader{
		pairs:       []v1.Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 2, Volume24hUSD: &vol}},
		hourVolumes: map[string]map[int]string{"native|fiat:USD": {23: "610"}},
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	var env struct {
		Data []v1.Market `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/markets?include=sparkline,inception")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].VolumeHistory24h == nil || env.Data[0].FirstTradeAt == nil {
		t.Fatalf("unfiltered: rows=%+v; want sparkline and first_trade_at attached", env.Data)
	}

	// The stub hands out its own slice, which the first request enriched in place.
	reader.pairs = []v1.Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 2, Volume24hUSD: &vol}}
	reader.sparkPairs, reader.firstPairs = nil, nil
	resp = mustGet(t, ts.URL+"/v1/markets?source=sdex&include=sparkline,inception")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1", len(env.Data))
	}
	if reader.sparkPairs != nil || reader.firstPairs != nil {
		t.Errorf("source-filtered: sparkline batch=%v inception batch=%v; want neither pair-wide reader called",
			reader.sparkPairs, reader.firstPairs)
	}
	if m := env.Data[0]; m.VolumeHistory24h != nil || m.FirstTradeAt != nil {
		t.Errorf("source-filtered row carries pair-wide enrichments: history=%v first_trade_at=%v", m.VolumeHistory24h, m.FirstTradeAt)
	}
}

// The ?asset= filter must reach SQL in its canonical spelling. ParseAsset accepts bare
// "XLM"/"NATIVE" and the Horizon-style "CODE:ISSUER", but the stores compare the canonical form
// with `=`, so discarding the parsed value would serve a 200 with an empty list for those aliases.
func TestMarkets_AssetFilterIsCanonicalised(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"XLM", "native"},
		{"NATIVE", "native"},
		{"native", "native"},
		{
			"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		},
	} {
		t.Run(tc.in, func(t *testing.T) {
			reader := &stubMarketsReader{}
			srv := v1.New(v1.Options{Markets: reader})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+"/v1/markets?asset="+url.QueryEscape(tc.in))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if reader.lastAsset != tc.want {
				t.Errorf("reader received asset %q, want canonical %q — an accepted "+
					"alias spelling reaches SQL verbatim and matches nothing",
					reader.lastAsset, tc.want)
			}
		})
	}
}

// TestMarkets_CachedRows_IncludeEnrichmentDoesNotLeakAcrossRequests: ?include=sparkline,inception must not write
// enrichment onto the cache entry's rows, since `include` is not in the cache key.
func TestMarkets_CachedRows_IncludeEnrichmentDoesNotLeakAcrossRequests(t *testing.T) {
	ts, up := sharedRowsServer(t)

	plainBefore, _ := sharedRowsGet(t, ts.URL+"/v1/markets")
	enriched, _ := sharedRowsGet(t, ts.URL+"/v1/markets?include=sparkline,inception")
	if !strings.Contains(enriched, `"volume_history_24h"`) || !strings.Contains(enriched, `"first_trade_at"`) {
		t.Fatalf("opt-in request did not carry the enrichment (test is vacuous): %s", enriched)
	}
	plainAfter, _ := sharedRowsGet(t, ts.URL+"/v1/markets")

	if strings.Contains(plainAfter, `"volume_history_24h"`) || strings.Contains(plainAfter, `"first_trade_at"`) {
		t.Errorf("enrichment leaked into a request that did not opt in: %s", plainAfter)
	}
	if plainAfter != plainBefore {
		t.Errorf("plain page changed after an ?include= request\nbefore: %s\n after: %s", plainBefore, plainAfter)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (all three requests must share one cache entry)", got)
	}
}

// TestMarkets_NonstandardDecimals_NormalizesLastPrice — /v1/markets'
// last_price (prices_1d/prices_1m raw ratio) was never guarded; now
// corrected per row. The unflagged sibling row must stay byte-identical.
func TestMarkets_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	cleanPrice := "0.1242"
	reader := &stubMarketsReader{pairs: []v1.Market{
		{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
		{Base: "native", Quote: classicUSDC, LastPrice: &cleanPrice},
	}}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged market row not normalized: %s", body)
	}
	if !strings.Contains(body, `"last_price":"0.1242"`) {
		t.Errorf("7dp market row must be byte-identical: %s", body)
	}
}

// TestPools_NonstandardDecimals_NormalizesLastPrice — same fix on
// /v1/pools (pools_per_source_1h bucket_last_price).
func TestPools_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	reader := &stubMarketsReader{pairs: []v1.Market{
		{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
	}}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged pool row not normalized: %s", body)
	}
}

// TestListingsWithholdScamFlaggedLastPrice pins /v1/markets, /v1/pools and /v1/pairs to the withholding decision of
// /v1/price, /v1/vwap and /v1/twap: a directory-flagged issuer on either leg still lists but ships no last_price.
func TestListingsWithholdScamFlaggedLastPrice(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	flaggedPrice, quoteLegPrice, cleanPrice := "1.5", "0.25", "2.5"
	rows := []v1.Market{
		{Base: fallbackFlaggedAsset, Quote: "native", LastPrice: &flaggedPrice, TradeCount24h: 3},
		{Base: "native", Quote: fallbackFlaggedAsset, LastPrice: &quoteLegPrice, TradeCount24h: 4},
		{Base: "native", Quote: usdc, LastPrice: &cleanPrice, TradeCount24h: 5},
	}
	gate := &fallbackScamGate{withheld: map[string]bool{fallbackFlaggedAsset: true}}
	reader := &stubMarketsReader{pairs: rows, pair: rows[0], pairFound: true}
	srv := v1.New(v1.Options{Markets: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	check := func(t *testing.T, path string, wantRows int) {
		t.Helper()
		resp := mustGet(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		var env struct {
			Data []v1.Market `json:"data"`
		}
		mustDecode(t, resp, &env)
		if len(env.Data) != wantRows {
			t.Fatalf("%s: got %d rows, want %d — withholding hides the price, never the market", path, len(env.Data), wantRows)
		}
		for _, m := range env.Data {
			flagged := m.Base == fallbackFlaggedAsset || m.Quote == fallbackFlaggedAsset
			switch {
			case flagged && m.LastPrice != nil:
				t.Errorf("%s: %s/%s last_price = %q, want withheld (null) — issuer is directory-flagged", path, m.Base, m.Quote, *m.LastPrice)
			case !flagged && (m.LastPrice == nil || *m.LastPrice != cleanPrice):
				t.Errorf("%s: %s/%s last_price = %v, want %q — an unflagged market must keep its price", path, m.Base, m.Quote, m.LastPrice, cleanPrice)
			}
			if m.TradeCount24h == 0 {
				t.Errorf("%s: %s/%s trade_count_24h = 0 — activity figures are not prices and must survive", path, m.Base, m.Quote)
			}
		}
	}
	t.Run("markets", func(t *testing.T) { check(t, "/v1/markets", 3) })
	t.Run("pools", func(t *testing.T) { check(t, "/v1/pools", 3) })
	t.Run("pairs", func(t *testing.T) {
		check(t, "/v1/pairs?base="+fallbackFlaggedAsset+"&quote=native", 1)
	})
	for _, surface := range []string{"markets", "pools", "pairs"} {
		if !slices.Contains(gate.surfaces, surface) {
			t.Errorf("scam gate was never consulted with surface %q (asked: %v)", surface, gate.surfaces)
		}
	}
}
