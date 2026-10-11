package v1

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// The RWA funnel: the complete accounting from every issuer that could carry a SEP-1 attestation
// down to the assets served.
//
// The served set is small by construction and the population it is drawn from is not; without the
// funnel nothing distinguishes "the network holds few real-world assets" from "the pipeline
// discarded thousands of candidates without saying so". A refusal tally cannot close that gap
// alone: it reports requirements that were EVALUATED, and the stages that discard most of the
// population run before any candidate reaches the definition. The funnel reports the stages, the
// tally the requirements, and together every unit that entered is accounted for.
//
// NOT a second membership rule: it admits nothing; widening coverage never loosens it.

// Funnel stage units. The unit changes down the funnel and comparing counts of different units is
// the easiest way to misread it, so every stage carries its own.
const (
	rwaUnitIssuers      = "issuer_accounts"
	rwaUnitDeclarations = "sep1_currency_declarations"
	rwaUnitAssets       = "assets"
)

// rwaFunnelBasis states what the funnel measured. Served beside the
// counts because two of its stages are populations no other field on
// this surface mentions at all.
const rwaFunnelBasis = "Every issuer account that could carry a SEP-1 attestation, narrowed to the assets served. " +
	"Units change down the funnel: issuer accounts, then the SEP-1 [[CURRENCIES]] declarations they publish, then the " +
	"(code, issuer) assets those bind to. Each stage's drops account exactly for the difference to the next stage of " +
	"the same unit. `actor` names who can move a number: an operator, the token's own issuer, or nobody — the " +
	"definition refusing it."

// rwaFunnelClassicUnmeasured corrects the basis sentence when no issuer-bound attestation reader is
// wired. Without it the stages read zero, and a narrowing of zeros would assert that no issuer
// attests to a real-world asset, out of a scan that never ran.
//
// A WIRED scan that did not answer never reaches here: that rebuild is refused at the cache, the
// last good set keeps being served, and the response says so in `membership.rebuild_failed_at`.
const rwaFunnelClassicUnmeasured = "The `classic` arm was NOT MEASURED: no issuer-bound SEP-1 attestation reader " +
	"is wired, so that population was never walked. Its stages below read zero because nobody looked, not because " +
	"no issuer on this network attests to a real-world asset."

// rwaFunnelContractsBasis describes the second arm. It is appended, not folded in, because the arms
// walk different populations: one narrowing read as the whole would make an entity absent from the
// first look refused when it was never in that population.
const rwaFunnelContractsBasis = "The `contract` arm walks a SEPARATE population: every entry in the curated " +
	"third-party directory, narrowed to the contract addresses it names with an issuing tag, then to the tokens " +
	"the definition admits. It exists because the issuers table the classic arm walks is populated only when a " +
	"CLASSIC asset is registered, so an entity whose Stellar presence is contract-issued is absent from that " +
	"population entirely rather than refused by any requirement. Stage counts reconcile WITHIN an arm; the two " +
	"arms meet only at the assets served. `directory_recognised_issuing_accounts` is a terminal census, not part " +
	"of the narrowing: it counts recognised issuing entities this index holds no token for, and names a bounded " +
	"sample of them in `unreached_entities`."

// rwaFunnelContractsUnmeasured is served when no directory contract reader is wired: nothing was
// looked at, which is not the same finding as nothing was there.
const rwaFunnelContractsUnmeasured = "The `contract` arm was NOT MEASURED: no curated-directory contract reader " +
	"is wired, so that population was never walked. Its absence from the stages below is a configuration " +
	"statement, not a network with no contract-issued real-world assets in it."

// rwaFunnelUnavailableBasis is served when membership could not be established at all. It states
// the absence rather than a narrowing of zeros, which would read as a measured population of nothing.
const rwaFunnelUnavailableBasis = "Not measured: the attestation scan or the curated account directory did not " +
	"answer, so no population was walked. A funnel of zeros would read as a network with nothing in it."

// rwaFunnelValuationBasis describes the third arm. It continues PAST the served set, and says so:
// read as a membership narrowing, a row with no oracle feed would look refused by the definition
// when it is an admitted member whose backing nobody independent prices.
const rwaFunnelValuationBasis = "The `valuation` arm continues PAST the served set: it accounts for which served rows carry a " +
	"REFERENCE-priced valuation — circulating supply times an independent oracle's value for the instrument — and " +
	"counts every row that carries none under the reason that refused it. Its drop reasons are the same strings the " +
	"rows carry in `reference_valuation.status`, read back off the rows rather than recomputed. It does NOT walk the " +
	"market-cap basis: that coverage is `summary.assets_valued` and `assets_unvalued`, and each of its refusals is " +
	"already a `valuation.status` on the row. Membership is decided before either valuation, so nothing in this arm " +
	"admits or refuses an asset."

