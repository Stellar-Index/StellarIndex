package rwa_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The contract arm of the definition (#352).
//
// These tests are driven by the population that makes the arm
// load-bearing rather than decorative. Measured on the production lake
// 2026-09-10, NINETEEN distinct issuers publish a token called BENJI and
// every one of them is an impersonator — franklintempleton.co.com,
// franklintempleton.hqlumens.com, benji.qlumen.co, stellar.dtcc.network,
// treasury.dtcc.company — most already carrying a scam-class directory
// tag. Not one is franklintempleton.com.
//
// Every one of those could deploy a Soroban token contract declaring
// symbol BENJI tomorrow, at no cost and with nobody's permission. The
// tests below pin that such a contract is refused, and pin the specific
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
// address: refused.
//
// This is the impersonator shape. If it ever passes, every one of the
// nineteen BENJI issuers can put a nine-figure valuation on this page by
// deploying one contract.
func TestQualifyContract_RefusesAContractNobodyNamed(t *testing.T) {
	// Every OTHER input is the strongest an attacker could arrange: an
	// issuing-class tag, no scam tag, and a symbol an oracle really
	// prices. Only the naming of this exact address is missing. So if C2
	// is ever removed or weakened, this candidate is ADMITTED rather than
	// merely refused for a different reason — which is what the assertion
	// below catches.
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
// The curated directory tags AMM pools and protocol routers `defi` and
// `exchange`. On an ACCOUNT those describe an entity that might issue a
// real-world instrument; on a CONTRACT address they describe a piece of
// infrastructure that issues nothing. Admitting them would put liquidity
// pool shares on a page that says the assets on it represent real-world
// value.
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
// posture. Populating it needs contract addresses from a primary source,
// and an address written from memory is a fabricated identity for a
// financial instrument.
//
// The test is not "the list is empty" (which would have had to be
// deleted the day someone added a verified entry, as the Spiko funds
// now are). It is that whatever the list holds is well-formed: a real
// strkey, a named instrument, and a class from the closed vocabulary. A
// malformed entry would silently admit or silently refuse, and both are
// worse than a compile error.
func TestContractInstrumentBindings_AreWellFormed(t *testing.T) {
	classes := rwa.AnchorClasses()
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
// These are real mainnet addresses rather than fixtures, which the note
// at the top of this file warns against — that warning is about standing
// a real address in for an imaginary one, and these stand for
// themselves. Each is asserted against the exact property its curated
// entry claims.
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
// The curated entry answers C4 and nothing else. With no directory entry
// for the address — which is the live state, every Spiko contract
// returning 404 from the curated directory on 2026-09-15 — the candidate
// must still be refused, and refused on C2.
//
// If this ever starts passing as an admission, a curated in-repo entry
// has silently become sufficient on its own, and the arm's whole
// argument (an independent party named THIS address) has been replaced
// by our own say-so without anyone deciding to.
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
// The addresses below are REAL and are the measured state of the two
// sources on 2026-09-15. They are fixtures in the sense that the test
// pins a rule, but unlike contractA/contractB they are not invented:
// the whole point of arm 2 is that two parties who do not read each
// other arrived at the same 56 characters, and a made-up address could
// not express that.
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
	// LISTED, and no in-repo curated binding names it. Real entries
	// from the same listing map: a tokenized gold contract and the
	// native asset's own Stellar Asset Contract. The listing is a
	// price-aggregation convenience and carries anything with a
	// market, which is exactly why it may not admit on its own.
	listedNotBoundXAUM = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	listedNotBoundUSDC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestListingCorroboratingCuratedBinding_Admits is the unlock, and the
// only combination in this file that admits without a directory entry.
//
// Two independent parties naming the same exact address is what C2 asks
// for. The row says which pair let it in, because the evidence is not
// the same strength as a curated directory attestation and a consumer
// must be able to tell them apart.
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

// TestListingAlone_AdmitsNothing is the negative control the arm lives
// or dies by, run against REAL addresses the listing directory really
// does name and this repository holds no binding for.
//
// If this ever passes as an admission, C2 has been replaced by "some
// price aggregator has heard of it" and the surface would publish the
// native asset's own SAC as a tokenized real-world asset.
func TestListingAlone_AdmitsNothing(t *testing.T) {
	for _, id := range []string{listedNotBoundXAUM, listedNotBoundUSDC} {
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
// bound on identical evidence to EUTBL and UKTBL, deployed by the same
// account under the same wasm hash, and holds a 32.9M-token supply the
// lake can see. The listing directory carries no Stellar address for
// it, so it is not admitted and the rule is demonstrably not a rubber
// stamp for the curated set.
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
// The listing directory carries no flags and never will. If the scam
// check had stayed inside the directory arm, an address the curated
// directory named ONLY to flag as malicious could have walked in
// through arm 2 — the precedence would have protected exactly the
// population that did not need it.
//
// Every scam-class tag in the one shared vocabulary is exercised, so a
// tag added to that list cannot quietly fail to bind here.
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
// in arm 2's coordinate. An impersonator whose contract the listing
// directory happens to carry — a token with a real market is exactly
// what that map collects — gets nothing, because the curated binding is
// keyed on the 56-character address and the impersonator does not have
// it.
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
// The network already carries at least five accounts issuing classic
// assets coded EUTBL and USTBL from lookalike domains. Any of them can
// deploy a Soroban token with the same symbol for the price of a
// transaction.
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
// judgement rather than leaving it to a comment. SPKCC is Spiko's, is
// deployed, and carries exactly the same identity evidence as the five
// bound funds — it is out because a digital-asset basis-trade fund is
// not a bond, a stock, a commodity or real estate, and [anchorClasses]
// excludes crypto on purpose.
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

// TestSpikoBindings_AreAllBondClass pins that the five bound funds
// classify as `bond` and that each names a falsifiable instrument rather
// than a category. `US Treasury money market fund` is a category; a
// named fund with a share class is an instrument.
func TestSpikoBindings_AreAllBondClass(t *testing.T) {
	var spiko int
	for _, b := range rwa.ContractInstrumentBindings() {
		if !strings.Contains(b.Instrument, "Spiko") {
			continue
		}
		spiko++
		if b.Class != "bond" {
			t.Errorf("%s: class = %q, want %q", b.ContractID, b.Class, "bond")
		}
		if !strings.Contains(b.Instrument, "(") {
			t.Errorf("%s: instrument %q names no share class — the bar wants a fund, not a category",
				b.ContractID, b.Instrument)
		}
	}
	if spiko != 5 {
		t.Errorf("bound Spiko funds = %d, want 5 (EUTBL, USTBL, UKTBL, eurUSTBL, eurUKTBL)", spiko)
	}
}
