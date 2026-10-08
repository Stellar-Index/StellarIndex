package rwa

import (
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The contract arm of the definition — how a Soroban-issued token
// qualifies when there is no (code, issuer) pair to bind a declaration
// to. The full argument and its measurements are in
// docs/methodology/rwa-definition.md ("The contract arm").
//
// The arm exists because the real-world issuers checked on production
// issue nothing classic: their Stellar presence is contract-issued, and
// the issuers table (keyed by G-account) never collects them.
//
// R2, the issuer-bound SEP-1 self-declaration, is impossible for a
// contract. Dropping it is not a relaxation: R2 costs an attacker one
// domain registration (19 impersonating BENJI issuers each served a valid,
// correctly-bound stellar.toml), and R3 is what kept them out. So C2
// requires an independent party to name THE EXACT CONTRACT ADDRESS, not
// an entity, domain or deployer. Recognition is per address, so a
// recognised contract carries no other token with it, unlike a classic
// recognised account.
//
// C2 is satisfiable two ways:
//
//  1. THE CURATED DIRECTORY NAMES THE ADDRESS, with an issuing-class
//     tag. It admits ON ITS OWN: it is a review-gated identity system
//     with a scam vocabulary, so it ATTESTS.
//  2. AN INDEPENDENT LISTING DIRECTORY NAMES THE ADDRESS **and** an
//     in-repo curated binding names the same address. Requires TWO
//     sources, and admits on NEITHER alone: a listing map exists for
//     price aggregation and only CORROBORATES, and this repository
//     vouching for itself is never independence.
//
// The listing map is not the curated directory re-published (the reason
// block explorers do not count): the two sets differ in both directions
// with ~1% overlap, disagree about what shared addresses are, and are
// keyed and fielded differently.
//
// Recognition alone is not enough, because the curated set tags
// stablecoins, pool shares and infrastructure the same way. C4 decides
// whether the token is a real-world instrument, never from the
// contract's own unaided word.
//
// Rejected: deployer provenance (we hold no deployer edge for arbitrary
// contracts), SEP-41 metadata as admission (free for an impersonator;
// read only after an independent party named the address), and the
// issuer's SEP-1 naming the contract (the strongest strengthener, but our
// parser does not read the field and most issuers have no fetched
// payload; it is the next arm).

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

// Contract-arm recognition sources. They name WHICH independent party's
// naming satisfied C2, and are served on the row so a consumer can
// filter on the strength of the recognition rather than trusting the
// membership decision wholesale — the same reason the C4 bases are
// served beside them.
//
// The two are NOT interchangeable, and the difference is the subject of
// this file's "Two ways to satisfy C2" section.
const (
	// RecognitionCuratedDirectory — the curated third-party account
	// directory holds an entry for this exact address with an
	// issuing-class tag. Admits on its own.
	RecognitionCuratedDirectory = "curated_account_directory"
	// RecognitionListingCorroborated — an independent listing directory
	// names this exact address AND an in-repo curated binding names the
	// same address. Requires BOTH; neither admits alone.
	RecognitionListingCorroborated = "independent_listing_corroborating_curated_binding"
)

// ContractRecognitionSources lists the recognition vocabulary in a
// stable order, served for the same reason [ContractRecognitionTags]
// is: a consumer reads the rule from the response rather than inferring
// it from whichever rows qualified today.
func ContractRecognitionSources() []string {
	return []string{RecognitionCuratedDirectory, RecognitionListingCorroborated}
}

// Contract-arm refusals. Distinct constants rather than reuse of the
// classic reasons, because the ACTION each implies is different: a
// contract absent from the directory needs a curated entry, while a
// classic asset failing R2 needs its issuer to publish a file.
//
// The four documented constants below exist because C2 has two arms,
// and a single reason covering both would tell a reader that
// recognition failed without saying which half of which arm was
// missing — which is the one thing that determines who can act. They
// are kept apart from [RejectContractNotNamed], which keeps its
// narrow meaning exactly: nobody named this address.
const (
	RejectNotContract      = "not_a_contract_address"
	RejectContractNotNamed = "contract_not_named_in_directory"
	RejectContractNoTag    = "contract_named_without_issuing_tag"
	RejectContractScam     = "contract_scam_flagged"
	RejectNoContractBasis  = "no_real_world_instrument_basis_for_contract"

	// RejectContractListedNotCurated — an independent listing directory
	// names the address and NO in-repo curated binding does. This is
	// arm 2 failing on its second half, and it is the refusal that
	// keeps a listing from admitting anything by itself.
	//
	// It is the commonest refusal on that arm by a wide margin and it
	// is meant to be: the listing set is built for price aggregation
	// and holds stablecoin contracts, wrapped bitcoin and the native
	// asset's own SAC alongside anything else with a market. Every one
	// of those is named by an independent party and none of them is
	// admitted, because the second source — the one that says WHICH
	// real-world instrument the address holds, to the evidence bar
	// [contractInstrument] documents — has never been written for them.
	RejectContractListedNotCurated = "contract_listed_without_curated_binding"
	// RejectContractCuratedNotListed — an in-repo curated binding names
	// the address and no independent party does. Arm 2 failing on its
	// FIRST half.
	//
	// This is the refusal that holds the independence requirement up,
	// and the one worth reading twice: the address is identified, its
	// instrument is named, its class is known, its supply is in the
	// lake and a price for it may well exist. Everything needed to
	// publish a number is present except somebody other than this
	// repository saying the address is what this repository says it is.
	// Publishing it anyway would be this surface vouching for itself.
	RejectContractCuratedNotListed = "contract_curated_binding_without_independent_listing"
	// RejectContractListingUnavailable — the listing read did not
	// answer, or its most recent answer is older than the recognition
	// bound, so arm 2 is CLOSED for this rebuild.
	//
	// Distinct from [RejectContractCuratedNotListed] for the reason
	// [RWAPremiumReferenceUnavailable] is distinct from every refusal
	// beside it: a read that did not answer is not entitled to report
	// an absence as a finding. A bound contract refused under this
	// reason may well be listed; nobody looked, or nobody looked
	// recently enough.
	//
	// It is also the shape of the fail-closed guarantee. When the
	// listing goes away the set SHRINKS, and this reason is what stops
	// that shrink from being silent.
	//
	// It names ONE source: the listing directory. A refusal caused by
	// any other read failing must not borrow it — see
	// [RejectContractCuratedTagsUnavailable].
	RejectContractListingUnavailable = "independent_listing_unavailable"
	// RejectContractCuratedTagsUnavailable — the CURATED DIRECTORY's tag
	// read did not answer, so C3 could not be evaluated and arm 2 is
	// closed for this rebuild.
	//
	// Arm 2 depends on two sources that have nothing to do with each
	// other. The listing directory supplies its recognition; the curated
	// directory supplies the scam vocabulary C3 reads, and C3 refuses a
	// flagged address whatever named it. Either read failing closes the
	// arm, and until this constant existed both closed it under
	// [RejectContractListingUnavailable].
	//
	// That conflation was a defect of the same kind this whole surface
	// exists to prevent, one level up: a refusal reason is an
	// instruction to somebody, and these two instruct different people
	// to look in different places. Worse, the two states are not even
	// correlated — the curated directory can be unreadable while the
	// listing directory sits there perfectly fresh, and the funnel would
	// then report an outage of the source that ANSWERED and send an
	// operator to a working sync.
	//
	// Same actor as the reason above, deliberately: an operator fixes
	// both. The actor says WHO acts and the reason says WHERE, and it
	// was the second of those that was being destroyed.
	RejectContractCuratedTagsUnavailable = "curated_tag_lookup_unavailable"
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
	//
	// Read on BOTH C2 arms, not only the first. The scam vocabulary
	// lives here and nowhere else, so a candidate reaching the second
	// arm still has to have had this column looked up — otherwise the
	// scam precedence would hold only for the population that did not
	// need it.
	DirectoryTags []string
	// ListingNamed reports whether an independent LISTING directory —
	// a price-aggregation platform's own per-coin map from its coin ids
	// to the chain addresses that carry them — names THIS EXACT
	// address on Stellar. Established by the caller from a cached read;
	// this package never assumes a naming it did not see.
	//
	// Half of C2's second arm. On its own it admits NOTHING, and the
	// refusal that says so is [RejectContractListedNotCurated].
	ListingNamed bool
	// ListingAvailable reports whether the listing read ANSWERED, and
	// answered recently enough to be reused. False covers three cases a
	// caller cannot distinguish and should not have to: no listing
	// reader is wired, the read failed, or the cached snapshot is past
	// the recognition bound.
	//
	// All three close arm 2, and they close it the same way: a
	// candidate that would have needed it is refused under
	// [RejectContractListingUnavailable] rather than admitted on a
	// recognition nobody re-established. ListingNamed is meaningless
	// when this is false, and the verdict never reads it.
	ListingAvailable bool
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
	// C3 — NOT SCAM-FLAGGED. Hoisted ABOVE C2 rather than sitting
	// inside it, because C2 now has two arms and the scam precedence
	// has to apply to both. Left where it was, an address the curated
	// directory named ONLY to flag it could still have been admitted by
	// the listing arm, which does not carry flags and never will — the
	// precedence would have held for exactly the population that did
	// not need it.
	//
	// The reported reason is unchanged for the population that reaches
	// it today: an address carrying both a recognition tag and a
	// scam-class tag was already refused as flagged, which is the
	// stronger and more useful statement, and still is. The scam
	// vocabulary is read from the ONE list in
	// timescale.DirectoryScamFlagTags via [ScamFlagged], so a contract
	// whose price the serving gates withhold can never be admitted here
	// by any arm.
	if ScamFlagged(c.DirectoryTags) {
		return Verdict{Reject: RejectContractScam}
	}
	// C2 — INDEPENDENT NAMING OF THIS EXACT ADDRESS. The requirement
	// that replaces R2, and the one an attacker has to defeat.
	recognition, reject := contractRecognitionOf(c)
	if recognition == "" {
		return Verdict{Reject: reject}
	}
	// C4 — REAL-WORLD INSTRUMENT. The curated binding first: it names
	// the instrument AND its class, so it produces a strictly more
	// informative row than the oracle arm and should win when both
	// apply.
	if b, ok := contractInstrumentOf(c.ContractID); ok {
		return Verdict{
			InSet: true, Basis: BasisCuratedContract,
			AnchorClass: b.Class, Recognition: recognition,
		}
	}
	// The oracle arm carries no class: a feed names an instrument, not
	// its classification, and inventing one would publish a category
	// nothing declared. Same rule the classic oracle arm follows.
	//
	// Unreachable under [RecognitionListingCorroborated] by
	// construction — that arm requires a curated binding, which the
	// branch above has already answered — so a row admitted here was
	// recognised by the directory. It is written as a general branch
	// rather than gated on the recognition source because the gate
	// would be a second statement of the same fact, and the two could
	// drift.
	if isOracleRWACode(c.Symbol) {
		return Verdict{InSet: true, Basis: BasisContractOracleFeed, Recognition: recognition}
	}
	return Verdict{Reject: RejectNoContractBasis}
}

// contractRecognitionOf applies C2's two arms and returns either the
// recognition source that satisfied it, or the refusal that names what
// was missing. Exactly one of the two returns is non-empty.
//
// Called with the scam check already made, so neither arm has to
// re-state it and neither can forget to.
//
// # The order
//
// Arm 1 is tried first because it admits on its own and produces the
// stronger row. Arm 2 is tried second, and is also allowed to rescue an
// address the directory named WITHOUT an issuing tag: the two arms rest
// on different evidence, and a directory entry that describes the
// address as something other than an issuer is simply not evidence
// arm 2 uses.
//
// # The refusals
//
// Reported most-specific-first, and the order is the order of what
// would have to change:
//
//   - a directory entry without an issuing tag is still reported as
//     [RejectContractNoTag], unchanged, because the directory naming an
//     address is the strongest signal present and its tag is the one
//     thing that would have had to differ;
//   - then the fail-closed case, so an outage never masquerades as a
//     finding about the data;
//   - then the two halves of arm 2, each naming the half that answered
//     so the half that did not is implied exactly;
//   - and finally the original reason, which keeps its original
//     meaning: nobody named this address at all.
func contractRecognitionOf(c ContractCandidate) (recognition, reject string) {
	_, bound := contractInstrumentOf(c.ContractID)
	listed := c.ListingAvailable && c.ListingNamed

	// ARM 1 — THE CURATED ACCOUNT DIRECTORY, ALONE.
	//
	// Admits by itself, and the file header says why it may: it is a
	// Stellar-specific, ADDRESS-level directory whose whole purpose is
	// identity attestation, synced from a reviewed third-party
	// repository, and it carries the scam vocabulary that C3 reads.
	// Landing a false entry in it means landing a reviewed change in
	// somebody else's published set under the name of the entity being
	// impersonated.
	if c.DirectoryNamed && HasContractRecognitionTag(c.DirectoryTags) {
		return RecognitionCuratedDirectory, ""
	}
	// ARM 2 — AN INDEPENDENT LISTING AND A CURATED BINDING, TOGETHER.
	//
	// Requires TWO sources that do not read each other, and admits on
	// NEITHER alone. The asymmetry with arm 1 is deliberate and is
	// argued in the file header: a listing map is a convenience built
	// for price aggregation, not an adversarial identity system, so it
	// CORROBORATES a claim rather than attesting to one.
	if listed && bound {
		return RecognitionListingCorroborated, ""
	}

	switch {
	case c.DirectoryNamed:
		return "", RejectContractNoTag
	case !c.ListingAvailable && bound:
		return "", RejectContractListingUnavailable
	case c.ListingNamed && c.ListingAvailable:
		return "", RejectContractListedNotCurated
	case bound:
		return "", RejectContractCuratedNotListed
	default:
		return "", RejectContractNotNamed
	}
}

// contractInstrument is one curated binding from a contract address to
// the real-world instrument it holds: the ADR-0040 curated-set mechanism
// [instrumentBindings] also uses, where an unlisted candidate is refused
// and the refusal is REPORTED rather than absorbed.
//
// # The evidence bar for an entry
//
// An entry needs all three, recorded in a comment beside it:
//
//  1. THE ADDRESS, from a primary source: the issuer's own documentation,
//     or a signed statement, naming this exact C-strkey. A block explorer
//     only corroborates; explorers read the same curated directories we
//     do. An address written from memory or inferred from a screenshot is
//     a fabricated identity for a financial instrument.
//  2. THE INSTRUMENT, named specifically enough to be falsifiable: a named
//     fund with a share class, not `US Treasury money market fund`.
//  3. THE CLASS, from [anchorClasses], the same closed vocabulary the
//     SEP-1 arm normalises to.
//
// # What an entry does NOT do
//
// It never admits anything by itself. It is read by
// [contractRecognitionOf] as the SECOND half of C2's arm 2, worthless
// without an independent listing naming the same address, and by
// [QualifyContract] at C4, which only runs on a candidate C2 admitted. An
// address listed here and named by nobody else is refused under
// [RejectContractCuratedNotListed], with its instrument and class on the
// wire beside the refusal.
//
// Every entry is in the candidate population, so an entry whose
// recognition never arrives appears under a reason instead of vanishing.
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
// public Soroban RPC and byte-match config/production.json
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
// the same evidence as the nine below. They are deliberately NOT bound:
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

	// The Spiko Amundi Overnight Swap Fund, one address per share
	// class. Co-created with a fund manager, swapped with a bank
	// counterparty, and a UCITS with an ISIN per class.
	//
	// These are `fund`, not `bond`, and the distinction is the reason
	// [contractAnchorClasses] exists. The fund holds 152 listed equities
	// (119% of net assets) and hands every penny of their return to the
	// swap counterparty in exchange for the overnight index rate: `bond`
	// would be false on the assets and false on the exposure, `stock`
	// would be true of the assets and the exact opposite of the
	// instrument. Held unbound until the vocabulary had a word for it.
	spikoEurSAFO = "CBOOCGZSVRSZFRE4U2NWR2B4RXYVJWRCBTGOUD2JPI2TDJPWMTJX7FZP"
	spikoSAFO    = "CDGSC6BA4TCAOVSFQCUEHDMOIIHYYVNYBT6YEARS4MX3ITAHUINVGQHX"
	spikoGbpSAFO = "CAGYRRKPFSWKM6SJOE4QAAVYMOSHMDS5WOQ4T5A2E6XNCU7LZZKUNQKP"
	spikoChfSAFO = "CAJD2IBSP7VO2VYJQUYJSOGPJINTUYV7MQITINXVPTIH3CCLCUENNMW4"
)