// rwaFunnelListingBasis describes C2's second arm. Appended, like the contract arm's sentence,
// because its root is a different population.
const rwaFunnelListingBasis = "The `listing` arm walks C2's SECOND route to recognition and a THIRD population: every in-repo " +
	"curated contract binding, narrowed to the addresses an independent listing directory ALSO names. It exists " +
	"because the curated account directory the `contract` arm walks names none of those addresses, so a verified " +
	"in-repo identity could never be reached to be tested and its refusal never appeared in any tally. An in-repo " +
	"binding NEVER admits on its own — that would be this index vouching for itself — and a listing entry never " +
	"admits on its own either: the listing is a convenience built for price aggregation, so it corroborates a claim " +
	"rather than attesting to one, and both sources must name the same exact address. A scam-class tag on that " +
	"address refuses it whatever either source says — and when the curated read that carries those tags does not " +
	"answer, the arm closes under `curated_tag_lookup_unavailable`, which names THAT source rather than borrowing " +
	"the listing's name for somebody else's outage. `listing_contracts_without_curated_binding` is a terminal " +
	"census, not part of the narrowing: it counts addresses the listing names that no curated binding does, which " +
	"is what makes `a listing admits nothing alone` auditable rather than merely asserted. The arm FAILS CLOSED: if " +
	"the listing read does not answer, or its cached snapshot is past the recognition bound, every binding it would " +
	"have corroborated is dropped under `independent_listing_unavailable` and the set is SMALLER, rather than being " +
	"held up by a recognition nobody re-established. `funnel.listing_directory` carries the EVIDENCE behind that " +
	"verdict — what the directory held and WHEN this set read it — so a closed arm can be told apart from a stopped " +
	"sync without a database. `entries` counts FRESH rows only and `stale` counts the rows past the recognition " +
	"bound beside them, so zero entries with zero stale is a directory nobody has ever synced, zero entries with " +
	"stale rows is a sync that DIED, and entries with no contracts among them is a healthy directory that names no " +
	"Stellar contract address. Compare `observed_at` against the sync's own clock in every closed-arm case: a sync " +
	"that completed after it means this set predates the rows and the next rebuild carries them."

// rwaFunnelListingUnmeasured is served when no listing reader is wired. This arm's refusals are
// about whether a second party agrees, so reporting "nobody agrees" when nobody was asked would be
// the strongest possible misstatement.
const rwaFunnelListingUnmeasured = "The `listing` arm was NOT MEASURED: no independent listing reader is wired, so that " +
	"population was never walked and no corroboration was looked for. A reader that IS wired and cannot answer — a " +
	"failed read, or a snapshot past the recognition bound — is a different state and takes the measured branch, " +
	"reporting every binding dropped under `independent_listing_unavailable`. Here, nothing was refused at all: an " +
	"unwired reader makes no finding, because nobody looked. The set is SMALLER than it would otherwise be and this " +
	"sentence is why."

// rwaFunnelUnavailable is the funnel for a response that publishes no
// set because membership could not be established.
func rwaFunnelUnavailable() RWAFunnel {
	return RWAFunnel{Stages: []RWAFunnelStage{}, Balanced: false, Basis: rwaFunnelUnavailableBasis}
}

