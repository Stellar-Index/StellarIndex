package v1

import (
	"strings"
	"testing"
)

// TestRWASummariseReference_NamesTheOracleOutage: rows refused because the reference read did not answer must be named
// as an outage in the basis, not described as having no bound oracle feed.
func TestRWASummariseReference_NamesTheOracleOutage(t *testing.T) {
	assets := []RWAAsset{{}, {}}
	assets[0].ReferenceValuation.Status = RWAPremiumReferenceUnavailable
	if basis := rwaSummariseReference(assets, rwaTruncation{}).Basis; !strings.Contains(basis, "1 of these carry no reference valuation because the oracle reference read did not answer") {
		t.Errorf("basis = %q, want the outage named", basis)
	}
	if basis := rwaSummariseReference([]RWAAsset{{}}, rwaTruncation{}).Basis; strings.Contains(basis, "did not answer") {
		t.Errorf("basis names an outage with every reference read answered: %q", basis)
	}
}
