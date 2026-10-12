package phoenix

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Decoder is the stateful dispatcher-facing Phoenix view: one swap is 8 separate events correlated by (ledger, tx_hash, op_index) in its buffer before a canonical.Trade is emitted; Decode is not re-entrant, the mutex guards parallel ledger replay.
type Decoder struct {
	mu  sync.Mutex
	buf *buffer

	// evictedOrphans counts incomplete RawSwaps the buffer dropped (aged past defaultOrphanMaxAge).
	// The dispatcher reads it via the optional EvictedOrphans() int interface and the indexer
	// reports it as obs.SourceOrphanEventsTotal.
	evictedOrphans int
	// carried holds rescued trades from a Decode call that failed on its
	// own event; the next successful Decode emits them.
	carried []consumer.Event
	// reg gates Matches() on contract identity (ADR-0035/0040).
	reg *contractid.Registry
}

// NewDecoder constructs a phoenix Decoder with a fresh buffer, gated on contract identity (ADR-0035/0040): the curated mainnet pool and stake set is the cold-start root, the factory's ("create","liquidity_pool") events self-register pools, and stake contracts (not factory-announced) come only from the seed or the protocol_contracts warm; an admitted pool trusts the upgradeable factory admin, and admission is not price trust.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetFactory}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{buf: newBuffer(), reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the gate (factory root plus every registered pool/stake contract) as the contract-id prefilter for the completeness re-derive; Matches() gates purely on contract identity and the buffer groups events from a single pool, so the prefilter never splits a group.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on contract identity and claiming swap (older 8-event ScvString schema or newer single ScvSymbol("swap") + ScvMap schema, picked by classifyAny from the topic), provide/withdraw_liquidity, bond/unbond and other stake events, and the factory's create / config-update events (IsFactory); each action's per-field correlation is independent.
func (d *Decoder) Matches(ev events.Event) bool {
	a, _ := classifyAny(&ev)
	if a == actionUnknown {
		return false
	}
	if a == actionCreatePool || a == actionFactoryConfig {
		// Gate on the FACTORY trust root, never reg.Has: a curated pool already passes reg.Has,
		// so it could otherwise republish these topics and inject a child pool.
		return d.reg.IsFactory(ev.ContractID)
	}
	// ADR-0035/0040: topic shape alone is forgeable, any pubnet contract can publish
	// ("swap","sender") tuples. Only the registered set is attributed; a foreign emitter is left
	// for the recognition audit to surface.
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder], routing to the per-action reassembly buffer and returning an event once the action's required field count is met, (nil, nil) while buffering; withdraw_liquidity's optional `auto unbonded` event is discarded since the bond contract emits its own unbond.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	a, fieldTopic := classifyAny(&ev)
	if a == actionUnknown {
		// Matches() already vetted this; defensive skip.
		return nil, nil
	}
	if a == actionCreatePool {
		// Handled before the lock: the announcement needs neither the
		// close time nor the correlation buffer, and Seed fires the
		// durable live-upsert hook — which should not run under d.mu
		// (aquarius does the same for add_pool).
		return nil, d.seedAnnouncedPool(&ev)
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	out, err := d.decodeAction(a, &ev, fieldTopic, closedAt)
	if err != nil {
		return nil, err
	}
	if len(d.carried) > 0 {
		out = append(d.carried, out...)
		d.carried = nil
	}
	return out, nil
}

// decodeAction routes one event to its per-action decoder. Assumes d.mu
// is held by Decode.
func (d *Decoder) decodeAction(a action, ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	switch a {
	case actionSwap:
		return d.decodeSwapEvent(ev, fieldTopic, closedAt)
	case actionSwapMap:
		return d.decodeSwapMapEvent(ev, closedAt)
	case actionProvideLiquidity:
		return d.decodeProvideLiquidityEvent(ev, fieldTopic, closedAt)
	case actionWithdrawLiquidity:
		return d.decodeWithdrawLiquidityEvent(ev, fieldTopic, closedAt)
	case actionProvideLiquidityMap:
		return liquidityEventOf(decodeProvideLiquidityMap(ev, closedAt))
	case actionWithdrawLiquidityMap:
		return liquidityEventOf(decodeWithdrawLiquidityMap(ev, closedAt))
	case actionBond:
		return d.decodeStakeEvent(ev, fieldTopic, closedAt, true)
	case actionUnbond:
		return d.decodeStakeEvent(ev, fieldTopic, closedAt, false)
	case actionWithdrawRewards:
		return d.decodeWithdrawRewardsEvent(ev, fieldTopic, closedAt)
	case actionDistributeRewards:
		return d.decodeDistributeRewardsEvent(ev, closedAt)
	case actionInitialize:
		return decodeInitializeEvent(ev, fieldTopic, closedAt)
	case actionAdmin:
		return decodeAdminEvent(ev, fieldTopic, closedAt)
	case actionCreateDistributionFlow:
		return decodeCreateDistributionFlowEvent(ev, closedAt)
	case actionStakeMigration:
		return decodeStakeMigrationEvent(ev, fieldTopic, closedAt)
	case actionFactoryConfig:
		return decodeFactoryConfigEvent(ev, closedAt)
	case actionBlendPoolAdmin:
		return decodeBlendPoolAdminEvent(ev, fieldTopic, closedAt)
	case actionToggleTrading:
		return nil, checkToggleTradingBody(ev)
	case actionCreatePool:
		// Handled above, before the lock. Enumerated so `exhaustive`
		// keeps covering the action enum.
		return nil, nil
	case actionUnknown:
		// Unrecognised Phoenix action — recognised at the topic[0] level
		// so the dispatcher doesn't file it as unmatched, but nothing to
		// project. Explicit per the EVERY-event policy: a NEW phoenix
		// action lands here and trips `exhaustive` until it's decided.
		return nil, nil
	}
	return nil, nil
}

