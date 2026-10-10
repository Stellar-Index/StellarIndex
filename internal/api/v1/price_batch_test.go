package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestPriceBatch_RejectsBadRequests covers the GET and POST refusals:
// no reader wired is 503; malformed or over-cap input is 400.
func TestPriceBatch_RejectsBadRequests(t *testing.T) {
	// 101 distinct GET ids (limit fires after dedupe) and 1001 distinct
	// POST ids (POST cap is 1000), as unique fiat:XYZ codes so dedupe
	// cannot collapse them.
	getIDs := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		getIDs = append(getIDs, "fiat:"+string(rune('A'+i%26))+string(rune('A'+i/26))+"X")
	}
	postIDs := make([]string, 0, 1001)
	for i := 0; i < 1001; i++ {
		postIDs = append(postIDs, fmt.Sprintf("fiat:%c%c%c", 'A'+i%26, 'A'+(i/26)%26, 'A'+(i/26/26)%26))
	}
	cases := []struct {
		name     string
		noReader bool
		query    string // GET when post is empty
		post     string
		want     int
	}{
		{name: "GET no reader", noReader: true, query: "?asset_ids=native", want: http.StatusServiceUnavailable},
		{name: "POST no reader", noReader: true, post: `{"asset_ids":["native"]}`, want: http.StatusServiceUnavailable},
		{name: "GET missing asset_ids", query: "", want: http.StatusBadRequest},
		{name: "GET empty after trim", query: "?asset_ids=,,,", want: http.StatusBadRequest},
		{name: "GET too many assets", query: "?asset_ids=" + strings.Join(getIDs, ","), want: http.StatusBadRequest},
		{name: "GET invalid asset", query: "?asset_ids=native,@@@", want: http.StatusBadRequest},
		{name: "GET invalid quote", query: "?asset_ids=native&quote=garbage", want: http.StatusBadRequest},
		{name: "GET identity pair", query: "?asset_ids=fiat:USD&quote=fiat:USD", want: http.StatusBadRequest},
		{name: "GET asset_ids and pairs both", query: "?asset_ids=native&pairs=native", want: http.StatusBadRequest},
		{name: "POST invalid quote", post: `{"asset_ids":["native"],"quote":"garbage"}`, want: http.StatusBadRequest},
		{name: "POST not json", post: `not-json`, want: http.StatusBadRequest},
		{name: "POST truncated json", post: `{`, want: http.StatusBadRequest},
		{name: "POST int ids", post: `{"asset_ids":[1,2]}`, want: http.StatusBadRequest},
		{name: "POST unknown field", post: `{"asset_ids":["native"],"quote":"x","unknown":42}`, want: http.StatusBadRequest},
		{name: "POST empty array", post: `{"asset_ids":[]}`, want: http.StatusBadRequest},
		{name: "POST too many assets", post: `{"asset_ids":["` + strings.Join(postIDs, `","`) + `"]}`, want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Prices: &stubPriceReader{}}
			if tc.noReader {
				opts = v1.Options{}
			}
			ts := startHTTPTest(t, v1.New(opts).Handler())
			var resp *http.Response
			if tc.post != "" {
				resp = mustPostJSON(t, ts.URL+"/v1/price/batch", tc.post)
			} else {
				resp = mustGet(t, ts.URL+"/v1/price/batch"+tc.query)
			}
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestPriceBatch_OmitsMissingAssets(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID: "native", Quote: "fiat:USD",
				Price: "0.12", PriceType: "last_trade", ObservedAt: v1.WireTime(t0),
			},
		},
		sources: map[string][]string{
			"native/fiat:USD": {"soroswap"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	// Two requested, one known. Response array should have one row.
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native,fiat:EUR&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data    []v1.PriceSnapshot `json:"data"`
		Sources []string           `json:"sources"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d entries, want 1", len(env.Data))
	}
	if env.Data[0].AssetID != "native" {
		t.Errorf("data[0].asset_id = %q, want native", env.Data[0].AssetID)
	}
	if len(env.Sources) != 1 || env.Sources[0] != "soroswap" {
		t.Errorf("sources = %v, want [soroswap]", env.Sources)
	}
}

// TestPriceBatch_AliasResolvesXLM pins, on the batch surface:
// asset_ids=native must resolve a snapshot published under the
// crypto:XLM alias key, exactly like handlePrice's primary read.
// Querying the literal form only would silently
// drop the row while /v1/price?asset=native serves fresh.
func TestPriceBatch_AliasResolvesXLM(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		// Only the crypto:XLM form is populated; native is absent.
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.12", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
		sources: map[string][]string{"crypto:XLM/fiat:USD": {"binance"}},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d entries, want 1 (crypto:XLM alias should resolve)", len(env.Data))
	}
	if env.Data[0].Price != "0.12" {
		t.Errorf("data[0].price = %q, want 0.12 (resolved via crypto:XLM alias)", env.Data[0].Price)
	}
}

// TestPriceBatch_RedisFallbackForRewrittenPair — when the
// PriceReader returns ErrPriceNotFound (typical for an aggregator-
// rewritten pair like XLM/fiat:USD whose literal form isn't in
// prices_1m), the batch path falls through to the Redis VWAP cache.
// Without this fix the batch endpoint silently omitted the headline
// pair even though /v1/price served it just fine.
func TestPriceBatch_RedisFallbackForRewrittenPair(t *testing.T) {
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	looker := &stubTriangulatedPriceLooker{
		value:          "0.157384502084",
		isTriangulated: false,
		found:          true,
	}
	srv := v1.New(v1.Options{Prices: reader, Triangulated: looker})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d entries, want 1 (Redis fallback should populate)", len(env.Data))
	}
	if env.Data[0].Price != "0.157384502084" {
		t.Errorf("data[0].price = %q, want 0.157384502084", env.Data[0].Price)
	}
}

// TestPriceBatch_StablecoinFallback exercises the X / fiat:USD →
// X / <USD-pegged classic> retry inside fetchBatchRow. Mirrors
// /v1/price's tryStablecoinFiatProxy.
//
// Without it, a batch path that inlined only the Redis-VWAP and
// fiat-cross-rate fallbacks would drop an asset_id whose only price
// comes via the stablecoin-proxy chain (e.g. USDT-G… / USDC-G… on a
// deployment with [aggregate].enable_stablecoin_fiat_proxy=false):
// /v1/price would return 200 while the batch envelope omits it.
func TestPriceBatch_StablecoinFallback(t *testing.T) {
	usdc, err := canonical.NewClassicAsset(
		"USDC",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	)
	if err != nil {
		t.Fatal(err)
	}
	usdt, err := canonical.NewClassicAsset(
		"USDT",
		"GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V",
	)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		// USDT/fiat:USD missing — the literal pair never has rows on
		// mainnet because no on-chain trades quote in fiat:USD.
		// USDT/USDC present — provides the proxy value.
		snapshots: map[string]v1.PriceSnapshot{
			usdt.String() + "/" + usdc.String(): {
				AssetID: usdt.String(), Quote: usdc.String(),
				Price: "1.001", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
		sources: map[string][]string{
			usdt.String() + "/" + usdc.String(): {"sdex"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+usdt.String()+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d entries, want 1 (stablecoin proxy should populate)", len(env.Data))
	}
	if env.Data[0].Price != "1.001" {
		t.Errorf("data[0].price = %q, want 1.001", env.Data[0].Price)
	}
	if env.Data[0].AssetID != usdt.String() {
		t.Errorf("data[0].asset_id = %q, want %q", env.Data[0].AssetID, usdt.String())
	}
}

func TestPriceBatch_DeduplicatesIds(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID: "native", Quote: "fiat:USD",
				Price: "0.12", PriceType: "last_trade", ObservedAt: v1.WireTime(t0),
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	// `native` appears 3 times; result must be a single row.
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native,native,native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Errorf("got %d entries, want 1 (after dedupe)", len(env.Data))
	}
}

func TestPriceBatch_StaleFlagOR(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD":   {AssetID: "native", Quote: "fiat:USD", Price: "0.12", PriceType: "last_trade", ObservedAt: v1.WireTime(t0)},
			"fiat:EUR/fiat:USD": {AssetID: "fiat:EUR", Quote: "fiat:USD", Price: "1.08", PriceType: "last_trade", ObservedAt: v1.WireTime(t0)},
		},
		stale: map[string]bool{
			"fiat:EUR/fiat:USD": true,
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native,fiat:EUR&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Flags v1.Flags `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if !env.Flags.Stale {
		t.Errorf("expected envelope.flags.stale=true (any item stale)")
	}
}

func TestPriceBatch_ReaderError500(t *testing.T) {
	reader := &stubPriceReader{err: errors.New("boom")}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// A server-side request deadline (middleware.RequestTimeout) firing mid-
// batch must NOT be treated as a client abort. lookupPriceBatch must not
// test `ctx.Err() != nil` after wg.Wait() and return without writing
// anything on ANY done context — including DeadlineExceeded — or net/http
// answers with an implicit 200 and an empty body: an authoritative-
// looking empty result for what is actually a blown server budget.
//
// This holds the reader open past a deliberately tiny RequestTimeout so
// the blanket deadline fires while the client is still connected (not
// cancelled), then releases it — mirroring the CanceledRequestWritesNothing
// / ExpiredRequestDeadlineUpgrades500To503 split in
// envelope_test.go, at the batch endpoint.
func TestPriceBatch_ExpiredRequestDeadlineReturns503NotBlank200(t *testing.T) {
	// No startedCh: `native` walks multiple aliases (assetAliases), each
	// alias a SEPARATE sequential LatestPrice call once released — a
	// buffered startedCh drained only once would fill back up on a later
	// alias's send and deadlock. A flat sleep before closing release is
	// simpler and sufficient: it only needs to outlast both the request
	// reaching the reader and the 20ms deadline below.
	release := make(chan struct{})
	reader := &stubPriceReader{
		err:       errors.New("boom"),
		releaseCh: release,
	}
	srv := v1.New(v1.Options{Prices: reader, RequestTimeout: 20 * time.Millisecond})
	ts := startHTTPTest(t, srv.Handler())

	done := make(chan *http.Response, 1)
	go func() {
		done <- mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	}()

	// Let the 20ms RequestTimeout deadline actually pass before releasing
	// the reader, so the handler observes DeadlineExceeded, not a live
	// context racing the release.
	time.Sleep(200 * time.Millisecond)
	close(release)

	resp := <-done
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 (body %q) — a blown server-side request deadline must not read as "+
			"an authoritative empty result; want a retryable 503", body)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body %q)", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Errorf("empty body on a %d — client cannot distinguish this from success", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("missing Retry-After on the deadline-upgraded 503")
	}
}

