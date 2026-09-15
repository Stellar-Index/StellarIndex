package rwa_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
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
	v := rwa.QualifyContract(rwa.ContractCandidate{
		ContractID:     spikoEUTBLContract,
		DirectoryNamed: false,
		DirectoryTags:  []string{"issuer"},
	})
	if v.InSet {
		t.Fatalf("a curated binding admitted its contract with no independent recognition (basis %q)", v.Basis)
	}
	if v.Reject != rwa.RejectContractNotNamed {
		t.Errorf("reject = %q, want %q", v.Reject, rwa.RejectContractNotNamed)
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
