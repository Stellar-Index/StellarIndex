package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// oracleExclSeries is three live closed 1m sdex buckets of native/quote, newest first, priced p0 (newest), p1, p2.
func oracleExclSeries(quote string, now time.Time) []v1.PriceSnapshot {
	out := make([]v1.PriceSnapshot, 0, 3)
	for i, p := range []string{"0.2470", "0.2471", "0.2472"} {
		snap := v1.VWAP1mToSnapshot("native", quote, p, now.Add(-time.Duration(i+2)*time.Minute))
		snap.BucketSources = []string{"sdex"}
		out = append(out, snap)
	}
	return out
}

// TestOraclePrices_PegSeriesJudgedOnItsOwnSources: a peg bucket is judged on the sources that fed it, so an exclusion of
// another source (source-wide, in force over the whole series) leaves it served; one naming its source does not.
func TestOraclePrices_PegSeriesJudgedOnItsOwnSources(t *testing.T) {
	usdc, _ := canonical.ParseAsset(exclUSDC)
	now := time.Now().UTC().Truncate(time.Minute)
	get := func(source string) (int, string) {
		return exclGet(t, v1.New(v1.Options{
			Prices: &stubPriceReader{recent: map[string][]v1.PriceSnapshot{"native/" + exclUSDC: oracleExclSeries(exclUSDC, now)}},
			Exclusions: pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{
				{ID: 1, Source: source, From: now.Add(-time.Hour)},
			}}, pricingguard.SourceExclusionGateOptions{}),
			USDPeggedClassics: []canonical.Asset{usdc},
		}), "/v1/oracle/prices?asset=native&records=5")
	}
	if status, body := get("coinbase"); status != http.StatusOK || !strings.Contains(body, "0.2470") ||
		strings.Contains(body, "fills_withheld_reason") {
		t.Fatalf("coinbase excluded: %d %s, want the three sdex peg buckets unflagged", status, body)
	}
	if status, body := get("sdex"); status != http.StatusNotFound || strings.Contains(body, "0.247") {
		t.Fatalf("sdex excluded: %d %s, want 404 with no price", status, body)
	}
}

// TestOraclePrices_WithheldDirectSeriesFallsToPeg: a direct series the reader withheld is no direct series, so a clean peg
// book still answers, flagged peg-served as any peg answer is.
func TestOraclePrices_WithheldDirectSeriesFallsToPeg(t *testing.T) {
	usdc, _ := canonical.ParseAsset(exclUSDC)
	now := time.Now().UTC().Truncate(time.Minute)
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			recent:    map[string][]v1.PriceSnapshot{"native/" + exclUSDC: oracleExclSeries(exclUSDC, now)},
			errByPair: map[string]error{"native/fiat:USD": v1.PriceWithheldError(pricingguard.WithheldSourceExcluded)},
		},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	status, body := exclGet(t, srv, "/v1/oracle/prices?asset=native&records=5")
	if status != http.StatusOK || !strings.Contains(body, "0.2470") || !strings.Contains(body, `"triangulated":true`) {
		t.Fatalf("%d %s, want the peg series served triangulated", status, body)
	}
}

// TestOraclePrices_DormantFallbackAfterWithheldIsFlagged: when an exclusion empties the live series and an older one is
// served instead, the answer says buckets were withheld and is not cached, for a peg leg and for an alias spelling alike.
func TestOraclePrices_DormantFallbackAfterWithheldIsFlagged(t *testing.T) {
	usdc, _ := canonical.ParseAsset(exclUSDC)
	now := time.Now().UTC().Truncate(time.Minute)
	dormant := func(base string) []v1.PriceSnapshot {
		return []v1.PriceSnapshot{v1.VWAP1mToSnapshot(base, "fiat:USD", "0.1900", now.Add(-21*24*time.Hour))}
	}
	withheld := v1.PriceWithheldError(pricingguard.WithheldSourceExcluded)
	cases := map[string]v1.Options{
		"peg leg withheld": {
			Prices: &stubPriceReader{
				recent:    map[string][]v1.PriceSnapshot{"native/fiat:USD": dormant("native")},
				errByPair: map[string]error{"native/" + exclUSDC: withheld},
			},
			USDPeggedClassics: []canonical.Asset{usdc},
		},
		"alias spelling withheld": {
			Prices: &stubPriceReader{
				recent:    map[string][]v1.PriceSnapshot{"crypto:XLM/fiat:USD": dormant("crypto:XLM")},
				errByPair: map[string]error{"native/fiat:USD": withheld},
			},
		},
	}
	for name, opts := range cases {
		ts := startHTTPTest(t, v1.New(opts).Handler())
		resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
		var env struct {
			Data  []v1.SEP40Price `json:"data"`
			Flags v1.Flags        `json:"flags"`
		}
		mustDecode(t, resp, &env)
		if resp.StatusCode != http.StatusOK || len(env.Data) != 1 || env.Flags.FillsWithheldReason != v1.PriceWithheldSourceExcluded {
			t.Errorf("%s: %d %+v %+v, want the dormant series flagged fills_withheld_reason=source_excluded", name, resp.StatusCode, env.Data, env.Flags)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s: Cache-Control %q, want no-store", name, cc)
		}
	}
}

