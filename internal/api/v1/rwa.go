package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — tokenized real-world assets on Stellar.
//
// The membership rule is `internal/rwa` and is documented for readers
// at docs/methodology/rwa-definition.md; this file is the read path and
// the wire shape. Two properties of the design are load-bearing and
// easy to erode:
//
//  1. MEMBERSHIP IS DECIDED BEFORE VALUATION. The set is built from
//     identity and attestation only. No number can move an asset in or
//     out, so a withheld price cannot silently shrink the set and a
//     large market cap cannot buy a place in it.
//
//  2. VALUATION COMES FROM THE EXISTING LISTING PIPELINE, UNCHANGED.
//     The same store query, the same substance gate, the same
//     supply-derived market cap, the same dust guard and the same
//     scam-issuer payload suppression that /v1/assets runs. This
//     surface adds no price path of its own, so it cannot publish a
//     figure /v1/assets would have withheld.
//
// Where a valuation is unavailable the row says so in
// `valuation.status` and carries no number. Nothing here renders a
// withheld or missing figure as zero.

// rwaMembershipTTL bounds how long the membership set is reused before
// a rebuild. Its inputs move on daily cadences — the SEP-1 refresh cron
// and the directory sync — so ten minutes keeps the scan off the
// request path while a newly-recognised issuer still appears the same
// hour. Matches the SEP-1 logo map beside it.
const rwaMembershipTTL = 10 * time.Minute

// rwaMaxIssuers bounds the per-issuer listing reads one rebuild will
// make. The qualifying issuer set is small by construction (four on the
// production network, 2026-09-05) because R3 requires an independent
// party to have named the account; the cap is a guard against a
// directory sync that suddenly tags thousands of accounts, not an
// expected condition. When it binds, the surface says so rather than
// serving a silently truncated set.
const rwaMaxIssuers = 64

// rwaAssetsPerIssuer bounds one issuer's listing page. An issuer with
// more classic assets than this has its long tail unread; the cap is
// reported the same way the issuer cap is.
const rwaAssetsPerIssuer = 500

// Sep1BoundCurrencyReader is the storage seam the membership build
// reads its attestations through. *timescale.Store satisfies it.
// Optional: a deployment without it serves an empty set with a stated
// reason rather than an error, exactly as the logo overlay degrades.
type Sep1BoundCurrencyReader interface {
	BoundSep1Currencies(
		ctx context.Context, keep timescale.Sep1CurrencyFilter,
	) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error)
}

// ─── wire shape ─────────────────────────────────────────────────────

// RWAAssetsView is the /v1/rwa/assets payload: the set, its aggregates
// and the rule that produced it.
type RWAAssetsView struct {
	// Definition restates the membership rule in the response so a
	// consumer reads it from the same document as the rows, rather than
	// inferring it from whichever assets happen to qualify today.
	Definition RWADefinition `json:"definition"`
	// Summary is the aggregate over the assets below.
	Summary RWASummary `json:"summary"`
	// Assets is the set, ordered by published market cap descending,
	// then by observation count. Rows with no published valuation sort
	// after every valued row — an unvalued asset is never ranked above
	// a valued one on a number it does not have.
	Assets []RWAAsset `json:"assets"`
	// ByClass totals the set per declared real-world class. Assets
	// admitted on the oracle basis declare no class and are grouped
	// under `unclassified`.
	ByClass []RWAGroupTotal `json:"by_class"`
	// ByIssuer totals the set per issuer G-address.
	ByIssuer []RWAIssuerTotal `json:"by_issuer"`
	// Refused counts the candidates each requirement turned away, so
	// the served set is never mistaken for the whole population of
	// assets that CLAIM to be real-world assets.
	//
	// It is the ORDERED requirement tally over the candidates that
	// reached the full R1→R4 evaluation. It is NOT the whole
	// population: the stages upstream of that evaluation — an issuer
	// with no attestation, a payload that would not decode, an entry
	// naming somebody else's account — are accounted for in Funnel,
	// which is the complete narrowing.
	Refused []RWARefusal `json:"refused"`
	// Funnel accounts for the ENTIRE population this set was narrowed
	// from, stage by stage, with a named reason on every drop.
	Funnel RWAFunnel `json:"funnel"`
	// UnreachedEntities names curated-directory entities that a third
	// party recognises as issuing or custodying value, that carry no
	// scam-class tag, and for which this index holds NO Stellar token at
	// all — no classic asset ever observed, and no directory entry
	// naming a contract of theirs.
	//
	// They are not refused by any requirement. There is nothing to
	// refuse: no token of theirs was ever collected, so none was ever
	// evaluated. Measured on r1 2026-09-10, Franklin Templeton and Spiko
	// are both in this position — recognised, correct domains, and
	// absent from the issuers table entirely, because that table is
	// populated only when a CLASSIC asset is registered.
	//
	// Serving them is a coverage statement and admits nothing. It exists
	// so a reader can tell a recognised entity this surface cannot see
	// from one that does not exist — the same distinction the funnel
	// draws for candidates, one level further out. The count is exact
	// (funnel stage `directory_recognised_issuing_accounts`); this list
	// is a bounded sample of it.
	UnreachedEntities []RWAUnreachedEntity `json:"unreached_entities"`
}

