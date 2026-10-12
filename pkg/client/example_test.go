package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// ExampleNew demonstrates the canonical SDK construction. The
// Options shape is intentionally small — anonymous use needs only
// a BaseURL; authenticated use adds an APIKey.
func ExampleNew() {
	// Construct against the public production endpoint:
	c := client.New(client.Options{
		BaseURL: "https://api.stellarindex.io",
		APIKey:  "sip_…", // optional; anonymous works at low rate limit
	})
	_ = c // silence "declared and not used" in this snippet

	// For a self-hosted deployment, point BaseURL at it:
	selfHosted := client.New(client.Options{BaseURL: "http://localhost:3000"})
	_ = selfHosted
}

// Example: current-price lookup. The Envelope carries the price plus advisory flags.
// The caller decides whether to act on `flags.stale`, `flags.divergence_warning` or `flags.frozen`.
func ExampleClient_Price() {
	// Stand up a fake server returning a representative response so
	// the example is self-contained + verified at build time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"asset_id": "native",
				"quote": "fiat:USD",
				"price": "0.13245",
				"price_type": "vwap",
				"observed_at": "2026-05-02T12:00:00Z",
				"window_seconds": 60
			},
			"as_of": "2026-05-02T12:00:00Z",
			"sources": ["binance", "kraken"],
			"flags": {
				"stale": false,
				"reduced_redundancy": false,
				"triangulated": false,
				"divergence_warning": false
			}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Price(ctx, client.PriceQuery{
		Asset: "native",
		Quote: "fiat:USD",
	})
	if err != nil {
		// Production handlers should distinguish APIError shape
		// (status-typed) from network errors.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			fmt.Printf("api error %d: %s\n", apiErr.Status, apiErr.Title)
			return
		}
		fmt.Println("request failed:", err)
		return
	}

	fmt.Printf("XLM/USD = %s (sources: %v, stale: %v)\n",
		resp.Data.Price, resp.Sources, resp.Flags.Stale)

	// Output: XLM/USD = 0.13245 (sources: [binance kraken], stale: false)
}

// Fetches the rich asset detail wallet UIs need: decimals, SEP-1 overlay, F2 supply fields and SEP-1 issuance
// declarations.
func ExampleClient_Asset() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"kind": "stellar_asset",
				"asset_id": "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				"type": "classic",
				"code": "USDC",
				"issuer": "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				"home_domain": "centre.io",
				"decimals": 7,
				"sep1_status": "verified",
				"name": "USD Coin",
				"description": "Centre-issued USDC stablecoin",
				"image": "https://centre.io/assets/usdc.svg",
				"org_name": "Centre Consortium",
				"anchor_asset": "USD",
				"anchor_asset_type": "fiat",
				"circulating_supply": "12345678900000000",
				"total_supply": "12345678900000000",
				"market_cap_usd": "1234567890.00",
				"supply_basis": "issuer_exclusion",
				"volume_24h_usd": "987654.32",
				"is_unlimited": false
			},
			"as_of": "2026-05-02T12:00:00Z",
			"flags": {
				"stale": false,
				"reduced_redundancy": false,
				"triangulated": false,
				"divergence_warning": false
			}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	resp, err := c.Asset(context.Background(), "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		fmt.Println(err)
		return
	}

	// resp.Data is an AssetLookup (ADR-0042): a canonical asset_id resolves to the stellar_asset branch; StellarAsset()
	// returns ok=false for a catalogue slug like "usdc" instead of a zero-valued struct.
	asset, ok := resp.Data.StellarAsset()
	if !ok {
		fmt.Println("unexpected kind:", resp.Data.Kind())
		return
	}
	fmt.Printf("%s (%s) — sep1=%s, circulating=%s, market_cap=$%s\n",
		asset.Code, *asset.Name, asset.Sep1Status,
		*asset.CirculatingSupply, *asset.MarketCapUSD)

	// Output: USDC (USD Coin) — sep1=verified, circulating=12345678900000000, market_cap=$1234567890.00
}

