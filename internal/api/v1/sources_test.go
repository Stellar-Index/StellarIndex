package v1_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"sort"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestSources_ReturnsRegistry(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/sources")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) == 0 {
		t.Fatal("expected at least one source in /v1/sources")
	}

	// Spot-check the canonical entries: binance is exchange-class
	// CEX, soroswap is exchange-class DEX, coingecko is
	// aggregator-class with no subclass, etc.
	want := map[string]struct {
		class    string
		subclass string
		inVWAP   bool
		// backfillSafe pinned for the 5 spot-checked entries —
		// flips on these are deliberate (per-WASM audit landings)
		// and shouldn't go silent.
		backfillSafe bool
	}{
		"binance":       {class: "exchange", subclass: "cex", inVWAP: true, backfillSafe: true},
		"soroswap":      {class: "exchange", subclass: "dex", inVWAP: true, backfillSafe: true},
		"coingecko":     {class: "aggregator", subclass: "", inVWAP: false, backfillSafe: true},
		"reflector-dex": {class: "oracle", subclass: "", inVWAP: false, backfillSafe: true},
		"ecb":           {class: "authority_sanity", subclass: "fx", inVWAP: false, backfillSafe: true},
	}
	got := map[string]v1.Source{}
	for _, s := range env.Data {
		got[s.Name] = s
	}
	for name, exp := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("source %q missing from /v1/sources", name)
			continue
		}
		if s.Class != exp.class {
			t.Errorf("%s.class = %q want %q", name, s.Class, exp.class)
		}
		if s.Subclass != exp.subclass {
			t.Errorf("%s.subclass = %q want %q", name, s.Subclass, exp.subclass)
		}
		if s.IncludeInVWAP != exp.inVWAP {
			t.Errorf("%s.include_in_vwap = %v want %v", name, s.IncludeInVWAP, exp.inVWAP)
		}
		if s.BackfillSafe != exp.backfillSafe {
			t.Errorf("%s.backfill_safe = %v want %v", name, s.BackfillSafe, exp.backfillSafe)
		}
	}
}

func TestSources_FilterByClass(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	cases := []struct {
		class string
		want  map[string]bool // expected names
	}{
		{
			class: "aggregator",
			want:  map[string]bool{"coingecko": true, "coinmarketcap": true, "cryptocompare": true},
		},
		{
			class: "oracle",
			want:  map[string]bool{"reflector-dex": true, "reflector-cex": true, "reflector-fx": true, "redstone": true, "band": true},
		},
		{
			class: "authority_sanity",
			want:  map[string]bool{"ecb": true},
		},
		{
			class: "bridge",
			want:  map[string]bool{"cctp": true, "rozo": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/sources?class="+tc.class)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var env struct {
				Data []v1.Source `json:"data"`
			}
			mustDecode(t, resp, &env)

			got := map[string]bool{}
			for _, s := range env.Data {
				if s.Class != tc.class {
					t.Errorf("class filter leaked: got %q in class=%q result", s.Class, tc.class)
				}
				got[s.Name] = true
			}
			for name := range tc.want {
				if !got[name] {
					t.Errorf("expected %s in class=%q result, got %v", name, tc.class, got)
				}
			}
		})
	}
}

func TestSources_FilterByClass_Unknown(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/sources?class=nonsense")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown class", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("invalid-class")) {
		t.Errorf("expected invalid-class error type in body: %s", body)
	}
}

func TestSources_SortedByName(t *testing.T) {
	// Stable ordering matters: CDN cache hit ratio + smoother diffs
	// in operator dashboards.
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/sources")
	var env struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, resp, &env)

	names := make([]string, len(env.Data))
	for i, s := range env.Data {
		names[i] = s.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("sources not sorted: %v", names)
	}
}

