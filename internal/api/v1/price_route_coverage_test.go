package v1

import (
	"cmp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/ops/incident"
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
// A new route fails TestPriceRouteCoverage_EveryRouteClassified until it is classified here, and
// TestPriceRouteCoverage_OperatorNoteMatchesCensus until incident.ReadSideNote tells the operator the same.
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
	"GET /v1/assets/{asset_id}":             gated,
	"GET /v1/price/at":                      gated,
	"GET /v1/price/changes":                 gated,
	"GET /v1/history":                       gated,
	"GET /v1/history/since-inception":       gated,
	"GET /v1/chart":                         gated,
	"GET /v1/assets":                        gated,
	"GET /v1/assets/verified":               gated,
	"GET /v1/external/assets":               uncovered("external asset prices"),
	"GET /v1/external/assets/{slug}":        gated,
	"GET /v1/rwa/assets":                    uncovered("RWA market prices; the oracle and fund-NAV reference is judged"),
	"GET /v1/rwa/history":                   uncovered("RWA price history"),
	"GET /v1/rwa/premium":                   gated,
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
	"GET /v1/lending/pools/{pool}/reserves/{asset}/history": gated,
	"GET /v1/lending/pools/{pool}/config-history":           lendingUncovered,
	"GET /v1/lending/pools/{pool}/backstop":                 lendingUncovered,
	"GET /v1/lending/pools/{pool}/backstop/history":         lendingUncovered,
	"GET /v1/lending/pools/{pool}/history":                  gated,
	"GET /v1/lending/accounts/{account}/earnings":           lendingUncovered,
	"GET /v1/lending/pools/{pool}/auctions":                 lendingUncovered,
	"GET /v1/lending/pools/{pool}/liquidations":             gated, // TestLendingPoolLiquidations_ExcludedSourceDoesNotValueFill
	"GET /v1/lending/pools/{pool}/emissions":                lendingUncovered,
	"GET /v1/lending/pools/{pool}/top-accounts":             lendingUncovered,
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
	"GET /v1/accounts":                                      uncovered("listing usd_value and priced_assets"),
	"GET /v1/network/stats":                                 uncovered("volume_24h_usd"),
	"GET /v1/sources/{name}/health":                         uncovered("volume_24h_usd"),
	"GET /v1/diagnostics/ingestion":                         uncovered("ledger and per-source volume_24h_usd"),
	"GET /v1/changes/{entity_type}/{id}":                    uncovered("stored change-summary price values, ATH/ATL"),
	"GET /v1/pools/reserves":                                uncovered("venue mid_price from reserves, quote units"),
	"GET /v1/liquidity-pools":                               uncovered("venue mid_price from reserves, quote units"),
	"GET /v1/sdex/orderbook":                                uncovered("raw offer prices, quote units"),
	"GET /v1/accounts/{g_strkey}/positions":                 {state: covNone, reason: "positions carry no USD price"},
	"GET /v1/search":                                        {state: covNone, reason: "no USD price"},
	"GET /v1/coverage":                                      {state: covNone, reason: "coverage verdicts, not prices"},
}

// notPriceServing lists the operations that serve no price, valuation or USD figure;
// TestPriceRouteCoverage_NoneRoutesServeNoValueField checks that against each one's response schema.
var notPriceServing = []string{
	"GET /v1/healthz", "GET /v1/readyz", "GET /v1/livez/lake", "GET /v1/version", "GET /v1/status", "GET /v1/status/notices",
	"GET /v1/assets/{asset_id}/metadata", "GET /v1/assets/{asset_id}/supply", "GET /v1/assets/{asset_id}/supply/flows",
	"GET /v1/assets/{asset_id}/holders", "GET /v1/assets/{asset_id}/movements", "GET /v1/assets/{asset_id}/entry-changes",
	"GET /v1/assets/{asset_id}/trustline-history", "GET /v1/issuers", "GET /v1/issuers/{g_strkey}",
	"GET /v1/contracts/{contract_id}/transfers", "GET /v1/diagnostics/cursors",
	"GET /v1/diagnostics/archive", "GET /v1/diagnostics/backups", "GET /v1/incidents",
	"GET /v1/incidents.atom", "GET /v1/ledger/tip", "GET /v1/ledger/stream",
	"GET /v1/network/throughput", "GET /v1/methodology", "GET /v1/sac-wrappers",
	"GET /v1/account/me", "GET /v1/account/usage", "GET /v1/account/keys", "GET /v1/admin/accounts/{id}",
	"GET /v1/admin/status-notices", "GET /v1/signup/verify", "GET /v1/dashboard/account/export", "GET /v1/dashboard/keys",
	"GET /v1/dashboard/webhooks", "GET /v1/dashboard/webhooks/{id}/deliveries", "GET /v1/dashboard/price-alerts", "GET /v1/auth/callback",
	"GET /v1/auth/passkey/credentials", "GET /v1/auth/sep10/challenge", "GET /v1/ledgers", "GET /v1/ledgers/at",
	"GET /v1/ledgers/{seq}", "GET /v1/ledgers/{seq}/transactions", "GET /v1/ledgers/{seq}/operations", "GET /v1/tx/{hash}",
	"GET /v1/operations", "GET /v1/contracts", "GET /v1/contracts/stats", "GET /v1/contracts/{contract_id}",
	"GET /v1/contracts/{contract_id}/wasm", "GET /v1/contracts/{contract_id}/interactions",
	"GET /v1/contracts/{contract_id}/code-history", "GET /v1/directory", "GET /v1/accounts/stats",
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
	ungated := 0
	for _, c := range priceRouteCoverage {
		if c.state == covPartial || c.state == covUncovered {
			ungated++
		}
	}
	if ungated != ungatedPriceRoutes {
		t.Errorf("%d price routes are partial or uncovered, want %d: a new price route judges source exclusions (gated, with a "+
			"proof in gatedRouteExclusionProof); after gating one, lower ungatedPriceRoutes", ungated, ungatedPriceRoutes)
	}
}

