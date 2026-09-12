package v1

import (
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// The RWA funnel — the complete accounting from every issuer that could
// carry a SEP-1 attestation down to the assets served.
//
// WHY IT EXISTS. The served set is small by construction and the
// population it is drawn from is not. Measured on production
// 2026-09-10, GET /v1/rwa/assets served six assets and a refusal tally
// of three, over a table holding 59,303 issuer accounts, 44,376 of them
// with a home_domain and 14,635 with a fetched SEP-1 payload. Nothing
// in the response distinguished "the network holds six real-world
// assets" from "the pipeline discarded fourteen thousand candidates
// without saying so", and the only way to tell the two apart was a
// database session.
//
// A refusal tally cannot close that gap on its own: it reports
// requirements that were EVALUATED, and the stages that discard most of
// the population run before any candidate reaches the definition. So
// the funnel reports the stages, the refusal tally reports the
// requirements, and between them every unit that entered is accounted
// for.
//
// WHAT IT IS NOT. It is not a second membership rule and it admits
// nothing. Widening coverage means moving a number here that somebody
// can act on — an attestation nobody has fetched, an issuer nobody has
// vouched for — never loosening the definition until a bucket empties.

// Funnel stage units. The unit changes down the funnel, and comparing
// two counts of different units is the easiest way to misread it, so
// every stage carries its own.
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

// rwaFunnelContractsBasis describes the second arm. It is appended
// rather than folded into the sentence above because the two arms walk
// different populations: a reader who took one narrowing for the whole
// would conclude that an entity absent from the first was refused, when
// it was never in that population to begin with.
const rwaFunnelContractsBasis = "The `contract` arm walks a SEPARATE population: every entry in the curated " +
	"third-party directory, narrowed to the contract addresses it names with an issuing tag, then to the tokens " +
	"the definition admits. It exists because the issuers table the classic arm walks is populated only when a " +
	"CLASSIC asset is registered, so an entity whose Stellar presence is contract-issued is absent from that " +
	"population entirely rather than refused by any requirement. Stage counts reconcile WITHIN an arm; the two " +
	"arms meet only at the assets served. `directory_recognised_issuing_accounts` is a terminal census, not part " +
	"of the narrowing: it counts recognised issuing entities this index holds no token for, and names a bounded " +
	"sample of them in `unreached_entities`."

// rwaFunnelContractsUnmeasured is served instead when no directory
// contract reader is wired. The distinction it draws is the one this
// whole structure exists for: nothing was looked at, which is not the
// same finding as nothing was there.
const rwaFunnelContractsUnmeasured = "The `contract` arm was NOT MEASURED: no curated-directory contract reader " +
	"is wired, so that population was never walked. Its absence from the stages below is a configuration " +
	"statement, not a network with no contract-issued real-world assets in it."

// rwaFunnelUnavailableBasis is the funnel served when membership could
// not be established at all. It states the absence rather than
// publishing a narrowing of zeros, which would read as a measured
// population of nothing.
const rwaFunnelUnavailableBasis = "Not measured: the attestation scan or the curated account directory did not " +
	"answer, so no population was walked. A funnel of zeros would read as a network with nothing in it."

// rwaFunnelValuationBasis describes the third arm. It continues PAST
// the served set rather than narrowing toward it, and says so: a reader
// who took it for another membership narrowing would read a row with no
// oracle feed as an asset the definition refused, when it is an
// admitted member whose backing nobody independent prices.
const rwaFunnelValuationBasis = "The `valuation` arm continues PAST the served set: it accounts for which served rows carry a " +
	"REFERENCE-priced valuation — circulating supply times an independent oracle's value for the instrument — and " +
	"counts every row that carries none under the reason that refused it. Its drop reasons are the same strings the " +
	"rows carry in `reference_valuation.status`, read back off the rows rather than recomputed. It does NOT walk the " +
	"market-cap basis: that coverage is `summary.assets_valued` and `assets_unvalued`, and each of its refusals is " +
	"already a `valuation.status` on the row. Membership is decided before either valuation, so nothing in this arm " +
	"admits or refuses an asset."

// rwaFunnelUnavailable is the funnel for a response that publishes no
// set because membership could not be established.
func rwaFunnelUnavailable() RWAFunnel {
	return RWAFunnel{Stages: []RWAFunnelStage{}, Balanced: false, Basis: rwaFunnelUnavailableBasis}
}

// rwaFunnelOf builds the accounting from one rebuild's census, the
// catalogue join, and the number of rows actually served.
//
// The refusal tally is read back out of the membership rather than
// restated, so the funnel can never disagree with `refused[]` about how
// many candidates a requirement turned away.
func rwaFunnelOf(
	m rwaMembership, join rwaCatalogueJoin, served, contractsServed int, assets []RWAAsset,
) RWAFunnel {
	stages := rwaClassicStages(m, join, served)
	classicOK := m.census.Check() == ""
	// The contract arm is appended only when its population was actually
	// measured. A deployment with no directory contract reader wired has
	// not looked, and a run of contract stages reading zero would assert
	// that the curated directory names no real-world contracts — a
	// finding, from a scan that never ran. The funnel basis says which
	// of the two happened.
	contractOK := true
	if m.contractCensus.available {
		stages = append(stages, rwaContractStages(m, contractsServed)...)
		contractOK = m.contractCensus.dir.Check() == ""
	}
	stages = append(stages, rwaValuationStages(assets)...)
	// The two membership arms and the row list are three accountings of
	// the same served set, and only two of them are related by the
	// stage arithmetic — the arm boundary is unbridgeable, so nothing
	// would otherwise notice a row lost between the arms and the list
	// the valuation arm walks.
	servedOK := len(assets) == served+contractsServed
	return RWAFunnel{
		Stages: stages,
		// Four checks now: each arm's own census (the storage layer's
		// statement about its own numbers), the served-set agreement
		// above, and the stage arithmetic derived from them. The census
		// checks keep holding if the stage list is ever restructured —
		// which is exactly when a derived check quietly stops covering
		// something.
		Balanced: classicOK && contractOK && servedOK && rwaFunnelImbalance(stages) == "",
		Basis:    rwaFunnelBasisFor(m.contractCensus.available),
	}
}

// rwaValuationStages is the reference-valuation walk: the served set,
// down to the rows carrying a reference-priced figure.
//
// The drops are read back OFF the served rows, exactly as the candidate
// drops are read back out of the refusal tally rather than restated.
// That is what makes this one story instead of two: a reader who filters
// the rows by `reference_valuation.status` gets the counts printed here,
// and the funnel cannot develop its own opinion about why a figure is
// missing.
//
// Its arithmetic is NOT tautological, which is the point of deriving the
// two counts differently. The stage below counts rows carrying a figure
// (`value_usd` present); the drops count rows whose status is not
// published. They reconcile only if published and carrying-money are the
// same set of rows — so a row that claimed `published` with no money, or
// carried money under a refusal, reports the funnel unbalanced.
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
	RWAPremiumNotInstrumentScoped,
	RWAPremiumNotBound,
	RWAPremiumReferenceNotUSD,
	RWAPremiumNoReference,
	RWAPremiumReferenceExpired,
	RWAPremiumReferenceNotPositive,
	RWAReferenceValuationNoSupply,
}

