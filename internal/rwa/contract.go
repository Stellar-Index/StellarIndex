package rwa

import (
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The contract arm of the definition — how a Soroban-issued token
// qualifies when there is no (code, issuer) pair to bind a declaration
// to.
//
// # Why an arm was needed at all
//
// R1 admitted classic assets only, and the reason given was sound: SEP-1
// [[CURRENCIES]] binds a declaration to a (code, issuer) pair, a bare
// C-address has no such binding, and admitting one would mean admitting
// an unbound claim.
//
// What that reasoning missed is that the real-world issuers are not on
// the classic side at all. Measured on production 2026-09-10, of the
// sixteen entities a public dashboard attributes 4.03 billion dollars of
// Stellar real-world assets to, the ones we could check issue NOTHING in
// classic_assets: Franklin Templeton (both G-addresses), Spiko, Mercado
// Bitcoin, Cometum and fourteen of WisdomTree's eighteen addresses all
// return zero rows. Their Stellar presence is contract-issued.
//
// The exclusion also ran deeper than the requirement. The issuers table
// is populated by registerIssuerSeen, called from ONE site: the
// classic-asset registration path. An entity that issues only contract
// tokens therefore never gets an issuers row, never gets a SEP-1 fetch,
// and never becomes a candidate — so it was not refused by R1 so much as
// never collected. Widening that table is not the fix either: its key is
// a G-account, a C-address is not one, and a SEP-1 fetch for a bare
// contract has nothing to bind to. The population has to come from
// somewhere else, and this file says where.
//
// # The requirements, and what replaces R2
//
// R2 — the issuer-bound SEP-1 self-declaration — is structurally
// impossible for a contract. There is no [[CURRENCIES]] entry naming a
// C-address in the payloads we hold, so there is nothing to inherit.
//
// The replacement is NOT a relaxation, because of what R2 was actually
// worth. R2 proves a claim came from a domain the issuer controls, and
// the cost of satisfying it is one domain registration. The measured
// population says so plainly: 19 distinct issuers publish a token called
// BENJI, every one of them an impersonator, from franklintempleton.co.com,
// franklintempleton.hqlumens.com, benji.qlumen.co, stellar.dtcc.network
// and a dozen more — each serving a valid, correctly-bound stellar.toml.
// Not one is franklintempleton.com. R2 admitted all of them. R3 is what
// kept them out, and R3 is what carries the contract arm.
//
// So the contract arm requires the curated third-party directory to name
// THE EXACT CONTRACT ADDRESS. Not the entity. Not a domain. Not a
// G-account that might have deployed it. The address whose supply this
// surface is about to multiply by a price.
//
// # What an attacker would have to control
//
// To get a fake contract admitted here, an attacker needs a curated
// directory entry naming their own C-address, carrying an issuing-class
// tag and no scam-class tag. The directory is the MIT-licensed
// stellar-expert public directory, synced from a reviewed repository, so
// that means landing a reviewed change in a third party's published set
// under the name of the entity being impersonated.
//
// Compare the classic arm. There, the equivalent step for R2 is
// registering a lookalike domain, pointing home_domain at it and serving
// a file — no review, no third party, roughly the price of a domain, and
// measured 19 times over for one ticker. R3 then has to stop it, and R3
// on the classic arm vouches for an ACCOUNT: one recognition of one
// G-address covers every token that account will ever issue, including
// ones issued after the recognition was granted.
//
// The contract arm has no such carry-over. Recognition is per address,
// and a contract address is one token. An attacker who somehow got one
// contract recognised gains exactly that contract and nothing else.
//
// The contract arm is therefore strictly harder to defeat than the
// classic arm, not easier. What it gives up is the issuer's own voice —
// and the issuer's own voice, measured, is the part of the evidence an
// impersonator supplies for ten dollars.
//
// # Why recognition alone still is not enough
//
// A directory entry says who an address belongs to. It does not say the
// token is a real-world asset. The curated set names stablecoin
// contracts, AMM pool shares and protocol infrastructure under the same
// tags, and admitting every recognised contract would publish USDC and a
// liquidity-pool share as tokenized real-world assets — inflating the
// headline with exactly the fiat-anchored population the anchorClasses
// vocabulary deliberately excludes.
//
// So C4 keeps R4's job, with the two bases a contract can actually
// carry. Neither is the contract's own unaided word about what it is.
//
// # What was considered and rejected
//
//   - CONTRACT-INSTANCE PROVENANCE — admitting a contract because a
//     recognised G-account deployed it. We hold no deployer edge for
//     arbitrary contracts (the contractid registry is factory-anchored
//     per ADR-0035 and covers protocol children, not token issuance), so
//     there is nothing to read. It is the right shape and it is future
//     work, not a thing this arm can pretend to.
//   - SEP-41 METADATA AS THE ADMISSION — admitting a contract whose
//     name() or symbol() says treasury or gold. That is the token
//     talking about itself with no cost at all: cheaper than a domain,
//     and it would admit every impersonator in the lake. Metadata is
//     read here only AFTER an independent party named the address, and
//     only to answer which instrument, never whether.
//   - THE ISSUER SEP-1 NAMING THE CONTRACT — the current SEP-1 draft
//     carries a contract field on [[CURRENCIES]]. This is the strongest
//     available strengthener and is genuinely worth having: the curator
//     supplies the domain, so an attacker cannot choose which domain has
//     to corroborate them, and defeating the pair would mean a directory
//     merge AND control of the real entity's domain. It is not built
//     here for a measured reason rather than an aesthetic one: our SEP-1
//     parser does not read the field, and 14 of the 16 entities have no
//     fetched payload at all — the refresh queue holds 29,741 issuers
//     against a daily limit of 100. Building it today would admit
//     nothing and would hide the operator lever behind machinery. It is
//     recorded in the methodology doc as the next arm.

// Contract-arm bases. They name WHY a contract-issued token is
// classified as a real-world instrument (C4), and are served on the row
// beside the classic bases so a consumer can filter on the strength of
// evidence rather than trusting the membership decision wholesale.
const (
	// BasisCuratedContract — an in-repo curated entry binds this exact
	// contract address to a named real-world instrument and its class.
	// The ADR-0040 curated-set mechanism: review-gated by living in
	// code, fail-closed, and published in full on the wire.
	BasisCuratedContract = "curated_contract_instrument"
	// BasisContractOracleFeed — the token's on-chain SEP-41 symbol is an
	// ADR-0028 allow-listed RWA code, meaning an independent oracle
	// publishes a net-asset-value feed for an instrument of that name.
	//
	// The symbol is contract-authored, exactly as a classic asset code is
	// issuer-authored, and this arm is admissible for exactly the same
	// reason [BasisOracleFeed] is on the classic side: the independent
	// recognition of the address has ALREADY happened. On its own it
	// would be the code-only identity the whole definition refuses; after
	// C2 it names which instrument an address someone vouched for holds.
	BasisContractOracleFeed = "contract_oracle_rwa_feed"
)

// Contract-arm refusals. Distinct constants rather than reuse of the
// classic reasons, because the ACTION each implies is different: a
// contract absent from the directory needs a curated entry, while a
// classic asset failing R2 needs its issuer to publish a file.
const (
	RejectNotContract       = "not_a_contract_address"
	RejectContractNotNamed  = "contract_not_named_in_directory"
	RejectContractNoTag     = "contract_named_without_issuing_tag"
	RejectContractScam      = "contract_scam_flagged"
	RejectNoContractBasis   = "no_real_world_instrument_basis_for_contract"
	RejectContractDuplicate = "duplicate_contract_declaration"
)

// contractRecognitionTags is the curated-directory vocabulary that
// counts as an independent party recognising a CONTRACT as an issuing
// entity for a real-world instrument.
//
// Deliberately narrower than [recognitionTags], which is the account
// vocabulary. Three of the six account tags describe infrastructure
// rather than issuance, and on a contract address that distinction is
// the whole difference between a tokenized treasury and a swap router:
//
//   - `exchange` and `defi` are what the upstream set puts on AMM pools,
//     routers and protocol contracts. The curated directory carries the
//     Aquarius pool contracts under exactly these tags, and admitting
//     them would put liquidity-pool shares on a real-world-asset page.
//   - `sdf` marks foundation infrastructure, which issues nothing.
//
// What survives is the three tags that assert the address ISSUES or
// CUSTODIES value on behalf of a named entity. A contract carrying none
// of them is refused under [RejectContractNoTag] and the refusal is
// counted, so a tag vocabulary that turns out to be too narrow shows up
// as a number an operator can see rather than as silence.
var contractRecognitionTags = map[string]struct{}{
	"issuer":    {},
	"anchor":    {},
	"custodian": {},
}

// ContractRecognitionTags lists the contract recognition vocabulary in a
// stable order. Served on the wire for the same reason [AnchorClasses]
// and [RecognitionTags] are: a consumer reads the rule from the response
// rather than inferring it from whichever rows qualified today.
func ContractRecognitionTags() []string {
	out := make([]string, 0, len(contractRecognitionTags))
	for t := range contractRecognitionTags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// HasContractRecognitionTag reports whether any tag vouches for the
// contract as issuing a real-world instrument. Matched
// case-insensitively on trimmed tags, the same way every other reader of
// this column matches.
func HasContractRecognitionTag(tags []string) bool {
	for _, t := range tags {
		if _, ok := contractRecognitionTags[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// ContractCandidate is one contract-issued token put to the definition,
// with every input the contract requirements read.
//
// Like [Candidate] it carries no valuation: membership is decided before
// a number is attached, so a thin or withheld market can never change
// who is in the set.
type ContractCandidate struct {
	// ContractID is the C-strkey. It is the whole identity — never a
	// symbol, never a name. Two contracts calling themselves BENJI are
	// two different assets, the same way two issuers of a classic BENJI
	// are.
	ContractID string
	// DirectoryNamed reports whether the curated third-party directory
	// holds an entry for THIS EXACT address. The caller establishes it
	// from the directory read; this package never assumes a naming it
	// did not see.
	DirectoryNamed bool
	// DirectoryTags are the curated tags on that entry. Empty when the
	// directory does not name the contract, which is a refusal and not
	// an error.
	DirectoryTags []string
	// Symbol is the token symbol read from the contract instance
	// storage METADATA map. Contract-authored display text, used only by
	// [BasisContractOracleFeed] and only after recognition. Empty when
	// the instance was not captured or declares no metadata.
	Symbol string
}

// QualifyContract applies the contract requirements in order and returns
// the verdict.
//
// The order matters for the reported reason, exactly as it does in
// [Qualify]: a contract nobody has named is reported as unnamed even
// when its symbol would also have failed C4, because being named is the
// thing that would have had to change first.
func QualifyContract(c ContractCandidate) Verdict {
	// C1 — IDENTITY. A real contract address, CRC-checked. A string that
	// merely starts with C is not one, and this surface must never join
	// a supply query on an identifier it did not verify.
	if !canonical.IsContractID(strings.TrimSpace(c.ContractID)) {
		return Verdict{Reject: RejectNotContract}
	}
	// C2 — INDEPENDENT NAMING OF THIS EXACT ADDRESS. The requirement
	// that replaces R2, and the one an attacker has to defeat.
	if !c.DirectoryNamed {
		return Verdict{Reject: RejectContractNotNamed}
	}
	// C3 — RECOGNITION, NOT SCAM-FLAGGED. Scam before recognition, the
	// same precedence [Qualify] applies: an address carrying both is
	// refused as flagged, which is the stronger and more useful
	// statement. The scam vocabulary is read from the ONE list in
	// timescale.DirectoryScamFlagTags via [ScamFlagged], so a contract
	// whose price the serving gates withhold can never be admitted here.
	if ScamFlagged(c.DirectoryTags) {
		return Verdict{Reject: RejectContractScam}
	}
	if !HasContractRecognitionTag(c.DirectoryTags) {
		return Verdict{Reject: RejectContractNoTag}
	}
	// C4 — REAL-WORLD INSTRUMENT. The curated binding first: it names
	// the instrument AND its class, so it produces a strictly more
	// informative row than the oracle arm and should win when both
	// apply.
	if b, ok := contractInstrumentOf(c.ContractID); ok {
		return Verdict{InSet: true, Basis: BasisCuratedContract, AnchorClass: b.Class}
	}
	// The oracle arm carries no class: a feed names an instrument, not
	// its classification, and inventing one would publish a category
	// nothing declared. Same rule the classic oracle arm follows.
	if isOracleRWACode(c.Symbol) {
		return Verdict{InSet: true, Basis: BasisContractOracleFeed}
	}
	return Verdict{Reject: RejectNoContractBasis}
}

// contractInstrument is one curated binding from a contract address to
// the real-world instrument it holds.
//
// This is the ADR-0040 curated-set mechanism, the same one
// [instrumentBindings] uses one file over: an enumerated allow-list,
// review-gated by living in code, where an unlisted candidate is refused
// and the refusal is REPORTED rather than absorbed.
//
// # The evidence bar for an entry
//
// An entry needs all three, recorded in a comment beside it:
//
//  1. THE ADDRESS, from a primary source. The issuer's own
//     documentation, or a signed statement, naming this exact C-strkey
//     as the contract for the instrument. A block explorer agreeing is
//     corroboration, not a source — explorers read the same curated
//     directories we do, so treating one as independent double-counts a
//     single claim.
//  2. THE INSTRUMENT, named specifically enough to be falsifiable. The
//     fund, note or commodity, not the category. `US Treasury money
//     market fund` is a category; a named fund with a share class is an
//     instrument.
//  3. THE CLASS, from [anchorClasses]. The same closed vocabulary the
//     SEP-1 arm normalises to, so a curated row and a declared row mean
//     the same thing by the same rule.
//
// # What an entry does NOT do
//
// It does not admit anything by itself. C2 runs before C4, and the
// candidate population upstream is drawn from the curated directory
// (buildRWAContractMembership reads DirectoryRecognisedContracts), so a
// contract the directory does not name is never enumerated, never has
// its symbol read, and never reaches this table at all. An entry here
// says only WHICH instrument an address holds, for the day recognition
// of that address exists. Until then it is published on the wire as a
// verified identity the surface is still refusing — which is a different
// and more useful statement than silence.
//
// This set previously shipped empty because the work that built the arm
// had no access to a primary source for any address. That is no longer
// true for the entries below; it remains true for every address not
// listed, and an address written from memory or inferred from a
// dashboard screenshot is still a fabricated identity for a financial
// instrument.
//
// Adding an entry is a code change, exactly as changing the audited
// wasm-hash set is.
type contractInstrument struct {
	// ContractID is the exact C-strkey. Matched exactly: a strkey is
	// CRC-checked and case-significant, and there is no near miss worth
	// accepting.
	ContractID string
	// Instrument names the real-world instrument, for the reviewer.
	Instrument string
	// Class is the [anchorClasses] value.
	Class string
}

// Spiko's tokenized money-market funds.
//
// Spiko Finance is an investment firm licensed by the French Prudential
// Control and Resolution Authority; spiko.io is its own registrable
// domain. The chain from that domain to these exact C-strkeys has no
// third party anywhere in it:
//
//  1. spiko.io links to its own engineering subdomain, tech.spiko.io.
//  2. tech.spiko.io/posts/stellar-integration/ — Spiko's own account of
//     the October 2025 Soroban deployment — publishes the source as
//     github.com/spiko-tech/stellar-contracts.
//  3. That repository's address/production.json names one mainnet
//     contract address per token, and its config/production.json names
//     the fund each address carries.
//
// The ledger CORROBORATES the addresses; it is not where they came from.
// Each contract's own name(), symbol() and decimals() were read over
// public Soroban RPC (2026-09-15) and byte-match config/production.json
// — every name verbatim, every decimals 5. That is the contract talking
// about itself, which admits nothing on its own and is read here for the
// one thing contract metadata is allowed to answer: whether the address
// the primary source named holds the instrument the primary source
// claimed. All seven deployments share one creator account and one wasm
// hash, so they are one deployment event rather than seven coincidences.
//
// # Why an impersonator cannot produce this
//
// The chain is rooted at spiko.io, and the curator picks the domain that
// has to corroborate — the property this file's header names as the
// strongest available strengthener. This network already carries at
// least five accounts issuing classic assets coded EUTBL and USTBL, from
// rwa.xlmhq.org, stellar.dtcc.network, treasury.dtcc.company,
// lumenvaultx.org and rwa.stellarsynth.org, each serving a valid and
// correctly self-bound stellar.toml, several with round duplicated
// supplies across both codes. Every one of them satisfies R2. None of
// them can publish at spiko.io, and none of them appears below — the
// real Spiko issues no classic asset at all, so those five collide with
// a code and nothing else.
//
// # Why the cash-and-carry funds are absent
//
// Spiko's production set also holds SPKCC and eurSPKCC
// ("Spiko Digital Assets Cash and Carry Fund"), deployed and verified on
// the same evidence as the five below. They are deliberately NOT bound:
// a digital-asset basis-trade fund holds crypto and futures, which
// [anchorClasses] excludes on purpose, and no class in that closed
// vocabulary describes it. Forcing one would publish a classification
// nothing supports. Their combined supply is under four million tokens,
// so the omission is a discipline point rather than a material one.
const (
	spikoEUTBL    = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	spikoUSTBL    = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	spikoUKTBL    = "CDT3KU6TQZNOHKNOHNAFFDQZDURVC3MSTL4ML7TUTZGNOPBZCLABP4FR"
	spikoEurUSTBL = "CCFIYXF32QI45KXO43J7XY3DMH6W6DKT7XFDEHA65UG4ONNPWBWR4YMA"
	spikoEurUKTBL = "CCPLGWIIZX6GUIV6JUWJBCGW3PB24ZTHKOVGQQNTTAZIJRRLFJ4PIUMZ"
)

// contractInstruments is the curated set. See [contractInstrument] for
// the evidence bar each entry has to meet, and for what an entry does
// and does not do.
//
// Class is `bond` throughout: each fund is a short-maturity Treasury
// Bill money-market fund, the same instrument class the SEP-1 arm
// accepts as `bond` from Etherfuse's sovereign-debt declarations.
//
// Supply read on 2026-09-15, at the 5 decimals every one of these
// contracts declares. It is recorded because it is the figure a reader
// should be able to falsify, and because two of the five hold none:
//
//	EUTBL     283,278,671.09 tokens
//	USTBL      32,964,659.79
//	UKTBL       9,320,573.45
//	eurUSTBL            none — deployed, never minted
//	eurUKTBL            none — deployed, never minted
//
// The two empty ones are bound anyway. The evidence is per ADDRESS and
// is identical for all five; an unminted token is an empty token, not an
// unidentified one, and binding it now means the day it mints it is
// already named rather than guessed at. This differs from the rule
// [instrumentBindings] applies one file over — bind a CODE only once
// this issuer is observed to have issued it — because that rule guards
// against attaching a code to a guessed issuer, and there is no guess
// here: the address is exact and its metadata was read off the ledger.
var contractInstruments = []contractInstrument{
	{ContractID: spikoEUTBL, Instrument: "Spiko EU T-Bills Money Market Fund (EUTBL)", Class: "bond"},
	{ContractID: spikoUSTBL, Instrument: "Spiko US T-Bills Money Market Fund (USTBL)", Class: "bond"},
	{ContractID: spikoUKTBL, Instrument: "Spiko UK T-Bills Money Market Fund (UKTBL)", Class: "bond"},
	{ContractID: spikoEurUSTBL, Instrument: "Spiko US T-Bills Money Market Fund, EUR share class (eurUSTBL)", Class: "bond"},
	{ContractID: spikoEurUKTBL, Instrument: "Spiko UK T-Bills Money Market Fund, EUR share class (eurUKTBL)", Class: "bond"},
}

// contractInstrumentOf returns the curated binding for a contract, if
// any. Exact match on the address.
func contractInstrumentOf(contractID string) (contractInstrument, bool) {
	id := strings.TrimSpace(contractID)
	for _, b := range contractInstruments {
		if b.ContractID == id {
			return b, true
		}
	}
	return contractInstrument{}, false
}

// ContractInstrumentBinding is one curated contract binding projected
// for the wire.
type ContractInstrumentBinding struct {
	ContractID string
	Instrument string
	Class      string
}

// ContractInstrumentBindings returns the curated set in a stable order,
// so a consumer can audit every contract this surface is willing to
// admit on the curated basis rather than inferring the rule from
// whichever rows carry a figure today.
func ContractInstrumentBindings() []ContractInstrumentBinding {
	out := make([]ContractInstrumentBinding, 0, len(contractInstruments))
	for _, b := range contractInstruments {
		out = append(out, ContractInstrumentBinding(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContractID < out[j].ContractID })
	return out
}

// CouldQualifyContract reports whether a contract could satisfy C4 at
// all, from contract-side inputs alone. It is the counterpart of
// [CouldQualify]: a pre-filter that lets a caller drop candidates before
// paying for a metadata read, never a decision. [QualifyContract] still
// has to run, and C2 and C3 still have to hold.
func CouldQualifyContract(contractID, symbol string) bool {
	if _, ok := contractInstrumentOf(contractID); ok {
		return true
	}
	return isOracleRWACode(symbol)
}
