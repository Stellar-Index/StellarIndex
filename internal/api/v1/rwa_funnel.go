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
// Without it nothing distinguishes "the network holds few real-world assets" from "the pipeline
// silently discarded thousands of candidates": the refusal tally covers only EVALUATED
// requirements, while most candidates are discarded before reaching the definition. Funnel stages
// plus tally account for every unit that entered.
//
// NOT a second membership rule: it admits nothing.

// Funnel stage units. The unit changes down the funnel and comparing counts of different units is
// the easiest way to misread it, so every stage carries its own.
const (
	rwaUnitIssuers      = "issuer_accounts"
	rwaUnitDeclarations = "sep1_currency_declarations"
	rwaUnitAssets       = "assets"
)

// rwaFunnelBasis states what the funnel measured, served beside the counts because two stages are
// populations no other field on this surface mentions.
const rwaFunnelBasis = "Every issuer account that could carry a SEP-1 attestation, narrowed to the assets served. " +
	"Units change down the funnel: issuer accounts, then the SEP-1 [[CURRENCIES]] declarations they publish, then the " +
	"(code, issuer) assets those bind to. Each stage's drops account exactly for the difference to the next stage of " +
	"the same unit. `actor` names who can move a number: an operator, the token's own issuer, or nobody — the " +
	"definition refusing it."

// rwaFunnelClassicUnmeasured corrects the basis sentence when no issuer-bound attestation reader is wired: otherwise zero stages would
// assert that no issuer attests to a real-world asset
// from a scan that never ran. A WIRED scan that failed never reaches here (the cache refuses that rebuild and keeps the last good set).
const rwaFunnelClassicUnmeasured = "The `classic` arm was NOT MEASURED: no issuer-bound SEP-1 attestation reader " +
	"is wired, so that population was never walked. Its stages below read zero because nobody looked, not because " +
	"no issuer on this network attests to a real-world asset."

// rwaFunnelContractsBasis describes the second arm; appended, not folded in, because the arms walk different populations and one
// narrowing read as the whole would make an entity absent from the first look refused.
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

// rwaFunnelValuationBasis describes the third arm, which continues PAST the served set: read as a membership narrowing, a row with no
// oracle feed would look refused when it is an admitted member nobody independent prices.
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

// rwaFunnelListingUnmeasured is served when no listing reader is wired: this arm's refusals mean a
// second party disagrees, so "nobody agrees" when nobody was asked would be a misstatement.
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

// rwaFunnelOf builds the accounting from one rebuild's census, the catalogue join and the rows served; the refusal tally is read back out
// of the membership so the funnel cannot disagree with `refused[]`.
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

// rwaListingDirectoryOf publishes the listing arm's evidence, or nil when none. Gated on the TIMESTAMP, not the counts: zeros come from
// both an empty directory and a read that never completed.
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

// rwaValuationStages is the reference-valuation walk from the served set down to rows carrying a reference-priced figure; drops are read
// back OFF the served rows, so the funnel holds no opinion of its own.
// Not tautological: the stage counts rows carrying `value_usd`, the drops count rows whose status is not published, so a row claiming
// `published` with no money (or money under a refusal) unbalances the funnel.
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

// rwaReferenceRefusalOrder is the order [rwaApplyReference] refuses a reference in, and so the
// order drops are reported; map iteration would reorder the wire between identical responses.
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

// rwaReferenceDropActors names who can move each reference-valuation drop: `operator` where somebody here can act (no oracle publishes
// the instrument, failed or stale read, no supply reading,
// empty curated feed set); `definition` where the rule is working (flagged issuer, unbound pair, ounce-priced feed, reserve-denominated
// or non-positive value). No drop is the ISSUER's.
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

// rwaReferenceDrops renders the tallied refusals in rule order. A status missing from [rwaReferenceDropActors] is a defect here, not in
// the data, so it is attributed to the OPERATOR and reported
// (dropping it would unbalance the funnel uninformatively); a test pins every status a row can carry to an actor.
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
	// Appended to the sentence above, not replacing it: that sentence also defines the units and
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

	// Issuers with an on-chain home_domain, less those holding no attestation. The gap is TWO
	// findings with two owners: an issuer nothing has fetched yet is an operator backlog; one whose
	// domain was reached and served nothing storable is the issuer's own publication problem (the
	// larger share).
	//
	// The split cannot say WHY a reached domain served nothing: no per-attempt outcome is stored, so
	// 404, dead name, TLS failure and a document without SEP-1 are one count. Both counts are
	// clamped; neither clamp hides an inconsistency, since the census Check() bounds the same
	// counts.
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
			// Requirement 4's pre-filter runs before any directory read, so its drop is a funnel
			// stage; it is ALSO in `refused[]` under requirement 4, out of R1-R4 order.
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

