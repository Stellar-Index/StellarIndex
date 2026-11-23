package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Route fixtures: AQUA, BAR and QUX are excluded ("A"); FOO, BAZ and ZED are not ("B"). AQUA/FOO back stablecoin catalogue
// entries, BAR/BAZ crypto ones, QUX/ZED are classic-only.
var (
	rtAQUA = "AQUA-" + aquaIssuer
	rtFOO  = "FOO-" + aquaIssuer
	rtBAR  = "BAR-" + aquaIssuer
	rtBAZ  = "BAZ-" + aquaIssuer
	rtQUX  = "QUX-" + aquaIssuer
	rtZED  = "ZED-" + aquaIssuer
	// rtCaps ranks the excluded assets above their clean siblings.
	rtCaps = map[string]string{rtAQUA: "9000", rtFOO: "8000", rtBAR: "7000", rtBAZ: "6000", rtQUX: "5000", rtZED: "4000"}
	rtA    = map[string]bool{rtAQUA: true, rtBAR: true, rtQUX: true, "rt-aqua": true, "rt-bar": true}
	rtB    = map[string]bool{rtFOO: true, rtBAZ: true, rtZED: true, "rt-foo": true, "rt-baz": true}
)

const rtCatalogueYAML = `verified_currencies:
  - {ticker: AQUA, slug: rt-aqua, name: Rt Aqua, description: x, class: stablecoin,
     networks: [{network: stellar, code: AQUA, issuer: ` + aquaIssuer + `, asset_id: AQUA-` + aquaIssuer + `}]}
  - {ticker: FOO, slug: rt-foo, name: Rt Foo, description: x, class: stablecoin,
     networks: [{network: stellar, code: FOO, issuer: ` + aquaIssuer + `, asset_id: FOO-` + aquaIssuer + `}]}
  - {ticker: BAR, slug: rt-bar, name: Rt Bar, description: x, class: crypto,
     networks: [{network: stellar, code: BAR, issuer: ` + aquaIssuer + `, asset_id: BAR-` + aquaIssuer + `}]}
  - {ticker: BAZ, slug: rt-baz, name: Rt Baz, description: x, class: crypto,
     networks: [{network: stellar, code: BAZ, issuer: ` + aquaIssuer + `, asset_id: BAZ-` + aquaIssuer + `}]}
`

func rtRow(id string) timescale.AssetRow {
	code, issuer, _ := strings.Cut(id, "-")
	return timescale.AssetRow{
		AssetID: id, Code: code, IssuerGStrkey: issuer, ObservationCount: 10,
		PriceUSD: sptrLocal("0.5"), Volume24hUSD: sptrLocal("1000"), MarketCapUSD: sptrLocal(rtCaps[id]), CirculatingSupply: sptrLocal("10000"),
		Change1hPct: sptrLocal("+1.00"), Change24hPct: sptrLocal("-2.00"), Change7dPct: sptrLocal("+3.00"),
	}
}

// rtAssets serves the six classic rows and XLM; every other read is empty.
type rtAssets struct{ AssetsReader }