// Matrixdock's tokenized gold.
//
// XAUm is one token per troy ounce of London Good Delivery gold held in
// Singapore vaults, issued by Matrixdock, Matrixport's real-world-asset
// arm. matrixdock.com is its own registrable domain, and the chain from
// that domain to this C-strkey has no third party in it: the XAUm
// product page at matrixdock.com/xaum lists the chains the token is
// available on, and its Stellar entry names this exact contract address.
//
// Two independent corroborations, neither of which is where the address
// came from:
//
//   - The deployed code is SOURCE-VERIFIED: the on-chain wasm hash
//     2f91a70d…abdd reproduces from github.com/Matrixdock-RWA/RWA-Contracts
//     at path `xaum-stellar`, package `xaum`. That is a stronger
//     statement than any of the Spiko bindings carry — it says the
//     contract executing at this address is built from published source,
//     not merely that its metadata agrees with a manifest.
//   - The independent listing directory names the same address under
//     `matrixdock-gold`, which is what C2's second arm requires and what
//     admits this row at all.
//
// The ledger agrees about what it is: code XAUM, token name
// "Matrixdock Gold", decimals 9 read from the contract's own instance
// storage, 1,060.884000000 tokens outstanding when read, with 38
// trades across 5 markets in the preceding day.
//
// # Why `commodity` and not `fund`
//
// The distinction [contractAnchorClasses] was widened for cuts the other
// way here. XAUm is not a claim on a portfolio managed by anyone; it is
// a bearer claim on a specific quantity of a specific metal, redeemable
// for the metal. `commodity` is exactly the word, and it is the same
// word the SEP-1 arm already accepts from issuers declaring
// anchor_asset_type="commodity". This is the first commodity row on this
// surface for which an independent price exists at all — the two
// classic ones carry supply and no feed.
const matrixdockXAUM = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

