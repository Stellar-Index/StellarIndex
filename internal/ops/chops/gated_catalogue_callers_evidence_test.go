//go:build rlt430evidence

package chops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// RLT-430, the legs that are still open.
//
// warmCatalogueGates / applyGatedOptions give a re-derive the gate the live
// indexer runs with (curated set ∪ protocol_contracts). ch-rebuild calls it;
// TestCHRebuild_WarmsTheCatalogueGatesBeforeAnythingReadsThem guards that in
// the default suite. The three OTHER consumers of the catalogue still build
// it bare, and each sits in a file outside the unit that landed the seam:
//
//   - compute_completeness.go — the EXPECTED side of /v1/coverage. This is
//     the phantom-rows leg of the finding: a registry-only contract's served
//     rows have no expected counterpart. It already holds gatedOpts (for the
//     recognition scan), so the change is applyGatedOptions(catalogue,
//     gatedOpts) — but the GatedRegistryOptions call has to move ABOVE the
//     first use of the catalogue's decoders.
//   - verify_reconciliation.go — same comparison, operator-invoked.
//   - ch_reproject.go — rewrites projections from the lake; same data-loss
//     shape as ch-rebuild -write.
//
// Build-tagged because it is RED until those three land:
//
//	go test -tags rlt430evidence ./internal/ops/chops/ -run TestRLT430 -v
//
// When it is green, drop the tag: it is then the class guard — any new
// consumer of buildReconciliationCatalogue must warm the gates or fail here.
func TestRLT430_EveryCatalogueConsumerWarmsTheGates(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)
	consumers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "reconciliation_catalogue.go" {
			continue
		}
		b, rerr := os.ReadFile(f) //nolint:gosec // package-relative, test-only
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		src := string(b)
		if !strings.Contains(src, "buildReconciliationCatalogue(") {
			continue
		}
		consumers++
		if !strings.Contains(src, "warmCatalogueGates(") && !strings.Contains(src, "applyGatedOptions(") {
			t.Errorf("%s builds the reconciliation catalogue and never warms its gated decoders — "+
				"it re-derives on the bare in-code seed, so a contract admitted through "+
				"protocol_contracts is invisible to it (RLT-430)", f)
		}
	}
	if consumers == 0 {
		t.Fatal("no consumer of buildReconciliationCatalogue found — this test is asserting nothing")
	}
}
