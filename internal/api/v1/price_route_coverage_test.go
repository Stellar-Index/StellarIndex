package v1

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// priceCoverage is a route's state under the operator source-exclusion kill switch (stellarindex-ops source-exclude).
type priceCoverage struct {
	state  string // gated | partial | uncovered | none
	reason string // why, for partial / uncovered / a none that could look like a price route
}

const (
	covGated     = "gated"
	covPartial   = "partial"
	covUncovered = "uncovered"
	covNone      = "none"
)

var (
	gated            = priceCoverage{state: covGated}
	uncovered        = func(why string) priceCoverage { return priceCoverage{state: covUncovered, reason: why} }
	lendingUncovered = uncovered("USD values from the catalogue-fallback price")
)

// priceRouteCoverage is the reviewed census of every public operation: whether the source-exclusion gate covers what it serves.
// A new route fails TestPriceRouteCoverage_EveryRouteClassified until it is classified here; keep source_exclude.go's
// readSideNote in step with the uncovered rows.
var priceRouteCoverage = map[string]priceCoverage{
	"GET /v1/price":                         gated,
	"GET /v1/price/batch":                   gated,
	"POST /v1/price/batch":                  gated,
	"GET /v1/price/tip":                     gated,
	"GET /v1/price/tip/stream":              gated,
	"GET /v1/price/stream":                  gated,
	"GET /v1/vwap":                          gated,
	"GET /v1/twap":                          gated,
	"GET /v1/ohlc":                          gated,
	"GET /v1/oracle/lastprice":              gated,
	"GET /v1/oracle/prices":                 gated,
	"GET /v1/oracle/x_last_price":           gated,
	"GET /v1/assets/{asset_id}":             {state: covPartial, reason: "headline gated; ATH, sparkline, transitive fill, fiat-asset headline and non-USD declared-peg FX are not"},
	"GET /v1/price/at":                      uncovered("historical point read"),
	"GET /v1/price/changes":                 uncovered("change windows over stored prices"),
	"GET /v1/history":                       uncovered("stored price history"),
	"GET /v1/history/since-inception":       uncovered("stored price history"),
	"GET /v1/chart":                         uncovered("chart series"),
	"GET /v1/assets":                        uncovered("listing price_usd and sparkline"),
	"GET /v1/assets/verified":               uncovered("listing prices"),
	"GET /v1/external/assets":               uncovered("external asset prices"),
	"GET /v1/external/assets/{slug}":        uncovered("external asset prices"),
	"GET /v1/rwa/assets":                    uncovered("RWA prices"),
	"GET /v1/rwa/history":                   uncovered("RWA price history"),
	"GET /v1/rwa/premium":                   uncovered("RWA premium over the served price"),
	"GET /v1/stablecoins":                   uncovered("stablecoin USD valuation"),
	"GET /v1/observations":                  uncovered("raw per-source observations"),
	"GET /v1/observations/stream":           uncovered("raw per-source observations"),
	"GET /v1/oracle/latest":                 uncovered("oracle readings"),
	"GET /v1/oracle/streams":                uncovered("oracle readings"),
	"GET /v1/markets":                       uncovered("market prices and USD volume"),
	"GET /v1/markets/sources":               uncovered("per-source market figures"),
	"GET /v1/pools":                         uncovered("pool USD figures"),
	"GET /v1/pairs":                         uncovered("pair prices"),
	"GET /v1/protocols":                     uncovered("TVL USD"),
	"GET /v1/protocols/{name}":              uncovered("TVL USD"),
	"GET /v1/protocols/{name}/tvl":          uncovered("TVL USD"),
	"GET /v1/lending/pools":                 lendingUncovered,
	"GET /v1/lending/rates":                 lendingUncovered,
	"GET /v1/lending/protocols":             lendingUncovered,
	"GET /v1/lending/pools/{pool}/reserves": lendingUncovered,
	"GET /v1/lending/pools/{pool}/reserves/{asset}/history": lendingUncovered,
	"GET /v1/lending/pools/{pool}/config-history":           lendingUncovered,
	"GET /v1/lending/pools/{pool}/backstop":                 lendingUncovered,
	"GET /v1/lending/pools/{pool}/backstop/history":         lendingUncovered,
	"GET /v1/lending/accounts/{account}/earnings":           lendingUncovered,
	"GET /v1/lending/pools/{pool}/auctions":                 lendingUncovered,
	"GET /v1/lending/pools/{pool}/emissions":                lendingUncovered,
	"GET /v1/aggregators":                                   uncovered("aggregator reference prices"),
	"GET /v1/divergence":                                    uncovered("our price against references"),
	"GET /v1/divergence/series":                             uncovered("our price against references"),
	"GET /v1/anomalies":                                     uncovered("our_price against ref_price"),
	"GET /v1/mev":                                           uncovered("USD-valued MEV"),
	"GET /v1/sources":                                       uncovered("per-source volume_usd"),
	"GET /v1/accounts/{g_strkey}":                           uncovered("balance usd_value"),
	"GET /v1/accounts/{g_strkey}/trades":                    uncovered("trade usd_volume"),
	"GET /v1/accounts/{g_strkey}/graph":                     uncovered("counterparty flows in USD"),
	"GET /v1/accounts/{g_strkey}/graph/history":             uncovered("counterparty flows in USD"),
	"GET /v1/accounts/{g_strkey}/graph/cohort":              uncovered("holding price_usd and value_usd"),
	"GET /v1/pools/reserves":                                {state: covNone, reason: "reserve ratios, no USD price"},
	"GET /v1/liquidity-pools":                               {state: covNone, reason: "reserve ratios, no USD price"},
	"GET /v1/sdex/orderbook":                                {state: covNone, reason: "raw offers in quote units"},
	"GET /v1/accounts/{g_strkey}/positions":                 {state: covNone, reason: "positions carry no USD price"},
	"GET /v1/search":                                        {state: covNone, reason: "no USD price"},
	"GET /v1/coverage":                                      {state: covNone, reason: "coverage verdicts, not prices"},
}

