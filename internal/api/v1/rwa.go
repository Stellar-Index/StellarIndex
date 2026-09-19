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
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
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
//
// WHERE THE CANDIDATE POOL COMES FROM, because it is not obvious from
// here and it silently bounded this surface for the whole of its life:
// the implementation reads `issuers WHERE sep1_payload IS NOT NULL`, and
// `issuers` rows are created ONLY by the classic-asset registry writer.
// Until migration 0158 that writer ran only on a TRADE, so an issuer
// whose assets are held but never traded on the SDEX never became a
// candidate at all — not refused by a requirement, absent from the
// `refused` counts, invisible. 61% of the classic-asset population was
// in that state (512,496 assets with a trustline against 199,793
// registered, measured 2026-09-10), Franklin Templeton's BENJI included.
// The registry now also registers from trustline holdings, so the pool
// is the held population — once `asset-registry-backfill`,
// `issuer-enrich` and `sep1-refresh` have each reached a given issuer.
type Sep1BoundCurrencyReader interface {
	BoundSep1Currencies(
		ctx context.Context, keep timescale.Sep1CurrencyFilter,
	) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error)
}

// Compile-time proof that the production reader still satisfies the seam —
// the same guard [Sep1FetchStateReader] and [preciseSupplyReader] carry,
// for the same reason: an optional seam that stops matching is not a build
// failure, it is a silent opt-out, and here the opt-out is the whole RWA
// classic membership going empty with `available: false` and nothing red
// anywhere. Only the bare store is listed because that is what the binary
// wires (cmd/stellarindex-api/main.go, `Sep1Cache: store`); a caching
// wrapper that one day stands in front of this seam must be added here the
// same day.
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
	// Imbalance names WHAT did not close, and is empty exactly when
	// Balanced is true — the two are one statement in two forms and
	// cannot disagree.
	//
	// Every reason in it was already being computed: each census
	// publishes a Check() that returns the invariant it broke, and each
	// adjacent stage pair reports the arithmetic that failed. All of it
	// was reduced to a boolean at this boundary and the sentences went
	// to a log, so a reader was told the accounting does not close and
	// given no way to find out what did not close. Semicolon-separated
	// when more than one check failed, because they are independent and
	// the first is not necessarily the cause of the rest.
	Imbalance string `json:"imbalance,omitempty"`
	// Basis is a one-line statement of what the funnel measured.
	Basis string `json:"basis"`
	// ListingDirectory is the EVIDENCE behind the `listing` arm's
	// verdict: what the independent listing directory held at the
	// moment this set was built.
	//
	// Omitted when nothing was observed — no listing reader wired, or a
	// read that did not answer. Its absence is therefore a statement in
	// its own right and never a default: an arm reporting refusals with
	// no evidence block beside them is an arm whose source could not be
	// read at all.
	ListingDirectory *RWAListingDirectory `json:"listing_directory,omitempty"`
}

// RWAMembershipSet dates the SET this response describes: when the
// rebuild that produced it finished, and whether the copy being served
// is past its own lifetime.
//
// It exists because the response could not say how old the thing it was
// describing was, and on 2026-09-15 that cost most of an afternoon. The
// envelope's `as_of` is the RESPONSE's instant and moves sub-second
// between requests; read as the set's build time it says the set is
// refreshing continuously, which is the opposite of what was happening.
// Every field here is named to make that substitution impossible:
// `built_at`, not `as_of`, because a set is BUILT and a response is AS
// OF — two different events that were being read as one.
//
// # The state this is loudest about
//
// The cache deliberately serves a lapsed set while a detached rebuild
// runs behind it, and that is correct: the inputs move on daily
// cadences, so a set a few minutes past its lifetime is the same set,
// and making a request wait for an eleven-second rescan is what the
// detachment exists to prevent. What was missing is that a reader could
// not tell it was happening.
//
//	Stale false                         the set is inside its lifetime
//	Stale true, RebuildFailedAt nil     lapsed, and a rebuild is running
//	                                    or about to — expected, transient
//	Stale true, RebuildFailedAt set     lapsed, and the last attempt to
//	                                    replace it FAILED. The set being
//	                                    served is the last good one and
//	                                    nothing is replacing it. This is
//	                                    the state to act on.
type RWAMembershipSet struct {
	// BuiltAt is when the rebuild that produced this set finished —
	// NOT when the response was rendered, and not when any source was
	// last written. Compare it against a source's own clock to tell a
	// set that predates a sync from a set that disagrees with one.
	//
	// It is deliberately the same instant the lifetime clock starts
	// from, so `stale` beside it is never a statement about a different
	// moment.
	BuiltAt WireTime `json:"built_at"`
	// Stale reports that the set has passed its lifetime and a rebuild
	// is owed. Served regardless, which is the design: an expired set
	// is the same set until the inputs move, and a request that waited
	// for the rescan would pay eleven seconds for an answer that has
	// not changed.
	Stale bool `json:"stale"`
	// RebuildFailedAt is when the most recent rebuild attempt failed,
	// present only while that is still the latest thing to have
	// happened — a successful rebuild clears it.
	//
	// Beside Stale it separates a set that is about to be replaced from
	// one that nothing is replacing. Without it both render as `stale:
	// true`, and the second is the only one anybody needs to act on.
	RebuildFailedAt *WireTime `json:"rebuild_failed_at,omitempty"`
}

