package v1_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── /v1/oracle/lastprice ──────────────────────────────────────

func TestOracleLastPrice_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOracleLastPrice_MissingAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleLastPrice_InvalidAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=garbage")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleLastPrice_QueryingQuoteRejected400(t *testing.T) {
	// Asking for the price of fiat:USD itself is meaningless and
	// rejected the same way /v1/price's identity check works.
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleLastPrice_NotFound404(t *testing.T) {
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{}}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestOracleLastPrice_RedisVWAPFallback: when prices_1m has no native/fiat:USD row (XLM trades
// against USDC, and the stablecoin-proxy rewrite lives only in the Redis cache), SEP-40
// lastprice must fall through to the TriangulatedPriceLooker /v1/price uses, or it 404s.
func TestOracleLastPrice_RedisVWAPFallback(t *testing.T) {
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	looker := &stubTriangulatedPriceLooker{
		value:          "0.155",
		isTriangulated: false, // direct rewrite, no marker
		found:          true,
	}
	srv := v1.New(v1.Options{Prices: reader, Triangulated: looker})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.155" {
		t.Errorf("price = %q, want \"0.155\"", env.Data.Price)
	}
}

// TestOracleLastPrice_StablecoinFiatProxyFallback: when the Redis VWAP cache also misses but the
// operator declared classic USD pegs, SEP-40 lastprice walks the pegs and returns the rewritten
// X/<peg> snapshot, mirroring /v1/price, instead of 404ing for a priced asset.
func TestOracleLastPrice_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	pegSnap := v1.PriceSnapshot{
		AssetID:    "native",
		Quote:      usdcClassic.String(),
		Price:      "0.1626",
		PriceType:  "vwap",
		ObservedAt: v1.WireTime(time.Unix(1_770_000_000, 0).UTC()),
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + usdcClassic.String(): pegSnap,
		},
		sources: map[string][]string{
			"native/" + usdcClassic.String(): {"sdex"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (peg fallback should serve)", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.1626" {
		t.Errorf("price = %q, want \"0.1626\"", env.Data.Price)
	}
	// SEP-40 surface always quotes in fiat:USD; the wire `asset` is
	// just the queried asset, so the fact we proxied through USDC
	// shouldn't leak into the wire response.
	if env.Data.Asset != "native" {
		t.Errorf("asset = %q, want \"native\"", env.Data.Asset)
	}
}

// TestOracleXLastPrice_StablecoinFiatProxyFallback: the x_last_price?base=native&quote=fiat:USD
// cross-pair case needs the same stablecoin-proxy fallback as /v1/oracle/lastprice above.
func TestOracleXLastPrice_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	pegSnap := v1.PriceSnapshot{
		AssetID:    "native",
		Quote:      usdcClassic.String(),
		Price:      "0.1626",
		PriceType:  "vwap",
		ObservedAt: v1.WireTime(time.Unix(1_770_000_000, 0).UTC()),
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + usdcClassic.String(): pegSnap,
		},
		sources: map[string][]string{
			"native/" + usdcClassic.String(): {"sdex"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.1626" {
		t.Errorf("price = %q, want \"0.1626\"", env.Data.Price)
	}
}

// TestOracleLastPrice_FallbackSetsStaleFlag pins that any priceFallback branch (Redis VWAP /
// stablecoin proxy / fiat cross) sets flags.stale=true; the chain itself is the staleness signal,
// and stale=false would label degraded data fresh.
func TestOracleLastPrice_FallbackSetsStaleFlag(t *testing.T) {
	t.Run("redis vwap fallback", func(t *testing.T) {
		reader := &stubPriceReader{err: v1.ErrPriceNotFound}
		looker := &stubTriangulatedPriceLooker{value: "0.155", found: true}
		srv := v1.New(v1.Options{Prices: reader, Triangulated: looker})
		ts := startHTTPTest(t, srv.Handler())

		resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 via fallback", resp.StatusCode)
		}
		body, _ := readAll(resp)
		if !strings.Contains(body, `"stale":true`) {
			t.Errorf("redis-vwap fallback must set stale=true; body: %s", body)
		}
	})
	t.Run("stablecoin-proxy fallback", func(t *testing.T) {
		usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatalf("parse USDC: %v", err)
		}
		reader := &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				"native/" + usdcClassic.String(): {
					AssetID: "native", Quote: usdcClassic.String(),
					Price: "0.1626", PriceType: "vwap",
				},
			},
		}
		srv := v1.New(v1.Options{
			Prices:            reader,
			USDPeggedClassics: []canonical.Asset{usdcClassic},
		})
		ts := startHTTPTest(t, srv.Handler())

		resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 via peg fallback", resp.StatusCode)
		}
		body, _ := readAll(resp)
		if !strings.Contains(body, `"stale":true`) {
			t.Errorf("stablecoin-proxy fallback must set stale=true; body: %s", body)
		}
	})
}