// rwaContractStages is the curated-directory walk, every third-party directory entry down to the contract rows served; it shares no stage
// with the classic arm (its entities are absent from the issuers table).
func rwaContractStages(m rwaMembership, served int) []RWAFunnelStage {
	cc := m.contractCensus
	c := cc.dir

	stages := []RWAFunnelStage{
		{
			// Split by strkey form: an account address names an ENTITY (many tokens or none), a
			// contract address ONE token. The arm identifies tokens, so account rows leave under
			// `definition` (scope of the rule, not a closable gap); recognised, unflagged, tokenless
			// entities are counted at directory_recognised_issuing_accounts and NAMED in
			// unreached_entities.
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
			// A TERMINAL stage, not part of the narrowing: the account rows the first stage dropped,
			// filtered to recognised, unflagged entities this index holds no classic asset for.
			// Folding a coverage census into the arithmetic would close the funnel with a number that
			// measures something else.
			Stage: rwaStageRecognisedIssuingAcct, Unit: rwaUnitDirectoryAddresses,
			Count: c.AccountsIssuingWithoutAsset,
		},
	}
	for i := range stages {
		stages[i].Arm = rwaArmContract
	}
	return stages
}

// rwaListingStages is C2's SECOND arm: every in-repo curated contract binding, narrowed to tokens an independent listing directory
// corroborated. A separate arm because it narrows a DIFFERENT
// population from a different root; folding them would sum two unrelated populations in one stage. Short on purpose: every drop is
// somebody else declining to agree, or nobody asked.
func rwaListingStages(m rwaMembership, served int) []RWAFunnelStage {
	lc := m.listingCensus
	stages := []RWAFunnelStage{
		{
			// The root: every address bound to a named real-world instrument at the evidence bar
			// internal/rwa/contract.go documents. A constant of the build, so the drops below describe
			// the world rather than our scan.
			Stage: rwaStageCuratedBindings, Unit: rwaUnitContracts, Count: lc.bindings,
			Dropped: rwaDrops(
				// Not a refusal: the contract arm already evaluates this address, and evaluating it
				// twice would serve it twice.
				RWAFunnelDrop{
					Reason: rwaDropAlreadyEvaluatedByDirectory,
					Count:  lc.alsoInDirectoryArm, Actor: rwaActorDefinition,
				},
				// The fail-closed shrink. An outage is the operator's to fix and is reported apart from
				// the finding below, since a read that did not answer may not report an absence. It
				// names the LISTING directory only, so a curated-directory failure does not alarm
				// against a healthy listing sync.
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
			// A TERMINAL census, sibling of directory_recognised_issuing_accounts: addresses the
			// listing names that no curated binding does, i.e. the population an operator reviews to
			// grow the set and the auditable form of "a listing admits nothing on its own". Outside
			// the arithmetic.
			Stage: rwaStageListedWithoutBinding, Unit: rwaUnitContracts,
			Count: lc.listedWithoutBinding,
		},
	}
	for i := range stages {
		stages[i].Arm = rwaArmListing
	}
	return stages
}

// rwaDirectoryAdmitted counts the members C2's FIRST arm admitted, by recognition source; with [rwaListingAdmitted] it partitions
// m.contracts exactly, as [rwa.QualifyContract] guarantees one recognition per contract.
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

// rwaListingCandidateDrops is what happened to candidates that reached the full ordered evaluation on this arm: C2's refusals appear on
// the stage above; what remains is C3, a scam flag on an address the
// directory named ONLY to flag (`definition`, the case the precedence exists for). The C4 drop is UNREACHABLE by construction, listed so
// an admitted candidate cannot drop out of the accounting silently.
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

// rwaContractCandidateDrops is what happened to contracts that reached the full ordered evaluation: the C4 refusal, then the structural
// non-refusal drop. C1 to C3 are decided by the scan itself, so their refusal constants are unreachable here.
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

// rwaCandidateDrops is what happened to candidates that reached the full ordered evaluation: requirement refusals in R1-R4 order, then
// the two structural drops that are not refusals.
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

// rwaFunnelImbalance returns the reason the funnel does not add up, or "" when it does: every adjacent pair must satisfy `count -
// sum(dropped) == next.count`, except (a) the one issuer-accounts-to-declarations
// transition, which must carry no drops, and (b) issuer_asset_page_truncated, which counts ISSUERS not assets (letting it into the asset
// arithmetic would close the funnel by inventing a number).
// Surfaced through RWAFunnel.Balanced rather than raised: a funnel that cannot be reconciled must SAY so.
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

// rwaUnbridgeablePair reports whether no subtraction relates two adjacent stages, and why; such a pair must carry no drops. Exactly
// three: an ARM BOUNDARY (arms narrow different populations and meet only at the served set),
// the ONE issuer-accounts-to-declarations transition (every other unit change is a one-to-one relabelling and must reconcile), and the
// TERMINAL CENSUS stage (counts a population an earlier stage already dropped).
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
