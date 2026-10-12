package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The CAGGs hold the same market in BOTH stored orientations, so a read binding one `base_asset = $1 AND
// quote_asset = $2` pair silently sees a partial market. This test parses every string literal in the package
// and fails any single-orientation pair read, including `trades` readers.

// pairBoundFilter matches a specific-pair filter: `base_asset = $N AND
// quote_asset = $M`, tolerant of whitespace and newlines between the
// two conjuncts.
var pairBoundFilter = regexp.MustCompile(
	`base_asset\s*=\s*\$(\d+)\s+AND\s+quote_asset\s*=\s*\$(\d+)`)

// directionedTable matches tables storing per-direction rows: prices_1m, the coarser CAGGs, and `trades`.
// Covering `trades` forces aggregate reads to fold both directions; a single-orientation read goes red
// and cannot be greened by an exempt entry, which directionExempt refuses.
var directionedTable = regexp.MustCompile(`FROM\s+(prices_(1m|15m|1h|4h|1d|1w)|trades)\b`)

// directionExempt lists literals that bind a pair yet legitimately read ONE orientation; add one only with
// the reason a single orientation is correct. Served prices must fold both directions. The one entry is a
// keyset cursor, which cannot resume across a union; its caller merges the arms.
var directionExempt = map[string]string{
	"(ts, ledger, tx_hash, op_index, source) > ($5, $6, $7, $8, $9)": "folded by its CALLER, not blind: " +
		"/v1/history reads this primitive once per direction and merges the two " +
		"under one keyset cursor (internal/api/v1.tradesInRangeAfterBothDirections). " +
		"A union cannot do it here — a cursor names a position in ONE ordering, and " +
		"resuming two directions independently cuts through a tie group.",
}

func TestCAGGPairReadsFoldBothDirections(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	type subject struct {
		file string
		line int
		lit  string
	}
	var subjects []subject

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
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// Raw backtick strings hold the SQL; Unquote handles both
			// forms and simply fails on anything exotic, which cannot
			// be SQL we care about.
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !directionedTable.MatchString(s) || !pairBoundFilter.MatchString(s) {
				return true
			}
			subjects = append(subjects, subject{
				file: f,
				line: fset.Position(lit.Pos()).Line,
				lit:  s,
			})
			return true
		})
	}

	// A guard is worth only what it checks: if the scan finds nothing, the queries moved and this test is
	// silently vacuous.
	if len(subjects) == 0 {
		t.Fatal("no pair-bound per-direction reads found — this guard has gone " +
			"vacuous (queries moved or the literal shape changed); " +
			"fix the scan, do not delete the test")
	}
	t.Logf("scanned %d pair-bound per-direction read(s)", len(subjects))

	for _, sub := range subjects {
		if reason, ok := exemptReason(sub.lit); ok {
			t.Logf("%s:%d exempt — %s", sub.file, sub.line, reason)
			continue
		}
		m := pairBoundFilter.FindStringSubmatch(sub.lit)
		base, quote := m[1], m[2] // e.g. "1", "2"

		// The flipped conjunct, in either spelling the package uses.
		flipped := regexp.MustCompile(
			`base_asset\s*=\s*\$` + quote + `\s+AND\s+quote_asset\s*=\s*\$` + base)
		if flipped.MatchString(sub.lit) {
			continue
		}
		t.Errorf(`%s:%d reads ONE stored market direction.

This table holds the market as both (base, quote) and (quote, base)
rows, so filtering `+"`base_asset = $%s AND quote_asset = $%s`"+` alone
drops every row in which the market traded only the other way.
Silent: the response looks well-formed, there is just less of it.

Fix it the way its siblings do — read both orientations, one UNION ALL
arm each (never one OR; see TestPairReadsNeverFoldDirectionsWithOr),
and fold them per ROW, on the row's own base_asset:

    WHERE base_asset = $%s AND quote_asset = $%s
    UNION ALL
    … WHERE base_asset = $%s AND quote_asset = $%s

On a CAGG that is combineDirVWAP via scanCombinedVwap1mRows, selecting
base_asset and volume so the Go combine can weight and invert the
flipped leg exactly. On the trades hypertable it is two limited arms
unioned and scanTradeOriented, which swaps the two legs and their two
integer amounts. Either way the inversion must NOT happen in SQL, where
1.0/vwap rounds before it is weighted (ADR-0003).

If one direction really is correct here, add the literal to
directionExempt with the reason.

Query:
%s`, sub.file, sub.line, base, quote, base, quote, quote, base, indent(sub.lit))
	}
}

