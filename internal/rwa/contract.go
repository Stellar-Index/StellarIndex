package rwa

import (
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The contract arm: how a Soroban-issued token qualifies with no (code, issuer) pair to
// bind a declaration to (docs/methodology/rwa-definition.md, "The contract arm").
//
// R2 is impossible for a contract and was never the guard (19 BENJI impersonators served
// valid, bound tomls; R3 kept them out). So C2 requires an independent party to name THIS
// EXACT ADDRESS, either:
//
//  1. the curated directory, with an issuing-class tag: a review-gated identity system
//     with a scam vocabulary, so it ATTESTS and admits alone; or
//  2. an independent listing directory AND an in-repo curated binding: a listing exists
//     for price aggregation and only CORROBORATES, and this repo vouching for itself is
//     never independence. The two sets overlap ~1%, so one is not the other re-published.
//
// C4 then decides whether the token is a real-world instrument, never on the contract's
// own unaided word. Deployer provenance, SEP-41 metadata as admission, and issuer SEP-1
// naming the contract were rejected (no deployer edge; free to forge; not parsed yet).

// Contract-arm bases name WHY a contract token is an instrument (C4), served beside the
// classic bases so a consumer can filter on evidence strength.
const (
	// BasisCuratedContract: an ADR-0040 in-repo curated entry binds this exact address to a
	// named instrument and class.
	BasisCuratedContract = "curated_contract_instrument"
	// BasisContractOracleFeed: the contract-authored SEP-41 symbol is an ADR-0028 allow-listed
	// RWA code. Code-only identity on its own; admissible only because C2 already recognised
	// the address, as [BasisOracleFeed] is after R3.
	BasisContractOracleFeed = "contract_oracle_rwa_feed"
)

// Contract-arm recognition sources name WHICH party satisfied C2, served so a consumer can
// filter on recognition strength; the two are not interchangeable.
const (
	// RecognitionCuratedDirectory: the curated directory names this address with an
	// issuing-class tag. Admits alone.
	RecognitionCuratedDirectory = "curated_account_directory"
	// RecognitionListingCorroborated: a listing directory and an in-repo curated binding both
	// name this address. Neither admits alone.
	RecognitionListingCorroborated = "independent_listing_corroborating_curated_binding"
)

// ContractRecognitionSources lists the recognition vocabulary in a stable order.
func ContractRecognitionSources() []string {
	return []string{RecognitionCuratedDirectory, RecognitionListingCorroborated}
}

// Contract-arm refusals are distinct from the classic ones because the action differs (a
// curated entry, not an issuer file), and C2's two arms get one reason per missing half so
// the refusal says who can act.
const (
	RejectNotContract      = "not_a_contract_address"
	RejectContractNotNamed = "contract_not_named_in_directory"
	RejectContractNoTag    = "contract_named_without_issuing_tag"
	RejectContractScam     = "contract_scam_flagged"
	RejectNoContractBasis  = "no_real_world_instrument_basis_for_contract"

	// RejectContractListedNotCurated: listed, no curated binding; arm 2 failing on its second
	// half. The commonest refusal by design: the listing holds stablecoins, wrapped BTC and the
	// XLM SAC, none bound to a real-world instrument.
	RejectContractListedNotCurated = "contract_listed_without_curated_binding"
	// RejectContractCuratedNotListed: bound here, named by nobody else. Everything needed to
	// publish is present except independence; publishing would be this surface vouching for itself.
	RejectContractCuratedNotListed = "contract_curated_binding_without_independent_listing"
	// RejectContractListingUnavailable: the listing read failed or is past the recognition
	// bound, so arm 2 is closed. A read that did not answer may not report an absence, and
	// this reason keeps the fail-closed shrink visible. Names the listing source only.
	RejectContractListingUnavailable = "independent_listing_unavailable"
	// RejectContractCuratedTagsUnavailable: the curated directory's tag read failed, so C3
	// cannot run and arm 2 is closed. Kept apart from the listing outage because the two are
	// uncorrelated and send an operator to different syncs.
	RejectContractCuratedTagsUnavailable = "curated_tag_lookup_unavailable"
)

// contractRecognitionTags is narrower than [recognitionTags]: on a contract, `exchange` and
// `defi` mark AMM pools and routers (the directory tags Aquarius pools so) and `sdf` issues
// nothing. A contract without one is refused under [RejectContractNoTag] and counted.
var contractRecognitionTags = map[string]struct{}{
	"issuer":    {},
	"anchor":    {},
	"custodian": {},
}

// ContractRecognitionTags lists the contract recognition vocabulary in a stable order.
func ContractRecognitionTags() []string {
	out := make([]string, 0, len(contractRecognitionTags))
	for t := range contractRecognitionTags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// HasContractRecognitionTag matches tags case-insensitively after trimming, like every
// other reader of this column.
func HasContractRecognitionTag(tags []string) bool {
	for _, t := range tags {
		if _, ok := contractRecognitionTags[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// ContractCandidate is one contract token with every input C1-C4 read; no valuation, so a
// market can never change membership.
type ContractCandidate struct {
	// ContractID is the C-strkey and the whole identity; two contracts calling themselves BENJI
	// are two assets.
	ContractID string
	// DirectoryNamed: the curated directory names this exact address, as the caller observed.
	DirectoryNamed bool
	// DirectoryTags is read on BOTH arms: the scam vocabulary lives only here, so the listing
	// arm must not skip the lookup.
	DirectoryTags []string
	// ListingNamed: a price-aggregation platform's coin-to-address map names this exact
	// address on Stellar. Half of arm 2; admits nothing alone.
	ListingNamed bool
	// ListingAvailable: the listing read answered within the recognition bound. When false
	// (no reader, failed read, stale cache) arm 2 is closed and ListingNamed is ignored.
	ListingAvailable bool
	// Symbol is the contract-authored METADATA symbol, used only by [BasisContractOracleFeed]
	// after recognition; empty when not captured.
	Symbol string
}

// QualifyContract applies C1-C4 in order; the reported reason is the requirement that
// would have had to change first.
func QualifyContract(c ContractCandidate) Verdict {
	// C1: a CRC-checked contract address; never join a supply query on an unverified id.
	if !canonical.IsContractID(strings.TrimSpace(c.ContractID)) {
		return Verdict{Reject: RejectNotContract}
	}
	// C3 runs before C2 so the scam flag binds both arms: the listing arm carries no flags.
	// The vocabulary is [ScamFlagged]'s one list, so nothing the serving gates withhold can
	// be admitted here.
	if ScamFlagged(c.DirectoryTags) {
		return Verdict{Reject: RejectContractScam}
	}
	// C2: independent naming of this exact address, the requirement an attacker must defeat.
	recognition, reject := contractRecognitionOf(c)
	if recognition == "" {
		return Verdict{Reject: reject}
	}
	// C4: the curated binding first, since it also names the class.
	if b, ok := contractInstrumentOf(c.ContractID); ok {
		return Verdict{
			InSet: true, Basis: BasisCuratedContract,
			AnchorClass: b.Class, Recognition: recognition,
		}
	}
	// The oracle arm carries no class (a feed names no classification). Unreachable under
	// arm 2, which requires a binding; left ungated so the fact is not stated twice.
	if isOracleRWACode(c.Symbol) {
		return Verdict{InSet: true, Basis: BasisContractOracleFeed, Recognition: recognition}
	}
	return Verdict{Reject: RejectNoContractBasis}
}

// contractRecognitionOf returns exactly one of the recognition source or the refusal; the
// scam check has already run. Arm 1 is tried first (stronger row); arm 2 may rescue an
// untagged directory entry. Refusals go most-specific-first: untagged entry, outage,
// whichever arm-2 half answered, then nobody named it.
func contractRecognitionOf(c ContractCandidate) (recognition, reject string) {
	_, bound := contractInstrumentOf(c.ContractID)
	listed := c.ListingAvailable && c.ListingNamed

	// Arm 1: an address-level, reviewed third-party identity directory admits alone.
	if c.DirectoryNamed && HasContractRecognitionTag(c.DirectoryTags) {
		return RecognitionCuratedDirectory, ""
	}
	// Arm 2: two sources that do not read each other, admitting only together.
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

// contractInstrument is one ADR-0040 curated binding. An entry needs, recorded beside it:
// the address from a primary source (issuer docs or a signed statement, not an explorer
// or memory), a falsifiable instrument name, and an [anchorClasses] class. It never admits
// alone: it is arm 2's second half, so a bound address nobody else names is refused under
// [RejectContractCuratedNotListed] and still appears with its reason.
type contractInstrument struct {
	// ContractID is matched exactly: a strkey is CRC-checked and case-significant.
	ContractID string
	// Instrument names the real-world instrument, for the reviewer.
	Instrument string
	// Class is the [anchorClasses] value.
	Class string
}

// Spiko's tokenized money-market funds. Chain with no third party: spiko.io → tech.spiko.io
// → github.com/spiko-tech/stellar-contracts address/production.json; on-chain metadata and
// a shared creator and wasm hash corroborate. Five lookalike issuers of classic EUTBL/USTBL
// satisfy R2 but cannot publish at spiko.io. SPKCC/eurSPKCC (crypto basis trade) stay
// unbound: no class in the vocabulary describes them.
const (
	spikoEUTBL    = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	spikoUSTBL    = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	spikoUKTBL    = "CDT3KU6TQZNOHKNOHNAFFDQZDURVC3MSTL4ML7TUTZGNOPBZCLABP4FR"
	spikoEurUSTBL = "CCFIYXF32QI45KXO43J7XY3DMH6W6DKT7XFDEHA65UG4ONNPWBWR4YMA"
	spikoEurUKTBL = "CCPLGWIIZX6GUIV6JUWJBCGW3PB24ZTHKOVGQQNTTAZIJRRLFJ4PIUMZ"

	// Spiko Amundi Overnight Swap Fund share classes are `fund`: it holds equities but swaps
	// their return for the overnight rate, so `bond` and `stock` are both false.
	spikoEurSAFO = "CBOOCGZSVRSZFRE4U2NWR2B4RXYVJWRCBTGOUD2JPI2TDJPWMTJX7FZP"
	spikoSAFO    = "CDGSC6BA4TCAOVSFQCUEHDMOIIHYYVNYBT6YEARS4MX3ITAHUINVGQHX"
	spikoGbpSAFO = "CAGYRRKPFSWKM6SJOE4QAAVYMOSHMDS5WOQ4T5A2E6XNCU7LZZKUNQKP"
	spikoChfSAFO = "CAJD2IBSP7VO2VYJQUYJSOGPJINTUYV7MQITINXVPTIH3CCLCUENNMW4"
)

// Matrixdock's XAUm, one token per troy ounce of LGD gold. Address from the matrixdock.com/xaum
// product page; the wasm reproduces from github.com/Matrixdock-RWA/RWA-Contracts, and the
// listing names it (arm 2). `commodity`, not `fund`: a bearer claim on metal.
const matrixdockXAUM = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

// Centrifuge deRWA wrappers of two Anemoy funds, addresses from centrifuge.io's SEP-1
// `contract` fields. Both `bond` (T-Bills; AAA CLO debt). The underlying JTRSY/JAAA
// contracts stay unbound so one holding is not counted twice.
const (
	centrifugeDeJTRSY = "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV"
	centrifugeDeJAAA  = "CC64WBDGS6QQP22QTTIACYIXT3WF7BBQEYOQPLTP7GTKYY7PZ74QYGSL"
)

// contractInstruments is the curated set; see [contractInstrument]. Unminted addresses
// (chfSAFO on Stellar, eurUSTBL, eurUKTBL) are bound anyway: the evidence is per address,
// so the day one mints it is already named. Spiko supplies were checked three ways (lake
// flows, instance TotalSupply, summed balances) at the declared 5 decimals.
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

// ContractInstrumentClass reports whether a curated binding may carry this class; it backs
// the guard on the hand-maintained [contractInstruments] literal.
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

// ContractInstrumentBinding is one curated contract binding projected for the wire.
type ContractInstrumentBinding struct {
	ContractID string
	Instrument string
	Class      string
}

// ContractInstrumentBindings returns the curated set in a stable order for audit.
func ContractInstrumentBindings() []ContractInstrumentBinding {
	out := make([]ContractInstrumentBinding, 0, len(contractInstruments))
	for _, b := range contractInstruments {
		out = append(out, ContractInstrumentBinding(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContractID < out[j].ContractID })
	return out
}

// CouldQualifyContract is [CouldQualify]'s contract counterpart: a pre-filter before the
// metadata read, never a decision.
func CouldQualifyContract(contractID, symbol string) bool {
	if _, ok := contractInstrumentOf(contractID); ok {
		return true
	}
	return isOracleRWACode(symbol)
}