// Centrifuge's deRWA tokens: Soroban-native wrappers of two Janus
// Henderson Anemoy funds, issued on Stellar by the Open Market
// Foundation. Neither has a classic (code, issuer) pair, so no SEP-1
// arm can reach them.
//
// The addresses come from the issuer's own SEP-1 file at
// centrifuge.io/.well-known/stellar.toml: its
// [[CURRENCIES]] entries for code deJTRSY and deJAAA each carry a
// `contract` field naming exactly these C-strkeys, display_decimals 18.
// The curator picked centrifuge.io, so a lookalike domain cannot stand
// in for it. Corroboration, not source: the ledger's own decimals() for
// deJTRSY is 18, its supply reproduces to the unit by event flows and by
// contract storage (docs/methodology/contract-storage-supply.md), and
// the independent listing directory names the deJTRSY address, which is
// what admits it under C2's second arm.
//
// Classes. deJTRSY is `bond`: the toml declares anchor_asset_type
// "bond", anchor "US Treasury Bills" — the same instrument class as
// Spiko's T-Bill funds. deJAAA is `bond` too, but the toml declares
// "other" / "AAA CLO", which the vocabulary excludes; the fund holds
// AAA-rated CLO debt tranches, so `bond` is true of its assets and its
// exposure, where `fund` would say only that someone manages it.
//
// The same toml names the underlying JTRSY and JAAA contracts. They are
// deliberately NOT bound: the deRWA tokens are wrappers of the same
// funds, and binding both before measuring what backs the wrapper risks
// counting one holding twice.
const (
	centrifugeDeJTRSY = "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV"
	centrifugeDeJAAA  = "CC64WBDGS6QQP22QTTIACYIXT3WF7BBQEYOQPLTP7GTKYY7PZ74QYGSL"
)

