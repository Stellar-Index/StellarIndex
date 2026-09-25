package v1

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
)

// TestHandlerResponseFieldsAreDocumented closes the gap that let
// `display_decimals` ship invisible (wave-D F-SDK-04).
//
// Two gates already reconcile this area and NEITHER could see it:
//
//   - lint-docs.sh compares handlers to the spec at ROUTE granularity
//     (method + path) — never fields.
//   - pkg/client's TestSDKSchemasMatchSpec compares the SDK to the spec
//     bidirectionally, which is genuinely useful but reconciles two
//     DERIVED artifacts. When a field exists only on the server, the SDK
//     and the spec agree — on both being wrong — and the gate stays
//     green.
//
// So a server-added response field was invisible to the whole chain.
// That is not hypothetical: F-1321 moved the issuer's SEP-1 rounding
// hint OFF `decimals` (where it inflated market_cap_usd by up to
// 10^(7-display_decimals)× and was an issuer-controlled manipulation
// vector) onto a new `display_decimals` field. The field never entered
// the spec, so every SDK consumer and the generated explorer types
// dropped it — the remediation's whole replacement surface was
// unreachable from the published product.
//
// This compares the HANDLER struct, which is the source of truth, to
// the spec.
//
// handlerSpecFieldPairs is every (spec schema, handler struct) pair this
// guard reconciles. It started as a single hardcoded pair (Asset /
// AssetDetail) — that covered the field display_decimals shipped invisible
// on, but left every other response struct with the same blind spot. Add a
// pair here whenever a new top-level response struct is introduced.
//
// schema is a components.schemas name, or "METHOD /path" for a route whose
// response schema is written inline (see specSchemaNode).
var handlerSpecFieldPairs = []struct {
	schema string
	typ    reflect.Type
}{
	{"Asset", reflect.TypeOf(AssetDetail{})},
	{"AssetSupply", reflect.TypeOf(AssetSupply{})},
	{"TradeRow", reflect.TypeOf(TradeRow{})},
	{"OHLCBar", reflect.TypeOf(OHLCBar{})},
	{"Price", reflect.TypeOf(PriceSnapshot{})},
	{"AccountActivity", reflect.TypeOf(explorerpkg.AccountActivityView{})},
	{"AccountTrade", reflect.TypeOf(explorerpkg.AccountTradeEntry{})},
	{"TxSummary", reflect.TypeOf(explorerpkg.TxSummaryView{})},
	{"KeyCreated", reflect.TypeOf(KeyCreated{})},
	{"AccountUser", reflect.TypeOf(AccountUser{})},
	{"AccountInfo", reflect.TypeOf(AccountInfo{})},
	{"LakeHealth", reflect.TypeOf(lakeHealth{})},
	{"ProtocolBespoke", reflect.TypeOf(ProtocolBespoke{})},
	{"BespokeKPI", reflect.TypeOf(BespokeKPI{})},
	{"BespokeSeries", reflect.TypeOf(BespokeSeries{})},
	{"BespokeSeriesPoint", reflect.TypeOf(BespokeSeriesPt{})},
	{"BespokeBreakdown", reflect.TypeOf(BespokeBreakdown{})},
	{"BespokeBreakdownRow", reflect.TypeOf(BespokeBreakdownRow{})},
	{"BespokeTable", reflect.TypeOf(BespokeTable{})},
	{"RWAAsset", reflect.TypeOf(RWAAsset{})},
	{"GET /diagnostics/ingestion", reflect.TypeOf(IngestionDiagnostics{})},
}

// TestHandlerRequiredFieldsAreAlwaysServed is the other direction: a
// property the spec marks `required` must be a handler field that is never
// omitted. `omitempty` on it drops a guaranteed field whenever the value is
// zero, so a schema-validating client rejects a successful response.
func TestHandlerRequiredFieldsAreAlwaysServed(t *testing.T) {
	for _, pair := range handlerSpecFieldPairs {
		t.Run(pair.schema, func(t *testing.T) {
			opts := structJSONTagOptions(pair.typ)
			for _, f := range specSchemaRequired(t, pair.schema) {
				o, ok := opts[f]
				switch {
				case !ok:
					t.Errorf("spec requires %q but %s has no such field", f, pair.typ.Name())
				case strings.Contains(o, "omitempty") || strings.Contains(o, "omitzero"):
					t.Errorf("spec requires %q but %s tags it %q — drop the omit option or make it optional in the spec",
						f, pair.typ.Name(), o)
				}
			}
		})
	}
}

// structJSONTagOptions maps each wire field name to its json tag options
// (the part after the first comma), following embedded structs.
func structJSONTagOptions(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if embedded := embeddedStruct(f); embedded != nil {
			for k, v := range structJSONTagOptions(embedded) {
				out[k] = v
			}
			continue
		}
		tag := f.Tag.Get("json")
		if !f.IsExported() || tag == "-" {
			continue
		}
		name, rest, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = rest
	}
	return out
}

