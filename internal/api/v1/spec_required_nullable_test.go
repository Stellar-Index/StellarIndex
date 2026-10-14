package v1

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpec_RequiredFieldsDocumented pins the `required` half of spec drift: these schemas serve every listed
// property unconditionally (non-pointer, no omitempty), so without `required` generated clients type them optional.
func TestSpec_RequiredFieldsDocumented(t *testing.T) {
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

// TestSpec_NullablePropertiesMatchGo pins the `nullable` half: these ten properties are plain Go string/int with
// `omitempty`, so encoding/json omits a zero value and never emits `null`; `type: [T, "null"]` would document an
// outcome that can't occur.
func TestSpec_NullablePropertiesMatchGo(t *testing.T) {
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

// TestSpec_LastEventIDDeclaredOnlyWhereAHandlerResumes: declaring Last-Event-ID on a stream whose handler never
// reads it makes spec-generated clients silently lose events missed while disconnected. A declaration needs an
// entry above, and that handler must call streaming.LastEventIDFrom.
func TestSpec_LastEventIDDeclaredOnlyWhereAHandlerResumes(t *testing.T) {
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
	var declared []string
	for path, methods := range doc.Paths {
		for _, node := range methods {
			var op struct {
				Parameters []struct {
					Name string `yaml:"name"`
					In   string `yaml:"in"`
				} `yaml:"parameters"`
			}
			if node.Kind != yaml.MappingNode || node.Decode(&op) != nil {
				continue
			}
			for _, p := range op.Parameters {
				if strings.EqualFold(p.Name, "Last-Event-ID") {
					declared = append(declared, path)
				}
			}
		}
	}
	sort.Strings(declared)
	if len(declared) == 0 {
		t.Fatal("no operation declares Last-Event-ID; the spec parse found nothing to check")
	}
	for _, path := range declared {
		src, ok := streamResumeHandlers[path]
		if !ok {
			t.Errorf("%s declares Last-Event-ID but no handler is registered as resuming it", path)
			continue
		}
		code, err := os.ReadFile(src) //nolint:gosec // package-relative fixture path
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(code), "streaming.LastEventIDFrom(r)") {
			t.Errorf("%s declares Last-Event-ID but %s never calls streaming.LastEventIDFrom", path, src)
		}
	}
}
