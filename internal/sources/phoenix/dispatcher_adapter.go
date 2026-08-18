package phoenix

import (
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
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

// GatedContractSet returns the decoder's gate — the factory trust root ∪
// every registered pool/stake contract (the curated seed; Phoenix's factory
// creation events predate the lake, so the set is static after
// construction). It is the contract-id prefilter the -ch completeness
// re-derive scopes its lake read to: Matches() gates purely on contract
// identity (reg.Has), so streaming just these contracts yields byte-identical
// counts to a whole-lake stream. The intra-tx correlation buffer only ever
// groups events from a SINGLE pool, so a contract-id prefilter never splits a
// correlation group (contrast a cross-contract correlation, which it would
// break — see the completeness catalogue's exclusion of defindex).
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

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
	case actionWithdrawRewards:
		return d.decodeWithdrawRewardsEvent(&ev, fieldTopic, closedAt)
	case actionDistributeRewards:
		return d.decodeDistributeRewardsEvent(&ev, closedAt)
	case actionInitialize:
		return decodeInitializeEvent(&ev, fieldTopic, closedAt)
	case actionAdmin:
		return decodeAdminEvent(&ev, fieldTopic, closedAt)
	case actionUnknown:
		// Unrecognised Phoenix action — recognised at the topic[0] level
		// so the dispatcher doesn't file it as unmatched, but nothing to
		// project. Explicit per the EVERY-event policy: a NEW phoenix
		// action lands here and trips `exhaustive` until it's decided.
		// (initialize + admin are now projected — see above.)
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
	// Sweep-time rescue (sources-decode audit 2026-08-04, finding 1):
	// an aged-out group whose decode-consumed slots are all present is a
	// pre-upgrade 7-event swap (that era never sends ActualReceived /
	// SpreadAmount / ReferralFee), not an orphan — decode and emit it.
	// Only genuinely under-filled groups count as orphans. Decode
	// failures on a rescued group fall through to the orphan count
	// rather than failing the CURRENT event, which is unrelated to the
	// swept group.
	var out []consumer.Event
	for i := range evicted {
		if !evicted[i].Decodable() {
			d.evictedOrphans++
			continue
		}
		trade, derr := decodeSwap(&evicted[i])
		if derr != nil {
			d.evictedOrphans++
			continue
		}
		out = append(out, TradeEvent{Trade: trade})
	}
	if err != nil {
		return out, err
	}
	if completed == nil {
		return out, nil
	}
	trade, err := decodeSwap(completed)
	if err != nil {
		return out, err
	}
	return append(out, TradeEvent{Trade: trade}), nil
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

func (d *Decoder) decodeWithdrawRewardsEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorbWithdrawRewards(ev, fieldTopic, closedAt)
	d.evictedOrphans += evicted
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, nil
	}
	change, err := decodeWithdrawRewards(completed)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{StakeEvent{Change: change}}, nil
}

// decodeDistributeRewardsEvent handles a distribute_rewards event directly
// (no correlation buffer — the action is a single field, see decode.go).
func (d *Decoder) decodeDistributeRewardsEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	change, err := decodeDistributeRewards(ev, closedAt)
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

// decodeInitializeEvent decodes a pool-deploy `initialize` event into an
// InitializeEvent. The token slot ('a'/'b') comes from topic[1]; the
// announced token contract address is the event body (a single
// Address). Self-contained — one event → one row, no correlation buffer.
func decodeInitializeEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	var slot string
	switch fieldTopic {
	case TopicInitTokenA:
		slot = "a"
	case TopicInitTokenB:
		slot = "b"
	default:
		return nil, fmt.Errorf("%w: initialize unrecognised token-slot topic", ErrMalformedPayload)
	}
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: initialize body: %w", ErrMalformedPayload, err)
	}
	token, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return nil, fmt.Errorf("%w: initialize token address: %w", ErrMalformedPayload, err)
	}
	return []consumer.Event{InitializeEvent{
		Pool:       ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex), //nolint:gosec // OperationIndex non-negative by Soroban spec.
		EventIndex: uint32(ev.EventIndex),     //nolint:gosec // EventIndex non-negative by Soroban spec.
		ObservedAt: closedAt,
		TokenSlot:  slot,
		Token:      token,
	}}, nil
}

// decodeAdminEvent decodes a pool admin-rotation event into an
// AdminEvent. The action slug comes from topic[1]; the admin address is
// the event body when it carries one. DEFENSIVE — 0 occurrences on
// mainnet, no lake sample confirms the exact body shape, so a
// non-Address / absent body yields an empty Admin rather than an error
// (the action + identity are still worth recording).
func decodeAdminEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	slug, ok := adminActionByTopic[fieldTopic]
	if !ok {
		return nil, fmt.Errorf("%w: admin unrecognised rotation-phrase topic", ErrMalformedPayload)
	}
	// Best-effort admin address: parse the body as an Address when it is
	// one; tolerate a void / other-shaped body (empty Admin).
	var admin string
	if sv, err := scval.Parse(ev.Value); err == nil {
		if a, aerr := scval.AsAddressStrkey(sv); aerr == nil {
			admin = a
		}
	}
	return []consumer.Event{AdminEvent{
		Pool:        ev.ContractID,
		Ledger:      ev.Ledger,
		TxHash:      ev.TxHash,
		OpIndex:     uint32(ev.OperationIndex), //nolint:gosec // OperationIndex non-negative by Soroban spec.
		EventIndex:  uint32(ev.EventIndex),     //nolint:gosec // EventIndex non-negative by Soroban spec.
		ObservedAt:  closedAt,
		AdminAction: slug,
		Admin:       admin,
	}}, nil
}
