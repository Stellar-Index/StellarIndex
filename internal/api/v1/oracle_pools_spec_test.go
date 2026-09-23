package v1

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOraclePricesSpecDocumentsAsset and TestPoolsSpecDocumentsPagination
// close RLT-127: `/v1/oracle/prices` rows always carry `asset`
// (handleOraclePrices builds SEP40Price{Asset: asset.String(), ...} for
// every row — oracle_sep40.go) and `/v1/pools` serves a `pagination`
// cursor on non-final pages, but the OpenAPI schemas for both response
// bodies omitted the field/property, so a spec-driven client couldn't
// type either one.
//
// These load the spec at full document depth (unlike specSchemaProps,
// which only resolves a named schema's TOP-LEVEL properties and can't
// see into an allOf branch or an array item schema) because both gaps
// live one level below that.

func loadSpecDoc(t *testing.T) map[string]any {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var specPath string
	for i := 0; i < 8; i++ {
		try := filepath.Join(dir, "openapi", "stellar-index.v1.yaml")
		if _, err := os.Stat(try); err == nil {
			specPath = try
			break
		}
		dir = filepath.Dir(dir)
	}
	if specPath == "" {
		t.Fatal("could not locate openapi/stellar-index.v1.yaml from cwd")
	}
	body, err := os.ReadFile(specPath) //nolint:gosec // repo-relative path resolved above
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	return doc
}

func TestOraclePricesSpecDocumentsAsset(t *testing.T) {
	doc := loadSpecDoc(t)
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	envelope, ok := schemas["OraclePricesEnvelope"].(map[string]any)
	if !ok {
		t.Fatal("components.schemas.OraclePricesEnvelope not found")
	}
	allOf, ok := envelope["allOf"].([]any)
	if !ok || len(allOf) < 2 {
		t.Fatalf("OraclePricesEnvelope.allOf malformed: %#v", envelope["allOf"])
	}
	branch, ok := allOf[1].(map[string]any)
	if !ok {
		t.Fatalf("OraclePricesEnvelope.allOf[1] malformed: %#v", allOf[1])
	}
	props, _ := branch["properties"].(map[string]any)
	data, ok := props["data"].(map[string]any)
	if !ok {
		t.Fatal("OraclePricesEnvelope.allOf[1].properties.data missing")
	}
	items, ok := data["items"].(map[string]any)
	if !ok {
		t.Fatal("OraclePricesEnvelope data.items missing")
	}
	itemProps, _ := items["properties"].(map[string]any)
	if _, ok := itemProps["asset"]; !ok {
		t.Errorf("OraclePricesEnvelope row schema does not document `asset`, but "+
			"handleOraclePrices always sets SEP40Price.Asset on every row "+
			"(oracle_sep40.go) — got properties: %v", itemProps)
	}
	required, _ := items["required"].([]any)
	var hasAssetRequired bool
	for _, r := range required {
		if r == "asset" {
			hasAssetRequired = true
		}
	}
	if !hasAssetRequired {
		t.Errorf("OraclePricesEnvelope row schema does not require `asset` — got required: %v", required)
	}
}

func TestPoolsSpecDocumentsPagination(t *testing.T) {
	doc := loadSpecDoc(t)
	paths, _ := doc["paths"].(map[string]any)
	pools, ok := paths["/pools"].(map[string]any)
	if !ok {
		t.Fatal("paths./pools not found")
	}
	get, ok := pools["get"].(map[string]any)
	if !ok {
		t.Fatal("paths./pools.get not found")
	}
	responses, _ := get["responses"].(map[string]any)
	resp200, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatal("paths./pools.get.responses.200 not found")
	}
	content, _ := resp200["content"].(map[string]any)
	appJSON, _ := content["application/json"].(map[string]any)
	schema, ok := appJSON["schema"].(map[string]any)
	if !ok {
		t.Fatal("paths./pools.get.responses.200.content.application/json.schema not found")
	}
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) < 2 {
		t.Fatalf("/pools 200 schema.allOf malformed: %#v", schema["allOf"])
	}
	branch, ok := allOf[1].(map[string]any)
	if !ok {
		t.Fatalf("/pools 200 schema.allOf[1] malformed: %#v", allOf[1])
	}
	props, _ := branch["properties"].(map[string]any)
	if _, ok := props["pagination"]; !ok {
		t.Errorf("/pools 200 schema does not document `pagination`, but the "+
			"wire response carries a pagination cursor when more rows exist "+
			"(see the example's `pagination.next`) — got properties: %v", props)
	}
}