// rwaFunnelOf builds the accounting from one rebuild's census, the
// catalogue join, and the number of rows actually served.
//
// The refusal tally is read back out of the membership, not restated, so the funnel cannot
// disagree with `refused[]` about how many candidates a requirement turned away.
func rwaFunnelOf(
	m rwaMembership, join rwaCatalogueJoin, served, contractsServed, listingServed int, assets []RWAAsset,
) RWAFunnel {
	stages := rwaClassicStages(m, join, served)
	// Every check reports WHY it failed, so a reader is told what did not close and not merely
	// `balanced: false`.
	var why []string
	if w := m.census.Check(); w != "" {
		why = append(why, "classic census: "+w)
	}
	// The contract arm is appended only when its population was measured: stages reading zero from a
	// scan that never ran would assert the directory names no real-world contracts.
	if m.contractCensus.available {
		stages = append(stages, rwaContractStages(m, contractsServed)...)
		if w := m.contractCensus.dir.Check(); w != "" {
			why = append(why, "contract census: "+w)
		}
	}
	// C2's SECOND arm, appended only when walked, for the same reason: zeros from an unwired
	// listing reader would assert that no independent party names any curated binding.
	if m.listingCensus.available {
		stages = append(stages, rwaListingStages(m, listingServed)...)
	}
	stages = append(stages, rwaValuationStages(assets)...)
	// The arms and the row list are three accountings of the same served set, but the arm boundary
	// is unbridgeable, so only this check notices a row lost between the arms and the valuation list.
	if armsServed := served + contractsServed + listingServed; len(assets) != armsServed {
		why = append(why, fmt.Sprintf(
			"served set: %d rows listed, %d counted across the arms", len(assets), armsServed))
	}
	// Each arm's own census is the storage layer's statement about its numbers; those checks keep
	// holding if the stage list is restructured, which is when a derived check quietly stops covering.
	if w := rwaFunnelImbalance(stages); w != "" {
		why = append(why, "stages: "+w)
	}
	return RWAFunnel{
		Stages: stages,
		// Balanced and Imbalance are one statement in two forms: the boolean IS the emptiness of
		// the list.
		Balanced:         len(why) == 0,
		Imbalance:        strings.Join(why, "; "),
		Basis:            rwaFunnelBasisFor(m.available, m.contractCensus.available, m.listingCensus.available),
		ListingDirectory: rwaListingDirectoryOf(m.listingCensus),
	}
}

// rwaListingDirectoryOf publishes the listing arm's evidence, or nil
// when there is none to publish.
//
// Gated on the TIMESTAMP, not the counts: zeros are left by both an empty directory and a read that
// never completed, and only the first is an observation. Serving zeros from a failed read would
// publish "the directory is empty" out of a query that did not run.
func rwaListingDirectoryOf(lc rwaListingCensus) *RWAListingDirectory {
	if lc.observedAt.IsZero() {
		return nil
	}
	return &RWAListingDirectory{
		ObservedAt: WireTime(lc.observedAt),
		Entries:    lc.dir.Entries,
		Contracts:  lc.dir.Contracts,
		Stale:      lc.dir.Stale,
	}
}

// rwaValuationStages is the reference-valuation walk: the served set,
// down to the rows carrying a reference-priced figure.
//
// The drops are read back OFF the served rows, so filtering rows by `reference_valuation.status`
// gives the counts printed here and the funnel cannot hold its own opinion about a missing figure.
//
// The arithmetic is NOT tautological: the stage counts rows carrying a figure (`value_usd`
// present) while the drops count rows whose status is not published. They reconcile only if
// published and carrying-money are the same rows, so a row claiming `published` with no money, or
// carrying money under a refusal, unbalances the funnel.
func rwaValuationStages(assets []RWAAsset) []RWAFunnelStage {
	valued := 0
	byReason := map[string]int{}
	for _, a := range assets {
		if a.ReferenceValuation.ValueUSD != nil {
			valued++
		}
		if a.ReferenceValuation.Status == RWAReferenceValuationPublished {
			continue
		}
		byReason[a.ReferenceValuation.Status]++
	}
	stages := []RWAFunnelStage{
		{
			Stage: rwaStageValuationCandidates, Unit: rwaUnitAssets, Count: len(assets),
			Dropped: rwaReferenceDrops(byReason),
		},
		{Stage: rwaStageReferenceValued, Unit: rwaUnitAssets, Count: valued},
	}
	for i := range stages {
		stages[i].Arm = rwaArmValuation
	}
	return stages
}

// rwaReferenceRefusalOrder is the order the rules refuse a reference in
// [rwaApplyReference], which is the order the drops are reported in. A
// map iteration would reorder the wire between two identical responses.
var rwaReferenceRefusalOrder = []string{
	RWAPremiumIssuerFlagged,
	RWAPremiumReferenceUnavailable,
	RWAPremiumContractNotBound,
	RWAPremiumReferenceISINMismatch,
	RWAPremiumNotInstrumentScoped,
	RWAPremiumNotBound,
	RWAPremiumReferenceNotUSD,
	RWAPremiumNoReference,
	RWAPremiumReferenceExpired,
	RWAPremiumReferenceNotPositive,
	RWAReferenceValuationNoSupply,
	RWAReferenceValuationDecimalsUnknown,
}

