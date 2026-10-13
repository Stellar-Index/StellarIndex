package v1

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A bound entry declaring neither a class nor an anchor_asset is admitted through the
// hand-vetted table, carries the binding on its row, and shares the collision count with any
// other issuer declaring the same ISIN outright.
func TestAdmitClassicCandidates_VettedISINBindingAdmitsAndCollides(t *testing.T) {
	const (
		bb1Issuer = "GD5J6HLF5666X4AZLTFTXLY46J5SW7EXRKBLEYPJP33S33MXZGV6CWFN"
		impostor  = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	)
	bound := []timescale.Sep1BoundCurrency{
		{Code: "BB1", Issuer: bb1Issuer, HomeDomain: "bitbondsto.com", Name: "Bitbond Finance Tokenized Bond 1"},
		{Code: "BB1", Issuer: impostor, HomeDomain: "example.org", AnchorAssetType: "other", AnchorAsset: "DE000A2TR7Q2"},
	}
	entries := map[string]timescale.DirectoryEntry{
		bb1Issuer: {Name: "Bitbond", Tags: []string{"anchor", "issuer"}},
		impostor:  {Name: "Other", Tags: []string{"issuer"}},
	}
	if !rwaCandidateFilter(bound[0]) {
		t.Fatal("the pre-filter drops the vetted pair")
	}
	s := &Server{logger: slog.Default()}
	out := rwaMembership{refusals: map[string]int{}}
	s.admitClassicCandidates(&out, bound, entries)
	byIssuer := map[string]rwaMember{}
	for _, m := range out.members {
		byIssuer[m.issuer] = m
	}
	got, ok := byIssuer[bb1Issuer]
	if !ok {
		t.Fatalf("BB1 not admitted: refusals %v", out.refusals)
	}
	if got.basis != rwa.BasisSep1ISINVetted || got.anchorClass != "" {
		t.Fatalf("BB1 basis %q class %q, want %s and no class", got.basis, got.anchorClass, rwa.BasisSep1ISINVetted)
	}
	if got.vettedISIN == nil || got.vettedISIN.ISIN != "DE000A2TR7Q2" || got.vettedISIN.ReviewBy == "" {
		t.Fatalf("BB1 vetted binding %+v", got.vettedISIN)
	}
	if got.isinCollision == nil || got.isinCollision.ISIN != "DE000A2TR7Q2" || got.isinCollision.DeclaredByIssuers != 2 {
		t.Fatalf("BB1 collision %+v, want the impostor counted", got.isinCollision)
	}
	other := byIssuer[impostor]
	if other.basis != rwa.BasisSep1ISIN || other.vettedISIN != nil {
		t.Fatalf("impostor basis %q vetted %+v: the table must not leak to another issuer", other.basis, other.vettedISIN)
	}
	if other.isinCollision == nil || other.isinCollision.DeclaredByIssuers != 2 {
		t.Fatalf("impostor collision %+v, want the vetted claim counted", other.isinCollision)
	}
	deadline := got.vettedISIN.ReviewDeadline()
	row := rwaVettedISINOf(got.vettedISIN, deadline)
	if row == nil || row.Source != got.vettedISIN.Source || row.VerifiedOn != got.vettedISIN.VerifiedOn || row.Instrument == "" {
		t.Fatalf("served binding %+v", row)
	}
	if row.ReviewDue {
		t.Fatalf("review_due at the deadline itself: %+v", row)
	}
	if late := rwaVettedISINOf(got.vettedISIN, deadline.Add(time.Nanosecond)); !late.ReviewDue {
		t.Fatalf("review_due not set past the deadline: %+v", late)
	}
	if rwaVettedISINOf(nil, deadline) != nil {
		t.Fatal("nil binding served")
	}
}
