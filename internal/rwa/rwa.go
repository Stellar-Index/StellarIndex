// Package rwa holds the definition of a tokenized real-world asset on
// the Stellar network: which on-chain assets the RWA surface admits,
// and why each one qualifies.
//
// The definition is the load-bearing part of the surface. An RWA page
// names issuers as representing real-world value and publishes
// valuations against them, so a permissive rule does not produce a
// longer dashboard — it produces a phishing amplifier. Asset codes are
// not unique on Stellar: anyone may issue a token called USTRY, BENJI
// or XAU, and the network holds many that impersonate exactly those.
// Identity here is therefore always (code, issuer), never a code alone
// (the same rule the verified-currency collision warning and
// [canonical.Asset] enforce).
//
// # The four requirements
//
// An asset is in the set when ALL of these hold. Each is checked by
// one of the predicates below; [Qualify] composes them and returns the
// reason a candidate was refused.
//
// R1 — IDENTITY. A classic Stellar asset with a code and an issuer
// G-address. The native asset is not an RWA. Soroban-only contracts
// are out of scope: SEP-1 [[CURRENCIES]] binds a declaration to a
// (code, issuer) pair, and a bare C-address has no such binding, so
// admitting one would mean admitting an unbound claim.
//
// R2 — ISSUER-BOUND SELF-DECLARATION. The issuer account carries a
// SEP-1 stellar.toml, fetched over HTTPS from the home_domain that
// account set ON CHAIN, containing a [[CURRENCIES]] entry whose code
// matches the asset and whose declared issuer equals the account that
// served the file. This is the same provenance rule the SEP-1 logo
// overlay enforces (timescale.AllSep1Images) after a token was able to
// claim another issuer's brand by declaring it in its own toml. It
// proves the claim came from a domain the issuer controls, bound to
// this exact strkey — nothing more, and the next requirement exists
// because nothing more is a low bar.
//
// R3 — INDEPENDENT RECOGNITION. The issuer G-address is named in the
// curated third-party account directory (migration 0136) with at least
// one recognition tag and no scam-class tag. A self-declaration alone
// is worthless here: measured on the production directory 2026-09-05,
// of the 130 issuers publishing a domain-bound real-world
// anchor_asset_type, 128 carry the `malicious` tag — the declarations
// come overwhelmingly from lookalike domains impersonating real
// exchanges. R3 is the requirement a party other than the issuer
// vouched for that specific account.
//
// R4 — REAL-WORLD INSTRUMENT. The asset is a real-world instrument
// rather than one of the issuer's other tokens, established either by
// [BasisSep1Anchor] or by [BasisOracleFeed]. R4 classifies within an
// issuer R3 has already vouched for; it is never load-bearing on its
// own, which is what keeps the code-keyed oracle basis safe.
//
// # What a failing asset gets
//
// Nothing. It is absent from this surface, and its own asset page
// continues to serve it under the gates that already apply there. The
// set is a claim about real-world backing; an asset that cannot meet
// the bar has no partial place in it.
package rwa

