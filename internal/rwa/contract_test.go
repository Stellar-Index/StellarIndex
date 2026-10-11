package rwa_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The contract arm of the definition.
//
// The arm exists because impersonators are the norm: many issuers publish a classic token called
// BENJI and none is the real one. Any of them could deploy a Soroban token contract declaring the
// same symbol at no cost, so the tests pin that such a contract is refused and pin the specific
// thing an attacker would have to control instead.

// Real C-strkeys, CRC-valid. They are test fixtures and stand for
// nothing on the network — the arm is about the SHAPE of the evidence,
// and using a real issuer's real contract address as a fixture would be
// the fabricated-identity problem this file exists to prevent.
const (
	contractA = "CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM"
	contractB = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// TestQualifyContract_RefusesAContractNobodyNamed is the whole arm in
// one case. A contract calling itself BENJI, with a symbol an
// independent oracle really does price, and no third party naming the
// address: refused. This is the impersonator shape; if it ever passes, any
// impersonator can put a large valuation on the page by deploying one contract.
func TestQualifyContract_RefusesAContractNobodyNamed(t *testing.T) {
	// Every OTHER input is the strongest an attacker could arrange; only the
	// naming of this exact address is missing. So if C2 is removed or weakened
	// the candidate is ADMITTED rather than refused for a different reason,
	// which is what the assertion below catches.
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     contractA,
		DirectoryNamed: false,
		DirectoryTags:  []string{"issuer"},
		Symbol:         "BENJI",
	})
	if v.InSet {
		t.Fatalf("admitted a contract no independent party named (basis %q) — "+
			"an attacker needs only to deploy a contract and pick a symbol", v.Basis)
	}
	if v.Reject != rwa.RejectContractNotNamed {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractNotNamed)
	}
}

// TestQualifyContract_SymbolAloneNeverAdmits pins the ordering that
// makes the symbol safe to read at all. Directory tags present but not
// issuing-class, symbol allow-listed: still refused, and refused on the
// TAG rather than on the symbol.
func TestQualifyContract_SymbolAloneNeverAdmits(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     contractA,
		DirectoryNamed: true,
		DirectoryTags:  []string{"wallet", "memo-required"},
		Symbol:         "USTRY",
	})
	if v.InSet {
		t.Fatalf("admitted on a symbol with no issuing recognition (basis %q)", v.Basis)
	}
	if v.Reject != rwa.RejectContractNoTag {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractNoTag)
	}
}

// TestQualifyContract_RefusesInfrastructureTags is the reason the
// contract recognition vocabulary is narrower than the account one.
//
// The curated directory tags AMM pools and protocol routers `defi` and `exchange`. On an ACCOUNT
// those describe an entity that might issue a real-world instrument; on a CONTRACT they describe
// infrastructure that issues nothing. Admitting them would list liquidity pool shares as
// real-world assets.
func TestQualifyContract_RefusesInfrastructureTags(t *testing.T) {
	for _, tag := range []string{"defi", "exchange", "sdf", "wallet", "application"} {
		t.Run(tag, func(t *testing.T) {
			v := rwa.QualifyContract(rwa.ContractCandidate{
				ContractID:     contractA,
				DirectoryNamed: true,
				DirectoryTags:  []string{tag},
				Symbol:         "XAUm",
			})
			if v.InSet {
				t.Fatalf("tag %q admitted a contract as a real-world asset issuer", tag)
			}
			if v.Reject != rwa.RejectContractNoTag {
				t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractNoTag)
			}
		})
	}
	// And the account vocabulary really is wider, so the narrowing above
	// is a decision this arm makes rather than an accident of both lists
	// happening to be equal.
	if !rwa.HasRecognitionTag([]string{"defi"}) {
		t.Error("HasRecognitionTag rejects `defi` — the two vocabularies are equal, so the contract narrowing is not being tested")
	}
	if rwa.HasContractRecognitionTag([]string{"defi"}) {
		t.Error("HasContractRecognitionTag accepts `defi`")
	}
}