// Historical time series: granularity is fixed per request, with one bucket per step from inception (or as far back as
// trades go) to the latest closed bucket.
func ExampleClient_HistorySinceInception() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"asset_id": "native",
				"quote": "fiat:USD",
				"granularity": "1d",
				"points": [
					{"t": "2026-04-01T00:00:00Z", "p": "0.13201"},
					{"t": "2026-04-02T00:00:00Z", "p": "0.13280"},
					{"t": "2026-04-03T00:00:00Z", "p": "0.13345"}
				]
			},
			"as_of": "2026-05-02T12:00:00Z",
			"flags": {"stale": false, "reduced_redundancy": false, "triangulated": false, "divergence_warning": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	resp, err := c.HistorySinceInception(context.Background(), client.HistoryQuery{
		Asset:       "native",
		Quote:       "fiat:USD",
		Granularity: "1d",
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("XLM/USD daily points: %d (first %s)\n",
		len(resp.Data.Points), resp.Data.Points[0].T.Format("2006-01-02"))
	// Output: XLM/USD daily points: 3 (first 2026-04-01)
}

// ExampleClient_Assets demonstrates the paginated asset catalogue.
// Use Cursor/Limit for cursor-based pagination; the response's
// Pagination block carries `next_cursor` for the follow-up call.
func ExampleClient_Assets() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"asset_id": "native", "type": "native", "decimals": 7},
				{"asset_id": "USDC-GA5...", "type": "classic", "code": "USDC", "decimals": 7}
			],
			"as_of": "2026-05-02T12:00:00Z",
			"flags": {"stale": false, "reduced_redundancy": false, "triangulated": false, "divergence_warning": false},
			"pagination": {"next": "eyJsYXN0IjoiVVNEQy1HQTUuLi4ifQ=="}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	resp, err := c.Assets(context.Background(), client.AssetsOptions{Limit: 2})
	if err != nil {
		fmt.Println(err)
		return
	}
	// Pagination is a pointer: nil when the response carries no page
	// cursor. Always nil-check before reading .Next.
	hasNext := resp.Pagination != nil && resp.Pagination.Next != ""
	fmt.Printf("first page: %d assets, next=%t\n", len(resp.Data), hasNext)
	// Output: first page: 2 assets, next=true
}

// Example: authenticated-caller identity, returning the key's tier and remaining rate-limit budget.
// Lets consumers self-throttle without parsing `X-RateLimit-*` headers on every call.
func ExampleClient_Me() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"identifier": "GAVATAR...",
				"tier": "apikey",
				"key_id": "ak_demo_abc123",
				"label": "demo wallet",
				"rate_limit_per_min": 1000,
				"created_at": "2026-04-15T10:00:00Z"
			},
			"as_of": "2026-05-02T12:00:00Z",
			"flags": {"stale": false, "reduced_redundancy": false, "triangulated": false, "divergence_warning": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL, APIKey: "ak_demo_abc123"})
	resp, err := c.Me(context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("tier=%s, budget=%d rpm, label=%q\n",
		resp.Data.Tier, resp.Data.RateLimitPerMin, resp.Data.Label)
	// Output: tier=apikey, budget=1000 rpm, label="demo wallet"
}

// ExampleAPIError demonstrates the status-typed error helpers the
// SDK exposes — wallet integrators handle 404 / 429 / 5xx with
// explicit predicates rather than parsing error strings.
func ExampleAPIError() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{
			"type": "https://api.stellarindex.io/errors/asset-not-found",
			"title": "Asset not found",
			"status": 404,
			"detail": "No trades observed for asset_id=XYZ-G..."
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	_, err := c.Asset(context.Background(), "XYZ-GUNKNOWNISSUER")

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.IsNotFound():
			fmt.Println("asset is not indexed")
		case apiErr.IsRateLimited():
			fmt.Println("back off — server rate-limited us")
		case apiErr.IsServerError():
			fmt.Println("server-side issue; consider retry with backoff")
		default:
			fmt.Println("client-side error:", apiErr.Title)
		}
	}

	// Output: asset is not indexed
}

