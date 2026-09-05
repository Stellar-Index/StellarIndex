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
// fails on any pair-bound per-direction read that filters one
// orientation, whether or not anyone remembered to write a test for it.
//
// It covers `trades` as well as the CAGGs. The raw hypertable is where
// the same market's two spellings originate, and the aggregate readers
// over it repeated the class a fourth time — Store.TradesInRange, which
// fed a mean and two extremes on five serving surfaces (launch-plan row
// 1.16).

// pairBoundFilter matches a specific-pair filter: `base_asset = $N AND
// quote_asset = $M`, tolerant of whitespace and newlines between the
// two conjuncts.
var pairBoundFilter = regexp.MustCompile(
	`base_asset\s*=\s*\$(\d+)\s+AND\s+quote_asset\s*=\s*\$(\d+)`)

// directionedTable matches the tables that store per-direction rows.
// prices_1m is where every known instance of the bug lived; the coarser
// CAGGs have identical orientation semantics, and `trades` is the
// hypertable they are all built from — the same market, the same two
// spellings, one layer down.
//
// The scope widened to `trades` with the aggregate fold
// ([Store.TradesInRange], launch-plan row 1.16). It could not widen
// before it: the extension goes red on any read that is deliberately
// single-orientation, and the only way to green those was an entry
// asserting a known-blind read is correct — which is what
// [directionExempt] refuses.
var directionedTable = regexp.MustCompile(`FROM\s+(prices_(1m|15m|1h|4h|1d|1w)|trades)\b`)

// directionExempt lists literals that bind a pair against a
// per-direction table yet legitimately read ONE orientation. Keyed by a
// distinctive substring of the literal.
//
// Add an entry ONLY with the reason a single orientation is correct for
// that read — not to quiet the test. If the read serves a price, a
// volume, or a change to a caller, it is not exempt; fold the
// directions with [combineDirVWAP] on the CAGG side, or with
// [orientTradeTo] on the raw side, like its siblings do.
//
// There is exactly one, and it is not "this read is blind". A cursor is
// the one thing a two-armed union cannot carry: a keyset resumes a page
// inside ONE ordering, and the two directions have to be merged and
// resumed together or a tie group is cut through. /v1/history therefore
// merges them in its caller — [tradesInRangeAfterBothDirections] in
// internal/api/v1 — and this read stays honest about serving one arm of
// that merge rather than pretending to be whole.
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

Fix it the way its siblings do — read both orientations and fold them
per ROW, on the row's own base_asset:

    WHERE ((base_asset = $%s AND quote_asset = $%s)
        OR (base_asset = $%s AND quote_asset = $%s))

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
