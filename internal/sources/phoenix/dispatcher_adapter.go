package phoenix

import (
	"sync"
	"time"

	"github.com/StellarIndex/stellar-index/internal/contractid"

	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/events"
)

// Decoder is the dispatcher-facing view of Phoenix. Unlike
// Reflector or Aquarius, Phoenix is stateful: one swap produces 8
// separate events that must be correlated by (ledger, tx_hash,
// op_index) before a canonical.Trade can be emitted. The Decoder
// owns the correlation buffer.
//
// Serial-call assumption: per docs/architecture/ingest-pipeline.md
// the dispatcher processes events in order. Decode is not
// re-entrant. The mutex below is belt-and-braces for the rare case
// an operator runs parallel ledger replay (not a current feature
// but cheap insurance).
type Decoder struct {
	mu  sync.Mutex
	buf *buffer

	// evictedOrphans is incremented every time the buffer drops an
	// incomplete RawSwap (aged past defaultOrphanMaxAge). The
	// dispatcher reads this via the optional `EvictedOrphans() int`
	// interface (see internal/dispatcher/dispatcher.go::Stats) and
	// the indexer reports the running counts as
	// obs.SourceOrphanEventsTotal in the per-ledger stats path
	// (internal/pipeline/processor.go).
	evictedOrphans int
	// reg gates Matches() on contract identity (ADR-0035/0040).
	reg *contractid.Registry
}

// NewDecoder constructs a Phoenix Decoder with a fresh buffer.
// NewDecoder constructs a phoenix Decoder. Contract-identity gating
// (ADR-0035/0040): the curated mainnet set (pools + stake contracts,
// docs/protocols/phoenix.md) is ALWAYS seeded — the factory's
// creation events predate the lake, so unlike blend the in-code seed
// is the trust root, not a warm-start. Caller opts layer the
// protocol_contracts DB warm + live-upsert hook on top (harmless
// no-ops today; load-bearing if the factory ever emits a creation
// event we can decode).
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetFactory}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{buf: newBuffer(), reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Phoenix emits its
// per-action events with topic[0] = String(<action>). The second
// topic slot carries the field name; the buffer routes it
// internally. The claimed actions:
//
//   - swap — TWO on-wire shapes: the legacy 8-event ScvString schema
//     (actionSwap) and the newer single-event ScvSymbol("swap") +
//     ScvMap body schema (actionSwapMap, Q5). classifyAny picks the
//     shape from the topic; both reconstruct into the same TradeEvent.
//   - provide_liquidity / withdraw_liquidity (String schema)
//   - bond / unbond (per-pool stake contracts)
//
// Each action's per-field correlation is independent.
func (d *Decoder) Matches(ev events.Event) bool {
	a, _ := classifyAny(&ev)
	if a == actionUnknown {
		return false
	}
	// ADR-0035/0040 (CS-026): topic shape alone is forgeable — any
	// pubnet contract can publish ("swap","sender") string tuples.
	// Only the curated phoenix set (pools + stake contracts) is
	// attributed; a foreign emitter of the same shape is left for
	// the recognition audit to surface.
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Routes to the per-action
// reassembly buffer. Returns one consumer.Event when an action's
// required field count is met; (nil, nil) for the per-field events
// still buffering. For withdraw_liquidity the optional 5th
// `auto unbonded` event is recognised but discarded (the bond
// contract emits its own unbond which carries the same data).
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	a, fieldTopic := classifyAny(&ev)
	if a == actionUnknown {
		// Matches() already vetted this; defensive skip.
		return nil, nil
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	switch a {
	case actionSwap:
		return d.decodeSwapEvent(&ev, fieldTopic, closedAt)
	case actionSwapMap:
		return d.decodeSwapMapEvent(&ev, closedAt)
	case actionProvideLiquidity:
		return d.decodeProvideLiquidityEvent(&ev, fieldTopic, closedAt)
	case actionWithdrawLiquidity:
		return d.decodeWithdrawLiquidityEvent(&ev, fieldTopic, closedAt)
	case actionBond:
		return d.decodeStakeEvent(&ev, fieldTopic, closedAt, true)
	case actionUnbond:
		return d.decodeStakeEvent(&ev, fieldTopic, closedAt, false)
	case actionUnknown, actionAdmin, actionInitialize:
		// Non-trade Phoenix actions (admin/init/unrecognised) — recognised
		// so the dispatcher doesn't file them as unmatched, but they emit no
		// trade. Explicit per the EVERY-event policy: a NEW phoenix action
		// lands here and trips `exhaustive` until it's decided.
		return nil, nil
	}
	return nil, nil
}

// decodeSwapMapEvent handles the NEWER single-event Map-body swap
// schema (Q5). One event carries the whole trade, so — unlike the
// 8-event String path — there is no correlation buffer: decode and
// emit immediately. Assumes d.mu is held by Decode.
func (d *Decoder) decodeSwapMapEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	trade, err := decodeSwapMap(ev, closedAt)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{TradeEvent{Trade: trade}}, nil
}

// decodeSwapEvent / decodeProvideLiquidityEvent /
// decodeWithdrawLiquidityEvent / decodeStakeEvent are the per-action
// helpers extracted from Decode to keep the switch's cognitive
// complexity under the gocognit ceiling. All assume d.mu is held by
// the caller — Decode owns the lock for the duration of one event.

func (d *Decoder) decodeSwapEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorb(ev, fieldTopic, closedAt)
	d.evictedOrphans += len(evicted)
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, nil
	}
	trade, err := decodeSwap(completed)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{TradeEvent{Trade: trade}}, nil
}

func (d *Decoder) decodeProvideLiquidityEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorbProvideLiquidity(ev, fieldTopic, closedAt)
	d.evictedOrphans += evicted
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, nil
	}
	change, err := decodeProvideLiquidity(completed)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{LiquidityEvent{Change: change}}, nil
}

func (d *Decoder) decodeWithdrawLiquidityEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorbWithdrawLiquidity(ev, fieldTopic, closedAt)
	d.evictedOrphans += evicted
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, nil
	}
	change, err := decodeWithdrawLiquidity(completed)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{LiquidityEvent{Change: change}}, nil
}

func (d *Decoder) decodeStakeEvent(ev *events.Event, fieldTopic string, closedAt time.Time, isBond bool) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorbStake(ev, fieldTopic, closedAt, isBond)
	d.evictedOrphans += evicted
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, nil
	}
	change, err := decodeStake(completed)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{StakeEvent{Change: change}}, nil
}

// EvictedOrphans is the count of incomplete RawSwaps dropped by
// buffer age-out since this Decoder was constructed. Production
// callers will read this via obs.SourceOrphanEventsTotal once the
// indexer binary is rewritten in PR 165d.
func (d *Decoder) EvictedOrphans() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evictedOrphans
}