// TestSources_NetworkScoped pins that a test net lists only the sources that
// exist on it, while pubnet (and the empty default) keeps the full registry.
func TestSources_NetworkScoped(t *testing.T) {
	names := func(network string) []string {
		t.Helper()
		ts := httpTestServer(t, v1.New(v1.Options{Network: network}))
		resp := mustGet(t, ts.URL+"/v1/sources")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("network %q: status = %d, want 200", network, resp.StatusCode)
		}
		var env struct {
			Data []v1.Source `json:"data"`
		}
		mustDecode(t, resp, &env)
		out := make([]string, 0, len(env.Data))
		for _, s := range env.Data {
			out = append(out, s.Name)
		}
		return out
	}

	def, pub := names(""), names("pubnet")
	if len(def) != len(external.Registry) || len(pub) != len(external.Registry) {
		t.Fatalf("pubnet must serve the full registry: default=%d pubnet=%d registry=%d",
			len(def), len(pub), len(external.Registry))
	}

	for _, network := range []string{"testnet", "futurenet"} {
		got := names(network)
		for _, n := range got {
			if ok, _ := sourcenet.Applicable(n, network); !ok {
				t.Errorf("%s: /v1/sources lists pubnet-only source %q", network, n)
			}
		}
		for _, n := range []string{"binance", "soroswap", "chainlink"} {
			if slices.Contains(got, n) {
				t.Errorf("%s: /v1/sources lists %q", network, n)
			}
		}
		if !slices.Contains(got, "sdex") {
			t.Errorf("%s: /v1/sources omits sdex", network)
		}
	}
}

// TestSources_EveryRegistryNameClassified guards the network gate's failure
// direction: sourcenet.Applicable answers false for any name it does not
// classify, so a new on-chain registry entry that runs on every network would
// silently vanish from test nets. Only off-chain feeds may rely on that default.
func TestSources_EveryRegistryNameClassified(t *testing.T) {
	offChain := map[string]bool{
		"binance": true, "kraken": true, "bitstamp": true, "coinbase": true, "poloniex_via_btc": true, // CEX
		"massive": true, "exchangeratesapi": true, "ecb": true, // FX
		"coingecko": true, "coinmarketcap": true, "cryptocompare": true, // aggregators
		"chainlink": true, // EVM oracle, read off-chain
		"tiingo":    true, // fund NAV vendor
	}
	for name := range external.Registry {
		if !sourcenet.Known(name) && !offChain[name] {
			t.Errorf("external.Registry[%q] is neither classified in sourcenet nor listed as an off-chain feed", name)
		}
	}
}

// A source whose figure excludes unpriced XLM-leg trades serves its USD
// volume as a named lower bound; a fully priced source does not.
func TestSources_VolumeLowerBound(t *testing.T) {
	srv := v1.New(v1.Options{SourcesStats: lowerBoundSourcesStats{
		stats: []timescale.SourceStats{
			{Source: "soroswap", TradeCount24h: 10, UnpricedTrades24h: 3},
			{Source: "aquarius", TradeCount24h: 5},
		},
		h24: []timescale.SourceVolumeBucket{{Source: "phoenix", VolumeUSD: "1", XLMUnpriced: true}},
	}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/sources?include=stats,sparkline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, resp, &body)
	got := map[string]bool{}
	for _, s := range body.Data {
		got[s.Name] = s.VolumeLowerBound
	}
	if !got["soroswap"] || !got["phoenix"] {
		t.Errorf("sources with excluded XLM legs must carry volume_lower_bound: %v", got)
	}
	if got["aquarius"] {
		t.Errorf("fully priced source must not be flagged: %v", got)
	}
}

type lowerBoundSourcesStats struct {
	stats []timescale.SourceStats
	h24   []timescale.SourceVolumeBucket
}

func (s lowerBoundSourcesStats) GetSourceStats(context.Context) ([]timescale.SourceStats, error) {
	return s.stats, nil
}

func (s lowerBoundSourcesStats) GetSourceVolumeHistory24h(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return s.h24, nil
}

func (lowerBoundSourcesStats) GetSourceVolumeHistory7d(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, nil
}
