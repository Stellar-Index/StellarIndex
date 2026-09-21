// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// K027 — the divergence-service and freeze-sink customer-webhook
// payloads were `map[string]any` literals: nothing tied their key set
// to openapi/stellar-index.v1.yaml's AnomalyFreezeWebhookPayload /
// DivergenceFiringWebhookPayload schemas, so a field rename on either
// side would drift silently into production deliveries. This test
// walks the spec's `properties` for both schemas and the equivalent
// Go struct's `json` tags and fails on any mismatch, in either
// direction.
const aggregatorSpecPath = "../../openapi/stellar-index.v1.yaml"

func loadAggregatorSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(aggregatorSpecPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	return doc
}

// specSchemaProps returns the property names of components.schemas.<name>.
func specSchemaProps(t *testing.T, doc map[string]any, name string) map[string]bool {
	t.Helper()
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schema, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas.%s not found in spec", name)
	}
	props, _ := schema["properties"].(map[string]any)
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// structJSONTags returns the set of `json` tag names on typ's fields.
func structJSONTags(typ reflect.Type) map[string]bool {
	out := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}

func assertPropsMatch(t *testing.T, schemaName string, spec, goStruct map[string]bool) {
	t.Helper()
	var missingInStruct, extraInStruct []string
	for k := range spec {
		if !goStruct[k] {
			missingInStruct = append(missingInStruct, k)
		}
	}
	for k := range goStruct {
		if !spec[k] {
			extraInStruct = append(extraInStruct, k)
		}
	}
	sort.Strings(missingInStruct)
	sort.Strings(extraInStruct)
	if len(missingInStruct) > 0 {
		t.Errorf("%s: spec has fields the Go struct is missing: %v", schemaName, missingInStruct)
	}
	if len(extraInStruct) > 0 {
		t.Errorf("%s: Go struct has fields the spec doesn't document: %v", schemaName, extraInStruct)
	}
}

func TestAnomalyFreezeWebhookPayloadMatchesSpec(t *testing.T) {
	doc := loadAggregatorSpec(t)
	spec := specSchemaProps(t, doc, "AnomalyFreezeWebhookPayload")
	got := structJSONTags(reflect.TypeOf(anomalyFreezeWebhookPayload{}))
	assertPropsMatch(t, "AnomalyFreezeWebhookPayload", spec, got)
}

func TestDivergenceFiringWebhookPayloadMatchesSpec(t *testing.T) {
	doc := loadAggregatorSpec(t)
	spec := specSchemaProps(t, doc, "DivergenceFiringWebhookPayload")
	got := structJSONTags(reflect.TypeOf(divergenceFiringWebhookPayload{}))
	assertPropsMatch(t, "DivergenceFiringWebhookPayload", spec, got)
}