import (
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Basis names WHY an asset is classified as a real-world instrument
// (R4). It is served on every row so a consumer can filter the set
// down to the strength of evidence it needs rather than trusting the
// membership decision wholesale.
const (
	// BasisSep1Anchor — the issuer-bound SEP-1 entry from R2 declares
	// an anchor_asset_type naming a real-world instrument class.
	BasisSep1Anchor = "sep1_anchor_declaration"
	// BasisOracleFeed — an independent price oracle publishes a
	// net-asset-value feed for an instrument of this code, per the
	// ADR-0028 allow-list (canonical.IsKnownRWA). The feed is keyed on
	// the CODE alone, which is exactly why this basis is admissible
	// only after R3 has bound the issuer to a recognised entity: on its
	// own it would readmit the code-only identity the whole definition
	// refuses.
	BasisOracleFeed = "oracle_rwa_feed"
)

// Reject names why a candidate is not in the set. Served on the
// aggregate so the surface can state how many assets each requirement
// turned away rather than presenting the admitted set as the whole
// population.
const (
	RejectNotClassic        = "not_a_classic_asset"
	RejectNoBoundSep1       = "no_issuer_bound_sep1_entry"
	RejectScamFlagged       = "issuer_scam_flagged"
	RejectNoRecognition     = "issuer_not_independently_recognised"
	RejectNoInstrumentClaim = "no_real_world_instrument_basis"
)

// anchorClasses is the closed vocabulary of anchor_asset_type values
// that name a real-world instrument.
//
// It is the SEP-1 enumeration itself, minus the terms that name no
// real-world instrument. anchor_asset_type is free text on the wire —
// the production set holds `equity`, `etf`, `metal`, `rwa`,
// `real_estate`, `sovereign` and dozens more invented spellings — and
// accepting synonyms is how a closed set stops being closed. A token
// whose issuer wants to appear here declares one of these four.
//
//   - `fiat` is excluded: a fiat-anchored token is a stablecoin, a
//     different instrument with a different risk story, counted on its
//     own surface (the comparison dashboards keep the two apart too).
//   - `crypto` and `nft` are excluded: neither is a real-world asset.
//   - `other` is excluded: it classifies nothing, and 6,309 bound
//     entries carry it.
var anchorClasses = map[string]struct{}{
	"stock":      {},
	"bond":       {},
	"commodity":  {},
	"realestate": {},
}

// AnchorClass normalises a declared SEP-1 anchor_asset_type to its
// closed-vocabulary form, or "" when the declaration names no
// real-world instrument class. Case and surrounding space are
// insignificant; nothing else is folded.
func AnchorClass(declared string) string {
	c := strings.ToLower(strings.TrimSpace(declared))
	if _, ok := anchorClasses[c]; !ok {
		return ""
	}
	return c
}

// AnchorClasses lists the closed vocabulary in a stable order. The
// API serves it so a consumer reads the rule from the response rather
// than inferring it from the rows present on the day.
func AnchorClasses() []string {
	out := make([]string, 0, len(anchorClasses))
	for c := range anchorClasses {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// recognitionTags is the curated-directory vocabulary that counts as
// an independent party recognising an account as an ISSUING entity
// (R3).
//
// Deliberately narrower than "has any tag": `personal`, `wallet`,
// `memo-required`, `airdrop`, `application` and `infra` describe an
// account without vouching for it as the issuer of a real-world
// instrument, and `memo-required` in particular is an operational
// note that any account can attract. The scam-class tags are handled
// separately and always exclude — a scam tag beats every recognition
// tag on the same account.
var recognitionTags = map[string]struct{}{
	"issuer":    {},
	"anchor":    {},
	"custodian": {},
	"exchange":  {},
	"defi":      {},
	"sdf":       {},
}

// RecognitionTags lists the recognition vocabulary in a stable order,
// for the same reason [AnchorClasses] is served.
func RecognitionTags() []string {
	out := make([]string, 0, len(recognitionTags))
	for t := range recognitionTags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// HasRecognitionTag reports whether any tag vouches for the account as
// an issuing entity. Matched case-insensitively on trimmed tags, the
// same way the scam classifier reads the same column.
func HasRecognitionTag(tags []string) bool {
	for _, t := range tags {
		if _, ok := recognitionTags[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// ScamFlagged reports whether any tag is scam-class. It reads the ONE
// vocabulary in timescale.DirectoryScamFlagTags rather than restating
// it, so this package can never admit an issuer whose price the
// serving gates withhold.
func ScamFlagged(tags []string) bool {
	for _, t := range tags {
		lt := strings.ToLower(strings.TrimSpace(t))
		for _, flag := range timescale.DirectoryScamFlagTags {
			if lt == flag {
				return true
			}
		}
	}
	return false
}

// Candidate is one asset put to the definition, with every input the
// four requirements read. It carries no valuation: membership is
// decided before a number is attached, so a thin or withheld market
// can never change who is in the set.
type Candidate struct {
	// Code and Issuer are the on-chain (code, issuer) identity.
	Code   string
	Issuer string
	// BoundSep1 reports whether the issuer served a SEP-1
	// [[CURRENCIES]] entry for this code whose declared issuer equals
	// the serving account (R2). The caller establishes it; this package
	// never assumes a match it did not see.
	BoundSep1 bool
	// DeclaredAnchorType is the anchor_asset_type from that bound
	// entry, verbatim. Empty when the entry declares none.
	DeclaredAnchorType string
	// DirectoryTags are the curated third-party tags on the issuer
	// G-address. Empty when the directory does not list it, which is a
	// refusal under R3 and not an error.
	DirectoryTags []string
}

// Verdict is the definition applied to one candidate.
type Verdict struct {
	// InSet is the membership decision.
	InSet bool
	// Basis names which R4 arm admitted the asset. Empty when refused.
	Basis string
	// AnchorClass is the closed-vocabulary class when [BasisSep1Anchor]
	// admitted it. Empty under [BasisOracleFeed]: an oracle feed names
	// an instrument, not its class, and inventing one would publish a
	// classification nothing declared.
	AnchorClass string
	// Reject names the FIRST requirement the candidate failed, in R1→R4
	// order. Empty when admitted.
	Reject string
}

// Qualify applies the four requirements in order and returns the
// verdict. The order matters for the reported reason: an unrecognised
// issuer whose token also declares nothing is reported as
// unrecognised, because recognition is the requirement that would have
// had to change first.
func Qualify(c Candidate) Verdict {
	if strings.TrimSpace(c.Code) == "" || strings.TrimSpace(c.Issuer) == "" {
		return Verdict{Reject: RejectNotClassic}
	}
	if !c.BoundSep1 {
		return Verdict{Reject: RejectNoBoundSep1}
	}
	// Scam before recognition: an account carrying both a recognition
	// tag and a scam-class tag is refused as flagged, which is the
	// stronger and more useful statement.
	if ScamFlagged(c.DirectoryTags) {
		return Verdict{Reject: RejectScamFlagged}
	}
	if !HasRecognitionTag(c.DirectoryTags) {
		return Verdict{Reject: RejectNoRecognition}
	}
	if class := AnchorClass(c.DeclaredAnchorType); class != "" {
		return Verdict{InSet: true, Basis: BasisSep1Anchor, AnchorClass: class}
	}
	// The oracle arm matches on code the way the SEP-1 overlay matches
	// a [[CURRENCIES]] code — case-insensitively — because R3 has
	// already bound the issuer and a case variant of the ticker of a
	// recognised entity is that same instrument.
	if isOracleRWACode(c.Code) {
		return Verdict{InSet: true, Basis: BasisOracleFeed}
	}
	return Verdict{Reject: RejectNoInstrumentClaim}
}

// isOracleRWACode reports whether an independent oracle publishes a
// net-asset-value feed for an instrument of this code, per the
// ADR-0028 allow-list. Case-insensitive: the allow-list spells codes
// as the instrument tickers (XAUm, iBENJI, deJAAA) while an on-chain
// asset code carries whatever case its issuer chose.
func isOracleRWACode(code string) bool {
	code = strings.TrimSpace(code)
	if canonical.IsKnownRWA(code) {
		return true
	}
	for _, known := range canonical.KnownRWACodes() {
		if strings.EqualFold(known, code) {
			return true
		}
	}
	return false
}

// CouldQualify reports whether an asset with this code and declared
// anchor type could satisfy requirement 4 at all, independent of the
// issuer. It exists so a caller scanning every issuer-bound SEP-1
// attestation can drop the overwhelming majority — the NFT, crypto and
// undeclared entries — without materialising them, and it is
// deliberately the ONLY predicate that answers a membership question
// from asset-side inputs alone. It is a pre-filter, never a decision:
// [Qualify] still has to run, and requirements 2 and 3 still have to
// hold.
func CouldQualify(code, declaredAnchorType string) bool {
	return AnchorClass(declaredAnchorType) != "" || isOracleRWACode(code)
}