// ─── POST /v1/price/batch ──────────────────────────────────────

func mustPostJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPriceBatchPost_OmitsMissingAssets(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {AssetID: "native", Quote: "fiat:USD", Price: "0.12", PriceType: "last_trade", ObservedAt: v1.WireTime(t0)},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/price/batch",
		`{"asset_ids":["native","fiat:EUR"],"quote":"fiat:USD"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Errorf("got %d entries, want 1 (one missing)", len(env.Data))
	}
}

func TestPriceBatchPost_LargeBatchAccepted(t *testing.T) {
	// 200 distinct Classic assets, all known. Verifies the POST
	// ceiling is genuinely larger than GET's 100 (200 > 100, would
	// have 400'd on the GET path) and that the shared core logic
	// handles the larger batch without bottlenecking.
	//
	// Synthesise alphanumeric 4-char codes "BAAA"..."BHRR" (200
	// unique combinations) paired with one well-known issuer; that
	// passes both validateClassicAssetCode and the strkey CRC check.
	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	t0 := time.Unix(1_770_000_000, 0).UTC()
	snapshots := make(map[string]v1.PriceSnapshot, 200)
	ids := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		// Codes start with 'B' to avoid colliding with real fiat
		// allow-list (USD/EUR/...) at the prefix; remaining 3 chars
		// vary i across A..Z.
		code := fmt.Sprintf("B%c%c%c",
			'A'+i/(26*26)%26,
			'A'+(i/26)%26,
			'A'+i%26)
		id := code + "-" + issuer
		ids = append(ids, id)
		snapshots[id+"/fiat:JPY"] = v1.PriceSnapshot{
			AssetID: id, Quote: "fiat:JPY",
			Price: "150", PriceType: "last_trade", ObservedAt: v1.WireTime(t0),
		}
	}
	reader := &stubPriceReader{snapshots: snapshots}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	body := `{"asset_ids":["` + strings.Join(ids, `","`) + `"],"quote":"fiat:JPY"}`
	resp := mustPostJSON(t, ts.URL+"/v1/price/batch", body)
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, buf.String())
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 200 {
		t.Errorf("got %d entries, want 200", len(env.Data))
	}
}

func TestPriceBatchPost_DefaultQuoteFiatUSD(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {AssetID: "native", Quote: "fiat:USD", Price: "0.12", PriceType: "last_trade", ObservedAt: v1.WireTime(t0)},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	// No quote field — should default to fiat:USD.
	resp := mustPostJSON(t, ts.URL+"/v1/price/batch", `{"asset_ids":["native"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].Quote != "fiat:USD" {
		t.Errorf("default quote not applied; got data=%+v", env.Data)
	}
}

// /v1/price/batch accepts `pairs=` as
// alias for `asset_ids=` so CG-style callers using `pairs` reach
// the endpoint without a 400 detour.
func TestPriceBatch_PairsAcceptedAsAssetIdsAlias(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/price/batch?pairs=native")
	if resp.StatusCode == http.StatusBadRequest {
		t.Errorf("pairs= alias rejected (400); want it accepted as asset_ids= alias")
	}
}

// TestPriceBatch_EchoesRequestedAssetNotStoreAlias pins that a batch row's
// asset_id is the id the CLIENT asked for, not whichever XLM alias the
// store happened to be keyed under.
//
// Guards against readPriceWithAliases returning the snapshot built from
// the winning alias, so `?asset_ids=native,crypto:XLM` would produce TWO rows
// both stamped "crypto:XLM". Since the batch route omits misses rather
// than returning null rows, `asset_id` is the only mapping the wire shape
// supports — a client keying the response by it would lose `native`
// entirely.
func TestPriceBatch_EchoesRequestedAssetNotStoreAlias(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	// Only the crypto:XLM key is populated; `native` must resolve through
	// the alias walk and still echo back as `native`.
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.17", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native,crypto:XLM&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.PriceSnapshot `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2 (both aliases resolve)", len(env.Data))
	}
	ids := []string{env.Data[0].AssetID, env.Data[1].AssetID}
	if ids[0] == ids[1] {
		t.Fatalf("both rows echo asset_id %q — a client keying the response "+
			"by asset_id loses one of the two requested assets", ids[0])
	}
	for _, want := range []string{"native", "crypto:XLM"} {
		if ids[0] != want && ids[1] != want {
			t.Errorf("no row echoes requested asset_id %q; got %v", want, ids)
		}
	}
}