// /v1/status always returns 200; degraded state lives in the body's Overall field, so dashboards can poll without
// alerting on 503s.
func ExampleClient_Status() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"overall": "ok",
				"region": {"name": "r1", "deployment": "production"},
				"services": [
					{"name":"api","status":"ok"},
					{"name":"indexer","status":"ok"},
					{"name":"aggregator","status":"ok"}
				],
				"latency": {"p50_ms": 0.6, "p95_ms": 3.85, "p99_ms": 4.77, "window_secs": 300},
				"freshness": {"active_sources": 13, "total_sources": 17},
				"incidents": {"active_count": 0}
			},
			"as_of": "2026-05-05T15:00:00Z",
			"flags": {"stale": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Status(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s — p95=%.2fms, %d/%d sources active\n",
		got.Data.Overall,
		got.Data.Latency.P95Ms,
		*got.Data.Freshness.ActiveSources,
		*got.Data.Freshness.TotalSources)

	// Output: ok — p95=3.85ms, 13/17 sources active
}

// Home-page aggregate snapshot in one round trip. `Volume24hUSD` is `*string` per ADR-0003 (can exceed int64); nil means
// no USD-equivalent trades, so render `—`, not zero. `total_sources` counts the static registry, not the runtime-enabled
// set /v1/status reports.
func ExampleClient_NetworkStats() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"volume_24h_usd": "3176584845.46668496",
				"markets_count_24h": 23848,
				"assets_indexed": 86175,
				"latest_ledger": 62490950,
				"exchange_sources": 11,
				"total_sources": 21
			},
			"as_of": "2026-05-09T15:09:00Z",
			"flags": {"stale": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.NetworkStats(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	vol := "—"
	if got.Data.Volume24hUSD != nil {
		vol = *got.Data.Volume24hUSD
	}
	fmt.Printf("ledger=%d, markets=%d, assets=%d, exchange/total=%d/%d, vol24h_usd=%s\n",
		got.Data.LatestLedger,
		got.Data.MarketsCount24h,
		got.Data.AssetsIndexed,
		got.Data.ExchangeSources,
		got.Data.TotalSources,
		vol)

	// Output: ledger=62490950, markets=23848, assets=86175, exchange/total=11/21, vol24h_usd=3176584845.46668496
}

// Incident-postmortem corpus for the status site; ResolvedAt is `*time.Time`, nil for still-active incidents. The corpus
// ships with the binary, so regions on different builds may differ; diff by Slug for cross-region consistency.
func ExampleClient_Incidents() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"incidents": [
					{
						"slug": "2026-05-06-postgres-lock-table-full",
						"title": "[SEV-3] Indexer dropping ~1% of trades",
						"severity": "SEV-3",
						"status": "resolved",
						"started_at": "2026-05-06T15:00:00Z",
						"resolved_at": "2026-05-06T22:39:00Z",
						"affected_components": ["indexer"],
						"body_markdown": "..."
					}
				],
				"count": 1
			},
			"as_of": "2026-05-09T15:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Incidents(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, inc := range got.Data.Incidents {
		state := "ongoing"
		if inc.ResolvedAt != nil {
			state = "resolved " + inc.ResolvedAt.Format("2006-01-02 15:04Z")
		}
		fmt.Printf("[%s/%s] %s — %s\n", inc.Severity, inc.Status, inc.Slug, state)
	}

	// Output: [SEV-3/resolved] 2026-05-06-postgres-lock-table-full — resolved 2026-05-06 22:39Z
}

// Lists Blend pools seen in the trailing 7d auction stream, sorted by total auctions desc; AuctionsTotal is lifetime.
// Wire shape is auction-derived today and will gain fields; decode ignores unknown fields, so callers must not switch on
// field count.
func ExampleClient_LendingPools() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"protocol": "blend",
					"pool": "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD",
					"auctions_24h": 29,
					"auctions_total": 5691,
					"unique_users_30d": 4,
					"last_seen": "2026-05-09T13:34:58Z"
				}
			],
			"as_of": "2026-05-09T15:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.LendingPools(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, p := range got.Data {
		// Truncate the contract ID for display — full C-strkeys are
		// 56 chars and don't render well in tables.
		short := p.Pool
		if len(short) > 12 {
			short = short[:6] + "…" + short[len(short)-4:]
		}
		fmt.Printf("%s/%s: %d auctions/24h, %d total, %d users/30d\n",
			p.Protocol, short, p.Auctions24h, p.AuctionsTotal, p.UniqueUsers30d)
	}

	// Output: blend/CAJJZS…BXBD: 29 auctions/24h, 5691 total, 4 users/30d
}