// RWAUnreachedEntity is one recognised issuing entity this index holds
// no token for. The name, domain and tags are the curated third party's
// own labels, already served on every asset row as issuer_directory_*.
type RWAUnreachedEntity struct {
	// Address is the G-account the directory names. It is served so an
	// operator can act on it without a database session.
	Address string   `json:"address"`
	Name    string   `json:"name,omitempty"`
	Domain  string   `json:"domain,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// RWAFunnel is the complete narrowing from every issuer that could
// carry a SEP-1 attestation down to the assets served.
//
// It exists because the set is small and the population is not, and a
// reader of six rows has no way to tell a network with six real-world
// assets from a pipeline discarding the other fourteen thousand
// candidates without saying so. Every stage names what it counts and
// every drop names why, so the question "where did the rest go?" is
// answered by this response rather than by a database session.
type RWAFunnel struct {
	// Stages is the narrowing in pipeline order, coarse to fine. Each
	// stage's Dropped reasons account exactly for the difference
	// between its own count and the next stage's.
	Stages []RWAFunnelStage `json:"stages"`
	// Balanced is true when the arithmetic above closes: every stage's
	// count minus its drops equals the next stage's count. False means
	// a stage could not be measured and the figures must not be
	// reconciled — stated rather than hidden, because an accounting
	// that silently fails to add up is worse than none.
	Balanced bool `json:"balanced"`
	// Basis is a one-line statement of what the funnel measured.
	Basis string `json:"basis"`
}

// RWAFunnelStage is one population on the way to the served set.
type RWAFunnelStage struct {
	// Arm names which membership arm this stage belongs to: `classic`
	// for the SEP-1 attestation walk, `contract` for the curated
	// directory walk.
	//
	// The two arms narrow DIFFERENT populations from different roots and
	// meet only at the served set, so the stage arithmetic reconciles
	// WITHIN an arm and never across the boundary between them. Without
	// this field a reader would try to subtract the last classic stage
	// from the first contract one and find no relation, which is exactly
	// the misreading the per-stage `unit` was added to prevent one level
	// down.
	Arm string `json:"arm"`
	// Stage names the population.
	Stage string `json:"stage"`
	// Unit names what is being counted at this stage: the unit changes
	// down the funnel (issuer accounts, then SEP-1 declarations, then
	// assets), and comparing two counts of different units is the
	// easiest way to misread a funnel.
	Unit string `json:"unit"`
	// Count is the size of the population at this stage.
	Count int `json:"count"`
	// Dropped names why the population shrank before the next stage.
	// The counts sum exactly to this stage's Count minus the next
	// stage's Count.
	Dropped []RWAFunnelDrop `json:"dropped,omitempty"`
}

// RWAFunnelDrop is one counted reason a population shrank.
type RWAFunnelDrop struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	// Actor names who can change this number: `operator` for a drop an
	// operator could act on (an attestation never fetched, an issuer
	// missing from the curated directory), `issuer` for one only the
	// token's issuer can (a toml that declares nothing, or declares
	// somebody else), `definition` for a drop that is the membership
	// rule working as intended. Served so a reader can tell a coverage
	// gap from a refusal without knowing the codebase.
	Actor string `json:"actor"`
}

// Funnel drop actors.
const (
	rwaActorOperator   = "operator"
	rwaActorIssuer     = "issuer"
	rwaActorDefinition = "definition"
)

// Funnel stage and drop names. They are a wire vocabulary, so they are
// spelled once here and never inline.
const (
	rwaStageIssuersWithHomeDomain = "issuers_with_home_domain"
	rwaStageIssuersWithSep1       = "issuers_with_sep1_attestation"
	rwaStageIssuersDeclaring      = "issuers_declaring_currencies"
	rwaStageSep1Entries           = "sep1_currency_entries"
	rwaStageBoundEntries          = "issuer_bound_entries"
	rwaStageCandidates            = "candidate_assets_evaluated"
	rwaStageAdmitted              = "assets_admitted"
	rwaStageServed                = "assets_served"

	rwaDropNoAttestation        = "sep1_attestation_never_fetched"
	rwaDropPayloadUnreadable    = "sep1_payload_unreadable"
	rwaDropDeclaresNothing      = "sep1_declares_no_currencies"
	rwaDropMissingCode          = "entry_declares_no_asset_code"
	rwaDropMissingIssuer        = "entry_declares_no_issuer"
	rwaDropAnotherIssuer        = "entry_declares_another_issuer"
	rwaDropNoInstrumentBasis    = "no_real_world_instrument_basis"
	rwaDropDuplicateDeclaration = "duplicate_declaration_of_the_same_asset"
	rwaDropOverIssuerCap        = "over_issuer_cap"
	rwaDropNotInCatalogue       = "admitted_but_never_observed_on_chain"
	rwaDropIssuerPageTruncate   = "issuer_asset_page_truncated"
)

// Funnel arms.
const (
	rwaArmClassic  = "classic"
	rwaArmContract = "contract"
)

// Contract-arm funnel stages, drops and units. Same rule as above: a
// wire vocabulary, spelled once.
const (
	rwaUnitDirectoryAddresses = "directory_addresses"
	rwaUnitContracts          = "contracts"

	rwaStageDirectoryEntries      = "curated_directory_entries"
	rwaStageDirectoryContracts    = "directory_contract_addresses"
	rwaStageRecognisedContracts   = "directory_recognised_contracts"
	rwaStageContractCandidates    = "contract_candidates_evaluated"
	rwaStageContractsAdmitted     = "contract_assets_admitted"
	rwaStageContractsServed       = "contract_assets_served"
	rwaStageRecognisedIssuingAcct = "directory_recognised_issuing_accounts"

	rwaDropDirectoryNamesAnAccount = "directory_entry_names_an_account"
	rwaDropOverContractScanCap     = "over_contract_scan_cap"
	rwaDropDuplicateContractEntry  = "duplicate_directory_entry_for_contract"
)

// RWADefinition is the machine-readable membership rule.
type RWADefinition struct {
	// Requirements names the four conjunctive requirements of the
	// CLASSIC arm, in order.
	Requirements []string `json:"requirements"`
	// ContractRequirements names the four conjunctive requirements of
	// the CONTRACT arm, in order.
	//
	// A separate list, not a widening of the one above, because the
	// second requirement differs in kind rather than in strictness: a
	// contract has no (code, issuer) pair for a SEP-1 [[CURRENCIES]]
	// entry to bind to, so an independent party naming the exact
	// contract address takes the place of the issuer naming its own
	// asset. Both lists are served so a consumer can see which rule
	// admitted which row rather than inferring it from the fields
	// present.
	ContractRequirements []string `json:"contract_requirements"`
	// AnchorClasses is the closed SEP-1 anchor_asset_type vocabulary
	// that admits an asset on the declaration basis.
	AnchorClasses []string `json:"anchor_classes"`
	// RecognitionTags is the curated-directory vocabulary that counts
	// as independent recognition of the issuer account.
	RecognitionTags []string `json:"recognition_tags"`
	// ContractRecognitionTags is the same vocabulary for a CONTRACT
	// address, and is deliberately narrower. The upstream directory tags
	// AMM pools and protocol routers `defi` and `exchange`; on an
	// account those describe an entity, on a contract address they
	// describe a piece of infrastructure that issues nothing. Served so
	// the narrowing is auditable rather than implied.
	ContractRecognitionTags []string `json:"contract_recognition_tags"`
	// ScamFlagTags is the vocabulary that excludes an issuer outright.
	ScamFlagTags []string `json:"scam_flag_tags"`
	// BoundInstruments is the CURATED set of (code, issuer) pairs this
	// surface will compare against an oracle feed, and the feed each is
	// bound to.
	//
	// It is keyed on the pair, never on the code: asset codes are not
	// unique on Stellar, and a reference joined on the code alone hands
	// every token wearing an instrument's ticker that instrument's net
	// asset value. Served in full so a consumer can audit every binding
	// rather than inferring the rule from which rows carry a figure
	// today. A pair absent from it gets no reference, whatever it is
	// called.
	BoundInstruments []RWABoundInstrument `json:"bound_instruments"`
	// BoundContractInstruments is the CURATED set of contract addresses
	// this surface will admit on the curated basis, and the real-world
	// instrument each is bound to.
	//
	// Served in full, and EMPTY is a meaningful answer rather than a
	// missing one: it says no contract has yet cleared the evidence bar
	// for a curated binding, so the only contracts that can be admitted
	// are those whose on-chain symbol an independent oracle already
	// prices. A consumer reads the rule from this list rather than
	// inferring it from whichever rows appear today.
	BoundContractInstruments []RWABoundContractInstrument `json:"bound_contract_instruments"`
	// DocumentationURL points at the prose statement of the rule.
	DocumentationURL string `json:"documentation_url"`
}

// RWABoundContractInstrument is one curated binding: an exact contract
// address, the real-world instrument it holds, and that instrument's
// closed-vocabulary class.
type RWABoundContractInstrument struct {
	ContractID string `json:"contract_id"`
	Instrument string `json:"instrument"`
	Class      string `json:"class"`
}

// rwaBoundContractInstruments projects the curated contract set onto the
// wire type. The set itself lives in internal/rwa, where the evidence
// bar for an entry is recorded beside it.
func rwaBoundContractInstruments() []RWABoundContractInstrument {
	src := rwa.ContractInstrumentBindings()
	out := make([]RWABoundContractInstrument, 0, len(src))
	for _, b := range src {
		out = append(out, RWABoundContractInstrument{
			ContractID: b.ContractID, Instrument: b.Instrument, Class: b.Class,
		})
	}
	return out
}

// RWABoundInstrument is one curated binding: the exact Stellar
// (code, issuer) and the oracle feed whose instrument it is.
type RWABoundInstrument struct {
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	// Feed is the canonical `rwa:<CODE>` id, so a consumer can take it
	// straight to /v1/oracle/latest and see the same figure at source.
	Feed string `json:"feed"`
}

// rwaBoundInstruments projects the curated set onto the wire type. The
// set itself lives in internal/rwa, where the evidence for each entry is
// recorded beside it.
func rwaBoundInstruments() []RWABoundInstrument {
	src := rwa.InstrumentBindings()
	out := make([]RWABoundInstrument, 0, len(src))
	for _, b := range src {
		out = append(out, RWABoundInstrument{Code: b.Code, Issuer: b.Issuer, Feed: b.Feed})
	}
	return out
}

// RWASummary aggregates the served set.
type RWASummary struct {
	Assets  int `json:"assets"`
	Issuers int `json:"issuers"`
	// MarketCapUSD is the exact sum of the PUBLISHED per-asset market
	// caps, as a decimal string. ABSENT — not "0.00" — when no asset in
	// the set publishes one: a zero there reads as a real total of zero
	// dollars, which is the one reading that is certainly wrong.
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
	// AssetsValued and AssetsUnvalued split the set by whether it
	// contributed to the total.
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// LowerBound is true whenever any member asset is unvalued, i.e.
	// whenever the total is less than the value of the set.
	LowerBound bool `json:"lower_bound"`
	// EarliestFirstSeenLedger is the lowest ledger at which any member
	// asset was first observed. The index holds every ledger from
	// genesis, so this is the true first appearance of the set, not the
	// start of a sampling window. 0 (omitted) when no member carried a
	// first-seen ledger.
	EarliestFirstSeenLedger uint32 `json:"earliest_first_seen_ledger,omitempty"`
	// AssetsWithReference counts the members carrying an independent
	// oracle valuation of their instrument; AssetsCompared counts the
	// subset where a market price could also be measured against it.
	// Both are served because the difference between them is the
	// coverage of the premium column, and a reader who saw only the
	// premiums would take the gaps for zeros.
	AssetsWithReference int `json:"assets_with_reference"`
	AssetsCompared      int `json:"assets_compared"`
	// Basis is a one-line statement of what was measured and how it was
	// valued, in the same posture the DEX TVL headline takes.
	Basis string `json:"basis"`
	// Truncated reports that a cap bound the rebuild, so the set is
	// known to be incomplete.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAValuationStatus values. Every row carries exactly one, and a row
// whose status is not published carries no market cap.
const (
	// RWAValuationPublished — a price and a supply were both available
	// and neither gate withheld them.
	RWAValuationPublished = "published"
	// RWAValuationIssuerFlagged — the issuer acquired a scam-class
	// directory tag after the membership set was built. The row stays
	// (this surface hides nothing it admitted) with its valuation
	// withheld by the same suppression /v1/assets applies.
	RWAValuationIssuerFlagged = "withheld_issuer_flagged"
	// RWAValuationUnpriced — no USD price. Either the market never
	// produced one or the substance gate withheld it as too thin to
	// aggregate.
	RWAValuationUnpriced = "unpriced"
	// RWAValuationLowLiquidity — a price exists but the dust guard
	// refused to turn it into a market cap.
	RWAValuationLowLiquidity = "withheld_low_liquidity"
	// RWAValuationNoSupply — a price exists but no circulating-supply
	// reading does, so no market cap can be computed.
	RWAValuationNoSupply = "supply_unavailable"
)

// RWAValuation carries a row valuation and the reason when there is
// none. Both money fields are absent unless Status is published.
type RWAValuation struct {
	Status   string  `json:"status"`
	PriceUSD *string `json:"price_usd,omitempty"`
	// PriceBasis names a price that is NOT a direct market observation,
	// carried through verbatim from the listing row (declared_peg or
	// transitive). Absent means the price came from a market. It is on
	// the wire because a valuation surface that showed the figure and
	// hid how it was derived would be the same claim with the caveat
	// removed.
	PriceBasis   string  `json:"price_basis,omitempty"`
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
}

// RWAAsset is one member of the set.
type RWAAsset struct {
	AssetID string `json:"asset_id"`
	// Code and Issuer are the classic (code, issuer) identity. BOTH are
	// empty on a contract-issued row, which carries ContractID instead —
	// a contract token has no code and no issuer account, and filling
	// either from contract metadata would put a self-declared string in
	// the field this surface identifies assets by.
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	// ContractID is the C-strkey of a contract-issued member, and the
	// whole of its identity. Empty on a classic row. Exactly one of
	// (Code, Issuer) and ContractID is populated on every row.
	ContractID string `json:"contract_id,omitempty"`
	// Symbol is the token symbol the CONTRACT declares in its on-chain
	// metadata. Contract-authored display text, served under its own
	// name so a reader can see it is metadata rather than identity: two
	// contracts may declare the same symbol and they are two different
	// assets. Empty on a classic row and on a contract with no readable
	// metadata.
	Symbol string `json:"symbol,omitempty"`
	Slug   string `json:"slug,omitempty"`
	// Name is the [[CURRENCIES]] name from the issuer-bound SEP-1
	// entry. Issuer-authored display text.
	Name string `json:"name,omitempty"`
	// HomeDomain is the domain the issuer account set ON CHAIN, from
	// which the attestation was fetched.
	HomeDomain string `json:"home_domain,omitempty"`
	// IssuerDirectoryName and IssuerDirectoryTags are the independent
	// third-party label on the issuer G-address — the evidence for R3.
	IssuerDirectoryName string   `json:"issuer_directory_name,omitempty"`
	IssuerDirectoryTags []string `json:"issuer_directory_tags,omitempty"`
	// Basis names which requirement-4 arm admitted this asset.
	Basis string `json:"basis"`
	// AnchorClass is the closed-vocabulary class, present only under
	// the declaration basis.
	AnchorClass string `json:"anchor_class,omitempty"`
	// AnchorAsset is the off-chain instrument the issuer declared this
	// token anchors to, verbatim.
	AnchorAsset string `json:"anchor_asset,omitempty"`
	// Valuation is the money, or the reason there is none.
	Valuation RWAValuation `json:"valuation"`
	// Reference is an independent oracle's valuation of the real-world
	// instrument this token declares it anchors to — not this platform's
	// price for the token, and not derived from any Stellar market.
	// Absent when no comparable feed qualifies; Premium.Status says why.
	Reference *RWAReference `json:"reference,omitempty"`
	// Premium is the token's market price measured against that
	// reference, or the reason the comparison could not be made. Always
	// present, always with a status — an absent premium and a premium of
	// zero are different findings and the wire keeps them apart.
	Premium RWAPremium `json:"premium"`
	// CirculatingSupply is a raw chain fact and is served even when the
	// valuation is withheld, in the smallest integer unit.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// Volume24hUSD is the trailing-24h USD trade volume as served on
	// /v1/assets.
	Volume24hUSD *string `json:"volume_24h_usd,omitempty"`
	// FirstSeenLedger is the ledger this asset was first observed at,
	// from an index complete since genesis.
	FirstSeenLedger  uint32 `json:"first_seen_ledger,omitempty"`
	ObservationCount int64  `json:"observation_count"`
}

// RWAGroupTotal is one row of the per-class breakdown.
type RWAGroupTotal struct {
	Class  string `json:"class"`
	Assets int    `json:"assets"`
	// MarketCapUSD is absent when no asset in the group publishes one,
	// for the same reason the summary total is.
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
}

// RWAIssuerTotal is one row of the per-issuer breakdown.
type RWAIssuerTotal struct {
	Issuer         string  `json:"issuer"`
	Name           string  `json:"name,omitempty"`
	HomeDomain     string  `json:"home_domain,omitempty"`
	Assets         int     `json:"assets"`
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
}

// RWARefusal counts the candidates one requirement turned away.
type RWARefusal struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
}

// ─── membership ─────────────────────────────────────────────────────

// rwaMember is one admitted (code, issuer) with the evidence that
// admitted it. Built without any valuation input.
type rwaMember struct {
	code        string
	issuer      string
	name        string
	homeDomain  string
	anchorAsset string
	basis       string
	anchorClass string
	dirName     string
	dirTags     []string
}

// rwaMembership is one rebuild: the admitted set, the refusal tally
// over every candidate evaluated, and the census of the population both
// were drawn from.
type rwaMembership struct {
	members  []rwaMember
	refusals map[string]int
	// census accounts for every issuer row and every SEP-1 declaration
	// the attestation scan walked, including the stages upstream of
	// rwa.Qualify that no refusal reason can describe.
	census timescale.Sep1BoundCensus
	// overIssuerCap counts members rwaMaxIssuers turned away. They met
	// the definition; the cap is a rebuild guard, so they are counted
	// apart from the refusals rather than mixed into them.
	overIssuerCap int
	// duplicateDeclarations counts candidates naming a (code, issuer)
	// already admitted. A toml may declare the same asset twice — the
	// spec does not forbid it — and each declaration used to become its
	// own member, so the asset was served twice and its market cap
	// entered every total twice.
	duplicateDeclarations int
	// truncated records that rwaMaxIssuers bound the set.
	truncated bool
	// available is false when no attestation reader is wired, which is
	// a configuration statement rather than an empty population.
	available bool

	// contracts is the CONTRACT arm's admitted set — tokens with no
	// (code, issuer) pair, drawn from a different population by a
	// different rule (rwa_contracts.go). It is carried on the same
	// membership value because both arms share one rebuild, one cache
	// entry and one TTL: two caches would let the two halves of a single
	// response come from different moments.
	contracts []rwaContractMember
	// contractCensus accounts for the curated-directory population that
	// arm narrowed, including the stages upstream of rwa.QualifyContract.
	contractCensus rwaContractCensus
	// contractsNotObserved counts admitted contracts with no catalogue
	// row. Recorded on the membership rather than returned beside the
	// rows because the funnel is built from this value, and a count that
	// travelled separately could be dropped on a path that forgot it.
	contractsNotObserved int
	// unreached names the recognised issuing entities this index holds
	// no token for. It admits nothing; see RWAUnreachedEntity.
	unreached []rwaUnreachedEntity
}

// rwaCandidateFilter keeps only the bound SEP-1 entries that could
// possibly satisfy requirement 4, so the scan never materialises the
// tens of thousands of bound entries that declare an NFT, a crypto
// token or nothing at all.
func rwaCandidateFilter(c timescale.Sep1BoundCurrency) bool {
	return rwa.CouldQualify(c.Code, c.AnchorAssetType)
}

// buildRWAMembership runs BOTH arms and returns the combined set.
//
// The arms are independent by construction: one walks SEP-1
// attestations keyed by (code, issuer), the other walks curated
// directory entries keyed by contract address, and neither can admit
// what the other admits. Either may be unavailable on its own without
// emptying the response — the funnel says which was measured — because
// a deployment missing one reader has not learned that the other's
// population is empty.
//
// Refusals from both arms land in one tally. The reason vocabularies are
// disjoint, so a reader can still tell which rule turned a candidate
// away, and a single tally keeps `refused[]` a statement about the whole
// surface rather than about whichever arm a consumer happened to read.
func (s *Server) buildRWAMembership(ctx context.Context) rwaMembership {
	out := s.buildRWAClassicMembership(ctx)
	contracts, unreached, census, refusals := s.buildRWAContractMembership(ctx)
	out.contracts = contracts
	out.unreached = unreached
	out.contractCensus = census
	for reason, n := range refusals {
		out.refusals[reason] += n
	}
	return out
}

// buildRWAClassicMembership applies the definition to every issuer-bound
// SEP-1 attestation and returns the admitted set.
//
// Order of work: attestations first (one indexed scan), then ONE batch
// directory lookup over the candidate issuers (no N+1), then the
// per-candidate verdict. Nothing here reads a price.
func (s *Server) buildRWAClassicMembership(ctx context.Context) rwaMembership {
	out := rwaMembership{refusals: map[string]int{}}
	reader, ok := s.sep1Cache.(Sep1BoundCurrencyReader)
	if !ok {
		return out
	}
	out.available = true

	bound, census, err := reader.BoundSep1Currencies(ctx, rwaCandidateFilter)
	if err != nil {
		s.logger.Warn("rwa membership: bound sep1 scan failed", "err", err)
		out.available = false
		return out
	}
	out.census = census
	if why := census.Check(); why != "" {
		// The funnel will publish itself as unbalanced, which is the
		// honest wire answer; the log is what names the stage a reader
		// of the response cannot.
		s.logger.Warn("rwa membership: attestation census does not balance", "why", why)
	}
	// The pre-filter drops the bound entries that name no real-world
	// instrument at all — the NFT, crypto and undeclared majority. They
	// are refusals under requirement 4 and are counted as such; a
	// refusal tally that reported only what the scan happened to
	// materialise would understate the population it narrowed from.
	//
	// This bucket is the ONE place the refusal tally reports a
	// requirement out of R1→R4 order: these entries were dropped before
	// requirement 3 was evaluated for them, so some also fail it. The
	// funnel keeps them as their own stage for exactly that reason.
	if census.EntriesFiltered > 0 {
		out.refusals[rwa.RejectNoInstrumentClaim] += census.EntriesFiltered
	}

	addrs := make([]string, 0, len(bound))
	seen := make(map[string]struct{}, len(bound))
	for _, c := range bound {
		if _, dup := seen[c.Issuer]; dup {
			continue
		}
		seen[c.Issuer] = struct{}{}
		addrs = append(addrs, c.Issuer)
	}

	var entries map[string]timescale.DirectoryEntry
	if s.directory != nil && len(addrs) > 0 {
		entries, err = s.directory.DirectoryEntriesByAddresses(ctx, addrs)
		if err != nil {
			// Requirement 3 cannot be evaluated without the directory,
			// and it is the requirement that keeps impersonators out.
			// Fail CLOSED here — unlike the display overlay, which fails
			// open because it only omits a label. Serving the set
			// without R3 would publish the lookalike-domain population
			// as real-world assets.
			s.logger.Warn("rwa membership: directory batch lookup failed", "n", len(addrs), "err", err)
			out.available = false
			return out
		}
	}

	s.admitClassicCandidates(&out, bound, entries)
	return out
}

// admitClassicCandidates runs the ordered R1→R4 evaluation over the
// bound declarations and accumulates the admitted set, the refusal
// tally and the two structural drops onto the membership.
//
// Split from [Server.buildRWAClassicMembership] so the scan, the
// directory read and the verdict loop are each readable on their own —
// and so the verdict loop, which is the part a reviewer of the
// definition actually needs to read, is not buried under two pages of
// read-error handling.
func (s *Server) admitClassicCandidates(
	out *rwaMembership,
	bound []timescale.Sep1BoundCurrency,
	entries map[string]timescale.DirectoryEntry,
) {
	issuers := map[string]struct{}{}
	admitted := make(map[string]struct{}, len(bound))
	for _, c := range bound {
		e := entries[c.Issuer]
		v := rwa.Qualify(rwa.Candidate{
			Code:               c.Code,
			Issuer:             c.Issuer,
			BoundSep1:          true,
			DeclaredAnchorType: c.AnchorAssetType,
			DirectoryTags:      e.Tags,
		})
		if !v.InSet {
			out.refusals[v.Reject]++
			continue
		}
		// Identity is (code, issuer), so a second declaration of the
		// same pair is the same asset. Admitting it again would serve
		// the row twice and add its market cap to every total twice —
		// membership is a SET, and nothing downstream deduplicates.
		key := rwaKey(c.Code, c.Issuer)
		if _, dup := admitted[key]; dup {
			out.duplicateDeclarations++
			continue
		}
		if _, known := issuers[c.Issuer]; !known {
			if len(issuers) >= rwaMaxIssuers {
				out.truncated = true
				out.overIssuerCap++
				continue
			}
			issuers[c.Issuer] = struct{}{}
		}
		admitted[key] = struct{}{}
		out.members = append(out.members, rwaMember{
			code:        c.Code,
			issuer:      c.Issuer,
			name:        c.Name,
			homeDomain:  c.HomeDomain,
			anchorAsset: c.AnchorAsset,
			basis:       v.Basis,
			anchorClass: v.AnchorClass,
			dirName:     e.Name,
			dirTags:     e.Tags,
		})
	}
}

// cachedRWAMembership returns the membership set, rebuilt at most once
// per TTL window and shared across concurrent requests by a
// single-flight gate. The last good set is served on a rebuild error —
// a directory or SEP-1 read that fails must not empty a surface whose
// inputs move on a daily cadence.
func (s *Server) cachedRWAMembership(ctx context.Context) rwaMembership {
	s.rwaMu.Lock()
	if s.rwaCache != nil && time.Since(s.rwaAt) < rwaMembershipTTL {
		m := *s.rwaCache
		s.rwaMu.Unlock()
		return m
	}
	if ch := s.rwaFlight; ch != nil {
		s.rwaMu.Unlock()
		select {
		case <-ch:
			s.rwaMu.Lock()
			var m rwaMembership
			if s.rwaCache != nil {
				m = *s.rwaCache
			}
			s.rwaMu.Unlock()
			return m
		case <-ctx.Done():
			return rwaMembership{refusals: map[string]int{}}
		}
	}
	done := make(chan struct{})
	s.rwaFlight = done
	s.rwaMu.Unlock()

	built := s.buildRWAMembership(ctx)

	s.rwaMu.Lock()
	// EITHER arm answering makes the rebuild worth caching. Requiring
	// both would mean a deployment with only one reader wired rebuilt on
	// every request and never cached, and a transient failure of one arm
	// would discard a good rebuild of the other.
	if built.available || built.contractCensus.available {
		s.rwaCache = &built
		s.rwaAt = time.Now()
	} else if s.rwaCache != nil {
		built = *s.rwaCache
	}
	s.rwaFlight = nil
	s.rwaMu.Unlock()
	close(done)
	return built
}

// ─── handler ────────────────────────────────────────────────────────

// handleRWAAssets serves GET /v1/rwa/assets.
//
// No query parameters: the set is small and complete by construction,
// so there is nothing to page and no filter that would not be better
// applied by the caller over a whole document it already holds.
func (s *Server) handleRWAAssets(w http.ResponseWriter, r *http.Request) {
	view := RWAAssetsView{
		Definition:        rwaDefinition(),
		Assets:            []RWAAsset{},
		ByClass:           []RWAGroupTotal{},
		ByIssuer:          []RWAIssuerTotal{},
		Refused:           []RWARefusal{},
		UnreachedEntities: []RWAUnreachedEntity{},
	}
	if s.assetsReader == nil {
		view.Summary.Basis = rwaBasisUnavailable
		view.Funnel = rwaFunnelUnavailable()
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	m := s.cachedRWAMembership(r.Context())
	view.Refused = rwaRefusalRows(m.refusals)
	view.UnreachedEntities = rwaUnreachedRows(m.unreached)
	// Unavailable means NEITHER arm answered. One arm failing while the
	// other reports a set is a partial measurement, not an absent one,
	// and the funnel is what says which of the two happened.
	if !m.available && !m.contractCensus.available && len(m.members) == 0 && len(m.contracts) == 0 {
		view.Summary.Basis = rwaBasisUnavailable
		view.Funnel = rwaFunnelUnavailable()
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	rows, join, readErr := s.rwaListingRows(r.Context(), m)
	if readErr != nil {
		if clientAborted(r, readErr) {
			return
		}
		s.logger.Error("rwa listing read failed", "err", readErr)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	classicAssets, unobserved := s.rwaAssetRows(m, rows)
	join.notObserved = unobserved

	// The contract arm, valued through its own read of the same
	// pipeline. A failure here degrades to zero contract rows rather
	// than failing the response: the classic arm is a complete answer to
	// its own question, and emptying it because a second population
	// could not be read would publish less than we know.
	contractRows, contractsUnobserved, contractErr := s.rwaContractListingRows(r.Context(), m.contracts)
	if contractErr != nil {
		if clientAborted(r, contractErr) {
			return
		}
		s.logger.Error("rwa contract listing read failed", "err", contractErr)
		contractRows = map[string]AssetDetail{}
		contractsUnobserved = len(m.contracts)
	}
	m.contractsNotObserved = contractsUnobserved
	contractAssets := rwaContractAssetRows(m.contracts, contractRows)

	view.Assets = append(classicAssets, contractAssets...)

	// Ordering runs ONCE over the combined set, so a contract row and a
	// classic row are ranked by the same rule and an unvalued row of
	// either kind sorts after every valued one.
	rwaSortAssets(view.Assets)

	// The reference is attached AFTER the valuation, never before: it
	// reads the gated row (including the scam-flag suppression) and must
	// not be able to put a figure on a row the gates emptied.
	refs := s.cachedRWAReferences(r.Context())
	now := time.Now()
	for i := range view.Assets {
		rwaApplyReference(&view.Assets[i], refs, now)
	}
	view.Summary = rwaSummarise(view.Assets, m.truncated)
	view.ByClass = rwaByClass(view.Assets)
	view.ByIssuer = rwaByIssuer(view.Assets)
	view.Funnel = rwaFunnelOf(m, join, len(classicAssets), len(contractAssets))
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
}

// rwaBasisUnavailable is the summary basis when the attestation or
// directory reads that decide membership are not answering. It states
// the absence rather than serving an empty set as a finding.
const rwaBasisUnavailable = "Membership could not be established: the issuer-bound SEP-1 attestations or the curated account directory did not answer. No set is published rather than an unverified one."

func rwaDefinition() RWADefinition {
	return RWADefinition{
		Requirements: []string{
			"classic asset identified by (code, issuer)",
			"issuer-bound SEP-1 [[CURRENCIES]] entry served from the on-chain home_domain",
			"issuer independently recognised in the curated account directory and not scam-flagged",
			"real-world instrument by SEP-1 anchor_asset_type or by an ADR-0028 oracle feed",
		},
		ContractRequirements: []string{
			"contract-issued token identified by its contract address",
			"that exact contract address named in the curated account directory",
			"named with an issuing-class tag and no scam-class tag",
			"real-world instrument by an in-repo curated binding or by an ADR-0028 oracle feed on the on-chain symbol",
		},
		AnchorClasses:            rwa.AnchorClasses(),
		RecognitionTags:          rwa.RecognitionTags(),
		ContractRecognitionTags:  rwa.ContractRecognitionTags(),
		ScamFlagTags:             append([]string(nil), timescale.DirectoryScamFlagTags...),
		BoundInstruments:         rwaBoundInstruments(),
		BoundContractInstruments: rwaBoundContractInstruments(),
		DocumentationURL:         "https://stellarindex.io/docs/methodology/rwa-definition",
	}
}

// rwaCatalogueJoin counts what the join from the admitted set to the
// asset catalogue removed. Both of its fields used to be silent drops
// at the very end of the funnel, where an asset that met every
// requirement could still vanish without appearing in any tally.
type rwaCatalogueJoin struct {
	// pagesTruncated counts member issuers whose classic-asset listing
	// page filled to rwaAssetsPerIssuer, so their long tail went unread
	// and a member in it cannot be found.
	pagesTruncated int
	// notObserved counts admitted members with no catalogue row: the
	// asset was attested but has never been seen on chain.
	notObserved int
}

// rwaListingRows reads the member issuers through the SAME listing
// query /v1/assets uses, one indexed page per issuer, and returns the
// projected rows keyed by asset_id, plus what the read could not cover.
// Running the real listing path is what guarantees this surface cannot
// publish a valuation /v1/assets would have refused.
func (s *Server) rwaListingRows(
	ctx context.Context, m rwaMembership,
) (map[string]AssetDetail, rwaCatalogueJoin, error) {
	var join rwaCatalogueJoin
	issuers := make([]string, 0, rwaMaxIssuers)
	seen := map[string]struct{}{}
	for _, mem := range m.members {
		if _, dup := seen[mem.issuer]; dup {
			continue
		}
		seen[mem.issuer] = struct{}{}
		issuers = append(issuers, mem.issuer)
	}
	sort.Strings(issuers)

	wanted := make(map[string]struct{}, len(m.members))
	for _, mem := range m.members {
		wanted[rwaKey(mem.code, mem.issuer)] = struct{}{}
	}

	out := make(map[string]AssetDetail, len(m.members))
	for _, issuer := range issuers {
		rows, err := s.assetsReader.ListAssetsExt(ctx, timescale.ListAssetsOptions{
			Limit:  rwaAssetsPerIssuer,
			Issuer: issuer,
			Type:   "classic",
		})
		if err != nil {
			return nil, join, err
		}
		// A full page means the issuer has more classic assets than one
		// read covers, so a member in the unread tail would disappear
		// from the set with nothing to show for it. The cap has always
		// been documented as reported; until now it was not.
		if len(rows) >= rwaAssetsPerIssuer {
			join.pagesTruncated++
		}
		keep := make([]timescale.AssetRow, 0, 8)
		for _, row := range rows {
			if _, ok := wanted[rwaKey(row.Code, row.IssuerGStrkey)]; ok {
				keep = append(keep, row)
			}
		}
		if len(keep) == 0 {
			continue
		}
		details := make([]AssetDetail, 0, len(keep))
		for _, row := range keep {
			details = append(details, assetDetailFromAssetRow(row))
		}
		// The /v1/assets post-query pipeline, in its order. Every step
		// is a price producer or a gate, and the order between them is
		// load-bearing there for reasons that apply identically here:
		// the collision stamp must precede the market-cap fill (which
		// reads it), the peg fill must follow both the gate that would
		// strip it and the fill that must not derive a valuation from
		// it, and the directory stamp brings the scam suppression.
		//
		// Running the whole pipeline rather than a chosen subset is the
		// point. A surface that ran only the steps it believed it
		// needed would drift from /v1/assets one omission at a time,
		// and each omission would show up as this page publishing a
		// figure that page withholds, or withholding one it publishes.
		s.stampListingCollisions(details)
		s.applySubstanceGateToListing(ctx, details)
		s.fillMarketCapsFromSupply(ctx, details, assetRowSourceCounts(keep))
		s.fillDeclaredPegPricesInListing(ctx, details)
		s.fillIssuerDirectoryTags(ctx, details)
		for _, d := range details {
			out[rwaKey(d.Code, issuer)] = d
		}
	}
	return out, join, nil
}

// rwaKey is the membership join key: the code case-folded (the SEP-1
// overlay matches codes case-insensitively, and so must the join that
// reads its output) and the issuer G-address exact.
func rwaKey(code, issuer string) string {
	return strings.ToUpper(strings.TrimSpace(code)) + "-" + issuer
}

// rwaAssetRows joins the membership evidence to the valued listing rows
// and orders the result, returning how many members the join could not
// place. A member with no listing row is dropped: the asset was
// attested but has never been observed on chain, and this surface
// reports what the index holds — but it now reports the drop too,
// because "admitted then never served" is otherwise invisible.
func (s *Server) rwaAssetRows(m rwaMembership, rows map[string]AssetDetail) ([]RWAAsset, int) {
	out := make([]RWAAsset, 0, len(m.members))
	notObserved := 0
	for _, mem := range m.members {
		d, ok := rows[rwaKey(mem.code, mem.issuer)]
		if !ok {
			notObserved++
			continue
		}
		a := RWAAsset{
			AssetID:             d.AssetID,
			Code:                d.Code,
			Issuer:              mem.issuer,
			Slug:                d.Slug,
			Name:                strings.TrimSpace(mem.name),
			HomeDomain:          mem.homeDomain,
			IssuerDirectoryName: mem.dirName,
			IssuerDirectoryTags: d.IssuerDirectoryTags,
			Basis:               mem.basis,
			AnchorClass:         mem.anchorClass,
			AnchorAsset:         strings.TrimSpace(mem.anchorAsset),
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			Volume24hUSD:        d.VolumeUSD24h,
		}
		if len(a.IssuerDirectoryTags) == 0 {
			a.IssuerDirectoryTags = mem.dirTags
		}
		if d.FirstSeenLedger != nil {
			a.FirstSeenLedger = *d.FirstSeenLedger
		}
		if d.ObservationCount != nil {
			a.ObservationCount = *d.ObservationCount
		}
		out = append(out, a)
	}
	return out, notObserved
}

// rwaSortAssets applies the served ordering: published market cap
// descending, then observation count, then asset id.
//
// It runs ONCE over the combined set rather than per arm. Sorting each
// arm and concatenating would rank every classic row above every
// contract row whatever their valuations, which is a claim about
// relative size that the concatenation order happened to make and
// nobody measured.
//
// Rows with no published valuation sort after every valued row — an
// unvalued asset is never ranked above a valued one on a number it does
// not have.
func rwaSortAssets(out []RWAAsset) {
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].Valuation.MarketCapUSD), ratFromOptionalString(out[j].Valuation.MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		if out[i].ObservationCount != out[j].ObservationCount {
			return out[i].ObservationCount > out[j].ObservationCount
		}
		return out[i].AssetID < out[j].AssetID
	})
}

// rwaValuationOf reads the valuation OFF the already-gated listing row.
// It never recomputes a price or a market cap: every withholding
// decision was made upstream by the gates, and this function only
// reports which one applied. That is why a withheld figure cannot
// reappear here as a number.
func rwaValuationOf(d AssetDetail) RWAValuation {
	if pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags) {
		return RWAValuation{Status: RWAValuationIssuerFlagged}
	}
	if d.PriceUSD == nil {
		return RWAValuation{Status: RWAValuationUnpriced}
	}
	if d.MarketCapUSD == nil {
		if d.MarketCapLowLiquidity {
			return RWAValuation{Status: RWAValuationLowLiquidity, PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis}
		}
		return RWAValuation{Status: RWAValuationNoSupply, PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis}
	}
	return RWAValuation{
		Status:       RWAValuationPublished,
		PriceUSD:     d.PriceUSD,
		PriceBasis:   d.PriceBasis,
		MarketCapUSD: d.MarketCapUSD,
	}
}

// ─── aggregates ─────────────────────────────────────────────────────

// rwaSumMarketCaps returns the exact sum of the published market caps
// and how many rows contributed. Exact big.Rat arithmetic over
// already-rounded 2-dp per-asset figures (ADR-0003), so each level is
// the exact sum of the level below. Returns nil when nothing was
// published.
func rwaSumMarketCaps(assets []RWAAsset) (*string, int) {
	sum := new(big.Rat)
	valued := 0
	for _, a := range assets {
		r := ratFromOptionalString(a.Valuation.MarketCapUSD)
		if r == nil {
			continue
		}
		sum.Add(sum, r)
		valued++
	}
	if valued == 0 {
		return nil, 0
	}
	s := sum.FloatString(2)
	return &s, valued
}

func rwaSummarise(assets []RWAAsset, truncated bool) RWASummary {
	total, valued := rwaSumMarketCaps(assets)
	issuers := map[string]struct{}{}
	var earliest uint32
	for _, a := range assets {
		issuers[a.Issuer] = struct{}{}
		if a.FirstSeenLedger != 0 && (earliest == 0 || a.FirstSeenLedger < earliest) {
			earliest = a.FirstSeenLedger
		}
	}
	referenced, compared := rwaReferenceCounts(assets)
	return RWASummary{
		Assets:                  len(assets),
		Issuers:                 len(issuers),
		MarketCapUSD:            total,
		AssetsValued:            valued,
		AssetsUnvalued:          len(assets) - valued,
		LowerBound:              len(assets)-valued > 0,
		EarliestFirstSeenLedger: earliest,
		AssetsWithReference:     referenced,
		AssetsCompared:          compared,
		Basis:                   rwaBasis(len(assets), valued, compared, truncated),
		Truncated:               truncated,
	}
}

// rwaBasis states what the total measured, in prose, so a reader of the
// figure gets its scope without having to reconstruct it from counts.
func rwaBasis(total, valued, compared int, truncated bool) string {
	var b strings.Builder
	b.WriteString("Sum of the published market caps of the assets meeting the four-requirement definition. ")
	b.WriteString("Market cap is circulating supply times the served USD price, both as /v1/assets serves them, ")
	b.WriteString("under the same substance, dust-liquidity and scam-issuer gates. ")
	switch {
	case total == 0:
		b.WriteString("No asset currently meets the definition.")
	case valued == total:
		b.WriteString("Every asset in the set publishes a valuation.")
	default:
		b.WriteString("Assets whose valuation is withheld or unavailable contribute nothing and are counted separately, so the total is a LOWER BOUND on the value of the set.")
	}
	if compared > 0 {
		// The comparison rests on the issuer's own domain-bound
		// declaration that this token anchors to the named instrument.
		// That is the evidence that admitted the asset, and it is not a
		// verified statement of denomination — so the surface says so
		// beside the figure rather than letting a percentage imply a
		// certainty nobody established.
		b.WriteString(" Premium and discount compare the token's market price against an independent oracle's valuation of the instrument the issuer declares it anchors to; the correspondence between one token and one unit of that instrument is the issuer's own declaration, not an independent measurement.")
	}
	if truncated {
		b.WriteString(" The issuer cap bound this rebuild, so the set is known to be incomplete.")
	}
	return b.String()
}

// rwaUnclassified groups the assets admitted on the oracle basis, which
// declare no anchor class. Naming the group is honest; inventing a
// class for it would not be.
const rwaUnclassified = "unclassified"

func rwaByClass(assets []RWAAsset) []RWAGroupTotal {
	byClass := map[string][]RWAAsset{}
	for _, a := range assets {
		c := a.AnchorClass
		if c == "" {
			c = rwaUnclassified
		}
		byClass[c] = append(byClass[c], a)
	}
	out := make([]RWAGroupTotal, 0, len(byClass))
	for c, group := range byClass {
		total, valued := rwaSumMarketCaps(group)
		out = append(out, RWAGroupTotal{
			Class:          c,
			Assets:         len(group),
			MarketCapUSD:   total,
			AssetsUnvalued: len(group) - valued,
		})
	}
	sortRWAGroups(out)
	return out
}

func sortRWAGroups(out []RWAGroupTotal) {
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Class < out[j].Class
	})
}

func rwaByIssuer(assets []RWAAsset) []RWAIssuerTotal {
	order := make([]string, 0, 8)
	byIssuer := map[string][]RWAAsset{}
	for _, a := range assets {
		if _, ok := byIssuer[a.Issuer]; !ok {
			order = append(order, a.Issuer)
		}
		byIssuer[a.Issuer] = append(byIssuer[a.Issuer], a)
	}
	out := make([]RWAIssuerTotal, 0, len(order))
	for _, issuer := range order {
		group := byIssuer[issuer]
		total, valued := rwaSumMarketCaps(group)
		out = append(out, RWAIssuerTotal{
			Issuer:         issuer,
			Name:           group[0].IssuerDirectoryName,
			HomeDomain:     group[0].HomeDomain,
			Assets:         len(group),
			MarketCapUSD:   total,
			AssetsUnvalued: len(group) - valued,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Issuer < out[j].Issuer
	})
	return out
}

func rwaRefusalRows(refusals map[string]int) []RWARefusal {
	out := make([]RWARefusal, 0, len(refusals))
	for reason, n := range refusals {
		out = append(out, RWARefusal{Reason: reason, Assets: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Assets != out[j].Assets {
			return out[i].Assets > out[j].Assets
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
