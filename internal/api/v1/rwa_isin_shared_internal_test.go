package v1

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestAdmitClassicCandidates_SharedISINReachesTheRow(t *testing.T) {
	const (
		a = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
		b = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
		c = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
	)
	bound := []timescale.Sep1BoundCurrency{
		{Code: "ONE", Issuer: a, HomeDomain: "a.example", AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
		{Code: "TWO", Issuer: b, HomeDomain: "b.example", AnchorAssetType: "other", AnchorAsset: "lu2900381208"},
		{Code: "SOLO", Issuer: c, HomeDomain: "c.example", AnchorAssetType: "other", AnchorAsset: "LU3258450587"},
	}
	entries := map[string]timescale.DirectoryEntry{
		a: {Name: "A", Tags: []string{"issuer"}},
		b: {Name: "B", Tags: []string{"issuer"}},
		c: {Name: "C", Tags: []string{"issuer"}},
	}
	s := &Server{logger: slog.Default()}
	out := rwaMembership{refusals: map[string]int{}}
	s.admitClassicCandidates(&out, bound, entries)
	if len(out.members) != 3 {
		t.Fatalf("admitted %d members, want 3 (sharing must not remove one)", len(out.members))
	}
	rows := map[string]AssetDetail{}
	for _, m := range out.members {
		rows[rwaKey(m.code, m.issuer)] = AssetDetail{Code: m.code}
	}
	served, _ := s.rwaAssetRows(out, rows)
	if len(served) != 3 {
		t.Fatalf("served %d rows, want 3", len(served))
	}
	want := map[string]bool{"ONE": true, "TWO": true, "SOLO": false}
	for _, r := range served {
		if r.ISINShared != want[r.Code] {
			t.Errorf("%s: ISINShared = %v, want %v", r.Code, r.ISINShared, want[r.Code])
		}
	}
}

func TestRWAMarkSharedISINs(t *testing.T) {
	const isin = "LU2900381208"
	members := []rwaMember{
		{code: "A", issuer: "GA", anchorAsset: isin},
		{code: "B", issuer: "GB", anchorAsset: " " + strings.ToLower(isin) + " "},
		{code: "C", issuer: "GC", anchorAsset: "LU3258450587"},
		{code: "D", issuer: "GD", anchorAsset: "US Treasury Notes"},
		{code: "E", issuer: "GE", anchorAsset: "US Treasury Notes"},
		{code: "F", issuer: "GF"},
	}
	rwaMarkSharedISINs(members)
	want := []bool{true, true, false, false, false, false}
	for i, m := range members {
		if m.isinShared != want[i] {
			t.Errorf("%s: isinShared = %v, want %v", m.code, m.isinShared, want[i])
		}
	}
	if len(members) != len(want) {
		t.Fatalf("members dropped: %d", len(members))
	}
}

func TestRWAAssetISINSharedJSON(t *testing.T) {
	b, _ := json.Marshal(RWAAsset{ISINShared: true})
	if !strings.Contains(string(b), `"isin_shared":true`) {
		t.Errorf("missing isin_shared: %s", b)
	}
	b, _ = json.Marshal(RWAAsset{})
	if strings.Contains(string(b), "isin_shared") {
		t.Errorf("isin_shared must be omitted when false: %s", b)
	}
}
