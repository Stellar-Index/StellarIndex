package v1

import (
	"context"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// GET /v1/rwa/assets — tokenized real-world assets on Stellar.
//
// The membership rule lives in `internal/rwa` (docs/methodology/rwa-definition.md); this file is
// the read path and wire shape. Two properties are load-bearing:
//
//  1. Membership is decided before valuation, from identity and attestation only, so a withheld
//     price cannot shrink the set and a large market cap cannot buy a place in it.
//
//  2. Valuation reuses the /v1/assets listing pipeline unchanged (store query, substance gate,
//     supply-derived market cap, dust guard, scam-issuer payload suppression), so this surface
//     cannot publish a figure /v1/assets would have withheld.
//
// An unavailable valuation is stated in `valuation.status` with no number; a withheld figure is
// never rendered as zero.

// rwaMembershipTTL bounds how long the membership set is reused. Its inputs (SEP-1 refresh,
// directory sync) move daily, so ten minutes keeps the scan off the request path while a new
// issuer still appears within the hour.
const rwaMembershipTTL = 10 * time.Minute

// rwaMaxIssuers bounds the per-issuer listing reads one rebuild makes. The qualifying set is
// small because R3 requires an independent party to have named the account; the cap only guards
// against a directory sync that suddenly tags thousands. When it binds, the surface says so
// rather than serving a silently truncated set.
const rwaMaxIssuers = 64

// rwaAssetsPerIssuer bounds one issuer's listing page. An issuer with
// more classic assets than this has its long tail unread; the cap is
// reported the same way the issuer cap is.
const rwaAssetsPerIssuer = 500

// Sep1BoundCurrencyReader is the storage seam the membership build reads attestations through;
// *timescale.Store satisfies it. Optional: without it the surface serves an empty set with a
// stated reason rather than an error.
//
// The candidate pool silently bounds this surface: it reads `issuers WHERE sep1_payload IS NOT
// NULL`, and `issuers` rows come only from the classic-asset registry writer. An issuer that
// `asset-registry-backfill`, `issuer-enrich` and `sep1-refresh` have not all reached is absent
// from the `refused` counts entirely, not refused by a requirement.
type Sep1BoundCurrencyReader interface {
	BoundSep1Currencies(
		ctx context.Context, keep timescale.Sep1CurrencyFilter,
	) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error)
}

// Compile-time proof that the production reader satisfies the seam. An optional seam that stops
// matching is a silent opt-out, here the whole classic RWA membership going empty with
// `available: false` and nothing red. Only the bare store is listed because that is what the
// binary wires (`Sep1Cache: store`); a caching wrapper in front of it must be added here too.
var _ Sep1BoundCurrencyReader = (*timescale.Store)(nil)

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
	// Membership dates the SET — when it was built, and whether what is
	// being served is a lapsed copy of it. Omitted only before the
	// first build has ever completed, when there is no set to date.
	Membership *RWAMembershipSet `json:"membership,omitempty"`
	// Assets is the set, ordered by published market cap descending,
	// then by observation count. Rows with no published valuation sort
	// after every valued row — an unvalued asset is never ranked above
	// a valued one on a number it does not have.
	Assets []RWAAsset `json:"assets"`
	// ByClass totals the set per declared real-world class. Assets
	// admitted on the oracle basis declare no class and are grouped
	// under `unclassified`.
	ByClass []RWAGroupTotal `json:"by_class"`
	// CuratedAssets are the rows a named third-party curator lists as
	// real-world assets, served APART from `assets`: never in the
	// summary, the class or the issuer breakdown above. See
	// [RWACuratedSummary]. Absent when no curated reader is wired.
	CuratedAssets []RWAAsset `json:"curated_assets,omitempty"`
	// Curated is the curated arm's own total and status.
	Curated *RWACuratedSummary `json:"curated,omitempty"`
	// ByIssuer totals the set per issuer G-address. Contract-issued
	// rows have no issuer account and are not in it; ByClass and
	// Summary still count them.
	ByIssuer []RWAIssuerTotal `json:"by_issuer"`
	// Refused counts the candidates each requirement turned away, so the served set is never
	// mistaken for the whole population of assets that claim to be real-world assets.
	//
	// It is the ordered requirement tally over candidates that reached the full R1-R4
	// evaluation only. Earlier drops (no attestation, undecodable payload, an entry naming
	// somebody else's account) are accounted for in Funnel, the complete narrowing.
	Refused []RWARefusal `json:"refused"`
	// Funnel accounts for the ENTIRE population this set was narrowed
	// from, stage by stage, with a named reason on every drop.
	Funnel RWAFunnel `json:"funnel"`
	// UnreachedEntities names curated-directory entities that a third party recognises as
	// issuing or custodying value, carry no scam-class tag, and for which this index holds no
	// Stellar token at all (no classic asset observed, no directory entry naming a contract).
	//
	// No requirement refused them because nothing was collected to evaluate. Serving them is a
	// coverage statement that admits nothing, so a reader can tell an entity this surface cannot
	// see from one that does not exist. The count is exact (funnel stage
	// `directory_recognised_issuing_accounts`); this list is a bounded sample.
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
// The set is small and the population is not, so a reader of a few rows cannot otherwise tell a
// network with few real-world assets from a pipeline silently discarding thousands of
// candidates. Every stage names what it counts and every drop names why.
type RWAFunnel struct {
	// Stages is the narrowing in pipeline order, coarse to fine. Each
	// stage's Dropped reasons account exactly for the difference
	// between its own count and the next stage's.
	Stages []RWAFunnelStage `json:"stages"`
	// Balanced is true when every stage's count minus its drops equals the next stage's count.
	// False means a stage could not be measured and the figures must not be reconciled; an
	// accounting that silently fails to add up is worse than none.
	Balanced bool `json:"balanced"`
	// Imbalance names what did not close and is empty exactly when Balanced is true.
	//
	// Each reason comes from a check that already runs (a census Check() or an adjacent
	// stage pair's arithmetic). Reasons are semicolon-separated because the checks are
	// independent and the first is not necessarily the cause of the rest.
	Imbalance string `json:"imbalance,omitempty"`
	// Basis is a one-line statement of what the funnel measured.
	Basis string `json:"basis"`
	// ListingDirectory is the evidence behind the `listing` arm's verdict: what the independent
	// listing directory held when this set was built.
	//
	// Omitted when nothing was observed (no reader wired, or the read failed); absence is a
	// statement, never a default.
	ListingDirectory *RWAListingDirectory `json:"listing_directory,omitempty"`
}

// RWAMembershipSet dates the set this response describes: when its rebuild finished, and whether
// the copy served is past its lifetime.
//
// The envelope's `as_of` is the response's instant and would wrongly suggest continuous refresh
// if read as build time, hence `built_at`.
//
// The cache deliberately serves a lapsed set while a detached rebuild runs, because a request
// must not wait for the multi-second rescan. This block makes that visible:
//
//	Stale false                       inside its lifetime
//	Stale true, RebuildFailedAt nil   lapsed, rebuild running or about to: transient
//	Stale true, RebuildFailedAt set   lapsed and the last replacement FAILED; the
//	                                  served set is the last good one. Act on this.
type RWAMembershipSet struct {
	// BuiltAt is when the rebuild that produced this set finished, not when the response was
	// rendered or any source was written. It is the instant the lifetime clock starts from, so
	// `stale` never describes a different moment.
	BuiltAt WireTime `json:"built_at"`
	// Stale reports that the set has passed its lifetime and a rebuild is owed. It is served
	// regardless: an expired set is unchanged until the inputs move, and waiting for the rescan
	// would cost eleven seconds for the same answer.
	Stale bool `json:"stale"`
	// RebuildFailedAt is when the latest rebuild attempt failed; a successful rebuild clears it.
	// Beside Stale it separates a set about to be replaced from one nothing is replacing, the
	// only case that needs action.
	RebuildFailedAt *WireTime `json:"rebuild_failed_at,omitempty"`
}

// RWAListingDirectory is what the independent listing directory looked like when the served set
// was built.
//
// A `listing` arm corroborating nothing can mean a directory never synced, a sync that stopped,
// or a healthy directory this set predates; each needs a different response.
//
// Entries counts fresh rows only (`Entries = Contracts + Classic`, Stale beside, never inside),
// so Stale tells the two zero cases apart:
//
//	Entries == 0 && Stale == 0     never synced: the table is empty
//	Entries == 0 && Stale > 0      every row aged out: the sync STOPPED
//	Entries > 0 && Contracts == 0  healthy, names no Stellar CONTRACT address
//	Entries > 0 && Contracts > 0   healthy and populated
//
// Compare ObservedAt against the sync's own clock: a sync completed AFTER it means
// the set predates the rows and the next rebuild carries them; nothing is broken.
// Evidence and verdict come from the same read, so they cannot contradict.
type RWAListingDirectory struct {
	// ObservedAt is when this index read the directory, not when the directory was synced or
	// the response rendered. Counts can be re-read later but this instant cannot, so it must
	// be published; it places a closed arm and a healthy directory in time relative to each
	// other.
	ObservedAt WireTime `json:"observed_at"`
	// Entries is every fresh row of either address form, before the contract/classic split.
	// Rows past the recognition bound are in Stale instead.
	Entries int `json:"entries"`
	// Contracts is how many rows name a Stellar contract address, the only form C2's second
	// arm can consult. A healthy directory of classic rows only still closes the arm, and this
	// count says so.
	Contracts int `json:"contracts"`
	// Stale is how many rows are present but past the recognition bound, counted beside
	// Entries and never inside it. It is the visible form of the fail-closed guarantee and
	// separates a stopped sync from one that never ran: both leave Entries at zero, but only
	// a stopped sync leaves rows behind to go stale.
	Stale int `json:"stale"`
}