// ungatedPriceRoutes ratchets the kill switch's known gaps: an operator exclusion does not reach these routes' figures.
const ungatedPriceRoutes = 45

// operatorNoteName is how incident.ReadSideNote's NOT COVERED list names an ungated route whose path it does not spell out.
var operatorNoteName = map[string]string{
	"GET /v1/rwa/assets":                          "RWA assets/history",
	"GET /v1/rwa/history":                         "RWA assets/history",
	"GET /v1/observations/stream":                 "/v1/observations + stream",
	"GET /v1/markets/sources":                     "/v1/markets + sources",
	"GET /v1/divergence/series":                   "/v1/divergence",
	"GET /v1/accounts/{g_strkey}":                 "account usd_value",
	"GET /v1/accounts/{g_strkey}/trades":          "trades usd_volume",
	"GET /v1/accounts/{g_strkey}/graph":           "graph USD",
	"GET /v1/accounts/{g_strkey}/graph/history":   "graph USD",
	"GET /v1/accounts/{g_strkey}/graph/cohort":    "graph USD",
	"GET /v1/accounts":                            "/v1/accounts usd_value",
	"GET /v1/protocols/{name}":                    "/v1/protocols + tvl",
	"GET /v1/protocols/{name}/tvl":                "/v1/protocols + tvl",
	"GET /v1/changes/{entity_type}/{id}":          "/v1/changes/{type}/{id}",
	"GET /v1/lending/pools":                       "/v1/lending/*",
	"GET /v1/lending/rates":                       "/v1/lending/*",
	"GET /v1/lending/protocols":                   "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/reserves":       "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/config-history": "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/backstop":       "/v1/lending/*",

	"GET /v1/lending/pools/{pool}/backstop/history": "/v1/lending/*",
	"GET /v1/lending/accounts/{account}/earnings":   "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/auctions":         "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/emissions":        "/v1/lending/*",
	"GET /v1/lending/pools/{pool}/top-accounts":     "/v1/lending/*",
}

// TestPriceRouteCoverage_OperatorNoteMatchesCensus: the operator who excludes a bad source is told which routes it does not
// reach. Every partial or uncovered route must be named in that NOT COVERED list, and no gated route may be.
func TestPriceRouteCoverage_OperatorNoteMatchesCensus(t *testing.T) {
	_, notCovered, ok := strings.Cut(incident.ReadSideNote, "NOT COVERED")
	if !ok {
		t.Fatal("incident.ReadSideNote has no NOT COVERED list")
	}
	notCovered, _, _ = strings.Cut(notCovered, "Check:")
	named := func(phrase string) bool {
		for rest := notCovered; ; {
			i := strings.Index(rest, phrase)
			if i < 0 {
				return false
			}
			rest = rest[i+len(phrase):]
			if rest == "" || strings.ContainsRune(" ,.\n", rune(rest[0])) {
				return true
			}
		}
	}
	for op, c := range priceRouteCoverage {
		_, path, _ := strings.Cut(op, " ")
		switch c.state {
		case covPartial, covUncovered:
			phrase := cmp.Or(operatorNoteName[op], path)
			if !named(phrase) {
				t.Errorf("%s is %s, but incident.ReadSideNote's NOT COVERED list does not name it (%q)", op, c.state, phrase)
			}
		case covGated:
			if named(path) {
				t.Errorf("%s is gated, but incident.ReadSideNote lists it as NOT COVERED", op)
			}
		}
	}
	for op := range operatorNoteName {
		if c := priceRouteCoverage[op]; c.state != covPartial && c.state != covUncovered {
			t.Errorf("operatorNoteName names %s, which the census does not file as partial or uncovered", op)
		}
	}
}

// GatedPriceRoutes returns the census's gated rows for the external-package proof check.
func GatedPriceRoutes() []string {
	var ops []string
	for op, c := range priceRouteCoverage {
		if c.state == covGated {
			ops = append(ops, op)
		}
	}
	sort.Strings(ops)
	return ops
}

// valueFieldNames matches a response field that can carry a price, valuation or USD figure.
var valueFieldNames = regexp.MustCompile(`usd|price|value|tvl|market_cap|fdv`)