// rwaReferenceDropActors names who can move each reference-valuation drop, on the membership arms'
// three-actor vocabulary.
//
// `operator` where somebody here can act: no oracle publishes the instrument, the read failed or
// went stale, no supply reading covers the asset, or the curated contract-to-feed set is empty.
// `definition` where the rule is working: a flagged issuer, an unbound pair (usually a token
// wearing an instrument's ticker), a feed pricing an ounce rather than a token, a reserve-asset
// denominated value, and a non-positive value. No drop here is the ISSUER's: an issuer cannot make
// an oracle price its instrument.
var rwaReferenceDropActors = map[string]string{
	RWAPremiumIssuerFlagged:        rwaActorDefinition,
	RWAPremiumNotBound:             rwaActorDefinition,
	RWAPremiumNotInstrumentScoped:  rwaActorDefinition,
	RWAPremiumReferenceNotUSD:      rwaActorDefinition,
	RWAPremiumReferenceNotPositive: rwaActorDefinition,
	RWAPremiumContractNotBound:     rwaActorOperator,
	RWAPremiumNoReference:          rwaActorOperator,
	RWAPremiumReferenceUnavailable: rwaActorOperator,
	RWAPremiumReferenceExpired:     rwaActorOperator,
	RWAReferenceValuationNoSupply:  rwaActorOperator,
	// The operator's: the constant-NAV binding must be re-read against the issuer's page and
	// re-bound or removed.
	RWAPremiumReferenceISINMismatch: rwaActorOperator,
	// The operator's: the scale is readable on chain, so this reason means a reader here is
	// unwired, failing, or has not captured the contract instance. Never the issuer's.
	RWAReferenceValuationDecimalsUnknown: rwaActorOperator,
}