// contractInstruments is the curated set. See [contractInstrument] for
// the evidence bar each entry has to meet, and for what an entry does
// and does not do.
//
// The T-Bill funds are `bond`: each is a short-maturity Treasury Bill
// money-market fund, the same instrument class the SEP-1 arm accepts as
// `bond` from Etherfuse's sovereign-debt declarations. The overnight
// swap fund's share classes are `fund`, for the reason recorded above
// their addresses and argued at [contractAnchorClasses] — the same
// issuer, a different instrument, and the vocabulary now has a word for
// the difference rather than rounding it to the nearest wrong one.
//
// Spiko supply was read at the 5 decimals each of the nine
// Spiko contracts declares — decoded from each contract's own METADATA
// map, not assumed (XAUm declares 9; see above). It is recorded because
// it is the figure a reader should be able to falsify, and because three
// of the nine hold none:
//
//	eurSAFO   918,684,368.85782 tokens
//	EUTBL     283,278,671.09034
//	SAFO       65,690,636.43583
//	USTBL      36,216,376.34835
//	gbpSAFO    30,414,646.27691
//	UKTBL       9,295,007.40439
//	chfSAFO             none — 1,000,000 shares exist, none on Stellar
//	eurUSTBL            none — deployed, never minted
//	eurUKTBL            none — deployed, never minted
//
// Each figure above is three measurements that agree TO THE UNIT, not
// one: the lake's mint−burn−clawback, the contract's own TotalSupply in
// instance storage, and the sum of every per-holder Balance entry in
// contract storage. The three arrive by independent paths, so a wrong
// decimals exponent could not survive all of them.
//
// The two empty ones are bound anyway. The evidence is per ADDRESS and
// is identical for all nine; an unminted token is an empty token, not an
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
	{ContractID: spikoEurSAFO, Instrument: "Spiko Amundi Overnight Swap Fund, EUR share class (eurSAFO)", Class: "fund"},
	{ContractID: spikoSAFO, Instrument: "Spiko Amundi Overnight Swap Fund, USD share class (SAFO)", Class: "fund"},
	{ContractID: spikoGbpSAFO, Instrument: "Spiko Amundi Overnight Swap Fund, GBP share class (gbpSAFO)", Class: "fund"},
	{ContractID: spikoChfSAFO, Instrument: "Spiko Amundi Overnight Swap Fund, CHF share class (chfSAFO)", Class: "fund"},
	{ContractID: matrixdockXAUM, Instrument: "Matrixdock Gold (XAUm)", Class: "commodity"},
	{ContractID: centrifugeDeJTRSY, Instrument: "Janus Henderson Anemoy Treasury Fund, Centrifuge deRWA token (deJTRSY)", Class: "bond"},
	{ContractID: centrifugeDeJAAA, Instrument: "Janus Henderson Anemoy AAA CLO Fund, Centrifuge deRWA token (deJAAA)", Class: "bond"},
}

// ContractInstrumentClass reports whether a class string is one a
// curated binding may carry. Exported for the guard that holds
// [contractInstruments] to [contractAnchorClasses]; the bindings are a
// hand-maintained literal, so nothing else would notice a typo or a
// class invented in a hurry to get an address admitted.
func ContractInstrumentClass(class string) bool {
	_, ok := contractAnchorClasses[strings.ToLower(strings.TrimSpace(class))]
	return ok
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