// TestQualifyContract_ScamTagBeatsRecognition pins the precedence the
// classic arm already applies: an address carrying BOTH an issuing tag
// and a scam-class tag is refused as flagged.
func TestQualifyContract_ScamTagBeatsRecognition(t *testing.T) {
	for _, scam := range []string{"malicious", "unsafe", "fraud", "scam", "hack", "phishing"} {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:     contractA,
			DirectoryNamed: true,
			DirectoryTags:  []string{"issuer", scam},
			Symbol:         "BENJI",
		})
		if v.InSet {
			t.Fatalf("tag %q admitted alongside `issuer`", scam)
		}
		if v.Reject != rwa.RejectContractScam {
			t.Errorf("tag %q: reject = %q, want %q", scam, v.Reject, rwa.RejectContractScam)
		}
	}
}

// TestQualifyContract_RecognisedButNoInstrumentIsRefused is the
// requirement that keeps stablecoins and utility tokens off the page.
// A recognised issuer contract whose symbol names no real-world
// instrument gets nothing.
func TestQualifyContract_RecognisedButNoInstrumentIsRefused(t *testing.T) {
	for _, sym := range []string{"USDC", "AQUA", "", "POOL"} {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:     contractA,
			DirectoryNamed: true,
			DirectoryTags:  []string{"issuer"},
			Symbol:         sym,
		})
		if v.InSet {
			t.Fatalf("symbol %q admitted as a real-world instrument", sym)
		}
		if v.Reject != rwa.RejectNoContractBasis {
			t.Errorf("symbol %q: reject = %q, want %q", sym, v.Reject, rwa.RejectNoContractBasis)
		}
	}
}

// TestQualifyContract_AdmitsARecognisedOracleInstrument is the positive
// case: an independent party named the exact address with an issuing
// tag, and an independent oracle prices an instrument of that symbol.
func TestQualifyContract_AdmitsARecognisedOracleInstrument(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     contractA,
		DirectoryNamed: true,
		DirectoryTags:  []string{"issuer"},
		Symbol:         "USTRY",
	})
	if !v.InSet {
		t.Fatalf("refused a recognised contract holding an oracle-priced instrument: %q", v.Reject)
	}
	if v.Basis != rwa.BasisContractOracleFeed {
		t.Errorf("basis = %q, want %q", v.Basis, rwa.BasisContractOracleFeed)
	}
	// The oracle arm carries no class: a feed names an instrument, not
	// its classification. Inventing one would publish a category nothing
	// declared.
	if v.AnchorClass != "" {
		t.Errorf("anchor_class = %q on the oracle basis, want empty", v.AnchorClass)
	}
}

// TestQualifyContract_RefusesAMalformedAddress pins C1. A string that
// merely starts with C is not a contract address, and this surface must
// never carry an unverified identifier into a supply query.
func TestQualifyContract_RefusesAMalformedAddress(t *testing.T) {
	for name, id := range map[string]string{
		"empty":       "",
		"account":     "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		"bad crc":     "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"too short":   "CAAAAAAA",
		"not base32":  "C1111111111111111111111111111111111111111111111111111111",
		"looks right": "Cnotarealstrkeyatallbutfiftysixcharacterslongsoitpasses1",
	} {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:     id,
			DirectoryNamed: true,
			DirectoryTags:  []string{"issuer"},
			Symbol:         "USTRY",
		})
		if v.InSet {
			t.Errorf("%s: admitted %q as a contract address", name, id)
		}
		if v.Reject != rwa.RejectNotContract {
			t.Errorf("%s: reject = %q, want %q", name, v.Reject, rwa.RejectNotContract)
		}
	}
}

