package v1

import (
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Two recognised issuers declaring one unbound ISIN are both refused; a
// claimant that fails R3 cannot evict an admitted holder; and a
// constant-NAV binding keeps its own pair against a recognised rival.
func TestAdmitClassicCandidates_ContestedISIN(t *testing.T) {
	const (
		issuerA     = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		issuerB     = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
		unlisted    = "GCUG7ARUFEEUMSL56K7245YCPXPZPOXAY6TSRXZB2JZFBI4DOBVOTUSA"
		cnavHolder  = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
		soleHolder  = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
		unboundISIN = "US0378331005"
	)
	bound := []timescale.Sep1BoundCurrency{
		{Code: "AAPL", Issuer: issuerA, HomeDomain: "a.example", AnchorAssetType: "other", AnchorAsset: unboundISIN},
		{Code: "APPLE", Issuer: issuerB, HomeDomain: "b.example", AnchorAssetType: "stock", AnchorAsset: unboundISIN},
		{Code: "GBX", Issuer: soleHolder, HomeDomain: "c.example", AnchorAssetType: "other", AnchorAsset: "GB0002634946"},
		{Code: "GBX", Issuer: unlisted, HomeDomain: "d.example", AnchorAssetType: "other", AnchorAsset: "GB0002634946"},
		{Code: "gBENJI", Issuer: cnavHolder, HomeDomain: "www.franklintempleton.com", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "gBENJI", Issuer: issuerB, HomeDomain: "b.example", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
	}
	entries := map[string]timescale.DirectoryEntry{
		issuerA:    {Name: "A", Tags: []string{"issuer"}},
		issuerB:    {Name: "B", Tags: []string{"issuer"}},
		soleHolder: {Name: "C", Tags: []string{"issuer"}},
		cnavHolder: {Name: "Franklin Templeton", Tags: []string{"issuer"}},
	}
	s := &Server{logger: slog.Default()}
	out := rwaMembership{refusals: map[string]int{}}
	s.admitClassicCandidates(&out, bound, entries)

	got := map[string]bool{}
	for _, m := range out.members {
		got[rwaKey(m.code, m.issuer)] = true
	}
	want := map[string]bool{
		rwaKey("GBX", soleHolder):    true,
		rwaKey("gBENJI", cnavHolder): true,
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s refused, want admitted", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s admitted, want refused", k)
		}
	}
	if n := out.refusals[rwa.RejectContestedISIN]; n != 3 {
		t.Errorf("refusals[%s] = %d, want 3 (AAPL, APPLE, the rival gBENJI)", rwa.RejectContestedISIN, n)
	}
	if n := out.refusals[rwa.RejectNoRecognition]; n != 1 {
		t.Errorf("refusals[%s] = %d, want 1 (the unlisted GBX)", rwa.RejectNoRecognition, n)
	}
}
