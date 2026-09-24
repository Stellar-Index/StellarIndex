package chops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// RLT-430 class guard.
//
// warmCatalogueGates / applyGatedOptions give a re-derive the gate the live
// indexer runs with (curated set ∪ protocol_contracts).
// buildReconciliationCatalogue takes only a config, so on its own it can
// only build each gated decoder bare: a contract an operator admitted
// through protocol_contracts is decoded LIVE and invisible to an unwarmed
// re-derive. Its served rows then read as phantoms on /v1/coverage, they
// report as a delta that is not in the data on `verify-reconciliation` and
// `ch-reproject`, and a `ch-rebuild -write` rebuilds the table without them.
//
// Every non-test consumer of the catalogue must therefore warm it. This
// test enumerates them from the source tree rather than a hand-kept list,
// so a FIFTH consumer added tomorrow fails here instead of silently
// re-deriving on the bare seed. The per-call-site ORDER is pinned
// separately, below and in TestCHRebuild_WarmsTheCatalogueGatesBeforeAnythingReadsThem.
func TestRLT430_EveryCatalogueConsumerWarmsTheGates(t *testing.T) {
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

// TestCatalogueConsumers_WarmBeforeAnythingReadsTheDecoders pins the ORDER
// at the three call sites the class guard above can only see the presence
// of. Each of these commands needs ClickHouse and/or Postgres past the
// config load, so — like the BackfillSafe legs and the ch-rebuild leg —
// the wiring is pinned at the source: the warm runs on the freshly built
// catalogue, and it precedes the first reader of that catalogue's decoders
// (the factory preseed seeds INTO the decoders the warm rebuilds, so a
// warm after it would discard the seed; the recognition attribution reads
// src.contractIDs, which the warm widens with the registry-only children).
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
			firstUse: "evalSource := func(src reconSource) error {",
			why: "the EXPECTED side of /v1/coverage: an unwarmed re-derive expects none of " +
				"an operator-admitted contract's rows, so its real served rows publish as phantoms",
		},
		{
			file:     "verify_reconciliation.go",
			fn:       "verifyReconciliation",
			warm:     "warmCatalogueGates(ctx, store, slog.Default(), catalogue)",
			firstUse: "preseedFactoryChildren(",
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

// TestApplyGatedOptions_TestNetCatalogueHasNothingToWarm bounds the blast
// radius of the three new call sites onto the non-pubnet deployments.
//
// Every gated source is anchored to PUBNET contract identities (ADR-0035),
// so filterCatalogueByNetwork drops all of them on testnet / futurenet
// (#483). The warm is therefore a no-op there — and, decisively, its
// FAIL-CLOSED leg cannot fire: applyGatedOptions refuses a gated catalogue
// entry with no warmed options, and on a test net there is no such entry,
// so even an entirely empty options map is accepted. compute-completeness
// runs hourly on both test nets; a warm that could return an error on a
// catalogue with nothing to warm would have turned their coverage verdict
// red every tick.
func TestApplyGatedOptions_TestNetCatalogueHasNothingToWarm(t *testing.T) {
	t.Parallel()
	for _, network := range []string{"testnet", "futurenet"} {
		t.Run(network, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{}
			cfg.Stellar.Network = network
			cat, _, err := buildReconciliationCatalogue(cfg)
			if err != nil {
				t.Fatalf("buildReconciliationCatalogue(%s): %v", network, err)
			}
			if len(cat) == 0 {
				t.Fatalf("%s catalogue is empty — this test is asserting nothing", network)
			}
			for _, src := range cat {
				if _, gated := pipeline.GatedMetaFor(src.name); gated {
					t.Errorf("%s: catalogue holds gated source %q — it is pubnet-anchored and must be network-filtered out",
						network, src.name)
				}
			}
			// The empty map is the worst case for the fail-closed leg.
			warmed, err := applyGatedOptions(cat, map[string][]contractid.Option{})
			if err != nil {
				t.Fatalf("%s: applyGatedOptions on a catalogue with no gated source failed closed: %v", network, err)
			}
			if len(warmed) != len(cat) {
				t.Errorf("%s: catalogue length changed: %d → %d", network, len(cat), len(warmed))
			}
			for i := range cat {
				if warmed[i].name != cat[i].name {
					t.Errorf("%s: catalogue order changed at %d: %s → %s", network, i, cat[i].name, warmed[i].name)
				}
			}
		})
	}
}
