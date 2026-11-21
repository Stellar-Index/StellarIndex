package v1_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const exclUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

type exclTable struct {
	rows []timescale.SourceExclusion
	err  error
}

func (e exclTable) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e.rows, e.err
}

// exclCache serves one cached VWAP for every pair, as the aggregator's Redis keys do for a triangulated value.
type exclCache struct{ value string }

func (c exclCache) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{Value: c.value, Triangulated: true, ObservedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)}, true, nil
}

type exclCase struct {
	name string
	gate v1.PriceExclusionGate // nil: no gate wired
	// withheld: the reason every surface must report; "" means served unchanged.
	withheld string
}

func exclCases() []exclCase {
	gate := func(e exclTable) v1.PriceExclusionGate {
		return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
	}
	from := time.Now().Add(-time.Hour)
	return []exclCase{
		{name: "no gate"},
		{name: "no exclusions", gate: gate(exclTable{})},
		{name: "another pair excluded", gate: gate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", Base: "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", Quote: exclUSDC, From: from,
		}}})},
		{name: "lifted exclusion", gate: gate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", From: from, LiftedAt: time.Now().Add(-time.Minute),
		}}})},
		{name: "source-wide exclusion", withheld: "source_excluded", gate: gate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", From: from,
		}}})},
		{name: "pair exclusion under the crypto:XLM alias", withheld: "source_excluded", gate: gate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", Base: "crypto:XLM", Quote: "fiat:USD", From: from,
		}}})},
		{name: "exclusions unreadable", withheld: "unattributed", gate: gate(exclTable{err: errors.New("db down")})},
	}
}

func exclGet(t *testing.T, srv *v1.Server, path string) (int, string) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

var exclAsOf = regexp.MustCompile(`"as_of":"[^"]*"`)

func wantExclVerdict(t *testing.T, tc exclCase, status int, body, served, baseline string) {
	t.Helper()
	// as_of is the response's own clock; everything else must match the ungated body.
	body, baseline = exclAsOf.ReplaceAllString(body, ""), exclAsOf.ReplaceAllString(baseline, "")
	if tc.withheld == "" {
		if status != http.StatusOK || body != baseline || !strings.Contains(body, served) {
			t.Fatalf("%s: status %d body %s, want the ungated 200 byte-for-byte: %s", tc.name, status, body, baseline)
		}
		return
	}
	if !strings.Contains(body, `"`+tc.withheld+`"`) || strings.Contains(body, served) {
		t.Fatalf("%s: status %d body %s, want withheld %q with no price", tc.name, status, body, tc.withheld)
	}
}

// TestPriceSourceExclusion_RedisLayer: /v1/price and the batch read a pair with no prices_1m rows from the VWAP cache, which
// carries no source list; an exclusion touching the pair withholds it and no lower layer answers instead.
func TestPriceSourceExclusion_RedisLayer(t *testing.T) {
	for _, quote := range []string{"fiat:USD", exclUSDC} {
		for _, path := range []string{"/v1/price?asset=native&quote=" + quote, "/v1/price/batch?asset_ids=native&quote=" + quote} {
			newSrv := func(g v1.PriceExclusionGate) *v1.Server {
				return v1.New(v1.Options{Prices: &stubPriceReader{}, Triangulated: exclCache{value: "0.2468"}, Exclusions: g})
			}
			_, baseline := exclGet(t, newSrv(nil), path)
			for _, tc := range exclCases() {
				if quote == exclUSDC && tc.name == "pair exclusion under the crypto:XLM alias" {
					continue // scoped to fiat:USD, which a USDC-quoted value is not
				}
				status, body := exclGet(t, newSrv(tc.gate), path)
				if strings.HasPrefix(path, "/v1/price/batch") && tc.withheld != "" {
					// The batch names withheld ids without a per-id reason.
					if !strings.Contains(body, `"withheld":["native"]`) || strings.Contains(body, "0.2468") {
						t.Fatalf("%s %s: %d %s, want native withheld", tc.name, path, status, body)
					}
					continue
				}
				wantExclVerdict(t, tc, status, body, "0.2468", baseline)
				if tc.withheld != "" && strings.HasPrefix(path, "/v1/price?") && status != http.StatusNotFound {
					t.Fatalf("%s %s: status %d, want 404", tc.name, path, status)
				}
			}
		}
	}
}

