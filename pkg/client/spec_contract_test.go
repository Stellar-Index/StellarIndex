// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the SDK↔spec reconciliation gate. The OpenAPI spec,
// the Go handlers, and this SDK are three hand-maintained
// representations of one contract; lint-docs.sh already reconciles
// handlers↔spec (CS-052), and this test closes the remaining edge:
//
//  1. Every SDK method's (HTTP method, path) exists in the spec.
//  2. Every spec operation is either covered by an SDK method or
//     EXPLICITLY listed in uncoveredOperations with a reason — a new
//     endpoint fails this test until its author makes the conscious
//     choice. The allowlist is shrink-preferred: entries for
//     operations that gain SDK coverage (or leave the spec) fail as
//     stale.
//  3. For covered operations, the spec's `data` schema properties
//     must exactly match the SDK payload struct's JSON tags (both
//     directions) — a field rename/addition/removal in the spec that
//     the SDK doesn't mirror is a silent production drift; this
//     turns it into a test failure.
//  4. The generic envelope and its Flags object — the members every
//     operation shares — must match Envelope[T] and Flags the same
//     way, both directions. The per-operation walk in (3) stops at
//     `data`, so a new envelope member or flag could ship in the spec
//     with no SDK field to land in; the walk in (4) covers it.

// coveredOperation maps one SDK method to the spec operation it
// calls. payload is the struct the envelope's `data` decodes into —
// the ELEMENT type when data is an array. nil payload skips the
// schema check (non-struct payloads, 204 responses).
type coveredOperation struct {
	sdkMethod string
	method    string
	path      string // spec path template, without the /v1 prefix
	payload   any
	// envelopeRef disambiguates operations whose 200 response is a
	// oneOf of several envelopes (e.g. /ohlc single-bar vs series,
	// the dual-shape /assets/{asset_id}): the named component is
	// the branch this SDK method's payload maps to.
	envelopeRef string
}

var coveredOperations = []coveredOperation{
	{"Price", "GET", "/price", PriceSnapshot{}, ""},
	{"PriceTip", "GET", "/price/tip", PriceSnapshot{}, ""},
	{"PriceAt", "GET", "/price/at", PriceSnapshot{}, ""},
	{"PriceChanges", "GET", "/price/changes", PriceChanges{}, ""},
	{"PriceBatch", "GET", "/price/batch", PriceSnapshot{}, ""},
	{"PriceBatch", "POST", "/price/batch", PriceSnapshot{}, ""},
	{"History", "GET", "/history", TradeRow{}, ""},
	{"HistorySinceInception", "GET", "/history/since-inception", HistorySeries{}, ""},
	{"Observations", "GET", "/observations", TradeRow{}, ""},
	{"Chart", "GET", "/chart", ChartSeries{}, ""},
	{sdkMethod: "OHLC", method: "GET", path: "/ohlc", payload: OHLCBar{}, envelopeRef: "#/components/schemas/OHLCEnvelope"},
	{"VWAP", "GET", "/vwap", VWAPResult{}, ""},
	{"TWAP", "GET", "/twap", TWAPResult{}, ""},
	{"Assets", "GET", "/assets", AssetDetail{}, ""},
	// payload: nil — Asset() returns AssetLookup (ADR-0042 LC-040), a
	// hand-written dual-shape union with custom UnmarshalJSON/
	// MarshalJSON, not a struct with static JSON tags the generic
	// reflection check (jsonTags) can walk. The seen[key] de-dupe in
	// TestSDKSchemasMatchSpec is also keyed by method+path alone, so a
	// second entry here for the GlobalAssetEnvelope branch would
	// silently no-op rather than add coverage. Both branches are
	// exercised directly instead by TestAssetLookup_UnmarshalJSON_BothBranches
	// (asset_lookup_test.go), which decodes real kind:"stellar_asset"
	// and kind:"catalogue" fixtures and asserts every field the two
	// spec schemas (Asset, GlobalAssetView) document round-trips.
	{sdkMethod: "Asset", method: "GET", path: "/assets/{asset_id}", payload: nil, envelopeRef: "#/components/schemas/AssetEnvelope"},
	{"AssetMetadata", "GET", "/assets/{asset_id}/metadata", AssetMetadata{}, ""},
	{"Sources", "GET", "/sources", Source{}, ""},
	{"Aggregators", "GET", "/aggregators", AggregatorRow{}, ""},
	{"Methodology", "GET", "/methodology", Methodology{}, ""},
	{"Markets", "GET", "/markets", Market{}, ""},
	{"Pair", "GET", "/pairs", Market{}, ""},
	{"Pools", "GET", "/pools", Pool{}, ""},
	{"LendingPools", "GET", "/lending/pools", LendingPool{}, ""},
	{"SACWrappers", "GET", "/sac-wrappers", nil, ""}, // map[string]string payload
	{"Issuers", "GET", "/issuers", IssuerListEntry{}, ""},
	{"Issuer", "GET", "/issuers/{g_strkey}", Issuer{}, ""},
	{"NetworkStats", "GET", "/network/stats", NetworkStats{}, ""},
	{"ChangeSummary", "GET", "/changes/{entity_type}/{id}", ChangeSummary{}, ""},
	{"Cursors", "GET", "/diagnostics/cursors", Cursor{}, ""},
	{"Incidents", "GET", "/incidents", IncidentsList{}, ""},
	{"Me", "GET", "/account/me", Account{}, ""},
	{"Usage", "GET", "/account/usage", UsageRow{}, ""},
	{"Keys", "GET", "/account/keys", Account{}, ""},
	{"CreateKey", "POST", "/account/keys", KeyCreated{}, ""},
	{"AdminCreateKey", "POST", "/admin/keys", KeyCreated{}, ""},
	{"RevokeKey", "DELETE", "/account/keys/{keyID}", nil, ""}, // 204, no body
	{"Status", "GET", "/status", Status{}, ""},
	{"Healthz", "GET", "/healthz", Health{}, ""},
	{"Readyz", "GET", "/readyz", Health{}, ""},
	{"Version", "GET", "/version", Version{}, ""},
}