// RWAFunnelStage is one population on the way to the served set.
type RWAFunnelStage struct {
	// Arm names the membership arm: `classic` for the SEP-1 attestation walk, `contract` for
	// the curated directory walk.
	//
	// The arms narrow different populations from different roots and meet only at the served
	// set, so stage arithmetic reconciles within an arm and never across them.
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
	// Actor names who can change this number: `operator` (an attestation never fetched, an
	// issuer missing from the curated directory), `issuer` (a domain serving no attestation, or
	// a toml declaring nothing or somebody else), or `definition` (the membership rule working
	// as intended). It lets a reader tell a coverage gap from a refusal.
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
	rwaDropDomainServedNothing  = "domain_served_no_sep1_attestation"
	rwaDropAttestationStale     = "sep1_attestation_stale"
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
	// rwaArmListing is C2's second arm: the in-repo curated contract bindings narrowed to those an
	// independent listing directory corroborates. It is separate from the contract arm because it
	// narrows a different population from a different root (a table in this repository, not a
	// third party's directory).
	rwaArmListing = "listing"
	// rwaArmValuation continues past the served set instead of narrowing toward it: it accounts
	// for which served rows carry a REFERENCE-priced valuation and, for each row that does not,
	// the reason. That coverage is otherwise visible only by reading every row.
	rwaArmValuation = "valuation"
)

