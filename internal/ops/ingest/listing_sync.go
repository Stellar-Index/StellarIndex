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
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listing-sync caches the Stellar slice of an independent
// price-aggregation platform's own per-coin platform→address map into
// the `asset_listing_directory` table (migration 0160): one HTTPS GET of
// the public coin catalogue, a second for the prices of just the coins
// that carry a Stellar address, a full upsert, and a prune of the rows
// the upstream no longer carries.
//
// Run from an hourly timer:
//
//	stellarindex-ops listing-sync -config /etc/stellarindex.toml
//
// Sibling of `directory-sync` in every structural respect — same flag
// set, same fail-closed write gate, same "refuse an empty parse so a
// broken fetch can never prune the table to nothing" guard — and a
// sibling in posture too: what it caches CORROBORATES an address, it
// does not attest to one. There are no scam flags in that table and it
// admits none; an absence from it means "not listed", which is the
// normal condition of nearly every asset on the network.
//
// Scale, measured against the live upstream 2026-09-15: 21,247 coin
// objects / 3.7 MB, of which exactly 50 carry a non-empty
// `platforms.stellar` — 17 Soroban contract C-strkeys and 33 classic
// `CODE-GISSUER` pairs. The price call then covers those 50 ids in one
// request.
const (
	listingSource = "coingecko"

	// listingProBaseURL / listingDemoBaseURL — a Pro key authenticates
	// ONLY against the pro host; the public host answers a Pro key with
	// HTTP 400 / error_code 10010. This is the same auto-selection the
	// price poller does (internal/sources/external/coingecko), and it
	// exists so an operator who provisions a paid key does not also have
	// to know that the hostname changes.
	listingProBaseURL  = "https://pro-api.coingecko.com"
	listingDemoBaseURL = "https://api.coingecko.com"

	listingCataloguePath = "/api/v3/coins/list"
	listingMarketsPath   = "/api/v3/coins/markets"

	// listingStellarPlatformKey is the upstream's chain key for Stellar
	// inside each coin's `platforms` object.
	listingStellarPlatformKey = "stellar"

	// listingMaxResponseBytes bounds every response read. The catalogue
	// is ~3.7 MB today; 64 MB is an order-of-magnitude guard against a
	// response that never ends from a compromised or misconfigured
	// -base-url, not a tight fit — the same reasoning, and the same kind
	// of headroom, as directoryMaxTarballBytes.
	listingMaxResponseBytes = 64 << 20

	// listingMarketsBatch is the upstream's maximum page size for the
	// markets endpoint.
	//
	// The id list is CHUNKED into batches of this size rather than
	// walked with `page=1,2,3…`. Both stay inside the same limit, but
	// they ask different questions: the markets endpoint orders by market
	// cap, so paging over a filtered id set asks "give me the Nth page of
	// a ranking that is moving underneath me" — a coin whose cap crosses
	// a page boundary between two requests is returned twice or not at
	// all. Chunking asks a fixed question per request ("these 250 ids"),
	// which cannot lose a member.
	listingMarketsBatch = 250

	// listingFetchTimeout bounds each HTTP request including the body
	// read. http.DefaultClient has NO timeout: a third-party host that
	// accepts the connection and then stops sending would hang this
	// command forever, which looks like a slow network rather than a
	// bug. The run's own -timeout ctx bounds it as well; this makes the
	// bound unconditional.
	listingFetchTimeout = 60 * time.Second
)

// listingAddressForm is which of the two Stellar address shapes a row
// carries. The two are disjoint and together exhaustive — the same split
// migration 0160's CHECK makes, and the one the storage census's
// arithmetic relies on.
type listingAddressForm int

const (
	listingFormUnknown listingAddressForm = iota
	listingFormContract
	listingFormClassic
)