// uncoveredOperations is the conscious-decision register: spec
// operations the SDK deliberately does not cover, each with the
// reason. Adding an endpoint to the spec without either an SDK
// method or an entry here fails TestSDKCoversSpec.
var uncoveredOperations = map[string]string{
	// SSE streams — the SDK has no streaming client yet. When one
	// lands, these five move to coveredOperations together.
	"GET /price/stream":        "SSE — no streaming client in the SDK yet",
	"GET /price/tip/stream":    "SSE — no streaming client in the SDK yet",
	"GET /observations/stream": "SSE — no streaming client in the SDK yet",
	"GET /ledger/stream":       "SSE — no streaming client in the SDK yet",
	"GET /oracle/streams":      "SEP-40 stream directory — pairs with the SSE gap",

	// Explorer read surface (ADR-0038) — served to the web explorer;
	// SDK is pricing-first. Deliberate until a customer asks.
	"GET /ledgers":                              "explorer surface — SDK is pricing-first",
	"GET /ledgers/{seq}":                        "explorer surface — SDK is pricing-first",
	"GET /ledgers/{seq}/transactions":           "explorer surface — SDK is pricing-first",
	"GET /tx/{hash}":                            "explorer surface — SDK is pricing-first",
	"GET /operations":                           "explorer surface — SDK is pricing-first",
	"GET /contracts":                            "explorer surface — SDK is pricing-first",
	"GET /contracts/{contract_id}":              "explorer surface — SDK is pricing-first",
	"GET /contracts/{contract_id}/wasm":         "explorer surface — SDK is pricing-first",
	"GET /contracts/{contract_id}/interactions": "explorer surface — SDK is pricing-first",
	"GET /contracts/{contract_id}/code-history": "explorer surface — SDK is pricing-first",
	"GET /contracts/{contract_id}/transfers":    "explorer surface — SDK is pricing-first",
	"GET /accounts":                             "explorer surface — SDK is pricing-first",
	"GET /directory":                            "explorer surface — SDK is pricing-first",
	"GET /accounts/stats":                       "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}":                  "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/transactions":     "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/operations":       "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/movements":        "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/positions":        "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/trades":           "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/activity":         "explorer surface — SDK is pricing-first",
	"GET /search":                               "explorer surface — SDK is pricing-first",
	"GET /network/throughput":                   "explorer chart feed — SDK is pricing-first",

	// SEP-40 oracle passthrough — contract-shaped responses, served
	// for parity with the on-chain interface; Go consumers use the
	// native pricing endpoints instead.
	"GET /oracle/latest":       "SEP-40 passthrough — native endpoints preferred in Go",
	"GET /oracle/lastprice":    "SEP-40 passthrough — native endpoints preferred in Go",
	"GET /oracle/prices":       "SEP-40 passthrough — native endpoints preferred in Go",
	"GET /oracle/x_last_price": "SEP-40 passthrough — native endpoints preferred in Go",

	// Analytics/diagnostics surfaces — explorer-facing, not yet SDK.
	"GET /assets/verified":               "explorer verified-badge feed",
	"GET /external/assets":               "reference (non-Stellar) asset list — explorer surface",
	"GET /external/assets/{slug}":        "reference (non-Stellar) asset detail — explorer surface",
	"GET /assets/{asset_id}/supply":      "supply drill-down — explorer surface",
	"GET /assets/{asset_id}/holders":     "holders drill-down — explorer surface",
	"GET /markets/sources":               "markets-by-source directory — explorer surface",
	"GET /protocols":                     "protocol analytics — explorer surface",
	"GET /protocols/{name}":              "protocol analytics — explorer surface",
	"GET /protocols/{name}/tvl":          "per-pool DEX TVL drill-down — explorer surface",
	"GET /sdex/orderbook":                "order-book depth — explorer surface; SDK is pricing-first",
	"GET /lending/pools/{pool}/reserves": "lending drill-down — explorer surface",
	"GET /pools/reserves":                "AMM reserve/depth drill-down — explorer surface",
	"GET /liquidity-pools":               "native (CAP-38) pool reserve/depth drill-down — explorer surface",
	"GET /mev":                           "MEV feed — explorer surface",
	"GET /anomalies":                     "anomaly feed — explorer surface",
	"GET /divergence":                    "divergence feed — explorer surface",
	"GET /divergence/series":             "divergence history chart feed — explorer surface",
	"GET /coverage":                      "coverage verdict — explorer/status surface",
	"GET /diagnostics/ingestion":         "operator diagnostics",
	"GET /diagnostics/archive":           "operator diagnostics — archive-completeness report, explorer surface",
	"GET /diagnostics/backups":           "operator diagnostics — backup freshness vs SLO, public status-page surface",
	"GET /sources/{name}/health":         "per-source health pane — explorer surface",
	"GET /ledger/tip":                    "explorer tip feed",
	"GET /incidents.atom":                "Atom feed — not JSON",

	// Auth/session/webhook flows — browser/dashboard interactions,
	// not machine-SDK surface.
	"POST /register":                          "one-shot curl-first onboarding — SDK consumers already hold a key; coverage lands with the account-management client if one ships",
	"POST /signup":                            "browser onboarding flow",
	"POST /auth/passkey/begin-login":          "browser WebAuthn ceremony — session-cookie dashboard surface",
	"POST /auth/passkey/finish-login":         "browser WebAuthn ceremony — session-cookie dashboard surface",
	"POST /auth/passkey/begin-register":       "browser WebAuthn ceremony — session-cookie dashboard surface",
	"POST /auth/passkey/finish-register":      "browser WebAuthn ceremony — session-cookie dashboard surface",
	"GET /auth/passkey/credentials":           "session-cookie dashboard surface",
	"DELETE /auth/passkey/credentials/{id}":   "session-cookie dashboard surface",
	"GET /signup/verify":                      "browser onboarding flow",
	"GET /dashboard/keys":                     "session-cookie dashboard surface",
	"POST /dashboard/keys":                    "session-cookie dashboard surface",
	"DELETE /dashboard/keys/{id}":             "session-cookie dashboard surface",
	"GET /dashboard/webhooks":                 "session-cookie dashboard surface",
	"POST /dashboard/webhooks":                "session-cookie dashboard surface",
	"PATCH /dashboard/webhooks/{id}":          "session-cookie dashboard surface",
	"DELETE /dashboard/webhooks/{id}":         "session-cookie dashboard surface",
	"GET /dashboard/webhooks/{id}/deliveries": "session-cookie dashboard surface",
	"GET /dashboard/price-alerts":             "session-cookie dashboard surface",
	"POST /dashboard/price-alerts":            "session-cookie dashboard surface",
	"PATCH /dashboard/price-alerts/{id}":      "session-cookie dashboard surface",
	"DELETE /dashboard/price-alerts/{id}":     "session-cookie dashboard surface",
	// POST rather than GET, and read-only despite the verb: the look-up
	// term is a customer email address, which must not travel in a URL
	// (#346). See dashboardauth.adminLookupRequest.
	"POST /account/admin/lookup": "session-cookie dashboard surface — staff-only customer look-up",
	"GET /livez/lake":            "load-balancer lake-health probe (ADR-0050 §7.3) — infrastructure surface, not an SDK data call",
	"POST /auth/login":           "magic-link browser flow",
	"GET /auth/callback":         "magic-link browser flow",
	"POST /auth/verify-code":     "magic-link browser flow",
	"POST /auth/logout":          "magic-link browser flow",
	"GET /auth/sep10/challenge":  "SEP-10 wallet flow — wallet SDKs handle this",
	"POST /auth/sep10/token":     "SEP-10 wallet flow — wallet SDKs handle this",

	// Operator/admin surfaces (admin Phase 1.5) — staff-issued
	// operator-tier credentials only; not a machine-SDK surface.
	"GET /admin/accounts/{id}":                "operator surface — staff-issued credential only",
	"PATCH /admin/accounts/{id}":              "operator surface — staff-issued credential only",
	"DELETE /admin/keys/{keyID}":              "operator kill switch — staff-issued credential only",
	"GET /admin/status-notices":               "operator surface — staff-issued credential only",
	"POST /admin/status-notices":              "operator surface — staff-issued credential only",
	"POST /admin/status-notices/{id}/resolve": "operator surface — staff-issued credential only",
	"GET /status/notices":                     "status-page banner feed — explorer/status surface",
}

