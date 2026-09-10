package v1

import (
	"fmt"

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

// rwaFunnelUnavailableBasis is the funnel served when membership could
// not be established at all. It states the absence rather than
// publishing a narrowing of zeros, which would read as a measured
// population of nothing.
const rwaFunnelUnavailableBasis = "Not measured: the attestation scan or the curated account directory did not " +
	"answer, so no population was walked. A funnel of zeros would read as a network with nothing in it."

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
func rwaFunnelOf(m rwaMembership, join rwaCatalogueJoin, served int) RWAFunnel {
	c := m.census

	// Every issuer with an on-chain home_domain, less those with no
	// fetched attestation. This is the coverage number an OPERATOR
	// moves, and it is invisible everywhere else on this surface: from
	// inside the pipeline an issuer the refresh cron has never reached
	// is indistinguishable from one that declares nothing.
	//
	// Clamped at zero because a negative difference is nonsense on the
	// wire. The clamp cannot hide the inconsistency that produced one:
	// the two counts then differ with no drop between them, and the
	// stage arithmetic below reports the funnel as unbalanced.
	neverFetched := max(c.IssuersWithHomeDomain-c.IssuersWithPayload, 0)

	stages := []RWAFunnelStage{
		{
			Stage: rwaStageIssuersWithHomeDomain, Unit: rwaUnitIssuers, Count: c.IssuersWithHomeDomain,
			Dropped: rwaDrops(RWAFunnelDrop{
				Reason: rwaDropNoAttestation, Count: neverFetched, Actor: rwaActorOperator,
			}),
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

	return RWAFunnel{
		Stages: stages,
		// Both checks, though the stage arithmetic is derived from these
		// same counts and the two agree today. The census check is the
		// storage layer's own statement about its own numbers and keeps
		// holding if the stage list is ever restructured — which is
		// exactly when a derived check quietly stops covering something.
		Balanced: c.Check() == "" && rwaFunnelImbalance(stages) == "",
		Basis:    rwaFunnelBasis,
	}
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
		cur, next := stages[i], stages[i+1]
		if cur.Unit == rwaUnitIssuers && next.Unit != rwaUnitIssuers {
			if len(cur.Dropped) > 0 {
				return fmt.Sprintf("%s is the last stage counted in issuer accounts and also drops — "+
					"a drop across that change of unit cannot be reconciled", cur.Stage)
			}
			continue
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
	}
	return ""
}