// RWAListingDirectory is what the independent listing directory looked
// like at the moment the served set was built.
//
// It exists because the funnel published a VERDICT and none of the
// evidence for it, and the verdict alone cannot be acted on. A `listing`
// arm reporting nothing corroborated is produced by three different
// states of the world — a directory nobody has ever synced, a sync that
// stopped days ago, and a perfectly healthy directory that this set
// simply predates — and they call for three different responses, from
// "run the sync" through "go and fix it" to "do nothing at all". Told
// apart, before this block, only by somebody with a database prompt.
//
// Read it as a decision table. Entries counts the FRESH rows only —
// `Entries = Contracts + Classic`, with Stale counted beside them and
// never inside them — so the two zero cases are told apart by Stale,
// which is the single number that says whether anything was ever there:
//
//	Entries == 0 && Stale == 0     never synced: the table is empty
//	Entries == 0 && Stale > 0      every row aged out: the sync STOPPED
//	Entries > 0 && Contracts == 0  healthy, but it names no Stellar
//	                               CONTRACT address — nothing this arm
//	                               can consult, and nothing to fix
//	Entries > 0 && Contracts > 0   healthy and populated
//
// Classic rows are not published separately because they are the
// remainder: `Classic = Entries - Contracts`, by the invariant above.
//
// Then, whatever the counts say, compare ObservedAt against the sync's
// own clock. A sync that completed AFTER this instant means the set in
// hand predates the rows it would have used and the next rebuild will
// carry them — nothing is broken and nobody needs to act. That is the
// comparison this block exists for, and the one no count can answer:
// the evidence and the verdict in a response always come from the same
// read, so they can never contradict each other, and a reader with only
// the verdict has no way to place it in time.
type RWAListingDirectory struct {
	// ObservedAt is when this index read the directory — NOT when the
	// directory was itself synced, and not when the response was
	// rendered. It is the field the rest of the block is useless
	// without.
	//
	// Every count below can be recovered afterwards by reading the
	// table again. This instant cannot be recovered by anyone, at any
	// point, once the moment has passed — so it is the one fact that
	// has to be published rather than left to be looked up, and the
	// only one that lets a reader place a closed arm and a healthy
	// directory in time relative to each other.
	ObservedAt WireTime `json:"observed_at"`
	// Entries is every FRESH row the directory held, of either address
	// form — the recognised population, before the contract/classic
	// split. Rows past the recognition bound are not in it; they are in
	// Stale.
	Entries int `json:"entries"`
	// Contracts is how many of those rows name a Stellar CONTRACT
	// address, which is the only form C2's second arm can consult. A
	// directory full of classic rows and no contract rows closes the
	// arm while being perfectly healthy, and this is the count that
	// says so.
	Contracts int `json:"contracts"`
	// Stale is how many rows are PRESENT in the directory but past the
	// recognition bound — counted beside Entries, never inside it. It
	// is the visible form of the fail-closed guarantee, and the one
	// number that separates a sync that has stopped from one that has
	// never run: both leave Entries at zero, and only a dead sync
	// leaves rows behind to go stale.
	Stale int `json:"stale"`
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
	// token's issuer can (a domain that serves no attestation at all, a
	// toml that declares nothing, or declares somebody else),
	// `definition` for a drop that is the membership
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
	rwaDropDomainServedNothing  = "domain_served_no_sep1_attestation"
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
	// rwaArmListing is C2's SECOND arm: the in-repo curated contract
	// bindings, narrowed to the ones an independent listing directory
	// corroborates. A separate arm rather than extra stages on the
	// contract one because it narrows a different population from a
	// different root — the contract arm starts at a third party's
	// directory, this one starts at a table in this repository and
	// looks for somebody else to agree with it.
	rwaArmListing = "listing"
	// rwaArmValuation continues past the served set rather than
	// narrowing a population toward it: it accounts for which served
	// rows carry a REFERENCE-priced valuation and, for every row that
	// does not, the reason that refused it. It is the same accounting
	// the membership arms make, applied to the one figure whose
	// coverage is otherwise visible only by reading every row.
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
	// ContractAnchorClasses is the vocabulary a CURATED CONTRACT BINDING
	// may use, which is a superset of the one above. The two arms do not
	// answer with the same words, and the difference is not an oversight.
	//
	// The classic arm READS an issuer's free-text anchor_asset_type, so
	// it can only accept terms SEP-1 defines: anything else is an
	// invented spelling, and the served population carries dozens
	// (`equity`, `etf`, `metal`, `rwa`, `sovereign`). The contract arm
	// reads no declaration at all — a binding's class is this index's own
	// statement, made in code from a primary source and reviewed as a
	// change — so it may use a term SEP-1 lacks, and `fund` is there
	// because SEP-1 has no word for a share in a pooled vehicle whose
	// exposure is the vehicle's objective rather than any asset type it
	// holds.
	//
	// A wider vocabulary widens what a binding may SAY. It does not widen
	// what is admitted: the identity, recognition and scam requirements
	// are untouched, and a contract's class is never read until its
	// address has already been named by two independent parties or by the
	// curated directory.
	ContractAnchorClasses []string `json:"contract_anchor_classes"`
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
	// ContractRecognitionSources is the closed vocabulary of ways C2
	// can be satisfied, in the order of their strength. Served for the
	// same reason every other rule on this block is: a consumer reads
	// which routes exist from the response rather than inferring them
	// from whichever rows qualified today, and can tell that a row
	// admitted on a corroborating pair is not the same claim as one
	// admitted on a directory attestation.
	ContractRecognitionSources []string `json:"contract_recognition_sources"`
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
	// Served in full, and an address appearing here is NOT a statement
	// that the asset is in the set. The curated binding answers C4 —
	// which instrument an address holds — while C2 still requires the
	// curated directory to name that same address, and the candidate
	// scan upstream only ever enumerates addresses the directory named.
	// So this list is the surface's verified-identity register: every
	// address on it has cleared the evidence bar, some of them while
	// still being refused membership for want of independent
	// recognition. A consumer reads the rule from this list rather than
	// inferring it from whichever rows appear today, and compares it
	// against the admitted rows to see which identities are known but
	// unvouched-for.
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
	// Truncated reports that a cap bound the rebuild, so the set is
	// known to be incomplete.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAReferenceSummary aggregates the reference-priced basis.
//
// Every field has a market-cap counterpart in [RWASummary] and behaves
// the same way, because the discipline that makes the market-cap total
// readable is not specific to market prices: the total is ABSENT rather
// than "0.00" when nothing is reference-valued, the contributing and
// non-contributing rows are counted separately, and LowerBound says
// whether the total is less than the value of the set.
//
// What is NOT shared is the meaning. Nothing summed here was observed
// being paid.
type RWAReferenceSummary struct {
	// ValueUSD is the exact sum of the published per-asset
	// `reference_valuation.value_usd` strings — add up what you can see
	// and you land on this number. ABSENT, never "0.00", when no member
	// is reference-valued.
	ValueUSD *string `json:"value_usd,omitempty"`
	// AssetsValued and AssetsUnvalued split the set by whether it
	// contributed. Counted independently of the market-cap split: the
	// two bases admit different rows, which is the whole reason both
	// totals exist.
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// LowerBound is true whenever any member carries no reference
	// valuation, i.e. whenever this total is less than the
	// reference-priced value of the set.
	LowerBound bool `json:"lower_bound"`
	// Sources names the distinct PUBLISHERS whose published values are
	// in the total, sorted. Not "oracles": the total now mixes two
	// provenances, and a field that called a listing platform an oracle
	// would misdescribe the weaker half of its own figure.
	//
	// A dollar figure that cannot be traced back to a publisher is
	// worse than an absent one on this surface, and the per-row
	// `reference` blocks carry the full provenance — kind, feed, quote
	// and vintage — that this list summarises. Which kind each
	// publisher contributed is on the row, never inferred from the
	// name.
	Sources []string `json:"sources,omitempty"`
	// Provenances names the distinct `reference.provenance` values in
	// the total, sorted. Served so the mixture is visible at the
	// summary level: a reader of one number must be able to see that it
	// is not all one kind of claim without walking every row.
	Provenances []string `json:"provenances,omitempty"`
	// Basis states in prose what was measured and, more importantly,
	// what it is not.
	Basis string `json:"basis"`
}

// RWABothBases totals the members that carry a market cap AND a
// reference valuation.
//
// It exists to stop one specific misreading. `market_cap_usd` and
// `reference_valuation.value_usd` are sums over DIFFERENT rows — an
// asset that never trades has the second and not the first — so their
// difference is not a premium, a discount, or anything else. These two
// totals are taken over the same rows, so their difference is the
// aggregate gap between what the market pays for that overlap and what
// the reference says it is worth. Per asset, the same gap in
// percentage terms is `premium.pct`.
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
	// RWAValuationDecimalsUnknown — a price and a supply both exist, and
	// the token's own declared SCALE does not, so there is no exponent
	// to divide the supply by.
	//
	// Contract rows only. A classic asset is 7 decimals by protocol and
	// a SAC inherits that; a SEP-41 token declares its own, and when
	// that declaration cannot be read the catalogue's default of 7 is a
	// convention rather than a reading. Multiplying by it would publish
	// a figure wrong by a factor of ten to the something — one hundredth
	// for the 5-decimal funds in the measured population, eleven orders
	// of magnitude the other way for the 18-decimal one.
	//
	// Refused rather than defaulted because the error is silent and
	// unbounded, and because this is the one surface where the exponent
	// IS the number. The circulating supply is still served beside it:
	// that is a raw chain fact and needs no scale to be true.
	RWAValuationDecimalsUnknown = "decimals_unavailable"
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
	// Recognition names WHICH independent party's naming satisfied the
	// recognition requirement — C2 on a contract-issued row, R3 on a
	// classic row. Present on every served row. On classic rows it is
	// `curated_account_directory` when the directory lists this issuer
	// account itself, or `curated_account_directory_via_domain_sibling`
	// when it lists another account that the same issuer-bound SEP-1
	// binds on the same domain (the directory never looked at this
	// account; the recognised entity named it from its own domain).
	//
	// It is on the wire because neither arm's two routes to
	// recognition are the same strength of evidence. A row reading
	// `curated_account_directory` was vouched for by an address-level
	// identity directory that carries scam flags and admits on its own.
	// A row reading `independent_listing_corroborating_curated_binding`
	// required TWO sources that do not read each other — a listing
	// platform naming the address, and an in-repo curated binding
	// naming the same address — because neither is sufficient alone.
	// A consumer that wants only directory-attested rows can filter on
	// this field rather than having to reconstruct the rule.
	Recognition string `json:"recognition,omitempty"`
	// DecimalsUnresolved is true when Decimals is the hardcoded default
	// rather than a reading from the token's own on-chain metadata.
	// INTERNAL — never serialised. It exists to stop a figure being
	// published, and both valuation bases consult it before they
	// divide by 10^Decimals.
	DecimalsUnresolved bool `json:"-"`
	// AnchorClass is the closed-vocabulary class, present only under
	// the declaration basis.
	AnchorClass string `json:"anchor_class,omitempty"`
	// AnchorAsset is the off-chain instrument the issuer declared this
	// token anchors to, verbatim.
	AnchorAsset string `json:"anchor_asset,omitempty"`
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
	// SupplyBasis names the reading that produced CirculatingSupply —
	// the ADR-0011 vocabulary, carried verbatim from the listing row.
	// Absent when no arm answered and no supply is served.
	//
	// It is load-bearing on THIS surface in a way it is not on a plain
	// listing. Every reference_valuation here is circulating_supply
	// multiplied by an oracle price, so the completeness of the supply
	// is half of every total the page publishes; a consumer reconciling
	// against an issuer's own figure needs to know whether it is
	// comparing two four-domain totals or a total against a floor.
	SupplyBasis string `json:"supply_basis,omitempty"`
	// CirculatingSupplyLowerBound is true when CirculatingSupply is a
	// provable floor rather than a complete reading — the per-row
	// sibling of the lower_bound this surface's totals already carry.
	// DERIVED from SupplyBasis ([supply.Basis.LowerBound]) rather than
	// carried alongside it, so the two can never disagree.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`
	// Decimals is the on-chain smallest-unit scale: 7 for every classic
	// asset, and whatever a SEP-41 contract declares for a
	// contract-issued one. Served so both valuations on this row can be
	// re-derived by hand — circulating_supply / 10^decimals is the
	// whole-token float that each price multiplies — rather than
	// leaving a reader to assume a scale that is only sometimes 7.
	Decimals int `json:"decimals"`
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
	// ReferenceValueUSD is the same group on the reference basis, and
	// AssetsReferenceUnvalued is that basis's own unvalued count. Both
	// bases are broken down because the two admit different rows, and a
	// breakdown carrying only one of them would leave a reader unable
	// to see which class the difference between the totals came from.
	ReferenceValueUSD       *string `json:"reference_value_usd,omitempty"`
	AssetsReferenceUnvalued int     `json:"assets_reference_unvalued"`
	// Note explains a class name that does not speak for itself.
	// Present only on `unclassified`, and it is there because the word
	// reads as a gap in OUR data when it is a statement about the
	// issuer's: these rows were admitted because an independent oracle
	// prices the instrument, and nobody declared a class for them in the
	// SEP-1 vocabulary. Franklin Templeton's own attestation for BENJI
	// says `anchor_asset_type = "other"`; Ondo's for USDY declares none
	// at all. Publishing a class here would contradict one issuer on
	// their own asset and invent one for the other.
	//
	// A reader comparing this breakdown with a third party's — which
	// classifies everything, because it is not reading the issuers —
	// would otherwise take the largest group on the page for missing
	// work. Absent on every group that named itself.
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
	basis       string
	anchorClass string
	recognition string
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
	// readFailed records that an arm which IS wired did not answer —
	// the attestation scan, the classic arm's directory lookup, or the
	// contract arm's directory scan returned an error.
	//
	// It exists because `available` cannot say this. That field is
	// false for an unwired reader AND for a read that failed, and the
	// two need opposite handling at the cache: an unwired arm is a
	// permanent, honest property of the deployment and its rebuild is
	// worth caching, while a failed read is an outage whose empty arm
	// must never be written over a good set. Conflating them is how a
	// half-empty membership set came to be served, self-certified
	// fresh, for a full lifetime — see [Server.refreshRWAMembership].
	readFailed bool

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
	// listingCensus accounts for C2's SECOND arm — the curated bindings
	// an independent listing directory was asked to corroborate.
	listingCensus rwaListingCensus
	// contractRefusals and listingRefusals are the two contract-side
	// tallies kept apart. Both arms run the same rwa.QualifyContract and
	// can produce the same reason string, so a single tally would let
	// the funnel attribute one arm's refusal to the other's narrowing.
	// Their union is already folded into refusals above, which is what
	// `refused[]` publishes.
	contractRefusals map[string]int
	listingRefusals  map[string]int
	// listingNotObserved is the same count for C2's second arm, kept
	// apart for the reason the refusal tallies are: each arm closes its
	// own arithmetic, and one combined count would leave both unable to.
	listingNotObserved int
	// contractsNotObserved counts admitted contracts with no catalogue
	// row. Recorded on the membership rather than returned beside the
	// rows because the funnel is built from this value, and a count that
	// travelled separately could be dropped on a path that forgot it.
	contractsNotObserved int
	// unreached names the recognised issuing entities this index holds
	// no token for. It admits nothing; see RWAUnreachedEntity.
	unreached []rwaUnreachedEntity

	// builtAt is when the rebuild that produced this set finished. Set
	// once, by that rebuild, and never touched again — so a set carries
	// its own age wherever it travels and a reader of it can never be
	// looking at one instant while reasoning about another.
	builtAt time.Time
	// servedStale and rebuildFailedAt are the only two fields here a
	// rebuild does NOT set. They are properties of this SERVE rather
	// than of the set, stamped onto a copy on the way out by
	// [Server.stampRWAServeState] — a set that recorded its own
	// staleness at build time would say `false` forever, since it is
	// never stale at the instant it is built.
	servedStale     bool
	rebuildFailedAt time.Time
}