// schemaExceptions lists per-operation JSON field names excluded
// from the bidirectional property check, keyed "METHOD /path" →
// field → reason. Keep this SHORT — every entry is live drift debt.
var schemaExceptions = map[string]map[string]string{
	// The SDK's Health type is shared by Healthz and Readyz; the
	// handler serves the same struct on both routes but `checks`
	// only populates on /readyz (omitempty hides it on /healthz),
	// and the /healthz spec schema documents the served-on-this-
	// route fields only.
	"GET /healthz": {"checks": "shared Health type; checks populates only on /readyz"},
}

const specPath = "../../openapi/stellar-index.v1.yaml"

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	return doc
}

// specOperations returns the set of "METHOD /path" strings in the spec.
func specOperations(t *testing.T, doc map[string]any) map[string]bool {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		t.Fatal("spec has no paths")
	}
	ops := map[string]bool{}
	for p, v := range paths {
		item, _ := v.(map[string]any)
		for m := range item {
			switch m {
			case "get", "post", "put", "delete", "patch":
				ops[strings.ToUpper(m)+" "+p] = true
			}
		}
	}
	return ops
}

// TestSDKCoversSpec — every spec operation is either SDK-covered or
// consciously allowlisted; no stale entries in either table.
func TestSDKCoversSpec(t *testing.T) {
	doc := loadSpec(t)
	ops := specOperations(t, doc)

	covered := map[string]bool{}
	for _, c := range coveredOperations {
		key := c.method + " " + c.path
		if !ops[key] {
			t.Errorf("SDK method %s targets %q which is NOT in the OpenAPI spec — path renamed or removed?", c.sdkMethod, key)
		}
		covered[key] = true
	}
	for key := range uncoveredOperations {
		if !ops[key] {
			t.Errorf("uncoveredOperations entry %q is stale — the operation is no longer in the spec", key)
		}
		if covered[key] {
			t.Errorf("uncoveredOperations entry %q is stale — the SDK covers it now; remove the allowlist entry", key)
		}
	}
	var missing []string
	for key := range ops {
		if !covered[key] && uncoveredOperations[key] == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		t.Errorf("spec operation %q has no SDK method and no uncoveredOperations entry — add one or the other (a conscious decision, not a default)", key)
	}
}