// Valuation-arm funnel stages. The market basis is not walked here: its
// own coverage is `summary.assets_valued` and `assets_unvalued`, and
// each of its refusals is already a `valuation.status` on the row.
const (
	// rwaStageValuationCandidates is the combined served set — both
	// membership arms, which meet exactly here.
	rwaStageValuationCandidates = "assets_served_all_arms"
	rwaStageReferenceValued     = "assets_reference_valued"
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

// Listing-arm funnel stages and drops — C2's second arm. Same rule as
// above: a wire vocabulary, spelled once.
const (
	rwaStageCuratedBindings      = "curated_contract_bindings"
	rwaStageListingCorroborated  = "listing_corroborated_contracts"
	rwaStageListingAdmitted      = "listing_contract_assets_admitted"
	rwaStageListingServed        = "listing_contract_assets_served"
	rwaStageListedWithoutBinding = "listing_contracts_without_curated_binding"

	rwaDropAlreadyEvaluatedByDirectory = "contract_already_evaluated_by_directory_arm"
)

// RWADefinition is the machine-readable membership rule.
type RWADefinition struct {
	// Requirements names the four conjunctive requirements of the
	// CLASSIC arm, in order.
	Requirements []string `json:"requirements"`
	// ContractRequirements names the four conjunctive requirements of the CONTRACT arm, in
	// order. It is a separate list because the second differs in kind: a contract has no
	// (code, issuer) pair for a SEP-1 entry to bind to, so an independent party naming the
	// exact contract address replaces the issuer naming its own asset.
	ContractRequirements []string `json:"contract_requirements"`
	// AnchorClasses is the closed SEP-1 anchor_asset_type vocabulary that admits an asset on the
	// declaration basis.
	AnchorClasses []string `json:"anchor_classes"`
	// ContractAnchorClasses is the vocabulary a curated contract binding may use, a superset of
	// AnchorClasses.
	//
	// The classic arm reads an issuer's free-text anchor_asset_type, so it accepts only terms
	// SEP-1 defines. A contract binding's class is this index's own reviewed statement, so it
	// may use a term SEP-1 lacks (`fund`). That widens what a binding may say, not what is
	// admitted; the identity, recognition and scam requirements are untouched.
	ContractAnchorClasses []string `json:"contract_anchor_classes"`
	// RecognitionTags is the curated-directory vocabulary that counts as independent recognition
	// of the issuer account.
	RecognitionTags []string `json:"recognition_tags"`
	// ContractRecognitionTags is the same vocabulary for a contract address, deliberately
	// narrower: the directory tags AMM pools and routers `defi` and `exchange`, which describe an
	// entity on an account but infrastructure that issues nothing on a contract.
	ContractRecognitionTags []string `json:"contract_recognition_tags"`
	// ContractRecognitionSources is the closed vocabulary of ways C2 can be satisfied, in order
	// of strength; a row admitted on a corroborating pair is not the same claim as one admitted
	// on a directory attestation.
	ContractRecognitionSources []string `json:"contract_recognition_sources"`
	// ScamFlagTags is the vocabulary that excludes an issuer outright.
	ScamFlagTags []string `json:"scam_flag_tags"`
	// BoundInstruments is the curated set of (code, issuer) pairs compared against an oracle
	// feed, and the feed each is bound to.
	//
	// Keyed on the pair, never the code: a reference joined on code alone would hand every token
	// wearing an instrument's ticker that instrument's net asset value. A pair absent from it
	// gets no reference, whatever it is called.
	BoundInstruments []RWABoundInstrument `json:"bound_instruments"`
	// BoundContractInstruments is the curated set of contract addresses admitted on the curated
	// basis, and the instrument each is bound to.
	//
	// An address here is not a statement that the asset is in the set. The binding answers C4
	// (which instrument an address holds) while C2 still requires the curated directory to
	// name the same address, so some entries are still refused for want of recognition.
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
	Assets int `json:"assets"`
	// Issuers is rwaIssuerCount: distinct issuer G-addresses plus one per
	// contract-issued row, whose address is its whole identity.
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
	// LowerBound is true whenever any member asset is unvalued or a
	// rebuild cap bound the set (Truncated), i.e. whenever the total may
	// be less than the value of the set.
	LowerBound bool `json:"lower_bound"`
	// EarliestFirstSeenLedger is the lowest ledger at which any member was first observed.
	// The lake holds every ledger from genesis, so this is the true first appearance, not a
	// sampling-window start. 0 (omitted) when no member carried a first-seen ledger.
	EarliestFirstSeenLedger uint32 `json:"earliest_first_seen_ledger,omitempty"`
	// AssetsWithReference counts members carrying an independent oracle valuation of their
	// instrument; AssetsCompared counts the subset where a market price could also be
	// measured against it. Both are served because their difference is the coverage of the
	// premium column, and a reader seeing only premiums would take the gaps for zeros.
	AssetsWithReference int `json:"assets_with_reference"`
	AssetsCompared      int `json:"assets_compared"`
	// ReferenceValuation is the SECOND, separately-labelled total: the
	// set valued at reference prices instead of at observed market
	// prices. A different basis over a different subset of the same
	// rows — never a component of MarketCapUSD and never added to it.
	ReferenceValuation RWAReferenceSummary `json:"reference_valuation"`
	// BothBases totals only the members carrying BOTH figures, so the
	// gap between the two bases can be read at set level without
	// subtracting two totals taken over different rows.
	BothBases RWABothBases `json:"both_bases"`
	// Basis is a one-line statement of what was measured and how it was
	// valued, in the same posture the DEX TVL headline takes.
	Basis string `json:"basis"`
	// Truncated reports that a cap bound the rebuild — the issuer cap,
	// the contract scan cap or a full per-issuer listing page — so
	// members may be missing from the set; Basis names which.
	Truncated bool `json:"truncated,omitempty"`
}

// rwaTruncation counts what each rebuild cap left unread. Every one of
// them makes the summary total a lower bound, not only the issuer cap.
type rwaTruncation struct {
	overIssuerCap   int
	overContractCap int
	issuerPagesFull int
}

func (t rwaTruncation) any() bool {
	return t.overIssuerCap > 0 || t.overContractCap > 0 || t.issuerPagesFull > 0
}

// rwaTruncationOf gathers the caps from the membership and the catalogue join.
func rwaTruncationOf(m rwaMembership, join rwaCatalogueJoin) rwaTruncation {
	return rwaTruncation{
		overIssuerCap:   m.overIssuerCap,
		overContractCap: m.contractCensus.overCap,
		issuerPagesFull: join.pagesTruncated,
	}
}

// RWAReferenceSummary aggregates the reference-priced basis.
//
// Every field has a market-cap counterpart in [RWASummary] and behaves the same way: the total is
// absent rather than "0.00" when nothing is reference-valued, contributing and non-contributing
// rows are counted separately, and LowerBound says whether the total is below the set's value.
// The meaning differs: nothing summed here was observed being paid.
type RWAReferenceSummary struct {
	// ValueUSD is the exact sum of the published per-asset `reference_valuation.value_usd`
	// strings. Absent, never "0.00", when no member is reference-valued.
	ValueUSD *string `json:"value_usd,omitempty"`
	// AssetsValued and AssetsUnvalued split the set by whether it
	// contributed. Counted independently of the market-cap split: the
	// two bases admit different rows, which is the whole reason both
	// totals exist.
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// LowerBound is true whenever any member carries no reference
	// valuation or a rebuild cap bound the set, i.e. whenever this total
	// may be less than the reference-priced value of the set.
	LowerBound bool `json:"lower_bound"`
	// Sources names the distinct publishers whose values are in the total, sorted. Not "oracles":
	// the total can mix three provenances, and calling a listing platform or a prospectus an
	// oracle would misdescribe part of the figure. The per-row `reference` blocks carry the full
	// provenance (kind, feed, quote, vintage); a kind is never inferred from a publisher name.
	Sources []string `json:"sources,omitempty"`
	// Provenances names the distinct `reference.provenance` values in the total, sorted, so the
	// mixture of claim kinds is visible without walking every row.
	Provenances []string `json:"provenances,omitempty"`
	// Basis states in prose what was measured and, more importantly,
	// what it is not.
	Basis string `json:"basis"`
}

// RWABothBases totals the members that carry a market cap AND a
// reference valuation.
//
// `market_cap_usd` and `reference_valuation.value_usd` are sums over different rows (an asset
// that never trades has only the second), so their difference means nothing. These two totals
// share the same rows, so their difference is the aggregate gap between market price and
// reference over that overlap; per asset the same gap is `premium.pct`.
type RWABothBases struct {
	// Assets is how many members carry both figures.
	Assets int `json:"assets"`
	// MarketCapUSD and ReferenceValueUSD are the two totals over those
	// rows and only those rows. Both absent when Assets is zero.
	MarketCapUSD      *string `json:"market_cap_usd,omitempty"`
	ReferenceValueUSD *string `json:"reference_value_usd,omitempty"`
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
	// RWAValuationDecimalsUnknown — a price and a supply exist but the token's declared scale
	// does not, so there is no exponent to divide the supply by.
	//
	// Contract rows only: a classic asset is 7 decimals by protocol and a SAC inherits that, but
	// a SEP-41 token declares its own, and the catalogue default of 7 is a convention, not a
	// reading. Defaulting would publish a figure wrong by a power of ten, silently. Also
	// reported when the scale disagrees with the one the DEX price was normalised with. The
	// circulating supply is still served; it is a raw chain fact and needs no scale.
	RWAValuationDecimalsUnknown = "decimals_unavailable"
)

// RWAValuation carries a row valuation and the reason when there is
// none. Both money fields are absent unless Status is published.
type RWAValuation struct {
	Status   string  `json:"status"`
	PriceUSD *string `json:"price_usd,omitempty"`
	// PriceBasis names a price that is NOT a direct market observation, verbatim from the
	// listing row (declared_peg or transitive). Absent means the price came from a market.
	// It is served so the figure is never shown with its derivation hidden.
	PriceBasis   string  `json:"price_basis,omitempty"`
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
}

// RWAAsset is one member of the set.
type RWAAsset struct {
	AssetID string `json:"asset_id"`
	// Code and Issuer are the classic (code, issuer) identity. BOTH are empty on a
	// contract-issued row, which carries ContractID instead: filling either from contract
	// metadata would put a self-declared string in the field assets are identified by.
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	// ContractID is the C-strkey of a contract-issued member, and the
	// whole of its identity. Empty on a classic row. Exactly one of
	// (Code, Issuer) and ContractID is populated on every row.
	ContractID string `json:"contract_id,omitempty"`
	// Symbol is the token symbol the CONTRACT declares in its on-chain metadata. It is display
	// text, not identity: two contracts may declare the same symbol and are different assets.
	// Empty on a classic row and on a contract with no readable metadata.
	Symbol string `json:"symbol,omitempty"`
	Slug   string `json:"slug,omitempty"`
	// Name is the [[CURRENCIES]] name from the issuer-bound SEP-1
	// entry. Issuer-authored display text.
	Name string `json:"name,omitempty"`
	// HomeDomain is the domain the issuer account set ON CHAIN, from
	// which the attestation was fetched. Empty on a contract-issued row:
	// a contract has no issuer account and no attestation was fetched.
	HomeDomain string `json:"home_domain,omitempty"`
	// IssuerDirectoryName, IssuerDirectoryTags and IssuerDirectoryDomain
	// are the independent third-party label on the issuer — the evidence
	// for R3 or C2. The domain is the directory's, not an on-chain one.
	IssuerDirectoryName   string   `json:"issuer_directory_name,omitempty"`
	IssuerDirectoryTags   []string `json:"issuer_directory_tags,omitempty"`
	IssuerDirectoryDomain string   `json:"issuer_directory_domain,omitempty"`
	// Basis names which requirement-4 arm admitted this asset.
	Basis string `json:"basis"`
	// Recognition names which independent party's naming satisfied the recognition requirement
	// (C2 on a contract row, R3 on a classic row). On classic rows it is
	// `curated_account_directory` when the directory lists this issuer account, or
	// `curated_account_directory_via_domain_sibling` when it lists another account that the same
	// issuer-bound SEP-1 binds on the same domain.
	//
	// The routes are not equal evidence: `curated_account_directory` is an address-level identity
	// directory that carries scam flags and admits on its own, while
	// `independent_listing_corroborating_curated_binding` needs two sources that do not read each
	// other (a listing platform and an in-repo binding), neither sufficient alone. Consumers can
	// filter on this field instead of reconstructing the rule.
	Recognition string `json:"recognition"`
	// AnchorClass is the closed-vocabulary class, present only under
	// the declaration basis.
	AnchorClass string `json:"anchor_class,omitempty"`
	// AnchorAsset is the off-chain instrument the issuer declared this
	// token anchors to, verbatim.
	AnchorAsset string `json:"anchor_asset,omitempty"`
	// ISINCollision is present when anchor_asset is an ISIN that more
	// than one issuer account declares. Informational only: it never
	// changes membership, valuation or the reference.
	ISINCollision *RWAISINCollision `json:"isin_collision,omitempty"`
	// Valuation is the OBSERVED-MARKET-PRICE money, or the reason there
	// is none. Unchanged by anything on the reference basis below.
	Valuation RWAValuation `json:"valuation"`
	// ReferenceValuation is the same float valued at the reference
	// price instead — a claim about the backing, not an observation of
	// a payment. Always present, always with a status, and never summed
	// into Valuation.
	ReferenceValuation RWAReferenceValuation `json:"reference_valuation"`
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
	// Curator is present only on rows served in `curated_assets`: what
	// the curator said about the row, verbatim, and whether the verified
	// set carries the same address.
	Curator *RWACurator `json:"curator,omitempty"`
	// CirculatingSupply is a raw chain fact and is served even when the
	// valuation is withheld, in the smallest integer unit.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// SupplyBasis names the reading that produced CirculatingSupply (ADR-0011 vocabulary, verbatim
	// from the listing row). Absent when no arm answered.
	//
	// Every reference_valuation here is circulating_supply times an oracle price, so supply
	// completeness is half of every total; a consumer reconciling against an issuer's figure needs
	// to know whether it compares complete totals or a total against a floor.
	SupplyBasis string `json:"supply_basis,omitempty"`
	// CirculatingSupplyLowerBound is true when CirculatingSupply is a provable floor, the per-row
	// sibling of the totals' lower_bound. Derived from SupplyBasis ([supply.Basis.LowerBound]) so
	// the two cannot disagree.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`
	// Decimals is the on-chain smallest-unit scale: 7 for every classic asset, whatever a SEP-41
	// contract declares otherwise. Served so both valuations can be re-derived by hand
	// (circulating_supply / 10^decimals times price).
	//
	// nil (JSON null) when a contract's scale could not be read: the catalogue default of 7 is a
	// convention, not a reading, and both valuation bases refuse to divide when it is nil.
	Decimals *int `json:"decimals"`
	// Volume24hUSD is the trailing-24h USD trade volume as served on
	// /v1/assets.
	Volume24hUSD *string `json:"volume_24h_usd,omitempty"`
	// FirstSeenLedger is the ledger this asset was first observed at,
	// from an index complete since genesis.
	FirstSeenLedger  uint32 `json:"first_seen_ledger,omitempty"`
	ObservationCount int64  `json:"observation_count"`
}

// RWAISINCollision reports one ISIN declared in anchor_asset by several
// issuer accounts. A declared ISIN is a claim, not proof of holding the
// security, so the row only says how many accounts make the same claim.
type RWAISINCollision struct {
	// ISIN is the declared identifier in canonical upper-case form.
	ISIN string `json:"isin"`
	// DeclaredByIssuers counts the distinct issuer accounts, this one
	// included, whose issuer-bound SEP-1 declares ISIN.
	DeclaredByIssuers int `json:"declared_by_issuers"`
}

// RWAGroupTotal is one row of the per-class breakdown.
type RWAGroupTotal struct {
	Class  string `json:"class"`
	Assets int    `json:"assets"`
	// MarketCapUSD is absent when no asset in the group publishes one,
	// for the same reason the summary total is.
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
	// ReferenceValueUSD is the same group on the reference basis, and AssetsReferenceUnvalued
	// is that basis's own unvalued count. Both bases are broken down because they admit
	// different rows; one alone cannot show which class the totals' difference came from.
	ReferenceValueUSD       *string `json:"reference_value_usd,omitempty"`
	AssetsReferenceUnvalued int     `json:"assets_reference_unvalued"`
	// Note explains `unclassified`, which would read as a gap in our data when it reflects the
	// issuers': these rows were admitted because an independent oracle prices the instrument,
	// while the SEP-1 attestation declares no usable class. Publishing one would invent it.
	Note string `json:"note,omitempty"`
}

// RWAIssuerTotal is one row of the per-issuer breakdown.
type RWAIssuerTotal struct {
	Issuer         string  `json:"issuer"`
	Name           string  `json:"name,omitempty"`
	HomeDomain     string  `json:"home_domain,omitempty"`
	Assets         int     `json:"assets"`
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
	// The reference basis for this issuer, on the same terms as
	// [RWAGroupTotal].
	ReferenceValueUSD       *string `json:"reference_value_usd,omitempty"`
	AssetsReferenceUnvalued int     `json:"assets_reference_unvalued"`
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
	// isinCollision is set when anchorAsset is an ISIN another issuer
	// account also declares.
	isinCollision *RWAISINCollision
	basis         string
	anchorClass   string
	recognition   string
	dirName       string
	dirTags       []string
}

// rwaMembership is one rebuild: the admitted set, the refusal tally
// over every candidate evaluated, and the census of the population both
// were drawn from.
type rwaMembership struct {
	members  []rwaMember
	refusals map[string]int
	// census accounts for every issuer row and SEP-1 declaration the attestation scan walked,
	// including stages upstream of rwa.Qualify that no refusal reason can describe.
	census timescale.Sep1BoundCensus
	// overIssuerCap counts members rwaMaxIssuers turned away. They met the definition, so they
	// are counted apart from the refusals.
	overIssuerCap int
	// duplicateDeclarations counts candidates naming a (code, issuer) already admitted. A toml
	// may declare an asset twice; admitting both would serve it twice and double its market cap
	// in every total.
	duplicateDeclarations int
	// available is false when no attestation reader is wired: a configuration statement, not an
	// empty population.
	available bool
	// readFailed records that a wired arm did not answer (attestation scan, classic directory
	// lookup, or contract directory scan errored).
	//
	// `available` cannot say this: it is false for an unwired reader and a failed read alike,
	// which need opposite cache handling. An unwired arm is permanent and cacheable; a failed
	// read is an outage whose empty arm must never overwrite a good set (see
	// [Server.refreshRWAMembership]).
	readFailed bool

	// contracts is the contract arm's admitted set: tokens with no (code, issuer) pair
	// (rwa_contracts.go). It shares this value because both arms share one rebuild, cache entry
	// and TTL; two caches would let the halves of one response come from different moments.
	contracts []rwaContractMember
	// contractCensus accounts for the curated-directory population that arm narrowed, including
	// stages upstream of rwa.QualifyContract.
	contractCensus rwaContractCensus
	// listingCensus accounts for C2's second arm: curated bindings an independent listing
	// directory was asked to corroborate.
	listingCensus rwaListingCensus
	// contractRefusals and listingRefusals are kept apart because both arms run
	// rwa.QualifyContract and can produce the same reason string; a single tally would let the
	// funnel attribute one arm's refusal to the other's narrowing. Their union is already in
	// refusals, which `refused[]` publishes.
	contractRefusals map[string]int
	listingRefusals  map[string]int
	// listingNotObserved is the listing arm's equivalent of contractsNotObserved, kept apart so
	// each arm closes its own arithmetic.
	listingNotObserved int
	// contractsNotObserved counts admitted contracts with no catalogue row. It lives on the
	// membership because the funnel is built from this value and a separately-travelling count
	// could be dropped on a path that forgot it.
	contractsNotObserved int
	// unreached names the recognised issuing entities this index holds no token for. It admits
	// nothing; see RWAUnreachedEntity.
	unreached []rwaUnreachedEntity

	// builtAt is when the rebuild that produced this set finished. Set once, so a set carries its
	// own age wherever it travels.
	builtAt time.Time
	// servedStale and rebuildFailedAt are the only fields a rebuild does not set. They describe
	// this serve, not the set, and are stamped onto a copy by [Server.stampRWAServeState]; a set
	// recording its own staleness at build time would say false forever.
	servedStale     bool
	rebuildFailedAt time.Time
}

// rwaCandidateFilter keeps only the bound SEP-1 entries that could satisfy requirement 4, so the
// scan never materialises the tens of thousands that declare an NFT, a crypto token or nothing.
//
// Every asset-side input a requirement-4 arm reads must be passed (anchor asset as well as code
// and type); otherwise entries such as ISIN-only declarations are counted as EntriesFiltered
// before [Server.admitClassicCandidates] ever sees them.
func rwaCandidateFilter(c timescale.Sep1BoundCurrency) bool {
	return rwa.CouldQualify(c.Code, c.AnchorAssetType, c.AnchorAsset)
}

// buildRWAMembership runs BOTH arms and returns the combined set.
//
// The arms are independent: one walks SEP-1 attestations keyed by (code, issuer), the other
// curated directory entries keyed by contract address. Either may be unavailable without
// emptying the response (the funnel says which was measured).
//
// Refusals from both arms land in one tally; the reason vocabularies are disjoint, so a reader
// can still tell which rule applied.
func (s *Server) buildRWAMembership(ctx context.Context) rwaMembership {
	out := s.buildRWAClassicMembership(ctx)
	c := s.buildRWAContractMembership(ctx)
	out.contracts = c.members
	out.unreached = c.unreached
	out.contractCensus = c.census
	out.listingCensus = c.listingCensus
	out.contractRefusals = c.refusals
	out.listingRefusals = c.listingRefusals
	// A wired contract reader whose scan errored leaves census.available false just like an
	// unwired one; only this layer holds the wiring, so only it can tell them apart. That decides
	// whether the rebuild may be cached over the last good set (see [rwaMembership.readFailed]).
	//
	// The listing arm is not folded in: it fails closed, dropping every binding under
	// `independent_listing_unavailable`, so its outage is already a stated refusal.
	if s.RWAContracts != nil && !c.census.available {
		out.readFailed = true
	}
	// One `refused[]` for the whole surface; the per-arm tallies stay for the funnel, which must
	// attribute each refusal to its own narrowing.
	for _, tally := range []map[string]int{c.refusals, c.listingRefusals} {
		for reason, n := range tally {
			out.refusals[reason] += n
		}
	}
	return out
}

// buildRWAClassicMembership applies the definition to every issuer-bound SEP-1 attestation.
// It does one indexed scan, one batch directory lookup (no N+1), then the per-candidate verdict;
// nothing here reads a price.
func (s *Server) buildRWAClassicMembership(ctx context.Context) rwaMembership {
	out := rwaMembership{refusals: map[string]int{}}
	reader, ok := s.Sep1Cache.(Sep1BoundCurrencyReader)
	if !ok {
		return out
	}
	out.available = true

	bound, census, err := reader.BoundSep1Currencies(ctx, rwaCandidateFilter)
	if err != nil {
		s.logger.Warn("rwa membership: bound sep1 scan failed", "err", err)
		out.available = false
		out.readFailed = true
		return out
	}
	out.census = census
	if why := census.Check(); why != "" {
		// The funnel publishes itself as unbalanced; the log names the stage the response cannot.
		s.logger.Warn("rwa membership: attestation census does not balance", "why", why)
	}
	// The pre-filter drops bound entries naming no real-world instrument (the NFT, crypto and
	// undeclared majority). They are counted as requirement-4 refusals, since a tally of only the
	// materialised rows would understate the population narrowed from.
	//
	// This is the one bucket reported out of R1-R4 order: they were dropped before requirement 3
	// was evaluated, so some also fail it. The funnel keeps them as their own stage.
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
	if s.Directory != nil && len(addrs) > 0 {
		entries, err = s.Directory.DirectoryEntriesByAddresses(ctx, addrs)
		if err != nil {
			// R3 keeps impersonators out and cannot be evaluated without the directory, so fail
			// closed (unlike the display overlay, which only omits a label); serving without it
			// would publish the lookalike-domain population as real-world assets.
			s.logger.Warn("rwa membership: directory batch lookup failed", "n", len(addrs), "err", err)
			out.available = false
			out.readFailed = true
			return out
		}
	}

	s.admitClassicCandidates(&out, bound, entries)
	return out
}

// admitClassicCandidates runs the ordered R1-R4 evaluation over the bound declarations and
// accumulates the admitted set, the refusal tally and the two structural drops onto the
// membership. It is split from [Server.buildRWAClassicMembership] to keep the verdict loop
// separate from read-error handling.
func (s *Server) admitClassicCandidates(
	out *rwaMembership,
	bound []timescale.Sep1BoundCurrency,
	entries map[string]timescale.DirectoryEntry,
) {
	issuers := map[string]struct{}{}
	admitted := make(map[string]struct{}, len(bound))
	isinDeclarers := rwaISINDeclarers(bound)
	// Domains with a directory-recognised, unflagged account among the
	// issuers bound on them: an account the same SEP-1 names beside one
	// of those is recognised by its sibling (rwa.RecognitionDomainSibling).
	recognisedDomains := map[string]struct{}{}
	for _, c := range bound {
		if c.HomeDomain == "" {
			continue
		}
		tags := entries[c.Issuer].Tags
		if rwa.HasRecognitionTag(tags) && !rwa.ScamFlagged(tags) {
			recognisedDomains[strings.ToLower(c.HomeDomain)] = struct{}{}
		}
	}
	for _, c := range bound {
		e := entries[c.Issuer]
		_, sibling := recognisedDomains[strings.ToLower(c.HomeDomain)]
		v := rwa.Qualify(rwa.Candidate{
			Code:               c.Code,
			Issuer:             c.Issuer,
			BoundSep1:          true,
			DeclaredAnchorType: c.AnchorAssetType,
			// The declaration's two halves answer different questions
			// and an issuer may give a usable answer to only one. Both
			// are passed; the definition decides which it can use.
			DeclaredAnchorAsset: c.AnchorAsset,
			DirectoryTags:       e.Tags,
			SiblingRecognised:   sibling && c.HomeDomain != "",
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
				out.overIssuerCap++
				continue
			}
			issuers[c.Issuer] = struct{}{}
		}
		admitted[key] = struct{}{}
		out.members = append(out.members, rwaMember{
			code:          c.Code,
			issuer:        c.Issuer,
			name:          c.Name,
			homeDomain:    c.HomeDomain,
			anchorAsset:   c.AnchorAsset,
			isinCollision: rwaISINCollisionOf(c.AnchorAsset, isinDeclarers),
			basis:         v.Basis,
			anchorClass:   v.AnchorClass,
			recognition:   v.Recognition,
			dirName:       e.Name,
			dirTags:       e.Tags,
		})
	}
}

// rwaISINDeclarers counts, per declared ISIN, the distinct issuer
// accounts declaring it across every bound declaration — refused ones
// included, since a refused impostor still claims the security.
func rwaISINDeclarers(bound []timescale.Sep1BoundCurrency) map[string]int {
	seen := map[string]map[string]struct{}{}
	for _, c := range bound {
		isin, ok := rwa.CanonicalISIN(c.AnchorAsset)
		if !ok {
			continue
		}
		if seen[isin] == nil {
			seen[isin] = map[string]struct{}{}
		}
		seen[isin][c.Issuer] = struct{}{}
	}
	out := make(map[string]int, len(seen))
	for isin, accounts := range seen {
		out[isin] = len(accounts)
	}
	return out
}

// rwaISINCollisionOf is the collision a row declaring anchorAsset
// carries, or nil when the ISIN has a single declarer or is no ISIN.
func rwaISINCollisionOf(anchorAsset string, declarers map[string]int) *RWAISINCollision {
	isin, ok := rwa.CanonicalISIN(anchorAsset)
	if !ok || declarers[isin] < 2 {
		return nil
	}
	return &RWAISINCollision{ISIN: isin, DeclaredByIssuers: declarers[isin]}
}

// rwaMembershipRetryGap rate-limits rebuild attempts, not rebuilds. A failed rebuild leaves the
// entry stale, so without a gap every request would retry and a broken read would become a scan
// storm against already-struggling tables.
const rwaMembershipRetryGap = 60 * time.Second

// rwaMembershipBudget is the detached rebuild's own deadline, not a request budget. It must
// exceed the measured build cost (~11.5 s in production) with room to spare, or the refresh is
// killed halfway and the entry never warms.
const rwaMembershipBudget = 2 * time.Minute

// cachedRWAMembership returns the membership set for a request.
//
// Once a set has been built it never blocks on a rebuild: a stale set is served as-is and a
// detached rebuild is kicked behind it. An inline rebuild scans every issuer-bound SEP-1
// payload plus the curated directory (~11 s), which would make latency bimodal.
//
// The one case that waits is a never-filled cache: an empty set would falsely state that no
// real-world asset exists on Stellar. [Server.PrewarmRWA] makes the prewarm goroutine the
// waiter, not a visitor.
func (s *Server) cachedRWAMembership(ctx context.Context) rwaMembership {
	served, flight := s.readRWAMembership() //nolint:contextcheck // the rebuild is deliberately detached from this request's context — see this function's doc; ctx is honoured by the cold-cache wait below.
	if served != nil {
		return s.stampRWAServeState(*served)
	}
	if flight == nil {
		// Never built, and an attempt is gapped out. The handler turns
		// this into a stated absence, not an empty set.
		return rwaMembership{refusals: map[string]int{}}
	}
	select {
	case <-flight:
	case <-ctx.Done():
		return rwaMembership{refusals: map[string]int{}}
	}
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	if s.rwaCache != nil {
		return s.stampServeStateLocked(*s.rwaCache)
	}
	return rwaMembership{refusals: map[string]int{}}
}

// rwaMembershipSetOf dates the served set, or returns nil when there is no set to date.
//
// Gated on the build instant: a zero-valued set and a real but empty set render identically
// elsewhere, and only one is an answer. Omission is more honest than publishing the Unix epoch.
func rwaMembershipSetOf(m rwaMembership) *RWAMembershipSet {
	if m.builtAt.IsZero() {
		return nil
	}
	out := &RWAMembershipSet{BuiltAt: WireTime(m.builtAt), Stale: m.servedStale}
	if !m.rebuildFailedAt.IsZero() {
		failed := WireTime(m.rebuildFailedAt)
		out.RebuildFailedAt = &failed
	}
	return out
}

// stampRWAServeState records, on a copy of the set, the two facts that belong to this serve
// rather than the set: whether it has outlived its lifetime, and whether its replacement failed.
// Both are "as of now" answers, so they cannot live on the cached value.
//
// The copy matters: [Server.readRWAMembership] returns a pointer into the cache, and writing
// through it would make one request's view of the clock permanent for every later reader.
func (s *Server) stampRWAServeState(m rwaMembership) rwaMembership {
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	return s.stampServeStateLocked(m)
}

// stampServeStateLocked is [Server.stampRWAServeState] for callers that
// already hold rwaMu.
func (s *Server) stampServeStateLocked(m rwaMembership) rwaMembership {
	m.servedStale = !s.rwaAt.IsZero() && time.Since(s.rwaAt) >= rwaMembershipTTL
	m.rebuildFailedAt = s.rwaFailedAt
	return m
}

// readRWAMembership serves what the cache holds and kicks a detached rebuild when the entry is
// stale or absent and no attempt is running or too recent. It returns the served set (nil only
// before the first successful build) and the in-flight rebuild's completion channel (nil when
// none is running). The channel is for [Server.PrewarmRWA] and the cold-cache arm of
// [Server.cachedRWAMembership]; a warm-or-stale request takes the set and goes.
func (s *Server) readRWAMembership() (*rwaMembership, chan struct{}) {
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()

	served := s.rwaCache // last good set; nil only before the first success
	if served != nil && time.Since(s.rwaAt) < rwaMembershipTTL {
		return served, nil
	}
	if s.rwaFlight != nil {
		return served, s.rwaFlight
	}
	if time.Since(s.rwaAttemptAt) < rwaMembershipRetryGap {
		return served, nil
	}
	s.rwaAttemptAt = time.Now() // advances on failure too
	flight := make(chan struct{})
	s.rwaFlight = flight
	// G118 is the intended behaviour, not a defect: detaching from the
	// request context is what stops the scan being killed at the request
	// deadline and restarted, unbounded, by the next caller.
	go s.refreshRWAMembership(flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see cachedRWAMembership's doc.
	return served, flight
}

// refreshRWAMembership rebuilds the set on a detached context and swaps it in. A failed rebuild
// leaves the previous set untouched: a failed read must not empty a surface whose inputs move
// daily.
func (s *Server) refreshRWAMembership(done chan struct{}) {
	// Deferred so a panic in the scan cannot leave the single-flight marker set, which would
	// freeze the cache for the life of the process since a non-nil flight is never replaced.
	defer s.endRWAMembershipFlight(done)
	defer worker.Recover(s.logger, "api-rwa-membership-refresh")

	ctx, cancel := context.WithTimeout(context.Background(), rwaMembershipBudget)
	defer cancel()

	built := s.buildRWAMembership(ctx)
	// One instant for the published age and the lifetime clock, so `stale` always describes the
	// same moment as the `built_at` beside it.
	at := time.Now().UTC()
	built.builtAt = at

	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	// Every wired arm must have answered; otherwise the rebuild is a partial measurement and
	// may not be published. Requiring only either arm is wrong: a failed classic scan beside a
	// healthy contract scan would overwrite the last good set with a half-empty one, re-date it
	// and clear the failure stamp.
	//
	// The good half is discarded on purpose: merging it into an older other arm would put one
	// `built_at` over two moments. A cold cache with an arm down stays cold and the handler
	// states the absence (see rwaBasisUnavailable).
	if built.readFailed || (!built.available && !built.contractCensus.available) {
		// The previous set keeps being served, and rwaAttemptAt (already advanced) stops a retry
		// storm. The failure is recorded, not only logged, because from outside a lapsed set about
		// to be replaced and one nothing is replacing look identical.
		s.rwaFailedAt = at
		// The log names which arm went down, which the wire deliberately does not carry.
		s.logger.Warn("rwa membership rebuild failed (serving the last good set)",
			"classic_answered", built.available,
			"contracts_answered", built.contractCensus.available)
		return
	}
	// Cleared on success: the field means "the latest attempt failed", not "one ever failed".
	s.rwaFailedAt = time.Time{}
	s.rwaCache = &built
	s.rwaAt = at
}

// endRWAMembershipFlight releases the single-flight marker and wakes
// every waiter. Deferred by refreshRWAMembership so it runs on EVERY
// exit, panic included.
func (s *Server) endRWAMembershipFlight(done chan struct{}) {
	s.rwaMu.Lock()
	s.rwaFlight = nil
	s.rwaMu.Unlock()
	close(done)
}

// PrewarmRWA fills the three caches the /rwa page reads: the membership set shared by all RWA
// routes, then the value and premium series behind the history panels (which build on it), so
// the first build is paid by this goroutine, not the first visitor.
//
// There is no cache key to drift: membership is one process-wide entry read through
// [Server.readRWAMembership], as in [Server.cachedRWAMembership]. Best-effort: no reader wired,
// or a failed scan, leaves the cache as it was.
func (s *Server) PrewarmRWA(ctx context.Context) {
	// The membership set depends on neither history reader, so it warms on its own terms.
	if _, flight := s.readRWAMembership(); flight != nil { //nolint:contextcheck // same detachment as the request path; ctx is honoured by the wait below, not by the scan.
		// Waiting makes this a prewarm rather than a nudge. Shutdown returns at once and still
		// lets the detached rebuild finish and record what it learned.
		select {
		case <-flight:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return
	}
	// The two series below are warmed behind the SAME guards their handlers apply, in the same
	// order; a prewarm starting a build its handler would refuse reads through readers the
	// handler proved it may not.
	//
	// Both BLOCK on their own build, unlike the membership above, so this goroutine, not a
	// page reader, should pay for it.
	if s.AssetsReader == nil || s.OracleHistory == nil {
		return
	}
	s.cachedRWAValueHistory(ctx)
	if ctx.Err() != nil || s.MarketHistory == nil {
		return
	}
	s.cachedRWAPremiumHistory(ctx)
}

// ─── handler ────────────────────────────────────────────────────────

// handleRWAAssets serves GET /v1/rwa/assets.
//
// No query parameters: the set is small and complete by construction, so there is nothing to
// page and any filter is better applied by the caller over the whole document.
func (s *Server) handleRWAAssets(w http.ResponseWriter, r *http.Request) {
	view := RWAAssetsView{
		Definition:        rwaDefinition(),
		Assets:            []RWAAsset{},
		ByClass:           []RWAGroupTotal{},
		ByIssuer:          []RWAIssuerTotal{},
		Refused:           []RWARefusal{},
		UnreachedEntities: []RWAUnreachedEntity{},
	}
	if s.AssetsReader == nil {
		view.Summary.Basis = rwaBasisUnavailable
		view.Funnel = rwaFunnelUnavailable()
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	m := s.cachedRWAMembership(r.Context())
	view.Refused = rwaRefusalRows(m.refusals)
	view.UnreachedEntities = rwaUnreachedRows(m.unreached)
	// Dated BEFORE the unavailable branch below, because a response
	// that publishes no set is exactly where a reader most needs to
	// know whether one was ever built and when.
	view.Membership = rwaMembershipSetOf(m)
	// Unavailable means NEITHER arm answered. One arm failing while the
	// other reports a set is a partial measurement, not an absent one,
	// and the funnel is what says which of the two happened.
	if !m.available && !m.contractCensus.available && len(m.members) == 0 && len(m.contracts) == 0 {
		view.Summary.Basis = rwaBasisUnavailable
		view.Funnel = rwaFunnelUnavailable()
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{Degraded: m.readFailed}})
		return
	}

	if !middleware.ChargeRateLimit(w, r, rwaAssetsCost(m)) {
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

	// The contract arm fails the response exactly as the classic arm does: degrading to zero
	// rows would drop admitted members from the totals without a lower_bound. Both contract
	// arms share one valuation budget inside the handler ceiling; a cut (not failed) valuation
	// is served under flags.stale.
	vctx, cancel := context.WithTimeout(r.Context(), rwaContractValuationBudget)
	defer cancel()
	contractRows, degraded, live := s.rwaVerifiedContractRows(vctx, w, r, m.contracts)
	if !live {
		return
	}
	// Attributed per C2 arm, from the same two values the projection
	// reads. The two arms narrow separate populations and each has to
	// close its own arithmetic, so one combined count would leave both
	// unable to.
	dirCounts, listingCounts := rwaContractArmSplit(m.contracts, contractRows)
	m.contractsNotObserved = dirCounts.notObserved
	m.listingNotObserved = listingCounts.notObserved
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
	// The listing rows that priced the admitted contracts are indexed from the MEMBERSHIP, not a
	// second read: the row that prices an asset must be the one that recognised it.
	listingRefs := rwaListingReferencesOf(m.contracts)
	// The classic arm's listing rows come from the same cached read and the same rule
	// ([listingEntryIn]) as /v1/assets: the `CODE-GISSUER` id first, then the derived Stellar
	// Asset Contract address, because the directory publishes each asset under one of those forms
	// with no pattern. The contract map above cannot answer for a classic member.
	//
	// An unavailable snapshot yields a nil map, so every lookup keeps its "no listing named this
	// address" refusal: this read can add a reference, never remove one.
	classicListingRefs := s.assetListingSnapshot(r.Context()).byAddress
	now := time.Now()
	for i := range view.Assets {
		rwaApplyReference(&view.Assets[i], refs, listingRefs, classicListingRefs, now)
	}
	view.Summary = rwaSummarise(view.Assets, rwaTruncationOf(m, join))
	view.ByClass = rwaByClass(view.Assets)
	view.ByIssuer = rwaByIssuer(view.Assets)
	view.Funnel = rwaFunnelOf(m, join, len(classicAssets), dirCounts.served, listingCounts.served, view.Assets)
	// The curated arm runs LAST, over the finished verified view, so it
	// can say which of its rows the verified set already carries and can
	// never feed a figure back into the totals above.
	curatedDegraded := s.attachRWACurated(vctx, &view, now)
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{Stale: degraded || curatedDegraded, Degraded: degraded || curatedDegraded || m.readFailed}})
}

// rwaVerifiedContractRows values the verified contract arm. A valuation ctx cut short still
// returns its rows, reported as degraded (flags.stale). A failed read fails the response like the
// classic arm's does, since a swallowed error would publish admitted-but-unread members as never
// observed. live is false when nothing further should be written: the client has gone, or the
// failure response was already written.
func (s *Server) rwaVerifiedContractRows(
	ctx context.Context, w http.ResponseWriter, r *http.Request, members []rwaContractMember,
) (rows map[string]AssetDetail, degraded, live bool) {
	rows, _, cut, err := s.rwaContractListingRows(ctx, members)
	if err == nil {
		return rows, cut, true
	}
	if clientAborted(r, err) {
		return nil, false, false
	}
	s.logger.Error("rwa contract listing read failed", "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
	return nil, false, false
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
			"issuer independently recognised in the curated account directory and not scam-flagged, " +
				"or unflagged and bound by the same issuer-bound SEP-1 on the same domain as an account the directory recognises",
			"real-world instrument by SEP-1 anchor_asset_type, by an ADR-0028 oracle feed, or by a well-formed ISIN in SEP-1 anchor_asset",
		},
		ContractRequirements: []string{
			"contract-issued token identified by its contract address",
			"no scam-class tag on that address in the curated account directory",
			"that exact contract address named either by the curated account directory with an issuing-class tag, " +
				"or by an independent listing directory AND an in-repo curated binding together",
			"real-world instrument by an in-repo curated binding or by an ADR-0028 oracle feed on the on-chain symbol",
		},
		AnchorClasses:              rwa.AnchorClasses(),
		ContractAnchorClasses:      rwa.ContractAnchorClasses(),
		RecognitionTags:            rwa.RecognitionTags(),
		ContractRecognitionTags:    rwa.ContractRecognitionTags(),
		ContractRecognitionSources: rwa.ContractRecognitionSources(),
		ScamFlagTags:               append([]string(nil), timescale.DirectoryScamFlagTags...),
		BoundInstruments:           rwaBoundInstruments(),
		BoundContractInstruments:   rwaBoundContractInstruments(),
		DocumentationURL:           "https://stellarindex.io/docs/methodology/rwa-definition",
	}
}

