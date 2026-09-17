package v1

import (
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The 2026-09-17 case: franklintempleton.com's SEP-1 binds BENJI on a
// directory-listed account and gBENJI on an account the directory never
// listed, both on the same domain. The sibling arm admits gBENJI and
// says so; an unlisted issuer on a domain with no listed sibling stays
// refused; and the direct arm keeps its label on the listed account.
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