// TestOracleLastPrice_AliasResolvesXLM pins that asset=native resolves a snapshot published under
// the crypto:XLM alias key (and vice versa), like handlePrice. The literal form alone would 404.
func TestOracleLastPrice_AliasResolvesXLM(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		// Only the crypto:XLM form is populated (CEX trades write it);
		// the literal native/fiat:USD key is absent.
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

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via crypto:XLM alias", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.12" {
		t.Errorf("price = %q, want \"0.12\" (resolved via crypto:XLM alias)", env.Data.Price)
	}
}

func TestOracleLastPrice_HappyPath(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID: "native", Quote: "fiat:USD",
				Price: "0.12", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
		sources: map[string][]string{
			"native/fiat:USD": {"soroswap", "phoenix"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data    v1.SEP40Price `json:"data"`
		Sources []string      `json:"sources"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Asset != "native" {
		t.Errorf("asset = %q", env.Data.Asset)
	}
	if env.Data.Price != "0.12" {
		t.Errorf("price = %q", env.Data.Price)
	}
	if !env.Data.Timestamp.Time().Equal(t0) {
		t.Errorf("timestamp = %v, want %v", env.Data.Timestamp, t0)
	}
	if len(env.Sources) != 2 {
		t.Errorf("sources len = %d, want 2", len(env.Sources))
	}
}

func TestOracleLastPrice_ReaderError500(t *testing.T) {
	reader := &stubPriceReader{err: errors.New("boom")}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset=native")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ─── /v1/oracle/x_last_price ───────────────────────────────────

func TestOracleXLastPrice_MissingBase400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleXLastPrice_MissingQuote400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleXLastPrice_IdentityPair400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleXLastPrice_NotFound404(t *testing.T) {
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{}}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestOracleXLastPrice_RedisVWAPFallback: when prices_1m has no row for the cross pair, x_last_price falls through to
// the same TriangulatedPriceLooker /v1/price uses, mirroring the lastprice fallback.
func TestOracleXLastPrice_RedisVWAPFallback(t *testing.T) {
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	looker := &stubTriangulatedPriceLooker{
		value:          "0.91",
		isTriangulated: true, // x_last_price is the cross-pair surface; triangulation is the headline use
		found:          true,
	}
	srv := v1.New(v1.Options{Prices: reader, Triangulated: looker})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.91" {
		t.Errorf("price = %q, want \"0.91\"", env.Data.Price)
	}
}

// TestOracleXLastPrice_AliasResolvesXLM pins that x_last_price(native, fiat:USD) resolves a snapshot published under the
// crypto:XLM alias key, like handlePrice's primary read.
func TestOracleXLastPrice_AliasResolvesXLM(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.12", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via crypto:XLM alias", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.12" {
		t.Errorf("price = %q, want \"0.12\" (resolved via crypto:XLM alias)", env.Data.Price)
	}
}

// TestOracleXLastPrice_FallbackSetsStaleFlag pins, on the
// cross-pair surface: a Redis-VWAP fallback must set stale=true.
func TestOracleXLastPrice_FallbackSetsStaleFlag(t *testing.T) {
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	looker := &stubTriangulatedPriceLooker{value: "0.91", isTriangulated: true, found: true}
	srv := v1.New(v1.Options{Prices: reader, Triangulated: looker})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("x_last_price fallback must set stale=true; body: %s", body)
	}
}

func TestOracleXLastPrice_InvalidBase400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=garbage&quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleXLastPrice_InvalidQuote400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=garbage")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleXLastPrice_ReaderError500(t *testing.T) {
	reader := &stubPriceReader{err: errors.New("boom")}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestOracleXLastPrice_NoReader503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOracleXLastPrice_HappyPath(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:EUR": {
				AssetID: "native", Quote: "fiat:EUR",
				Price: "0.10", PriceType: "vwap", ObservedAt: v1.WireTime(t0),
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:EUR")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Asset != "native" {
		t.Errorf("asset = %q, want native (the base)", env.Data.Asset)
	}
	if env.Data.Price != "0.10" {
		t.Errorf("price = %q, want 0.10", env.Data.Price)
	}
}

// ─── /v1/oracle/prices ─────────────────────────────────────────

func TestOraclePrices_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOraclePrices_MissingAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOraclePrices_BadRecords400(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	for _, raw := range []string{"0", "201", "abc", "-1"} {
		resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records="+raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("records=%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}
}

// TestOraclePrices_HappyPath exercises the SEP-40 prices() passthrough
// — verifies the response is an array of {price, timestamp} records,
// newest first, and that the records cap is honoured.
func TestOraclePrices_HappyPath(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		recent: map[string][]v1.PriceSnapshot{
			"native/fiat:USD": {
				{AssetID: "native", Quote: "fiat:USD", Price: "0.12", PriceType: "vwap", ObservedAt: v1.WireTime(t0)},
				{AssetID: "native", Quote: "fiat:USD", Price: "0.13", PriceType: "vwap", ObservedAt: v1.WireTime(t0.Add(-1 * time.Minute))},
				{AssetID: "native", Quote: "fiat:USD", Price: "0.14", PriceType: "vwap", ObservedAt: v1.WireTime(t0.Add(-2 * time.Minute))},
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("returned %d records, want 2 (records= cap)", len(env.Data))
	}
	if env.Data[0].Price != "0.12" {
		t.Errorf("data[0].price = %q, want newest-first 0.12", env.Data[0].Price)
	}
}

// TestOraclePrices_AliasResolvesXLM: only the crypto:XLM form has closed buckets (CEX trades write it), so
// recentClosedWithStablecoinFallback must loop aliases rather than read native/fiat:USD and return an empty array.
func TestOraclePrices_AliasResolvesXLM(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		recent: map[string][]v1.PriceSnapshot{
			"crypto:XLM/fiat:USD": {
				{AssetID: "crypto:XLM", Quote: "fiat:USD", Price: "0.12", PriceType: "vwap", ObservedAt: v1.WireTime(t0)},
			},
		},
	}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via crypto:XLM alias", resp.StatusCode)
	}
	var env struct {
		Data []v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("returned %d records, want 1 (resolved via crypto:XLM alias)", len(env.Data))
	}
	if env.Data[0].Price != "0.12" {
		t.Errorf("price = %q, want \"0.12\"", env.Data[0].Price)
	}
}

// TestOraclePrices_EmptyAsArray is the "no closed buckets yet"
// case — should return 200 with an empty array, not 404. Distinct
// from /v1/oracle/lastprice which 404s on a bare unknown asset.
func TestOraclePrices_EmptyAsArray(t *testing.T) {
	reader := &stubPriceReader{recent: map[string][]v1.PriceSnapshot{}}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.SEP40Price `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data == nil {
		t.Errorf("data = null, want []  — empty array != null per OpenAPI")
	}
	if len(env.Data) != 0 {
		t.Errorf("expected empty array, got %d entries", len(env.Data))
	}
}

// TestOraclePrices_StablecoinFiatProxyFallback: with no closed asset/fiat:USD buckets but declared USD pegs, the handler
// walks the pegs and returns rewritten asset/<peg> snapshots, as /v1/oracle/lastprice does, not an empty array.
func TestOraclePrices_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		// Literal native/fiat:USD missing. native/<USDC-classic>
		// returns three closed buckets.
		recent: map[string][]v1.PriceSnapshot{
			"native/" + usdcClassic.String(): {
				{AssetID: "native", Quote: usdcClassic.String(), Price: "0.1626", PriceType: "vwap", ObservedAt: v1.WireTime(t0)},
				{AssetID: "native", Quote: usdcClassic.String(), Price: "0.1625", PriceType: "vwap", ObservedAt: v1.WireTime(t0.Add(-1 * time.Minute))},
			},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (peg fallback should serve)", resp.StatusCode)
	}
	var env struct {
		Data  []v1.SEP40Price `json:"data"`
		Flags v1.Flags        `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d records via fallback, want 2", len(env.Data))
	}
	if env.Data[0].Price != "0.1626" {
		t.Errorf("data[0].price = %q, want \"0.1626\" (newest from peg)", env.Data[0].Price)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated should be true on peg fallback")
	}
	// SEP-40 surface always quotes in fiat:USD; the wire `asset` is
	// just the queried asset name — proxying through USDC shouldn't
	// leak into the wire.
	if env.Data[0].Asset != "native" {
		t.Errorf("data[0].asset = %q, want \"native\"", env.Data[0].Asset)
	}
}

// (errors import retained — used by other tests in this file.)
var _ = errors.New

// oracleFallbackCrossReader is the single-peg shape of the declared-peg XLM cross: USDC's SDEX book at 9.5 XLM per USDC
// and XLM's CEX market at 0.10, composing 0.95, with no fiat:USD bucket for USDC.
func oracleFallbackCrossReader() (*stubPriceReader, canonical.Asset) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		panic(err)
	}
	at := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			usdcClassic.String() + "/native": {
				AssetID: usdcClassic.String(), Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: v1.WireTime(at), WindowSeconds: 60,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: v1.WireTime(at), WindowSeconds: 60,
			},
		},
		sources: map[string][]string{
			usdcClassic.String() + "/native": {"sdex"},
			"crypto:XLM/fiat:USD":            {"coinbase"},
		},
	}
	return reader, usdcClassic
}

type oracleFlagsEnvelope struct {
	Data  v1.SEP40Price `json:"data"`
	Flags v1.Flags      `json:"flags"`
}

// TestOracleLastPrice_FallbackCrossSetsTriangulated pins that SEP-40 lastprice carries priceFallback's triangulated
// verdict: a peg served through its XLM cross is composed, and the envelope must say so as /v1/price does.
func TestOracleLastPrice_FallbackCrossSetsTriangulated(t *testing.T) {
	reader, usdcClassic := oracleFallbackCrossReader()
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/lastprice?asset="+usdcClassic.String())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 via the XLM cross. Body: %s", resp.StatusCode, body)
	}
	var env oracleFlagsEnvelope
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want the composed 0.9500000000", env.Data.Price)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	if !env.Flags.Stale {
		t.Errorf("flags.stale = false, want true — every fallback answer is stale on this surface")
	}
}

// TestOracleXLastPrice_FallbackCrossSetsTriangulated is the cross-pair
// twin of the test above: x_last_price(USDC-G…, fiat:USD) served through
// the declared-peg XLM cross carries triangulated=true.
func TestOracleXLastPrice_FallbackCrossSetsTriangulated(t *testing.T) {
	reader, usdcClassic := oracleFallbackCrossReader()
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base="+usdcClassic.String()+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 via the XLM cross. Body: %s", resp.StatusCode, body)
	}
	var env oracleFlagsEnvelope
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want the composed 0.9500000000", env.Data.Price)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	if !env.Flags.Stale {
		t.Errorf("flags.stale = false, want true — every fallback answer is stale on this surface")
	}
}

// TestOraclePrices_NonstandardDecimals_Normalizes: the SEP-40 prices passthrough reads the raw prices_1m CAGG and must
// be guarded and normalized like the /v1/price closed-bucket path.
func TestOraclePrices_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		recent: map[string][]v1.PriceSnapshot{key: {{
			AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
			PriceType: "vwap", ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
		}}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("oracle prices row not normalized: %s", body)
	}
}

func TestOracleXLastPrice_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
				PriceType: "vwap", ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/oracle/x_last_price?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/oracle/x_last_price not normalized (want 4132.0000000000): %s", body)
	}
}

func TestOraclePrices_WithheldPegLegAnswersWithheldNotEmpty(t *testing.T) {
	usdc, _ := oraclePegs(t)
	reader := &stubPriceReader{
		errByPair: map[string]error{"native/" + usdc.String(): v1.ErrPriceWithheld},
	}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 price-withheld (a refused peg leg is not a miss): %s", status, body)
	}
	if !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
}

// The destructive branch's boundary: a withheld verdict on one peg is a
// verdict on THAT pair, so a later peg that serves still wins, exactly as
// walkUSDPegs does for lastprice.
func TestOraclePrices_WithheldPegLegDoesNotHideALaterServingPeg(t *testing.T) {
	usdc, usdt := oraclePegs(t)
	reader := &stubPriceReader{
		errByPair: map[string]error{"native/" + usdc.String(): v1.ErrPriceWithheld},
		recent:    map[string][]v1.PriceSnapshot{"native/" + usdt.String(): pegSeries(usdt.String(), "0.1631")},
	}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc, usdt}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the second peg", resp.StatusCode)
	}
	var env struct {
		Data  []v1.SEP40Price `json:"data"`
		Flags v1.Flags        `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].Price != "0.1631" {
		t.Fatalf("data = %+v, want the USDT peg's single 0.1631 record", env.Data)
	}
}

func TestOraclePrices_PegFallbackIsStale(t *testing.T) {
	usdc, _ := oraclePegs(t)
	cases := []struct {
		name   string
		recent map[string][]v1.PriceSnapshot
		want   bool
	}{
		{"direct fiat:USD series", map[string][]v1.PriceSnapshot{"native/fiat:USD": pegSeries("fiat:USD", "0.1620")}, false},
		{"peg-proxied series", map[string][]v1.PriceSnapshot{"native/" + usdc.String(): pegSeries(usdc.String(), "0.1626")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:            &stubPriceReader{recent: tc.recent},
				USDPeggedClassics: []canonical.Asset{usdc},
			})
			ts := startHTTPTest(t, srv.Handler())
			resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data  []v1.SEP40Price `json:"data"`
				Flags v1.Flags        `json:"flags"`
			}
			mustDecode(t, resp, &env)
			if len(env.Data) != 1 {
				t.Fatalf("got %d records, want 1", len(env.Data))
			}
			if env.Flags.Stale != tc.want || env.Flags.Triangulated != tc.want {
				t.Errorf("flags stale=%v triangulated=%v, want both %v",
					env.Flags.Stale, env.Flags.Triangulated, tc.want)
			}
		})
	}
}

