package v1_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func envelopeStale(t *testing.T, url string) bool {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	// Non-vacuity: the completeness summary must actually be served, so
	// flags.stale is qualifying a republished verdict.
	if !strings.Contains(string(env.Data), `"completeness":{"complete":true`) {
		t.Fatalf("GET %s served no completeness summary; flags.stale would be meaningless", url)
	}
	return env.Flags.Stale
}

// TestProtocols_StaleWhenRepublishedVerdictIsOld pins #1296: /v1/protocols
// and /v1/protocols/{name} republish the completeness_snapshots rows
// /v1/coverage gates, so a verdict 30h old must raise flags.stale on all
// three — not only on /v1/coverage.
func TestProtocols_StaleWhenRepublishedVerdictIsOld(t *testing.T) {
	snaps := []timescale.CompletenessSnapshot{{
		Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 63_000_000,
		CoveragePct: 1, Complete: true, LakeComplete: true,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
		ComputedAt: time.Now().UTC().Add(-30 * time.Hour),
	}}
	old := httpTestServer(t, v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}}))
	for _, path := range []string{"/v1/protocols", "/v1/protocols/blend"} {
		if !envelopeStale(t, old.URL+path) {
			t.Errorf("GET %s: flags.stale = false for a 30h-old verdict, want true "+
				"(/v1/coverage flags the same row stale)", path)
		}
	}
	if !coverageStaleFlag(t, old.URL) {
		t.Fatal("/v1/coverage flags.stale = false for the same row — the gate itself regressed")
	}

	snaps[0].ComputedAt = time.Now().UTC().Add(-5 * time.Minute)
	fresh := httpTestServer(t, v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}}))
	for _, path := range []string{"/v1/protocols", "/v1/protocols/blend"} {
		if envelopeStale(t, fresh.URL+path) {
			t.Errorf("GET %s: flags.stale = true for a 5-minute-old verdict", path)
		}
	}
}

// TestCompletenessVerdictReadsGoThroughGate is the class guard for #1296:
// outside diagnostics (which republishes computed_at per row, so each
// claim carries its own age), the only production read of the verdict
// rows is completenessVerdicts, which returns them with their stale gate.
func TestCompletenessVerdictReadsGoThroughGate(t *testing.T) {
	allowed := map[string]bool{
		"coverage_verdicts.go:completenessVerdicts":    true,
		"diagnostics_ingestion.go:overlayCompleteness": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "ListCompletenessSnapshots" && !allowed[f+":"+fn.Name.Name] {
					t.Errorf("%s: %s reads completeness verdicts directly; use completenessVerdicts so "+
						"the rows travel with their stale gate", fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}