// rwaCandidateFilter keeps only the bound SEP-1 entries that could
// possibly satisfy requirement 4, so the scan never materialises the
// tens of thousands of bound entries that declare an NFT, a crypto
// token or nothing at all.
//
// Every asset-side input a requirement-4 arm reads goes through: the
// anchor asset as well as the code and the type. Passing fewer is how
// the ISIN arm was unreachable for the Franklin share classes (type
// `other`, ISIN in anchor_asset) — the entries were counted as
// EntriesFiltered before [Server.admitClassicCandidates] ever saw them.
func rwaCandidateFilter(c timescale.Sep1BoundCurrency) bool {
	return rwa.CouldQualify(c.Code, c.AnchorAssetType, c.AnchorAsset)
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
	c := s.buildRWAContractMembership(ctx)
	out.contracts = c.members
	out.unreached = c.unreached
	out.contractCensus = c.census
	out.listingCensus = c.listingCensus
	out.contractRefusals = c.refusals
	out.listingRefusals = c.listingRefusals
	// A WIRED contract reader whose scan errored leaves
	// census.available false in exactly the way an unwired one does,
	// and only this layer holds the wiring, so only this layer can tell
	// the two apart. The distinction decides whether the rebuild may be
	// cached over the last good set — see [rwaMembership.readFailed].
	//
	// The LISTING arm is deliberately not folded in here: it fails
	// CLOSED inside the measured branch, dropping every binding under
	// `independent_listing_unavailable`, so its outage is already a
	// stated refusal on the wire rather than a silent absence.
	if s.rwaContracts != nil && !c.census.available {
		out.readFailed = true
	}
	// One `refused[]` for the whole surface. The per-arm tallies stay
	// on the membership for the funnel, which has to attribute a
	// refusal to the narrowing it belongs to; a consumer counting how
	// many candidates a requirement turned away does not.
	for _, tally := range []map[string]int{c.refusals, c.listingRefusals} {
		for reason, n := range tally {
			out.refusals[reason] += n
		}
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
		out.readFailed = true
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
			out.readFailed = true
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
			recognition: v.Recognition,
			dirName:     e.Name,
			dirTags:     e.Tags,
		})
	}
}