// rwaCatalogueJoin counts what the join from the admitted set to the
// asset catalogue removed. Both of its fields count drops at the very
// end of the funnel, where an asset that met every requirement could
// otherwise vanish without appearing in any tally.
type rwaCatalogueJoin struct {
	// pagesTruncated counts member issuers whose classic-asset listing
	// page filled to rwaAssetsPerIssuer, so their long tail went unread
	// and a member in it cannot be found.
	pagesTruncated int
	// notObserved counts admitted members with no catalogue row: the
	// asset was attested but has never been seen on chain.
	notObserved int
}

// rwaAssetsCost is the rate-limit price, in tokens, of one /v1/rwa/assets request: one per store
// listing read. Membership is cached but valuation is not, so every request runs a listing read
// and the full post-query pipeline per member issuer, plus one batched catalogue read for the
// contract arm. The pipeline's own supply and price reads are deliberately not counted.
func rwaAssetsCost(m rwaMembership) int {
	cost := len(rwaMemberIssuers(m))
	if len(m.contracts) > 0 {
		cost++
	}
	return max(1, cost)
}

// rwaMemberIssuers returns the distinct classic member issuers, sorted —
// the set [Server.rwaListingRows] reads one page per, and
// [rwaAssetsCost] prices.
func rwaMemberIssuers(m rwaMembership) []string {
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
	return issuers
}