// TestContractInstrumentBindings_AreWellFormed pins the curated set's
// posture. Addresses must come from a primary source; one written from memory is a fabricated
// identity for a financial instrument.
//
// It checks that whatever the list holds is well-formed (a real strkey, a named instrument, a
// class from the closed vocabulary) rather than that the list is empty. A malformed entry would
// silently admit or silently refuse, both worse than a compile error.
func TestContractInstrumentBindings_AreWellFormed(t *testing.T) {
	classes := rwa.ContractAnchorClasses()
	seen := map[string]struct{}{}
	for _, b := range rwa.ContractInstrumentBindings() {
		if !slices.Contains(classes, b.Class) {
			t.Errorf("binding %s declares class %q, not in the closed vocabulary %v",
				b.ContractID, b.Class, classes)
		}
		if b.Instrument == "" {
			t.Errorf("binding %s names no instrument — the evidence bar requires a specific, falsifiable one",
				b.ContractID)
		}
		if _, dup := seen[b.ContractID]; dup {
			t.Errorf("binding %s appears twice", b.ContractID)
		}
		seen[b.ContractID] = struct{}{}
		// A curated entry that is not a valid contract address would
		// bind to nothing and refuse silently.
		if v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID: b.ContractID, DirectoryNamed: true, DirectoryTags: []string{"issuer"},
		}); !v.InSet {
			t.Errorf("binding %s does not admit its own contract under a recognised directory entry: %q",
				b.ContractID, v.Reject)
		}
	}
}

// TestCouldQualifyContract_IsAPreFilterNotADecision pins that the
// pre-filter never answers the membership question. It reads
// contract-side inputs only, so it must not be able to say yes to
// something QualifyContract says no to for a directory reason.
func TestCouldQualifyContract_IsAPreFilterNotADecision(t *testing.T) {
	if !rwa.CouldQualifyContract(contractB, "CETES") {
		t.Fatal("pre-filter dropped an oracle-priced symbol")
	}
	v := rwa.QualifyContract(rwa.ContractCandidate{ContractID: contractB, Symbol: "CETES"})
	if v.InSet {
		t.Fatal("the pre-filter's yes became a membership yes without a directory entry")
	}
}

// The Spiko curated bindings.
//
// These are real mainnet addresses, not fixtures standing in for imaginary ones, so the warning
// at the top of this file does not apply. Each is asserted against the exact property its
// curated entry claims.
const (
	// Bound: "Spiko EU T-Bills Money Market Fund", the largest of the
	// five by supply.
	spikoEUTBLContract = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	// Deliberately NOT bound: "Spiko Digital Assets Cash and Carry
	// Fund". Same issuer, same evidence, no class in the closed
	// vocabulary that describes a crypto basis-trade fund.
	spikoSPKCCContract = "CDS2GCAQTNQINSCJUJIVBJXILKBWP5PU7LOBGHMP3X47QCQBFKPMTCNT"
)

// TestSpikoBinding_StillNeedsIndependentRecognition is the reason adding
// these bindings does not change what the surface publishes today.
//
// The curated entry answers C4 and nothing else. With no directory entry for the address, the
// candidate must still be refused, and refused on C2. If it ever admits, a curated in-repo entry
// has silently become sufficient on its own, replacing the arm's argument (an independent party
// named THIS address) with our own say-so.
func TestSpikoBinding_StillNeedsIndependentRecognition(t *testing.T) {
	// Both ways the independent half can be missing. Neither admits,
	// and each says which half was missing rather than reporting one
	// indistinguishable recognition failure.
	for _, tc := range []struct {
		name      string
		available bool
		want      string
	}{
		{
			// Nobody looked: no listing reader wired, the read failed,
			// or the snapshot aged past the recognition bound. An
			// outage may not report an absence as a finding.
			name: "listing read did not answer", available: false,
			want: rwa.RejectContractListingUnavailable,
		},
		{
			// Somebody looked and the listing does not name it. This
			// is a finding, and it is the one holding independence up.
			name: "listing answered and does not name it", available: true,
			want: rwa.RejectContractCuratedNotListed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := rwa.QualifyContract(rwa.ContractCandidate{
				ContractID:       spikoEUTBLContract,
				DirectoryNamed:   false,
				DirectoryTags:    []string{"issuer"},
				ListingNamed:     false,
				ListingAvailable: tc.available,
			})
			if v.InSet {
				t.Fatalf("a curated binding admitted its contract with no independent recognition (basis %q)", v.Basis)
			}
			if v.Reject != tc.want {
				t.Errorf("reject = %q, want %q", v.Reject, tc.want)
			}
		})
	}
}