// ─── Schema reconciliation ──────────────────────────────────────────

// resolveRef follows "#/components/schemas/X".
func resolveRef(doc map[string]any, ref string) map[string]any {
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	cur := any(doc)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	out, _ := cur.(map[string]any)
	return out
}

// mergeSchema flattens $ref + allOf into a single schema map with a
// combined "properties" set.
func mergeSchema(doc map[string]any, schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		return mergeSchema(doc, resolveRef(doc, ref))
	}
	out := map[string]any{}
	props := map[string]any{}
	for k, v := range schema {
		out[k] = v
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, part := range all {
			pm, _ := part.(map[string]any)
			merged := mergeSchema(doc, pm)
			if merged == nil {
				continue
			}
			for k, v := range merged {
				if k == "properties" {
					continue
				}
				out[k] = v
			}
			if pp, ok := merged["properties"].(map[string]any); ok {
				for k, v := range pp {
					props[k] = v
				}
			}
		}
	}
	if pp, ok := schema["properties"].(map[string]any); ok {
		for k, v := range pp {
			props[k] = v
		}
	}
	if len(props) > 0 {
		out["properties"] = props
	}
	return out
}

// dataSchemaProps resolves an operation's 200/201-response envelope
// and returns the property names of its `data` payload — the element
// schema when data is an array. envelopeRef, when non-empty,
// overrides resolution for oneOf responses by naming the branch.
// Second return distinguishes "no object properties to check"
// (false) from a real property set.
func dataSchemaProps(t *testing.T, doc map[string]any, method, path, envelopeRef string) (map[string]bool, bool) {
	t.Helper()
	dig := func(m map[string]any, keys ...string) map[string]any {
		cur := m
		for _, k := range keys {
			next, _ := cur[k].(map[string]any)
			if next == nil {
				return nil
			}
			cur = next
		}
		return cur
	}
	var schema map[string]any
	if envelopeRef != "" {
		schema = map[string]any{"$ref": envelopeRef}
	} else {
		op := dig(doc, "paths", path, strings.ToLower(method))
		if op == nil {
			t.Errorf("%s %s: operation missing while resolving schema", method, path)
			return nil, false
		}
		for _, status := range []string{"200", "201"} {
			schema = dig(op, "responses", status, "content", "application/json", "schema")
			if schema != nil {
				break
			}
		}
	}
	if schema == nil {
		return nil, false // 204 / non-JSON
	}
	env := mergeSchema(doc, schema)
	props, _ := env["properties"].(map[string]any)
	dataRaw, _ := props["data"].(map[string]any)
	var data map[string]any
	if dataRaw == nil {
		// Not enveloped — treat the whole schema as the payload.
		data = env
	} else {
		data = mergeSchema(doc, dataRaw)
	}
	if data == nil {
		return nil, false
	}
	if data["type"] == "array" {
		items, _ := data["items"].(map[string]any)
		data = mergeSchema(doc, items)
		if data == nil {
			return nil, false
		}
	}
	dp, _ := data["properties"].(map[string]any)
	if dp == nil {
		return nil, false // primitive / additionalProperties payload
	}
	out := map[string]bool{}
	for k := range dp {
		out[k] = true
	}
	return out, true
}