func exemptReason(lit string) (string, bool) {
	keys := make([]string, 0, len(directionExempt))
	for k := range directionExempt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(lit, k) {
			return directionExempt[k], true
		}
	}
	return "", false
}

func indent(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "    " + strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

// pairKeySetFilter matches a batch read that selects pairs by their
// stored `base|quote` key text, which names ONE orientation per key.
var pairKeySetFilter = regexp.MustCompile(
	`\(\s*(?:\w+\.)?base_asset\s*\|\|\s*'\|'\s*\|\|\s*(?:\w+\.)?quote_asset\s*\)\s*=\s*ANY`)

// TestBatchPairReadsFoldBothDirections is the set-keyed twin of TestCAGGPairReadsFoldBothDirections: an `=
// ANY($1)` pair-key match misses the reverse orientation, so each pair must match both ways via UNION ALL.
func TestBatchPairReadsFoldBothDirections(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for name, q := range declSQL(t, f) {
			if directionedTable.MatchString(q) && pairKeySetFilter.MatchString(q) {
				t.Errorf("%s: %s selects %s rows by their stored `base|quote` key, "+
					"which reads ONE orientation of each requested pair; a market "+
					"stored only the other way round reads as empty. Join the "+
					"request set once per direction and UNION ALL the arms.",
					f, name, directionedTable.FindString(q))
			}
		}
	}
}

// orientationDisjunction matches a pair read that folds the two stored
// orientations with one OR, in either conjunct order, bound or joined:
// `(base_asset = $1 AND quote_asset = $2) OR (base_asset = $2 AND …)`.
var orientationDisjunction = regexp.MustCompile(
	`(?i)\bOR\s*\(\s*(?:\w+\.)?(?:base_asset\s*=\s*\S+\s+AND\s+(?:\w+\.)?quote_asset|` +
		`quote_asset\s*=\s*\S+\s+AND\s+(?:\w+\.)?base_asset)\s*=`)

// TestPairReadsNeverFoldDirectionsWithOr is the package-wide half of TestBothDirectionReadersUseUnionNotOr.
// Composite pair indexes cover one (base, quote) equality, so an OR of two scans slowly (10683 ms vs 3.6
// ms); fold directions as two single-direction UNION ALL arms.
func TestPairReadsNeverFoldDirectionsWithOr(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for name, q := range declSQL(t, f) {
			// Not directionedTable: several readers interpolate the CAGG
			// name (`FROM prices_%[1]s`), and the disjunction is specific
			// enough to need no table gate.
			if !strings.Contains(q, "FROM") {
				continue
			}
			scanned++
			if m := orientationDisjunction.FindString(q); m != "" {
				t.Errorf("%s: %s folds both stored orientations with an OR (%q).\n\n"+
					"Read each direction in its own arm — `base_asset = $1 AND "+
					"quote_asset = $2` UNION ALL `base_asset = $2 AND quote_asset = $1` "+
					"— so each arm is one index equality and an empty pair is an "+
					"immediate miss.", f, name, m)
			}
			if m := nonSargableBucketRe.FindString(q); m != "" {
				t.Errorf("%s: %s applies a function to the indexed bucket column (%q). "+
					"Put the interval on the RHS: `bucket <= now() - INTERVAL …`", f, name, m)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no declaration holds a FROM clause — the scan has gone vacuous")
	}
}
