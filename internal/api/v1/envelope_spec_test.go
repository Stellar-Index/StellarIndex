package v1

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

const envelopeMetaRef = "#/components/schemas/EnvelopeMeta"

// bareResponseOperations are the operations whose 2xx body is the bare
// resource on the wire, so their spec schemas deliberately omit EnvelopeMeta.
var bareResponseOperations = map[string]string{
	"adminLookupCustomer":           "session-cookie admin lookup; handler writes the bare DTO via httpx.WriteJSON",
	"exportDashboardAccount":        "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"listDashboardKeys":             "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"createDashboardKey":            "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"listDashboardWebhooks":         "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"createDashboardWebhook":        "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"updateDashboardWebhook":        "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"getDashboardWebhookDeliveries": "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"listDashboardPriceAlerts":      "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"createDashboardPriceAlert":     "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"updateDashboardPriceAlert":     "session-cookie dashboard flow; handler writes the bare DTO via httpx.WriteJSON",
	"requestMagicLink":              "session-cookie magic-link sign-in; handler writes the bare DTO",
	"verifyLoginCode":               "session-cookie magic-link sign-in; handler writes the bare DTO",
	"beginPasskeyLogin":             "session-cookie passkey sign-in; handler writes the bare WebAuthn options",
	"finishPasskeyLogin":            "session-cookie passkey sign-in; handler writes the bare DTO",
	"beginPasskeyRegistration":      "session-cookie passkey registration; handler writes the bare WebAuthn options",
	"finishPasskeyRegistration":     "session-cookie passkey registration; handler writes the bare DTO",
	"listPasskeyCredentials":        "session-cookie passkey management; handler writes the bare DTO",
}

// composesEnvelope reports whether node is, references, or allOf-composes
// EnvelopeMeta.
func composesEnvelope(doc map[string]any, node any, depth int) bool {
	m, _ := node.(map[string]any)
	if m == nil || depth > 32 {
		return false
	}
	if ref, ok := m["$ref"].(string); ok {
		if ref == envelopeMetaRef {
			return true
		}
		name, local := strings.CutPrefix(ref, "#/components/schemas/")
		return local && composesEnvelope(doc, specDig(doc, "components", "schemas", name), depth+1)
	}
	members, _ := m["allOf"].([]any)
	for _, part := range members {
		if composesEnvelope(doc, part, depth+1) {
			return true
		}
	}
	return false
}

// bareEnvelopeResponses lists every 2xx application/json response (or oneOf
// branch of one) whose schema does not compose EnvelopeMeta, as
// "operationId METHOD path status", plus how many responses it examined.
func bareEnvelopeResponses(doc map[string]any) (bare []string, checked int) {
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, op := range methods {
			responses, _ := specDig(op, "responses").(map[string]any)
			opID, _ := specDig(op, "operationId").(string)
			for status, resp := range responses {
				if !strings.HasPrefix(status, "2") {
					continue
				}
				if ref, ok := specDig(resp, "$ref").(string); ok {
					name, _ := strings.CutPrefix(ref, "#/components/responses/")
					resp = specDig(doc, "components", "responses", name)
				}
				schema := specDig(resp, "content", "application/json", "schema")
				if schema == nil {
					continue
				}
				checked++
				branches := []any{schema}
				if oneOf, ok := specDig(schema, "oneOf").([]any); ok {
					branches = oneOf
				}
				for _, b := range branches {
					if !composesEnvelope(doc, b, 0) {
						bare = append(bare, opID+" "+strings.ToUpper(method)+" "+path+" "+status)
						break
					}
				}
			}
		}
	}
	sort.Strings(bare)
	return bare, checked
}

// TestEvery2xxJSONResponseComposesEnvelopeMeta holds EnvelopeMeta's "every
// 2xx JSON response carries these" to the spec, with named exemptions.
func TestEvery2xxJSONResponseComposesEnvelopeMeta(t *testing.T) {
	bare, checked := bareEnvelopeResponses(loadSpecDoc(t))
	if checked == 0 {
		t.Fatal("walked no 2xx application/json responses; the spec walk is broken")
	}
	seen := map[string]bool{}
	for _, entry := range bare {
		opID, _, _ := strings.Cut(entry, " ")
		seen[opID] = true
		if _, ok := bareResponseOperations[opID]; !ok {
			t.Errorf("%s: spec 2xx does not compose EnvelopeMeta but EnvelopeMeta says every 2xx carries it — wrap it, or, if the handler really writes a bare body, add it to bareResponseOperations and the EnvelopeMeta description", entry)
		}
	}
	for opID := range bareResponseOperations {
		if !seen[opID] {
			t.Errorf("bareResponseOperations[%q] is stale: the operation is gone or its 2xx now composes EnvelopeMeta", opID)
		}
	}
}

// TestBareEnvelopeResponsesWalk pins the walk on a synthetic document so a
// walk that skips a branch cannot pass the spec test vacuously.
func TestBareEnvelopeResponsesWalk(t *testing.T) {
	dataObj := func() map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{"data": map[string]any{"type": "object"}}}
	}
	inlineEnvelope := func() map[string]any {
		return map[string]any{"allOf": []any{map[string]any{"$ref": envelopeMetaRef}, dataObj()}}
	}
	jsonResp := func(schema any) map[string]any {
		return map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": schema}}}
	}
	op := func(id, status string, resp any) map[string]any {
		return map[string]any{"get": map[string]any{
			"operationId": id,
			"responses":   map[string]any{status: resp},
		}}
	}
	xEnvelopeRef := map[string]any{"$ref": "#/components/schemas/XEnvelope"}
	doc := map[string]any{
		"components": map[string]any{
			"schemas": map[string]any{
				"EnvelopeMeta": map[string]any{"type": "object", "properties": map[string]any{"as_of": map[string]any{"type": "string"}}},
				"XEnvelope":    inlineEnvelope(),
			},
			"responses": map[string]any{"R": jsonResp(dataObj())},
		},
		"paths": map[string]any{
			"/ok-ref":     op("okRef", "200", jsonResp(xEnvelopeRef)),
			"/ok-inline":  op("okInline", "200", jsonResp(inlineEnvelope())),
			"/bare":       op("bareInline", "200", jsonResp(dataObj())),
			"/mixed":      op("mixedOneOf", "200", jsonResp(map[string]any{"oneOf": []any{xEnvelopeRef, dataObj()}})),
			"/via-ref":    op("viaResponseRef", "201", map[string]any{"$ref": "#/components/responses/R"}),
			"/no-body":    op("noBody", "204", map[string]any{"description": "no content"}),
			"/plain-text": op("nonJSON", "200", map[string]any{"content": map[string]any{"text/plain": map[string]any{"schema": dataObj()}}}),
		},
	}

	bare, checked := bareEnvelopeResponses(doc)
	want := []string{
		"bareInline GET /bare 200",
		"mixedOneOf GET /mixed 200",
		"viaResponseRef GET /via-ref 201",
	}
	if !reflect.DeepEqual(bare, want) {
		t.Errorf("bare = %q, want %q", bare, want)
	}
	if checked != 5 {
		t.Errorf("checked = %d, want 5 (the 204 and the text/plain response must be skipped)", checked)
	}
}