// rwaReferenceDropActors names who can move each reference-valuation
// drop, on the same three-actor vocabulary the membership arms use.
//
// `operator` where somebody here can act: no oracle publishes the
// instrument (a source could be enabled), the read did not answer or
// has gone stale (an outage here), no supply reading covers the asset
// (a pipeline gap), or the curated contract-to-feed set is empty (a
// reviewer with a primary source can add an entry).
//
// `definition` where the rule is working and nobody should act: a
// flagged issuer, a pair no curated binding names — usually a token
// wearing an instrument's ticker — a feed that prices an ounce rather
// than a token, a value denominated in a reserve asset rather than
// dollars, and a non-positive value nothing may be multiplied by.
//
// No drop here is the ISSUER's: an issuer cannot make an oracle price
// its instrument, and attributing it to them would tell a reader to
// chase the one party who cannot fix it.
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
}

// rwaReferenceDrops renders the tallied refusals in rule order.
//
// A status with no entry in [rwaReferenceDropActors] is a defect here
// rather than in the data, so it is attributed to the OPERATOR — the
// only party who can fix a missing mapping — and reported rather than
// dropped. Dropping it would unbalance the funnel, which is the loudest
// possible signal but not an informative one; a test pins every status
// a row can carry to an actor so this path stays unreachable.
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

