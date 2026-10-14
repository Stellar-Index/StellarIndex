package chops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Class guard: every catalogue consumer must warm the gates (warmCatalogueGates: curated set plus protocol_contracts).
// Unwarmed, a protocol_contracts contract decodes live but not in re-derive, so served rows read as phantoms on
// /v1/coverage.
func TestCatalogueConsumers_WarmTheGates(t *testing.T) {
	t.Parallel()
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

// TestCatalogueConsumers_WarmBeforeAnythingReadsTheDecoders pins the ORDER at the three call sites: the warm runs on the
// freshly built catalogue and precedes the first decoder reader. The factory preseed seeds INTO the decoders the warm
// rebuilds, so a later warm discards the seed, and recognition attribution reads src.contractIDs, which the warm widens.
func TestCatalogueConsumers_WarmBeforeAnythingReadsTheDecoders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file     string
		fn       string
		warm     string
		firstUse string // the first statement that reads the catalogue's decoders
		why      string
	}{
		{
			file:     "compute_completeness.go",
			fn:       "computeCompleteness",
			warm:     "applyGatedOptions(catalogue, gatedOpts)",
			firstUse: "evalSource := func(ctx context.Context, src reconSource) error {",
			why: "the EXPECTED side of /v1/coverage: an unwarmed re-derive expects none of " +
				"an operator-admitted contract's rows, so its real served rows publish as phantoms",
		},
		{
			file:     "verify_reconciliation.go",
			fn:       "verifyReconciliation",
			warm:     "warmCatalogueGates(ctx, store, slog.Default(), catalogue)",
			firstUse: "verifyReconExpected(",
			why:      "same comparison, operator-invoked: it would report a mismatch that is not in the data",
		},
		{
			file:     "ch_reproject.go",
			fn:       "chReproject",
			warm:     "warmCatalogueGates(ctx, store, slog.Default(), cat)",
			firstUse: "preseedFactoryChildren(",
			why: "the report that answers \"what would rebuilding Postgres from ClickHouse change?\" — " +
				"it would show a CH-under-served delta that is not in the data, and invite a `ch-rebuild -write`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(tc.file) //nolint:gosec // package-relative, test-only
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			body := funcBodyFrom(string(b), tc.fn)
			if body == "" {
				t.Fatalf("%s not found — this test is asserting nothing", tc.fn)
			}
			catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
			firstUse := strings.Index(body, tc.firstUse)
			if catalogue < 0 || firstUse < 0 {
				t.Fatalf("anchors not found (catalogue %d, first use %d) — this test is asserting nothing",
					catalogue, firstUse)
			}
			warm := strings.Index(body, tc.warm)
			switch {
			case warm < 0:
				t.Errorf("%s never warms the catalogue's gated decoders from protocol_contracts: %s (RLT-430)", tc.fn, tc.why)
			case warm < catalogue:
				t.Errorf("%s: the gate warm runs BEFORE the catalogue is built", tc.fn)
			case warm > firstUse:
				t.Errorf("%s: the gate warm runs AFTER %q, the first reader of the catalogue's decoders — "+
					"it rebuilds them, so anything that read or seeded the old instances is discarded", tc.fn, tc.firstUse)
			}
		})
	}
}
