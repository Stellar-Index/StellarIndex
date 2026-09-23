package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestPairBoundPrices1mReadsAreClosed is the ADR-0015 companion to
// TestCAGGPairReadsFoldBothDirections: a pair-bound read of prices_1m must
// admit only CLOSED buckets. The in-progress minute is still filling, so a
// read that admits it serves a value no other region (and no later read)
// will reproduce — and on /v1/changes that value was ratcheted into the
// stored ath/atl for good (GH-757: `bucket < $4` with $4 = the worker's
// wall clock).
//
// Subjects are derived from the source (every string literal that reads
// prices_1m with a `base_asset = $n AND quote_asset = $m` filter), so a new
// reader is covered the day it lands. Each subject is named by its
// enclosing func or const.
func TestPairBoundPrices1mReadsAreClosed(t *testing.T) {
	t.Parallel()

	// Reads that deliberately admit the in-progress bucket, each with why.
	// An entry is a reviewed decision, not a way to silence the guard.
	openBucketExempt := map[string]string{
		"pairMarketQuery": "last_price is the pair's LAST TRADE, an observation rather than a " +
			"closed aggregate, and vol_24h_usd is a trailing activity sum; neither is an ADR-0015 rate",
		"TimedVWAPsForPair1m": "anomaly-baseline TRAINING input (baseline.Refresher, to = now), never " +
			"served; the open minute is one point among ~43k — tracked with the ADR-0015 chokepoint work",
		"VWAPsForPair1m": "anomaly-baseline TRAINING input, never served — same as TimedVWAPsForPair1m",
		"closedVWAP1mCombinedBeforeTemplate": "bucket < $4 (the candidate's own bucket start), not " +
			"bucket <= now() — the candidate is established closed by the guard's own caller " +
			"(GuardServedVWAP1mAt) before this trailing-baseline read is issued, so every bucket " +
			"strictly before it has necessarily closed too",
	}
	fromPrices1m := regexp.MustCompile(`FROM\s+prices_1m\b`)
	closedPredicate := regexp.MustCompile(`bucket\s*<=\s*[^\n]*-\s*INTERVAL\s*'1 minute'`)

	subjects := 0
	for _, s := range scanPairBoundLiterals(t, fromPrices1m) {
		subjects++
		if _, ok := openBucketExempt[s.owner]; ok {
			continue
		}
		if closedPredicate.MatchString(s.lit) {
			continue
		}
		t.Errorf("%s (%s) reads prices_1m for a pair without a closed-bucket predicate.\n"+
			"Admit a bucket only once it has ended, in the sargable form the package uses:\n"+
			"    AND bucket <= now() - INTERVAL '1 minute'\n"+
			"or, bounded by a caller's instant, `bucket <= $n::timestamptz - INTERVAL '1 minute'`.\n"+
			"If the open bucket is genuinely wanted, add %q to openBucketExempt with the reason.",
			s.pos, s.owner, s.owner)
	}
	if subjects == 0 {
		t.Fatal("no pair-bound prices_1m reads found — the scan has gone vacuous; fix it, do not delete it")
	}
}

type pairBoundLiteral struct {
	owner string
	pos   string
	lit   string
}

// scanPairBoundLiterals returns every string literal in the package's
// non-test sources that matches table and carries a pair-bound filter,
// attributed to its enclosing func or top-level const.
func scanPairBoundLiterals(t *testing.T, table *regexp.Regexp) []pairBoundLiteral {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var out []pairBoundLiteral
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, o := range ownedNodes(file) {
			owner := o.owner
			ast.Inspect(o.node, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil || !table.MatchString(s) || !pairBoundFilter.MatchString(s) {
					return true
				}
				out = append(out, pairBoundLiteral{owner: owner, pos: fset.Position(lit.Pos()).String(), lit: s})
				return true
			})
		}
	}
	return out
}

type ownedNode struct {
	owner string
	node  ast.Node
}

// ownedNodes splits a file into its funcs and its individual const/var
// specs, so a literal inside a `const ( … )` block is named by its own
// const rather than the block's first.
func ownedNodes(file *ast.File) []ownedNode {
	var out []ownedNode
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = append(out, ownedNode{owner: d.Name.Name, node: d})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
					out = append(out, ownedNode{owner: vs.Names[0].Name, node: vs})
				}
			}
		}
	}
	return out
}
