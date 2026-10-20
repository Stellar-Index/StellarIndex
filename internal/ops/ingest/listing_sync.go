// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listing-sync caches the Stellar slice of a price-aggregation platform's per-coin platform-to-address map into asset_listing_directory (migration 0160); run hourly.
// Sibling of directory-sync: same flags, fail-closed write gate, and refusal of an empty parse so a broken fetch can never prune the table.
// What it caches corroborates an address, it does not attest to one; absence means "not listed".
const (
	listingSource = "coingecko"

	// A Pro key authenticates ONLY against the pro host (the public host answers it with HTTP 400 / error_code 10010);
	// same auto-selection as internal/sources/external/coingecko.
	listingProBaseURL  = "https://pro-api.coingecko.com"
	listingDemoBaseURL = "https://api.coingecko.com"

	listingCataloguePath = "/api/v3/coins/list"
	listingMarketsPath   = "/api/v3/coins/markets"

	// listingStellarPlatformKey is the upstream's chain key for Stellar
	// inside each coin's `platforms` object.
	listingStellarPlatformKey = "stellar"

	// listingMaxResponseBytes bounds every response read: an order-of-magnitude guard (catalogue is ~3.7 MB) against an endless
	// response from a bad -base-url, as with directoryMaxTarballBytes.
	listingMaxResponseBytes = 64 << 20

	// listingMarketsBatch is the upstream's maximum page size for the markets endpoint.
	// Ids are chunked rather than paged: the endpoint orders by market cap, so paging a filtered set can return a coin twice or never as the ranking moves.
	listingMarketsBatch = 250

	// listingFetchTimeout bounds each HTTP request including the body read; http.DefaultClient has no timeout,
	// so a stalled third-party host would hang the command forever.
	listingFetchTimeout = 60 * time.Second
)

// listingAddressForm is which of the two Stellar address shapes a row carries; disjoint and exhaustive, matching migration 0160's
// CHECK.
type listingAddressForm int

const (
	listingFormUnknown listingAddressForm = iota
	listingFormContract
	listingFormClassic
)

