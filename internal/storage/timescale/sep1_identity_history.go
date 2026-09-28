package timescale

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Sep1 identity fields recorded in issuer_identity_history (migration 0190).
const (
	Sep1FieldOrgName       = "org_name"
	Sep1FieldOrgURL        = "org_url"
	Sep1FieldOrgLogo       = "org_logo"
	Sep1FieldCurrencyImage = "currency_image"
)

// Sep1IdentityChange is one transition of an identity field between the
// held sep1_payload and the one replacing it. Old/New "" = absent.
type Sep1IdentityChange struct {
	Field    string
	Currency string // `<Code>-<Issuer>` as declared; currency_image only
	Old, New string
}

type sep1IdentityView struct {
	OrgName       string            `json:"OrgName"`
	Documentation map[string]string `json:"Documentation"`
	Currencies    []struct {
		Code   string `json:"Code"`
		Issuer string `json:"Issuer"`
		Image  string `json:"Image"`
	} `json:"Currencies"`
}

// sep1IdentityChanges diffs the identity fields a holder of the issuer's
// home_domain controls. A nil prev is a first fetch: nothing established
// changed, so nothing is returned.
func sep1IdentityChanges(prev, next []byte) ([]Sep1IdentityChange, error) {
	if len(prev) == 0 {
		return nil, nil
	}
	var a, b sep1IdentityView
	if err := json.Unmarshal(prev, &a); err != nil {
		return nil, fmt.Errorf("decode held sep1_payload: %w", err)
	}
	if err := json.Unmarshal(next, &b); err != nil {
		return nil, fmt.Errorf("decode new sep1_payload: %w", err)
	}
	var out []Sep1IdentityChange
	add := func(field, currency, o, n string) {
		if o != n {
			out = append(out, Sep1IdentityChange{Field: field, Currency: currency, Old: o, New: n})
		}
	}
	add(Sep1FieldOrgName, "", a.OrgName, b.OrgName)
	add(Sep1FieldOrgURL, "", a.Documentation["ORG_URL"], b.Documentation["ORG_URL"])
	add(Sep1FieldOrgLogo, "", a.Documentation["ORG_LOGO"], b.Documentation["ORG_LOGO"])

	images := func(v sep1IdentityView) map[string]string {
		m := make(map[string]string, len(v.Currencies))
		for _, c := range v.Currencies {
			m[c.Code+"-"+c.Issuer] = c.Image
		}
		return m
	}
	oldImg, newImg := images(a), images(b)
	keys := make([]string, 0, len(oldImg)+len(newImg))
	for k := range oldImg {
		keys = append(keys, k)
	}
	for k := range newImg {
		if _, ok := oldImg[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(Sep1FieldCurrencyImage, k, oldImg[k], newImg[k])
	}
	return out, nil
}
