package confidence_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
)

// Doc/code parity for the bootstrap density gate stated in the
// oracle-manipulation-defense page.

const (
	// The formulas, factor constants and bootstrap gate live here
	// (#confidence-score); ADR-0019 links to it for them.
	adr0019DetailPath = "../../../docs/architecture/oracle-manipulation-defense.md"
)

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

// TestADR0019PinsTheBootstrapDensityGate — the ADR's warmup rule says
// "< 30 days of history", but the cap gates on bucket density at
// [confidence.BootstrapDensityDays], re-engaging below
// [confidence.BootstrapReengageDensityDays]. An ADR amendment
// records both; this fails if it is dropped or a constant moves away from it.
func TestADR0019PinsTheBootstrapDensityGate(t *testing.T) {
	adr := readSquashed(t, adr0019DetailPath)
	gate := strconv.FormatFloat(confidence.BootstrapDensityDays, 'f', -1, 64)
	if !strings.Contains(adr, "`BootstrapDensityDays` = "+gate) {
		t.Errorf("ADR-0019 does not state the shipped bootstrap density gate (%s days-equivalent)", gate)
	}
	reengage := strconv.FormatFloat(confidence.BootstrapReengageDensityDays, 'f', -1, 64)
	if !strings.Contains(adr, "`BootstrapReengageDensityDays` = "+reengage) {
		t.Errorf("ADR-0019 does not state the bootstrap gate's hysteresis edge (%s days-equivalent)", reengage)
	}
	if !strings.Contains(adr, "`baseline_age_days` and `bootstrap_capped`.") {
		t.Error("ADR-0019 detail page does not record that baseline_age_days and bootstrap_capped are served")
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
