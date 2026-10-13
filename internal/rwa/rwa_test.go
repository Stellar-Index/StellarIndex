package rwa

import (
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAnchorClass_ClosedVocabulary — only the four SEP-1 terms naming a real-world instrument
// normalise; invented synonyms seen in real payloads stay out, because accepting synonyms is how
// a closed set stops being closed.
func TestAnchorClass_ClosedVocabulary(t *testing.T) {
	for _, in := range []string{"stock", "BOND", " commodity ", "RealEstate"} {
		if AnchorClass(in) == "" {
			t.Errorf("AnchorClass(%q) = \"\", want a class", in)
		}
	}
	for _, in := range []string{
		"fiat", "crypto", "nft", "other", "",
		"equity", "etf", "fund", "index", "metal", "rwa", "real_estate", "sovereign", "security",
	} {
		if got := AnchorClass(in); got != "" {
			t.Errorf("AnchorClass(%q) = %q, want \"\"", in, got)
		}
	}
}

// TestAnchorClass_ExcludesFiat pins the exclusion a reader is most likely to undo: a fiat-anchored token is a stablecoin
// counted on its own surface, and folding it in would silently multiply the headline figure.
func TestAnchorClass_ExcludesFiat(t *testing.T) {
	if AnchorClass("fiat") != "" {
		t.Error("fiat normalised to a real-world class — stablecoins are not RWAs on this surface")
	}
	if slices.Contains(AnchorClasses(), "fiat") {
		t.Errorf("AnchorClasses() = %v, must not advertise fiat", AnchorClasses())
	}
}

// TestScamFlagged_ReadsTheOneVocabulary pins that the scam vocabulary is read from the single list shared with the price-withholding gate
// and the listing rank expression, so a tag that withholds a price also keeps an issuer out of this set.
func TestScamFlagged_ReadsTheOneVocabulary(t *testing.T) {
	for _, tag := range timescale.DirectoryScamFlagTags {
		if !ScamFlagged([]string{tag}) {
			t.Errorf("ScamFlagged([%q]) = false — the vocabularies have drifted apart", tag)
		}
		if !ScamFlagged([]string{" " + tag + " "}) {
			t.Errorf("ScamFlagged did not trim %q", tag)
		}
	}
	if ScamFlagged([]string{"issuer", "anchor"}) {
		t.Error("a recognition tag was read as scam-class")
	}
}

// TestQualify_RequiresEveryRequirement walks the four requirements one
// at a time: from an admitted candidate, breaking each requirement on
// its own must refuse it, with the reason naming that requirement.
func TestQualify_RequiresEveryRequirement(t *testing.T) {
	good := Candidate{
		Code:               "USTRY",
		Issuer:             "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		BoundSep1:          true,
		DeclaredAnchorType: "bond",
		DirectoryTags:      []string{"issuer"},
	}
	if v := Qualify(good); !v.InSet || v.Basis != BasisSep1Anchor || v.AnchorClass != "bond" {
		t.Fatalf("baseline candidate refused: %+v", v)
	}

	tests := []struct {
		name   string
		mutate func(Candidate) Candidate
		want   string
	}{
		{"no issuer", func(c Candidate) Candidate { c.Issuer = ""; return c }, RejectNotClassic},
		{"no code", func(c Candidate) Candidate { c.Code = ""; return c }, RejectNotClassic},
		{"no bound sep1 entry", func(c Candidate) Candidate { c.BoundSep1 = false; return c }, RejectNoBoundSep1},
		{"scam-flagged issuer", func(c Candidate) Candidate {
			c.DirectoryTags = []string{"issuer", "malicious"}
			return c
		}, RejectScamFlagged},
		{"issuer not in the directory", func(c Candidate) Candidate { c.DirectoryTags = nil; return c }, RejectNoRecognition},
		{"issuer tagged but not recognised as one", func(c Candidate) Candidate {
			c.DirectoryTags = []string{"personal", "memo-required"}
			return c
		}, RejectNoRecognition},
		{"declares no real-world instrument", func(c Candidate) Candidate {
			c.DeclaredAnchorType = "crypto"
			c.Code = "MEME"
			return c
		}, RejectNoInstrumentClaim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Qualify(tc.mutate(good))
			if v.InSet {
				t.Fatalf("candidate admitted with %s", tc.name)
			}
			if v.Reject != tc.want {
				t.Errorf("reject = %q, want %q", v.Reject, tc.want)
			}
		})
	}
}

// TestQualify_ScamTagBeatsRecognition — an account carrying both is
// refused as flagged. Reporting it as merely unrecognised would
// understate what is known about it.
func TestQualify_ScamTagBeatsRecognition(t *testing.T) {
	v := Qualify(Candidate{
		Code: "USTRY", Issuer: "G1", BoundSep1: true, DeclaredAnchorType: "bond",
		DirectoryTags: []string{"issuer", "anchor", "unsafe"},
	})
	if v.InSet || v.Reject != RejectScamFlagged {
		t.Errorf("verdict = %+v, want refused as %s", v, RejectScamFlagged)
	}
}