// rwaMembershipRetryGap rate-limits rebuild ATTEMPTS, not rebuilds. A
// rebuild that fails leaves the entry stale, so without a gap the next
// request would start another one immediately and a broken read would
// become a scan storm against the same tables that are already
// struggling. Same role as sep1ImagesRetryGap beside it.
const rwaMembershipRetryGap = 60 * time.Second

// rwaMembershipBudget is the DETACHED rebuild's own deadline. It is not
// a request budget: the whole point of detaching is that no request is
// waiting on it. It has to exceed the measured build cost with room to
// spare — the production build was ~11.5 s on 2026-09-15 — or the
// refresh that was supposed to keep the entry warm is killed halfway
// and the entry never warms at all.
const rwaMembershipBudget = 2 * time.Minute

// cachedRWAMembership returns the membership set for a request.
//
// It NEVER blocks on a rebuild once a set has ever been built. A stale
// set is served as-is and a detached rebuild is kicked behind it,
// because the inputs move on daily cadences: a set a few minutes past
// its TTL is the same set, and waiting for the rescan is what cost the
// request the wall-clock time.
//
// That wait was the whole of this surface's latency. Measured on r1
// (2026-09-15) the route was perfectly bimodal — 39 of 43 requests
// under 1 s, the other 4 over 10 s — because the rebuild is an
// indexed scan over every issuer-bound SEP-1 payload (1.18M currency
// entries) plus the curated-directory walk, and whichever request
// happened to find the ten-minute entry expired paid all ~11.5 s of it
// inline. With no more than one page load per TTL window, that is
// roughly one visitor in ten meeting a twelve-second page. It is the
// same defect, with the same fix, as the SEP-1 logo map in
// [Server.readSep1Images].
//
// The ONE case that still waits is a cache that has never been filled.
// An empty set there is not a stale answer, it is the false statement
// that no real-world asset exists on Stellar — so a cold process waits
// for its first build rather than publishing that. [Server.PrewarmRWA]
// is what makes sure the waiter is the prewarm goroutine and not a
// visitor.
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

