package timescale

import "testing"

// The provenance rule, without a database. A stellar.toml describes only
// the issuer that served it: an entry naming someone else is dropped,
// which is what stops any account from publishing a real-world claim
// under another issuer's identity.
func TestBoundSep1CurrenciesFromPayload_DropsEntriesNamingAnotherIssuer(t *testing.T) {
	const (
		serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		other   = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"
	)
	payload := `{
		"OrgName": "Etherfuse",
		"Currencies": [
			{"Code":"USTRY","Issuer":"` + serving + `","AnchorAssetType":"bond","AnchorAsset":"US Treasury Notes","Name":"US Treasury Bill"},
			{"Code":"USDY","Issuer":"` + other + `","AnchorAssetType":"bond"},
			{"Code":"","Issuer":"` + serving + `","AnchorAssetType":"bond"},
			{"Code":"NOISSUER","AnchorAssetType":"bond"}
		]
	}`
	got, dropped := boundSep1CurrenciesFromPayload(serving, "etherfuse.com", payload, nil)
	if dropped != 0 {
		t.Errorf("dropped = %d with a nil filter, want 0 — a nil filter keeps everything", dropped)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want only the self-declared one: %+v", len(got), got)
	}
	e := got[0]
	if e.Code != "USTRY" || e.Issuer != serving {
		t.Errorf("entry = %+v, want USTRY bound to the serving account", e)
	}
	if e.HomeDomain != "etherfuse.com" || e.OrgName != "Etherfuse" {
		t.Errorf("issuer context not carried: %+v", e)
	}
	if e.AnchorAssetType != "bond" || e.AnchorAsset != "US Treasury Notes" || e.Name != "US Treasury Bill" {
		t.Errorf("declaration not carried verbatim: %+v", e)
	}
}

// A filter narrows the result and REPORTS what it dropped, so a caller
// can state the population it narrowed from. A filtered read that
// reported nothing would be indistinguishable from an unfiltered one
// that found little.
func TestBoundSep1CurrenciesFromPayload_CountsWhatTheFilterDropped(t *testing.T) {
	const serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	payload := `{"Currencies":[
		{"Code":"A","Issuer":"` + serving + `","AnchorAssetType":"bond"},
		{"Code":"B","Issuer":"` + serving + `","AnchorAssetType":"nft"},
		{"Code":"C","Issuer":"` + serving + `","AnchorAssetType":"nft"}
	]}`
	got, dropped := boundSep1CurrenciesFromPayload(serving, "", payload,
		func(c Sep1BoundCurrency) bool { return c.AnchorAssetType == "bond" })
	if len(got) != 1 || got[0].Code != "A" {
		t.Fatalf("kept = %+v, want only A", got)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
}

// One malformed payload must not empty the whole result: the scan
// walks every issuer, and a single corrupt toml is a data fact about
// that issuer, not an outage of the surface.
func TestBoundSep1CurrenciesFromPayload_CorruptPayloadIsSkipped(t *testing.T) {
	got, dropped := boundSep1CurrenciesFromPayload("G1", "", "{not json", nil)
	if len(got) != 0 || dropped != 0 {
		t.Errorf("got %+v / dropped %d, want an empty, non-fatal result", got, dropped)
	}
}