// The SAC wrapper registry maps a Soroban contract address to the underlying classic asset's "<CODE>:<G_STRKEY>" form,
// used to resolve `transfer` events on SAC contracts. It is operator config (~20 entries), stable across regions with
// the same config.
func ExampleClient_SACWrappers() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA": "native",
				"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
			},
			"as_of": "2026-05-09T15:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.SACWrappers(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// Look up a specific Soroban contract — useful when a swap
	// event's base/quote came in as a C-strkey and the explorer
	// wants to render the underlying asset symbol.
	const xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	if asset, ok := got.Data[xlmSAC]; ok {
		fmt.Printf("XLM SAC (%s…) wraps: %s\n", xlmSAC[:6], asset)
	}

	// Output: XLM SAC (CAS3J7…) wraps: native
}

// Binned price + USD-volume series over a caller-chosen window with server-default granularity per timeframe (24h to 1h
// bins, 7d to 4h, 30d to 1d). Use HistorySinceInception instead for the full series at one granularity.
func ExampleClient_Chart() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"asset_id": "native",
				"quote": "fiat:USD",
				"granularity": "1h",
				"timeframe": "24h",
				"price_type": "vwap",
				"points": [
					{"t": "2026-05-08T22:00:00Z", "p": "0.158", "v_usd": "20808.05"},
					{"t": "2026-05-08T23:00:00Z", "p": "0.161", "v_usd": "31204.18"}
				]
			},
			"as_of": "2026-05-08T23:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Chart(context.Background(), client.ChartQuery{
		Asset: "native", Timeframe: "24h",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s/%s @ %s, %d points\n",
		got.Data.AssetID, got.Data.Quote, got.Data.Granularity, len(got.Data.Points))
	for _, p := range got.Data.Points {
		fmt.Printf("  %s: %s\n", p.T.Format("15:04Z"), p.P)
	}

	// Output:
	// native/fiat:USD @ 1h, 2 points
	//   22:00Z: 0.158
	//   23:00Z: 0.161
}

// Rawest per-source trade view per ADR-0018: one row per source with a trade on (asset, quote), before the aggregator
// layer. Empty array (not 404) when none; flags.stale is always false here; Aggregate="latest" collapses to the newest
// observation across sources.
func ExampleClient_Observations() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"source": "binance",
					"ledger": 0,
					"tx_hash": "",
					"ts": "2026-05-09T15:00:00Z",
					"base_asset": "native",
					"quote_asset": "fiat:USD",
					"base_amount": "100",
					"quote_amount": "16.0",
					"price": "0.16"
				},
				{
					"source": "sdex",
					"ledger": 62490950,
					"tx_hash": "abc",
					"ts": "2026-05-09T14:59:50Z",
					"base_asset": "native",
					"quote_asset": "fiat:USD",
					"base_amount": "1000000",
					"quote_amount": "159000",
					"price": "0.159"
				}
			],
			"as_of": "2026-05-09T15:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Observations(context.Background(), client.ObservationsQuery{
		Asset: "native", Quote: "fiat:USD",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, row := range got.Data {
		price := "no price" // a zero-leg fill has a null price
		if row.Price != nil {
			price = *row.Price
		}
		fmt.Printf("%s @ %s: %s\n", row.Source, row.Timestamp.Format("15:04:05Z"), price)
	}

	// Output:
	// binance @ 15:00:00Z: 0.16
	// sdex @ 14:59:50Z: 0.159
}

// Latest multi-window delta rollup per (entity_type, entity_id), written every 5 min; EntityType is "coin" or "pair"
// (coin slugs like XLM are expanded server-side). H*/D* fields are *float64: nil means no value yet, so render `—`, not
// 0%.
func ExampleClient_ChangeSummary() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"entity_type": "coin",
				"entity_id": "crypto:XLM",
				"refreshed_at": "2026-05-09T15:00:00Z",
				"current_value": "0.163",
				"h24_delta_pct": 3.21,
				"d7_delta_pct": -1.8,
				"streak_direction": "up",
				"acceleration": "increasing"
			},
			"as_of": "2026-05-09T15:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.ChangeSummary(context.Background(), client.ChangeSummaryQuery{
		EntityType: "coin", EntityID: "crypto:XLM",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	pct := func(p *float64) string {
		if p == nil {
			return "—"
		}
		return fmt.Sprintf("%+.2f%%", *p)
	}
	fmt.Printf("%s: $%s, 24h=%s 7d=%s (%s, %s)\n",
		got.Data.EntityID,
		got.Data.CurrentValue, // money is a string end to end
		pct(got.Data.H24DeltaPct),
		pct(got.Data.D7DeltaPct),
		got.Data.StreakDirection,
		got.Data.Acceleration)

	// Output: crypto:XLM: $0.163, 24h=+3.21% 7d=-1.80% (up, increasing)
}