// TestPriceSourceExclusion_TipWindow: the tip's rolling window drops an excluded source's trades and recomputes from the
// survivors; when none survive, the cache fallback is withheld rather than served; an unread set withholds outright.
func TestPriceSourceExclusion_TipWindow(t *testing.T) {
	now := time.Now().UTC()
	mixed := []canonical.Trade{mkTipTrade(now.Add(-2*time.Second), 5, 100, "soroswap"), mkTipTrade(now.Add(-time.Second), 5, 50, "sdex")}
	newSrv := func(g v1.PriceExclusionGate, trades []canonical.Trade) *v1.Server {
		return v1.New(v1.Options{
			Prices: &stubPriceReader{}, History: &stubHistoryReader{trades: trades},
			Triangulated: exclCache{value: "0.2468"}, Exclusions: g,
		})
	}
	const path = "/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5"
	for _, tc := range exclCases() {
		status, body := exclGet(t, newSrv(tc.gate, mixed), path)
		switch tc.withheld {
		case "":
			if status != http.StatusOK || !strings.Contains(body, `"price":"15.0000000000"`) {
				t.Fatalf("%s: mixed window %d %s, want the two-source VWAP 15", tc.name, status, body)
			}
		case "source_excluded":
			if status != http.StatusOK || !strings.Contains(body, `"price":"10.0000000000"`) || !strings.Contains(body, `"sources":["sdex"]`) {
				t.Fatalf("%s: mixed window %d %s, want the sdex-only VWAP 10", tc.name, status, body)
			}
		default:
			if status != http.StatusNotFound || !strings.Contains(body, `"`+tc.withheld+`"`) {
				t.Fatalf("%s: %d %s, want withheld %q and no fallback", tc.name, status, body, tc.withheld)
			}
		}

		_, baseline := exclGet(t, newSrv(nil, mixed[:1]), path)
		status, body = exclGet(t, newSrv(tc.gate, mixed[:1]), path)
		if tc.withheld != "" && status != http.StatusNotFound {
			t.Fatalf("%s: excluded-only window %d %s, want 404", tc.name, status, body)
		}
		if tc.withheld == "" && !strings.Contains(body, `"price":"20.0000000000"`) {
			t.Fatalf("%s: kraken-only window %d %s, want 20", tc.name, status, body)
		}
		if tc.withheld != "" {
			wantExclVerdict(t, tc, status, body, "0.2468", baseline)
		}
	}
}

// TestPriceSourceExclusion_TipStream: the tip stream shares computeTip, so a withheld tip is never emitted as a price.
func TestPriceSourceExclusion_TipStream(t *testing.T) {
	now := time.Now().UTC()
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{}, History: &stubHistoryReader{trades: []canonical.Trade{mkTipTrade(now, 5, 100, "soroswap")}},
		Triangulated: exclCache{value: "0.2468"}, Hub: streaming.NewHub(0),
		Exclusions: exclCases()[4].gate,
	})
	ts := startHTTPTest(t, srv.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got string
	if resp.StatusCode == http.StatusOK {
		got = readPriceStreamFrame(t, bufio.NewReader(resp.Body), 3*time.Second)
	} else {
		b, _ := io.ReadAll(resp.Body)
		got = string(b)
	}
	if !strings.Contains(got, "source_excluded") || strings.Contains(got, "0.2468") || strings.Contains(got, `"price":"20.0000000000"`) {
		t.Fatalf("tip stream (%d) emitted %q, want a source_excluded withholding", resp.StatusCode, got)
	}
}

// TestPriceSourceExclusion_ClosedStream: closed-bucket frames carry no source list, so the stream refuses a pair an in-force
// exclusion touches, as /v1/price does.
func TestPriceSourceExclusion_ClosedStream(t *testing.T) {
	for _, tc := range exclCases() {
		srv := v1.New(v1.Options{Hub: streaming.NewHub(0), Exclusions: tc.gate})
		ts := startHTTPTest(t, srv.Handler())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("GET: %v", err)
		}
		if tc.withheld == "" {
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d, want the stream to open", tc.name, resp.StatusCode)
			}
		} else {
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), `"`+tc.withheld+`"`) {
				t.Fatalf("%s: status %d body %s, want 404 %q", tc.name, resp.StatusCode, b, tc.withheld)
			}
		}
		_ = resp.Body.Close()
		cancel()
	}
}
