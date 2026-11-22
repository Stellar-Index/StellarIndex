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

const (
	exclUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	exclAQUA = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

type exclTable struct {
	rows []timescale.SourceExclusion
	err  error
}

func (e exclTable) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e.rows, e.err
}

// exclCache serves one cached VWAP for every pair, as the aggregator's Redis keys do for a triangulated value (direct: a
// marker-less direct value instead).
type exclCache struct {
	value  string
	direct bool
}

func (c exclCache) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{Value: c.value, Triangulated: !c.direct, ObservedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)}, true, nil
}

type exclCase struct {
	name string
	gate v1.PriceExclusionGate // nil: no gate wired
	// withheld: the reason every surface must report; "" means served unchanged.
	withheld string
	// multiHop, when set, replaces withheld for a value that may be a composite over unrecorded legs.
	multiHop string
}

// forMultiHop is tc as judged for a triangulated (or provenance-unknown) value, which any in-force exclusion withholds.
func (tc exclCase) forMultiHop() exclCase {
	if tc.multiHop != "" {
		tc.withheld = tc.multiHop
	}
	return tc
}

func exclCases() []exclCase {
	gate := func(e exclTable) v1.PriceExclusionGate {
		return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
	}
	from := time.Now().Add(-time.Hour)
	return []exclCase{
		{name: "no gate"},
		{name: "no exclusions", gate: gate(exclTable{})},
		{name: "another pair excluded", multiHop: "source_excluded", gate: gate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "soroswap", Base: exclAQUA, Quote: exclUSDC, From: from,
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
			for _, direct := range []bool{false, true} {
				exclRedisLayerCase(t, quote, path, direct)
			}
		}
	}
}

func exclRedisLayerCase(t *testing.T, quote, path string, direct bool) {
	t.Helper()
	newSrv := func(g v1.PriceExclusionGate) *v1.Server {
		return v1.New(v1.Options{Prices: &stubPriceReader{}, Triangulated: exclCache{value: "0.2468", direct: direct}, Exclusions: g})
	}
	_, baseline := exclGet(t, newSrv(nil), path)
	for _, tc := range exclCases() {
		if !direct {
			tc = tc.forMultiHop()
		}
		if quote == exclUSDC && tc.name == "pair exclusion under the crypto:XLM alias" {
			// A cached value carries no source list, so a pair-scoped exclusion withholds every cached price whose base is either
			// of its legs, whatever the quote: native/USDC is withheld by an exclusion scoped to crypto:XLM/fiat:USD.
			tc.withheld = "source_excluded"
		}
		status, body := exclGet(t, newSrv(tc.gate), path)
		if strings.HasPrefix(path, "/v1/price/batch") && tc.withheld != "" {
			// The batch names withheld ids without a per-id reason.
			if !strings.Contains(body, `"withheld":["native"]`) || strings.Contains(body, "0.2468") {
				t.Fatalf("%s %s direct=%v: %d %s, want native withheld", tc.name, path, direct, status, body)
			}
			continue
		}
		wantExclVerdict(t, tc, status, body, "0.2468", baseline)
		if tc.withheld != "" && strings.HasPrefix(path, "/v1/price?") && status != http.StatusNotFound {
			t.Fatalf("%s %s direct=%v: status %d, want 404", tc.name, path, direct, status)
		}
	}
}

// TestPriceSourceExclusion_PivotLegOfTriangulatedValue: a triangulated AQUA/USD cache value can route AQUA/XLM x XLM/USDC, and
// its legs are not recorded, so an exclusion on the XLM/USDC leg withholds it; a direct AQUA value keeps the pair/base rule.
func TestPriceSourceExclusion_PivotLegOfTriangulatedValue(t *testing.T) {
	const path = "/v1/price?asset=" + exclAQUA
	pivotLeg := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "sdex", Base: "native", Quote: exclUSDC, From: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	for _, direct := range []bool{false, true} {
		newSrv := func(g v1.PriceExclusionGate) *v1.Server {
			return v1.New(v1.Options{Prices: &stubPriceReader{}, Triangulated: exclCache{value: "0.0042", direct: direct}, Exclusions: g})
		}
		_, baseline := exclGet(t, newSrv(nil), path)
		_, clean := exclGet(t, newSrv(pricingguard.NewSourceExclusionGate(exclTable{}, pricingguard.SourceExclusionGateOptions{})), path)
		if exclAsOf.ReplaceAllString(clean, "") != exclAsOf.ReplaceAllString(baseline, "") || !strings.Contains(baseline, "0.0042") {
			t.Fatalf("direct=%v: no exclusions %s, want the ungated body %s", direct, clean, baseline)
		}
		status, body := exclGet(t, newSrv(pivotLeg), path)
		if direct {
			wantExclVerdict(t, exclCase{name: "direct, unrelated pair"}, status, body, "0.0042", baseline)
			continue
		}
		if status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) || strings.Contains(body, "0.0042") {
			t.Fatalf("triangulated: %d %s, want 404 source_excluded", status, body)
		}
	}
}