// rwaReferenceDrops renders the tallied refusals in rule order.
//
// A status with no entry in [rwaReferenceDropActors] is a defect here, not in the data, so it is
// attributed to the OPERATOR and reported rather than dropped (which would unbalance the funnel
// uninformatively); a test pins every status a row can carry to an actor.
func rwaReferenceDrops(byReason map[string]int) []RWAFunnelDrop {
	out := make([]RWAFunnelDrop, 0, len(byReason))
	seen := map[string]struct{}{}
	for _, reason := range rwaReferenceRefusalOrder {
		seen[reason] = struct{}{}
		if n := byReason[reason]; n > 0 {
			out = append(out, RWAFunnelDrop{
				Reason: reason, Count: n, Actor: rwaReferenceDropActors[reason],
			})
		}
	}
	unknown := make([]string, 0, 2)
	for reason := range byReason {
		if _, ok := seen[reason]; !ok {
			unknown = append(unknown, reason)
		}
	}
	sort.Strings(unknown)
	for _, reason := range unknown {
		out = append(out, RWAFunnelDrop{
			Reason: reason, Count: byReason[reason], Actor: rwaActorOperator,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rwaFunnelBasisFor states what was measured, including whether each
// arm was walked at all.
func rwaFunnelBasisFor(classicMeasured, contractsMeasured, listingMeasured bool) string {
	parts := []string{rwaFunnelBasis}
	// Appended as a correction to the sentence above rather than
	// replacing it, because that sentence also defines the units and
	// the `actor` column every other arm's stages use.
	if !classicMeasured {
		parts = append(parts, rwaFunnelClassicUnmeasured)
	}
	if contractsMeasured {
		parts = append(parts, rwaFunnelContractsBasis)
	} else {
		parts = append(parts, rwaFunnelContractsUnmeasured)
	}
	if listingMeasured {
		parts = append(parts, rwaFunnelListingBasis)
	} else {
		parts = append(parts, rwaFunnelListingUnmeasured)
	}
	return strings.Join(append(parts, rwaFunnelValuationBasis), " ")
}

// rwaClassicStages is the SEP-1 attestation walk: every issuer account
// that could carry an attestation, down to the classic rows served.
func rwaClassicStages(m rwaMembership, join rwaCatalogueJoin, served int) []RWAFunnelStage {
	c := m.census

	// Issuers with an on-chain home_domain, less those holding no attestation. That gap is TWO
	// findings with two owners: an issuer nothing has fetched yet is an operator backlog, while one
	// whose domain WAS reached and served nothing storable is the issuer's own publication problem.
	// The second dominates in practice, so calling all of it backlog would overstate the operator's
	// share of the gap.
	//
	// The split cannot say WHY a reached domain served nothing: no per-attempt outcome is stored, so
	// 404, dead name, TLS failure and a document without SEP-1 are one count.
	//
	// Both counts are clamped (the gap at zero; fetched-but-empty to the gap). Neither clamp hides an
	// inconsistency: the census Check() bounds the same counts independently, and Balanced requires both.
	gap := max(c.IssuersWithHomeDomain-c.IssuersWithPayload, 0)
	servedNothing := min(c.IssuersFetchedWithoutPayload, gap)
	neverFetched := gap - servedNothing

	stages := []RWAFunnelStage{
		{
			Stage: rwaStageIssuersWithHomeDomain, Unit: rwaUnitIssuers, Count: c.IssuersWithHomeDomain,
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwaDropNoAttestation, Count: neverFetched, Actor: rwaActorOperator},
				RWAFunnelDrop{Reason: rwaDropDomainServedNothing, Count: servedNothing, Actor: rwaActorIssuer},
			),
		},
		{
			// The unreadable bucket is counted explicitly: a swallowed parse error would let a whole
			// population vanish and the response look like a quiet network.
			Stage: rwaStageIssuersWithSep1, Unit: rwaUnitIssuers, Count: c.IssuersWithPayload,
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwaDropAttestationStale, Count: c.IssuersPayloadStale, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropPayloadUnreadable, Count: c.IssuersPayloadUnreadable, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropDeclaresNothing, Count: c.IssuersDeclaringNothing, Actor: rwaActorIssuer},
			),
		},
		{
			// Last stage counted in issuer accounts; the next counts their declarations, so the unit
			// does not change silently under a drop list.
			Stage: rwaStageIssuersDeclaring, Unit: rwaUnitIssuers, Count: c.IssuersDeclaring,
		},
		{
			Stage: rwaStageSep1Entries, Unit: rwaUnitDeclarations, Count: c.Entries,
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwaDropMissingCode, Count: c.EntriesMissingCode, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropMissingIssuer, Count: c.EntriesMissingIssuer, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropAnotherIssuer, Count: c.EntriesNamingAnotherIssuer, Actor: rwaActorDefinition},
			),
		},
		{
			// Requirement 4's pre-filter runs on the bound entries before any directory read, so its
			// drop is a funnel stage. It is ALSO in `refused[]` under requirement 4, the one place that
			// tally reports out of R1-R4 order.
			Stage: rwaStageBoundEntries, Unit: rwaUnitDeclarations, Count: c.EntriesBound,
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropNoInstrumentBasis, Count: c.EntriesFiltered, Actor: rwaActorDefinition,
			}),
		},
		{
			Stage: rwaStageCandidates, Unit: rwaUnitAssets, Count: c.EntriesKept,
			Dropped: rwaCandidateDrops(m),
		},
		{
			Stage: rwaStageAdmitted, Unit: rwaUnitAssets, Count: len(m.members),
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwaDropNotInCatalogue, Count: join.notObserved, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropIssuerPageTruncate, Count: join.pagesTruncated, Actor: rwaActorOperator},
			),
		},
		{Stage: rwaStageServed, Unit: rwaUnitAssets, Count: served},
	}
	for i := range stages {
		stages[i].Arm = rwaArmClassic
	}
	return stages
}