// TestOraclePrices_DeclaredPegSACTwinNeverProbesItsOwnClassicForm: the
// self-pair guard on the surface that walks the pegs with its own reader call.
func TestOraclePrices_DeclaredPegSACTwinNeverProbesItsOwnClassicForm(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &recordingRecentReader{}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset="+pegAliasUSDCSAC)
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	reader.mu.Lock()
	asked := append([]string(nil), reader.calls...)
	reader.mu.Unlock()
	if callIndex(asked, pegAliasUSDCSAC+"/fiat:USD") < 0 {
		t.Errorf("the requested pair was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// The SEP-40 single-price surfaces read the same closed prices_1m bucket as /v1/price, so for a frozen pair
// lastprice/x_last_price must serve the held last-known-good, not the refused newest bucket. These assert the VALUE.

const (
	xlmUSD       = "crypto:XLM/fiat:USD"
	heldUSD      = "0.1011"
	movedUSD     = "0.1377"
	lastPriceURL = "/v1/oracle/lastprice?asset=crypto:XLM"
	xLastURL     = "/v1/oracle/x_last_price?base=crypto:XLM&quote=fiat:USD"
)

func movedUSDBucketReader() *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmUSD: {AssetID: "crypto:XLM", Quote: "fiat:USD", Price: movedUSD, PriceType: "vwap", WindowSeconds: 60},
		},
		sources: map[string][]string{xlmUSD: {"kraken", "coinbase"}},
	}
}

