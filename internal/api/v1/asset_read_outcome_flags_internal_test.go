package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// classicPageReader serves one fixed classic page from the listing read.
type classicPageReader struct {
	truthAssetsReader
	rows []timescale.AssetRow
}

func (c *classicPageReader) ListAssetsExtAt(
	context.Context, timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	return c.rows, time.Time{}, false, nil
}

func (c *classicPageReader) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, sql.ErrNoRows
}

// noStore is how a degraded envelope reaches the wire; Flags.Degraded itself is never serialised.
func noStore(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get("Cache-Control") == "no-store"
}

type envFlags struct {
	Flags Flags `json:"flags"`
}

func decodeFlags(t *testing.T, rec *httptest.ResponseRecorder) Flags {
	t.Helper()
	var got envFlags
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got.Flags
}

// Page 1 of asset_class=all tops a short catalogue up with classic rows; that fill must not clear the catalogue's stale verdict.
func TestCatalogueUnifiedPage_ClassicFillKeepsCatalogueStale(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	for name, classic := range map[string][]timescale.AssetRow{
		"catalogue only":         nil,
		"catalogue plus classic": {{AssetID: cetesAsset, Code: "CETES", Slug: "cetes"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{VerifiedCurrencies: cat})
			s.AssetsReader = &classicPageReader{rows: classic}
			s.globalPrice = oldVWAPReader{age: 3 * time.Hour}
			s.globalPriceOpts = aggregate.GlobalPriceOptions{MaxVWAPAge: 10 * time.Minute}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&limit=100", nil)
			s.serveCatalogueUnifiedPage(rec, req, assetListFilters{}, 100, "")
			if f := decodeFlags(t, rec); !f.Stale {
				t.Errorf("flags.stale = false; catalogue rows priced from a VWAP past its ceiling must keep the page stale")
			}
		})
	}
}