// rwaMembershipSetOf dates the served set, or returns nil when there is
// no set to date.
//
// Gated on the BUILD INSTANT, for the reason the listing evidence is
// gated on its read instant: a zero-valued set and a real set that
// happens to be empty render identically in every other field, and only
// one of them is an answer. Before the first build has ever completed
// there is nothing to date, and saying so by omission is more honest
// than publishing the Unix epoch.
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

// stampRWAServeState records, on a COPY of the set, the two facts that
// belong to this serve rather than to the set: whether the copy has
// outlived its lifetime, and whether the rebuild that should have
// replaced it failed.
//
// They are stamped here, on the way out, rather than carried on the
// cached value, because both are answers to "as of now" and the cached
// value is by definition not from now. A set that recorded its own
// staleness when it was built would report `false` for the whole of its
// life, which is exactly backwards.
//
// The copy matters: the value returned by [Server.readRWAMembership]
// points AT the cache. Writing through it would make one request's
// view of the clock permanent for every later reader.
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

// readRWAMembership serves what the cache holds and kicks a detached
// rebuild when the entry is stale (or absent) and no attempt is already
// running or too recent. Returns the served set — nil only before the
// first successful build, possibly stale otherwise, both of which are
// fine — and the in-flight rebuild's completion channel (nil when no
// rebuild is running).
//
// The channel exists for [Server.PrewarmRWA] and for the cold-cache arm
// of [Server.cachedRWAMembership]. A warm-or-stale request path takes
// the set and goes.
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