// seedAnnouncedPool admits the pool a factory ("create","liquidity_pool") event announces into the identity gate and fires the
// live-upsert hook so the mapping survives a restart. Emits no consumer.Event. Matches() already proved ev.ContractID is a factory
// trust root, so the announced address is a genuine pool; ev.ContractID is recorded as provenance.
func (d *Decoder) seedAnnouncedPool(ev *events.Event) error {
	pool, err := decodeAnnouncedPool(ev)
	if err != nil {
		return err
	}
	d.reg.Seed(pool, ev.ContractID, ev.Ledger)
	return nil
}

// decodeSwapMapEvent handles the newer single-event Map-body swap schema (Q5): one event is the whole trade, so decode and emit
// immediately with no correlation buffer. Assumes d.mu is held by Decode.
func (d *Decoder) decodeSwapMapEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	return tradeOrNoOp(decodeSwapMap(ev, closedAt))
}

// isNotATrade reports a swap that decoded fully but maps to zero rows (zero leg or self-pair). Returned as an error it would count
// undecodable and fail the source's ADR-0033 verdict closed forever; indeterminate parse failures keep the error path.
func isNotATrade(err error) bool {
	return errors.Is(err, ErrZeroAmountSwap) || errors.Is(err, canonical.ErrPairMismatch)
}

// tradeOrNoOp turns a decodeSwap / decodeSwapMap result into Decode's return shape: one TradeEvent, a recognised no-op, or the error.
func tradeOrNoOp(trade canonical.Trade, err error) ([]consumer.Event, error) {
	if err != nil {
		if isNotATrade(err) {
			return nil, nil
		}
		return nil, err
	}
	return []consumer.Event{TradeEvent{Trade: trade}}, nil
}

// decodeSwapEvent / decodeProvideLiquidityEvent / decodeWithdrawLiquidityEvent / decodeStakeEvent
// are the per-action helpers behind Decode. All assume the caller holds d.mu for the one event.

func (d *Decoder) decodeSwapEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	completed, evicted, err := d.buf.absorb(ev, fieldTopic, closedAt)
	d.evictedOrphans += d.buf.takeOtherEvicted()
	out := d.rescueEvicted(evicted)
	if err == nil && completed != nil {
		var cur []consumer.Event
		cur, err = tradeOrNoOp(decodeSwap(completed))
		out = append(out, cur...)
	}
	if err != nil {
		// Every Decode caller discards the outputs of an erroring call, so
		// the rescued trades — complete, and unrelated to this event —
		// ride out on the next successful Decode instead.
		d.carried = append(d.carried, out...)
		return nil, err
	}
	return out, nil
}

// rescueEvicted decodes the swap groups the buffer aged or rotated out. A group whose decode-consumed slots are all present is a
// pre-upgrade 7-event swap (no ActualReceived / SpreadAmount / ReferralFee) or the first of two same-pool swaps in one op, not an
// orphan. Only under-filled or undecodable groups count as orphans; a recognised no-op is neither trade nor orphan.
func (d *Decoder) rescueEvicted(evicted []RawSwap) []consumer.Event {
	var out []consumer.Event
	for i := range evicted {
		if !evicted[i].Decodable() {
			d.evictedOrphans++
			continue
		}
		trade, derr := decodeSwap(&evicted[i])
		switch {
		case derr == nil:
			out = append(out, TradeEvent{Trade: trade})
		case !isNotATrade(derr):
			d.evictedOrphans++
		}
	}
	return out
}

// liquidityEventOf wraps a single-event (Map schema) liquidity decode;
// like the swap Map path it needs no correlation buffer.
func liquidityEventOf(change LiquidityChange, err error) ([]consumer.Event, error) {
	if err != nil {
		return nil, err
	}
	return []consumer.Event{LiquidityEvent{Change: change}}, nil
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

// Drain implements [dispatcher.Drainer], flushing every open correlation group at the end of a bounded stream: Decodable swap groups are emitted, the rest count as orphans.
func (d *Decoder) Drain() []consumer.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	swaps, other := d.buf.drain()
	d.evictedOrphans += other
	out := append(d.carried, d.rescueEvicted(swaps)...)
	d.carried = nil
	return out
}

// EvictedOrphans is the count of incomplete correlation groups dropped since construction, read by the dispatcher through its optional EvictedOrphans() interface.
func (d *Decoder) EvictedOrphans() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evictedOrphans
}

// decodeInitializeEvent decodes a pool-deploy `initialize` event into an InitializeEvent: the token slot ('a'/'b') comes from
// topic[1], the body is the announced token's single Address. One event, one row, no correlation buffer.
func decodeInitializeEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	var slot string
	switch fieldTopic {
	case TopicInitTokenA:
		slot = "a"
	case TopicInitTokenB:
		slot = "b"
	case TopicInitLPShareStaking:
		// The per-pool STAKE contract's deploy-time self-announcement is not a pool token_a/token_b
		// slot, so it maps to no phoenix_initialize row. It is recognised but not projected: emit
		// nothing rather than ErrMalformedPayload, which would leave undecodable-but-matched blind
		// ledgers in the projection re-derive. See events.go TopicInitLPShareStaking.
		return nil, nil
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

// decodeAdminEvent decodes a pool admin-rotation event: the action slug comes from topic[1], the admin from the body when present.
// No mainnet sample confirms the body shape, so a non-Address or absent body yields an empty Admin, not an error.
func decodeAdminEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	slug, ok := adminActionByTopic[fieldTopic]
	if !ok {
		// Recognised-not-projected: the raw event is in the soroban_events
		// landing zone (ADR-0029); an unknown phrase must not fail ingest.
		return nil, nil
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
