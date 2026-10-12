package timescale

import (
	"database/sql"
	"reflect"
	"testing"
)

// The provenance rule, without a database: a stellar.toml describes only the issuer that served it, so an
// entry naming someone else is dropped, stopping any account from publishing a claim under another issuer's
// identity.
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
	got, census := boundSep1CurrenciesFromPayload(serving, "etherfuse.com", payload, nil)
	if census.EntriesFiltered != 0 {
		t.Errorf("EntriesFiltered = %d with a nil filter, want 0 — a nil filter keeps everything", census.EntriesFiltered)
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

	// Each of the three dropped entries lands in its OWN bucket, so payloads of four and four thousand entries
	// are distinguishable from outside.
	want := Sep1BoundCensus{
		IssuersDeclaring: 1, Entries: 4,
		EntriesMissingCode: 1, EntriesMissingIssuer: 1, EntriesNamingAnotherIssuer: 1,
		EntriesBound: 1, EntriesKept: 1,
	}
	if census != want {
		t.Errorf("census = %+v, want %+v", census, want)
	}
}

// A filter narrows the result and reports what it dropped, so a caller can state the population it narrowed
// from; a filtered read reporting nothing would look the same as an unfiltered one that found little.
func TestBoundSep1CurrenciesFromPayload_CountsWhatTheFilterDropped(t *testing.T) {
	const serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	payload := `{"Currencies":[
		{"Code":"A","Issuer":"` + serving + `","AnchorAssetType":"bond"},
		{"Code":"B","Issuer":"` + serving + `","AnchorAssetType":"nft"},
		{"Code":"C","Issuer":"` + serving + `","AnchorAssetType":"nft"}
	]}`
	got, census := boundSep1CurrenciesFromPayload(serving, "", payload,
		func(c Sep1BoundCurrency) bool { return c.AnchorAssetType == "bond" })
	if len(got) != 1 || got[0].Code != "A" {
		t.Fatalf("kept = %+v, want only A", got)
	}
	if census.EntriesFiltered != 2 {
		t.Errorf("EntriesFiltered = %d, want 2", census.EntriesFiltered)
	}
	if census.EntriesBound != 3 || census.EntriesKept != 1 {
		t.Errorf("bound/kept = %d/%d, want 3/1", census.EntriesBound, census.EntriesKept)
	}
}

// One malformed payload must not empty the whole result: a single corrupt toml is a fact about
// that issuer, not an outage of the surface. It IS counted, because a payload that will not
// decode and an issuer that declares nothing are different findings with different owners.
func TestBoundSep1CurrenciesFromPayload_CorruptPayloadIsCountedNotSwallowed(t *testing.T) {
	got, census := boundSep1CurrenciesFromPayload("G1", "", "{not json", nil)
	if len(got) != 0 {
		t.Errorf("got %+v, want an empty, non-fatal result", got)
	}
	if census.IssuersPayloadUnreadable != 1 {
		t.Errorf("IssuersPayloadUnreadable = %d, want 1 — an unreadable payload used to return a bare zero, "+
			"so a whole population could disappear through this branch without a single counter moving",
			census.IssuersPayloadUnreadable)
	}
	if census.IssuersDeclaringNothing != 0 || census.Entries != 0 {
		t.Errorf("census = %+v, want the drop attributed ONLY to the unreadable bucket", census)
	}
}

// A payload that decodes but declares no [[CURRENCIES]], one that fails to decode, and an issuer whose toml
// was never fetched are three different findings with different owners and fixes, so each gets its own
// bucket.
func TestBoundSep1CurrenciesFromPayload_DeclaringNothingIsItsOwnBucket(t *testing.T) {
	got, census := boundSep1CurrenciesFromPayload("G1", "example.com", `{"OrgName":"Example"}`, nil)
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
	if census.IssuersDeclaringNothing != 1 || census.IssuersPayloadUnreadable != 0 || census.IssuersDeclaring != 0 {
		t.Errorf("census = %+v, want exactly one issuer in the declares-nothing bucket", census)
	}
}

// The binding rule compares ACCOUNTS, not the bytes a toml carried: surrounding space and the case of an
// issuer's strkey do not change which account it is. This is canonicalisation, not relaxation: a DIFFERENT
// account is still refused, and the entry is carried under the canonical database spelling.
func TestBoundSep1CurrenciesFromPayload_BindsOnTheAccountNotTheSpelling(t *testing.T) {
	const (
		serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		other   = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"
	)
	payload := `{"Currencies":[
		{"Code":" USTRY ","Issuer":"  ` + serving + `\n","AnchorAssetType":"bond"},
		{"Code":"CETES","Issuer":"` + lower(serving) + `","AnchorAssetType":"bond"},
		{"Code":"NOTOURS","Issuer":"` + other + `","AnchorAssetType":"bond"},
		{"Code":"NOTAKEY","Issuer":"franklintempleton.com","AnchorAssetType":"bond"}
	]}`
	got, census := boundSep1CurrenciesFromPayload(serving, "etherfuse.com", payload, nil)
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want the two that name this account: %+v", len(got), got)
	}
	if got[0].Code != "USTRY" {
		t.Errorf("code = %q, want the declared code with surrounding space removed", got[0].Code)
	}
	for _, e := range got {
		if e.Issuer != serving {
			t.Errorf("issuer = %q, want the canonical database spelling %q — the toml's spelling must never "+
				"become the identity anything downstream joins on", e.Issuer, serving)
		}
	}
	// A value naming a different account, and a value that is not an
	// account at all, both bind to nothing.
	if census.EntriesNamingAnotherIssuer != 2 {
		t.Errorf("EntriesNamingAnotherIssuer = %d, want 2 (one other account, one non-strkey)",
			census.EntriesNamingAnotherIssuer)
	}
}