// TestQualify_OracleBasisNeverAdmitsOnCodeAlone is the anti-phishing invariant in its sharpest
// form. USTRY, BENJI and XAU are codes an RWA oracle prices, and all three are also issued by
// accounts the directory flags. The oracle basis must not carry any of them past requirement 3.
func TestQualify_OracleBasisNeverAdmitsOnCodeAlone(t *testing.T) {
	for _, code := range []string{"USTRY", "BENJI", "XAU", "xaum", "deJTRSY"} {
		unrecognised := Qualify(Candidate{Code: code, Issuer: "G1", BoundSep1: true})
		if unrecognised.InSet {
			t.Errorf("%s admitted from an unrecognised issuer on the code alone", code)
		}
		flagged := Qualify(Candidate{
			Code: code, Issuer: "G1", BoundSep1: true,
			DirectoryTags: []string{"issuer", "malicious"},
		})
		if flagged.InSet {
			t.Errorf("%s admitted from a scam-flagged issuer", code)
		}
		ok := Qualify(Candidate{
			Code: code, Issuer: "G1", BoundSep1: true, DirectoryTags: []string{"issuer"},
		})
		if !ok.InSet || ok.Basis != BasisOracleFeed {
			t.Errorf("%s from a recognised issuer: %+v, want the oracle basis", code, ok)
		}
		if ok.AnchorClass != "" {
			t.Errorf("%s got anchor_class %q — an oracle feed names an instrument, not a class",
				code, ok.AnchorClass)
		}
	}
}

// TestCouldQualify_MatchesQualifyOnTheAssetSideInputs pins that the storage pre-filter never drops an entry [Qualify] would admit,
// since a narrower pre-filter is a silent membership change. The matrix spans every asset-side input a requirement-4 arm reads:
// code (oracle arm), anchor type (class arm) and anchor asset (ISIN arm).
func TestCouldQualify_MatchesQualifyOnTheAssetSideInputs(t *testing.T) {
	codes := []string{"USTRY", "BENJI", "XAU", "XAUm", "MEME", "USDC", "", "LNGV", "BB1"}
	types := []string{"bond", "stock", "commodity", "realestate", "fiat", "crypto", "nft", "other", ""}
	anchors := []string{
		"LU2900381208", // gBENJI, valid ISIN
		"SGXZ71843866", // sgBENJI, valid ISIN
		"US0378331005", // Apple, valid ISIN
		"LU2900381209", // wrong check digit
		"FOBXX",        // a ticker, not an ISIN
		"",
	}
	issuers := []string{
		"G1",
		"GAHOGWBAWNIKESGNNW7Y7JU5KL54HIEHJGY6Y5QLY6YR3J7WZIDHLC6D", // LNGV fund-NAV binding
		"GD5J6HLF5666X4AZLTFTXLY46J5SW7EXRKBLEYPJP33S33MXZGV6CWFN", // BB1 vetted ISIN
	}
	for _, issuer := range issuers {
		for _, code := range codes {
			for _, typ := range types {
				for _, anchor := range anchors {
					admitted := Qualify(Candidate{
						Code: code, Issuer: issuer, BoundSep1: true,
						DeclaredAnchorType:  typ,
						DeclaredAnchorAsset: anchor,
						DirectoryTags:       []string{"issuer"},
					}).InSet
					if admitted && !CouldQualify(code, issuer, typ, anchor) {
						t.Errorf("CouldQualify(%q, %q, %q, %q) = false but Qualify admits it", code, issuer, typ, anchor)
					}
				}
			}
		}
	}
}

// TestCouldQualify_KeepsAnISINDeclaredEntryWithNoClass names a shape the pre-filter once dropped: a code no oracle prices,
// type `other`, and a well-formed ISIN in anchor_asset.
func TestCouldQualify_KeepsAnISINDeclaredEntryWithNoClass(t *testing.T) {
	for _, c := range []struct{ code, isin string }{
		{"gBENJI", "LU2900381208"},
		{"grBENJI", "LU3258450587"},
		{"sgBENJI", "SGXZ71843866"},
	} {
		if !CouldQualify(c.code, "G1", "other", c.isin) {
			t.Errorf("CouldQualify(%q, other, %s) = false: the ISIN arm is unreachable through the pre-filter", c.code, c.isin)
		}
	}
	if CouldQualify("gBENJI", "G1", "other", "LU2900381209") {
		t.Error("a malformed ISIN (bad check digit) must not pass the pre-filter on the ISIN arm")
	}
	if CouldQualify("MEME", "G1", "other", "") {
		t.Error("type other with no anchor asset and no oracle code must still be filtered")
	}
}