// TestPriceBatch_NonstandardDecimals_Normalizes — the batch endpoint's
// direct closed-bucket read goes through the same normalization as the
// single-asset handler.
func TestPriceBatch_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
			PriceType: "vwap", ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("batch row not normalized: %s", body)
	}
}

// TestPriceBatch_ChargesOneTokenPerID is the rate-limit regression.
// One rate-limit token must not buy a whole batch: a 40-id GET left 99
// of 100 tokens, so the per-minute ceiling bounded HTTP requests while
// the work behind them was the caller's to choose. A batch must cost
// its id count.
func TestPriceBatch_ChargesOneTokenPerID(t *testing.T) {
	ts, reader := newBatchLimitedServer(t, 100)
	url := ts.URL + "/v1/price/batch?asset_ids=" + strings.Join(batchIDs(40), ",")

	// Each probe that reads the remainder spends one token of its own.
	for i, wantRemaining := range []int{60, 19} {
		resp := mustGet(t, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("batch %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		if got := remainingBeforeProbe(t, resp, ts.URL+batchProbe); got != wantRemaining {
			t.Fatalf("batch %d: X-RateLimit-Remaining = %d, want %d (40 ids must cost 40 tokens)",
				i+1, got, wantRemaining)
		}
	}

	// 80 spent on batches, 2 on probes; a third 40-id batch does not fit
	// in the remaining 18.
	served := reader.calls.Load()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third batch: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("429 Content-Type = %q, want application/problem+json", ct)
	}
	if got := reader.calls.Load(); got != served {
		t.Fatalf("a denied batch still resolved %d price(s); the charge must land BEFORE the fan-out", got-served)
	}
}

