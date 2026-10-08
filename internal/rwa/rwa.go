// Package rwa defines which Stellar assets the RWA surface admits and why.
//
// A permissive rule is a phishing amplifier, not a longer dashboard: anyone may issue USTRY,
// BENJI or XAU, so identity is always (code, issuer). [Qualify] requires all four:
//
// R1 IDENTITY: a classic asset with an issuer G-address (contract tokens use contract.go's
// C1-C4 arm, keyed by contract address; the arms never overlap).
//
// R2 ISSUER-BOUND SELF-DECLARATION: the SEP-1 fetched from the account's on-chain
// home_domain has a [[CURRENCIES]] entry for this code naming this account as issuer.
//
// R3 INDEPENDENT RECOGNITION: the curated account directory tags the issuer (or an unflagged
// domain sibling) with a recognition tag and no scam tag. 128 of 130 issuers self-declaring
// a real-world anchor were tagged malicious, so R2 alone is worthless.
//
// R4 REAL-WORLD INSTRUMENT: [BasisSep1Anchor], [BasisOracleFeed] or [BasisSep1ISIN], in
// that order; never load-bearing alone, which keeps the code-keyed oracle basis safe.
//
// A failing asset is simply absent here; its own asset page still serves it.
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
	// BasisOracleFeed: an ADR-0028 oracle publishes a NAV feed for this CODE, admissible
	// only because R3 already bound the issuer. A vetted fund-NAV binding ([FundNAVTicker])
	// also admits here, keyed on the exact pair.
	BasisOracleFeed = "oracle_rwa_feed"
	// BasisSep1ISIN: the bound entry's anchor_asset is a well-formed ISIN. Admits without a
	// class; an agency-assigned, checkable ISIN beats a self-picked class string, and entitlement
	// to it is left to R2 and R3, which an impersonator must defeat first.
	BasisSep1ISIN = "sep1_isin_declaration"
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

// anchorClasses is SEP-1's anchor_asset_type enumeration minus terms naming no real-world
// instrument (fiat is a stablecoin, crypto/nft are not RWAs, other classifies nothing).
// No synonyms: accepting invented spellings is how a closed set stops being closed.
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

// contractAnchorClasses is [anchorClasses] plus `fund`. A curated binding's class is our
// own reviewed statement, not a declaration read, so it may use terms SEP-1 lacks; `fund`
// exists because the Spiko Amundi swap fund is neither `stock` nor `bond` in exposure.
// Widening it admits nothing: C1-C3 still decide membership.
var contractAnchorClasses = func() map[string]struct{} {
	out := make(map[string]struct{}, len(anchorClasses)+1)
	for c := range anchorClasses {
		out[c] = struct{}{}
	}
	out["fund"] = struct{}{}
	return out
}()

// ContractAnchorClasses lists the contract-binding vocabulary in a
// stable order, served beside [AnchorClasses] so a consumer can see that
// the two arms do not answer with the same words and why a contract row
// may carry a class no SEP-1 declaration could.
func ContractAnchorClasses() []string {
	out := make([]string, 0, len(contractAnchorClasses))
	for c := range contractAnchorClasses {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
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

// recognitionTags are directory tags vouching for an account as an ISSUER (R3); tags like
// `wallet` or `memo-required` describe without vouching. A scam tag always wins.
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
	// DeclaredAnchorAsset is the bound entry's anchor_asset, verbatim; kept apart from the type
	// because an issuer may answer one usefully and not the other (Franklin: `other` + ISINs).
	DeclaredAnchorAsset string
	// DirectoryTags are the curated third-party tags on the issuer
	// G-address. Empty when the directory does not list it, which is a
	// refusal under R3 and not an error.
	DirectoryTags []string
	// SiblingRecognised stands in for R3 when another account on this issuer-bound domain is
	// recognised and unflagged. It never overrides a scam flag or supplies an instrument claim.
	SiblingRecognised bool
}

// Verdict is the definition applied to one candidate.
type Verdict struct {
	// InSet is the membership decision.
	InSet bool
	// Basis names which R4 arm admitted the asset. Empty when refused.
	Basis string
	// AnchorClass is the closed-vocabulary class when [BasisSep1Anchor]
	// admitted it. Empty under [BasisOracleFeed] and [BasisSep1ISIN]:
	// both name an instrument rather than a class, and inventing one
	// would publish a classification nothing declared.
	AnchorClass string
	// Reject names the FIRST requirement the candidate failed, in R1→R4
	// order. Empty when admitted.
	Reject string
	// Recognition names which route satisfied recognition, set on every admission. Each arm
	// has two routes of unequal weight (direct attestation vs inference), and a row that could
	// not say which would serve two evidence strengths as one membership.
	Recognition string
}

// Qualify applies the requirements in order; the reported reason is the requirement that
// would have had to change first.
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
	recognition := RecognitionDirectory
	if !HasRecognitionTag(c.DirectoryTags) {
		if !c.SiblingRecognised {
			return Verdict{Reject: RejectNoRecognition}
		}
		recognition = RecognitionDomainSibling
	}
	if class := AnchorClass(c.DeclaredAnchorType); class != "" {
		return Verdict{InSet: true, Basis: BasisSep1Anchor, AnchorClass: class, Recognition: recognition}
	}
	// The oracle arm matches on code the way the SEP-1 overlay matches
	// a [[CURRENCIES]] code — case-insensitively — because R3 has
	// already bound the issuer and a case variant of the ticker of a
	// recognised entity is that same instrument.
	if isOracleRWACode(c.Code) {
		return Verdict{InSet: true, Basis: BasisOracleFeed, Recognition: recognition}
	}
	// A hand-vetted fund-NAV binding is the same evidence keyed tighter:
	// an independent NAV feed for exactly this (code, issuer).
	if _, ok := FundNAVTicker(c.Code, c.Issuer); ok {
		return Verdict{InSet: true, Basis: BasisOracleFeed, Recognition: recognition}
	}
	// The ISIN arm is last: strongest in identity but tells no class, and carries only the
	// issuer's own declaration, checked for form.
	if IsISIN(c.DeclaredAnchorAsset) {
		return Verdict{InSet: true, Basis: BasisSep1ISIN, Recognition: recognition}
	}
	return Verdict{Reject: RejectNoInstrumentClaim}
}

// How R3 was satisfied for a classic member, served on the row.
const (
	// RecognitionDirectory — the curated account directory lists THIS
	// issuer account with a recognition tag. The original arm.
	RecognitionDirectory = "curated_account_directory"
	// RecognitionDomainSibling: the directory lists (unflagged) another account the same
	// issuer-bound SEP-1 declares, e.g. Franklin's gBENJI/grBENJI/sgBENJI beside BENJI.
	// It ASSUMES one entity per domain; a multi-tenant toml host would let one recognised
	// tenant pass every other through R3. No such domain has a recognised tenant today.
	RecognitionDomainSibling = "curated_account_directory_via_domain_sibling"
)

// isOracleRWACode reports whether the ADR-0028 allow-list has a feed for this code,
// case-insensitively, since tickers (XAUm, iBENJI) and on-chain codes differ in case.
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

// CouldQualify is the asset-side pre-filter for R4, never a decision. It must read every
// input Qualify's R4 arms read, or it silently changes membership (Franklin's `other` + ISIN
// classes); TestCouldQualify_MatchesQualifyOnTheAssetSideInputs pins that.
func CouldQualify(code, declaredAnchorType, declaredAnchorAsset string) bool {
	return AnchorClass(declaredAnchorType) != "" ||
		isOracleRWACode(code) ||
		IsISIN(declaredAnchorAsset)
}
