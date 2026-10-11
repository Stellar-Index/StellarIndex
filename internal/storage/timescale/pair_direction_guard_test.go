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

// The CAGGs hold the same market in BOTH stored orientations.
//
// sdex/decode.go sets base = soldAsset, quote = boughtAsset, and canonical.Trade deliberately
// does not normalise direction. trades.go writes one unmirrored row per trade, so a market that
// trades both ways lands in prices_* as both (A,B) and (B,A) rows, and, as [dirVWAP]'s doc says,
// "every serving read has to fold the two together itself".
//
// A read that binds a pair with `base_asset = $1 AND quote_asset = $2` and nothing else sees a
// partial market, silently: nothing in the response looks malformed, there is just less of it.
//
// A test pinning one function's query cannot see the next reader, so this test is written
// against the class: it parses every string literal in the package and fails on any pair-bound
// per-direction read that filters one orientation, whether or not anyone wrote a test for it.
//
// It covers `trades` as well as the CAGGs, since the raw hypertable is where the two spellings
// originate and aggregate readers over it (Store.TradesInRange) repeated the bug.

// pairBoundFilter matches a specific-pair filter: `base_asset = $N AND
// quote_asset = $M`, tolerant of whitespace and newlines between the
// two conjuncts.
var pairBoundFilter = regexp.MustCompile(
	`base_asset\s*=\s*\$(\d+)\s+AND\s+quote_asset\s*=\s*\$(\d+)`)

// directionedTable matches the tables that store per-direction rows. prices_1m is where the
// bug lived; the coarser CAGGs have identical orientation semantics, and `trades` is the
// hypertable they are built from, with the same two spellings one layer down.
//
// Covering `trades` requires the aggregate reads to fold both directions first: the check goes
// red on any deliberately single-orientation read, and greening one would need an exempt entry
// asserting a known-blind read is correct, which [directionExempt] refuses.
var directionedTable = regexp.MustCompile(`FROM\s+(prices_(1m|15m|1h|4h|1d|1w)|trades)\b`)

// directionExempt lists literals that bind a pair against a per-direction table yet
// legitimately read ONE orientation, keyed by a distinctive substring of the literal.
//
// Add an entry only with the reason a single orientation is correct for that read, never to
// quiet the test. A read that serves a price, volume or change to a caller is not exempt; fold
// the directions with [combineDirVWAP] on the CAGG side or [orientTradeTo] on the raw side.
//
// The one entry is not "this read is blind": a keyset cursor resumes a page inside ONE ordering
// and a two-armed union cannot carry it, so the directions must be merged and resumed together
// or a tie group is cut through. /v1/history merges them in its caller
// ([tradesInRangeAfterBothDirections] in internal/api/v1), and this read stays honest about
// serving one arm of that merge.
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

	// A guard is worth only what it checks. If the scan finds nothing,
	// the queries moved (to a builder, another package, a generator) and
	// this test is silently vacuous — which is precisely how the class
	// survived two prior remediations.
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

// TestBatchPairReadsFoldBothDirections is the set-keyed twin of
// TestCAGGPairReadsFoldBothDirections, which sees only `$N`-bound pairs. Selecting prices_1m
// rows by `(base_asset || '|' || quote_asset) = ANY($1)` with canonical keys matches nothing
// for a market stored the other way round (zero sparkline bars beside a non-zero
// volume_24h_usd), so each requested pair must match in both orientations (a join per
// direction, UNION ALL'd).
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

// TestPairReadsNeverFoldDirectionsWithOr is the package-wide, list-free half of
// TestBothDirectionReadersUseUnionNotOr: every declaration in every non-test file, inline
// Sprintf builders included. The composite pair indexes cover ONE (base, quote) equality, so a
// disjunction of two is answered by a BitmapOr at best or by a bucket-index walk that filters
// the pair afterwards (10683 ms vs 3.6 ms for an empty pair). Fold the directions as two
// single-direction UNION ALL arms instead.
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