// TestPriceBatch_POSTChargesPerID pins the variant the findings name:
// the JSON-body route whose 1000-id ceiling is what made one token
// worth a thousand resolutions. Against the deployed 6000/min anonymous
// budget a full batch must leave 5000, not 5999.
func TestPriceBatch_POSTChargesPerID(t *testing.T) {
	ts, _ := newBatchLimitedServer(t, 6000)

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "5000" {
		t.Fatalf("X-RateLimit-Remaining after a 1000-id POST = %q, want 5000", got)
	}
}

// TestPriceBatch_OverCeilingBatchSpendsTheWindow pins the decision for
// a batch priced above the caller's whole budget (1000 ids against the
// 60/min default). It is served into an untouched window — refusing it
// in every window would make the documented 1000-id ceiling unusable
// behind a Retry-After that never comes true — and it takes the entire
// window with it, so the next request of any size is denied.
func TestPriceBatch_OverCeilingBatchSpendsTheWindow(t *testing.T) {
	ts, _ := newBatchLimitedServer(t, 60)

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first batch in a fresh window: status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0 (the batch must spend the whole window)", got)
	}

	resp = mustGet(t, ts.URL+"/v1/price/batch?asset_ids=fiat:EUR")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request after an over-ceiling batch: status = %d, want 429", resp.StatusCode)
	}
}