// notPriceServing lists the operations that serve no price, valuation or USD figure.
var notPriceServing = []string{
	"GET /v1/healthz", "GET /v1/readyz", "GET /v1/livez/lake", "GET /v1/version", "GET /v1/status", "GET /v1/status/notices",
	"GET /v1/assets/{asset_id}/metadata", "GET /v1/assets/{asset_id}/supply", "GET /v1/assets/{asset_id}/supply/flows",
	"GET /v1/assets/{asset_id}/holders", "GET /v1/assets/{asset_id}/movements", "GET /v1/assets/{asset_id}/entry-changes",
	"GET /v1/assets/{asset_id}/trustline-history", "GET /v1/issuers", "GET /v1/issuers/{g_strkey}",
	"GET /v1/contracts/{contract_id}/transfers", "GET /v1/changes/{entity_type}/{id}", "GET /v1/diagnostics/cursors",
	"GET /v1/diagnostics/ingestion", "GET /v1/diagnostics/archive", "GET /v1/diagnostics/backups", "GET /v1/incidents",
	"GET /v1/incidents.atom", "GET /v1/ledger/tip", "GET /v1/ledger/stream", "GET /v1/network/stats",
	"GET /v1/network/throughput", "GET /v1/methodology", "GET /v1/sources/{name}/health", "GET /v1/sac-wrappers",
	"GET /v1/account/me", "GET /v1/account/usage", "GET /v1/account/keys", "GET /v1/admin/accounts/{id}",
	"GET /v1/admin/status-notices", "GET /v1/signup/verify", "GET /v1/dashboard/account/export", "GET /v1/dashboard/keys",
	"GET /v1/dashboard/webhooks", "GET /v1/dashboard/webhooks/{id}/deliveries", "GET /v1/dashboard/price-alerts", "GET /v1/auth/callback",
	"GET /v1/auth/passkey/credentials", "GET /v1/auth/sep10/challenge", "GET /v1/ledgers", "GET /v1/ledgers/at",
	"GET /v1/ledgers/{seq}", "GET /v1/ledgers/{seq}/transactions", "GET /v1/ledgers/{seq}/operations", "GET /v1/tx/{hash}",
	"GET /v1/operations", "GET /v1/contracts", "GET /v1/contracts/stats", "GET /v1/contracts/{contract_id}",
	"GET /v1/contracts/{contract_id}/wasm", "GET /v1/contracts/{contract_id}/interactions",
	"GET /v1/contracts/{contract_id}/code-history", "GET /v1/accounts", "GET /v1/directory", "GET /v1/accounts/stats",
	"GET /v1/accounts/creators", "GET /v1/accounts/sponsors", "GET /v1/accounts/{g_strkey}/transactions",
	"GET /v1/accounts/{g_strkey}/operations", "GET /v1/accounts/{g_strkey}/entry-changes",
	"GET /v1/accounts/{g_strkey}/claimable-balances", "GET /v1/accounts/{g_strkey}/signer-on",
	"GET /v1/accounts/{g_strkey}/sponsorships", "GET /v1/accounts/{g_strkey}/movements", "GET /v1/accounts/{g_strkey}/activity",
}

// specOperations returns every "METHOD /v1/path" in the OpenAPI spec, which lint-docs keeps in parity with the mux.
func specOperations(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for path, methods := range doc.Paths {
		for method := range methods {
			switch m := strings.ToUpper(method); m {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				ops = append(ops, m+" /v1"+path)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

func TestPriceRouteCoverage_EveryRouteClassified(t *testing.T) {
	none := map[string]bool{}
	for _, op := range notPriceServing {
		if _, dup := priceRouteCoverage[op]; dup || none[op] {
			t.Errorf("%s is listed twice", op)
		}
		none[op] = true
	}
	spec := map[string]bool{}
	for _, op := range specOperations(t) {
		spec[op] = true
		method, _, _ := strings.Cut(op, " ")
		_, listed := priceRouteCoverage[op]
		// A mutation other than POST /v1/price/batch serves no price; every read must be classified.
		if !listed && !none[op] && method == http.MethodGet {
			t.Errorf("%s is not classified: add it to priceRouteCoverage (gated, partial, uncovered + reason) or notPriceServing", op)
		}
	}
	for op, c := range priceRouteCoverage {
		if !spec[op] {
			t.Errorf("priceRouteCoverage lists %s, which the spec does not define", op)
		}
		if (c.state == covPartial || c.state == covUncovered) && c.reason == "" {
			t.Errorf("%s is %s with no reason", op, c.state)
		}
	}
	for op := range none {
		if !spec[op] {
			t.Errorf("notPriceServing lists %s, which the spec does not define", op)
		}
	}
}

// TestPriceRouteCoverage_ClassifiedRoutesAreMounted checks the census against the real router, so a row cannot name a
// pattern the server does not register.
func TestPriceRouteCoverage_ClassifiedRoutesAreMounted(t *testing.T) {
	s := New(Options{})
	param := regexp.MustCompile(`\{[^}]+\}`)
	for op := range priceRouteCoverage {
		method, path, _ := strings.Cut(op, " ")
		req := httptest.NewRequest(method, param.ReplaceAllString(path, "x"), nil)
		if _, pattern := s.mux.Handler(req); pattern != op {
			t.Errorf("%s: the mux routes it to %q", op, pattern)
		}
	}
}
