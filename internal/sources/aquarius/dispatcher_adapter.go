package aquarius

import (
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Aquarius AMM. Swaps carry their asset identities in
// the event topics, so trade decoding needs no per-source state; the only state is the
// contract-identity gate (ADR-0035/0040), the registry of Aquarius pools anchored on the router.
type Decoder struct {
	// reg gates Matches() on contract identity (ADR-0035/0040).
	// Trust root = MainnetRouter (the protocol's own registry: its
	// add_pool events announce every pool the protocol's public API
	// serves — verified byte-identical, docs/protocols/aquarius.md).
	reg *contractid.Registry
}

// NewDecoder constructs an Aquarius Decoder. Contract-identity gating (ADR-0035/0040): the curated
// MainnetPools set is always seeded and the router trust root always installed, so a bare
// NewDecoder() carries the full verified registry (reconciliation and sub-range re-derives rely
// on this). Caller opts layer the protocol_contracts DB warm and live-upsert hook on top; live
// router `add_pool` events self-register pools created after the snapshot.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetRouter}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the decoder's current gate: the router trust root plus every registered
// pool (curated seed and router-announced). It is the contract-id prefilter the completeness
// re-derive scopes its lake read to; Matches() accepts events only from these contracts, so
// streaming just them yields the same counts as a whole-lake stream, far faster. It reflects only
// what Decode has registered so far, so callers needing in-window pools must seed the registry
// before reading it.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. It gates on contract identity, not topic bytes
// (ADR-0035/0040):
//
//   - a pool `trade` matches only when emitted by a registered Aquarius pool (curated seed or
//     router-announced). The bare Symbol("trade") topic is forgeable: the lake shows a parallel
//     non-registry router and a look-alike fork emitting the identical shape.
//   - the router's `add_pool` matches only when emitted by the canonical router (the trust root);
//     Decode registers the announced pool so its later trades pass the gate.
//
// An unregistered real pool fails closed into an ADR-0033 recognition gap, visible and never
// silently mis-attributed. Registry completeness comes from the router's add_pool events in the
// lake plus the curated seed for history.
func (d *Decoder) Matches(ev events.Event) bool {
	switch classify(&ev) {
	case EventTrade, EventUpdateReserves, EventReservesSync, EventDepositLiquidity, EventWithdrawLiquidity,
		EventPoolState, EventClaimReward, EventSetRewardsConfig, EventPositionUpdate,
		EventGaugeDeposit, EventClaimFees, EventRewardsGaugeClaim, EventGaugeClaim,
		EventRewardsGaugeScheduleReward, EventSetRewardsState, EventRewardsGaugeAdd:
		// Pool flow events (trade, liquidity/reserves, rewards-gauge) are all gated on contract
		// identity: only a registered pool matches. The bare topic symbols are forgeable, so a
		// look-alike must not inject fabricated reserves, liquidity or rewards any more than trades.
		return d.reg.Has(ev.ContractID)
	case EventApplyUpgrade, EventCommitUpgrade, EventSetPrivilegedAddrs,
		EventApplyTransferOwnership, EventCommitTransferOwnership,
		EventEnableEmergencyMode, EventDisableEmergencyMode,
		EventSetProtocolFee, EventClaimProtocolFee,
		EventKillDeposit, EventUnkillDeposit, EventKillSwap, EventUnkillSwap,
		EventKillClaim, EventUnkillClaim, EventKillGaugesClaim, EventUnkillGaugesClaim:
		// Pool-emittable governance/upgrade surface (protocol-fee and kill-switch kinds included;
		// the router also emits set_protocol_fee): gated on the pool registry (reg.Has) plus the
		// router trust root (reg.IsFactory). Registered pools legitimately emit these (WASM
		// upgrades, ownership and privileged-address changes, emergency toggles), so gating on
		// the router alone would fail-close them into recognition gaps and drop real governance
		// history from Decode. An emitter in neither set fails closed like its trades: a visible
		// gap, never a silent mis-attribution.
		return d.reg.Has(ev.ContractID) || d.reg.IsFactory(ev.ContractID)
	case EventConfigRewards, EventPoolGaugeSwitchToken:
		// Router-scoped governance surface: config_rewards and pool_gauge_switch_token are only
		// ever router-emitted, so the gate is the router trust root alone. A pool or foreign
		// contract emitting these fails closed into a visible recognition gap.
		return d.reg.IsFactory(ev.ContractID)
	}
	return isAddPool(&ev) && d.reg.IsFactory(ev.ContractID)
}

// isAddPool reports whether the event is a router pool-registration
// announcement (topic[0] = Symbol("add_pool")). Caller must still
// verify the emitter is the canonical router (reg.IsFactory).
func isAddPool(e *events.Event) bool {
	return len(e.Topic) > 0 && e.Topic[0] == TopicSymbolAddPool
}

// isNotATrade reports a recognised no-op: a genuine dust swap whose sold
// or bought side is zero, or a fully-decoded self-pair (sold == bought
// token, comet's exploit primitive). Neither can form a canonical.Trade,
// so there is no row to project and no error to be blind on.
func isNotATrade(err error) bool {
	return errors.Is(err, ErrZeroAmountTrade) || errors.Is(err, canonical.ErrPairMismatch)
}

// Decode implements [dispatcher.Decoder]. It returns one TradeEvent per successful pool-trade
// decode (Aquarius trades are single-pair). A router `add_pool` announcement registers the new
// pool in the gate and emits nothing ((nil, nil) is the dispatcher's "match, nothing to emit");
// Seed fires the persistence hook so the mapping survives restarts.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if isAddPool(&ev) {
		pool, err := decodeAnnouncedPool(&ev)
		if err != nil {
			return nil, err
		}
		// Matches() already guaranteed ev.ContractID is the canonical
		// router (the trust root), so the announced address is a
		// genuine Aquarius pool; ev.ContractID is the provenance.
		d.reg.Seed(pool, ev.ContractID, ev.Ledger)
		return nil, nil
	}
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	kind := classify(&ev)
	switch kind {
	case EventUpdateReserves, EventReservesSync:
		rv, err := decodeReserves(&ev, closedAt, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{rv}, nil
	case EventSetProtocolFee, EventClaimProtocolFee:
		return emitFee(&ev, closedAt, kind)
	case EventKillDeposit, EventUnkillDeposit, EventKillSwap, EventUnkillSwap,
		EventKillClaim, EventUnkillClaim, EventKillGaugesClaim, EventUnkillGaugesClaim:
		return emitKill(&ev, closedAt, kind)
	case EventDepositLiquidity:
		lq, err := decodeLiquidity(&ev, LiquidityDeposit, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{lq}, nil
	case EventWithdrawLiquidity:
		lq, err := decodeLiquidity(&ev, LiquidityWithdraw, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{lq}, nil
	case EventPoolState, EventClaimReward, EventSetRewardsConfig, EventPositionUpdate,
		EventGaugeDeposit, EventClaimFees, EventRewardsGaugeClaim, EventGaugeClaim,
		EventRewardsGaugeScheduleReward, EventSetRewardsState, EventRewardsGaugeAdd,
		EventConfigRewards:
		rv, err := decodeRewardsEvent(&ev, kind, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{rv}, nil
	case EventApplyUpgrade, EventCommitUpgrade, EventSetPrivilegedAddrs,
		EventApplyTransferOwnership, EventCommitTransferOwnership,
		EventEnableEmergencyMode, EventDisableEmergencyMode, EventPoolGaugeSwitchToken:
		av, err := decodeAdminEvent(&ev, kind, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{av}, nil
	case EventTrade:
		trade, err := decodeTrade(&ev, closedAt)
		if err != nil {
			if isNotATrade(err) {
				return nil, nil
			}
			return nil, err
		}
		return []consumer.Event{TradeEvent{Trade: trade}}, nil
	default:
		// A kind that passed the identity gate but has no decode arm fails closed (ADR-0035)
		// instead of going through decodeTrade, which would misattribute a trade-shaped topic as
		// a real trade rather than surfacing a visible gap.
		return nil, fmt.Errorf("%w: unhandled event kind %q", ErrMalformedPayload, kind)
	}
}

// emitFee decodes a protocol-fee event (set_protocol_fee /
// claim_protocol_fee) into a single FeeEvent. Extracted from Decode to
// keep its cognitive complexity under the gocognit ceiling.
func emitFee(ev *events.Event, closedAt time.Time, kind string) ([]consumer.Event, error) {
	fe, err := decodeFee(ev, closedAt, kind)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{fe}, nil
}

// emitKill builds a KillEvent for a pool circuit-breaker toggle. These events carry no body
// (SCV_VOID) and a single topic, validated via requireVoidBody, so the classify() action and the
// event identity are the whole signal once the shape is confirmed.
func emitKill(ev *events.Event, closedAt time.Time, kind string) ([]consumer.Event, error) {
	if err := requireVoidBody(ev, kind); err != nil {
		return nil, err
	}
	return []consumer.Event{KillEvent{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex), //nolint:gosec // OperationIndex non-negative by Soroban spec.
		EventIndex: uint32(ev.EventIndex),     //nolint:gosec // EventIndex non-negative by Soroban spec.
		ObservedAt: closedAt,
		Action:     kind,
	}}, nil
}