// ─── C2 arm 2: an independent listing corroborating a curated binding ──
//
// The addresses below are REAL, unlike contractA/contractB: arm 2 rests on two parties who do
// not read each other arriving at the same 56 characters, which a made-up address cannot express.
const (
	// Named by the listing directory AND by an in-repo curated binding.
	// The curated binding was derived from the issuer's own deployment
	// manifest reached through its own registrable domain; the listing
	// entry was not.
	listedAndBoundEUTBL = spikoEUTBLContract
	listedAndBoundUKTBL = "CDT3KU6TQZNOHKNOHNAFFDQZDURVC3MSTL4ML7TUTZGNOPBZCLABP4FR"
	// BOUND, and the listing directory does NOT name it. The control
	// that proves arm 2 is not simply admitting everything this
	// repository has an opinion about: same issuer, same deployment,
	// same evidence bar, no independent naming, no admission.
	boundNotListedUSTBL = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	// LISTED, and no in-repo curated binding names it: the native asset's own SAC and USDC's.
	// The listing carries anything with a market, which is why it may not admit on its own.
	//
	// Both are permanent controls: neither the native asset nor a fiat-backed stablecoin can
	// acquire a curated RWA binding, so no later admission can quietly retire this test.
	listedNotBoundXLM  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	listedNotBoundUSDC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestListingCorroboratingCuratedBinding_Admits is the unlock, and the
// only combination in this file that admits without a directory entry.
//
// Two independent parties naming the same exact address is what C2 asks for. The row says which
// pair let it in, because this evidence is weaker than a curated directory attestation and a
// consumer must be able to tell them apart.
func TestListingCorroboratingCuratedBinding_Admits(t *testing.T) {
	for _, id := range []string{listedAndBoundEUTBL, listedAndBoundUKTBL} {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:       id,
			DirectoryNamed:   false,
			ListingNamed:     true,
			ListingAvailable: true,
		})
		if !v.InSet {
			t.Fatalf("%s: refused %q; an independent listing plus a curated binding must satisfy C2", id, v.Reject)
		}
		if v.Basis != rwa.BasisCuratedContract {
			t.Errorf("%s: basis = %q, want %q", id, v.Basis, rwa.BasisCuratedContract)
		}
		if v.Recognition != rwa.RecognitionListingCorroborated {
			t.Errorf("%s: recognition = %q, want %q", id, v.Recognition, rwa.RecognitionListingCorroborated)
		}
		if v.AnchorClass != "bond" {
			t.Errorf("%s: class = %q, want bond", id, v.AnchorClass)
		}
	}
}

// TestCuratedSetCarriesMoreThanOneClass guards a property the surface's
// by-class breakdown depends on and nothing else asserts: the curated
// contract set is not a set of bonds with a vocabulary bolted on.
//
// Most bindings are bond or fund classes, so `commodity` can exist in [contractAnchorClasses]
// while never being produced. A refactor that narrowed the admitted classes to the ones in use
// would pass every other test here.
func TestCuratedSetCarriesMoreThanOneClass(t *testing.T) {
	seen := map[string]string{}
	for _, b := range rwa.ContractInstrumentBindings() {
		if !rwa.ContractInstrumentClass(b.Class) {
			t.Errorf("%s (%s): class %q is not in the contract vocabulary", b.ContractID, b.Instrument, b.Class)
		}
		seen[b.Class] = b.ContractID
	}
	for _, want := range []string{"bond", "fund", "commodity"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no curated binding carries class %q; the by-class breakdown cannot show it", want)
		}
	}
	// XAUm specifically: tokenized gold is a bearer claim on metal, not
	// a claim on a managed portfolio. Classifying it `fund` or `bond`
	// would be false on both the asset and the exposure.
	const xaum = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	var class string
	for _, b := range rwa.ContractInstrumentBindings() {
		if b.ContractID == xaum {
			class = b.Class
		}
	}
	if class != "commodity" {
		t.Errorf("XAUm is bound as %q, want commodity", class)
	}
}

