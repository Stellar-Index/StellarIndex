package client

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Tests for the harness itself: a blind spot in mergeSchema or
// schemaWalker lets TestSDKSchemasMatchSpec pass over drift.

func harnessDoc() map[string]any {
	return map[string]any{"components": map[string]any{"schemas": map[string]any{
		"Money": map[string]any{
			"type":       "object",
			"required":   []any{"amount"},
			"properties": map[string]any{"amount": map[string]any{"type": "string"}},
		},
	}}}
}

func requiredNames(t *testing.T, s map[string]any) []string {
	t.Helper()
	req, _ := s["required"].([]any)
	out := make([]string, 0, len(req))
	for _, r := range req {
		name, ok := r.(string)
		if !ok {
			t.Fatalf("required entry %v is not a string", r)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func walkDrift(doc, schema map[string]any, payload any) []string {
	w := &schemaWalker{doc: doc, exc: map[string]string{}, seen: map[string]bool{}}
	w.walk(schema, reflect.TypeOf(payload), "")
	return w.drift
}

func TestMergeSchemaUnionsRequiredAcrossAllOf(t *testing.T) {
	doc := harnessDoc()
	cases := []struct {
		name   string
		schema map[string]any
		want   []string
	}{
		{
			name: "earlier $ref part and later inline part",
			schema: map[string]any{"allOf": []any{
				map[string]any{"$ref": "#/components/schemas/Money"},
				map[string]any{"required": []any{"id"}, "properties": map[string]any{"id": map[string]any{"type": "string"}}},
			}},
			want: []string{"amount", "id"},
		},
		{
			name: "own required beside allOf parts",
			schema: map[string]any{
				"required": []any{"own"},
				"allOf": []any{
					map[string]any{"$ref": "#/components/schemas/Money"},
					map[string]any{"required": []any{"id", "amount"}},
				},
			},
			want: []string{"amount", "id", "own"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requiredNames(t, mergeSchema(doc, tc.schema))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merged required = %v, want the union %v", got, tc.want)
			}
		})
	}
}

// The walker end of the same defect: a field required only by an
// earlier allOf part must still trip the omitempty check.
func TestSchemaWalkerSeesRequiredFromEarlierAllOfPart(t *testing.T) {
	type payload struct {
		Amount string `json:"amount,omitempty"`
		ID     string `json:"id"`
	}
	schema := map[string]any{"allOf": []any{
		map[string]any{"$ref": "#/components/schemas/Money"},
		map[string]any{"required": []any{"id"}, "properties": map[string]any{"id": map[string]any{"type": "string"}}},
	}}
	drift := walkDrift(harnessDoc(), schema, payload{})
	want := "amount: spec marks it required, SDK tag has omitempty"
	if len(drift) != 1 || !strings.HasPrefix(drift[0], want) {
		t.Fatalf("drift = %q, want exactly one entry starting %q", drift, want)
	}
}

func TestSchemaWalkerReportsUnresolvableOneOf(t *testing.T) {
	type payload struct {
		Value string `json:"value"`
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"value": map[string]any{"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		}},
	}}
	drift := walkDrift(harnessDoc(), schema, payload{})
	if len(drift) != 1 || !strings.HasPrefix(drift[0], "value: oneOf with 2 non-null branches") {
		t.Fatalf("drift = %q, want one entry naming value's unresolvable oneOf", drift)
	}
}

// A money-typed field reached through a nullable oneOf must be walked:
// skipping it let amount regress string→float64 unseen.
func TestSchemaWalkerWalksNullableOneOf(t *testing.T) {
	type money struct {
		Amount float64 `json:"amount"`
	}
	type payload struct {
		Price *money `json:"price"`
	}
	for _, order := range [][]any{
		{map[string]any{"$ref": "#/components/schemas/Money"}, map[string]any{"type": "null"}},
		{map[string]any{"type": "null"}, map[string]any{"$ref": "#/components/schemas/Money"}},
	} {
		schema := map[string]any{"type": "object", "properties": map[string]any{
			"price": map[string]any{"oneOf": order},
		}}
		drift := walkDrift(harnessDoc(), schema, payload{})
		want := `price.amount: spec type "string", SDK Go type float64 cannot decode it`
		if len(drift) != 1 || drift[0] != want {
			t.Fatalf("drift = %q, want [%q]", drift, want)
		}
	}
}

// A Go type with its own UnmarshalJSON owns a union's wire shape, so an
// unresolvable oneOf over it is not drift.
func TestSchemaWalkerLeavesCustomDecodedOneOf(t *testing.T) {
	type payload struct {
		Value json.RawMessage `json:"value"`
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"value": map[string]any{"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		}},
	}}
	if drift := walkDrift(harnessDoc(), schema, payload{}); len(drift) != 0 {
		t.Fatalf("drift = %q, want none", drift)
	}
}