func TestOracleSEP40_FrozenPairServesLastKnownGood(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{xlmUSD: true},
				Triangulated: lkgPairs{xlmUSD + "/300": heldUSD},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			if strings.Contains(body, movedUSD) {
				t.Fatalf("frozen pair served the bucket the freeze refused (%s): %s", movedUSD, body)
			}
			for _, want := range []string{`"price":"` + heldUSD + `"`, `"frozen":true`, `"stale":true`, `"single_source":true`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, "kraken") {
				t.Errorf("sources still credit the refused bucket's venues: %s", body)
			}
		})
	}
}

func TestOracleSEP40_FrozenPairWithNothingHeldRefuses(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{xlmUSD: true},
				Triangulated: lkgPairs{},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", status, body)
			}
			if strings.Contains(body, movedUSD) {
				t.Fatalf("refusal leaked the withheld bucket: %s", body)
			}
		})
	}
}

// The substitution discards the bucket, so pin where it must not fire.
func TestOracleSEP40_UnfrozenPairServesTheBucketUntouched(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{},
				Triangulated: lkgPairs{xlmUSD + "/300": heldUSD},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			for _, want := range []string{`"price":"` + movedUSD + `"`, `"stale":false`, `"frozen_checked":true`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, heldUSD) || strings.Contains(body, `"frozen":true`) {
				t.Errorf("unfrozen pair must not be touched by the freeze path: %s", body)
			}
		})
	}
}