// rwaFunnelBasisFor states what was measured, including whether the
// contract arm was walked at all.
func rwaFunnelBasisFor(contractsMeasured bool) string {
	if !contractsMeasured {
		return rwaFunnelBasis + " " + rwaFunnelContractsUnmeasured + " " + rwaFunnelValuationBasis
	}
	return rwaFunnelBasis + " " + rwaFunnelContractsBasis + " " + rwaFunnelValuationBasis
}

// rwaClassicStages is the SEP-1 attestation walk: every issuer account
// that could carry an attestation, down to the classic rows served.
func rwaClassicStages(m rwaMembership, join rwaCatalogueJoin, served int) []RWAFunnelStage {
	c := m.census

	// Every issuer with an on-chain home_domain, less those holding no
	// attestation — and that gap is TWO findings with two different
	// owners, so it is reported as two drops.
	//
	// An issuer nothing has fetched yet is an operator's backlog: a
	// cron that has not reached that account, movable by running it. An
	// issuer whose domain WAS reached and served nothing storable is
	// the issuer's own publication: dead, parked, or serving no SEP-1
	// document. Nobody here can fetch a file that is not there.
	//
	// They were published as one bucket under the operator's name until
	// the drain that emptied the first one proved how few of them it
	// was: measured on production 2026-09-12, 40,838 domain-bearing
	// issuers held no payload and exactly ONE of them had never been
	// attempted. Naming all 40,838 an unfetched backlog overstated both
	// this index's reachable coverage and the operator's share of the
	// gap, on a page whose whole purpose is saying who can move a
	// number.
	//
	// What the split cannot say is WHY a reached domain served nothing:
	// no per-attempt outcome is stored, so a 404, a dead name, a TLS
	// failure and a document with no SEP-1 in it are one count. It is
	// attributed to the issuer because that is where the sampled
	// population lives; a transport fault at this end would land here
	// too, which is the reason the reason string says what was
	// observed rather than whose fault it was.
	//
	// Clamped at zero because a negative difference is nonsense on the
	// wire, and the fetched-but-empty count is clamped to the gap for
	// the same reason, with the never-attempted remainder taking what
	// is left. Neither clamp can hide the inconsistency that produced
	// one: the census Check() bounds the same two counts against the
	// same population independently of the stage arithmetic, and
	// Balanced requires both.
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
			// The unreadable bucket was a bare early return before the
			// census existed. It is the exact shape of a swallowed parse
			// error: a whole population could vanish through it and the
			// response would look the same as a quiet network.
			Stage: rwaStageIssuersWithSep1, Unit: rwaUnitIssuers, Count: c.IssuersWithPayload,
			Dropped: rwaDrops(
				RWAFunnelDrop{Reason: rwaDropPayloadUnreadable, Count: c.IssuersPayloadUnreadable, Actor: rwaActorIssuer},
				RWAFunnelDrop{Reason: rwaDropDeclaresNothing, Count: c.IssuersDeclaringNothing, Actor: rwaActorIssuer},
			),
		},
		{
			// Last stage counted in issuer accounts. The next one counts
			// the declarations these issuers publish, which is why this
			// stage exists rather than the unit changing silently under a
			// drop list.
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
			// Requirement 4's pre-filter runs on the bound entries before
			// any directory read, so its drop is a funnel stage. It is
			// ALSO in `refused[]` under requirement 4 — the one place
			// that tally reports out of R1→R4 order, and the reason both
			// views are served.
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
// It starts from a DIFFERENT root than the classic arm and shares no
// stage with it. That is the whole point: the entities this arm reaches
// are absent from the issuers table the classic arm walks, so no
// narrowing of that table could ever have found them.
func rwaContractStages(m rwaMembership, served int) []RWAFunnelStage {
	cc := m.contractCensus
	c := cc.dir

	stages := []RWAFunnelStage{
		{
			// The whole curated set, split by strkey form. An account
			// address names an ENTITY, which may issue many tokens or
			// none; a contract address names ONE token. The arm
			// identifies tokens, so the account rows leave here — under
			// `definition`, because it is the scope of the rule and not
			// a gap anybody can close by fetching something.
			//
			// The entities behind those account rows that matter are not
			// lost with them: the recognised, unflagged, tokenless ones
			// are counted at directory_recognised_issuing_accounts below
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
			// Unit changes from contracts to assets and the arithmetic
			// still reconciles, because here the mapping really is
			// one-to-one: a contract IS the asset it issues. That is not
			// true on the classic arm, where one issuer publishes many
			// declarations, which is why that arm has an exception and
			// this one does not.
			Stage: rwaStageContractCandidates, Unit: rwaUnitAssets, Count: cc.evaluated,
			Dropped: rwaContractCandidateDrops(m),
		},
		{
			Stage: rwaStageContractsAdmitted, Unit: rwaUnitAssets, Count: len(m.contracts),
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropNotInCatalogue, Count: m.contractsNotObserved, Actor: rwaActorIssuer,
			}),
		},
		{Stage: rwaStageContractsServed, Unit: rwaUnitAssets, Count: served},
		{
			// A TERMINAL stage, not part of the narrowing above it. It
			// counts account rows — the ones the first stage dropped —
			// filtered to the recognised, unflagged entities this index
			// holds no classic asset for.
			//
			// It carries no drops and nothing follows it, so it takes no
			// part in the stage arithmetic. That is deliberate: folding a
			// coverage census into a narrowing would make the funnel
			// close by adding a number that measures something else.
			Stage: rwaStageRecognisedIssuingAcct, Unit: rwaUnitDirectoryAddresses,
			Count: c.AccountsIssuingWithoutAsset,
		},
	}
	for i := range stages {
		stages[i].Arm = rwaArmContract
	}
	return stages
}

