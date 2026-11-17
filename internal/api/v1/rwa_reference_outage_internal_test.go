package v1

import (
	"strings"
	"testing"
	"time"
)

// TestRWASummariseReference_NamesTheReadThatDidNotAnswer: rows refused because a reference read did not answer are named
// as an outage of THAT read in the basis: an oracle outage on an oracle-bound row, a listing directory outage on an
// unbound one, never the oracle for a read it was not part of.
func TestRWASummariseReference_NamesTheReadThatDidNotAnswer(t *testing.T) {
	now := time.Now()
	oracleDown := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&oracleDown, rwaReferences{}, nil, listingAnswered(nil), now)
	listingDown := RWAAsset{
		AssetID: "WTGX-GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6", Code: "WTGX",
		Issuer: "GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6",
	}
	rwaApplyReference(&listingDown, rwaReferenceSnapshotFrom(nil), nil, assetListing{}, now)
	for _, a := range []RWAAsset{oracleDown, listingDown} {
		if a.ReferenceValuation.Status != RWAPremiumReferenceUnavailable {
			t.Fatalf("%s status = %q, want %q", a.Code, a.ReferenceValuation.Status, RWAPremiumReferenceUnavailable)
		}
	}

	basis := rwaSummariseReference([]RWAAsset{listingDown}, rwaTruncation{}).Basis
	if !strings.Contains(basis, "1 of these carry no reference valuation because the listing directory read did not answer") || strings.Contains(basis, "oracle reference read") {
		t.Errorf("listing outage basis = %q, want the listing directory named and the oracle not blamed", basis)
	}
	basis = rwaSummariseReference([]RWAAsset{oracleDown, listingDown, {}}, rwaTruncation{}).Basis
	if !strings.Contains(basis, "1 of these carry no reference valuation because the oracle reference read did not answer, "+
		"and 1 because the listing directory read did not answer: an outage") {
		t.Errorf("mixed outage basis = %q, want each read counted against its own rows", basis)
	}
	if basis := rwaSummariseReference([]RWAAsset{{}}, rwaTruncation{}).Basis; strings.Contains(basis, "did not answer") {
		t.Errorf("basis names an outage with every reference read answered: %q", basis)
	}
}