func TestQualify_FundNAVBindingAdmitsOnlyItsOwnIssuer(t *testing.T) {
	const lngvIssuer = "GAHOGWBAWNIKESGNNW7Y7JU5KL54HIEHJGY6Y5QLY6YR3J7WZIDHLC6D"
	c := Candidate{Code: "LNGV", Issuer: lngvIssuer, BoundSep1: true, DirectoryTags: []string{"issuer"}}
	if v := Qualify(c); !v.InSet || v.Basis != BasisOracleFeed {
		t.Fatalf("bound LNGV refused: %+v", v)
	}
	c.Issuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	if v := Qualify(c); v.InSet || v.Reject != RejectNoInstrumentClaim {
		t.Fatalf("LNGV under another issuer admitted: %+v", v)
	}
	c.Issuer, c.DirectoryTags = lngvIssuer, nil
	if v := Qualify(c); v.InSet || v.Reject != RejectNoRecognition {
		t.Fatalf("unrecognised LNGV admitted: %+v", v)
	}
}

// Franklin Templeton's Luxembourg and Singapore share classes are declared with ISINs in
// franklintempleton.com's own SEP-1, on issuer accounts the curated directory never listed.
// A sibling on the same bound domain recognises them, and the verdict says which arm did it.
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

func TestConstantNAV_IsExactOnBothHalvesAndWellFormed(t *testing.T) {
	b, ok := ConstantNAV("gBENJI", franklinLuxIBIssuer)
	if !ok || b.ISIN != "LU2900381208" || b.NAVUSD != "1.00" {
		t.Fatalf("gBENJI binding = %+v, %v", b, ok)
	}
	if _, ok := ConstantNAV("gBENJI", "GAIMPOSTOR"); ok {
		t.Error("a code alone must not bind: an impostor would be valued at par")
	}
	if _, ok := ConstantNAV("GBENJI", franklinLuxIBIssuer); ok {
		t.Error("codes are case-exact")
	}
	for _, b := range ConstantNAVBindings() {
		if !IsISIN(b.ISIN) {
			t.Errorf("%s: ISIN %q malformed", b.Code, b.ISIN)
		}
		if _, ok := new(big.Rat).SetString(b.NAVUSD); !ok {
			t.Errorf("%s: NAV %q is not a decimal", b.Code, b.NAVUSD)
		}
		if b.Source == "" || b.VerifiedOn == "" || b.Regime == "" {
			t.Errorf("%s: a constant NAV must name its source, regime and verification date", b.Code)
		}
	}
	// An accumulating class is deliberately absent.
	if _, ok := ConstantNAV("sgBENJI", "GAGICV3VBJSKKH5H5MQQIUTUP462YVHC23KUHZY6FJERRJFBDIVZBM5C"); ok {
		t.Error("sgBENJI accumulates; its NAV is a measurement and must not be bound as a constant")
	}
}

// TestConstantNAV_EveryBindingIsEvidencedAndBounded pins that each binding cites its OWN share class's page:
// the source URL must name the binding's own ISIN, and the review bound derives from the date that page was read.
func TestConstantNAV_EveryBindingIsEvidencedAndBounded(t *testing.T) {
	bindings := ConstantNAVBindings()
	if len(bindings) == 0 {
		t.Fatal("no bindings: the table under test is empty")
	}
	for _, b := range bindings {
		if b.Source == "" || b.ISIN == "" || b.VerifiedOn == "" || b.ReviewBy == "" {
			t.Errorf("%s: source %q, ISIN %q, verified %q, review by %q — every one must be set", b.Code, b.Source, b.ISIN, b.VerifiedOn, b.ReviewBy)
			continue
		}
		if !strings.HasPrefix(b.Source, "https://") {
			t.Errorf("%s: source %q is not an https URL", b.Code, b.Source)
		}
		if !strings.Contains(b.Source, b.ISIN) {
			t.Errorf("%s: source %q does not name the binding's own ISIN %s — it is evidence for a different share class", b.Code, b.Source, b.ISIN)
		}
		verified, err := time.Parse(reviewDateLayout, b.VerifiedOn)
		if err != nil {
			t.Errorf("%s: VerifiedOn %q: %v", b.Code, b.VerifiedOn, err)
			continue
		}
		reviewBy := b.ReviewDeadline()
		if reviewBy.IsZero() {
			t.Errorf("%s: ReviewBy %q does not parse", b.Code, b.ReviewBy)
			continue
		}
		if !reviewBy.After(verified) {
			t.Errorf("%s: ReviewBy %s is not after VerifiedOn %s", b.Code, b.ReviewBy, b.VerifiedOn)
		}
		if got := reviewBy.Sub(verified); got != ConstantNAVReviewInterval {
			t.Errorf("%s: ReviewBy − VerifiedOn = %s, want the documented interval %s", b.Code, got, ConstantNAVReviewInterval)
		}
	}
	// The sibling classes are keyed to different pages: a copy-paste of
	// one class's URL onto the other is exactly the defect above.
	seen := map[string]string{}
	for _, b := range bindings {
		if other, dup := seen[b.Source]; dup {
			t.Errorf("%s and %s cite the same page %s", other, b.Code, b.Source)
		}
		seen[b.Source] = b.Code
	}
}