// Shallow liveness probe: 200 + status="ok" when healthy, 503 (mapped to APIError) otherwise; anonymous-friendly.
// Distinct from Readyz (dependency check) and Status (customer rollup).
func ExampleClient_Healthz() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {"status": "ok", "uptime": "50h26m"},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Healthz(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("status=%s uptime=%s\n", got.Data.Status, got.Data.Uptime)

	// Output: status=ok uptime=50h26m
}

// Example: deep readiness probe that pings every backing dependency; 200 + status="ok" when all pass, 503 otherwise.
// Use for readiness gating, distinct from Healthz, which only tests that the process is alive.
func ExampleClient_Readyz() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {"status": "ok", "uptime": "50h26m"},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Readyz(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("ready: %s\n", got.Data.Status)

	// Output: ready: ok
}

// API build metadata (semver tag, commit, build date, Go version, dirty flag) for client-side build-skew detection and
// incident logs.
func ExampleClient_Version() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"version": "v0.5.0-rc.37",
				"build_date": "2026-05-08T11:41:13Z",
				"commit": "8dc0b9d",
				"dirty": "false",
				"go_version": "go1.25.9"
			},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Version(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s (commit %s, %s)\n",
		got.Data.Version, got.Data.Commit, got.Data.GoVersion)

	// Output: v0.5.0-rc.37 (commit 8dc0b9d, go1.25.9)
}

// Per-day usage rollup (requests, errors, throttled) for billing reconciliation and quota dashboards. Requires
// authentication (anonymous gets 401); returns an empty array when there is no usage in the window.
func ExampleClient_Usage() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"date":"2026-05-10","requests":12345,"errors":12,"throttled":0},
				{"date":"2026-05-09","requests":11890,"errors":8,"throttled":2}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL, APIKey: "sip_demo"})
	got, err := c.Usage(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, row := range got.Data {
		fmt.Printf("%s: %d requests (%d err, %d throttled)\n",
			row.Date, row.Requests, row.Errors, row.Throttled)
	}

	// Output:
	// 2026-05-10: 12345 requests (12 err, 0 throttled)
	// 2026-05-09: 11890 requests (8 err, 2 throttled)
}

// Issues a new API key. `Plaintext` is the only place the secret appears and the server never returns it again, so store
// it immediately. Requires authentication; the key inherits the caller's identifier and tier.
func ExampleClient_CreateKey() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"data": {
				"key_id": "ak_demo_xyz789",
				"plaintext": "sip_live_abc123def456ghi789jkl012mno345pqr",
				"label": "production-server-1"
			},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL, APIKey: "sip_demo"})
	got, err := c.CreateKey(context.Background(), client.CreateKeyRequest{
		Label: "production-server-1",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// In production, IMMEDIATELY persist got.Data.Plaintext
	// somewhere safe — the server will never return it again.
	fmt.Printf("created %s (label=%s, plaintext=%d chars)\n",
		got.Data.KeyID, got.Data.Label, len(got.Data.Plaintext))

	// Output: created ak_demo_xyz789 (label=production-server-1, plaintext=42 chars)
}

// Revocation is permanent; a new key needs CreateKey. keyID is the public ID (KeyCreated.KeyID / Keys rows), not the
// plaintext secret. An unknown, already-revoked or foreign ID, or a plaintext secret, returns nil (204); *APIError for
// 400 (operator without X-Reason), 401, 409, 503.
func ExampleClient_RevokeKey() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL, APIKey: "sip_demo"})
	err := c.RevokeKey(context.Background(), "ak_demo_xyz789")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("revoked")

	// Output: revoked
}