// listingContractRe / listingClassicRe match the two address forms the table's CHECK accepts; anything else is skipped and counted, so a malformed upstream row cannot abort the hourly pass.
// The classic code class is [A-Za-z0-9] (codes are case-sensitive; MXNe, sUSD, yUSDC, yETH are listed), and a code may start with C (CETES-), so the contract test must be the anchored 56-char strkey, never "starts with C".
var (
	listingContractRe = regexp.MustCompile(`^C[A-Z2-7]{55}$`)
	listingClassicRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$`)
)

// listingCatalogueRow is the subset of one `/coins/list?include_platform=true` element this sync reads; `platforms` maps chain key to
// address, and an absent chain is missing or an empty string.
type listingCatalogueRow struct {
	ID        string            `json:"id"`
	Symbol    string            `json:"symbol"`
	Platforms map[string]string `json:"platforms"`
}

// listingMarket is the subset of one /coins/markets element this sync reads.
// CurrentPrice is a [json.Number] (ADR-0003): a float64 decode loses digits undetectably, while json.Number keeps the literal that reaches the NUMERIC column.
type listingMarket struct {
	ID           string      `json:"id"`
	CurrentPrice json.Number `json:"current_price"`
	LastUpdated  string      `json:"last_updated"`
}

// listingPrice is one upstream price with the upstream's own publication time. Both or neither:
// without a parseable time its freshness is unverifiable, and the storage layer's 24-hour price bound is measured on that column.
type listingPrice struct {
	USD string
	At  time.Time
}

// listingCounts is what one pass saw, so the journal can tell a shrinking upstream from a parser that stopped recognising it.
type listingCounts struct {
	// Scanned is every element in the upstream catalogue.
	Scanned int
	// Stellar is the coins carrying a non-empty platforms.stellar.
	Stellar int
	// Contracts / Classic split those by address form.
	Contracts int
	Classic   int
	// Skipped counts Stellar-carrying coins dropped because the address
	// matched neither form, or because the coin carried no id to trace
	// the row back to. Skipped, never fatal.
	Skipped int
	// Priced is how many entries ended up carrying a price.
	Priced int
	// PriceRejected counts price records discarded for lacking a usable publication time; it distinguishes
	// "no price published" from "the timestamp format changed".
	PriceRejected int
}

func listingSync(args []string) error {
	fs := flag.NewFlagSet("listing-sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	baseURL := fs.String("base-url", "",
		"API base URL (https only). Empty selects the host that matches the API key mode: the Pro host when COINGECKO_API_KEY is set, the public host otherwise.")
	timeout := fs.Duration("timeout", 2*time.Minute, "Wall-clock timeout for the whole run")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *baseURL != "" && !strings.HasPrefix(*baseURL, "https://") {
		return fmt.Errorf("-base-url must be https:// (got %q)", *baseURL)
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := newListingClient(*baseURL, cfg.External.CoinGecko)
	fmt.Printf("Listing source %s at %s (auth_mode=%s).\n",
		listingSource, client.baseURL, client.authMode)

	entries, counts, err := client.fetchStellarListings(ctx)
	if err != nil {
		return err
	}

	prices, rejected, err := client.fetchListingPrices(ctx, listingIDsOf(entries))
	if err != nil {
		return err
	}
	counts.PriceRejected = rejected
	counts.Priced = applyListingPrices(entries, prices)

	fmt.Printf("Scanned %d coins; %d carry a Stellar address (%d contracts, %d classic); %d skipped-malformed.\n",
		counts.Scanned, counts.Stellar, counts.Contracts, counts.Classic, counts.Skipped)
	fmt.Printf("Priced %d of %d entries (%d upstream price records rejected: no usable publication time).\n",
		counts.Priced, len(entries), counts.PriceRejected)

	if dryRun {
		fmt.Println("Dry run — nothing written.")
		return nil
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	upserted, pruned, err := store.ReplaceListingDirectory(ctx, entries, listingSource)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d upserted, %d pruned (source=%s).\n", upserted, pruned, listingSource)
	return nil
}

// listingClient holds the resolved host + auth for one run.
type listingClient struct {
	baseURL   string
	keyHeader string
	key       string
	authMode  string
	http      *http.Client
}

// newListingClient resolves the API key and, from it, the host.
// The keys are the indexer's own [external.coingecko] pair (COINGECKO_API_KEY / COINGECKO_DEMO_API_KEY); a -base-url override wins over the key-derived host.
func newListingClient(baseURL string, keys config.CoinGeckoVenueConfig) *listingClient {
	c := &listingClient{
		authMode: "none",
		baseURL:  listingDemoBaseURL,
		http:     httpx.NewKeyedClient("listing-sync", listingFetchTimeout),
	}
	if k := strings.TrimSpace(keys.APIKey); k != "" {
		c.key, c.keyHeader, c.authMode, c.baseURL = k, "x-cg-pro-api-key", "pro", listingProBaseURL
	} else if k := strings.TrimSpace(keys.DemoAPIKey); k != "" {
		c.key, c.keyHeader, c.authMode, c.baseURL = k, "x-cg-demo-api-key", "demo", listingDemoBaseURL
	}
	if baseURL != "" {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
	return c
}

// get issues one bounded GET and returns the response for the caller to
// read and close.
func (c *listingClient) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("listing-sync: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// The key goes in a request HEADER, never the query string: a transport error's *url.Error embeds the URL,
	// so a query-string key would leak into every log line reporting a failed fetch.
	if c.key != "" {
		req.Header.Set(c.keyHeader, c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing-sync: fetch %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("listing-sync: fetch %s: HTTP %d: %s",
			path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// fetchStellarListings pulls the full coin catalogue and keeps the rows
// that carry a Stellar address.
func (c *listingClient) fetchStellarListings(ctx context.Context) ([]timescale.ListingEntry, listingCounts, error) {
	resp, err := c.get(ctx, listingCataloguePath, url.Values{"include_platform": {"true"}})
	if err != nil {
		return nil, listingCounts{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return parseListingCatalogue(io.LimitReader(resp.Body, listingMaxResponseBytes))
}

// parseListingCatalogue streams the catalogue array one element at a time and returns the Stellar-carrying entries plus the census of the pass.
// Streaming avoids building ~21k coin objects on a host that also runs core, Postgres and ClickHouse. A malformed address is skipped and counted; a JSON decode failure mid-array is fatal because the stream is unsynchronised.
func parseListingCatalogue(r io.Reader) ([]timescale.ListingEntry, listingCounts, error) {
	var (
		counts  listingCounts
		entries []timescale.ListingEntry
	)
	dec := json.NewDecoder(r)
	// UseNumber for the same reason listingMarket.CurrentPrice is a json.Number: nothing on this path may turn an upstream
	// number into a float64. The catalogue carries none today, and the decoder is configured so that it still would not if one
	// appeared.
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, counts, fmt.Errorf("listing-sync: coins list: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, counts, fmt.Errorf("listing-sync: coins list: expected a JSON array, got %v", tok)
	}
	for dec.More() {
		var coin listingCatalogueRow
		if err := dec.Decode(&coin); err != nil {
			return nil, counts, fmt.Errorf("listing-sync: coins list element %d: %w", counts.Scanned, err)
		}
		counts.Scanned++

		addr := strings.TrimSpace(coin.Platforms[listingStellarPlatformKey])
		if addr == "" {
			continue
		}
		counts.Stellar++

		id := strings.TrimSpace(coin.ID)
		form, ok := classifyListingAddress(addr)
		if !ok || id == "" {
			counts.Skipped++
			continue
		}
		switch form {
		case listingFormContract:
			counts.Contracts++
		case listingFormClassic:
			counts.Classic++
		case listingFormUnknown:
			// Unreachable: classifyListingAddress returns ok=false with
			// this form, and the branch above already took it.
			counts.Skipped++
			continue
		}
		entries = append(entries, timescale.ListingEntry{
			Address:   addr,
			ListingID: id,
			Symbol:    strings.TrimSpace(coin.Symbol),
			Source:    listingSource,
		})
	}
	// An empty result means the upstream layout changed or the fetch was truncated, not a mass delisting;
	// refuse here rather than hand ReplaceListingDirectory a set that prunes the table to nothing.
	if len(entries) == 0 {
		return nil, counts, fmt.Errorf(
			"listing-sync: parsed 0 Stellar entries from %d coins (%d skipped) — refusing; upstream shape changed or the fetch was truncated",
			counts.Scanned, counts.Skipped)
	}
	return entries, counts, nil
}

// classifyListingAddress reports which address form addr carries, with
// ok=false for anything that is neither.
func classifyListingAddress(addr string) (listingAddressForm, bool) {
	switch {
	case listingContractRe.MatchString(addr):
		return listingFormContract, true
	case listingClassicRe.MatchString(addr):
		return listingFormClassic, true
	default:
		return listingFormUnknown, false
	}
}

// listingIDsOf collects the upstream coin ids to price, in entry order.
func listingIDsOf(entries []timescale.ListingEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ListingID)
	}
	return ids
}

// fetchListingPrices pulls the published USD price for exactly the given coin ids in chunks of listingMarketsBatch and returns the
// merged map plus the count of records rejected for lack of a usable publication time.
func (c *listingClient) fetchListingPrices(
	ctx context.Context, ids []string,
) (map[string]listingPrice, int, error) {
	out := make(map[string]listingPrice, len(ids))
	rejected := 0
	for start := 0; start < len(ids); start += listingMarketsBatch {
		end := min(start+listingMarketsBatch, len(ids))
		q := url.Values{
			"vs_currency": {"usd"},
			"ids":         {strings.Join(ids[start:end], ",")},
			"per_page":    {fmt.Sprint(listingMarketsBatch)},
			"page":        {"1"},
		}
		resp, err := c.get(ctx, listingMarketsPath, q)
		if err != nil {
			return nil, 0, err
		}
		batch, batchRejected, err := parseMarkets(
			io.LimitReader(resp.Body, listingMaxResponseBytes), time.Now())
		_ = resp.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		rejected += batchRejected
		for id, p := range batch {
			out[id] = p
		}
	}
	return out, rejected, nil
}

// parseMarkets streams a markets response into id -> price. A coin is ABSENT with no price or no parseable last_updated (or one beyond
// [timescale.ListingPriceMaxFutureSkew]): a fabricated priced_at would defeat the storage layer's 24-hour bound.
func parseMarkets(r io.Reader, now time.Time) (map[string]listingPrice, int, error) {
	ceiling := now.Add(timescale.ListingPriceMaxFutureSkew)
	out := map[string]listingPrice{}
	rejected := 0

	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, fmt.Errorf("listing-sync: markets: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, 0, fmt.Errorf("listing-sync: markets: expected a JSON array, got %v", tok)
	}
	for dec.More() {
		var m listingMarket
		if err := dec.Decode(&m); err != nil {
			return nil, 0, fmt.Errorf("listing-sync: markets element: %w", err)
		}
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		// The literal the upstream printed. The decoder has already
		// validated it as a JSON number, so it is a well-formed decimal;
		// nothing here re-parses it, which is the point.
		price := strings.TrimSpace(m.CurrentPrice.String())
		if price == "" {
			continue // the platform published no price — a normal state
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(m.LastUpdated))
		if err != nil || at.IsZero() || at.After(ceiling) {
			rejected++
			continue
		}
		out[id] = listingPrice{USD: price, At: at.UTC()}
	}
	return out, rejected, nil
}

// applyListingPrices fills price columns for entries that have one and returns how many did; the rest keep an empty price string and
// zero time, which storage stores as NULL/NULL.
func applyListingPrices(entries []timescale.ListingEntry, prices map[string]listingPrice) int {
	priced := 0
	for i := range entries {
		p, ok := prices[entries[i].ListingID]
		if !ok || p.USD == "" || p.At.IsZero() {
			continue
		}
		entries[i].PriceUSD = p.USD
		entries[i].PricedAt = p.At
		priced++
	}
	return priced
}
