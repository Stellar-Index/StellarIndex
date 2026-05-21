package defindex

import (
	"github.com/RatesEngine/rates-engine/internal/consumer"
	"github.com/RatesEngine/rates-engine/internal/events"
)

// Decoder implements dispatcher.Decoder (the event-based variant —
// not ContractCallDecoder). DeFindex contracts publish Soroban
// contract events on every capital flow at both layers:
//
//   - Vault wrappers: `("DeFindexVault","deposit"|"withdraw")` —
//     end-user (G-strkey) attribution.
//   - Blend strategies: `("BlendStrategy","deposit"|"withdraw"|…)` —
//     vault↔strategy capital movement (`from` = vault C-strkey).
//
// We match both. Dispatch is by TOPIC, not a hand-curated contract
// set — any contract emitting either topic shape is decoded. This
// mirrors the comet/aquarius shared-emitter topology and is what
// the granular-coverage mission wants — every DeFindex
// instance (the 100+ wrappers the factory has spawned over its
// life, not just the 7 currently advertised on defindex.io).
//
// Stateless. Matching is O(1) — two byte-equal topic compares per
// layer before any SCVal parsing.
type Decoder struct{}

// NewDecoder constructs a topic-matched DeFindex event decoder.
// No arguments — matching is purely on the two layer-prefix
// topic shapes ("BlendStrategy" / "DeFindexVault").
func NewDecoder() *Decoder { return &Decoder{} }

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Cheap predicate: the
// topic shape is a BlendStrategy or DeFindexVault deposit/withdraw.
// The dispatcher only calls Decode() when this returns true.
func (d *Decoder) Matches(ev events.Event) bool {
	return classify(&ev) != "" || classifyVault(&ev) != ""
}

// Decode implements [dispatcher.Decoder]. Emits one Event per
// matched flow — Event (strategy layer) or VaultEvent (vault
// wrapper layer) depending on the topic prefix. Returning an error
// is a "skip + count" signal per the dispatcher's contract: a
// malformed event doesn't abort the ledger, just gets dropped +
// counted.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if kind := classify(&ev); kind != "" {
		flow, err := decodeFlow(&ev, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{Event{Flow: flow}}, nil
	}
	if kind := classifyVault(&ev); kind != "" {
		flow, err := decodeVaultFlow(&ev, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{VaultEvent{Flow: flow}}, nil
	}
	// Defensive — Matches should have filtered.
	return nil, ErrUnknownEvent
}