// Example: list every API key the caller owns (KeyID, Label, tier, creation time).
// The plaintext secret is never returned; the server shows it once, at CreateKey time.
func ExampleClient_Keys() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"identifier":"GAUSER","tier":"apikey","key_id":"ak_a",
				 "label":"production-server-1","created_at":"2026-05-01T10:00:00Z"},
				{"identifier":"GAUSER","tier":"apikey","key_id":"ak_b",
				 "label":"staging","created_at":"2026-05-08T14:30:00Z"}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL, APIKey: "sip_demo"})
	got, err := c.Keys(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, k := range got.Data {
		fmt.Printf("%s — %s (created %s)\n",
			k.KeyID, k.Label, k.CreatedAt.Format("2006-01-02"))
	}

	// Output:
	// ak_a — production-server-1 (created 2026-05-01)
	// ak_b — staging (created 2026-05-08)
}

// Issuer directory sorted by total observation count across the issuer's classic assets, descending. HomeDomain/OrgName
// are empty strings (not nil) until the SEP-1 fetcher resolves stellar.toml.
func ExampleClient_Issuers() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"g_strkey":"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				 "home_domain":"centre.io","org_name":"Centre Consortium",
				 "asset_count":1,"total_observation_count":50000000},
				{"g_strkey":"GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
				 "home_domain":"aqua.network","org_name":"Aquarius",
				 "asset_count":1,"total_observation_count":14764050}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Issuers(context.Background(), client.IssuersOptions{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, iss := range got.Data {
		fmt.Printf("%s — %d observations across %d assets\n",
			iss.OrgName, iss.TotalObservationCount, iss.AssetCount)
	}

	// Output:
	// Centre Consortium — 50000000 observations across 1 assets
	// Aquarius — 14764050 observations across 1 assets
}

// Example: one issuer's detail by G-strkey: SEP-1 overlay plus classic assets it minted that we observe trades for.
// Auth flags are pointer-nil pre-resolution; render `—` rather than fabricating false.
func ExampleClient_Issuer() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"g_strkey": "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				"home_domain": "centre.io",
				"org_name": "Centre Consortium",
				"sep1_resolved_at": "2026-05-10T08:30:00Z",
				"assets": [
					{"asset_id":"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
					 "code":"USDC","slug":"USDC",
					 "first_seen_ledger":1000000,"last_seen_ledger":62500000,
					 "observation_count":50000000}
				]
			},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Issuer(context.Background(),
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s (%s) issues %d asset(s)\n",
		got.Data.OrgName, got.Data.HomeDomain, len(got.Data.Assets))
	for _, a := range got.Data.Assets {
		fmt.Printf("  - %s (%d observations)\n", a.Code, a.ObservationCount)
	}

	// Output:
	// Centre Consortium (centre.io) issues 1 asset(s)
	//   - USDC (50000000 observations)
}

// SEP-1 overlay: issuer-declared stellar.toml fields, distinct from the observed F2 supply fields on AssetDetail.
// Overlay fields populate only when Sep1Status == "verified"; other states leave them nil.
func ExampleClient_AssetMetadata() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"asset_id": "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				"home_domain": "centre.io",
				"sep1_status": "verified",
				"name": "USD Coin",
				"description": "Centre-issued USDC stablecoin",
				"image": "https://centre.io/assets/usdc.svg",
				"org_name": "Centre Consortium",
				"anchor_asset": "USD",
				"anchor_asset_type": "fiat",
				"is_unlimited": false
			},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.AssetMetadata(context.Background(),
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	if got.Data.Sep1Status != "verified" {
		fmt.Println("not verified yet")
		return
	}
	fmt.Printf("%s (%s) — anchored to %s\n",
		*got.Data.Name, *got.Data.OrgName, *got.Data.AnchorAsset)

	// Output: USD Coin (Centre Consortium) — anchored to USD
}

