package completeness_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
)

func TestIsAuditAxis(t *testing.T) {
	if !completeness.IsAuditAxis(completeness.SystemRecognitionSource) {
		t.Error("the system recognition row must classify as an audit axis")
	}
	// Fail-LOUD: anything unclassified counts as a source, so it can
	// still fail the public headline rather than excusing itself.
	for _, s := range []string{"soroswap", "blend", "sdex", "", "Recognition", "recognition2"} {
		if completeness.IsAuditAxis(s) {
			t.Errorf("IsAuditAxis(%q) = true — an unclassified name must count as a source", s)
		}
	}
}

func TestRecognitionDetail_roundTrip(t *testing.T) {
	for _, want := range []completeness.RecognitionCensus{
		{}, // clean
		{Shapes: 23945, Contracts: 4172, EarliestLedger: 50560486}, // r1, 2026-09-02
		{Shapes: 1, Contracts: 1, EarliestLedger: 50457424},
	} {
		got, ok := completeness.ParseRecognitionDetail(completeness.FormatRecognitionDetail(want))
		if !ok {
			t.Fatalf("ParseRecognitionDetail failed to round-trip %+v", want)
		}
		if got != want {
			t.Errorf("round-trip: got %+v, want %+v", got, want)
		}
	}
}

// WIRE CONTRACT: the deployed data-freshness exporter derives
// stellarindex_recognition_unattributed_shapes with
// `substring(detail FROM '^[0-9]+')`
// (configs/ansible/roles/archival-node/files/data-freshness.sh). A
// non-numeric prefix silently reports 0 shapes — the under-reporting
// direction. This pins the same regex against the Go producer.
func TestFormatRecognitionDetail_leadsWithTheShapeCount(t *testing.T) {
	re := regexp.MustCompile(`^[0-9]+`)
	for _, c := range []completeness.RecognitionCensus{
		{},
		{Shapes: 23945, Contracts: 4172, EarliestLedger: 50560486},
	} {
		d := completeness.FormatRecognitionDetail(c)
		got := re.FindString(d)
		if got == "" {
			t.Fatalf("detail %q does not lead with digits — data-freshness.sh would report 0 shapes", d)
		}
		if want := strconv.Itoa(c.Shapes); got != want {
			t.Errorf("leading digits = %q, want %q (detail %q)", got, want, d)
		}
	}
}

// The detail must SAY what the census is, not only count it: it is
// served verbatim on /v1/coverage and read by operators in psql.
func TestFormatRecognitionDetail_saysWhatItMeans(t *testing.T) {
	d := completeness.FormatRecognitionDetail(completeness.RecognitionCensus{
		Shapes: 23945, Contracts: 4172, EarliestLedger: 50560486,
	})
	for _, want := range []string{"4172 unowned contract(s)", "no indexed source owns", "not missing data"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q does not contain %q", d, want)
		}
	}
}

// ParseRecognitionDetail must refuse anything it did not write, so the
// caller omits the typed counts rather than publishing invented zeroes.
func TestParseRecognitionDetail_refusesForeignStrings(t *testing.T) {
	for _, s := range []string{
		"",
		"complete: substrate + recognition + projection verified to tip",
		// The PRE-change format: it led with the count but carried no
		// contract count, so it must not parse into a census claiming
		// zero contracts.
		"23945 unrecognized shape(s) on unowned contracts (earliest ledger 50560486) — run verify-recognition",
	} {
		if c, ok := completeness.ParseRecognitionDetail(s); ok {
			t.Errorf("ParseRecognitionDetail(%q) = %+v, true — want refusal", s, c)
		}
	}
}