// TestListingAlone_AdmitsNothing is the negative control the arm lives
// or dies by, run against REAL addresses the listing names and this repository has no binding
// for. If it ever admits, C2 has become "some price aggregator has heard of it" and the native
// asset's own SAC would be published as a tokenized real-world asset.
func TestListingAlone_AdmitsNothing(t *testing.T) {
	for _, id := range []string{listedNotBoundXLM, listedNotBoundUSDC} {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:       id,
			DirectoryNamed:   false,
			ListingNamed:     true,
			ListingAvailable: true,
		})
		if v.InSet {
			t.Fatalf("%s: a listing entry alone admitted a contract (basis %q, recognition %q)", id, v.Basis, v.Recognition)
		}
		if v.Reject != rwa.RejectContractListedNotCurated {
			t.Errorf("%s: reject = %q, want %q", id, v.Reject, rwa.RejectContractListedNotCurated)
		}
	}
}

// TestBoundButNotListed_StaysRefused pins the other control. USTBL is
// bound on identical evidence to EUTBL and UKTBL. The listing directory carries no Stellar
// address for it, so it is not admitted, showing the rule is not a rubber stamp for the
// curated set.
func TestBoundButNotListed_StaysRefused(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:       boundNotListedUSTBL,
		DirectoryNamed:   false,
		ListingNamed:     false,
		ListingAvailable: true,
	})
	if v.InSet {
		t.Fatalf("a bound contract nobody independent named was admitted (recognition %q)", v.Recognition)
	}
	if v.Reject != rwa.RejectContractCuratedNotListed {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractCuratedNotListed)
	}
}

// TestScamPrecedence_SurvivesTheListingArm is the single most important
// test in this file.
//
// The listing directory carries no flags. If the scam check lived only inside the directory arm,
// an address the curated directory named ONLY to flag as malicious could walk in through arm 2.
// Every scam-class tag in the shared vocabulary is exercised, so a newly added tag cannot
// quietly fail to bind here.
func TestScamPrecedence_SurvivesTheListingArm(t *testing.T) {
	for _, tag := range timescale.DirectoryScamFlagTags {
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:       listedAndBoundEUTBL,
			DirectoryNamed:   true,
			DirectoryTags:    []string{"issuer", tag},
			ListingNamed:     true,
			ListingAvailable: true,
		})
		if v.InSet {
			t.Fatalf("tag %q: a scam-flagged address was admitted through the listing arm (recognition %q)", tag, v.Recognition)
		}
		if v.Reject != rwa.RejectContractScam {
			t.Errorf("tag %q: reject = %q, want %q", tag, v.Reject, rwa.RejectContractScam)
		}
	}
}

// TestListingUnavailable_FailsClosed pins the shrink direction. When the
// listing read cannot answer, arm 2 stops admitting — it does not carry
// a recognition forward from a snapshot nobody re-established — and the
// refusal names the outage so the set does not shrink in silence.
func TestListingUnavailable_FailsClosed(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     listedAndBoundEUTBL,
		DirectoryNamed: false,
		// Even asserted as named: an unavailable read means the naming
		// was never established, so the field may not be read at all.
		ListingNamed:     true,
		ListingAvailable: false,
	})
	if v.InSet {
		t.Fatalf("arm 2 admitted while the listing read was unavailable (recognition %q)", v.Recognition)
	}
	if v.Reject != rwa.RejectContractListingUnavailable {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractListingUnavailable)
	}
}

// TestDirectoryArm_UnchangedByTheSecondArm pins that adding arm 2 moved
// nothing on arm 1. A directory-recognised contract is admitted with no
// listing anywhere in sight, and says the directory recognised it.
func TestDirectoryArm_UnchangedByTheSecondArm(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:       listedAndBoundEUTBL,
		DirectoryNamed:   true,
		DirectoryTags:    []string{"issuer"},
		ListingNamed:     false,
		ListingAvailable: false,
	})
	if !v.InSet {
		t.Fatalf("the curated directory arm stopped admitting: %q", v.Reject)
	}
	if v.Recognition != rwa.RecognitionCuratedDirectory {
		t.Errorf("recognition = %q, want %q", v.Recognition, rwa.RecognitionCuratedDirectory)
	}
}