// rwaContractCandidateDrops is what happened to the contracts that
// reached the full ordered evaluation: the C4 refusal, then the
// structural drop that is not a refusal at all.
//
// C1 to C3 are decided by the scan itself and appear as stages above, so
// their refusal constants are unreachable here by construction — the
// same relationship the classic arm has with not_a_classic_asset and
// no_issuer_bound_sep1_entry.
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
			// The pre-filter's share is already a stage of its own
			// above. Counting it twice would stop the arithmetic
			// closing, and a funnel that does not close is the defect
			// this structure exists to prevent.
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

// rwaDrops keeps the drops that actually happened. A zero bucket is
// noise on every response with nothing to report there, and the
// arithmetic reads the same with it absent.
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
// Every adjacent pair must satisfy `count - sum(dropped) == next.count`,
// with exactly two stated exceptions:
//
//   - the ONE transition from issuer accounts to the declarations those
//     issuers publish. The two counts measure different things and no
//     subtraction relates them, so that pair is required to carry no
//     drops instead. Every other unit change is a relabelling of a
//     population that maps one-to-one — a bound declaration IS the
//     candidate asset it names — and still has to reconcile.
//   - the issuer_asset_page_truncated drop, which counts ISSUERS whose
//     asset tail went unread rather than assets. Letting it into the
//     asset arithmetic would make the funnel close by inventing a
//     number, which is worse than reporting that it does not.
//
// Surfaced through RWAFunnel.Balanced rather than raised as an error: a
// funnel that cannot be reconciled must SAY so. A reader who silently
// fails to make the numbers meet is exactly the outcome this structure
// exists to prevent.
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
// adjacent stages, and why. Such a pair is required to carry no drops
// instead of reconciling — a drop across one would have to be accounted
// for in a population it was never part of.
//
// There are exactly three, all stated:
//
//   - an ARM BOUNDARY. The arms narrow different populations from
//     different roots and meet only at the served set.
//   - the ONE transition from issuer accounts to the declarations those
//     issuers publish, where one issuer publishes many. Every other
//     change of unit is a relabelling of a population that maps one to
//     one — a bound declaration IS the candidate asset it names, a
//     contract IS the asset it issues — and still has to reconcile.
//   - the TERMINAL CENSUS stage, which counts a population an earlier
//     stage already dropped. Folding it into the narrowing would make
//     the funnel close by adding a number that measures something else.
func rwaUnbridgeablePair(cur, next RWAFunnelStage) (string, bool) {
	switch {
	case cur.Arm != next.Arm:
		return "is the last stage of the " + cur.Arm + " arm", true
	case next.Stage == rwaStageRecognisedIssuingAcct:
		return "precedes a terminal census stage that takes no part in the narrowing", true
	case cur.Unit == rwaUnitIssuers && next.Unit != rwaUnitIssuers:
		return "is the last stage counted in issuer accounts", true
	}
	return "", false
}
