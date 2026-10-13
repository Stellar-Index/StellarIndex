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

func TestRWAApplyReference_DeclaredISINMustMatchVettedBinding(t *testing.T) {
	const bb1Issuer = "GD5J6HLF5666X4AZLTFTXLY46J5SW7EXRKBLEYPJP33S33MXZGV6CWFN"
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613"
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	listed := timescale.ListingEntry{PriceUSD: "0.99", PricedAt: now.Add(-time.Hour), Source: "listing", ListingID: "x"}
	row := func(code, issuer, anchor string) *RWAAsset {
		return &RWAAsset{AssetID: code + "-" + issuer, Code: code, Issuer: issuer, AnchorAsset: anchor, CirculatingSupply: &supply, Decimals: intPtr(7)}
	}
	refused := []struct {
		name    string
		a       *RWAAsset
		listing bool
	}{
		{"bound pair declares another security", row("BB1", bb1Issuer, "US0378331005"), false},
		{"bound pair declares another security, listed", row("BB1", bb1Issuer, "US0378331005"), true},
		{"another pair declares the vetted ISIN", row("BB1", unboundIssuer, "DE000A2TR7Q2"), true},
		{"another code on the vetted issuer declares it", row("FAKE", bb1Issuer, "DE000A2TR7Q2"), true},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			listings := map[string]timescale.ListingEntry{}
			if c.listing {
				listings[c.a.AssetID] = listed
			}
			rwaApplyReference(c.a, snap, nil, listings, now)
			if c.a.Reference != nil {
				t.Fatalf("reference = %+v, want none: the row declares %s", c.a.Reference, c.a.AnchorAsset)
			}
			if c.a.ReferenceValuation.Status != RWAPremiumReferenceISINMismatch || c.a.Premium.Status != RWAPremiumReferenceISINMismatch {
				t.Errorf("statuses = %q / %q, want %q on both", c.a.ReferenceValuation.Status, c.a.Premium.Status, RWAPremiumReferenceISINMismatch)
			}
			if c.a.ReferenceValuation.ValueUSD != nil {
				t.Errorf("a refused row carries a figure: %q", *c.a.ReferenceValuation.ValueUSD)
			}
		})
	}
}