// jsonTags returns the JSON field names of a struct type (embedded
// structs flattened, `json:"-"` skipped).
func jsonTags(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(tt reflect.Type) {
		for i := 0; i < tt.NumField(); i++ {
			f := tt.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			out[name] = true
		}
	}
	walk(typ)
	return out
}

// TestSDKSchemasMatchSpec — for each covered operation with a struct
// payload, the spec's data properties and the Go struct's JSON tags
// must match exactly (modulo the explicit exceptions map).
func TestSDKSchemasMatchSpec(t *testing.T) {
	doc := loadSpec(t)
	seen := map[string]bool{}
	for _, c := range coveredOperations {
		if c.payload == nil {
			continue
		}
		key := c.method + " " + c.path
		if seen[key] {
			continue
		}
		seen[key] = true

		specProps, ok := dataSchemaProps(t, doc, c.method, c.path, c.envelopeRef)
		if !ok {
			t.Errorf("%s: could not resolve an object schema for the 200 response — if the payload is deliberately unstructured, set payload nil in coveredOperations", key)
			continue
		}
		goProps := jsonTags(reflect.TypeOf(c.payload))
		exc := schemaExceptions[key]

		var missingInGo, missingInSpec []string
		for p := range specProps {
			if !goProps[p] && exc[p] == "" {
				missingInGo = append(missingInGo, p)
			}
		}
		for p := range goProps {
			if !specProps[p] && exc[p] == "" {
				missingInSpec = append(missingInSpec, p)
			}
		}
		sort.Strings(missingInGo)
		sort.Strings(missingInSpec)
		if len(missingInGo) > 0 {
			t.Errorf("%s (%s): spec documents data fields the SDK type %T does not carry — the SDK silently drops them: %v",
				key, c.sdkMethod, c.payload, missingInGo)
		}
		if len(missingInSpec) > 0 {
			t.Errorf("%s (%s): SDK type %T declares fields the spec does not document: %v",
				key, c.sdkMethod, c.payload, missingInSpec)
		}
		_ = fmt.Sprintf("%v", exc)
	}
}