// TestPriceBatch_ChargeFollowsTheWorkDone pins what the cost is a count
// OF: the de-duplicated ids the handler will resolve. Forty copies of
// one id are one resolution and cost one token; a request rejected as
// malformed does no resolution and costs only the base token every
// request pays.
func TestPriceBatch_ChargeFollowsTheWorkDone(t *testing.T) {
	ts, reader := newBatchLimitedServer(t, 100)

	dupes := strings.TrimSuffix(strings.Repeat("fiat:EUR,", 40), ",")
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+dupes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := remainingBeforeProbe(t, resp, ts.URL+batchProbe); got != 99 {
		t.Fatalf("40 duplicates of one id: X-RateLimit-Remaining = %d, want 99", got)
	}

	// 101 ids on the GET route is a 400 (ceiling 100): base token only,
	// on top of the probe's.
	before := reader.calls.Load()
	resp = mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+strings.Join(batchIDs(101), ","))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "97" {
		t.Fatalf("a rejected batch: X-RateLimit-Remaining = %q, want 97 (base token only)", got)
	}
	if got := reader.calls.Load(); got != before {
		t.Fatalf("a 400 resolved %d price(s)", got-before)
	}
}

// TestPriceBatch_UnlimitedDeploymentIsUncharged: with no limiter wired
// there is no account to charge, and the batch is served as before.
func TestPriceBatch_UnlimitedDeploymentIsUncharged(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
		t.Fatalf("X-RateLimit-Remaining = %q on a deployment with no limiter", got)
	}
}

