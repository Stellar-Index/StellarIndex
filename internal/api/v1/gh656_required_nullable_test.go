package v1

import "testing"

// TestGH656RequiredFieldsDocumented pins the `required` half of GH-656:
// these schemas serve every listed property unconditionally (non-pointer,
// no omitempty in the Go view struct) but the spec declared no `required`
// at all, so a generated client typed every one of them optional.
func TestGH656RequiredFieldsDocumented(t *testing.T) {
	doc := loadSpecDoc(t)
	schemas := mustMap(t, doc, "components", "schemas")

	cases := map[string][]string{
		"Ledger":              {"sequence", "close_time", "hash", "prev_hash", "protocol_version", "tx_count", "op_count", "soroban_event_count", "total_coins", "fee_pool", "base_fee", "base_reserve"},
		"TxSummary":           {"hash", "ledger", "close_time", "index", "source_account", "fee_charged", "max_fee", "operation_count", "successful", "result_code", "result"},
		"Operation":           {"ledger", "close_time", "tx_hash", "tx_index", "op_index", "type"},
		"ContractEvent":       {"op_index", "event_index", "contract_id", "event_type"},
		"AccountTransactions": {"account", "transactions", "scope"},
		"AccountOperations":   {"account", "operations", "scope"},
		"UsageRow":            {"date", "requests", "billable", "errors", "throttled"},
	}

	for name, want := range cases {
		sch := mustMap(t, schemas, name)
		req, _ := sch["required"].([]any)
		got := map[string]bool{}
		for _, r := range req {
			if s, ok := r.(string); ok {
				got[s] = true
			}
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s: %q served unconditionally by its Go view struct but not required in the spec", name, w)
			}
		}
	}

	// TxDetail is an allOf; the second branch's own `required` must name
	// `operations` (TxDetailView.Operations has no omitempty).
	txDetail := mustMap(t, schemas, "TxDetail")
	allOf, _ := txDetail["allOf"].([]any)
	if len(allOf) < 2 {
		t.Fatalf("TxDetail.allOf has %d entries, want >= 2", len(allOf))
	}
	branch, _ := allOf[1].(map[string]any)
	req, _ := branch["required"].([]any)
	found := false
	for _, r := range req {
		if r == "operations" {
			found = true
		}
	}
	if !found {
		t.Error("TxDetail's second allOf branch: operations served unconditionally, not required in the spec")
	}
}

// TestGH656NullablePropertiesMatchGo pins the `nullable` half of GH-656:
// these ten properties are backed by a plain Go string/int with
// `omitempty` (never a pointer) — encoding/json OMITS a zero value, it
// never serialises `null` — so `type: [T, "null"]` documented an outcome
// that can never occur on the wire.
func TestGH656NullablePropertiesMatchGo(t *testing.T) {
	doc := loadSpecDoc(t)
	schemas := mustMap(t, doc, "components", "schemas")

	cases := []struct{ schema, prop string }{
		{"WebhookDeliveryDTO", "next_attempt_at"},
		{"WebhookDeliveryDTO", "delivered_at"},
		{"WebhookDeliveryDTO", "last_error"},
		{"WebhookDeliveryDTO", "last_response_status"},
		{"IncidentWebhookPayload", "postmortem"},
		{"Pagination", "next"},
		{"AssetListingValuation", "circulating_supply"},
		{"UnverifiedWarning", "verified_issuer"},
		{"VerifiedCurrencyListItem", "image"},
		{"Price", "window_seconds"},
	}

	for _, c := range cases {
		sch := mustMap(t, schemas, c.schema)
		prop := mustMap(t, sch, "properties", c.prop)
		switch v := prop["type"].(type) {
		case string:
			// fine — a plain scalar type, not [T, "null"]
		case []any:
			t.Errorf("%s.%s: type %v documents null, but the Go field is a non-pointer omitempty — it is only ever omitted, never null", c.schema, c.prop, v)
		default:
			t.Errorf("%s.%s: unexpected type field %T", c.schema, c.prop, v)
		}
	}
}
