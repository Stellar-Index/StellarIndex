package rwa

import (
	"fmt"
	"strings"
	"time"
)

// VettedISINBinding is an ISIN transcribed by hand from the issuer's own SEP-1 for one exact
// (code, issuer) whose entry names the instrument only in prose the served tier does not keep.
// It admits under [BasisSep1ISINVetted] and is served on the row with the page it was read
// from and the date it falls due for review.
type VettedISINBinding struct {
	Code       string
	Issuer     string
	ISIN       string
	Instrument string // the issuer's own name for the instrument
	Source     string // the issuer-bound SEP-1 the ISIN was transcribed from
	VerifiedOn string // calendar date the page was read
	ReviewBy   string // derived: VerifiedOn plus VettedISINReviewInterval
}

// VettedISINReviewInterval is the constant-NAV quarter: both tables are hand transcriptions of an issuer page and go stale the same way.
const VettedISINReviewInterval = ConstantNAVReviewInterval

var vettedISINBindings = []VettedISINBinding{
	// Bitbond Finance Tokenized Bond 1: the bound entry declares no anchor_asset_type and no
	// anchor_asset, and names its ISIN only in desc, which the served tier does not store.
	{
		Code: "BB1", Issuer: "GD5J6HLF5666X4AZLTFTXLY46J5SW7EXRKBLEYPJP33S33MXZGV6CWFN", ISIN: "DE000A2TR7Q2",
		Instrument: "Bitbond Finance Tokenized Bond 1",
		Source:     "https://bitbondsto.com/.well-known/stellar.toml", VerifiedOn: "2026-10-13",
	},
}

// vettedISINIndex is built once; a defective table is a build error, not a runtime surprise.
var vettedISINIndex = func() vettedISINTable {
	t, err := indexVettedISIN(vettedISINBindings)
	if err != nil {
		panic(err)
	}
	return t
}()

type vettedISINTable struct {
	byPair map[instrumentKey]VettedISINBinding
	byISIN map[string]VettedISINBinding
}

// indexVettedISIN validates the table and stamps each ReviewBy; a pair or an ISIN bound twice is refused.
func indexVettedISIN(bindings []VettedISINBinding) (vettedISINTable, error) {
	t := vettedISINTable{
		byPair: make(map[instrumentKey]VettedISINBinding, len(bindings)),
		byISIN: make(map[string]VettedISINBinding, len(bindings)),
	}
	for i, b := range bindings {
		reviewBy, err := reviewByDate(b.VerifiedOn, VettedISINReviewInterval)
		if err != nil {
			return vettedISINTable{}, fmt.Errorf("vetted ISIN binding %s: %w", b.Code, err)
		}
		if norm, ok := upperASCII12(b.ISIN); !ok || norm != b.ISIN || !IsISIN(b.ISIN) {
			return vettedISINTable{}, fmt.Errorf("vetted ISIN binding %s: ISIN %q is not a well-formed upper-case ISIN", b.Code, b.ISIN)
		}
		key := instrumentKey{code: b.Code, issuer: b.Issuer}
		if prev, dup := t.byPair[key]; dup {
			return vettedISINTable{}, fmt.Errorf("vetted ISIN binding %s: pair %s-%s bound twice (ISINs %s and %s)", b.Code, b.Code, b.Issuer, prev.ISIN, b.ISIN)
		}
		if prev, dup := t.byISIN[b.ISIN]; dup {
			return vettedISINTable{}, fmt.Errorf("vetted ISIN binding %s: ISIN %s already bound to %s-%s", b.Code, b.ISIN, prev.Code, prev.Issuer)
		}
		b.ReviewBy = reviewBy
		bindings[i] = b
		t.byPair[key] = b
		t.byISIN[b.ISIN] = b
	}
	return t, nil
}

// ReviewDeadline is the start of the ReviewBy date, UTC; an unparseable date is the zero time so a hand-built binding fails closed as already due.
func (b VettedISINBinding) ReviewDeadline() time.Time {
	return reviewDeadline(b.ReviewBy)
}

// ReviewDue reports whether the binding is strictly past its review deadline at now.
func (b VettedISINBinding) ReviewDue(now time.Time) bool {
	return now.After(b.ReviewDeadline())
}

// VettedISIN returns the hand-vetted ISIN binding for this exact (code, issuer), and whether one exists.
func VettedISIN(code, issuer string) (VettedISINBinding, bool) {
	b, ok := vettedISINIndex.byPair[instrumentKey{code: strings.TrimSpace(code), issuer: strings.TrimSpace(issuer)}]
	return b, ok
}

// VettedISINBindings returns the table in declaration order, for documentation surfaces and tests.
func VettedISINBindings() []VettedISINBinding {
	out := make([]VettedISINBinding, len(vettedISINBindings))
	copy(out, vettedISINBindings)
	return out
}
