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

// The both-directions readers of prices_1m must express the two stored market orientations as a
// UNION ALL of two single-direction branches, never as one `(A AND B) OR (B AND A)`
// disjunction.
//
// Measured with prices_1m at 34 chunks, reading the empty pair native/fiat:USD:
//
//	OR form        10682.994 ms
//	UNION ALL form     3.610 ms
//
// A populated pair is unaffected (0.852 ms vs 1.279 ms), which hides the problem: every pair
// spot-checked by hand has rows.
//
// prices_1m carries `prices_1m_pair_bucket_idx` on (base_asset, quote_asset, bucket DESC), an
// exact cover for ONE direction. Postgres cannot drive a single index scan from an OR of two
// different (base, quote) equality pairs, so with `ORDER BY bucket DESC LIMIT n` it switches to
// the plain `bucket DESC` index and applies the pair test as a post-index FILTER. An empty pair
// can then only be proven by walking every chunk, because the LIMIT never fills. Splitting the
// directions lets each branch use the pair index, where an empty pair is an immediate miss.
//
// Folding in the second orientation was a genuine correctness fix (a bucket holding only the
// flipped leg went missing) and must stay; only its shape was wrong. This shape guard pins both
// properties so neither is traded for the other.
//
// It guards the template text rather than a plan so it needs no database and runs in the unit
// suite, where a planner regression would otherwise only surface as production latency.

// bothDirectionUnionQueries are the prices_1m readers that fold the two
// stored orientations and are driven by ORDER BY bucket DESC + LIMIT —
// the shape where the OR plan degrades to a full multi-chunk walk.
var bothDirectionUnionQueries = map[string]string{
	"closedVWAP1mAtOrBeforeQuery":     closedVWAP1mAtOrBeforeQuery,
	"recentClosedVWAP1mForPairQuery":  recentClosedVWAP1mForPairQuery,
	"recentClosedVWAP1mCombinedQuery": recentClosedVWAP1mCombinedTemplate,
}

func TestBothDirectionReadersUseUnionNotOr(t *testing.T) {
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
			// Each branch must carry its own ORDER BY + LIMIT, or the branches materialise in
			// full before the outer sort and the walk comes back.
			// The outer sort needs a tiebreaker: `bucket DESC` alone is not a total order once
			// a bucket holds both directions, and the branches interleave differently than the
			// OR plan did. [combineDirVWAP] is commutative so the served number is unaffected,
			// but ADR-0015's byte-identical cross-region property should not rest on that;
			// `base_asset` makes the order total.
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

// ── Package scan ────────────────────────────────────────────────────
//
// bothDirectionUnionQueries above is a hand-maintained list, and a guard whose coverage is a
// list only guards what someone remembered to type: readers with the forbidden shape
// (HistoryPoints, HistoryPointsInRange, TWAPPointsInRange, OHLCSeries, OHLCSeriesReBucketed)
// shipped green because they were never added to it.
//
// So the invariant below is not given subjects: it scans aggregates.go (the pair-bound CAGG
// reads), recovers each declaration's SQL from its string literals, and picks subjects by
// shape. A new reader added to that file is covered the moment it is written.
//
// Subject rule: a query that folds both stored orientations (`base_asset = $1 AND quote_asset =
// $2` together with the flipped spelling) and emits bucket-ordered output. That pairing is the
// pathological plan: ordered by bucket, the planner abandons prices_*_pair_bucket_idx and
// applies the pair test as a post-index FILTER, so proving an empty pair empty walks every
// chunk (10682.994 ms vs 3.610 ms above).
//
// Unordered reads (point reads, the LIMIT 1 existence gate, the substance sums) are not
// subjects of this test's ordering checks; the OR fold itself is banned for every read in every
// file by TestPairReadsNeverFoldDirectionsWithOr.

// bucketOrderedRe matches an ORDER BY on a bucket column (`bucket`,
// `n.bucket`, `out_bucket`).
var bucketOrderedRe = regexp.MustCompile(`ORDER BY\s+[a-z_.]*bucket`)

// nonSargableBucketRe matches the forbidden closed-bucket spelling: an INTERVAL added to the
// indexed bucket column (or an expression over it) on the left of the comparison, which the
// planner cannot use for index access or chunk pruning. The sargable form puts the interval on
// the right: `bucket <= now() - INTERVAL '…'`.
var nonSargableBucketRe = regexp.MustCompile(`bucket[^<>=\n]*\+\s*INTERVAL\s*'[^']*'\s*<`)

// declSQL returns the SQL text of every top-level declaration in `file`, keyed by declaration
// name. The text concatenates all its string literals, so a query assembled from fragments (a
// Sprintf template plus optional clauses) is scanned as one statement rather than slipping
// through as innocent pieces.
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

// pairDeclFiles lists every non-test .go file in the package, the same set
// TestPairReadsNeverFoldDirectionsWithOr and TestBatchPairReadsFoldBothDirections scan. A scan
// hardcoded to one file goes silently blind the moment a both-directions reader lands
// elsewhere.
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

// TestCAGGSeriesReadsFoldDirectionsWithUnion is the list-free half of
// TestBothDirectionReadersUseUnionNotOr: every bucket-ordered both-directions read in the
// package must split the orientations into two index-drivable UNION ALL branches. Subjects are
// selected by shape (a pair-bound predicate in both operand orders), not one fixed spelling, so
// swapping operands or breaking the OR across lines cannot hide a reader.
func TestCAGGSeriesReadsFoldDirectionsWithUnion(t *testing.T) {
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