// TestCoveredOperationsBindToRealMethods closes the wave-D F-SDK-10
// gap: nothing tied coveredOperations to the code it claims to
// describe.
//
// TestSDKCoversSpec reconciles the TABLE against the OpenAPI spec, so
// it catches an endpoint the SDK forgot. It cannot catch the table
// drifting from the SDK: rename a Client method, or delete one, and
// the table keeps naming the old identifier while the reconciliation
// stays green — declared-but-not-enforced, the same shape as the rest
// of that finding's unit.
//
// This binds it in both directions:
//
//   - every `sdkMethod` in the table must resolve on *Client, so a
//     rename or deletion fails here rather than rotting silently;
//   - every exported *Client method must appear in the table or in the
//     explicit exemption set below, so a NEW method cannot be added
//     without deciding whether it is a spec operation.
//
// The second direction is the load-bearing one. Without it the table
// only ever describes the subset someone remembered to add to it.
func TestCoveredOperationsBindToRealMethods(t *testing.T) {
	t.Parallel()

	clientType := reflect.TypeOf(&Client{})

	// notSpecOperations are exported *Client methods that are
	// deliberately NOT spec operations. Add an entry only with the
	// reason it does not correspond to an HTTP operation.
	notSpecOperations := map[string]string{
		"Close": "releases the HTTP client's idle connections; no request",
	}

	// Direction 1: every tabled method must exist.
	for _, op := range coveredOperations {
		if op.sdkMethod == "" {
			t.Errorf("coveredOperations has an entry with an empty sdkMethod (%s %s)",
				op.method, op.path)
			continue
		}
		if _, ok := clientType.MethodByName(op.sdkMethod); !ok {
			t.Errorf("coveredOperations names Client.%s (%s %s), which does not exist — "+
				"the table has drifted from the code. TestSDKCoversSpec cannot catch "+
				"this: it reconciles the table against the SPEC, not against the SDK.",
				op.sdkMethod, op.method, op.path)
		}
	}

	// Direction 2: every exported method must be accounted for.
	tabled := make(map[string]bool, len(coveredOperations))
	for _, op := range coveredOperations {
		tabled[op.sdkMethod] = true
	}
	var unaccounted []string
	for i := 0; i < clientType.NumMethod(); i++ {
		name := clientType.Method(i).Name
		if tabled[name] {
			continue
		}
		if _, ok := notSpecOperations[name]; ok {
			continue
		}
		unaccounted = append(unaccounted, name)
	}
	sort.Strings(unaccounted)
	for _, name := range unaccounted {
		t.Errorf("Client.%s is exported but appears in neither coveredOperations nor "+
			"notSpecOperations. Add it to the table if it calls a spec operation, or "+
			"to notSpecOperations with the reason it does not — an unlisted method is "+
			"a spec operation nobody decided about.", name)
	}

	// A guard is worth only what it checks.
	if len(coveredOperations) == 0 {
		t.Fatal("coveredOperations is empty — this guard has gone vacuous")
	}
	if clientType.NumMethod() == 0 {
		t.Fatal("*Client has no exported methods — reflection found nothing, so " +
			"direction 2 checked nothing")
	}
}

// ─── Envelope + Flags reconciliation ────────────────────────────────

// envelopeMetaRef names the component every 2xx envelope in the spec
// composes through allOf. The per-operation envelopes add `data` and,
// on list surfaces, `pagination` as allOf siblings, so the generic
// wire shape is EnvelopeMeta's own properties plus the union of every
// composer's siblings — and that union is what Envelope[T] has to
// carry, because the SDK decodes every operation through it.
const envelopeMetaRef = "#/components/schemas/EnvelopeMeta"

// composesEnvelopeMeta reports whether schema is an allOf whose parts
// include a direct $ref to EnvelopeMeta.
func composesEnvelopeMeta(schema map[string]any) bool {
	all, _ := schema["allOf"].([]any)
	for _, part := range all {
		pm, _ := part.(map[string]any)
		if ref, _ := pm["$ref"].(string); ref == envelopeMetaRef {
			return true
		}
	}
	return false
}

// specEnvelopeProps walks every schema that composes EnvelopeMeta —
// the named components and the inline response schemas under
// `paths`, including oneOf branches — and returns the union of their
// top-level property names, each mapped to the sorted places that
// declare it. The walk is what gives the check its reach: a member
// added on one operation's inline response is as much a change to
// the generic envelope as one added to EnvelopeMeta itself.
func specEnvelopeProps(t *testing.T, doc map[string]any) map[string][]string {
	t.Helper()
	meta := resolveRef(doc, envelopeMetaRef)
	if meta == nil {
		t.Fatalf("spec has no %s component — renamed? update envelopeMetaRef", envelopeMetaRef)
	}
	origins := map[string]map[string]bool{}
	note := func(prop, where string) {
		if origins[prop] == nil {
			origins[prop] = map[string]bool{}
		}
		origins[prop][where] = true
	}
	metaProps, _ := meta["properties"].(map[string]any)
	if len(metaProps) == 0 {
		t.Fatalf("%s declares no properties — the check would be vacuous", envelopeMetaRef)
	}
	for p := range metaProps {
		note(p, "EnvelopeMeta")
	}

	composers := 0
	visit := func(where string, schema map[string]any) {
		if schema == nil || !composesEnvelopeMeta(schema) {
			return
		}
		composers++
		merged := mergeSchema(doc, schema)
		props, _ := merged["properties"].(map[string]any)
		for p := range props {
			if _, fromMeta := metaProps[p]; !fromMeta {
				note(p, where)
			}
		}
	}

	schemas := resolveRef(doc, "#/components/schemas")
	for name, raw := range schemas {
		s, _ := raw.(map[string]any)
		visit(name, s)
	}
	paths, _ := doc["paths"].(map[string]any)
	for path, v := range paths {
		item, _ := v.(map[string]any)
		for method, opRaw := range item {
			op, _ := opRaw.(map[string]any)
			responses, _ := op["responses"].(map[string]any)
			for status, rRaw := range responses {
				r, _ := rRaw.(map[string]any)
				content, _ := r["content"].(map[string]any)
				mt, _ := content["application/json"].(map[string]any)
				schema, _ := mt["schema"].(map[string]any)
				if schema == nil {
					continue
				}
				where := strings.ToUpper(method) + " " + path + " " + status
				visit(where, schema)
				alts, _ := schema["oneOf"].([]any)
				for i, a := range alts {
					am, _ := a.(map[string]any)
					visit(fmt.Sprintf("%s oneOf[%d]", where, i), am)
				}
			}
		}
	}
	if composers == 0 {
		t.Fatalf("no schema in the spec composes %s — the walk found nothing to check", envelopeMetaRef)
	}

	out := make(map[string][]string, len(origins))
	for p, where := range origins {
		list := make([]string, 0, len(where))
		for w := range where {
			list = append(list, w)
		}
		sort.Strings(list)
		out[p] = list
	}
	return out
}