// refreshRWAMembership rebuilds the set on a DETACHED context and swaps
// it in. On a failed rebuild the previous set is left exactly as it
// was — a directory or SEP-1 read that fails must not empty a surface
// whose inputs move on a daily cadence.
func (s *Server) refreshRWAMembership(done chan struct{}) {
	// Deferred so a panic in the scan cannot leave the single-flight
	// marker set — which would freeze the cache for the life of the
	// process, since a non-nil flight is never replaced.
	defer s.endRWAMembershipFlight(done)
	defer worker.Recover(s.logger, "api-rwa-membership-refresh")

	ctx, cancel := context.WithTimeout(context.Background(), rwaMembershipBudget)
	defer cancel()

	built := s.buildRWAMembership(ctx)
	// ONE instant for the set's published age and for the lifetime
	// clock, so `stale` is never a statement about a different moment
	// from the `built_at` it is served beside.
	at := time.Now().UTC()
	built.builtAt = at

	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	// Every WIRED arm must have answered. An arm that is not wired is
	// no obstacle — requiring it would mean a deployment with only one
	// reader rebuilt on every request and never cached — but an arm
	// that IS wired and did not answer makes this rebuild a PARTIAL
	// measurement, and a partial measurement may not be published as
	// the set.
	//
	// It used to be enough for EITHER arm to answer, which sounds like
	// the same rule and is not: a failed classic scan beside a healthy
	// contract scan wrote a set with no classic members over the last
	// good one, re-dated it to now and cleared the failure stamp below,
	// so the surface served a half-empty set reporting `stale: false`
	// and no `rebuild_failed_at` for the whole ten-minute lifetime.
	// Membership is what this index CALLS a real-world asset, so that
	// is published coverage which is wrong AND self-certified fresh —
	// the one combination a reader cannot defend against.
	//
	// The good half of a partial rebuild is discarded deliberately. The
	// alternative is merging one arm of a new set into the other arm of
	// an older one, which would publish a single `built_at` over two
	// different moments; and the last good set is a FULL measurement
	// whose inputs move on a daily cadence, so keeping it and saying it
	// is stale is both more complete and more honest than half of a
	// fresh one. A cold cache with an arm down therefore stays cold and
	// the handler states the absence — the posture rwaBasisUnavailable
	// already describes: no set is published rather than an unverified
	// one.
	if built.readFailed || (!built.available && !built.contractCensus.available) {
		// Not an error path for the caller: the previous set keeps being
		// served, and rwaAttemptAt (already advanced) is what stops this
		// becoming a retry storm.
		//
		// Recorded rather than only logged. From outside, a lapsed set
		// that a rebuild is about to replace and a lapsed set that
		// nothing is replacing are identical — and the second is the
		// only one anybody needs to act on.
		s.rwaFailedAt = at
		// Which arm went down is named here because it is the one thing
		// the wire deliberately does not carry: the response says a
		// rebuild failed, the log says what to go and look at.
		s.logger.Warn("rwa membership rebuild failed (serving the last good set)",
			"classic_answered", built.available,
			"contracts_answered", built.contractCensus.available)
		return
	}
	// Cleared on success: the field answers "is the latest thing to
	// have happened a failure", not "has one ever happened".
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

// PrewarmRWA fills the three caches the /rwa page reads out of band, so
// no request ever meets a cold one: the membership set shared by all
// three RWA routes, then the value and premium series behind the page's
// two history panels.
//
// The detached rebuild above already means a STALE membership entry
// costs a request nothing. This is what stops the entry being stale in
// the first place, and — the case that still blocks — what makes the
// waiter for the very first build the prewarm goroutine rather than the
// first visitor after a deploy.
//
// The two series are warmed for the reason the page is measured by its
// slowest panel rather than by its first: they are their own
// ten-minute caches over their own scans, and a visitor who found the
// membership warm could still sit behind one of them. They are warmed
// AFTER the membership because each builds on it.
//
// # No cache key, so nothing to drift
//
// The repo's prewarm rule is that a prewarm must call the cached reader
// with byte-identical arguments to the handler, or it warms a different
// slot and does nothing (three production bugs: Order, Sources, Limit).
// It is satisfied here structurally rather than by matching arguments:
// the membership set is ONE process-wide entry with no key at all, and
// this calls [Server.readRWAMembership] — the exact function
// [Server.cachedRWAMembership] calls — so there is no second slot for
// it to land in. The only thing it adds is the wait.
//
// Best-effort, like every other prewarm: no reader wired, or a failed
// scan, each leave the cache exactly as it was.
func (s *Server) PrewarmRWA(ctx context.Context) {
	// The membership set is shared by all three RWA routes and depends
	// on neither history reader, so it warms on its own terms.
	if _, flight := s.readRWAMembership(); flight != nil { //nolint:contextcheck // same detachment as the request path; ctx is honoured by the wait below, not by the scan.
		// Waiting is what makes this a prewarm rather than a nudge: the
		// caller's next pass should find the entry warm, not find its
		// own kick still running. Shutdown returns at once and still
		// lets the detached rebuild finish and record what it learned.
		select {
		case <-flight:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return
	}
	// The two series below are warmed behind the SAME guards their
	// handlers apply, and in the same order. A prewarm that started a
	// build its handler would have refused to start is reading through
	// readers the handler proved it may not — which is the prewarm
	// drift class, in its most expensive form.
	//
	// Both of these BLOCK on their own build, unlike the membership
	// above. That is what makes warming them worth the goroutine's
	// time: whoever pays the build should be this goroutine, not a
	// reader of the page.
	if s.assetsReader == nil || s.oracleHistory == nil {
		return
	}
	s.cachedRWAValueHistory(ctx)
	if ctx.Err() != nil || s.marketHistory == nil {
		return
	}
	s.cachedRWAPremiumHistory(ctx)
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
	contractRows, _, contractErr := s.rwaContractListingRows(r.Context(), m.contracts)
	if contractErr != nil {
		if clientAborted(r, contractErr) {
			return
		}
		s.logger.Error("rwa contract listing read failed", "err", contractErr)
		contractRows = map[string]AssetDetail{}
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
	// The listing rows that priced the admitted contracts, indexed from
	// the MEMBERSHIP rather than from a second read: the row that
	// prices an asset must be the same row that recognised it, or the
	// surface could publish a price from a snapshot in which the
	// address was not named.
	listingRefs := rwaListingReferencesOf(m.contracts)
	// The classic arm's own listing rows, from the SAME cached read
	// /v1/assets uses, resolved by the SAME rule ([listingEntryIn]):
	// the `CODE-GISSUER` id first, then the Stellar Asset Contract
	// address derived from it, because the directory publishes each
	// asset under ONE of those forms with no pattern. The contract map
	// above cannot answer for a classic member, because the read behind
	// it returns C-strkeys the membership named, not the derived ones.
	//
	// A snapshot that is not available yields a nil map, and a nil map
	// answers every lookup with a zero entry — which is the "no listing
	// named this address" refusal, unchanged. That is the right reading
	// only because the refusal it produces is the one the row already
	// had: this read can add a reference and can never remove one.
	classicListingRefs := s.assetListingSnapshot(r.Context()).byAddress
	now := time.Now()
	for i := range view.Assets {
		rwaApplyReference(&view.Assets[i], refs, listingRefs, classicListingRefs, now)
	}
	view.Summary = rwaSummarise(view.Assets, m.truncated)
	view.ByClass = rwaByClass(view.Assets)
	view.ByIssuer = rwaByIssuer(view.Assets)
	view.Funnel = rwaFunnelOf(m, join, len(classicAssets), dirCounts.served, listingCounts.served, view.Assets)
	// The curated arm runs LAST, over the finished verified view, so it
	// can say which of its rows the verified set already carries and can
	// never feed a figure back into the totals above.
	s.attachRWACurated(r, &view, now)
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
		// ADDITIVE, and additive only to a raw chain fact. The step
		// above reads a supply only when it is about to multiply it by
		// a price, so an asset with no served price keeps no supply on
		// the listing — which is correct there and wrong here, where
		// the reference basis values a float that has never traded.
		// This fills the gap from the SAME supply readers, in the same
		// preference order, and touches nothing else: no price, no
		// market cap, no gate.
		//
		// The contract arm needs no equivalent: fillContractMarketCaps
		// reads the lake supply for every contract row before it looks
		// at a price, so those rows already carry the fact.
		s.rwaFillMissingSupply(ctx, details)
		s.fillDeclaredPegPricesInListing(ctx, details)
		s.fillIssuerDirectoryTags(ctx, details)
		for _, d := range details {
			out[rwaKey(d.Code, issuer)] = d
		}
	}
	return out, join, nil
}

// rwaFillMissingSupply attaches circulating supply to the RWA rows the
// market-cap fill left without one.
//
// Only rows with no reading at all are touched, so a supply the
// pipeline DECIDED on — the dust-suppressed and ticker-collision
// branches both attach one deliberately — is never overwritten. The
// only rows this reaches are the ones the market-cap fill returned from
// early, before it looked a supply up, because the row carried no
// price to multiply it by.
//
// Filling it changes no valuation and no gate. A circulating supply is
// a chain fact rather than a price claim, which is why this surface
// already serves one beside a WITHHELD market cap; the reference basis
// is the first thing here that needs the fact on a row that has no
// price at all. Scoped to this surface: /v1/assets is untouched.
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
			Recognition:         mem.recognition,
			AnchorAsset:         strings.TrimSpace(mem.anchorAsset),
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			// The asset's OWN scale, carried from the listing row. The
			// reference valuation divides by it, and a constant in its
			// place would be the hardcoded-decimals defect the
			// market-cap path already had to fix, in a new coordinate.
			Decimals:     d.Decimals,
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

// rwaSupplyProvenance carries the listing row's supply basis onto the RWA
// row, together with whether a figure on that basis is a floor.
//
// Both are read off the SAME field, which is the point: the flag is
// [supply.Basis.LowerBound] applied to the basis actually stamped, not a
// second fact recorded beside it that a later arm could forget to set.
//
// A row with a supply but no basis returns nothing rather than guessing
// one. That state is reachable — a supply another branch attached
// deliberately (the dust-suppressed and ticker-collision paths both do)
// travels without one — and naming an arm that did not answer would be a
// worse failure than naming none.
func rwaSupplyProvenance(d AssetDetail) (string, bool) {
	if d.CirculatingSupply == nil || d.SupplyBasis == nil {
		return "", false
	}
	b := supply.Basis(*d.SupplyBasis)
	return b.String(), b.LowerBound()
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
		// Reported before the supply reason, because it is the more
		// specific finding: a row here HAS a supply and is missing the
		// exponent, which is a different gap with a different owner.
		if d.DecimalsUnresolved {
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

// rwaSumReferenceValues is [rwaSumMarketCaps] on the reference basis:
// the exact sum of the published per-asset reference valuations, and
// how many rows contributed. Same big.Rat arithmetic over the same
// already-rounded 2-dp strings, so this level is likewise the exact sum
// of the level below. nil when nothing was reference-valued — a zero
// there would read as "the backing is worth nothing", which is the one
// reading certain to be wrong.
//
// A separate function rather than a parameterised one because the two
// bases must stay independently readable: a shared accessor invites a
// later caller to pass the wrong selector and quietly sum a market cap
// into the reference total.
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

// rwaBothBases totals the rows carrying BOTH a market cap and a
// reference valuation, on each basis separately.
//
// The restriction to the overlap is the entire point. Summing every
// market cap and every reference valuation and subtracting compares two
// different populations of assets, and the difference then reads as a
// premium or discount that nobody measured.
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

// rwaReferenceSources lists the distinct oracles behind the published
// reference valuations, sorted. Only the rows that CONTRIBUTED are
// read: naming a publisher whose figure was refused would attribute to
// it a number it is not in.
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

// rwaReferenceProvenances lists the distinct kinds of claim in the
// reference total, read off the CONTRIBUTING rows only. A row whose
// reference was published but whose valuation was refused contributed
// nothing, so naming its provenance here would describe the total by
// something not in it.
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

// rwaReferenceProvenanceProse describes WHAT KIND of claim is in the
// total, from the provenances actually present in it.
//
// This exists because the basis string used to describe one provenance
// and the total now admits two, which are different claims about
// different subjects:
//
//   - an ORACLE NAV values the INSTRUMENT, and the step from there to
//     the token rests on the issuer's own domain-bound declaration that
//     one token is one unit of it;
//   - a LISTING PRICE values the TOKEN directly, and makes no claim
//     about the backing at all — which is weaker in one way (nobody
//     independent has said what is behind the token) and stronger in
//     another (no unstated one-for-one assumption sits inside it).
//
// The sentence is derived from the rows rather than written once for
// all cases, because a total that is entirely oracle-priced today must
// not carry a paragraph about listing prices, and a total that gains
// its first listing-priced row must not keep describing itself as
// resting on an issuer's declaration. A basis string that describes a
// provenance the total does not contain is exactly as wrong as one that
// omits a provenance it does.
func rwaReferenceProvenanceProse(provenances []string) string {
	var (
		oracle  bool
		listing bool
	)
	for _, p := range provenances {
		switch p {
		case RWAReferenceOracleNAV:
			oracle = true
		case RWAReferenceListingPrice:
			listing = true
		}
	}
	const oracleProse = "Oracle-priced rows (`provenance: oracle_instrument_nav`) are what an independent oracle says one unit " +
		"of the BACKING is worth, multiplied by the tokens in circulation, resting on the issuer's own domain-bound " +
		"declaration that one token is one unit of that instrument. On those rows nobody was seen paying it: it is an " +
		"assertion about the value of the backing, and no gate on this platform can corroborate an assertion. "
	const listingProse = "Listing-priced rows (`provenance: listing_platform_price`) are an independent listing platform's own USD price " +
		"for the TOKEN, bound to the exact address it was published against and never matched on a code. On a " +
		"contract-issued row it comes from the same source that corroborated the address at C2; on a classic row it " +
		"corroborated nothing and supplies only the price, because that row was admitted by its issuer's own " +
		"domain-bound declaration. It is a different and weaker claim than an oracle NAV: it says what the token " +
		"changes hands at on the venues that platform tracks, and asserts NOTHING about what stands behind it. No premium " +
		"is published against it, because a premium measured against an aggregate of the same markets our own price " +
		"samples would be the market compared with itself. Somebody WAS seen paying something like it — on venues this " +
		"index does not gate — which is precisely why it may not be added to market_cap_usd, whose whole meaning is a " +
		"price that survived those gates. "
	switch {
	case oracle && listing:
		return "The total MIXES TWO KINDS OF CLAIM and the per-row `provenance` field says which is which. " +
			oracleProse + listingProse
	case listing:
		return listingProse
	case oracle:
		return oracleProse
	default:
		// No contributing rows, so no claim to describe. The callers
		// below state the absence.
		return ""
	}
}

// rwaReferenceBasis states what the reference total measured and, at
// least as importantly, what it is not.
//
// The prose carries weight the field name cannot. `market_cap_usd` is a
// figure somebody was observed paying, filtered by three gates that can
// each withhold it; this one is an assertion about the value of the
// backing, and no gate on this platform can corroborate an assertion.
// A reader who takes the two for the same kind of number will read a
// claim as a measurement, so the difference is stated rather than
// implied.
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
		ReferenceValuation:      rwaSummariseReference(assets),
		BothBases:               rwaBothBases(assets),
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
	// Stated on the market-cap basis as well as on the reference one,
	// because the misreading to prevent is a reader arriving at THIS
	// total and assuming it now includes the reference-priced figure.
	// It does not, and no row moved into or out of it.
	b.WriteString(" Reference-priced valuations are a separate basis and are NOT in this total: summary.reference_valuation carries them under their own name.")
	if truncated {
		b.WriteString(" The issuer cap bound this rebuild, so the set is known to be incomplete.")
	}
	return b.String()
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
