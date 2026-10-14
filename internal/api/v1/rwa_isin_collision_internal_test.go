package v1

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Two issuer accounts declaring one ISIN are surfaced on every admitted row declaring it (each account counted once,
// refused declarers included); the collision never demotes membership, basis or the constant-NAV reference.
func TestAdmitClassicCandidates_ISINCollisionAcrossIssuers(t *testing.T) {
	const (
		franklinIB = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
		listed     = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
		other      = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
		refused    = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	)
	bound := []timescale.Sep1BoundCurrency{
		{Code: "BENJI", Issuer: listed, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "FOBXX"},
		{Code: "gBENJI", Issuer: franklinIB, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "COPY", Issuer: other, HomeDomain: "copy.example", AnchorAssetType: "other", AnchorAsset: " lu2900381208 "},
		{Code: "COPY2", Issuer: other, HomeDomain: "copy.example", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "LONE", Issuer: refused, HomeDomain: "unlisted.example", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "grBENJI", Issuer: listed, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "LU3258450587"},
	}
	entries := map[string]timescale.DirectoryEntry{
		listed: {Name: "Franklin Templeton", Tags: []string{"issuer"}},
		other:  {Name: "Copy Issuer", Tags: []string{"issuer"}},
	}
	s := &Server{logger: slog.Default()}
	out := rwaMembership{refusals: map[string]int{}}
	s.admitClassicCandidates(&out, bound, entries)

	got := map[string]rwaMember{}
	for _, m := range out.members {
		got[m.code] = m
	}
	if _, ok := got["LONE"]; ok {
		t.Fatal("LONE admitted: an unrecognised issuer must stay refused")
	}
	// gBENJI, COPY and COPY2 share the ISIN across three accounts: the
	// refused LONE counts, COPY's two codes count once.
	for _, code := range []string{"gBENJI", "COPY", "COPY2"} {
		m, ok := got[code]
		if !ok {
			t.Fatalf("%s not admitted: a collision must never demote membership", code)
		}
		if m.basis != rwa.BasisSep1ISIN {
			t.Errorf("%s basis = %q, want %q", code, m.basis, rwa.BasisSep1ISIN)
		}
		if c := m.isinCollision; c == nil || c.ISIN != "LU2900381208" || c.DeclaredByIssuers != 3 {
			t.Errorf("%s isinCollision = %+v, want LU2900381208 declared by 3 issuers", code, c)
		}
	}
	for _, code := range []string{"BENJI", "grBENJI"} {
		if c := got[code].isinCollision; c != nil {
			t.Errorf("%s isinCollision = %+v, want none: its anchor has one declarer or is no ISIN", code, c)
		}
	}

	rows := map[string]AssetDetail{
		rwaKey("gBENJI", franklinIB): {AssetID: "gBENJI-" + franklinIB, Code: "gBENJI"},
		rwaKey("grBENJI", listed):    {AssetID: "grBENJI-" + listed, Code: "grBENJI"},
	}
	served, _ := s.rwaAssetRows(out, rows, time.Now())
	byCode := map[string]RWAAsset{}
	for _, a := range served {
		byCode[a.Code] = a
	}
	body, err := json.Marshal(byCode["gBENJI"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"isin_collision":{"isin":"LU2900381208","declared_by_issuers":3}`) {
		t.Errorf("gBENJI wire row lacks the collision: %s", body)
	}
	body, err = json.Marshal(byCode["grBENJI"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "isin_collision") {
		t.Errorf("grBENJI wire row carries a collision it does not have: %s", body)
	}

	// The verified pair keeps its prospectus reference beside the
	// collision; only the exact-pair binding prices through the ISIN.
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613"
	a := byCode["gBENJI"]
	a.Issuer, a.CirculatingSupply, a.Decimals = franklinIB, &supply, intPtr(7)
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	rwaApplyReference(&a, snap, nil, map[string]timescale.ListingEntry{}, now)
	if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV {
		t.Errorf("gBENJI reference = %+v, want the prospectus CNAV unchanged by the collision", a.Reference)
	}
}

// The franklintempleton.com case: SEP-1 binds BENJI on a directory-listed account and gBENJI on an unlisted one, same
// domain. The sibling arm admits gBENJI and says so; an unlisted issuer with no listed sibling stays refused.
func TestAdmitClassicCandidates_SiblingOnTheSameDomain(t *testing.T) {
	const (
		listed   = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
		unlisted = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
		lonely   = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
	)
	bound := []timescale.Sep1BoundCurrency{
		{Code: "BENJI", Issuer: listed, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "FOBXX"},
		{Code: "gBENJI", Issuer: unlisted, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "LONE", Issuer: lonely, HomeDomain: "unlisted.example", AnchorAssetType: "other", AnchorAsset: "LU3258450587"},
	}
	entries := map[string]timescale.DirectoryEntry{
		listed: {Name: "Franklin Templeton", Tags: []string{"issuer"}},
	}
	s := &Server{logger: slog.Default()}
	out := rwaMembership{refusals: map[string]int{}}
	s.admitClassicCandidates(&out, bound, entries)

	got := map[string]rwaMember{}
	for _, m := range out.members {
		got[m.code] = m
	}
	if m, ok := got["BENJI"]; !ok || m.recognition != rwa.RecognitionDirectory {
		t.Errorf("BENJI = %+v, want admitted on the direct directory arm", m)
	}
	if m, ok := got["gBENJI"]; !ok || m.recognition != rwa.RecognitionDomainSibling || m.basis != rwa.BasisSep1ISIN {
		t.Errorf("gBENJI = %+v, want admitted on the ISIN with sibling recognition", m)
	}
	if _, ok := got["LONE"]; ok {
		t.Error("LONE admitted: an unlisted issuer whose domain has no listed sibling must stay refused")
	}
	if out.refusals[rwa.RejectNoRecognition] != 1 {
		t.Errorf("refusals[%s] = %d, want 1", rwa.RejectNoRecognition, out.refusals[rwa.RejectNoRecognition])
	}
}

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
