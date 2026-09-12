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

	// Each of the three dropped entries lands in its OWN bucket. Before
	// the census they shared one fate — a bare `continue` — so a payload
	// of four entries and a payload of four thousand looked identical
	// from outside.
	want := Sep1BoundCensus{
		IssuersDeclaring: 1, Entries: 4,
		EntriesMissingCode: 1, EntriesMissingIssuer: 1, EntriesNamingAnotherIssuer: 1,
		EntriesBound: 1, EntriesKept: 1,
	}
	if census != want {
		t.Errorf("census = %+v, want %+v", census, want)
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

// One malformed payload must not empty the whole result: the scan
// walks every issuer, and a single corrupt toml is a data fact about
// that issuer, not an outage of the surface. It IS counted, because a
// payload that would not decode and an issuer that declares nothing are
// different findings with different owners.
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

// A payload that decodes but declares no [[CURRENCIES]] is a different
// finding from one that would not decode, and both are different from
// an issuer whose toml was never fetched. Each gets its own bucket
// because each has a different owner and a different fix.
func TestBoundSep1CurrenciesFromPayload_DeclaringNothingIsItsOwnBucket(t *testing.T) {
	got, census := boundSep1CurrenciesFromPayload("G1", "example.com", `{"OrgName":"Example"}`, nil)
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
	if census.IssuersDeclaringNothing != 1 || census.IssuersPayloadUnreadable != 0 || census.IssuersDeclaring != 0 {
		t.Errorf("census = %+v, want exactly one issuer in the declares-nothing bucket", census)
	}
}

// The binding rule compares ACCOUNTS, not the bytes a toml happened to
// carry. Surrounding space and the case an issuer typed its own strkey
// in do not change which account it is — a strkey is base32, so two
// spellings that differ only in case decode to the same 32 bytes.
//
// This is a canonicalisation, never a relaxation: the test below pins
// that a DIFFERENT account is still refused, and that the entry is
// carried under the canonical database spelling rather than the toml's.
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

// A whitespace-only code names no asset. It used to pass the empty
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

// The domain-bearing population splits into the issuers holding a
// payload, the issuers a fetch reached that hold none, and — as a
// REMAINDER — the ones nothing has tried yet. The first two are counted
// by different queries, so they can contradict each other, and the
// surface that publishes them clamps the split to keep its arithmetic
// printable. Check is what stops that clamp turning an impossible
// census into a sound-looking one.
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