// TestOraclePrices_PegSeriesJudgedUnderRequestedPair: prices() for native in fiat:USD with no fiat:USD series is served from
// the native/USDC book, so an exclusion stored under native/fiat:USD must reach those buckets: a covered bucket is not
// served, the survivors carry fills_withheld_reason, and a series the exclusion empties is withheld, never 200 [].
func TestOraclePrices_PegSeriesJudgedUnderRequestedPair(t *testing.T) {
	usdc, _ := canonical.ParseAsset(exclUSDC)
	now := time.Now().UTC().Truncate(time.Minute)
	newSrv := func(rows ...timescale.SourceExclusion) *v1.Server {
		return v1.New(v1.Options{
			Prices:            &stubPriceReader{recent: map[string][]v1.PriceSnapshot{"native/" + exclUSDC: oracleExclSeries(exclUSDC, now)}},
			Exclusions:        pricingguard.NewSourceExclusionGate(exclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{}),
			USDPeggedClassics: []canonical.Asset{usdc},
		})
	}
	const path = "/v1/oracle/prices?asset=native&records=5"
	fiatPair := func(from time.Time) timescale.SourceExclusion {
		return timescale.SourceExclusion{ID: 1, Source: "sdex", Base: "native", Quote: "fiat:USD", From: from}
	}

	status, body := exclGet(t, newSrv(), path)
	if status != http.StatusOK || !strings.Contains(body, "0.2470") || strings.Contains(body, "fills_withheld_reason") {
		t.Fatalf("no exclusion: %d %s, want the three peg buckets unflagged", status, body)
	}

	status, body = exclGet(t, newSrv(fiatPair(now.Add(-time.Hour))), path)
	if status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) || strings.Contains(body, "0.247") {
		t.Fatalf("whole series covered: %d %s, want 404 source_excluded with no price", status, body)
	}

	// Covers only the newest bucket, [now-2m, now-1m).
	ts := startHTTPTest(t, newSrv(fiatPair(now.Add(-90*time.Second))).Handler())
	resp := mustGet(t, ts.URL+path)
	var env struct {
		Data  []v1.SEP40Price `json:"data"`
		Flags v1.Flags        `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if resp.StatusCode != http.StatusOK || len(env.Data) != 2 || env.Data[0].Price != "0.2471" {
		t.Fatalf("newest bucket covered: %d %+v, want the two older buckets", resp.StatusCode, env.Data)
	}
	if env.Flags.FillsWithheldReason != v1.PriceWithheldSourceExcluded || !env.Flags.Triangulated {
		t.Fatalf("newest bucket covered: flags %+v, want fills_withheld_reason=source_excluded on a triangulated series", env.Flags)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("newest bucket covered: Cache-Control %q, want no-store (degraded)", cc)
	}
}

// TestOraclePrices_ReaderRemovalIsFlagged: buckets the reader's own exclusion check removed from the direct series are
// marked on the snapshots it returns, and the envelope says so.
func TestOraclePrices_ReaderRemovalIsFlagged(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	series := oracleExclSeries("fiat:USD", now)[1:]
	for i := range series {
		series[i].FillsWithheld = true
	}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{recent: map[string][]v1.PriceSnapshot{"native/fiat:USD": series}}})
	status, body := exclGet(t, srv, "/v1/oracle/prices?asset=native&records=5")
	if status != http.StatusOK || !strings.Contains(body, `"fills_withheld_reason":"source_excluded"`) {
		t.Fatalf("%d %s, want the direct series served with fills_withheld_reason=source_excluded", status, body)
	}
}