// embeddedStruct returns the struct type encoding/json promotes into the
// parent for field f — an untagged embedded struct or pointer to one — or
// nil when f is an ordinary (or tag-named, or `json:"-"`) field.
func embeddedStruct(f reflect.StructField) reflect.Type {
	if !f.Anonymous || f.Tag.Get("json") != "" && !strings.HasPrefix(f.Tag.Get("json"), ",") {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		if !f.IsExported() {
			return nil // encoding/json ignores an embedded pointer to an unexported type
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// specSchemaRequired returns the `required` list of a pair's schema, with
// $ref and allOf resolved (see specSchemaNode).
func specSchemaRequired(t *testing.T, locator string) []string {
	t.Helper()
	raw, _ := specSchemaNode(t, locator)["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if name, ok := r.(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func TestHandlerResponseFieldsAreDocumented(t *testing.T) {
	for _, pair := range handlerSpecFieldPairs {
		pair := pair
		t.Run(pair.schema, func(t *testing.T) {
			props := specSchemaProps(t, pair.schema)
			if len(props) == 0 {
				t.Fatalf("resolved no properties for the %s schema — the lookup is "+
					"broken, and a check with an empty subject set passes forever", pair.schema)
			}

			got := structJSONTags(pair.typ)
			if len(got) == 0 {
				t.Fatalf("%s exposed no json tags — the reflection walk is broken", pair.typ.Name())
			}

			// Fields the handler serves that the spec does not document. This is
			// the direction that hid display_decimals: a consumer cannot ask for
			// what it has never been told exists.
			var undocumented []string
			for f := range got {
				if !props[f] && handlerFieldSpecExceptions[f] == "" {
					undocumented = append(undocumented, f)
				}
			}
			sort.Strings(undocumented)
			if len(undocumented) > 0 {
				t.Errorf("%s serves field(s) the OpenAPI spec does not document: %v\n"+
					"A server field absent from the spec is invisible to pkg/client, to the "+
					"explorer's generated types, and to every downstream consumer — and the "+
					"SDK-vs-spec gate cannot see it, because that reconciles two derived "+
					"artifacts which agree when BOTH are missing the field. Document it in "+
					"openapi/stellar-index.v1.yaml (and regenerate), or record it in "+
					"handlerFieldSpecExceptions with the reason it is deliberately internal.",
					pair.typ.Name(), undocumented)
			}
		})
	}
}

// TestOpenAPIAccountInfoStatusEnumMatchesPlatformAccountStatus is the
// vocabulary-level counterpart to TestHandlerResponseFieldsAreDocumented
// above: that test reconciles FIELD NAMES against the spec, but a field
// whose name is documented can still carry an undocumented VALUE. account.go
// serves AccountInfo.Status as string(platform.AccountStatus) verbatim
// (account.go:267), so every platform.AccountStatus constant must appear
// in the spec's AccountInfo.status enum or a schema-validating client
// rejects an otherwise-successful response. The constants are read from
// source so a new status fails here until the spec names it.
func TestOpenAPIAccountInfoStatusEnumMatchesPlatformAccountStatus(t *testing.T) {
	want := goStringConsts(t, filepath.Join("internal", "platform"), "AccountStatus")
	if len(want) == 0 {
		t.Fatal("found no platform.AccountStatus constants — the source walk is broken")
	}

	got := specEnumAt(t, loadSpecDoc(t), "components", "schemas", "AccountInfo", "properties", "status")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi AccountInfo.status enum = %v, want %v (every platform.AccountStatus "+
			"constant) — account.go serves string(AccountStatus) directly, so an undocumented "+
			"or unreachable enum value here is a client-visible contract gap",
			got, want)
	}
}

// handlerFieldSpecExceptions records response fields deliberately absent
// from the published spec, with the reason. An entry is a decision, not
// a way to silence the check.
var handlerFieldSpecExceptions = map[string]string{}

// structJSONTags returns the wire field names of a struct, following
// embedded structs and skipping `json:"-"`.
func structJSONTags(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if embedded := embeddedStruct(f); embedded != nil {
			for k := range structJSONTags(embedded) {
				out[k] = true
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// specSchemaProps returns the property names of a pair's schema, with
// $ref and allOf resolved (see specSchemaNode).
func specSchemaProps(t *testing.T, locator string) map[string]bool {
	t.Helper()
	props, _ := specSchemaNode(t, locator)["properties"].(map[string]any)
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// specSchemaNode resolves a pair locator to one flattened schema. A bare
// name is a components.schemas entry; "METHOD /path" is a route whose 200
// application/json body is the standard envelope, and the served struct is
// its `data` member (array items unwrapped) — the only way an inline route
// schema gets reconciled at all.
func specSchemaNode(t *testing.T, locator string) map[string]any {
	t.Helper()
	doc := loadSpecDoc(t)
	method, path, isRoute := strings.Cut(locator, " ")
	if !isRoute {
		return resolveSpecSchema(doc, map[string]any{"$ref": "#/components/schemas/" + locator}, 0)
	}
	body := specDig(doc, "paths", path, strings.ToLower(method), "responses", "200",
		"content", "application/json", "schema")
	if body == nil {
		t.Fatalf("spec has no 200 application/json schema for %s", locator)
	}
	props, _ := resolveSpecSchema(doc, body, 0)["properties"].(map[string]any)
	data := resolveSpecSchema(doc, props["data"], 0)
	if items, ok := data["items"]; ok && data["type"] == "array" {
		data = resolveSpecSchema(doc, items, 0)
	}
	return data
}

// specDig walks nested maps by key, returning nil on the first miss.
func specDig(node any, keys ...string) any {
	for _, k := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[k]
	}
	return node
}

// resolveSpecSchema follows local $refs and merges allOf members, so a
// property contributed by a referenced or composed schema counts the same
// as one written inline. The result carries the merged `properties` and
// `required` alongside the node's other keys.
func resolveSpecSchema(doc map[string]any, node any, depth int) map[string]any {
	m, _ := node.(map[string]any)
	if m == nil || depth > 32 {
		return map[string]any{}
	}
	if ref, ok := m["$ref"].(string); ok {
		name, local := strings.CutPrefix(ref, "#/components/schemas/")
		if !local {
			return map[string]any{}
		}
		return resolveSpecSchema(doc, specDig(doc, "components", "schemas", name), depth+1)
	}
	members, _ := m["allOf"].([]any)
	if len(members) == 0 {
		return m
	}
	out := map[string]any{}
	props := map[string]any{}
	var required []any
	merge := func(r map[string]any) {
		p, _ := r["properties"].(map[string]any)
		for k, v := range p {
			props[k] = v
		}
		req, _ := r["required"].([]any)
		required = append(required, req...)
	}
	for k, v := range m {
		if k != "allOf" {
			out[k] = v
		}
	}
	merge(out)
	for _, part := range members {
		merge(resolveSpecSchema(doc, part, depth+1))
	}
	out["properties"] = props
	out["required"] = required
	return out
}

type specWalkInner struct {
	Promoted string `json:"promoted"`
}

type specWalkOuter struct {
	*specWalkInner
	Own string `json:"own,omitempty"`
}

type SpecWalkExportedInner struct {
	Promoted string `json:"promoted"`
}

type specWalkOuterExported struct {
	*SpecWalkExportedInner
	Own string `json:"own,omitempty"`
}

// TestStructJSONWalkers_MatchEncodingJSONPromotion pins the walkers to what
// encoding/json actually serves: an embedded pointer to an exported struct
// promotes its fields (the walker used to follow only value embeds, so
// those fields were invisible to both directions of the gate), and an
// embedded pointer to an unexported type is dropped.
func TestStructJSONWalkers_MatchEncodingJSONPromotion(t *testing.T) {
	got := structJSONTags(reflect.TypeOf(specWalkOuterExported{}))
	if !got["promoted"] || !got["own"] || len(got) != 2 {
		t.Errorf("structJSONTags(embedded exported pointer) = %v, want {promoted, own}", got)
	}
	opts := structJSONTagOptions(reflect.TypeOf(specWalkOuterExported{}))
	if o, ok := opts["promoted"]; !ok || o != "" {
		t.Errorf("structJSONTagOptions missed the promoted field: %v", opts)
	}
	unexported := structJSONTags(reflect.TypeOf(specWalkOuter{}))
	if unexported["promoted"] || !unexported["own"] {
		t.Errorf("structJSONTags(embedded unexported pointer) = %v, want {own} only", unexported)
	}
}

// TestSpecSchemaResolver_FollowsRefAllOfAndRoutes: a schema composed with
// allOf, or reached through a $ref, must resolve to the properties it
// serves — a flat `.properties` read sees none of them, and a pair that
// resolves to nothing is a check that cannot fail.
func TestSpecSchemaResolver_FollowsRefAllOfAndRoutes(t *testing.T) {
	env := specSchemaProps(t, "LakeHealthEnvelope")
	if !env["data"] || !env["as_of"] {
		t.Errorf("LakeHealthEnvelope (allOf EnvelopeMeta + data) resolved to %v, want data and as_of", env)
	}
	route := specSchemaProps(t, "GET /livez/lake")
	want := specSchemaProps(t, "LakeHealth")
	if len(want) == 0 || !reflect.DeepEqual(route, want) {
		t.Errorf("GET /livez/lake data resolved to %v, want the LakeHealth properties %v", route, want)
	}
	if req := specSchemaRequired(t, "GET /livez/lake"); !reflect.DeepEqual(req, []string{"status"}) {
		t.Errorf("GET /livez/lake data required = %v, want [status]", req)
	}
}
