package rwa_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The contract arm of the definition. Impersonators are the norm: any issuer can deploy a Soroban token declaring the same symbol
// (e.g. BENJI) at no cost, so these tests pin that such a contract is refused and what an attacker would have to control instead.

// Real C-strkeys, CRC-valid, standing for nothing on the network: the arm is about the SHAPE of the evidence,
// and a real issuer's contract address as a fixture would be the fabricated-identity problem this file prevents.
const (
	contractA = "CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM"
	contractB = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// TestQualifyContract_RefusesAContractNobodyNamed is the whole arm in one case: a contract calling itself BENJI, with a
// symbol an oracle prices and no third party naming the address, is refused. If it passes, any impersonator can deploy
// one contract and inflate the page.
func TestQualifyContract_RefusesAContractNobodyNamed(t *testing.T) {
	// Every other input is the strongest an attacker could arrange; only the naming of this exact address is missing,
	// so weakening C2 ADMITS the candidate instead of refusing it for a different reason.
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

// TestQualifyContract_SymbolAloneNeverAdmits pins the ordering that makes the symbol safe to read:
// directory tags present but not issuing-class, symbol allow-listed, is refused on the TAG rather than the symbol.
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

// TestQualifyContract_RefusesInfrastructureTags explains why the contract vocabulary is narrower than the account one:
// the directory tags AMM pools and routers `defi`/`exchange`, which suggest an issuer on an account but infrastructure on a contract,
// and admitting them would list liquidity pool shares as real-world assets.
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

// TestQualifyContract_RecognisedButNoInstrumentIsRefused keeps stablecoins and utility tokens off the page:
// a recognised issuer contract whose symbol names no real-world instrument gets nothing.
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

// TestContractInstrumentBindings_AreWellFormed pins the curated set: addresses must come from a primary source, since one written
// from memory is a fabricated identity. Whatever the list holds must be a real strkey, a named instrument and a class from the closed
// vocabulary, because a malformed entry silently admits or refuses.
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

// TestCouldQualifyContract_IsAPreFilterNotADecision pins that the pre-filter reads contract-side inputs only,
// so it can never say yes to something QualifyContract refuses for a directory reason.
func TestCouldQualifyContract_IsAPreFilterNotADecision(t *testing.T) {
	if !rwa.CouldQualifyContract(contractB, "CETES") {
		t.Fatal("pre-filter dropped an oracle-priced symbol")
	}
	v := rwa.QualifyContract(rwa.ContractCandidate{ContractID: contractB, Symbol: "CETES"})
	if v.InSet {
		t.Fatal("the pre-filter's yes became a membership yes without a directory entry")
	}
}

// The Spiko curated bindings: real mainnet addresses, not fixtures, so the fixture warning at the top of this file does not apply.
// Each is asserted against the exact property its curated entry claims.
const (
	// Bound: "Spiko EU T-Bills Money Market Fund", the largest of the
	// five by supply.
	spikoEUTBLContract = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	// Deliberately NOT bound: "Spiko Digital Assets Cash and Carry
	// Fund". Same issuer, same evidence, no class in the closed
	// vocabulary that describes a crypto basis-trade fund.
	spikoSPKCCContract = "CDS2GCAQTNQINSCJUJIVBJXILKBWP5PU7LOBGHMP3X47QCQBFKPMTCNT"
)

// TestSpikoBinding_StillNeedsIndependentRecognition pins that a curated entry answers C4 and nothing else: with no
// directory entry the candidate is still refused on C2. Admission here would make our own say-so replace the arm's
// argument that an independent party named THIS address.
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

// C2 arm 2: an independent listing corroborating a curated binding. These addresses are REAL, unlike contractA/contractB,
// because arm 2 rests on two parties who do not read each other arriving at the same 56 characters.
const (
	// Named by the listing directory AND by an in-repo curated binding; the binding came from the issuer's own deployment manifest,
	// the listing entry did not.
	listedAndBoundEUTBL = spikoEUTBLContract
	listedAndBoundUKTBL = "CDT3KU6TQZNOHKNOHNAFFDQZDURVC3MSTL4ML7TUTZGNOPBZCLABP4FR"
	// BOUND but not named by the listing directory: the control proving arm 2 does not admit everything this repository has an opinion about.
	boundNotListedUSTBL = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	// LISTED but named by no curated binding: the native asset's SAC and USDC's. The listing carries anything with a
	// market, so it may not admit alone. Neither can acquire a curated RWA binding, so these stay permanent controls.
	listedNotBoundXLM  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	listedNotBoundUSDC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestListingCorroboratingCuratedBinding_Admits is the only combination here that admits without a directory entry: two
// independent parties naming the same address is what C2 asks for. The row names the pair, since this evidence is weaker
// than a directory attestation.
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

// TestCuratedSetCarriesMoreThanOneClass guards the by-class breakdown: most bindings are bond or fund, so `commodity`
// could exist in [contractAnchorClasses] while never being produced, and a refactor narrowing the admitted classes to
// those in use would pass every other test.
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

// TestListingAlone_AdmitsNothing is the negative control for the arm, run against REAL addresses the listing names with no binding.
// If it admits, C2 becomes "some price aggregator has heard of it" and the native asset's SAC would be published as a tokenized RWA.
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

// TestBoundButNotListed_StaysRefused pins the other control: USTBL is bound on the same evidence as EUTBL and UKTBL,
// but the listing carries no Stellar address for it, so it is refused and the rule is no rubber stamp for the curated set.
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

// TestScamPrecedence_SurvivesTheListingArm is the most important test here. The listing directory carries no flags, so if the scam check
// lived only in the directory arm, an address the curated directory named ONLY to flag as malicious could enter via arm 2.
// Every scam-class tag in the shared vocabulary is exercised, so a new tag cannot quietly fail to bind.
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

// TestListingUnavailable_FailsClosed pins the shrink direction: when the listing read cannot answer, arm 2 stops admitting
// rather than carrying a stale recognition forward, and the refusal names the outage so the set does not shrink silently.
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

// TestListingArm_IsOnTheAddressNotTheSymbol pins arm 2 as address-keyed: an impersonator the listing happens to carry gets nothing,
// because the curated binding is keyed on the 56-character address, which the impersonator lacks.
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

// TestEveryAdmittedContract_NamesItsRecogniser pins the wire guarantee that no contract is admitted without naming its recogniser,
// always one of the two published sources, so a third arm without a recognition source fails here.
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

// TestSpikoBinding_IsOnTheAddressNotTheSymbol pins the curated set as address-keyed: a token with symbol EUTBL gets
// nothing, however recognised its own address might be. Lookalike classic accounts already issue EUTBL and USTBL, and
// anyone can deploy a same-symbol Soroban token.
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

// TestSpikoBinding_ExcludesTheCashAndCarryFund pins a classification judgement: SPKCC has the same identity evidence as the bound funds
// but is a digital-asset basis-trade fund, not a bond, stock, commodity or real estate, and [anchorClasses] excludes crypto on purpose.
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

// TestSpikoBindings_ClassifyByInstrumentNotByIssuer pins that each bound fund names a falsifiable instrument (a named fund with a
// share class, not a category) and that the class follows the INSTRUMENT: the T-Bill funds are `bond`, the overnight swap fund is `fund`,
// since swaps hand the return away so `bond` is false and `stock` is true of the assets but not the instrument.
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

// TestCuratedBindings_DeclareAClassFromTheClosedVocabulary: QualifyContract returns a binding's Class verbatim, so an
// out-of-vocabulary class would be admitted yet uncovered by the wire contract. The vocabulary is
// [ContractAnchorClasses], not [AnchorClasses]: a binding is our own statement and may use a term SEP-1 lacks.
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