func (rtAssets) ListAssetsExt(_ context.Context, o timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	var out []timescale.AssetRow
	for _, id := range []string{rtAQUA, rtFOO, rtBAR, rtBAZ, rtQUX, rtZED} {
		if r := rtRow(id); o.Code == "" || (o.Code == r.Code && o.Issuer == r.IssuerGStrkey) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (rtAssets) GetAssetByAssetID(_ context.Context, id string) (timescale.AssetRow, error) {
	if _, ok := rtCaps[id]; ok {
		return rtRow(id), nil
	}
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (rtAssets) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	r := rtRow("native")
	r.AssetID, r.Code, r.IssuerGStrkey, r.MarketCapUSD = "native", "XLM", "", sptrLocal("3000")
	return r, nil
}

func (rtAssets) GetAssetBySlug(context.Context, string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (rtAssets) GetAssetTopMarkets(context.Context, string, int) ([]timescale.AssetTopMarket, error) {
	return nil, nil
}

func (rtAssets) GetAssetPriceHistory24h(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (rtAssets) GetAssetPriceHistory7d(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (rtAssets) GetAssetsPriceHistory24hBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (rtAssets) GetAssetsPriceHistory7dBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}
func (rtAssets) GetAssetMarketsCount(context.Context, string) (int64, error)      { return 0, nil }
func (rtAssets) GetAssetATH(context.Context, string) (*timescale.AssetATH, error) { return nil, nil }
func (rtAssets) GetAssetsATHBatch(context.Context, []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}
func (rtAssets) GetAssetTradeCount24h(context.Context, string) (int64, error) { return 0, nil }

// rtLean is the lean AssetReader: identity for the detail, priced rows for the legacy listing.
type rtLean struct{}

func (rtLean) GetAsset(_ context.Context, a canonical.Asset) (AssetDetail, error) {
	return AssetDetail{AssetID: a.String(), Code: a.Code, Type: string(a.Type)}, nil
}

func (rtLean) ListAssets(context.Context, string, int) ([]AssetDetail, string, error) {
	var out []AssetDetail
	for _, id := range []string{rtAQUA, rtFOO} {
		code, _, _ := strings.Cut(id, "-")
		out = append(out, AssetDetail{AssetID: id, Code: code, Type: "credit_alphanum4", PriceUSD: sptrLocal("0.5"), MarketCapUSD: sptrLocal(rtCaps[id])})
	}
	return out, "", nil
}

// rtExplorer resolves the SAC of any classic asset; down fails the lookup.
type rtExplorer struct {
	ExplorerReader
	down atomic.Bool
}

func (e *rtExplorer) SACClassicAssetName(_ context.Context, contract string) (string, bool, error) {
	if e.down.Load() {
		return "", false, errors.New("lake down")
	}
	for id := range rtCaps {
		a := mustParseAsset(id)
		if sac, _ := a.SacContractID(); sac == contract {
			return a.Code + ":" + a.Issuer, true, nil
		}
	}
	return "", false, nil
}

func (*rtExplorer) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, errors.New("not in fixture")
}

func rtSAC(t *testing.T, id string) string {
	t.Helper()
	sac, err := mustParseAsset(id).SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	return sac
}

func rtExclusions(now time.Time, bases ...string) PriceExclusionGate {
	var rows []timescale.SourceExclusion
	for i, b := range bases {
		q := "fiat:USD"
		if b == "native" {
			q = xlmAnchorQuotes[0].String()
		}
		rows = append(rows, timescale.SourceExclusion{ID: int64(i + 1), Source: "sdex", Base: b, Quote: q, From: now.Add(-time.Hour)})
	}
	return pricingguard.NewSourceExclusionGate(globalExclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{})
}

type rtServer struct {
	*Server
	explorer *rtExplorer
	h        http.Handler
}

func newRTServer(t *testing.T, lean bool, gate PriceExclusionGate) rtServer {
	t.Helper()
	cat, err := currency.LoadFromBytes([]byte(rtCatalogueYAML))
	if err != nil {
		t.Fatal(err)
	}
	ex := &rtExplorer{}
	o := Options{Assets: rtLean{}, Exclusions: gate}
	if !lean {
		o.AssetsReader, o.VerifiedCurrencies, o.Explorer = rtAssets{}, cat, ex
	}
	s := New(o)
	return rtServer{Server: s, explorer: ex, h: s.Handler()}
}

var rtAsOf = regexp.MustCompile(`"(as_of|price_as_of)":"[^"]*"`)

func (s rtServer) get(t *testing.T, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rtAsOf.ReplaceAllString(rec.Body.String(), `"$1":""`)
}

// rtPriceDerived are the row fields a withheld price takes with it.
var rtPriceDerived = []string{
	"price_usd", "market_cap_usd", "fdv_usd", "change_1h_pct", "change_24h_pct", "change_7d_pct", "ath",
	"price_history_24h", "price_history_7d", "listing_reference", "listing_valuation", "global_market", "volume_24h_usd",
}

func rtWantWithheld(t *testing.T, where string, row map[string]any) {
	t.Helper()
	if row["price_withheld_reason"] != string(PriceWithheldSourceExcluded) {
		t.Errorf("%s %v: price_withheld_reason = %v, want source_excluded", where, row["asset_id"], row["price_withheld_reason"])
	}
	for _, f := range rtPriceDerived {
		if v, ok := row[f]; ok {
			if pts, series := v.([]any); series && !rtSeriesPriced(pts) {
				continue
			}
			t.Errorf("%s %v: %s = %v served while withheld", where, row["asset_id"], f, v)
		}
	}
}

func rtSeriesPriced(pts []any) bool {
	for _, p := range pts {
		if m, ok := p.(map[string]any); ok && m["p"] != nil {
			return true
		}
	}
	return false
}

func rtDecode(t *testing.T, body string) (rows []map[string]any, one map[string]any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if strings.HasPrefix(string(env.Data), "[") {
		if err := json.Unmarshal(env.Data, &rows); err != nil {
			t.Fatal(err)
		}
		return rows, nil
	}
	if err := json.Unmarshal(env.Data, &one); err != nil {
		t.Fatal(err)
	}
	return nil, one
}

func rtJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAssetRoutes_ExclusionGatesEveryVariant drives every listing variant and detail id form through the router: with no
// exclusion the body is the ungated body; with one covering A, A's price-derived values are absent with source_excluded and a
// clean B on the same page is byte-identical to its clean row.
func TestAssetRoutes_ExclusionGatesEveryVariant(t *testing.T) {
	now := time.Now()
	aquaSAC, fooSAC := rtSAC(t, rtAQUA), rtSAC(t, rtFOO)
	excludedA := []string{rtAQUA, rtBAR, rtQUX}
	cases := []struct {
		name, path string
		lean       bool
		excluded   []string
		// replay: request once to fill the listing cache, then fail the SAC lookup so the page replays from it.
		replay, cached bool
		noRows         bool
	}{
		{name: "classic", path: "/v1/assets"},
		{name: "classic cached", path: "/v1/assets", cached: true},
		{name: "lean reader", path: "/v1/assets", lean: true},
		{name: "fiat", path: "/v1/assets?asset_class=fiat", noRows: true},
		{name: "stablecoin", path: "/v1/assets?asset_class=stablecoin"},
		{name: "crypto", path: "/v1/assets?asset_class=crypto"},
		{name: "all top-up", path: "/v1/assets?asset_class=all"},
		{name: "all next page", path: "/v1/assets?asset_class=all&limit=3"},
		{name: "all catalogue boundary", path: "/v1/assets?asset_class=all&limit=4"},
		{name: "all classic phase", path: "/v1/assets?asset_class=all&cursor=classic:"},
		{name: "q=SAC", path: "/v1/assets?q=" + aquaSAC},
		{name: "q=SAC replay", path: "/v1/assets?q=" + aquaSAC, replay: true},
		{name: "detail CODE-ISSUER A", path: "/v1/assets/" + rtAQUA},
		{name: "detail CODE-ISSUER B", path: "/v1/assets/" + rtFOO},
		{name: "detail SAC A", path: "/v1/assets/" + aquaSAC},
		{name: "detail SAC B", path: "/v1/assets/" + fooSAC},
		{name: "detail native A", path: "/v1/assets/native", excluded: []string{"native"}},
		{name: "detail native B", path: "/v1/assets/native"},
		{name: "detail crypto:XLM A", path: "/v1/assets/crypto:XLM", excluded: []string{"native"}},
		{name: "detail crypto:XLM B", path: "/v1/assets/crypto:XLM"},
		{name: "detail slug A", path: "/v1/assets/rt-aqua"},
		{name: "detail slug B", path: "/v1/assets/rt-foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.excluded == nil {
				tc.excluded = excludedA
			}
			isA := func(row map[string]any) bool {
				id, _ := row["asset_id"].(string)
				if slug, ok := row["slug"].(string); ok && id == "" {
					id = slug // the slug detail is a GlobalAssetView
				}
				return rtA[id] || (slices.Contains(tc.excluded, "native") && id == "native")
			}
			ungated := newRTServer(t, tc.lean, nil).get(t, tc.path)
			if empty := newRTServer(t, tc.lean, rtExclusions(now)).get(t, tc.path); empty != ungated {
				t.Fatalf("empty exclusion set changed the body:\n got %s\nwant %s", empty, ungated)
			}
			srv := newRTServer(t, tc.lean, rtExclusions(now, tc.excluded...))
			body := srv.get(t, tc.path)
			if tc.replay || tc.cached {
				srv.explorer.down.Store(tc.replay)
				body = srv.get(t, tc.path)
			}
			cleanRows, cleanOne := rtDecode(t, ungated)
			rows, one := rtDecode(t, body)
			if one != nil {
				if isA(one) {
					if cleanOne["price_usd"] == nil {
						t.Fatalf("fixture serves no detail price: %s", ungated)
					}
					rtWantWithheld(t, tc.name, one)
				} else if got, want := rtJSON(t, one), rtJSON(t, cleanOne); got != want {
					t.Errorf("unexcluded detail changed:\n got %s\nwant %s", got, want)
				}
				return
			}
			if tc.noRows {
				if len(rows) != 0 {
					t.Fatalf("want an empty page, got %s", body)
				}
				return
			}
			clean := map[string]string{}
			if strings.Contains(tc.path, "asset_class=all") {
				// A withheld row ranks lower, so a clean row from the next ungated page can move onto this one.
				all, _ := rtDecode(t, newRTServer(t, false, nil).get(t, "/v1/assets?asset_class=all"))
				cleanRows = append(all, cleanRows...)
			}
			for _, r := range cleanRows {
				clean[r["asset_id"].(string)] = rtJSON(t, r)
			}
			var seenA, seenB bool
			for _, r := range rows {
				id, _ := r["asset_id"].(string)
				switch {
				case rtA[id]:
					seenA = true
					if !strings.Contains(clean[id], `"price_usd"`) {
						t.Fatalf("fixture serves no price on %s: %s", id, ungated)
					}
					rtWantWithheld(t, tc.name, r)
				case rtB[id]:
					seenB = true
					if got := rtJSON(t, r); got != clean[id] {
						t.Errorf("unexcluded row %s changed:\n got %s\nwant %s", id, got, clean[id])
					}
				}
			}
			if !seenA || !seenB {
				t.Fatalf("page holds no excluded and clean pair (A %v, B %v): %s", seenA, seenB, body)
			}
		})
	}
}

// TestCatalogueRanking_WithheldRowRanksAsUnpriced: a catalogue row whose cap an exclusion withholds ranks with the rows that
// have none, not in the slot the hidden cap earned; paging by the served-slug cursor still serves every row exactly once.
func TestCatalogueRanking_WithheldRowRanksAsUnpriced(t *testing.T) {
	srv := newRTServer(t, false, rtExclusions(time.Now(), rtAQUA, rtBAR, rtQUX))
	page := func(path string) (ids []string, next string) {
		var env struct {
			Data       []map[string]any `json:"data"`
			Pagination *struct {
				Next string `json:"next"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal([]byte(srv.get(t, path)), &env); err != nil {
			t.Fatal(err)
		}
		for _, r := range env.Data {
			ids = append(ids, r["asset_id"].(string))
		}
		if env.Pagination != nil {
			next = env.Pagination.Next
		}
		return ids, next
	}
	if got, _ := page("/v1/assets?asset_class=stablecoin"); !slices.Equal(got, []string{"rt-foo", "rt-aqua"}) {
		t.Errorf("stablecoin order = %v, want the withheld rt-aqua after the priced rt-foo", got)
	}
	var served []string
	path := "/v1/assets?asset_class=all&limit=1"
	for range 4 {
		ids, next := page(path)
		served = append(served, ids...)
		path = "/v1/assets?asset_class=all&limit=1&cursor=" + url.QueryEscape(next)
	}
	if want := []string{"rt-foo", "rt-baz", "rt-aqua", "rt-bar"}; !slices.Equal(served, want) {
		t.Errorf("catalogue pages served %v, want %v (priced by cap, then withheld in catalogue order, each once)", served, want)
	}
}