// specFlagsProps resolves the Flags object the way a consumer meets
// it — through EnvelopeMeta.properties.flags — whether that is a $ref
// to a named component or an inline object, and returns its property
// names.
func specFlagsProps(t *testing.T, doc map[string]any) map[string]bool {
	t.Helper()
	meta := resolveRef(doc, envelopeMetaRef)
	if meta == nil {
		t.Fatalf("spec has no %s component — renamed? update envelopeMetaRef", envelopeMetaRef)
	}
	props, _ := meta["properties"].(map[string]any)
	flagsRaw, _ := props["flags"].(map[string]any)
	if flagsRaw == nil {
		t.Fatalf("%s has no `flags` property — the Flags check has nothing to walk", envelopeMetaRef)
	}
	flags := mergeSchema(doc, flagsRaw)
	fp, _ := flags["properties"].(map[string]any)
	if len(fp) == 0 {
		t.Fatalf("the Flags schema behind %s.properties.flags declares no properties — the check would be vacuous", envelopeMetaRef)
	}
	out := make(map[string]bool, len(fp))
	for k := range fp {
		out[k] = true
	}
	return out
}

// describeOrigins renders where a property is declared, capped so a
// member every one of the spec's envelopes carries does not turn one
// failure line into a page.
func describeOrigins(where []string) string {
	const shown = 4
	if len(where) <= shown {
		return strings.Join(where, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(where[:shown], ", "), len(where)-shown)
}

// TestSDKEnvelopeMatchesSpec — the generic Envelope[T] must carry
// exactly the top-level members the spec's envelopes declare, in
// both directions. A spec member with no SDK field is silently
// dropped by the decoder for every operation at once; an SDK field
// no envelope documents is a wire shape the server never sends.
func TestSDKEnvelopeMatchesSpec(t *testing.T) {
	doc := loadSpec(t)
	specProps := specEnvelopeProps(t, doc)
	goProps := jsonTags(reflect.TypeOf(Envelope[json.RawMessage]{}))

	var missingInGo, missingInSpec []string
	for p, where := range specProps {
		if !goProps[p] {
			missingInGo = append(missingInGo, fmt.Sprintf("%s (declared by %s)", p, describeOrigins(where)))
		}
	}
	for p := range goProps {
		if specProps[p] == nil {
			missingInSpec = append(missingInSpec, p)
		}
	}
	sort.Strings(missingInGo)
	sort.Strings(missingInSpec)
	if len(missingInGo) > 0 {
		t.Errorf("spec envelopes declare top-level members the SDK Envelope[T] does not carry — the SDK silently drops them on every operation: %v", missingInGo)
	}
	if len(missingInSpec) > 0 {
		t.Errorf("SDK Envelope[T] declares top-level members no spec envelope documents: %v", missingInSpec)
	}
}

// TestSDKFlagsMatchSpec — the advisory Flags object must match the
// spec's Flags schema property-for-property, in both directions. The
// decoder ignores unknown fields, so a flag the spec adds and the
// SDK lacks is not an error at runtime — it is a signal consumers
// can never read, which is exactly the drift this pins.
func TestSDKFlagsMatchSpec(t *testing.T) {
	doc := loadSpec(t)
	specProps := specFlagsProps(t, doc)
	goProps := jsonTags(reflect.TypeOf(Flags{}))

	var missingInGo, missingInSpec []string
	for p := range specProps {
		if !goProps[p] {
			missingInGo = append(missingInGo, p)
		}
	}
	for p := range goProps {
		if !specProps[p] {
			missingInSpec = append(missingInSpec, p)
		}
	}
	sort.Strings(missingInGo)
	sort.Strings(missingInSpec)
	if len(missingInGo) > 0 {
		t.Errorf("spec Flags declares flags the SDK Flags type does not carry — the SDK silently drops them: %v", missingInGo)
	}
	if len(missingInSpec) > 0 {
		t.Errorf("SDK Flags declares flags the spec does not document: %v", missingInSpec)
	}
}

// TestSpecEnvelopeWalkSeesEveryComposer pins the walk behind
// TestSDKEnvelopeMatchesSpec and TestSDKFlagsMatchSpec to a synthetic
// document. Against the real spec a walk that skipped inline
// responses, oneOf branches, or an inline Flags object passes exactly
// like one that covered them, because nothing is drifting there
// today; this is the run that tells the two apart.
func TestSpecEnvelopeWalkSeesEveryComposer(t *testing.T) {
	str := map[string]any{"type": "string"}
	obj := func(props ...string) map[string]any {
		pm := map[string]any{}
		for _, p := range props {
			pm[p] = str
		}
		return map[string]any{"type": "object", "properties": pm}
	}
	composer := func(props ...string) map[string]any {
		return map[string]any{"allOf": []any{
			map[string]any{"$ref": envelopeMetaRef},
			obj(props...),
		}}
	}
	response := func(schema map[string]any) map[string]any {
		return map[string]any{"responses": map[string]any{"200": map[string]any{
			"content": map[string]any{"application/json": map[string]any{"schema": schema}},
		}}}
	}
	doc := map[string]any{
		"components": map[string]any{"schemas": map[string]any{
			"EnvelopeMeta": map[string]any{"type": "object", "properties": map[string]any{
				"as_of": str,
				"flags": obj("stale", "inline_flag"),
			}},
			"ListEnvelope": composer("data", "pagination"),
			"Unrelated":    obj("not_an_envelope_member"),
		}},
		"paths": map[string]any{
			"/inline": map[string]any{"get": response(composer("data", "inline_member"))},
			"/either": map[string]any{"get": response(map[string]any{"oneOf": []any{
				map[string]any{"$ref": "#/components/schemas/ListEnvelope"},
				composer("data", "branch_member"),
			}})},
		},
	}

	got := specEnvelopeProps(t, doc)
	want := map[string][]string{
		"as_of":         {"EnvelopeMeta"},
		"flags":         {"EnvelopeMeta"},
		"data":          {"GET /either 200 oneOf[1]", "GET /inline 200", "ListEnvelope"},
		"pagination":    {"ListEnvelope"},
		"inline_member": {"GET /inline 200"},
		"branch_member": {"GET /either 200 oneOf[1]"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envelope walk\n got: %v\nwant: %v", got, want)
	}

	flags := specFlagsProps(t, doc)
	wantFlags := map[string]bool{"stale": true, "inline_flag": true}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("inline Flags walk\n got: %v\nwant: %v", flags, wantFlags)
	}
}

// TestProblemJSONMatchesSpec is the error-path twin of
// TestSDKSchemasMatchSpec. The `data` walk above never reaches the
// spec's `Problem` schema — it is a response body, not an envelope —
// so an extension member added to the server's problem type and to
// the spec could be missing from problemJSON with every other gate
// green. Bidirectional: a member in the spec the SDK does not decode
// is a field consumers cannot read; a tag in the SDK the spec does not
// document is a field consumers cannot trust.
func TestProblemJSONMatchesSpec(t *testing.T) {
	doc := loadSpec(t)
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	problem, _ := schemas["Problem"].(map[string]any)
	if problem == nil {
		t.Fatal("spec has no components.schemas.Problem")
	}
	props, _ := problem["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatal("spec Problem schema has no properties")
	}
	specProps := map[string]bool{}
	for name := range props {
		specProps[name] = true
	}
	sdkTags := jsonTags(reflect.TypeOf(problemJSON{}))

	var missing, extra []string
	for name := range specProps {
		if !sdkTags[name] {
			missing = append(missing, name)
		}
	}
	for name := range sdkTags {
		if !specProps[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("spec Problem members the SDK does not decode: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("SDK problemJSON tags the spec Problem schema does not document: %v", extra)
	}
}