// TestListingArm_IsOnTheAddressNotTheSymbol is the non-negotiable rule
// in arm 2's coordinate. An impersonator whose contract the listing directory happens to carry
// gets nothing, because the curated binding is keyed on the 56-character address and the
// impersonator does not have it.
func TestListingArm_IsOnTheAddressNotTheSymbol(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:       contractB, // not a bound address
		Symbol:           "EUTBL",
		DirectoryNamed:   false,
		ListingNamed:     true,
		ListingAvailable: true,
	})
	if v.InSet {
		t.Fatalf("a listed impersonator carrying a bound symbol was admitted (basis %q)", v.Basis)
	}
	if v.Reject != rwa.RejectContractListedNotCurated {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractListedNotCurated)
	}
}

// TestEveryAdmittedContract_NamesItsRecogniser pins the wire guarantee:
// no contract is ever admitted without saying who recognised it, and
// the value is always one of the two published sources. A third arm
// added later without a recognition source would land here.
func TestEveryAdmittedContract_NamesItsRecogniser(t *testing.T) {
	sources := rwa.ContractRecognitionSources()
	for _, c := range []rwa.ContractCandidate{
		{ContractID: listedAndBoundEUTBL, DirectoryNamed: true, DirectoryTags: []string{"issuer"}},
		{ContractID: listedAndBoundEUTBL, ListingNamed: true, ListingAvailable: true},
		{ContractID: contractA, DirectoryNamed: true, DirectoryTags: []string{"anchor"}, Symbol: "USTRY"},
	} {
		v := rwa.QualifyContract(c)
		if !v.InSet {
			t.Fatalf("%+v: refused %q", c, v.Reject)
		}
		if !slices.Contains(sources, v.Recognition) {
			t.Errorf("%s: recognition %q is not in the published vocabulary %v", c.ContractID, v.Recognition, sources)
		}
	}
}

// TestSpikoBinding_IsOnTheAddressNotTheSymbol is the non-negotiable rule
// in its contract-arm coordinate. An impersonator deploying a token
// whose symbol is EUTBL gets nothing from the curated set, however
// recognised its own address might be.
//
// Lookalike accounts already issue classic assets coded EUTBL and USTBL, and any of them can
// deploy a Soroban token with the same symbol for the price of a transaction.
func TestSpikoBinding_IsOnTheAddressNotTheSymbol(t *testing.T) {
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     contractB, // not Spiko's address
		DirectoryNamed: true,
		DirectoryTags:  []string{"issuer"},
		Symbol:         "EUTBL",
	})
	if v.InSet {
		t.Fatalf("admitted a non-Spiko contract wearing the symbol EUTBL (basis %q)", v.Basis)
	}
	if v.Reject != rwa.RejectNoContractBasis {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectNoContractBasis)
	}
	// And the real address IS bound, so the refusal above is the address
	// mismatch rather than the whole table being unreachable.
	if !rwa.CouldQualifyContract(spikoEUTBLContract, "") {
		t.Error("the curated EUTBL address is not bound — the assertion above proves nothing")
	}
}

// TestSpikoBinding_ExcludesTheCashAndCarryFund pins the classification
// judgement rather than leaving it to a comment. SPKCC carries the same identity evidence as
// the bound funds but is out: a digital-asset basis-trade fund is not a bond, stock, commodity
// or real estate, and [anchorClasses] excludes crypto on purpose.
func TestSpikoBinding_ExcludesTheCashAndCarryFund(t *testing.T) {
	if rwa.CouldQualifyContract(spikoSPKCCContract, "") {
		t.Error("the cash-and-carry fund is bound — it has no class in the closed vocabulary")
	}
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     spikoSPKCCContract,
		DirectoryNamed: true,
		DirectoryTags:  []string{"issuer"},
		Symbol:         "SPKCC",
	})
	if v.InSet {
		t.Fatalf("admitted the cash-and-carry fund as a real-world instrument (basis %q)", v.Basis)
	}
	if v.Reject != rwa.RejectNoContractBasis {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectNoContractBasis)
	}
}