// TestPriceSourceExclusion_FrozenHeldValue: a frozen pair replaces its clean closed bucket with the held VWAP-cache value,
// which carries no source list, so an exclusion touching the pair withholds the held value instead of serving it.
func TestPriceSourceExclusion_FrozenHeldValue(t *testing.T) {
	for _, triangulated := range []bool{false, true} {
		exclFrozenHeldCase(t, triangulated)
	}
}

func exclFrozenHeldCase(t *testing.T, triangulated bool) {
	t.Helper()
	const key = "crypto:XLM/fiat:USD"
	heldAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	newSrv := func(g v1.PriceExclusionGate) *v1.Server {
		return v1.New(v1.Options{
			Prices: &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{key: {AssetID: "crypto:XLM", Quote: "fiat:USD", Price: movedBucket, PriceType: "vwap", WindowSeconds: 60}},
				sources:   map[string][]string{key: {"kraken", "coinbase", "bitstamp"}},
			},
			Freeze:       frozenPairs{key: true},
			Triangulated: heldSincePairs{values: lkgPairs{key + "/300": heldLKG}, at: heldAt, triangulated: triangulated},
			Exclusions:   g,
		})
	}
	for _, path := range []string{"/v1/price?asset=crypto:XLM&quote=fiat:USD", "/v1/price/batch?asset_ids=crypto:XLM&quote=fiat:USD"} {
		_, baseline := exclGet(t, newSrv(nil), path)
		if !strings.Contains(baseline, heldLKG) {
			t.Fatalf("%s: ungated frozen pair %s, want the held value %s", path, baseline, heldLKG)
		}
		for _, tc := range exclCases() {
			if triangulated {
				tc = tc.forMultiHop()
			}
			status, body := exclGet(t, newSrv(tc.gate), path)
			if strings.Contains(body, movedBucket) {
				t.Fatalf("%s %s: served the refused bucket: %s", tc.name, path, body)
			}
			if strings.HasPrefix(path, "/v1/price/batch") && tc.withheld != "" {
				if !strings.Contains(body, `"withheld":["crypto:XLM"]`) || strings.Contains(body, heldLKG) {
					t.Fatalf("%s %s: %d %s, want crypto:XLM withheld", tc.name, path, status, body)
				}
				continue
			}
			if !strings.HasPrefix(path, "/v1/price/batch") && tc.withheld != "" && status != http.StatusNotFound {
				t.Fatalf("%s %s: status %d %s, want a 404 withholding", tc.name, path, status, body)
			}
			wantExclVerdict(t, tc, status, body, heldLKG, baseline)
		}
	}
}

// TestPriceSourceExclusion_SEP40FrozenHeldValue: SEP-40's three reads of a frozen pair whose held value an exclusion withholds
// answer as /v1/price does, a 404 naming the reason, not the 503 "retry shortly" of a freeze with nothing held.
func TestPriceSourceExclusion_SEP40FrozenHeldValue(t *testing.T) {
	sourceWide := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", From: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	unread := pricingguard.NewSourceExclusionGate(exclTable{err: errors.New("db down")}, pricingguard.SourceExclusionGateOptions{})
	for _, path := range []string{lastPriceURL, xLastURL, pricesURL} {
		for reason, g := range map[string]v1.PriceExclusionGate{"source_excluded": sourceWide, "unattributed": unread} {
			srv := v1.New(v1.Options{
				Prices: movedUSDBucketReader(), Freeze: frozenPairs{xlmUSD: true},
				Triangulated: lkgPairs{xlmUSD + "/300": heldUSD}, Exclusions: g,
			})
			status, body := exclGet(t, srv, path)
			if status != http.StatusNotFound || !strings.Contains(body, `"`+reason+`"`) || strings.Contains(body, heldUSD) || strings.Contains(body, movedUSD) {
				t.Errorf("%s %s: %d %s, want a 404 withholding %q with no price", path, reason, status, body, reason)
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
		tc = tc.forMultiHop() // a frame does not say whether it is a composite
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