// rwaContractStages is the curated-directory walk: every entry in the
// third-party directory, down to the contract rows served.
//
// It starts from a DIFFERENT root than the classic arm and shares no stage with it: the entities it
// reaches are absent from the issuers table the classic arm walks.
func rwaContractStages(m rwaMembership, served int) []RWAFunnelStage {
	cc := m.contractCensus
	c := cc.dir

	stages := []RWAFunnelStage{
		{
			// The curated set split by strkey form. An account address names an ENTITY (many tokens or
			// none); a contract address names ONE token. The arm identifies tokens, so account rows
			// leave here under `definition` (scope of the rule, not a closable gap). The recognised,
			// unflagged, tokenless entities are counted at directory_recognised_issuing_accounts below
			// and NAMED in unreached_entities.
			Stage: rwaStageDirectoryEntries, Unit: rwaUnitDirectoryAddresses, Count: c.Entries,
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropDirectoryNamesAnAccount, Count: c.Accounts, Actor: rwaActorDefinition,
			}),
		},
		{
			Stage: rwaStageDirectoryContracts, Unit: rwaUnitContracts, Count: c.Contracts,
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwa.RejectContractScam, Count: c.ContractsScamFlagged, Actor: rwaActorDefinition},
				RWAFunnelDrop{Reason: rwa.RejectContractNoTag, Count: c.ContractsWithoutIssuingTag, Actor: rwaActorOperator},
			),
		},
		{
			// C2 and C3 are satisfied at this line. Everything below is
			// C4 and the catalogue join.
			Stage: rwaStageRecognisedContracts, Unit: rwaUnitContracts, Count: c.ContractsRecognised,
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropOverContractScanCap, Count: cc.overCap, Actor: rwaActorOperator,
			}),
		},
		{
			// Unit changes from contracts to assets and still reconciles: a contract IS the asset it
			// issues, unlike the classic arm where one issuer publishes many declarations.
			Stage: rwaStageContractCandidates, Unit: rwaUnitAssets, Count: cc.evaluated,
			Dropped: rwaContractCandidateDrops(m),
		},
		{
			// This arm's OWN admissions: m.contracts carries both C2 arms' rows, and counting all of
			// them would claim the other arm's and stop the arithmetic closing.
			Stage: rwaStageContractsAdmitted, Unit: rwaUnitAssets, Count: rwaDirectoryAdmitted(m),
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropNotInCatalogue, Count: m.contractsNotObserved, Actor: rwaActorIssuer,
			}),
		},
		{Stage: rwaStageContractsServed, Unit: rwaUnitAssets, Count: served},
		{
			// A TERMINAL stage, not part of the narrowing. It counts the account rows the first stage
			// dropped, filtered to recognised, unflagged entities this index holds no classic asset for.
			// It takes no part in the stage arithmetic: folding a coverage census into a narrowing would
			// close the funnel by adding a number that measures something else.
			Stage: rwaStageRecognisedIssuingAcct, Unit: rwaUnitDirectoryAddresses,
			Count: c.AccountsIssuingWithoutAsset,
		},
	}
	for i := range stages {
		stages[i].Arm = rwaArmContract
	}
	return stages
}

// rwaListingStages is C2's SECOND arm: every in-repo curated contract
// binding, narrowed to the tokens an independent listing directory
// corroborated into the set.
//
// It is a separate arm because it narrows a DIFFERENT population from a different root: the contract
// arm starts at the curated account directory, this one at a hand-reviewed in-repo table, looking
// for somebody else agreeing. Folding them would sum two unrelated populations in one stage.
//
// The arm is short on purpose: the drops between a verified in-repo identity and an admitted asset
// are all somebody other than this repository declining to agree, or nobody having been asked.
func rwaListingStages(m rwaMembership, served int) []RWAFunnelStage {
	lc := m.listingCensus
	stages := []RWAFunnelStage{
		{
			// The root: every address this repository has bound to a named real-world instrument at the
			// evidence bar internal/rwa/contract.go documents. A constant of the build, so the drops
			// below read as statements about the world rather than about our scan.
			Stage: rwaStageCuratedBindings, Unit: rwaUnitContracts, Count: lc.bindings,
			Dropped: rwaDrops(
				// Not a refusal: the contract arm already evaluates this address, and evaluating it
				// twice would serve it twice.
				RWAFunnelDrop{
					Reason: rwaDropAlreadyEvaluatedByDirectory,
					Count:  lc.alsoInDirectoryArm, Actor: rwaActorDefinition,
				},
				// The fail-closed shrink. An outage is the operator's to fix and is reported apart from
				// the finding below, since a read that did not answer may not report an absence. It names
				// the LISTING directory only, so a curated-directory failure does not alarm against a
				// healthy listing sync.
				RWAFunnelDrop{
					Reason: rwa.RejectContractListingUnavailable,
					Count:  lc.listingUnavailable, Actor: rwaActorOperator,
				},
				// The arm's OTHER outage: the curated tag read C3 depends on. Same actor, different
				// reason: the actor says who acts and the reason says where.
				RWAFunnelDrop{
					Reason: rwa.RejectContractCuratedTagsUnavailable,
					Count:  lc.tagsUnavailable, Actor: rwaActorOperator,
				},
				// The requirement doing its job. `operator` not `definition`, because somebody CAN move
				// it by finding a second independent source that names the address.
				RWAFunnelDrop{
					Reason: rwa.RejectContractCuratedNotListed,
					Count:  lc.notListed, Actor: rwaActorOperator,
				},
			),
		},
		{
			// Unit changes from contracts to assets and reconciles: a contract IS the asset it issues.
			Stage: rwaStageListingCorroborated, Unit: rwaUnitAssets, Count: lc.evaluated,
			Dropped: rwaListingCandidateDrops(m),
		},
		{
			Stage: rwaStageListingAdmitted, Unit: rwaUnitAssets, Count: rwaListingAdmitted(m),
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropNotInCatalogue, Count: m.listingNotObserved, Actor: rwaActorIssuer,
			}),
		},
		{Stage: rwaStageListingServed, Unit: rwaUnitAssets, Count: served},
		{
			// A TERMINAL census, sibling of directory_recognised_issuing_accounts. It counts addresses
			// the listing names that no curated binding does: the population an operator reviews to grow
			// the set, and the auditable form of "a listing admits nothing on its own". It takes no part
			// in the arithmetic.
			Stage: rwaStageListedWithoutBinding, Unit: rwaUnitContracts,
			Count: lc.listedWithoutBinding,
		},
	}
	for i := range stages {
		stages[i].Arm = rwaArmListing
	}
	return stages
}

