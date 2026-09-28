package timescale

import (
	"reflect"
	"testing"
)

// An established issuer whose home_domain now serves a different identity
// must surface every changed field — the overwrite used to be blind.
func TestSep1IdentityChanges(t *testing.T) {
	held := []byte(`{"OrgName":"Circle","OrgVerified":true,
		"Documentation":{"ORG_URL":"https://circle.com","ORG_LOGO":"https://circle.com/l.png","ORG_DBA":"x"},
		"Currencies":[{"Code":"USDC","Issuer":"GA5Z","Image":"https://circle.com/usdc.png","Name":"USD Coin"},
		              {"Code":"EURC","Issuer":"GA5Z","Image":"https://circle.com/eurc.png"}]}`)
	next := []byte(`{"OrgName":"Circle Internet","OrgVerified":true,
		"Documentation":{"ORG_URL":"https://evil.example","ORG_DBA":"y"},
		"Currencies":[{"Code":"USDC","Issuer":"GA5Z","Image":"https://evil.example/usdc.png","Name":"Renamed"},
		              {"Code":"NEWT","Issuer":"GA5Z","Image":"https://evil.example/n.png"}]}`)

	got, err := sep1IdentityChanges(held, next)
	if err != nil {
		t.Fatal(err)
	}
	want := []Sep1IdentityChange{
		{Field: Sep1FieldOrgName, Old: "Circle", New: "Circle Internet"},
		{Field: Sep1FieldOrgURL, Old: "https://circle.com", New: "https://evil.example"},
		{Field: Sep1FieldOrgLogo, Old: "https://circle.com/l.png", New: ""},
		{Field: Sep1FieldCurrencyImage, Currency: "EURC-GA5Z", Old: "https://circle.com/eurc.png", New: ""},
		{Field: Sep1FieldCurrencyImage, Currency: "NEWT-GA5Z", Old: "", New: "https://evil.example/n.png"},
		{Field: Sep1FieldCurrencyImage, Currency: "USDC-GA5Z", Old: "https://circle.com/usdc.png", New: "https://evil.example/usdc.png"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}

	if got, err := sep1IdentityChanges(held, held); err != nil || len(got) != 0 {
		t.Fatalf("identical payload: got (%+v, %v), want no changes", got, err)
	}
	if got, err := sep1IdentityChanges(nil, next); err != nil || len(got) != 0 {
		t.Fatalf("first fetch: got (%+v, %v), want no changes (nothing was established)", got, err)
	}
}
