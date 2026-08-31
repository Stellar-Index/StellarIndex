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
// internal/sources/sdex/decode.go sets base = soldAsset, quote =
// boughtAsset, and canonical.Trade deliberately does not normalise
// ("Direction matches the on-chain event — we do not normalise here").
// internal/storage/timescale/trades.go writes one unmirrored row per
// trade. So a market that trades both ways lands in prices_* as both
// (A,B) and (B,A) rows, and — as [dirVWAP]'s own doc says — "every
// serving read has to fold the two together itself".
//
// A read that binds a specific pair with `base_asset = $1 AND
// quote_asset = $2` and nothing else therefore sees a partial market.
// The failure is silent in the worst way: nothing in the response looks
// malformed, there is just less of it than there should be.
//
// This has now been the same bug three times:
//
//   - LatestClosedVWAP1mForPair (fixed, audit-2026-07-23 MNY-06)
//   - RecentClosedVWAP1mForPair — reported by the MNY-06 fixer as R-076,
//     recorded in that audit's remediation-state.json, and never
//     dispositioned. Fixed 2026-08-31, wave-D UNAUTH-DOS-9.
//   - ClosedVWAP1mAtOrBefore — the second reader UNAUTH-DOS-9 itself
//     missed; found by its skeptic. Fixed in the same change.
//
// Each previous fix came with a test pinning THAT function's query.
// None of them could see the next reader, which is why the class
// survived two remediations. This test is deliberately written against
// the class instead: it parses every string literal in the package and
// fails on any pair-bound CAGG read that filters one orientation,
// whether or not anyone remembered to write a test for it.

// pairBoundFilter matches a specific-pair filter: `base_asset = $N AND
// quote_asset = $M`, tolerant of whitespace and newlines between the
// two conjuncts.
var pairBoundFilter = regexp.MustCompile(
	`base_asset\s*=\s*\$(\d+)\s+AND\s+quote_asset\s*=\s*\$(\d+)`)

// caggTable matches the continuous aggregates that store per-direction
// rows. prices_1m is where every known instance of the bug lived; the
// coarser CAGGs have identical orientation semantics.
var caggTable = regexp.MustCompile(`FROM\s+prices_(1m|15m|1h|4h|1d|1w)\b`)

// directionExempt lists literals that bind a pair against a
// per-direction CAGG yet legitimately read ONE orientation. Keyed by a
// distinctive substring of the literal.
//
// Add an entry ONLY with the reason a single orientation is correct for
// that read — not to quiet the test. If the read serves a price, a
// volume, or a change to a caller, it is not exempt; fold the
// directions with [combineDirVWAP] like its siblings do.
var directionExempt = map[string]string{}

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
			if !caggTable.MatchString(s) || !pairBoundFilter.MatchString(s) {
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
		t.Fatal("no pair-bound CAGG reads found — this guard has gone " +
			"vacuous (queries moved or the literal shape changed); " +
			"fix the scan, do not delete the test")
	}
	t.Logf("scanned %d pair-bound CAGG read(s)", len(subjects))

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

The CAGG holds this market as both (base, quote) and (quote, base)
rows, so filtering `+"`base_asset = $%s AND quote_asset = $%s`"+` alone
drops every bucket in which the market traded only the other way.
Silent: the response looks well-formed, there is just less of it.

Fix it the way its siblings do — read both orientations and fold them
with combineDirVWAP via scanCombinedVwap1mRows:

    WHERE ((base_asset = $%s AND quote_asset = $%s)
        OR (base_asset = $%s AND quote_asset = $%s))

selecting base_asset and volume so the Go combine can weight and invert
the flipped leg exactly (ADR-0003 — the inversion must NOT happen in
SQL, where 1.0/vwap rounds before it is weighted).

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