// Example: raw trade rows over an arbitrary [From, To) window, unlike HistorySinceInception's bucketed VWAP/TWAP.
// Paginate via opaque `Cursor` while Pagination is non-nil and Pagination.Next is non-empty.
func ExampleClient_History() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"source":"binance","ledger":0,"tx_hash":"abc","op_index":0,
				 "ts":"2026-05-10T11:30:00Z","base_asset":"native","quote_asset":"fiat:USD",
				 "base_amount":"100000000","quote_amount":"16475000","price":"0.16475"},
				{"source":"kraken","ledger":0,"tx_hash":"def","op_index":0,
				 "ts":"2026-05-10T11:30:05Z","base_asset":"native","quote_asset":"fiat:USD",
				 "base_amount":"50000000","quote_amount":"8240000","price":"0.16480"}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {},
			"pagination": {"next": ""}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	from := time.Date(2026, 5, 10, 11, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	got, err := c.History(context.Background(), client.HistoryRangeQuery{
		Base: "native", Quote: "fiat:USD", From: from, To: to, Limit: 100,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, t := range got.Data {
		if t.Price == nil { // a zero-leg fill has no price
			continue
		}
		fmt.Printf("%s %s @ %s\n",
			t.Source, t.Timestamp.Format("15:04:05"), *t.Price)
	}

	// Output:
	// binance 11:30:00 @ 0.16475
	// kraken 11:30:05 @ 0.16480
}

// ExampleClient_Currency was removed: /v1/currencies/{ticker} was folded into /v1/assets (asset_class=fiat) and the SDK
// has no Currency method.

// Per-source ingest cursors: which sources are caught up or lagging, for operator dashboards. Multi-position sources
// return one row per (source, sub_source); `LagSeconds` is computed server-side, so no clock sync is needed.
func ExampleClient_Cursors() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"source":"ledgerstream","last_ledger":62505721,
				 "last_updated":"2026-05-10T12:00:00Z","lag_seconds":2},
				{"source":"backfill","sub_source":"60000000-62000000:soroswap",
				 "last_ledger":62000000,"last_updated":"2026-05-09T18:30:00Z",
				 "lag_seconds":63000}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Cursors(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, cur := range got.Data {
		fmt.Printf("%s: ledger=%d (lag=%ds)\n",
			cur.Source, cur.LastLedger, cur.LagSeconds)
	}

	// Output:
	// ledgerstream: ledger=62505721 (lag=2s)
	// backfill: ledger=62000000 (lag=63000s)
}

// Bulk pricing in one round trip; each snapshot is from the same closed-bucket window `/v1/price` would serve. Routing
// is automatic (up to 100 ids GET, 101..1000 POST); `flags.stale` is the OR over rows; assets with no data are omitted,
// so diff input vs output.
func ExampleClient_PriceBatch() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"asset_id":"native","quote":"fiat:USD","price":"0.16475","price_type":"vwap","observed_at":"2026-05-10T12:00:00Z","window_seconds":60},
				{"asset_id":"crypto:BTC","quote":"fiat:USD","price":"81000.00","price_type":"vwap","observed_at":"2026-05-10T12:00:00Z","window_seconds":60}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {"stale": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.PriceBatch(context.Background(), client.PriceBatchQuery{
		AssetIDs: []string{"native", "crypto:BTC"},
		Quote:    "fiat:USD",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, row := range got.Data {
		fmt.Printf("%s = %s\n", row.AssetID, row.Price)
	}

	// Output:
	// native = 0.16475
	// crypto:BTC = 81000.00
}

// Single-pair lookup returning a slice of Market rows, one per source, even for a single source; the empty array is the
// canonical "no such pair" signal, so callers needn't branch on 1-vs-many or 0-vs-404.
func ExampleClient_Pair() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"base":"native","quote":"fiat:USD","last_trade_at":"2026-05-10T12:00:00Z",
				 "trade_count_24h":5400,"volume_24h_usd":"45000.00"},
				{"base":"native","quote":"fiat:USD","last_trade_at":"2026-05-10T11:59:55Z",
				 "trade_count_24h":7200,"volume_24h_usd":"53000.50"}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Pair(context.Background(), "native", "fiat:USD")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	totalTrades := int64(0)
	for _, m := range got.Data {
		totalTrades += m.TradeCount24h
	}
	fmt.Printf("XLM/USD: %d sources, %d trades in last 24h\n",
		len(got.Data), totalTrades)

	// Output: XLM/USD: 2 sources, 12600 trades in last 24h
}