// rwaDirectoryAdmitted counts the members C2's FIRST arm admitted, by recognition source. With
// [rwaListingAdmitted] it partitions m.contracts exactly, as [rwa.QualifyContract] guarantees one
// recognition per admitted contract.
func rwaDirectoryAdmitted(m rwaMembership) int {
	n := 0
	for _, c := range m.contracts {
		if c.recognition == rwa.RecognitionCuratedDirectory {
			n++
		}
	}
	return n
}

// rwaListingAdmitted counts the members this arm admitted, read back off
// the membership by recognition source rather than tallied separately.
func rwaListingAdmitted(m rwaMembership) int {
	n := 0
	for _, c := range m.contracts {
		if c.recognition == rwa.RecognitionListingCorroborated {
			n++
		}
	}
	return n
}

// rwaListingCandidateDrops is what happened to the candidates that
// reached the full ordered evaluation on this arm.
//
// C2's own refusals are decided by the candidate build and appear on the stage above. What remains
// is C3: a scam flag on an address the curated directory named ONLY to flag, attributed to
// `definition` because an address both verified here and flagged by a third party is the case the
// precedence exists for.
//
// The C4 drop is UNREACHABLE on this arm by construction (a candidate reaches the verdict only if a
// curated binding names it, which C4's first branch answers on). It is listed so a future candidate
// admitted on other evidence cannot drop out of the accounting silently.
func rwaListingCandidateDrops(m rwaMembership) []RWAFunnelDrop {
	out := make([]RWAFunnelDrop, 0, 2)
	for _, d := range []struct {
		reason string
		actor  string
	}{
		{rwa.RejectContractScam, rwaActorDefinition},
		{rwa.RejectNoContractBasis, rwaActorDefinition},
	} {
		if n := m.listingRefusals[d.reason]; n > 0 {
			out = append(out, RWAFunnelDrop{Reason: d.reason, Count: n, Actor: d.actor})
		}
	}
	return out
}

// rwaContractCandidateDrops is what happened to the contracts that
// reached the full ordered evaluation: the C4 refusal, then the
// structural drop that is not a refusal at all.
//
// C1 to C3 are decided by the scan itself and appear as stages above, so their refusal constants are
// unreachable here, as not_a_classic_asset and no_issuer_bound_sep1_entry are on the classic arm.
func rwaContractCandidateDrops(m rwaMembership) []RWAFunnelDrop {
	out := make([]RWAFunnelDrop, 0, 2)
	if n := m.refusals[rwa.RejectNoContractBasis]; n > 0 {
		out = append(out, RWAFunnelDrop{
			Reason: rwa.RejectNoContractBasis, Count: n, Actor: rwaActorDefinition,
		})
	}
	return append(out, rwaDrops(RWAFunnelDrop{
		Reason: rwaDropDuplicateContractEntry, Count: m.contractCensus.duplicates, Actor: rwaActorOperator,
	})...)
}

