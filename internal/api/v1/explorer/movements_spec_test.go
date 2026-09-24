package explorer

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestMovementsSpecDescribesWatermarkSeam pins getAccountMovements'
// operation description to the handler's merge seam: the archive has a
// second writer (ch-cap67-movements, cap67_derived rows past P23) and
// the CH/PG boundary is its watermark. A description that still claims
// a single writer hard-clamped at P23 tells an integrator to attribute
// every post-P23 row to the Postgres tail.
func TestMovementsSpecDescribesWatermarkSeam(t *testing.T) {
	raw, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	op := doc.Paths["/accounts/{g_strkey}/movements"]["get"]
	if op.OperationID != "getAccountMovements" {
		t.Fatalf("GET /accounts/{g_strkey}/movements operationId = %q, want getAccountMovements", op.OperationID)
	}
	desc := strings.Join(strings.Fields(op.Description), " ")

	for _, want := range []string{
		"ch-cap67-movements",
		clickhouse.ProvenanceCAP67Derived,
		"classic-movements-backfill",
		"watermark",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("getAccountMovements description does not mention %q — the handler's seam is the cap67 derive's watermark", want)
		}
	}
	for _, stale := range []string{"only writer", "hard-clamps below the P23 boundary"} {
		if strings.Contains(desc, stale) {
			t.Errorf("getAccountMovements description still claims %q — the archive has two writers and a watermark boundary", stale)
		}
	}
}