// The review bound flips exactly once, at the deadline: standing through
// the instant the interval ends, due from the first instant after it.
func TestConstantNAV_ReviewDueFlipsAtTheDeadline(t *testing.T) {
	b, ok := ConstantNAV("gBENJI", franklinLuxIBIssuer)
	if !ok {
		t.Fatal("gBENJI binding missing")
	}
	if b.VerifiedOn != "2026-09-16" || b.ReviewBy != "2026-12-15" {
		t.Fatalf("gBENJI verified %s review by %s, want 2026-09-16 → 2026-12-15 (90 days)", b.VerifiedOn, b.ReviewBy)
	}
	deadline := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	if got := b.ReviewDeadline(); !got.Equal(deadline) {
		t.Fatalf("ReviewDeadline = %s, want %s", got, deadline)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		due  bool
	}{
		{"the day it was verified", time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC), false},
		{"the day before the deadline", deadline.Add(-24 * time.Hour), false},
		{"one second before", deadline.Add(-time.Second), false},
		{"the deadline itself", deadline, false},
		{"one second after", deadline.Add(time.Second), true},
		{"a year later", deadline.AddDate(1, 0, 0), true},
	} {
		if got := b.ReviewDue(tc.now); got != tc.due {
			t.Errorf("%s (%s): due = %v, want %v", tc.name, tc.now.Format(time.RFC3339), got, tc.due)
		}
	}
	// A binding whose bound cannot be read is due, not indefinitely at
	// par: the failure mode is a label, never a silent constant.
	broken := ConstantNAVBinding{Code: "x", ReviewBy: "not a date"}
	if !broken.ReviewDue(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("an unreadable ReviewBy must fail closed as due")
	}
}

// The table's own ISINs are keys: well-formed, upper-case and each on
// exactly one pair, or the process refuses to start.
func TestIndexConstantNAV_RefusesAnAmbiguousTable(t *testing.T) {
	ib := ConstantNAVBinding{Code: "gBENJI", Issuer: franklinLuxIBIssuer, ISIN: "LU2900381208", NAVUSD: "1.00", VerifiedOn: "2026-09-16"}
	ab := ConstantNAVBinding{Code: "grBENJI", Issuer: franklinLuxABIssuer, ISIN: "LU3258450587", NAVUSD: "1.00", VerifiedOn: "2026-09-16"}
	with := func(b ConstantNAVBinding, f func(*ConstantNAVBinding)) ConstantNAVBinding { f(&b); return b }
	cases := map[string][]ConstantNAVBinding{
		"one ISIN on two issuers": {ib, with(ab, func(b *ConstantNAVBinding) { b.ISIN = ib.ISIN })},
		"one pair bound twice":    {ib, with(ab, func(b *ConstantNAVBinding) { b.Code, b.Issuer = ib.Code, ib.Issuer })},
		"lower-case ISIN":         {with(ib, func(b *ConstantNAVBinding) { b.ISIN = "lu2900381208" })},
		"bad check digit":         {with(ib, func(b *ConstantNAVBinding) { b.ISIN = "LU2900381209" })},
		"unparseable date":        {with(ib, func(b *ConstantNAVBinding) { b.VerifiedOn = "16/09/2026" })},
	}
	for name, bindings := range cases {
		if _, err := indexConstantNAV(bindings); err == nil {
			t.Errorf("%s: indexed without error", name)
		}
	}
	good := []ConstantNAVBinding{ib, ab}
	tbl, err := indexConstantNAV(good)
	if err != nil {
		t.Fatalf("a well-formed table: %v", err)
	}
	if tbl.byISIN["LU3258450587"].Code != "grBENJI" || good[0].ReviewBy != "2026-12-15" {
		t.Errorf("index = %+v, bindings = %+v", tbl, good)
	}
}