// rwaCandidateDrops is what happened to the candidates that reached the
// full ordered evaluation: the requirement refusals in R1→R4 order,
// then the two structural drops that are not refusals at all.
func rwaCandidateDrops(m rwaMembership) []RWAFunnelDrop {
	ordered := []string{
		rwa.RejectNotClassic,
		rwa.RejectNoBoundSep1,
		rwa.RejectScamFlagged,
		rwa.RejectNoRecognition,
		rwa.RejectNoInstrumentClaim,
	}
	out := make([]RWAFunnelDrop, 0, len(ordered)+2)
	for _, reason := range ordered {
		n := m.refusals[reason]
		if reason == rwa.RejectNoInstrumentClaim {
			// The pre-filter's share is already a stage above; counting it twice would stop the
			// arithmetic closing.
			n -= m.census.EntriesFiltered
		}
		if n > 0 {
			out = append(out, RWAFunnelDrop{Reason: reason, Count: n, Actor: rwaActorDefinition})
		}
	}
	return append(out, rwaDrops(
		RWAFunnelDrop{Reason: rwaDropDuplicateDeclaration, Count: m.duplicateDeclarations, Actor: rwaActorIssuer},
		RWAFunnelDrop{Reason: rwaDropOverIssuerCap, Count: m.overIssuerCap, Actor: rwaActorOperator},
	)...)
}

// rwaDrops keeps the drops that actually happened; a zero bucket is noise and the arithmetic reads
// the same without it.
func rwaDrops(drops ...RWAFunnelDrop) []RWAFunnelDrop {
	out := make([]RWAFunnelDrop, 0, len(drops))
	for _, d := range drops {
		if d.Count > 0 {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rwaFunnelImbalance returns the reason the funnel does not add up, or
// "" when it does.
//
// Every adjacent pair must satisfy `count - sum(dropped) == next.count`, with two exceptions:
//
//   - the ONE transition from issuer accounts to their declarations, where no subtraction relates
//     the counts, so that pair must carry no drops. Every other unit change is a one-to-one
//     relabelling and still has to reconcile.
//   - the issuer_asset_page_truncated drop, which counts ISSUERS whose asset tail went unread, not
//     assets; letting it into the asset arithmetic would close the funnel by inventing a number.
//
// Surfaced through RWAFunnel.Balanced rather than raised as an error: a funnel that cannot be
// reconciled must SAY so.
func rwaFunnelImbalance(stages []RWAFunnelStage) string {
	for i := 0; i+1 < len(stages); i++ {
		if bad := rwaStagePairImbalance(stages[i], stages[i+1]); bad != "" {
			return bad
		}
	}
	return ""
}

// rwaStagePairImbalance checks one adjacent pair, returning the reason
// it does not reconcile or "" when it does.
func rwaStagePairImbalance(cur, next RWAFunnelStage) string {
	if why, unbridgeable := rwaUnbridgeablePair(cur, next); unbridgeable {
		if len(cur.Dropped) > 0 {
			return fmt.Sprintf("%s %s and also drops — a drop across it cannot be reconciled", cur.Stage, why)
		}
		return ""
	}
	sum := 0
	for _, d := range cur.Dropped {
		if d.Reason == rwaDropIssuerPageTruncate {
			continue
		}
		sum += d.Count
	}
	if cur.Count-sum != next.Count {
		return fmt.Sprintf("%s: %d less %d dropped is %d, not the %d at %s",
			cur.Stage, cur.Count, sum, cur.Count-sum, next.Count, next.Stage)
	}
	return ""
}

// rwaUnbridgeablePair reports whether no subtraction relates two
// adjacent stages, and why. Such a pair must carry no drops instead of reconciling. Exactly three:
//
//   - an ARM BOUNDARY: the arms narrow different populations and meet only at the served set.
//   - the ONE transition from issuer accounts to declarations, where one issuer publishes many.
//     Every other unit change is a one-to-one relabelling (a bound declaration IS the candidate
//     asset it names) and still has to reconcile.
//   - the TERMINAL CENSUS stage, which counts a population an earlier stage already dropped.
func rwaUnbridgeablePair(cur, next RWAFunnelStage) (string, bool) {
	switch {
	case cur.Arm != next.Arm:
		return "is the last stage of the " + cur.Arm + " arm", true
	case next.Stage == rwaStageRecognisedIssuingAcct, next.Stage == rwaStageListedWithoutBinding:
		return "precedes a terminal census stage that takes no part in the narrowing", true
	case cur.Unit == rwaUnitIssuers && next.Unit != rwaUnitIssuers:
		return "is the last stage counted in issuer accounts", true
	}
	return "", false
}
