package redstone

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the RedStone adapter: one instance per indexer since one adapter contract covers every feed. Stateless; feed attribution is decoded per event from the InvokeContract args in events.Event.OpArgs (see docs/protocols/redstone.md).
type Decoder struct {
	contractID string
}

// NewDecoder constructs a Decoder bound to the adapter's mainnet contract; other contracts (per-feed proxies, unrelated deployments) are rejected at Matches time.
func NewDecoder(adapterContractID string) *Decoder {
	return &Decoder{contractID: adapterContractID}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// StateWriteContracts implements the dispatcher's optional StateWriteKeyConsumer so value-changed keys are resolved for this contract only (resolveFeedAttribution uses events.Event.StateWriteKeys for subset attribution and equal-arity corroboration); the walk is skipped for every other op.
func (d *Decoder) StateWriteContracts() []string { return []string{d.contractID} }

// Matches implements [dispatcher.Decoder], gating on contract identity then topic[0], with no SCVal parsing on the hot path.
func (d *Decoder) Matches(ev events.Event) bool {
	if ev.ContractID != d.contractID {
		return false
	}
	return classify(&ev)
}

// Decode implements [dispatcher.Decoder], returning one UpdateEvent per (feed_id, price) entry in updated_feeds minus slots decodeWritePrices drops (non-positive price, or a feed_id not representable even as raw:).
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Fail closed rather than substituting time.Now(): closedAt is the fallback
		// canonical.SafeUnixMillis uses when PackageTimestamp is 0 or out of its sanity window,
		// so a wall-clock value would mis-timestamp the row during a backfill replay.
		return nil, err
	}
	updates, err := decodeWritePrices(&ev, closedAt)
	if err != nil {
		return nil, err
	}
	out := make([]consumer.Event, 0, len(updates))
	for _, u := range updates {
		out = append(out, UpdateEvent{Update: u})
	}
	return out, nil
}