// listingContractRe / listingClassicRe match the two address forms the
// table's CHECK accepts. Anything matching NEITHER is skipped and
// counted rather than failing the run — the same posture as
// directoryAddressRe, and for the same reason: a malformed upstream row
// is upstream's problem, and letting it abort the whole hourly pass
// hands a third party a switch that turns this sync off.
//
// Two details that are easy to get wrong and both occur live:
//
//   - The classic code class is `[A-Za-z0-9]`, not `[A-Z0-9]`. Stellar
//     asset codes are case-SENSITIVE alphanumerics, and four listed
//     assets carry a lowercase letter (MXNe, sUSD, yUSDC, yETH). An
//     uppercase-only pattern silently discards all four as malformed.
//   - A classic CODE may itself begin with C (CETES-, C1USD-), so the
//     contract test has to be the anchored 56-char strkey pattern and
//     never a "starts with C" shortcut.
var (
	listingContractRe = regexp.MustCompile(`^C[A-Z2-7]{55}$`)
	listingClassicRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$`)
)

// listingCatalogueRow is the subset of one `/coins/list?include_platform=true`
// element this sync reads. `platforms` maps a chain key to the address
// the coin has on that chain; a chain the coin is not on is either
// absent or carries an empty string.
type listingCatalogueRow struct {
	ID        string            `json:"id"`
	Symbol    string            `json:"symbol"`
	Platforms map[string]string `json:"platforms"`
}

// listingMarket is the subset of one `/coins/markets` element this sync
// reads.
//
// CurrentPrice is a [json.Number] — deliberately, and this is the ADR-0003
// boundary for this path. The upstream ships the price as a JSON number,
// and a JSON number decoded into a float64 has already lost digits by the
// time anything looks at it; there is no later point at which the loss
// can be detected, because a float64 prints back as a plausible decimal.
// json.Number keeps the LITERAL the upstream printed, and that literal is
// what reaches the NUMERIC column.
type listingMarket struct {
	ID           string      `json:"id"`
	CurrentPrice json.Number `json:"current_price"`
	LastUpdated  string      `json:"last_updated"`
}

// listingPrice is one upstream price together with the upstream's OWN
// publication time. Both or neither: a price whose publication time is
// missing or unparseable is not a price this sync will carry, because
// its freshness is then unverifiable and the storage layer's 24-hour
// price bound is measured on exactly that column.
type listingPrice struct {
	USD string
	At  time.Time
}

// listingCounts is what one pass saw, reported in the run summary so an
// operator reading the journal can tell a shrinking upstream from a
// parser that stopped recognising it.
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
	// PriceRejected counts upstream price records DISCARDED because they
	// carried a price with no usable publication time. Counted rather
	// than silently dropped: it is the number that distinguishes "the
	// platform publishes no price for these" from "the platform changed
	// its timestamp format and this sync now serves nothing".
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

	client := newListingClient(*baseURL)
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
//
// The key comes from the environment, not from config: `COINGECKO_API_KEY`
// (Pro) and `COINGECKO_DEMO_API_KEY` (Demo) are the two variables the
// indexer already reads for exactly this credential, and both are already
// provisioned in /etc/default/stellarindex on the deployed host. Adding a
// third spelling in a config file would mean an operator could set the key
// and still have this command run unauthenticated, with nothing to say why.
//
// An override passed as -base-url wins over the key-derived host, so an
// operator can point the run at a mirror or a proxy without also having to
// change which key it sends.
func newListingClient(baseURL string) *listingClient {
	c := &listingClient{
		authMode: "none",
		baseURL:  listingDemoBaseURL,
		http: &http.Client{
			Timeout:       listingFetchTimeout,
			CheckRedirect: keyedSameOriginRedirect("listing-sync"),
		},
	}
	if k := strings.TrimSpace(os.Getenv("COINGECKO_API_KEY")); k != "" {
		c.key, c.keyHeader, c.authMode, c.baseURL = k, "x-cg-pro-api-key", "pro", listingProBaseURL
	} else if k := strings.TrimSpace(os.Getenv("COINGECKO_DEMO_API_KEY")); k != "" {
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
	// G10-04: the key goes in a request HEADER, never the query string.
	// A transport error's *url.Error embeds the request URL in its
	// message, so a key in the query string leaks into every log line
	// that reports a failed fetch. The upstream still accepts the
	// `x_cg_*_api_key` query parameters — those are the leaky form.
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

// parseListingCatalogue streams the catalogue array one element at a time and
// returns the Stellar-carrying entries plus the census of the pass.
//
// Streaming rather than unmarshalling the whole array: 21k coin objects
// each holding a platforms map is a large live heap to build in order to
// keep 50 of them, on a host that also runs a captive stellar-core,
// Postgres and ClickHouse. Only one coin object is live at a time here.
//
// A malformed ADDRESS is data and is skipped + counted. A JSON decode
// failure mid-array is NOT — the stream is unsynchronised at that point
// and nothing after it can be trusted, so it is fatal. That is the same
// line directory-sync draws between a bad account file and a bad tarball.
func parseListingCatalogue(r io.Reader) ([]timescale.ListingEntry, listingCounts, error) {
	var (
		counts  listingCounts
		entries []timescale.ListingEntry
	)
	dec := json.NewDecoder(r)
	// UseNumber for the same reason listingMarket.CurrentPrice is a
	// json.Number: nothing on this path may turn an upstream number into
	// a float64. The catalogue carries none today, and the decoder is
	// configured so that it still would not if one appeared.
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
	// An empty result means the upstream layout changed or the fetch was
	// truncated, not that a platform listing ~50 Stellar assets delisted
	// all of them within the hour. Refuse here rather than hand
	// ReplaceListingDirectory a set that would prune the table to
	// nothing — the storage layer refuses it too, and the message is more
	// useful from this side.
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

// fetchListingPrices pulls the published USD price for exactly the given
// coin ids, in chunks of listingMarketsBatch, and returns the merged
// map plus the number of upstream records rejected for want of a usable
// publication time.
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
		batch, batchRejected, err := parseMarkets(io.LimitReader(resp.Body, listingMaxResponseBytes))
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

// parseMarkets streams a markets response into id → price.
//
// A coin is ABSENT from the result whenever it has no price, or has a
// price with no parseable `last_updated`. Absence is the whole encoding
// of "unpriced" — there is no zero-value entry that a later reader could
// mistake for a price of nothing, and no timestamp is invented to stand
// in for one the upstream did not publish. A fabricated `priced_at`
// would defeat the storage layer's 24-hour price bound completely, since
// that bound is measured on precisely that column.
//
// The rejected count is returned rather than logged here so the caller
// can put it in the run summary: a price record discarded silently is
// indistinguishable from a coin the platform never priced.
func parseMarkets(r io.Reader) (map[string]listingPrice, int, error) {
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
		if err != nil || at.IsZero() {
			rejected++
			continue
		}
		out[id] = listingPrice{USD: price, At: at.UTC()}
	}
	return out, rejected, nil
}

// applyListingPrices fills in the price columns of the entries that have
// one, and returns how many did. Entries with no upstream price are left
// exactly as they were: an empty price string and a zero time, which is
// what the storage layer stores as NULL/NULL.
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
