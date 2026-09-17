package rwa

import "testing"

// Franklin Templeton's Luxembourg and Singapore share classes: declared
// with ISINs in franklintempleton.com's own SEP-1, on issuer accounts the
// curated directory never listed, beside the BENJI issuer it did. A
// sibling on the same bound domain recognises them — and the verdict
// says which arm did it.
func TestQualify_SiblingOnTheSameDomainRecognisesAnUnlistedIssuer(t *testing.T) {
	base := Candidate{
		Code: "gBENJI", Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		BoundSep1: true, DeclaredAnchorType: "other", DeclaredAnchorAsset: "LU2900381208",
	}
	if v := Qualify(base); v.InSet || v.Reject != RejectNoRecognition {
		t.Fatalf("unlisted, no sibling: %+v, want refused for recognition", v)
	}
	withSibling := base
	withSibling.SiblingRecognised = true
	v := Qualify(withSibling)
	if !v.InSet || v.Basis != BasisSep1ISIN || v.Recognition != RecognitionDomainSibling {
		t.Fatalf("unlisted with a recognised sibling: %+v, want in set on the ISIN with sibling recognition", v)
	}
	listed := withSibling
	listed.DirectoryTags = []string{"issuer"}
	if v := Qualify(listed); !v.InSet || v.Recognition != RecognitionDirectory {
		t.Fatalf("listed: %+v, want the direct directory arm", v)
	}
	flagged := withSibling
	flagged.DirectoryTags = []string{"scam"}
	if v := Qualify(flagged); v.InSet || v.Reject != RejectScamFlagged {
		t.Fatalf("flagged with a recognised sibling: %+v, want refused as scam", v)
	}
	noClaim := withSibling
	noClaim.DeclaredAnchorAsset = "not-an-isin"
	if v := Qualify(noClaim); v.InSet || v.Reject != RejectNoInstrumentClaim {
		t.Fatalf("sibling without an instrument claim: %+v, want refused for the claim", v)
	}
}
