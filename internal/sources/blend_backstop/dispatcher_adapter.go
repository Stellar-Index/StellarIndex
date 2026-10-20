package blend_backstop

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing, stateless view of the Blend Backstop: each of the twelve
// events decodes independently into one blend_backstop_events row, with no cross-event
// correlation.
//
// Matching is by topic[0] symbol AND contract id. Backstop symbols (claim / withdraw /
// queue_withdrawal / gulp_emissions) OVERLAP with Blend POOL event symbols, so the contract-id
// gate is LOAD-BEARING (docs/architecture/domain-traps.md "Comet shared topic", ADR-0035).
type Decoder struct{}

// NewDecoder constructs a Backstop Decoder. Stateless — the returned
// value is safe to share.
func NewDecoder() *Decoder { return &Decoder{} }

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// backstopContracts is the set of contract C-strkeys this decoder claims: the V2 singleton and the V1 deployment before it (a
// backfill range may replay either). A redeploy is operator-visible, so a hard-coded set is right.
var backstopContracts = map[string]struct{}{
	MainnetBackstopV2: {},
	MainnetBackstopV1: {},
}

// IsBackstopContract reports whether id is one of the known Blend
// Backstop contracts on Stellar mainnet (V1 or V2).
func IsBackstopContract(id string) bool {
	_, ok := backstopContracts[id]
	return ok
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Claims an event when its topic[0] is one of the twelve backstop symbols AND it was
// emitted by a known backstop contract. The contract gate disambiguates the symbol-overlap with Blend pool events.
func (*Decoder) Matches(ev events.Event) bool {
	return IsBackstopContract(ev.ContractID) && Classify(&ev) != ""
}

// Decode implements [dispatcher.Decoder]. It emits one [Event] per recognised backstop event and
// nothing otherwise, re-checking what Matches filtered so direct callers are safe. A decode
// error is non-fatal: counted and skipped.
func (*Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if !IsBackstopContract(ev.ContractID) || Classify(&ev) == "" {
		return nil, nil
	}
	return project(&ev)
}