// noneValueFieldWhy justifies, per field, a value-looking response field on a "none" route; a field the scan finds that
// is not here fails TestPriceRouteCoverage_NoneRoutesServeNoValueField.
var noneValueFieldWhy = map[string]map[string]string{
	"GET /v1/coverage": {
		"data.usd_volume_pricing":                      "counts of sources with and without a USD volume price, no figure",
		"data.usd_volume_pricing.sources.priced":       "a source count",
		"data.usd_volume_pricing.sources.priced_ratio": "a ratio of source counts",
		"data.usd_volume_pricing.sources.unpriced":     "a source count",
	},
	"GET /v1/methodology":              {"data.aggregation.price_method": "the method's name, a string"},
	"GET /v1/dashboard/account/export": {"price_alerts": "the caller's own alert rules; threshold is the number they set"},
	"POST /v1/register":                {"data.limits.max_price_alerts": "a quota count, no figure"},
}

// specValueFields returns, per GET operation, the value-looking property names (dotted paths) in its 2xx response schemas.
func specValueFields(t *testing.T) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil || len(root.Content) == 0 {
		t.Fatalf("spec: %v", err)
	}
	components := mapGet(root.Content[0], "components")
	deref := func(n *yaml.Node) *yaml.Node {
		for n != nil && n.Kind == yaml.MappingNode {
			ref := mapGet(n, "$ref")
			if ref == nil {
				return n
			}
			kind, name, ok := strings.Cut(strings.TrimPrefix(ref.Value, "#/components/"), "/")
			target := mapGet(mapGet(components, kind), name)
			if !ok || target == nil {
				t.Fatalf("unresolvable $ref %q", ref.Value)
			}
			n = target
		}
		return n
	}
	var walk func(n *yaml.Node, prefix string, seen map[*yaml.Node]bool, out map[string]bool)
	walk = func(n *yaml.Node, prefix string, seen map[*yaml.Node]bool, out map[string]bool) {
		n = deref(n)
		if n == nil || n.Kind != yaml.MappingNode || seen[n] {
			return
		}
		seen[n] = true
		defer delete(seen, n)
		if props := mapGet(n, "properties"); props != nil {
			for i := 0; i+1 < len(props.Content); i += 2 {
				name := props.Content[i].Value
				if valueFieldNames.MatchString(strings.ToLower(name)) {
					out[prefix+name] = true
				}
				walk(props.Content[i+1], prefix+name+".", seen, out)
			}
		}
		for _, k := range []string{"items", "additionalProperties"} {
			walk(mapGet(n, k), prefix, seen, out)
		}
		for _, k := range []string{"allOf", "oneOf", "anyOf"} {
			if list := mapGet(n, k); list != nil {
				for _, c := range list.Content {
					walk(c, prefix, seen, out)
				}
			}
		}
	}
	fields := map[string][]string{}
	paths := mapGet(root.Content[0], "paths")
	for p := 0; paths != nil && p+1 < len(paths.Content); p += 2 {
		path := paths.Content[p].Value
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			operation := mapGet(paths.Content[p+1], strings.ToLower(method))
			if operation == nil {
				continue
			}
			out := map[string]bool{}
			responses := mapGet(operation, "responses")
			for i := 0; responses != nil && i+1 < len(responses.Content); i += 2 {
				if !strings.HasPrefix(responses.Content[i].Value, "2") {
					continue
				}
				content := mapGet(deref(responses.Content[i+1]), "content")
				for j := 0; content != nil && j+1 < len(content.Content); j += 2 {
					walk(mapGet(content.Content[j+1], "schema"), "", map[*yaml.Node]bool{}, out)
				}
			}
			op := method + " /v1" + path
			for f := range out {
				fields[op] = append(fields[op], f)
			}
			sort.Strings(fields[op])
		}
	}
	return fields
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// TestPriceRouteCoverage_NoneRoutesServeNoValueField reads every "none" route's response schema, so a route that serves a
// USD figure cannot be filed as serving none without a per-field reason. An unclassified mutation is presumed to serve none
// and is scanned too, so a write that returns a price cannot skip the census.
func TestPriceRouteCoverage_NoneRoutesServeNoValueField(t *testing.T) {
	fields := specValueFields(t)
	none := append([]string{}, notPriceServing...)
	for op, c := range priceRouteCoverage {
		if c.state == covNone {
			none = append(none, op)
		}
	}
	for _, op := range specOperations(t) {
		if _, listed := priceRouteCoverage[op]; !listed && !strings.HasPrefix(op, http.MethodGet+" ") {
			none = append(none, op)
		}
	}
	sort.Strings(none)
	for _, op := range none {
		for _, f := range fields[op] {
			if noneValueFieldWhy[op][f] == "" {
				t.Errorf("%s is filed as serving no price but its response has %q: classify it uncovered, or justify the field in noneValueFieldWhy", op, f)
			}
		}
	}
	for op, byField := range noneValueFieldWhy {
		for f := range byField {
			if !slices.Contains(fields[op], f) {
				t.Errorf("noneValueFieldWhy justifies %s %q, which its response schema no longer has", op, f)
			}
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