// TestSpikoBindings_ClassifyByInstrumentNotByIssuer pins that each bound fund
// names a falsifiable instrument rather than a category (`US Treasury money
// market fund` is a category, a named fund with a share class is an
// instrument) and that the class follows the INSTRUMENT.
//
// One issuer has two kinds of fund bound. The T-Bill funds are `bond`. The overnight swap
// fund's share classes are `fund`: it holds listed equities but total return swaps hand the
// return away, so `bond` is false on assets and exposure, and `stock` is true of the assets but
// the opposite of the instrument. Classifying by issuer is how the wrong one gets published.
func TestSpikoBindings_ClassifyByInstrumentNotByIssuer(t *testing.T) {
	var spiko, tbill, swap int
	for _, b := range rwa.ContractInstrumentBindings() {
		if !strings.Contains(b.Instrument, "Spiko") {
			continue
		}
		spiko++
		switch {
		case strings.Contains(b.Instrument, "T-Bills Money Market Fund"):
			tbill++
			if b.Class != "bond" {
				t.Errorf("%s: class = %q, want %q", b.ContractID, b.Class, "bond")
			}
		case strings.Contains(b.Instrument, "Overnight Swap Fund"):
			swap++
			if b.Class != "fund" {
				t.Errorf("%s: class = %q, want %q", b.ContractID, b.Class, "fund")
			}
		default:
			t.Errorf("%s: instrument %q matches neither bound fund family — a new one "+
				"needs its class argued here, not defaulted", b.ContractID, b.Instrument)
		}
		if !strings.Contains(b.Instrument, "(") {
			t.Errorf("%s: instrument %q names no share class — the bar wants a fund, not a category",
				b.ContractID, b.Instrument)
		}
	}
	if tbill != 5 {
		t.Errorf("bound T-Bill funds = %d, want 5 (EUTBL, USTBL, UKTBL, eurUSTBL, eurUKTBL)", tbill)
	}
	if swap != 4 {
		t.Errorf("bound overnight-swap share classes = %d, want 4 (eurSAFO, SAFO, gbpSAFO, chfSAFO)", swap)
	}
	if spiko != tbill+swap {
		t.Errorf("bound Spiko funds = %d, but only %d were classified", spiko, tbill+swap)
	}
}

// TestCuratedBindings_DeclareAClassFromTheClosedVocabulary is the guard
// for the funnel's arm-2 claim that a curated class is in the closed
// vocabulary.
//
// QualifyContract returns a curated binding's Class VERBATIM without checking a vocabulary. An
// entry with a class outside it would be ADMITTED and published while the definition block
// still says the vocabulary is closed, so the wire contract would describe a row it does not
// cover, which is worse than a refusal.
//
// The vocabulary is [ContractAnchorClasses], not [AnchorClasses], on purpose. The classic arm
// READS an issuer's free-text anchor_asset_type, so it only accepts terms SEP-1 defines; a
// binding's class is this repository's own statement and may use a term SEP-1 lacks. `definition`
// serves BOTH lists, so the wire declares exactly the set a row may draw from.
//
// A test rather than a runtime check because the table is hand-reviewed: the failure belongs
// at the moment the entry is written, not when a response is served.
func TestCuratedBindings_DeclareAClassFromTheClosedVocabulary(t *testing.T) {
	classes := rwa.ContractAnchorClasses()
	bindings := rwa.ContractInstrumentBindings()
	if len(bindings) == 0 {
		t.Fatal("no curated bindings — a check over an empty set passes forever")
	}
	for _, b := range bindings {
		if !slices.Contains(classes, b.Class) {
			t.Errorf("%s (%s) declares class %q, which is not in the closed vocabulary %v",
				b.ContractID, b.Instrument, b.Class, classes)
		}
		if strings.TrimSpace(b.Instrument) == "" {
			t.Errorf("%s declares no instrument — the evidence bar requires a falsifiable one", b.ContractID)
		}
	}
}