// A whitespace-only code names no asset. It must not pass the empty
// check, bind, and then fail every downstream join silently.
func TestBoundSep1CurrenciesFromPayload_WhitespaceCodeNamesNoAsset(t *testing.T) {
	const serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	got, census := boundSep1CurrenciesFromPayload(serving, "", `{"Currencies":[
		{"Code":"   ","Issuer":"`+serving+`","AnchorAssetType":"bond"}]}`, nil)
	if len(got) != 0 {
		t.Fatalf("kept %+v, want nothing — a blank code identifies no asset", got)
	}
	if census.EntriesMissingCode != 1 {
		t.Errorf("EntriesMissingCode = %d, want 1", census.EntriesMissingCode)
	}
}

// The census must add up, or the surface publishing it would hand a
// reader numbers that cannot be reconciled — which is worse than
// publishing none.
func TestSep1BoundCensus_ArithmeticCloses(t *testing.T) {
	const serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	payload := `{"Currencies":[
		{"Code":"A","Issuer":"` + serving + `","AnchorAssetType":"bond"},
		{"Code":"B","Issuer":"` + serving + `","AnchorAssetType":"nft"},
		{"Code":"C","AnchorAssetType":"bond"},
		{"Code":"","Issuer":"` + serving + `"},
		{"Code":"E","Issuer":"GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"}
	]}`
	_, census := boundSep1CurrenciesFromPayload(serving, "", payload,
		func(c Sep1BoundCurrency) bool { return c.AnchorAssetType == "bond" })
	census.IssuersWithPayload = 1
	census.IssuersWithHomeDomain = 1
	if why := census.Check(); why != "" {
		t.Errorf("census does not balance: %s (%+v)", why, census)
	}

	// And a census that has been tampered with must SAY it does not
	// balance rather than reading as sound.
	bad := census
	bad.EntriesBound++
	if why := bad.Check(); why == "" {
		t.Error("Check() passed a census whose entry stages do not sum — the guard proves nothing")
	}
}

// The domain-bearing population splits into issuers holding a payload, issuers a fetch reached that hold
// none, and an untried remainder. The first two come from different queries and can contradict; Check stops
// the publishing surface's clamp turning an impossible census into a sound-looking one.
func TestSep1BoundCensus_FetchSplitCannotExceedThePopulation(t *testing.T) {
	base := Sep1BoundCensus{
		IssuersWithHomeDomain:        10,
		IssuersWithPayload:           4,
		IssuersFetchedWithoutPayload: 6,
		IssuersDeclaring:             4,
	}
	if why := base.Check(); why != "" {
		t.Errorf("census does not balance: %s — 4 payloads plus 6 reached-and-empty is exactly the "+
			"population, leaving no issuer untried, which is what a completed drain looks like", why)
	}

	bad := base
	bad.IssuersFetchedWithoutPayload++
	if why := bad.Check(); why == "" {
		t.Error("Check() passed a census claiming more fetched issuers than issuers with a domain — " +
			"the never-attempted remainder would go negative and be published as zero")
	}
}

// A payload older than the attestation bound binds nothing. The refresh keeps a dark domain's last payload
// and stamps sep1_resolved_at on every failed attempt, so without this a domain that stopped serving its
// stellar.toml still admitted its accounts.
func TestBoundSep1CurrenciesFromRow_StalePayloadBindsNothing(t *testing.T) {
	const serving = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	payload := sql.NullString{Valid: true, String: `{"OrgName":"Etherfuse","Currencies":[
		{"Code":"USTRY","Issuer":"` + serving + `","AnchorAssetType":"bond"}]}`}

	fresh, freshCensus := boundSep1CurrenciesFromRow(serving, "etherfuse.com", payload, true, nil)
	if len(fresh) != 1 || fresh[0].Code != "USTRY" {
		t.Fatalf("fresh payload bound %+v, want USTRY", fresh)
	}
	if freshCensus.IssuersWithPayload != 1 || freshCensus.IssuersDeclaring != 1 || freshCensus.IssuersPayloadStale != 0 {
		t.Errorf("fresh census = %+v, want one payload walked and declaring", freshCensus)
	}

	stale, census := boundSep1CurrenciesFromRow(serving, "etherfuse.com", payload, false, nil)
	if len(stale) != 0 {
		t.Errorf("stale payload bound %+v, want nothing — a dark domain no longer attests", stale)
	}
	want := Sep1BoundCensus{IssuersWithPayload: 1, IssuersPayloadStale: 1}
	if census != want {
		t.Errorf("stale census = %+v, want %+v — counted in its own bucket, never decoded", census, want)
	}
	census.IssuersWithHomeDomain = 1
	if why := census.Check(); why != "" {
		t.Errorf("stale census does not balance: %s", why)
	}
}

// canonicalAccountStrkey is the shape gate the binding rule runs on.
func TestCanonicalAccountStrkey(t *testing.T) {
	const good = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	cases := []struct {
		in, want string
	}{
		{good, good},
		{"  " + good + "\t", good},
		{lower(good), good},
		{"", ""},
		{good[:55], ""},
		{good + "A", ""},
		{"C" + good[1:], ""},          // a contract strkey is not an account
		{"G1" + good[2:], ""},         // 1 is not in the base32 alphabet
		{"franklintempleton.com", ""}, // a domain is not an account
	}
	for _, c := range cases {
		if got := canonicalAccountStrkey(c.in); got != c.want {
			t.Errorf("canonicalAccountStrkey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// lower spells a strkey in lower case, the way a hand-written toml
// sometimes carries one.
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// An established issuer whose home_domain now serves a different identity
// must surface every changed field — an overwrite must not be blind.
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
