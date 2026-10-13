package rwa

import (
	"strings"
	"testing"
	"time"
)

const testBB1Issuer = "GD5J6HLF5666X4AZLTFTXLY46J5SW7EXRKBLEYPJP33S33MXZGV6CWFN"

func TestVettedISIN_IsExactOnBothHalvesAndWellFormed(t *testing.T) {
	for _, b := range VettedISINBindings() {
		got, ok := VettedISIN(b.Code, b.Issuer)
		if !ok || got != b {
			t.Fatalf("VettedISIN(%s, %s) = %+v, %v", b.Code, b.Issuer, got, ok)
		}
		if _, ok := VettedISIN(b.Code, "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"); ok {
			t.Errorf("%s bound under a foreign issuer", b.Code)
		}
		if _, ok := VettedISIN(b.Code+"X", b.Issuer); ok {
			t.Errorf("%s bound under a foreign code", b.Code)
		}
		if norm, ok := CanonicalISIN(b.ISIN); !ok || norm != b.ISIN {
			t.Errorf("%s: ISIN %q is not canonical", b.Code, b.ISIN)
		}
	}
}

func TestVettedISIN_EveryBindingIsEvidencedAndBounded(t *testing.T) {
	for _, b := range VettedISINBindings() {
		if !strings.HasPrefix(b.Source, "https://") || !strings.HasSuffix(b.Source, "/.well-known/stellar.toml") {
			t.Errorf("%s: Source %q is not the issuer's https SEP-1", b.Code, b.Source)
		}
		if strings.TrimSpace(b.Instrument) == "" {
			t.Errorf("%s: no instrument name", b.Code)
		}
		verified, err := time.Parse(reviewDateLayout, b.VerifiedOn)
		if err != nil {
			t.Fatalf("%s: VerifiedOn %q: %v", b.Code, b.VerifiedOn, err)
		}
		if want := verified.Add(VettedISINReviewInterval).Format(reviewDateLayout); b.ReviewBy != want {
			t.Errorf("%s: ReviewBy %q, want %q", b.Code, b.ReviewBy, want)
		}
	}
}

func TestVettedISIN_ReviewDueFlipsAtTheDeadline(t *testing.T) {
	b, ok := VettedISIN("BB1", testBB1Issuer)
	if !ok {
		t.Fatal("BB1 unbound")
	}
	deadline := b.ReviewDeadline()
	if deadline.IsZero() {
		t.Fatal("zero deadline")
	}
	if b.ReviewDue(deadline) {
		t.Error("due at the exact deadline")
	}
	if !b.ReviewDue(deadline.Add(time.Nanosecond)) {
		t.Error("not due after the deadline")
	}
	if !(VettedISINBinding{ReviewBy: "soon"}).ReviewDue(time.Unix(0, 0)) {
		t.Error("an unparseable ReviewBy is not already due")
	}
}

func TestIndexVettedISIN_RefusesADefectiveTable(t *testing.T) {
	good := VettedISINBinding{Code: "A", Issuer: "G1", ISIN: "LU2900381208", Instrument: "x", Source: "https://a/.well-known/stellar.toml", VerifiedOn: "2026-01-01"}
	cases := map[string][]VettedISINBinding{
		"pair twice": {good, {Code: "A", Issuer: "G1", ISIN: "US0378331005", VerifiedOn: "2026-01-01"}},
		"isin twice": {good, {Code: "B", Issuer: "G2", ISIN: "LU2900381208", VerifiedOn: "2026-01-01"}},
		"bad date":   {{Code: "A", Issuer: "G1", ISIN: "LU2900381208", VerifiedOn: "January"}},
		"bad isin":   {{Code: "A", Issuer: "G1", ISIN: "LU2900381209", VerifiedOn: "2026-01-01"}},
		"lower isin": {{Code: "A", Issuer: "G1", ISIN: "lu2900381208", VerifiedOn: "2026-01-01"}},
	}
	for name, table := range cases {
		if _, err := indexVettedISIN(table); err == nil {
			t.Errorf("%s: indexed", name)
		}
	}
	tbl, err := indexVettedISIN([]VettedISINBinding{good})
	if err != nil {
		t.Fatal(err)
	}
	if got := tbl.byPair[instrumentKey{code: "A", issuer: "G1"}].ReviewBy; got != "2026-04-01" {
		t.Errorf("ReviewBy %q, want 2026-04-01", got)
	}
}