// TestPriceBatch_MetersOneUsageUnitPerID pins that the monthly meter
// must not count HTTP requests, or a 1000-id POST /v1/price/batch cost one unit
// of a quota sold in price lookups. Each batch must advance BOTH the
// billable total MonthlyQuota reads and the per-endpoint detail counter
// by its de-duplicated id count, and a single-price call stays at one.
func TestPriceBatch_MetersOneUsageUnitPerID(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	counter := usage.New(rdb, usage.WithClock(func() time.Time { return now }))

	subject := auth.Subject{
		Identifier: auth.AccountIdentifier("acme"),
		KeyID:      "kid_units",
		Tier:       auth.TierAPIKey,
	}
	attach := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	srv := v1.New(v1.Options{
		Prices:       &countingPriceReader{},
		Auth:         attach,
		UsageTracker: middleware.UsageTracker(counter, nil),
	})
	ts := startHTTPTest(t, srv.Handler())

	post := postBatch(t, ts.URL, batchIDs(250))
	_ = post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("POST batch status = %d, want 200", post.StatusCode)
	}
	// 40 distinct ids plus a duplicate: the unit count is the work done.
	ids := batchIDs(40)
	ids = append(ids, batchIDs(1)...)
	get := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+strings.Join(ids, ","))
	_ = get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET batch status = %d, want 200", get.StatusCode)
	}
	// UsageTracker's counter writes run on the shared after-response pool,
	// not inline, so a read right after the requests return must
	// wait for them to land first.
	if !middleware.AfterResponseDrainForTest(2 * time.Second) {
		t.Fatal("after-response pool did not drain in time")
	}

	key := middleware.UsageKeyForSubject(subject)
	got, err := counter.MonthToDate(context.Background(), key)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 290 {
		t.Fatalf("billable month-to-date = %d, want 290 (250 + 40 price lookups)", got)
	}

	rows, err := counter.ScanDetail(context.Background(), []string{"2026-09-15"})
	if err != nil {
		t.Fatalf("ScanDetail: %v", err)
	}
	var detail int64
	for _, r := range rows {
		if r.Subject == key && r.Endpoint == "/v1/price/batch" && r.Class == usage.ClassOK {
			detail += r.Count
		}
	}
	if detail != 290 {
		t.Fatalf("detail ok units for /v1/price/batch = %d, want 290 — the rollup's billable sum must equal the quota counter", detail)
	}
}

// TestPriceBatch_WithheldIDsNamedOnEnvelope: a withheld row is omitted
// from data (unchanged) AND named on `withheld`, so a batch caller can
// tell "we decline to publish" from "we have nothing" — before, both
// were a silent omission.
func TestPriceBatch_WithheldIDsNamedOnEnvelope(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	n, withheld, _ := batchWithheld(t, srv, "native")
	if n != 0 {
		t.Errorf("withheld row served in data (%d rows)", n)
	}
	if !reflect.DeepEqual(withheld, []string{"native"}) {
		t.Errorf("withheld = %v, want [native]", withheld)
	}
}

// TestPriceBatch_FallbackWithheldIDsNamed: a verdict reached inside the
// fallback chain (flagged issuer answering from the VWAP cache) is
// reported the same way, not folded into "no data".
func TestPriceBatch_FallbackWithheldIDsNamed(t *testing.T) {
	base := fallbackFlaggedBase(t)
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "0.00723"},
		Scam:         &fallbackScamGate{withheld: map[string]bool{base.String(): true}},
	})
	n, withheld, _ := batchWithheld(t, srv, base.String())
	if n != 0 || !reflect.DeepEqual(withheld, []string{base.String()}) {
		t.Errorf("rows = %d withheld = %v, want 0 rows and [%s]", n, withheld, base)
	}
}

// TestPriceBatch_PlainMissNotWithheld: the discriminator must not fire
// on a genuine miss — no data means no `withheld` member at all.
func TestPriceBatch_PlainMissNotWithheld(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}})
	n, withheld, present := batchWithheld(t, srv, "native")
	if n != 0 || present {
		t.Errorf("rows = %d withheld present = %v (%v), want 0 rows and no withheld member", n, present, withheld)
	}
}