// rwaListingRows reads each member issuer through the same listing query /v1/assets uses (one
// indexed page per issuer) and returns rows keyed by asset_id, plus what the read could not
// cover. Running the real listing path keeps this surface from publishing a valuation /v1/assets
// would have refused.
func (s *Server) rwaListingRows(
	ctx context.Context, m rwaMembership,
) (map[string]AssetDetail, rwaCatalogueJoin, error) {
	var join rwaCatalogueJoin
	issuers := rwaMemberIssuers(m)

	codesByIssuer := make(map[string][]string, len(issuers))
	for _, mem := range m.members {
		codesByIssuer[mem.issuer] = append(codesByIssuer[mem.issuer], strings.TrimSpace(mem.code))
	}

	out := make(map[string]AssetDetail, len(m.members))
	for _, issuer := range issuers {
		rows, err := s.AssetsReader.ListAssetsExt(ctx, timescale.ListAssetsOptions{
			Limit:  rwaAssetsPerIssuer,
			Issuer: issuer,
			Type:   "classic",
		})
		if err != nil {
			return nil, join, err
		}
		// A full page means the issuer has more classic assets than one
		// read covers, so a member in the unread tail would disappear
		// from the set with nothing to show for it. The cap is documented
		// as reported, so the truncation is counted here.
		if len(rows) >= rwaAssetsPerIssuer {
			join.pagesTruncated++
		}
		keep, memberCodes := rwaMatchMemberRows(codesByIssuer[issuer], rows)
		if len(keep) == 0 {
			continue
		}
		details := make([]AssetDetail, 0, len(keep))
		for _, row := range keep {
			details = append(details, assetDetailFromAssetRow(row))
		}
		// The /v1/assets post-query pipeline, in its order. The order is load-bearing: the
		// collision stamp precedes the market-cap fill (which reads it), the peg fill follows both
		// the gate that would strip it and the fill that must not derive a valuation from it, and
		// the directory stamp brings the scam suppression.
		//
		// The whole pipeline runs, not a subset: a surface running only the steps it thought it
		// needed would drift from /v1/assets one omission at a time, publishing a figure that page
		// withholds or the reverse.
		s.stampListingCollisions(details)
		s.applySubstanceGateToListing(ctx, details, false)
		s.fillMarketCapsFromSupply(ctx, details, assetRowSourceCounts(keep))
		// Additive, and only to a raw chain fact. The step above reads a supply only to multiply
		// it by a price, so an unpriced asset keeps none: right on the listing, wrong here,
		// where the reference basis values a float that has never traded. This uses the same
		// supply readers and preference order and touches no price, market cap or gate.
		// The contract arm needs no equivalent: fillContractMarketCaps reads supply first.
		s.rwaFillMissingSupply(ctx, details)
		s.fillDeclaredPegPricesInListing(ctx, details)
		s.fillIssuerDirectoryTags(ctx, details)
		for i, d := range details {
			out[rwaKey(memberCodes[i], issuer)] = d
		}
	}
	return out, join, nil
}