// A declared ISIN contradicts the table when the pair is bound to
// another class or the ISIN is bound to another pair; a matching or
// non-ISIN declaration contradicts nothing.
func TestConstantNAVISINConflict(t *testing.T) {
	cases := []struct {
		code, issuer, declared string
		want                   bool
	}{
		{"gBENJI", franklinLuxIBIssuer, "LU2900381208", false},
		{"gBENJI", franklinLuxIBIssuer, " lu2900381208 ", false},
		{"gBENJI", franklinLuxIBIssuer, "LU3258450587", true},
		{"gBENJI", franklinLuxIBIssuer, "US0378331005", true},
		{"gBENJI", franklinLuxIBIssuer, "LU2900381209", false},
		{"gBENJI", franklinLuxIBIssuer, "Franklin OnChain Fund", false},
		{"gBENJI", "GAIMPOSTOR", "LU2900381208", true},
		{"FAKE", franklinLuxIBIssuer, "LU2900381208", true},
		{"FAKE", "GAIMPOSTOR", "US0378331005", false},
	}
	for _, c := range cases {
		if got := ConstantNAVISINConflict(c.code, c.issuer, c.declared); got != c.want {
			t.Errorf("ConstantNAVISINConflict(%q, %q, %q) = %v, want %v", c.code, c.issuer, c.declared, got, c.want)
		}
	}
}

