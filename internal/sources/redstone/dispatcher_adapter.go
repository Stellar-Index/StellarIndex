package redstone

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the RedStone adapter: one instance per indexer, since a
// single adapter contract covers every feed. It is stateless; feed attribution is decoded per
// event from the InvokeContract args in events.Event.OpArgs (see docs/protocols/redstone.md).
type Decoder struct {
	contractID string
}

// NewDecoder constructs a Redstone Decoder bound to the adapter's mainnet contract address. Other
// contracts (per-feed proxies, unrelated deployments) are rejected at Matches time; they do not
// emit REDSTONE events.
func NewDecoder(adapterContractID string) *Decoder {
	return &Decoder{contractID: adapterContractID}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// StateWriteContracts implements the dispatcher's optional StateWriteKeyConsumer interface.
// resolveFeedAttribution reads events.Event.StateWriteKeys for exact subset attribution and
// equal-arity corroboration, so the dispatcher resolves value-changed keys for this contract
// only; the walk is skipped for every other op.
func (d *Decoder) StateWriteContracts() []string { return []string{d.contractID} }

// Matches implements [dispatcher.Decoder]. It gates on contract identity, then topic[0], with no
// SCVal parsing on the hot path.
func (d *Decoder) Matches(ev events.Event) bool {
	if ev.ContractID != d.contractID {
		return false
	}
	return classify(&ev)
}

// Decode implements [dispatcher.Decoder]. Returns one UpdateEvent per (feed_id, price) entry in
// updated_feeds, minus slots decodeWritePrices drops (non-positive price, or a feed_id the record
// layer cannot represent even as raw:).
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
