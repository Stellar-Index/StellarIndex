package confidence_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// R-003 (audit-2026-07-23, COR-14) parity guard.
//
// [Compute] combines the six factors as a NORMALISED weighted
// geometric mean — `prod(factor_i ^ weight_i) ^ (1 / sum(weights))`.
// ADR-0019's formula block writes the product WITHOUT the `^ (1 /
// sum(weights))` exponent, and this package's doc.go used to repeat
// that truncated form, so the two documents described a combiner the
// aggregator has never shipped — and, worse, one on which the
// documented freeze threshold (`confidence < 0.10`) means something
// materially different: as a bare product, ANY single-source window
// scores below 0.10 before z is even considered (source-count factor
// 0.119 × one-class diversity 0.5 = 0.06), collapsing the ADR's
// "three signals must agree" into two.
//
// These tests pin the prose to the shipped math. They are text
// assertions on purpose: the drift they exist to catch is a
// documentation drift, and nothing else in the build fails when an
// ADR quietly says something the code doesn't do.
//
// Whitespace (including line wrapping) is normalised before matching
// so re-flowing a paragraph doesn't fail the guard; the load-bearing
// tokens are the exponent and the weight sum.

const adr0019Path = "../../../docs/adr/0019-anomaly-response-and-confidence-scoring.md"

// squashWhitespace collapses every run of whitespace (spaces,
// newlines, markdown line-wrapping, Go comment `//` prefixes) to a
// single space so the matchers below survive re-flowing.
func squashWhitespace(s string) string {
	s = strings.ReplaceAll(s, "//", " ")
	return regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
}

func readSquashed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return squashWhitespace(string(b))
}

// TestADR0019PinsTheNormalisedCombiner — ADR-0019 must state the
// normalisation exponent over the SUM of the six weights. The
// original (immutable) formula block stays as written; the amendment
// blockquote at the top of the ADR carries the correction, per the
// docs/adr/README.md "supersede/amend, don't rewrite" rule.
func TestADR0019PinsTheNormalisedCombiner(t *testing.T) {
	adr := readSquashed(t, adr0019Path)

	// The normalising exponent, applied over the sum of all six
	// per-factor weights.
	wantExponent := regexp.MustCompile(
		`\^ \(1 / \(w_z \+ w_src \+ w_div \+ w_liq \+ w_xoracle \+ w_qual\)\)`)
	if !wantExponent.MatchString(adr) {
		t.Errorf("ADR-0019 does not spell the normalisation exponent "+
			"`^ (1 / (w_z + w_src + w_div + w_liq + w_xoracle + w_qual))` — "+
			"its formula block describes a bare product, which is NOT what "+
			"confidence.Compute ships (see %s)", adr0019Path)
	}

	// The generic restatement that ties the ADR to score.go's own
	// doc comment ("prod(factor_i ^ weight_i) ^ (1 / sum(weights))").
	if !strings.Contains(adr, "prod(factor_i ^ weight_i) ^ (1 / sum(weights))") {
		t.Error("ADR-0019 does not restate the shipped combiner as " +
			"`prod(factor_i ^ weight_i) ^ (1 / sum(weights))`")
	}

	// The freeze threshold's meaning on the normalised scale must be
	// stated, or `confidence < 0.10` reads as product units.
	if !strings.Contains(adr, "confidence < 0.10") {
		t.Error("ADR-0019 no longer mentions the `confidence < 0.10` freeze condition")
	}
}

// TestADR0019PinsTheShippedFactorSet — the ADR's "Factor shapes"
// bullet list is the only prose description of what actually goes into
// a published `confidence`, and two of the shipped shapes no longer
// match the original text: the liquidity ceiling moved $100K → $1M, and
// a seventh factor (triangulation agreement) exists that the ADR
// predates. Both are carried by the 2026-07-25 amendment; this guard
// fails if the amendment is dropped or the constants drift away from
// it, which is the same class of silent doc/code divergence R-003 was.
func TestADR0019PinsTheShippedFactorSet(t *testing.T) {
	adr := readSquashed(t, adr0019Path)

	// The ceiling constant, stated in the form an operator can match
	// against internal/aggregate/confidence/factors.go.
	if !strings.Contains(adr, "$100K → $1,000,000") {
		t.Error("ADR-0019 does not record the liquidity_factor ceiling change " +
			"($100K → $1,000,000); its factor-shape bullet still describes a " +
			"ceiling the code has not used since 2026-07-25")
	}
	// The measurement that justifies it — a number, not an adjective,
	// so a future re-tune has to argue with the evidence.
	if !strings.Contains(adr, "$123,678") {
		t.Error("ADR-0019 no longer carries the measured BTC/USD p50 bucket volume " +
			"that justifies the liquidity ceiling")
	}
	// The seventh factor and the two properties that make it safe.
	if !strings.Contains(adr, "triangulation_agreement_factor") {
		t.Error("ADR-0019 does not mention triangulation_agreement_factor — the " +
			"shipped combiner has a factor the ADR's list omits")
	}
	if !strings.Contains(adr, "Weight 0 when unchecked") {
		t.Error("ADR-0019 does not record that the triangulation factor is EXCLUDED " +
			"(weight 0), not merely neutral, when no composite exists — the property " +
			"that keeps every un-triangulated pair's score unchanged")
	}
	if !strings.Contains(adr, "does not feed `source_count`") &&
		!strings.Contains(adr, "does **not** feed `source_count`") {
		t.Error("ADR-0019 does not state that a composite is excluded from " +
			"source_count — the freeze AND's independence leg depends on it")
	}
}

// TestPackageDocPinsTheNormalisedCombiner — the package doc comment
// is the first thing a reader hits on pkg.go.dev; it carried the same
// truncated `prod(factor_i ^ weight_i)` formula as the ADR.
func TestPackageDocPinsTheNormalisedCombiner(t *testing.T) {
	doc := readSquashed(t, "doc.go")

	if !strings.Contains(doc, "prod(factor_i ^ weight_i) ^ (1 / sum(weights))") {
		t.Error("confidence/doc.go does not spell the normalised combiner " +
			"`prod(factor_i ^ weight_i) ^ (1 / sum(weights))` — a bare " +
			"product is not what Compute implements")
	}

	// Guard the specific mis-statement this finding was about: the
	// bare product must not be presented as THE combiner.
	bareProduct := regexp.MustCompile(`prod\(factor_i \^ weight_i\)\)? is the right`)
	if bareProduct.MatchString(doc) {
		t.Error("confidence/doc.go still presents the un-normalised product as the combiner")
	}
}