// /v1/oracle/prices walks declared USD pegs when asset/fiat:USD is empty. A peg leg the withholding gate refused must
// give the price-withheld 404 like sibling surfaces, not 200 []; a peg-served series is stamped stale and triangulated.

func oraclePegs(t *testing.T) (usdc, usdt canonical.Asset) {
	t.Helper()
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usdt, err = canonical.ParseAsset("USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatal(err)
	}
	return usdc, usdt
}

func pegSeries(quote, price string) []v1.PriceSnapshot {
	at := time.Unix(1_770_000_000, 0).UTC()
	return []v1.PriceSnapshot{{
		AssetID: "native", Quote: quote, Price: price, PriceType: "vwap", ObservedAt: v1.WireTime(at),
	}}
}

// A declared peg is an assumption, not a record: SEP-40's answer for "no
// record" is None, and SEP40Price has no field to tell a declaration from
// a print. Both point reads refuse it where /v1/price serves it as "peg".
func TestOracleSEP40PointReads_RefuseADeclaredPeg(t *testing.T) {
	usdc, _ := oraclePegs(t)
	srv := v1.New(v1.Options{
		Prices:            &stubPriceReader{err: v1.ErrPriceNotFound},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset="+usdc.String()+"&quote=fiat:USD")
	if status != http.StatusOK || !strings.Contains(body, `"price_type":"peg"`) {
		t.Fatalf("precondition: /v1/price must serve the declaration as price_type peg, got %d: %s", status, body)
	}
	for _, path := range []string{
		"/v1/oracle/lastprice?asset=" + usdc.String(),
		"/v1/oracle/lastprice?asset=crypto:USDC",
		"/v1/oracle/x_last_price?base=" + usdc.String() + "&quote=fiat:USD",
		"/v1/oracle/x_last_price?base=crypto:USDC&quote=fiat:USD",
		"/v1/oracle/x_last_price?base=crypto:EURC&quote=fiat:EUR",
	} {
		status, body := getBody(t, ts.URL+path)
		if status != http.StatusNotFound || !strings.Contains(body, "errors/price-not-found") {
			t.Errorf("%s: status = %d, want 404 price-not-found (a declaration is not a SEP-40 record): %s", path, status, body)
		}
	}
}