// A listing row filled from a declared peg at a week-old FX rate carries that rate's stale verdict to the envelope.
func TestClassicUnifiedPage_StalePegRateMarksThePageStale(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-6*24*time.Hour - 12*time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
	}}
	s := pegTestServer(t, fx)
	s.logger = discardLogger()
	s.AssetsReader = &classicPageReader{rows: []timescale.AssetRow{{AssetID: pegTestAUDD, Code: "AUDD", Slug: "audd"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&cursor=classic:", nil)
	s.serveClassicUnifiedPage(rec, req, assetListFilters{}, 10, "")
	if f := decodeFlags(t, rec); !f.Stale {
		t.Errorf("flags.stale = false over a declared-peg price from a 6.5-day-old rate; body %s", rec.Body.String())
	}
}

// The detail's declared-peg fill reports a 6.5-day-old FX rate stale; the detail must say so.
func TestAssetDetail_StalePegRateMarksTheDetailStale(t *testing.T) {
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		age  time.Duration
		want bool
	}{
		"six and a half days": {6*24*time.Hour + 12*time.Hour, true},
		"one day":             {24 * time.Hour, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{
				FXHistory: &stubFXHistory{points: []FXQuotePoint{
					{Bucket: time.Now().UTC().Add(-tc.age), RateUSDText: "1.5267", InverseUSDText: "0.655"},
				}},
				FiatPeggedClassics: map[string]canonical.Asset{pegTestAUDD: aud},
			})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+pegTestAUDD, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Data  AssetDetail `json:"data"`
				Flags Flags       `json:"flags"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Data.PriceUSD == nil || *got.Data.PriceUSD != "0.655" {
				t.Fatalf("price_usd = %v, want the peg fill 0.655", strPtr(got.Data.PriceUSD))
			}
			if got.Flags.Stale != tc.want {
				t.Errorf("flags.stale = %v, want %v", got.Flags.Stale, tc.want)
			}
		})
	}
}

// erroringSep1Cache fails every SEP-1 read.
type erroringSep1Cache struct{}

func (erroringSep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, errors.New("pq: connection reset")
}

// emptySep1Cache answers every SEP-1 read with no payload.
type emptySep1Cache struct{}

func (emptySep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

// A failed SEP-1 read is ours: the detail and metadata bodies are degraded, no-store, and not replayed from the detail cache.
func TestAssetDetail_FailedSep1ReadIsDegraded(t *testing.T) {
	s := New(Options{Sep1Cache: erroringSep1Cache{}})
	for _, path := range []string{"/v1/assets/" + pegTestAUDD, "/v1/assets/" + pegTestAUDD + "/metadata"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, rec.Code, rec.Body.String())
		}
		if f := decodeFlags(t, rec); !f.Stale || !noStore(rec) {
			t.Errorf("%s: stale = %v, Cache-Control = %q; want stale and no-store", path, f.Stale, rec.Header().Get("Cache-Control"))
		}
	}
	s.Sep1Cache = emptySep1Cache{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+pegTestAUDD, nil))
	if f := decodeFlags(t, rec); f.Stale || noStore(rec) {
		t.Error("the degraded body was replayed from the detail cache after the SEP-1 read recovered")
	}
}

// erroringFXHistory fails every fx_quotes read.
type erroringFXHistory struct{}

func (erroringFXHistory) ListFXHistory(context.Context, string, time.Time, time.Time) ([]FXQuotePoint, error) {
	return nil, errors.New("statement timeout")
}

// /v1/assets/verified computes fiat caps from fx_quotes; a failed or week-old read reaches its flags.
func TestAssetsVerified_FXReadOutcomeReachesFlags(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	old := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-6*24*time.Hour - 12*time.Hour), RateUSDText: "0.9", InverseUSDText: "1.1"},
	}}
	fresh := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "0.9", InverseUSDText: "1.1"},
	}}
	for name, tc := range map[string]struct {
		fx                  FXHistoryReader
		stale, wantDegraded bool
	}{
		"read failed":       {erroringFXHistory{}, true, true},
		"rate week old":     {old, true, false},
		"rate yesterday":    {fresh, false, false},
		"feed never quoted": {windowedFXHistory{}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{VerifiedCurrencies: cat, FXHistory: tc.fx})
			rec := httptest.NewRecorder()
			s.handleAssetsVerified(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/verified", nil))
			if f := decodeFlags(t, rec); f.Stale != tc.stale || noStore(rec) != tc.wantDegraded {
				t.Errorf("stale = %v, no-store = %v, want %v, %v", f.Stale, noStore(rec), tc.stale, tc.wantDegraded)
			}
		})
	}
}

// A substance gate that could not measure leaves the on-chain price unread, not withheld.
func TestOnChainListingPriceUSD_UnmeasuredGateIsReportedUnread(t *testing.T) {
	price := "0.25"
	for name, tc := range map[string]struct {
		gate PriceSubstanceGate
		want bool
	}{
		"gate did not rule": {&unmeasuredGate{}, true},
		"gate refused":      {&stubListingGate{allow: map[string]bool{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{
				Substance: tc.gate,
				AssetsReader: &truthAssetsReader{byID: map[string]timescale.AssetRow{
					withheldTestAsset: {AssetID: withheldTestAsset, PriceUSD: &price},
				}},
			})
			ctx, rep := withReadReport(context.Background())
			if got, _ := s.onChainListingPriceUSD(ctx, withheldTestAsset); got != nil {
				t.Fatalf("price = %s, want withheld", *got)
			}
			if rep.unread.Load() != tc.want {
				t.Errorf("unread reported = %v, want %v", rep.unread.Load(), tc.want)
			}
		})
	}
}

// windowedFXHistory answers each read with only the points inside its [from, to] window, as fx_quotes does.
type windowedFXHistory struct{ points []FXQuotePoint }

func (w windowedFXHistory) ListFXHistory(_ context.Context, _ string, from, to time.Time) ([]FXQuotePoint, error) {
	var out []FXQuotePoint
	for _, p := range w.points {
		if !p.Bucket.Before(from) && !p.Bucket.After(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

// A wired FX feed that quoted a ticker before and has nothing in its 7-day window has gone quiet; that is a stale feed. A ticker
// the feed has never quoted (a test net runs without the aggregator) is a clean miss.
func TestFiatUSDPriceFor_SilentFeedIsReportedStale(t *testing.T) {
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		points []FXQuotePoint
		ok     bool
		stale  bool
	}{
		"never quoted":       {nil, false, false},
		"quoted a month ago": {[]FXQuotePoint{{Bucket: now.Add(-30 * 24 * time.Hour), RateUSDText: "0.9", InverseUSDText: "1.1"}}, false, true},
		"fresh rate":         {[]FXQuotePoint{{Bucket: now.Add(-time.Hour), RateUSDText: "0.9", InverseUSDText: "1.1"}}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{Options: Options{FXHistory: windowedFXHistory{points: tc.points}}, logger: discardLogger()}
			ctx, rep := withReadReport(context.Background())
			_, _, ok := s.fiatUSDPriceFor(ctx, "EUR")
			if ok != tc.ok || rep.stale.Load() != tc.stale || rep.unread.Load() {
				t.Errorf("ok = %v, stale = %v, unread = %v; want %v, %v, false", ok, rep.stale.Load(), rep.unread.Load(), tc.ok, tc.stale)
			}
		})
	}
}

// A broad trustline-sum read that did not answer leaves a priced row with no cap, or a lake net unchecked against the floor
// that may exceed it; either way the listing must report it unread.
func TestFillMarketCapsFromSupply_FailedBroadArmIsUnread(t *testing.T) {
	for name, tc := range map[string]struct {
		lake string
		want bool
	}{
		"no other arm answers": {"", true},
		"lake arm answers":     {"400000000", true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := lakeSupplyServer(t, tc.lake)
			s.Explorer = failingClassicExplorer{}
			s.classicSupplyAttemptAt = time.Now() // retry gap open, nothing cached
			rows := cetesRow()
			if got := s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{}); got != tc.want {
				t.Errorf("supplyUnread = %v, want %v (circulating_supply = %v)", got, tc.want, strPtr(rows[0].CirculatingSupply))
			}
		})
	}
}

// Only a classic row could have been filled from the broad trustline map; a priced Soroban row missing supply while that map is
// down is not that read's hole.
func TestFillMarketCapsFromSupply_FailedBroadArmIgnoresSorobanRows(t *testing.T) {
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	// The SAC's own scale reads fine; only the broad supply arm is down.
	s.TokenDecimals = &countingScaleReader{declared: map[string]uint32{sac: 7}}
	s.classicSupplyAttemptAt = time.Now()
	price := "1.00"
	rows := []AssetDetail{{Kind: "stellar_asset", AssetID: sac, Type: string(canonical.AssetSoroban), Decimals: 7, PriceUSD: &price}}
	if got := s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{}); got {
		t.Error("supplyUnread = true for a Soroban-only page the broad trustline map could never have filled")
	}
}

// A broad trustline map that answers leaves the listing clean.
func TestFillMarketCapsFromSupply_HealthyBroadArmIsClean(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = &trustlineOnlyExplorer{supply: map[string]string{cetesAsset: "400000000"}}
	rows := cetesRow()
	if got := s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{}); got || rows[0].CirculatingSupply == nil {
		t.Errorf("supplyUnread = %v, circulating_supply = %v; want clean and filled", got, strPtr(rows[0].CirculatingSupply))
	}
}

// Page 1 with a fresh VWAP stays clean through the classic fill.
func TestCatalogueUnifiedPage_FreshVWAPIsNotStale(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{VerifiedCurrencies: cat})
	s.AssetsReader = &classicPageReader{rows: []timescale.AssetRow{{AssetID: cetesAsset, Code: "CETES", Slug: "cetes"}}}
	s.globalPrice = oldVWAPReader{age: time.Minute}
	s.globalPriceOpts = aggregate.GlobalPriceOptions{MaxVWAPAge: 10 * time.Minute}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&limit=100", nil)
	s.serveCatalogueUnifiedPage(rec, req, assetListFilters{}, 100, "")
	if f := decodeFlags(t, rec); f.Stale {
		t.Errorf("flags.stale = true over a minute-old VWAP; body %s", rec.Body.String())
	}
}

// The cached listing's declared-peg fill: a fresh rate leaves the page clean and replayable, a week-old one does not.
func TestAssetListPage_PegRateFreshnessDecidesCacheability(t *testing.T) {
	for name, tc := range map[string]struct {
		age       time.Duration
		cacheable bool
	}{
		"rate yesterday": {24 * time.Hour, true},
		"rate week old":  {6*24*time.Hour + 12*time.Hour, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := pegTestServer(t, &stubFXHistory{points: []FXQuotePoint{
				{Bucket: time.Now().UTC().Add(-tc.age), RateUSDText: "1.5267", InverseUSDText: "0.655"},
			}})
			s.logger = discardLogger()
			s.AssetsReader = &classicPageReader{rows: []timescale.AssetRow{{AssetID: pegTestAUDD, Code: "AUDD", Slug: "audd"}}}
			req := httptest.NewRequest(http.MethodGet, "/v1/assets?limit=10", nil)
			env, rows, err := s.buildAssetListPage(req, assetListFilters{}, "", 10, timescale.AssetsOrderVolume24hUSDDesc)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].PriceUSD == nil || *rows[0].PriceUSD != "0.655" {
				t.Fatalf("instrument: want the AUDD row priced from its peg, got %+v", rows)
			}
			if env.Flags.Stale == tc.cacheable || assetListPageCacheable(env, rows) != tc.cacheable {
				t.Errorf("stale = %v, cacheable = %v; want cacheable %v", env.Flags.Stale, assetListPageCacheable(env, rows), tc.cacheable)
			}
		})
	}
}

// noIssuerRowSep1Cache answers as the store does for an issuer with no issuers row.
type noIssuerRowSep1Cache struct{}

func (noIssuerRowSep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, sql.ErrNoRows
}

// verifiedSep1Cache serves a SEP-1 document listing the AUDD currency and a reachable domain.
type verifiedSep1Cache struct{}

func (verifiedSep1Cache) GetIssuerSep1Cached(_ context.Context, issuer string) (*timescale.IssuerSep1Cached, error) {
	return &timescale.IssuerSep1Cached{
		OrgName:    "AUDD Issuer",
		Currencies: []timescale.IssuerSep1Currency{{Code: "AUDD", Issuer: issuer, Name: "AUDD"}},
	}, nil
}

func (verifiedSep1Cache) IssuerSep1Unreachable(context.Context, string) (bool, error) {
	return false, nil
}

var _ Sep1FetchStateReader = verifiedSep1Cache{}

// A SEP-1 read that answered (an issuer we have no row for, or a verified document) is a clean, cacheable detail and metadata body.
func TestAssetDetail_AnsweredSep1ReadIsCleanAndCached(t *testing.T) {
	for name, tc := range map[string]struct {
		cache  Sep1CachedReader
		status string
	}{
		"no issuers row": {noIssuerRowSep1Cache{}, "not_fetched"},
		"verified":       {verifiedSep1Cache{}, "verified"},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{Sep1Cache: tc.cache})
			for _, path := range []string{"/v1/assets/" + pegTestAUDD, "/v1/assets/" + pegTestAUDD + "/metadata"} {
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: status = %d: %s", path, rec.Code, rec.Body.String())
				}
				var got struct {
					Data struct {
						Sep1Status string `json:"sep1_status"`
					} `json:"data"`
					Flags Flags `json:"flags"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Data.Sep1Status != tc.status || got.Flags.Stale || noStore(rec) {
					t.Errorf("%s: sep1_status = %q, stale = %v, no-store = %v; want %q, clean", path,
						got.Data.Sep1Status, got.Flags.Stale, noStore(rec), tc.status)
				}
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+pegTestAUDD, nil))
			if c := rec.Header().Get("X-Stellarindex-Cache"); c != "HIT" {
				t.Errorf("second detail request X-Stellarindex-Cache = %q, want HIT", c)
			}
		})
	}
}

// cachedClassicSupply serves callers that carry no read report and judge supply holes themselves; it must not report.
func TestCachedClassicSupply_DoesNotReport(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyAttemptAt = time.Now()
	ctx, rep := withReadReport(context.Background())
	if m := s.cachedClassicSupply(ctx); m != nil || rep.unread.Load() || rep.stale.Load() {
		t.Errorf("map = %v, unread = %v, stale = %v; want nil and nothing reported", m, rep.unread.Load(), rep.stale.Load())
	}
}