// Fixtures: the two ISINs Franklin Templeton declares for its Stellar share classes, and the shapes an `anchor_asset`
// takes when prose fills it. Every positive fixture's check digit was reproduced by a separate ISO 6166 implementation,
// since an expected value from the code under test asserts nothing.
func TestIsISIN(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		// Live declarations, read from Franklin Templeton's SEP-1 on
		// These are the assets this arm exists to admit.
		{"LU2900381208", true, "gBENJI's declared anchor_asset"},
		{"LU3258450587", true, "grBENJI's declared anchor_asset"},
		// Published ISINs with known-good check digits.
		{"US0378331005", true, "a US equity"},
		{"GB0002634946", true, "a GB security"},
		{"XS1748480172", true, "a supranational prefix is a valid country code"},
		// Lower case is accepted: an issuer's own file is not normalised.
		{"lu2900381208", true, "case is the declarer's, not the standard's"},
		{"  LU2900381208  ", true, "surrounding whitespace is the file's, not the value's"},

		// The whole point of checking the digit: one transposition and
		// it is a different security or no security at all.
		{"LU2900381209", false, "check digit off by one"},
		{"LU2900318208", false, "two body digits transposed"},
		{"US0378331004", false, "check digit off by one"},

		// What anchor_asset contains when it is not an identifier. Every
		// one of these is a real shape from the wild.
		{"", false, "declared nothing"},
		{"FOBXX", false, "a fund ticker, not an ISIN"},
		{"US Treasury Notes", false, "prose"},
		{"gold", false, "prose"},
		{"LU29003812", false, "too short"},
		{"LU29003812080", false, "too long"},
		{"1U2900381208", false, "first prefix character is not a letter"},
		{"L12900381208", false, "second prefix character is not a letter"},
		{"LU290038120X", false, "check position is not a digit"},
		{"LU-900381208", false, "non-alphanumeric in the body"},
		// Unicode case folding must not manufacture an ISIN: these
		// upper-case to US0378331005 and IE00B4L5Y983, but the served
		// anchor_asset would be the non-ASCII string nobody can look up.
		{"Uſ0378331005", false, "long s (U+017F) is not the letter S"},
		{"ıE00B4L5Y983", false, "dotless i (U+0131) is not the letter I"},
		{"IE00B4L5Y983", true, "the ASCII original of the case above"},
		{"DE0007164600", true, "check digit 0: the mod-10 complement wraps to 0, not 10"},
		{"au000000bhp4", true, "a lower-case 'a' folds like every other letter"},
		// Luhn-valid bodies, so only the prefix rule can refuse them.
		{"L12900381204", false, "second prefix character is a digit, check digit valid"},
		{"1U2900381202", false, "first prefix character is a digit, check digit valid"},
	} {
		if got := IsISIN(tc.in); got != tc.want {
			t.Errorf("IsISIN(%q) = %v, want %v — %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// The arm admits, and it admits WITHOUT a class. Publishing one would
// state a classification the issuer did not make.
func TestISINArmAdmitsWithoutInventingAClass(t *testing.T) {
	v := Qualify(Candidate{
		Code: "gBENJI", Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		BoundSep1:           true,
		DeclaredAnchorType:  "other",
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	})
	if !v.InSet {
		t.Fatalf("refused: %s", v.Reject)
	}
	if v.Basis != BasisSep1ISIN {
		t.Errorf("basis = %q, want %q", v.Basis, BasisSep1ISIN)
	}
	if v.AnchorClass != "" {
		t.Errorf("anchor_class = %q, want empty — an ISIN names an instrument, not a class", v.AnchorClass)
	}
}

// A declared class still wins: it carries strictly more information, so
// an asset that can be admitted with one must not fall through to the
// arm that cannot say what it is.
func TestDeclaredClassOutranksTheISINArm(t *testing.T) {
	v := Qualify(Candidate{
		Code: "USTRY", Issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		BoundSep1:           true,
		DeclaredAnchorType:  "bond",
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	})
	if v.Basis != BasisSep1Anchor {
		t.Errorf("basis = %q, want %q — a declared class is more informative", v.Basis, BasisSep1Anchor)
	}
	if v.AnchorClass != "bond" {
		t.Errorf("anchor_class = %q, want bond", v.AnchorClass)
	}
}

// THE GUARD: the ISIN arm relaxes R4, not R2 or R3. An ISIN is the issuer's own claim, checked for form only,
// so it must never carry a candidate past the requirements that bind the identity.
func TestISINNeverBypassesBindingOrRecognition(t *testing.T) {
	base := Candidate{
		Code: "gBENJI", Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		BoundSep1:           true,
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	}
	unbound := base
	unbound.BoundSep1 = false
	if v := Qualify(unbound); v.InSet || v.Reject != RejectNoBoundSep1 {
		t.Errorf("unbound SEP-1: got in_set=%v reject=%q, want refusal at R2", v.InSet, v.Reject)
	}

	unrecognised := base
	unrecognised.DirectoryTags = nil
	if v := Qualify(unrecognised); v.InSet || v.Reject != RejectNoRecognition {
		t.Errorf("unrecognised issuer: got in_set=%v reject=%q, want refusal at R3", v.InSet, v.Reject)
	}

	flagged := base
	flagged.DirectoryTags = []string{"issuer", "scam"}
	if v := Qualify(flagged); v.InSet || v.Reject != RejectScamFlagged {
		t.Errorf("scam-flagged issuer: got in_set=%v reject=%q, want refusal as flagged", v.InSet, v.Reject)
	}
}

// Prose in anchor_asset is the ordinary case and must still be refused,
// or the arm would admit every recognised issuer that filled the field
// in at all.
func TestProseAnchorAssetIsStillRefused(t *testing.T) {
	v := Qualify(Candidate{
		Code: "AUMTL", Issuer: "GACKTN5DAZGWXRWB2WLM6OPBDHAMT6SJNGLJZPQMEZBUX2UDX2UK7VXX",
		BoundSep1:           true,
		DeclaredAnchorType:  "other",
		DeclaredAnchorAsset: "AU",
		DirectoryTags:       []string{"issuer"},
	})
	if v.InSet {
		t.Fatalf("admitted on prose anchor_asset %q with basis %q", "AU", v.Basis)
	}
	if v.Reject != RejectNoInstrumentClaim {
		t.Errorf("reject = %q, want %q", v.Reject, RejectNoInstrumentClaim)
	}
}

// Every ISIN comparison keys on one form, so a declaration differing only
// in case or surrounding space names the same security.
func TestCanonicalISIN(t *testing.T) {
	for in, want := range map[string]string{
		"LU2900381208":    "LU2900381208",
		" lu2900381208\t": "LU2900381208",
		"LU2900381209":    "",
		"FOBXX":           "",
	} {
		got, ok := CanonicalISIN(in)
		if got != want || ok != (want != "") {
			t.Errorf("CanonicalISIN(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// TestInstrumentFeed_BindsThePairNotTheCode is the identity rule: asset codes are not unique on Stellar, so a code-keyed binding
// would give any issuer of a token called USTRY the real instrument's net asset value, a false financial claim.
func TestInstrumentFeed_BindsThePairNotTheCode(t *testing.T) {
	const impostor = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"

	if feed, ok := InstrumentFeed("USTRY", etherfuseIssuer); !ok || feed != "USTRY" {
		t.Fatalf("InstrumentFeed(USTRY, bound issuer) = %q/%v, want USTRY/true", feed, ok)
	}
	if feed, ok := InstrumentFeed("USTRY", impostor); ok {
		t.Errorf("InstrumentFeed(USTRY, unrelated issuer) = %q/true — the code is not the identity", feed)
	}
	if _, ok := InstrumentFeed("USTRY", ""); ok {
		t.Error("an empty issuer must bind nothing")
	}
	if _, ok := InstrumentFeed("", etherfuseIssuer); ok {
		t.Error("an empty code must bind nothing")
	}
}

// TestInstrumentFeed_DoesNotFoldCase pins that a case variant is a different token: a token coded XAUM must not match the XAUm feed
// unless a binding says so.
func TestInstrumentFeed_DoesNotFoldCase(t *testing.T) {
	for _, code := range []string{"ustry", "Ustry", "uSTRY"} {
		if feed, ok := InstrumentFeed(code, etherfuseIssuer); ok {
			t.Errorf("InstrumentFeed(%q, bound issuer) = %q/true — case variants are distinct tokens", code, feed)
		}
	}
	// Whitespace is never part of an identity, so it is trimmed.
	if _, ok := InstrumentFeed("  USTRY  ", " "+etherfuseIssuer+" "); !ok {
		t.Error("surrounding whitespace must not defeat a binding")
	}
}

// TestNoBindingTargetsAnOffChainReference pins that no binding targets a feed pricing a different quantity (a troy ounce of spot metal,
// one fund share), which would publish a unit conversion as a premium.
func TestNoBindingTargetsAnOffChainReference(t *testing.T) {
	for _, b := range instrumentBindings {
		if OffChainReferenceCode(b.Feed) {
			t.Errorf("%s-%s is bound to %s, which prices an off-chain quantity in its own unit, "+
				"not one token", b.Code, b.Issuer, b.Feed)
		}
	}
}

// TestBindingsTargetAllowListedFeeds. A binding whose feed ADR-0028 does
// not recognise can never match a stored oracle row — it is dead weight
// that reads as coverage.
func TestBindingsTargetAllowListedFeeds(t *testing.T) {
	known := map[string]struct{}{}
	for _, c := range canonical.KnownRWACodes() {
		known[c] = struct{}{}
	}
	for _, b := range instrumentBindings {
		if _, ok := known[b.Feed]; !ok {
			t.Errorf("%s-%s binds feed %q, which is not an ADR-0028 code", b.Code, b.Issuer, b.Feed)
		}
	}
	for c := range offChainReferenceCodes {
		if _, ok := known[c]; !ok {
			t.Errorf("%s is classified as an off-chain reference but is not an ADR-0028 code", c)
		}
	}
}

// TestBindingsAreWellFormed. Every entry names a real Stellar issuing
// account and a non-empty code, and no pair is bound twice — a duplicate
// pair would make the served set disagree with the lookup index.
func TestBindingsAreWellFormed(t *testing.T) {
	seen := map[instrumentKey]struct{}{}
	for _, b := range instrumentBindings {
		if strings.TrimSpace(b.Code) == "" {
			t.Errorf("binding with an empty code: %+v", b)
		}
		if len(b.Issuer) != 56 || !strings.HasPrefix(b.Issuer, "G") {
			t.Errorf("binding issuer %q is not a Stellar account G-strkey", b.Issuer)
		}
		k := instrumentKey{code: b.Code, issuer: b.Issuer}
		if _, dup := seen[k]; dup {
			t.Errorf("%s-%s is bound more than once", b.Code, b.Issuer)
		}
		seen[k] = struct{}{}
	}
	if len(bindingIndex) != len(instrumentBindings) {
		t.Errorf("lookup index holds %d of %d bindings", len(bindingIndex), len(instrumentBindings))
	}
}

// TestInstrumentBindings_Served: the curated set is published so a consumer can audit every
// pair this surface will compare. Feeds are served in canonical `rwa:` form so the id goes
// straight to the oracle endpoints.
func TestInstrumentBindings_Served(t *testing.T) {
	got := InstrumentBindings()
	if len(got) != len(instrumentBindings) {
		t.Fatalf("InstrumentBindings() returned %d of %d", len(got), len(instrumentBindings))
	}
	for i, b := range got {
		if !strings.HasPrefix(b.Feed, "rwa:") {
			t.Errorf("feed %q is not a canonical asset id", b.Feed)
		}
		if _, err := canonical.ParseAsset(b.Feed); err != nil {
			t.Errorf("served feed %q does not parse: %v", b.Feed, err)
		}
		if i > 0 && got[i-1].Code > b.Code {
			t.Fatalf("InstrumentBindings() not ordered: %v", got)
		}
	}
}

// TestSpotGoldIsNotComparableToAToken pins the case the off-chain classification exists for: `rwa:XAU` is the FX oracle's spot-gold slot
// (one troy ounce), while a Stellar token coded XAU is of unstated size.
func TestSpotGoldIsNotComparableToAToken(t *testing.T) {
	if !OffChainReferenceCode("XAU") {
		t.Error("XAU must be reported as an off-chain reference so the refusal has a stated reason")
	}
	if OffChainReferenceCode("XAUm") {
		t.Error("XAUm is a tokenized instrument, not an off-chain reference")
	}
	if OffChainReferenceCode("NOTANRWA") || OffChainReferenceCode("") {
		t.Error("an unrecognised code is not an off-chain reference")
	}
}

// TestFundNAVBindings_AllTwelveResolve pins the WisdomTree fund table to
// the pairs the issuer's SEP-1 declares: each resolves to its ticker on
// the exact (code, issuer) and on nothing looser.
func TestFundNAVBindings_AllTwelveResolve(t *testing.T) {
	want := map[string]string{
		"WTTS-GBBV5CF7UPA2PYRPA632URLB55BWML7X4H33ZRCDWMTULOXDGPHJR5VI": "WTTSX",
		"WTST-GDEBI5X7J4IDXCSVV3KPFZIHQRCBVF3DAZMS5H7KYOBK45T6XYGDE77P": "WTSTX",
		"FLTT-GBTZKH3RNKW46XEZNCGZEBAGJISKDZKQXKSQ2N5G5SFX36TLWKKR6QJ6": "FLTTX",
		"WTLG-GAK7PE7DD4ZRJQN3VBCQFBKFV53JGUM2SQATQAKLFK6MVONPGNYK34XH": "WTLGX",
		"WTSI-GAD22PDBRFEMXAKPFDP4JGDFWKKD6VPXWUWEAXBS6ZYJYFFQDUN7HAFG": "WTSIX",
		"WTSY-GB3ZUC7FGDEEBXY3BDEJWMPNGBFA66YRI4QQT6PBO3ZT6F33S7RL36VF": "WTSYX",
		"TIPS-GAJ4KSYLVBJKQ4UBPKJJXPYWVIRZWVTIYRMHBXTHGCDS4XJXXYEUALVD": "TIPSX",
		"EQTY-GAKODZFS4MV36JGDTULJACWJKBJCO33CJTVTWSQFSUV7XLZJNXTDH6D6": "EQTYX",
		"LNGV-GAHOGWBAWNIKESGNNW7Y7JU5KL54HIEHJGY6Y5QLY6YR3J7WZIDHLC6D": "LNGVX",
		"MODR-GANULT25TFO6V6BFWSEG4VSCR4QXBNHV5T344R2AFZEPE6B324LVLOOJ": "MODRX",
		"SPXU-GDJBVX3QA5HJPBSAU5VIX2W6MC37NU4UFXPKEGK42SJCYN6AEQ4Z6COM": "SPXUX",
		"TECH-GDSAW27GPR7EWKPTFDPGN2WWZYUHBFKVDBLOUUEKSNKHID4ZWUVOBF5R": "TECHX",
	}
	if len(fundNAVBindings) != len(want) || len(fundNAVIndex) != len(want) {
		t.Fatalf("fund table holds %d bindings (index %d), want %d", len(fundNAVBindings), len(fundNAVIndex), len(want))
	}
	for id, ticker := range want {
		code, issuer, _ := strings.Cut(id, "-")
		if !strkey.IsValidEd25519PublicKey(issuer) {
			t.Errorf("%s: issuer is not a valid G-strkey", id)
		}
		if got, ok := FundNAVTicker(code, " "+issuer+" "); !ok || got != ticker {
			t.Errorf("FundNAVTicker(%s) = %q, %v; want %q", id, got, ok, ticker)
		}
		if _, ok := FundNAVTicker(code, "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"); ok {
			t.Errorf("%s resolves under a foreign issuer", code)
		}
		if _, ok := FundNAVTicker(strings.ToLower(code), issuer); ok {
			t.Errorf("%s resolves case-folded", code)
		}
	}
	tickers := FundNAVTickers()
	if !slices.IsSorted(tickers) || len(tickers) != len(want) {
		t.Errorf("FundNAVTickers() = %v; want %d sorted tickers", tickers, len(want))
	}
}

// The vetted table admits exactly its pair, without a class, only once R2 and R3 stand, and
// never ahead of the issuer's own live declaration.
func TestQualify_VettedISINAdmitsOnlyItsOwnPair(t *testing.T) {
	c := Candidate{Code: "BB1", Issuer: testBB1Issuer, BoundSep1: true, DirectoryTags: []string{"anchor", "issuer"}}
	v := Qualify(c)
	if !v.InSet || v.Basis != BasisSep1ISINVetted || v.AnchorClass != "" || v.Recognition != RecognitionDirectory {
		t.Fatalf("bound BB1: %+v, want admitted under %s with no class", v, BasisSep1ISINVetted)
	}
	if !CouldQualify(c.Code, c.Issuer, "", "") {
		t.Fatal("the pre-filter drops the vetted pair before Qualify sees it")
	}
	c.DeclaredAnchorAsset = "DE000A2TR7Q2"
	if v := Qualify(c); v.Basis != BasisSep1ISIN {
		t.Fatalf("declared ISIN beside the vetted one: basis %s, want %s", v.Basis, BasisSep1ISIN)
	}
	c.DeclaredAnchorAsset = ""
	c.Issuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	if v := Qualify(c); v.InSet || v.Reject != RejectNoInstrumentClaim {
		t.Fatalf("BB1 under another issuer admitted: %+v", v)
	}
	if CouldQualify(c.Code, c.Issuer, "", "") {
		t.Fatal("BB1 under another issuer passes the pre-filter")
	}
	c.Issuer, c.Code = testBB1Issuer, "BB2"
	if v := Qualify(c); v.InSet {
		t.Fatalf("another code on the vetted issuer admitted: %+v", v)
	}
	c.Code, c.DirectoryTags = "BB1", nil
	if v := Qualify(c); v.InSet || v.Reject != RejectNoRecognition {
		t.Fatalf("unrecognised BB1 admitted: %+v", v)
	}
}