// Fastest-feed tip: trades cross-region byte-identical consistency (which `/v1/price` gives) for lower latency.
// `?window_seconds=` defaults to 5 s; a value outside [1, 60] is rejected with 400, not clamped. `price_type` is "vwap"
// when the window had trades or "last_trade" on fallback; the server never emits "tip".
func ExampleClient_PriceTip() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"asset_id": "native",
				"quote": "fiat:USD",
				"price": "0.16475",
				"price_type": "vwap",
				"observed_at": "2026-05-10T12:00:42Z",
				"window_seconds": 5
			},
			"as_of": "2026-05-10T12:00:42Z",
			"sources": ["binance"],
			"flags": {"stale": false}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	resp, err := c.PriceTip(context.Background(), client.PriceTipQuery{
		Asset:         "native",
		Quote:         "fiat:USD",
		WindowSeconds: 5,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("XLM/USD tip: %s (type: %s, window: %ds)\n",
		resp.Data.Price, resp.Data.PriceType, resp.Data.WindowSeconds)

	// Output: XLM/USD tip: 0.16475 (type: vwap, window: 5s)
}

// ExampleClient_Sources lists the source registry; only `exchange` class rows count toward the canonical VWAP.
// Aggregators and oracles are reported but excluded, since mixing them would double-count upstream markets.
func ExampleClient_Sources() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"name":"binance","class":"exchange","subclass":"cex",
				 "include_in_vwap":true,"backfill_available":true,"backfill_safe":true,"default_weight":100},
				{"name":"soroswap","class":"exchange","subclass":"dex",
				 "include_in_vwap":true,"backfill_available":true,"backfill_safe":true,"default_weight":100},
				{"name":"reflector-dex","class":"oracle",
				 "include_in_vwap":false,"backfill_available":true,"backfill_safe":true,"default_weight":100}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Sources(context.Background(), client.SourcesOptions{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, s := range got.Data {
		// Only `exchange` class contributes to VWAP per project policy.
		fmt.Printf("%-15s class=%-10s vwap=%v\n", s.Name, s.Class, s.IncludeInVWAP)
	}

	// Output:
	// binance         class=exchange   vwap=true
	// soroswap        class=exchange   vwap=true
	// reflector-dex   class=oracle     vwap=false
}

// ExampleClient_Markets lists every observed (base, quote) pair, sorted by 24h USD volume descending. Server-side limit:
// Limit <= 500; larger requests return 400.
func ExampleClient_Markets() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"base":"native","quote":"crypto:BTC",
				 "last_trade_at":"2026-05-10T12:00:00Z","trade_count_24h":4827,"volume_24h_usd":"125000.00"},
				{"base":"native","quote":"fiat:USD",
				 "last_trade_at":"2026-05-10T11:59:50Z","trade_count_24h":12345,"volume_24h_usd":"98000.50"}
			],
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	got, err := c.Markets(context.Background(), client.MarketsOptions{
		Limit:   25,
		OrderBy: client.MarketsOrderByVolume24hUSDDesc,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, m := range got.Data {
		vol := "—"
		if m.Volume24hUSD != nil {
			vol = "$" + *m.Volume24hUSD
		}
		fmt.Printf("%s/%s: vol_24h=%s trades_24h=%d\n",
			m.Base, m.Quote, vol, m.TradeCount24h)
	}

	// Output:
	// native/crypto:BTC: vol_24h=$125000.00 trades_24h=4827
	// native/fiat:USD: vol_24h=$98000.50 trades_24h=12345
}

// ExampleClient_OHLC fetches one bar over an arbitrary [from, to) range; for a series use `/v1/chart`. Truncated=true
// means the bar hit the per-bar trade-count cap; narrow the range.
func ExampleClient_OHLC() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"from": "2026-05-10T11:00:00Z",
				"to":   "2026-05-10T12:00:00Z",
				"open":  "0.16412",
				"high":  "0.16498",
				"low":   "0.16387",
				"close": "0.16475",
				"base_volume":  "85432.10",
				"quote_volume": "14082.41",
				"trade_count":  217,
				"truncated":    false
			},
			"as_of": "2026-05-10T12:00:00Z",
			"flags": {}
		}`))
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	from := time.Date(2026, 5, 10, 11, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	got, err := c.OHLC(context.Background(), client.OHLCQuery{
		Base:  "native",
		Quote: "fiat:USD",
		From:  from,
		To:    to,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	bar := got.Data
	fmt.Printf("o=%s h=%s l=%s c=%s vol=%s trades=%d\n",
		bar.Open, bar.High, bar.Low, bar.Close, bar.BaseVolume, bar.TradeCount)

	// Output: o=0.16412 h=0.16498 l=0.16387 c=0.16475 vol=85432.10 trades=217
}