// rwaFillMissingSupply attaches circulating supply to the RWA rows the
// market-cap fill left without one.
//
// Only rows with no reading at all are touched, so a supply the pipeline decided on (the
// dust-suppressed and ticker-collision branches attach one deliberately) is never overwritten.
// Supply is a chain fact, not a price claim, so this changes no valuation and no gate, and
// /v1/assets is untouched.
func (s *Server) rwaFillMissingSupply(ctx context.Context, rows []AssetDetail) {
	missing := false
	for i := range rows {
		if rows[i].CirculatingSupply == nil {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	precise := s.latestPreciseSupply(ctx)
	broad := s.cachedClassicSupply(ctx)
	lake := s.classicLakeSupply(ctx, rows)
	for i := range rows {
		if rows[i].CirculatingSupply != nil {
			continue
		}
		// One shared preference chain with the market-cap fill, and it
		// names the arm it took: [classicSupplyReading].
		circ, basis := classicSupplyReading(rows[i].AssetID, precise, lake, broad)
		if circ == "" {
			continue
		}
		stampCirculatingSupply(&rows[i], circ, basis)
	}
}

// rwaKey is the membership join key: code and issuer both exact, since
// asset codes are case-sensitive and USDX and usdx are two assets.
func rwaKey(code, issuer string) string {
	return strings.TrimSpace(code) + "-" + issuer
}

// rwaMatchMemberRows pairs one issuer's member codes with its listing
// rows via [matchAssetCode], so a mis-cased declaration still reaches
// its asset. A row two members resolve to goes to the member spelling
// it exactly, or to neither: serving it twice would count it twice.
func rwaMatchMemberRows(codes []string, rows []timescale.AssetRow) ([]timescale.AssetRow, []string) {
	rowCodes := make([]string, len(rows))
	for i := range rows {
		rowCodes[i] = rows[i].Code
	}
	claims := make(map[int][]string, len(codes))
	for _, c := range codes {
		if i := matchAssetCode(c, rowCodes); i >= 0 {
			claims[i] = append(claims[i], c)
		}
	}
	keep := make([]timescale.AssetRow, 0, len(claims))
	memberCodes := make([]string, 0, len(claims))
	for i := range rows {
		c := claims[i]
		if len(c) > 1 && slices.Contains(c, rows[i].Code) {
			c = []string{rows[i].Code}
		}
		if len(c) == 1 {
			keep = append(keep, rows[i])
			memberCodes = append(memberCodes, c[0])
		}
	}
	return keep, memberCodes
}

// rwaAssetRows joins the membership evidence to the valued listing rows and returns how many
// members the join could not place. A member with no listing row (attested but never observed on
// chain) is dropped but counted, because "admitted then never served" is otherwise invisible.
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
			Recognition:         mem.recognition,
			AnchorAsset:         strings.TrimSpace(mem.anchorAsset),
			ISINCollision:       mem.isinCollision,
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			// The asset's OWN scale, carried from the listing row. The
			// reference valuation divides by it, and a constant in its
			// place would misprice every asset whose scale differs (see
			// [rwaReferenceValueUSD]).
			Decimals:     rwaDecimalsOf(d),
			Volume24hUSD: d.VolumeUSD24h,
		}
		a.SupplyBasis, a.CirculatingSupplyLowerBound = rwaSupplyProvenance(d)
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

// rwaSupplyProvenance carries the listing row's supply basis onto the RWA row, with whether a
// figure on that basis is a floor.
//
// Both come off the same field: the flag is [supply.Basis.LowerBound] applied to the stamped
// basis, not a second fact a later arm could forget to set.
//
// A row with a supply but no basis returns nothing rather than a guess. That state is reachable
// (the dust-suppressed and ticker-collision paths attach a supply without one), and naming an arm
// that did not answer is worse than naming none.
func rwaSupplyProvenance(d AssetDetail) (string, bool) {
	if d.CirculatingSupply == nil || d.SupplyBasis == nil {
		return "", false
	}
	b := supply.Basis(*d.SupplyBasis)
	return b.String(), b.LowerBound()
}

// rwaSortAssets applies the served ordering: published market cap descending, then observation
// count, then asset id.
//
// It runs once over the combined set: sorting each arm and concatenating would rank every
// classic row above every contract row whatever their valuations, a size claim nobody measured.
// Rows with no published valuation sort after every valued row.
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

// rwaDecimalsOf is the row's scale as served: nil when it is the
// catalogue default rather than a reading.
func rwaDecimalsOf(d AssetDetail) *int {
	if d.DecimalsUnresolved {
		return nil
	}
	v := d.Decimals
	return &v
}

// rwaValuationOf reads the valuation off the already-gated listing row. It never recomputes a
// price or market cap, so a figure withheld upstream cannot reappear here as a number.
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
		// Reported before the supply reason as the more specific finding: the row has a
		// supply and lacks the exponent, a different gap with a different owner.
		if d.DecimalsUnresolved || d.MarketCapDecimalsMismatch {
			return RWAValuation{Status: RWAValuationDecimalsUnknown, PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis}
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

// rwaSumMarketCaps returns the exact sum of the published market caps and how many rows
// contributed. big.Rat over already-rounded 2-dp figures (ADR-0003) keeps each level the exact sum
// of the level below. Returns nil when nothing was published.
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

// rwaSumReferenceValues is [rwaSumMarketCaps] on the reference basis, with the same exact
// big.Rat arithmetic. It returns nil when nothing was reference-valued, since a zero would read
// as "the backing is worth nothing", the one reading certain to be wrong.
//
// Kept separate rather than parameterised so a caller cannot pass the wrong selector and quietly
// sum a market cap into the reference total.
func rwaSumReferenceValues(assets []RWAAsset) (*string, int) {
	sum := new(big.Rat)
	valued := 0
	for _, a := range assets {
		r := ratFromOptionalString(a.ReferenceValuation.ValueUSD)
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

// rwaBothBases totals, on each basis separately, the rows carrying both a market cap and a
// reference valuation. Restricting to the overlap matters: subtracting full sums compares two
// different populations, and the difference reads as a premium nobody measured.
func rwaBothBases(assets []RWAAsset) RWABothBases {
	market := new(big.Rat)
	reference := new(big.Rat)
	n := 0
	for _, a := range assets {
		mc := ratFromOptionalString(a.Valuation.MarketCapUSD)
		rv := ratFromOptionalString(a.ReferenceValuation.ValueUSD)
		if mc == nil || rv == nil {
			continue
		}
		market.Add(market, mc)
		reference.Add(reference, rv)
		n++
	}
	if n == 0 {
		return RWABothBases{}
	}
	m := market.FloatString(2)
	r := reference.FloatString(2)
	return RWABothBases{Assets: n, MarketCapUSD: &m, ReferenceValueUSD: &r}
}

// rwaReferenceSources lists the distinct oracles behind the published reference valuations,
// sorted. Only contributing rows count: naming a publisher whose figure was refused would
// attribute to it a number it is not in.
func rwaReferenceSources(assets []RWAAsset) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	for _, a := range assets {
		if a.ReferenceValuation.ValueUSD == nil || a.Reference == nil || a.Reference.Source == "" {
			continue
		}
		if _, dup := seen[a.Reference.Source]; dup {
			continue
		}
		seen[a.Reference.Source] = struct{}{}
		out = append(out, a.Reference.Source)
	}
	sort.Strings(out)
	return out
}

// rwaReferenceProvenances lists the distinct kinds of claim in the reference total, from
// contributing rows only: a row whose valuation was refused contributed nothing, so naming its
// provenance would describe the total by something not in it.
func rwaReferenceProvenances(assets []RWAAsset) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	for _, a := range assets {
		if a.ReferenceValuation.ValueUSD == nil || a.Reference == nil || a.Reference.Provenance == "" {
			continue
		}
		if _, dup := seen[a.Reference.Provenance]; dup {
			continue
		}
		seen[a.Reference.Provenance] = struct{}{}
		out = append(out, a.Reference.Provenance)
	}
	sort.Strings(out)
	return out
}

// rwaSummariseReference builds the reference-basis aggregate.
func rwaSummariseReference(assets []RWAAsset) RWAReferenceSummary {
	total, valued := rwaSumReferenceValues(assets)
	provenances := rwaReferenceProvenances(assets)
	return RWAReferenceSummary{
		Provenances:    provenances,
		ValueUSD:       total,
		AssetsValued:   valued,
		AssetsUnvalued: len(assets) - valued,
		LowerBound:     len(assets)-valued > 0,
		Sources:        rwaReferenceSources(assets),
		Basis:          rwaReferenceBasis(len(assets), valued, provenances),
	}
}

// rwaReferenceProvenanceProse describes what kind of claim is in the total, from the provenances
// actually present. They are claims about different subjects:
//
//   - an oracle NAV values the instrument; the step to the token rests on the issuer's own
//     domain-bound declaration that one token is one unit of it;
//   - a listing price values the token directly and says nothing about the backing;
//   - a fund NAV is a registered fund's own published daily value per share;
//   - a prospectus constant NAV is the issuer's own statement of a value its fund rules
//     prescribe, which nobody independent measured.
//
// The sentence is derived from the rows because a basis describing a provenance the total lacks
// is as wrong as one omitting a provenance it has.
func rwaReferenceProvenanceProse(provenances []string) string {
	present := make(map[string]bool, len(provenances))
	for _, p := range provenances {
		present[p] = true
	}
	var kinds []string
	for _, k := range rwaReferenceProvenanceProseOrder {
		if present[k.provenance] {
			kinds = append(kinds, k.prose)
		}
	}
	// Zero kinds means no contributing rows; the callers state the absence.
	if len(kinds) < 2 {
		return strings.Join(kinds, "")
	}
	return "The total MIXES " + rwaReferenceKindCount[len(kinds)] + " KINDS OF CLAIM and the per-row `provenance` field says which is which. " +
		strings.Join(kinds, "")
}

var rwaReferenceKindCount = map[int]string{2: "TWO", 3: "THREE", 4: "FOUR"}

// rwaReferenceProvenanceProseOrder is every provenance a verified row can carry into the summary
// total, in basis order. Curated rows never enter that total, so the curator provenance is absent.
var rwaReferenceProvenanceProseOrder = []struct{ provenance, prose string }{
	{RWAReferenceOracleNAV, "Oracle-priced rows (`provenance: oracle_instrument_nav`) are what an independent oracle says one unit " +
		"of the BACKING is worth, multiplied by the tokens in circulation, resting on the issuer's own domain-bound " +
		"declaration that one token is one unit of that instrument. On those rows nobody was seen paying it: it is an " +
		"assertion about the value of the backing, and no gate on this platform can corroborate an assertion. "},
	{RWAReferenceFundNAV, "Fund-NAV rows (`provenance: fund_nav`) are the net asset value per share a registered fund publishes daily, " +
		"relayed by a market-data vendor, for a token bound on the exact (code, issuer) as one share of that fund, " +
		"multiplied by the tokens in circulation. It is the fund's own statement of its value, published to the cent " +
		"(`decimals_published: 2`) and dated the business day it was struck; nobody independent measured it. No premium " +
		"is published against it, because half a cent of rounding is a material premium on a one-dollar share. "},
	{RWAReferenceListingPrice, "Listing-priced rows (`provenance: listing_platform_price`) are an independent listing platform's own USD price " +
		"for the TOKEN, bound to the exact address it was published against and never matched on a code. On a " +
		"contract-issued row it comes from the same source that corroborated the address at C2; on a classic row it " +
		"corroborated nothing and supplies only the price, because that row was admitted by its issuer's own " +
		"domain-bound declaration. It is a different and weaker claim than an oracle NAV: it says what the token " +
		"changes hands at on the venues that platform tracks, and asserts NOTHING about what stands behind it. No premium " +
		"is published against it, because a premium measured against an aggregate of the same markets our own price " +
		"samples would be the market compared with itself. Somebody WAS seen paying something like it — on venues this " +
		"index does not gate — which is precisely why it may not be added to market_cap_usd, whose whole meaning is a " +
		"price that survived those gates. "},
	{RWAReferenceProspectusCNAV, "Prospectus-priced rows (`provenance: prospectus_constant_nav`) are the net asset value a constant-NAV " +
		"share class's authorised prospectus fixes, as the issuer publishes it, bound on the exact (code, issuer) and " +
		"multiplied by the tokens in circulation. Nobody independent measured it: it is the issuer's own statement of a " +
		"value its fund rules prescribe, used only where no oracle, fund NAV or usable listing price exists for the row, " +
		"and served `stale: true` once the binding is past its review date. No premium is published against it, because " +
		"it is not an independent valuation of the instrument. "},
}

// rwaReferenceBasis states what the reference total measured and what it is not. `market_cap_usd`
// is a figure somebody was observed paying, behind three withholding gates; this one is an
// assertion about the value of the backing that no gate can corroborate. The difference is stated
// so a reader does not take a claim for a measurement.
func rwaReferenceBasis(total, valued int, provenances []string) string {
	var b strings.Builder
	b.WriteString("Sum of each member's circulating supply times an independent published price for it. ")
	b.WriteString("THIS IS NOT A MARKET CAPITALISATION: no row in it passed the substance, dust-liquidity and scam-issuer gates that stand behind market_cap_usd, and an asset that has never traded carries this figure at full size. ")
	b.WriteString(rwaReferenceProvenanceProse(provenances))
	b.WriteString("Published beside market_cap_usd and never added to it: the two are different bases over different rows, and market_cap_usd is exactly what it was before this figure existed. ")
	b.WriteString("Every contributing row names the kind of claim, the publisher, the key, the denominator and the vintage behind its number in `reference`, and the funnel's `valuation` arm counts every row that carries no figure under the reason that refused it. ")
	switch {
	case total == 0:
		b.WriteString("No asset currently meets the definition.")
	case valued == 0:
		b.WriteString("No member currently carries a reference valuation, so no total is published rather than a zero.")
	case valued == total:
		b.WriteString("Every asset in the set carries a reference valuation.")
	default:
		b.WriteString("Assets with no bound and current oracle feed, or no circulating-supply reading, contribute nothing and are counted separately, so the total is a LOWER BOUND on the reference-priced value of the set.")
	}
	return b.String()
}

// rwaIssuerCount is the single definition of "how many issuers" for the RWA set, shared by
// /v1/rwa/assets, /v1/rwa/history and /v1/rwa/premium. A contract-issued member has no issuer
// G-address, but its address is its whole identity, so each admitted contract counts as its own
// issuer rather than collapsing into a shared blank bucket.
func rwaIssuerCount(distinctClassicIssuers, contractCount int) int {
	return distinctClassicIssuers + contractCount
}

// rwaAssetIssuerCount applies rwaIssuerCount to the served /v1/rwa/assets
// rows: distinct non-empty Issuer strings, plus distinct ContractIDs.
func rwaAssetIssuerCount(assets []RWAAsset) int {
	issuers := map[string]struct{}{}
	contracts := map[string]struct{}{}
	for _, a := range assets {
		if a.Issuer != "" {
			issuers[a.Issuer] = struct{}{}
		}
		if a.ContractID != "" {
			contracts[a.ContractID] = struct{}{}
		}
	}
	return rwaIssuerCount(len(issuers), len(contracts))
}

func rwaSummarise(assets []RWAAsset, trunc rwaTruncation) RWASummary {
	total, valued := rwaSumMarketCaps(assets)
	var earliest uint32
	for _, a := range assets {
		if a.FirstSeenLedger != 0 && (earliest == 0 || a.FirstSeenLedger < earliest) {
			earliest = a.FirstSeenLedger
		}
	}
	referenced, compared := rwaReferenceCounts(assets)
	ref := rwaSummariseReference(assets)
	ref.LowerBound = ref.LowerBound || trunc.any()
	return RWASummary{
		Assets:                  len(assets),
		Issuers:                 rwaAssetIssuerCount(assets),
		MarketCapUSD:            total,
		AssetsValued:            valued,
		AssetsUnvalued:          len(assets) - valued,
		LowerBound:              len(assets)-valued > 0 || trunc.any(),
		EarliestFirstSeenLedger: earliest,
		AssetsWithReference:     referenced,
		AssetsCompared:          compared,
		ReferenceValuation:      ref,
		BothBases:               rwaBothBases(assets),
		Basis:                   rwaBasis(len(assets), valued, compared, trunc),
		Truncated:               trunc.any(),
	}
}

// rwaBasis states what the total measured, in prose, so a reader of the
// figure gets its scope without having to reconstruct it from counts.
func rwaBasis(total, valued, compared int, trunc rwaTruncation) string {
	var b strings.Builder
	b.WriteString("Sum of the published market caps of the assets meeting the four-requirement definition. ")
	b.WriteString("Market cap is circulating supply times the served USD price, both as /v1/assets serves them, ")
	b.WriteString("under the same substance, dust-liquidity and scam-issuer gates. ")
	switch {
	case total == 0:
		b.WriteString("No asset currently meets the definition.")
	case valued == total && trunc.any():
		b.WriteString("Every served asset publishes a valuation.")
	case valued == total:
		b.WriteString("Every asset in the set publishes a valuation.")
	default:
		b.WriteString("Assets whose valuation is withheld or unavailable contribute nothing and are counted separately, so the total is a LOWER BOUND on the value of the set.")
	}
	if compared > 0 {
		// The comparison rests on the issuer's own domain-bound declaration that the token anchors
		// to the instrument, which is not a verified denomination; the surface says so beside the
		// figure so a percentage does not imply certainty nobody established.
		b.WriteString(" Premium and discount compare the token's market price against an independent oracle's valuation of the instrument the issuer declares it anchors to; the correspondence between one token and one unit of that instrument is the issuer's own declaration, not an independent measurement.")
	}
	// Stated here too, to stop a reader of this total assuming it includes the reference-priced
	// figure.
	b.WriteString(" Reference-priced valuations are a separate basis and are NOT in this total: summary.reference_valuation carries them under their own name.")
	rwaWriteTruncation(&b, trunc)
	return b.String()
}

// rwaWriteTruncation names each cap that bound the rebuild and what it
// left unread, so a reader of a partial total knows what it excludes.
func rwaWriteTruncation(b *strings.Builder, t rwaTruncation) {
	if !t.any() {
		return
	}
	var caps []string
	if t.overIssuerCap > 0 {
		caps = append(caps, "the issuer cap turned away "+strconv.Itoa(t.overIssuerCap)+" qualifying asset(s)")
	}
	if t.overContractCap > 0 {
		caps = append(caps, "the contract scan cap left "+strconv.Itoa(t.overContractCap)+" recognised contract(s) unevaluated")
	}
	if t.issuerPagesFull > 0 {
		caps = append(caps, strconv.Itoa(t.issuerPagesFull)+" member issuer(s) have more classic assets than one listing page reads")
	}
	b.WriteString(" A rebuild cap bound this set (")
	b.WriteString(strings.Join(caps, "; "))
	b.WriteString("), so members may be missing and the total is a LOWER BOUND on the value of the set.")
}

// rwaUnclassified groups the assets admitted on the oracle basis, which
// declare no anchor class. Naming the group is honest; inventing a
// class for it would not be.
const rwaUnclassified = "unclassified"

// rwaUnclassifiedNote travels with the group so the word cannot be read
// as a gap in this index's data. It is a statement about what the
// ISSUERS declared, which is the only thing the classic arm is allowed
// to classify on.
const rwaUnclassifiedNote = "Admitted because an independent oracle prices the instrument, not because an issuer declared a class. " +
	"No class is published for these rows because none was declared in the SEP-1 vocabulary — one issuer's own attestation " +
	"says `other`, another declares no anchor type at all. A class here would contradict the first issuer on their own asset " +
	"and invent one for the second, so the group is named rather than filled."

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
		refTotal, refValued := rwaSumReferenceValues(group)
		row := RWAGroupTotal{
			Class:                   c,
			Assets:                  len(group),
			MarketCapUSD:            total,
			AssetsUnvalued:          len(group) - valued,
			ReferenceValueUSD:       refTotal,
			AssetsReferenceUnvalued: len(group) - refValued,
		}
		if c == rwaUnclassified {
			row.Note = rwaUnclassifiedNote
		}
		out = append(out, row)
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
		// A contract row's empty Issuer is not a key: grouping on it
		// merged every contract-issued asset into one blank issuer.
		if a.Issuer == "" {
			continue
		}
		if _, ok := byIssuer[a.Issuer]; !ok {
			order = append(order, a.Issuer)
		}
		byIssuer[a.Issuer] = append(byIssuer[a.Issuer], a)
	}
	out := make([]RWAIssuerTotal, 0, len(order))
	for _, issuer := range order {
		group := byIssuer[issuer]
		total, valued := rwaSumMarketCaps(group)
		refTotal, refValued := rwaSumReferenceValues(group)
		out = append(out, RWAIssuerTotal{
			Issuer:                  issuer,
			Name:                    group[0].IssuerDirectoryName,
			HomeDomain:              group[0].HomeDomain,
			Assets:                  len(group),
			MarketCapUSD:            total,
			AssetsUnvalued:          len(group) - valued,
			ReferenceValueUSD:       refTotal,
			AssetsReferenceUnvalued: len(group) - refValued,
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