// /v1/price/batch stamps the same flag over the same read, so it owes
// the same value: the frozen row carries the held value, and a frozen
// row with nothing held is omitted (the batch contract's "no price")
// rather than shipped as the refused bucket.
func TestPriceBatch_FrozenRowCarriesLastKnownGood(t *testing.T) {
	reader := movedBucketReader()
	reader.snapshots["crypto:BTC/fiat:GBP"] = v1.PriceSnapshot{Price: "51234.5", PriceType: "vwap", WindowSeconds: 60}
	reader.sources["crypto:BTC/fiat:GBP"] = []string{"kraken", "coinbase"}

	t.Run("held value served", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:       reader,
			Freeze:       frozenPairs{xlmGBP: true},
			Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
		})
		ts := startHTTPTest(t, srv.Handler())
		status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=crypto:XLM,crypto:BTC&quote=fiat:GBP")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, body)
		}
		if strings.Contains(body, movedBucket) {
			t.Fatalf("batch served the refused bucket: %s", body)
		}
		for _, want := range []string{`"price":"` + heldLKG + `"`, `"price":"51234.5"`, `"frozen":true`} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
		}
	})

	t.Run("nothing held omits the row", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:       reader,
			Freeze:       frozenPairs{xlmGBP: true},
			Triangulated: lkgPairs{},
		})
		ts := startHTTPTest(t, srv.Handler())
		status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=crypto:XLM,crypto:BTC&quote=fiat:GBP")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, body)
		}
		if strings.Contains(body, movedBucket) || strings.Contains(body, `"asset_id":"crypto:XLM"`) {
			t.Fatalf("frozen row with nothing held must be omitted: %s", body)
		}
		if !strings.Contains(body, `"price":"51234.5"`) {
			t.Errorf("the healthy row must still serve: %s", body)
		}
	})
}

func TestPriceBatch_FreezeOnRequestedLiteralOnlyKeepsTheHealthyAliasRow(t *testing.T) {
	for name, cache := range literalOnlyFreezeShapes() {
		t.Run(name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       healthyAliasReader(),
				Freeze:       frozenPairs{nativeGBP: true},
				Triangulated: cache,
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:GBP")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			if strings.Contains(body, literalHeld24h) {
				t.Fatalf("the literal's held value replaced the healthy alias bucket: %s", body)
			}
			for _, want := range []string{
				`"asset_id":"native"`, // the row must not be omitted
				`"price":"` + healthyAliasBucket + `"`,
				`"window_seconds":60`,
				`"stale":false`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, `"frozen":true`) {
				t.Errorf("an unfrozen served row must not flag the batch frozen: %s", body)
			}
		})
	}
}

// Envelope flags are OR over per-row signals, like Stale: one frozen row
// sets frozen and single_source on the batch.
func TestPriceBatch_FrozenORedAcrossRows(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD":   {Price: "0.07", PriceType: "vwap"},
			"fiat:EUR/fiat:USD": {Price: "1.10", PriceType: "vwap"},
		},
		sources: map[string][]string{
			"native/fiat:USD":   {"sdex", "soroswap"},
			"fiat:EUR/fiat:USD": {"sdex", "soroswap"},
		},
	}
	// Only the EUR row freezes; it serves its held last-known-good (see
	// TestPrice_FrozenSetsBothFlags) while the other keeps its closed bucket.
	held := &stubTriangulatedPriceLooker{value: "1.08", found: true}
	_, body := priceGet(t, v1.Options{Prices: reader, Freeze: &batchFreezeLooker{frozenForBase: "EUR"}, Triangulated: held},
		"/v1/price/batch?asset_ids=native,fiat:EUR&quote=fiat:USD")
	checkBody(t, body, []string{`"price":"1.08"`, `"price":"0.07"`, `"frozen":true`, `"single_source":true`}, []string{`"1.10"`})
}

// TestPriceBatch_Withheld_RowOmitted — the batch wire contract omits a
// withheld row exactly like a miss (no 500, no fallback serve).
func TestPriceBatch_Withheld_RowOmitted(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (batch omits, never fails, on withheld rows)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"data":[]`) {
		t.Errorf("withheld row must be omitted from the batch: %s", body)
	}
}

// batchWithheld GETs one batch and returns its data length and withheld list.
func batchWithheld(t *testing.T, srv *v1.Server, ids string) (int, []string, bool) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+ids+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var data []json.RawMessage
	_ = json.Unmarshal(env["data"], &data)
	raw, present := env["withheld"]
	var withheld []string
	if present {
		_ = json.Unmarshal(raw, &withheld)
	}
	return len(data), withheld, present
}
