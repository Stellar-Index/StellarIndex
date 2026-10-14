package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Both-directions prices_1m readers must use UNION ALL of single-direction branches, never an OR of two
// pairs. An OR defeats the pair index, so an empty pair walks every chunk (10683 ms vs 3.6 ms measured).
// Guards the template text, so it needs no database; the flipped-leg fold must stay.

// bothDirectionUnionQueries are the prices_1m readers that fold the two
// stored orientations and are driven by ORDER BY bucket DESC + LIMIT —
// the shape where the OR plan degrades to a full multi-chunk walk.
var bothDirectionUnionQueries = map[string]string{
	"closedVWAP1mAtOrBeforeQuery":     closedVWAP1mAtOrBeforeQuery,
	"recentClosedVWAP1mForPairQuery":  recentClosedVWAP1mForPairQuery,
	"recentClosedVWAP1mCombinedQuery": recentClosedVWAP1mCombinedTemplate,
}

func checkBothDirectionReadersUseUnionNotOr(t *testing.T) {
	for name, q := range bothDirectionUnionQueries {
		t.Run(name, func(t *testing.T) {
			// The disjunction is the defect. Its absence is the guard.
			if strings.Contains(q, "OR (base_asset") {
				t.Errorf("%s folds both directions with an OR disjunction. The "+
					"planner cannot drive prices_1m_pair_bucket_idx from an OR of two "+
					"(base,quote) pairs, so ORDER BY bucket DESC LIMIT n degrades to a "+
					"full walk of every chunk — 10683 ms vs 3.6 ms measured on r1 for a "+
					"pair with no rows. Use a UNION ALL of two single-direction "+
					"branches.\n\nquery:\n%s", name, q)
			}
			if !strings.Contains(q, "UNION ALL") {
				t.Errorf("%s does not use UNION ALL — the two stored orientations "+
					"must each be their own indexable branch", name)
			}
			// Correctness half: BOTH orientations must still be read. A
			// UNION ALL that dropped a leg would be fast and wrong, which is
			// exactly the trade this guard exists to prevent.
			if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") {
				t.Errorf("%s lost the forward direction", name)
			}
			if !strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
				t.Errorf("%s lost the flipped direction (wave-D UNAUTH-DOS-9)", name)
			}
			// Each branch needs its own ORDER BY + LIMIT, or the branches materialise fully before the outer sort.
			// The outer sort needs a tiebreaker (`base_asset`) so the order is total and cross-region output is
			// byte-identical (ADR-0015), rather than relying on combineDirVWAP being commutative.
			if !strings.Contains(q, "ORDER BY bucket DESC, base_asset") {
				t.Errorf("%s outer sort lacks the base_asset tiebreaker — with both "+
					"directions in one bucket the row order would be planner-defined, "+
					"which ADR-0015 byte-identical cross-region serving cannot rely on",
					name)
			}
			if strings.Count(q, "ORDER BY bucket DESC") < 3 {
				t.Errorf("%s: expected an ORDER BY bucket DESC in each branch AND on "+
					"the outer query (3 total), found %d — an unbounded branch "+
					"re-introduces the full scan this guard prevents",
					name, strings.Count(q, "ORDER BY bucket DESC"))
			}
		})
	}
}

// Package scan: instead of a hand-maintained list, scan aggregates.go and pick subjects by shape, so a
// new reader is covered when written. A subject folds both orientations and emits bucket-ordered output;
// that plan skips prices_*_pair_bucket_idx and filters post-index. The OR fold is banned elsewhere.

// bucketOrderedRe matches an ORDER BY on a bucket column (`bucket`,
// `n.bucket`, `out_bucket`).
var bucketOrderedRe = regexp.MustCompile(`ORDER BY\s+[a-z_.]*bucket`)

// nonSargableBucketRe matches the forbidden closed-bucket spelling, an INTERVAL added to the indexed bucket
// column on the left of the comparison, which blocks index access and chunk pruning. The sargable form is
// `bucket <= now() - INTERVAL '...'`.
var nonSargableBucketRe = regexp.MustCompile(`bucket[^<>=\n]*\+\s*INTERVAL\s*'[^']*'\s*<`)

// declSQL returns the SQL text of each top-level declaration in file, keyed by name, concatenating all its
// string literals so a query assembled from fragments is scanned as one statement.
func declSQL(t *testing.T, file string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := make(map[string]string, len(f.Decls))
	for _, d := range f.Decls {
		var lits []string
		ast.Inspect(d, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			lits = append(lits, s)
			return true
		})
		if len(lits) == 0 {
			continue
		}
		out[declName(d)] = strings.Join(lits, "\n")
	}
	return out
}

// declName labels a declaration for test output: `Store.HistoryPoints`
// for a method, the function name for a func, the first name for a
// const/var block.
func declName(d ast.Decl) string {
	switch decl := d.(type) {
	case *ast.FuncDecl:
		name := decl.Name.Name
		if decl.Recv != nil && len(decl.Recv.List) > 0 {
			typ := decl.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				name = id.Name + "." + name
			}
		}
		return name
	case *ast.GenDecl:
		for _, spec := range decl.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return vs.Names[0].Name
			}
		}
	}
	return "?"
}

// pairDeclFiles lists every non-test .go file in the package, the set the pair-direction guard tests scan; a
// scan hardcoded to one file goes blind once a both-directions reader lands elsewhere.
func pairDeclFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := files[:0]
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

// checkCAGGSeriesReadsFoldDirectionsWithUnion is the list-free half of checkBothDirectionReadersUseUnionNotOr:
// every bucket-ordered both-directions read must split orientations into two index-drivable UNION ALL
// branches. Subjects are chosen by shape, so swapped operands or a multi-line OR cannot hide one.
func checkCAGGSeriesReadsFoldDirectionsWithUnion(t *testing.T) {
	subjects := 0
	for _, file := range pairDeclFiles(t) {
		for name, q := range declSQL(t, file) {
			m := pairBoundFilter.FindStringSubmatch(q)
			if m == nil {
				continue
			}
			base, quote := m[1], m[2]
			flipped := regexp.MustCompile(
				`base_asset\s*=\s*\$` + quote + `\s+AND\s+quote_asset\s*=\s*\$` + base)
			if !flipped.MatchString(q) {
				continue
			}
			if !bucketOrderedRe.MatchString(q) {
				continue
			}
			subjects++
			t.Run(file+"/"+name, func(t *testing.T) {
				if m := orientationDisjunction.FindString(q); m != "" {
					t.Errorf("%s folds both directions with an OR disjunction (%q). The "+
						"planner cannot drive prices_*_pair_bucket_idx from an OR of two "+
						"(base,quote) pairs, so a bucket-ordered read degrades to a full "+
						"walk of every chunk — 10683 ms vs 3.6 ms measured on r1 for a "+
						"pair with no rows. Use a UNION ALL of two single-direction "+
						"branches.\n\nquery:\n%s", name, m, q)
				}
				if !strings.Contains(q, "UNION ALL") {
					t.Errorf("%s does not use UNION ALL — the two stored orientations "+
						"must each be their own indexable branch", name)
				}
			})
		}
	}
	// Non-vacuity: the scan must not silently cover nothing, and the floor must track the
	// real subject count (13 readers across aggregates.go, change_summary.go, markets.go); a
	// drop below it is the scan going blind, not the count settling lower by design.
	if subjects < 13 {
		t.Errorf("scan found %d both-directions bucket-ordered readers in "+
			"the package, expected at least 13 — the scan is no longer "+
			"matching the readers it exists to guard", subjects)
	}
}
