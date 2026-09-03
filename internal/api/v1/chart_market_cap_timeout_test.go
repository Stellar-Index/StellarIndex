package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// errChartReadBroke is a plain read failure carrying no deadline signal,
// so the deadline and non-deadline halves below are told apart by the
// request context alone.
var errChartReadBroke = errors.New("prices_1d: broke")

// stallingMarketCapHistory blocks its price-series read until the caller's
// context is done, then returns that context's error — how a cold
// prices_1d scan behaves when a deadline beats it. Everything else is the
// ordinary stub, so only the market-cap price leg stalls.
type stallingMarketCapHistory struct{ *stubHistoryReader }

func (stallingMarketCapHistory) HistoryPointsInRange(
	ctx context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int,
) ([]v1.HistoryPoint, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestChart_MarketCap_RequestDeadlineIs503NotEmptySeries pins the wire
// answer when the blanket request deadline fires inside
// /v1/chart?price_type=market_cap.
//
// Pre-fix the market-cap legs mapped ANY read error, deadline included,
// to emptyMarketCapSeries at HTTP 200 with Flags{} — no stale marker, no
// error. An empty series is syntactically valid, so a caller renders
// "market cap $0" for an asset holding real supply and cannot tell that
// from a genuine no-data window: the same wrong-answer-with-full-
// confidence failure as the bodyless 200 on
// /v1/lending/pools/{pool}/reserves, and just as invisible to any
// 5xx-based availability signal.
//
// RequestTimeout is set below every per-handler budget so the deadline
// the reader observes is the blanket one — the case no per-handler
// arithmetic can reach.
func TestChart_MarketCap_RequestDeadlineIs503NotEmptySeries(t *testing.T) {
	srv := v1.New(v1.Options{
		History: stallingMarketCapHistory{&stubHistoryReader{}},
		Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: time.Now().UTC().Truncate(24 * time.Hour), Circulating: big.NewInt(1_000_0000000)},
		}},
		VerifiedCurrencies: newTestCatalogue(t),
		RequestTimeout:     150 * time.Millisecond,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	body, _ := readAll(resp)
	if resp.StatusCode == http.StatusOK {
		var env struct {
			Data  v1.ChartSeries `json:"data"`
			Flags v1.Flags       `json:"flags"`
		}
		_ = json.Unmarshal([]byte(body), &env)
		t.Fatalf("status = 200 with %d points and stale=%v — a blown request deadline must never be "+
			"answered with a market-cap series a caller will read as fact",
			len(env.Data.Points), env.Flags.Stale)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a request deadline is retryable capacity): %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "chart-timeout") {
		t.Errorf("expected the `chart-timeout` problem type in the body, got: %s", body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestChart_MarketCap_ReadFailureFlagsStale covers the non-deadline half
// of the same degrade: a plain read failure still serves the empty series
// (the shape callers depend on), but flags.stale now says the series is
// empty because the read failed — not because the asset has no market cap.
func TestChart_MarketCap_ReadFailureFlagsStale(t *testing.T) {
	srv := v1.New(v1.Options{
		History:            &stubHistoryReader{pointsErr: errChartReadBroke},
		Supply:             &stubSupplyLooker{},
		VerifiedCurrencies: newTestCatalogue(t),
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the empty-series degrade is the documented shape)", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Points) != 0 {
		t.Fatalf("points = %d, want 0", len(env.Data.Points))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a market-cap series emptied by a READ FAILURE — unflagged, " +
			"it claims the asset has no market cap, which is a different fact")
	}
}

// stallingFXHistory blocks until the request budget expires, the way a
// slow fx_quotes read does, and returns the context's own error.
type stallingFXHistory struct{}

func (stallingFXHistory) ListFXHistory(ctx context.Context, _ string, _, _ time.Time) ([]v1.FXQuotePoint, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The fiat and fiat-cross legs shared the market-cap leg's shape exactly:
// a blown ListFXHistory budget fell through to an empty series at 200 with
// no flag. Same fix, same proof.
func TestChart_Fiat_RequestDeadlineIs503NotEmptySeries(t *testing.T) {
	srv := v1.New(v1.Options{
		History:            &stubHistoryReader{},
		FXHistory:          stallingFXHistory{},
		VerifiedCurrencies: newTestCatalogue(t),
		RequestTimeout:     150 * time.Millisecond,
	})
	ts := httpTestServer(t, srv)

	for _, q := range []string{
		"/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1y&granularity=1d",
		"/v1/chart?asset=fiat:EUR&quote=fiat:GBP&timeframe=1y&granularity=1d",
	} {
		resp := mustGet(t, ts.URL+q)
		body, _ := readAll(resp)
		if resp.StatusCode == http.StatusOK {
			var env struct {
				Data  v1.ChartSeries `json:"data"`
				Flags v1.Flags       `json:"flags"`
			}
			_ = json.Unmarshal([]byte(body), &env)
			t.Fatalf("%s: status = 200 with %d points and stale=%v — a blown fx read deadline must not be served as an empty series a caller reads as fact",
				q, len(env.Data.Points), env.Flags.Stale)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503: %s", q, resp.StatusCode, body)
		}
		if !strings.Contains(body, "chart-timeout") {
			t.Errorf("%s: expected the chart-timeout problem type, got: %s", q, body)
		}
	}
}
