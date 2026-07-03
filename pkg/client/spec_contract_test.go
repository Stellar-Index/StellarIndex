// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package client

import (
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
	{sdkMethod: "Asset", method: "GET", path: "/assets/{asset_id}", payload: AssetDetail{}, envelopeRef: "#/components/schemas/AssetEnvelope"},
	{"AssetMetadata", "GET", "/assets/{asset_id}/metadata", AssetMetadata{}, ""},
	{"Sources", "GET", "/sources", Source{}, ""},
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
	"GET /accounts/{g_strkey}":                  "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/transactions":     "explorer surface — SDK is pricing-first",
	"GET /accounts/{g_strkey}/operations":       "explorer surface — SDK is pricing-first",
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
	"GET /lending/pools/{pool}/reserves": "lending drill-down — explorer surface",
	"GET /mev":                           "MEV feed — explorer surface",
	"GET /anomalies":                     "anomaly feed — explorer surface",
	"GET /divergence":                    "divergence feed — explorer surface",
	"GET /coverage":                      "coverage verdict — explorer/status surface",
	"GET /diagnostics/ingestion":         "operator diagnostics",
	"GET /ledger/tip":                    "explorer tip feed",
	"GET /incidents.atom":                "Atom feed — not JSON",

	// Auth/session/webhook flows — browser/dashboard interactions,
	// not machine-SDK surface.
	"POST /webhooks/stripe":                   "Stripe calls this, not customers",
	"POST /signup":                            "browser onboarding flow",
	"GET /signup/verify":                      "browser onboarding flow",
	"GET /dashboard/keys":                     "session-cookie dashboard surface",
	"POST /dashboard/keys":                    "session-cookie dashboard surface",
	"DELETE /dashboard/keys/{id}":             "session-cookie dashboard surface",
	"GET /dashboard/webhooks":                 "session-cookie dashboard surface",
	"POST /dashboard/webhooks":                "session-cookie dashboard surface",
	"PATCH /dashboard/webhooks/{id}":          "session-cookie dashboard surface",
	"DELETE /dashboard/webhooks/{id}":         "session-cookie dashboard surface",
	"GET /dashboard/webhooks/{id}/deliveries": "session-cookie dashboard surface",
	"POST /auth/login":                        "magic-link browser flow",
	"GET /auth/callback":                      "magic-link browser flow",
	"POST /auth/verify-code":                  "magic-link browser flow",
	"POST /auth/logout":                       "magic-link browser flow",
	"GET /auth/sep10/challenge":               "SEP-10 wallet flow — wallet SDKs handle this",
	"POST /auth/sep10/token":                  "SEP-10 wallet flow — wallet SDKs handle this",
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
