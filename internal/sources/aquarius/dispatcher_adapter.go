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

// Decoder is the dispatcher-facing Aquarius view; swaps carry asset identities in topics so trade decoding needs no state, and the only state is the contract-identity gate (ADR-0035/0040), the pool registry anchored on the router.
type Decoder struct {
	// reg gates Matches() on contract identity (ADR-0035/0040). Trust root is MainnetRouter, whose
	// add_pool events announce every pool the protocol serves (docs/protocols/aquarius.md).
	reg *contractid.Registry
}

// NewDecoder constructs an Aquarius Decoder; the curated MainnetPools and router root are always installed (reconciliation and sub-range re-derives rely on that), and opts layer the DB warm and live-upsert hook on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetRouter}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the current gate (router plus every registered pool) as the contract-id prefilter for the completeness re-derive; it reflects only pools registered so far, so seed the registry first for in-window pools.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on contract identity, not topic bytes (ADR-0035/0040): a pool `trade` matches only from a registered pool (the bare Symbol("trade") topic is forgeable by look-alike routers and forks), the router's `add_pool` only from the canonical router (trust root); an unregistered real pool fails closed into a recognition gap.
func (d *Decoder) Matches(ev events.Event) bool {
	switch classify(&ev) {
	case EventTrade, EventUpdateReserves, EventReservesSync, EventDepositLiquidity, EventWithdrawLiquidity,
		EventPoolState, EventClaimReward, EventSetRewardsConfig, EventPositionUpdate,
		EventGaugeDeposit, EventClaimFees, EventRewardsGaugeClaim, EventGaugeClaim,
		EventRewardsGaugeScheduleReward, EventSetRewardsState, EventRewardsGaugeAdd:
		// Pool flow events are gated on contract identity like trades: the topic symbols are
		// forgeable, so a look-alike must not inject fabricated reserves, liquidity or rewards.
		return d.reg.Has(ev.ContractID)
	case EventApplyUpgrade, EventCommitUpgrade, EventSetPrivilegedAddrs,
		EventApplyTransferOwnership, EventCommitTransferOwnership,
		EventEnableEmergencyMode, EventDisableEmergencyMode,
		EventSetProtocolFee, EventClaimProtocolFee,
		EventKillDeposit, EventUnkillDeposit, EventKillSwap, EventUnkillSwap,
		EventKillClaim, EventUnkillClaim, EventKillGaugesClaim, EventUnkillGaugesClaim:
		// Governance/upgrade surface: gated on the pool registry plus the router trust root, since
		// pools legitimately emit these and the router also emits set_protocol_fee. Gating on the
		// router alone would drop real governance history into recognition gaps. An emitter in
		// neither set fails closed like its trades.
		return d.reg.Has(ev.ContractID) || d.reg.IsFactory(ev.ContractID)
	case EventConfigRewards, EventPoolGaugeSwitchToken:
		// config_rewards and pool_gauge_switch_token are only router-emitted, so the gate is the
		// router alone; any other emitter fails closed into a recognition gap.
		return d.reg.IsFactory(ev.ContractID)
	}
	return isAddPool(&ev) && d.reg.IsFactory(ev.ContractID)
}

// isAddPool reports whether the event is a pool-registration announcement. The caller must still
// verify the emitter is the canonical router (reg.IsFactory).
func isAddPool(e *events.Event) bool {
	return len(e.Topic) > 0 && e.Topic[0] == TopicSymbolAddPool
}

// isNotATrade reports a recognised no-op: a dust swap with a zero side, or a self-pair (sold ==
// bought token). Neither can form a canonical.Trade, so there is no row and no error to hide.
func isNotATrade(err error) bool {
	return errors.Is(err, ErrZeroAmountTrade) || errors.Is(err, canonical.ErrPairMismatch)
}

// Decode implements [dispatcher.Decoder], returning one TradeEvent per successful pool-trade decode (single-pair); a router `add_pool` registers the new pool in the gate and emits nothing ((nil, nil)), and Seed fires the persistence hook.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if isAddPool(&ev) {
		pool, err := decodeAnnouncedPool(&ev)
		if err != nil {
			return nil, err
		}
		// Matches() guaranteed the emitter is the canonical router, so the announced address
		// is a genuine pool; ev.ContractID is the provenance.
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
		// A kind that passed the gate but has no decode arm fails closed rather than going
		// through decodeTrade, which would misattribute it as a trade.
		return nil, fmt.Errorf("%w: unhandled event kind %q", ErrMalformedPayload, kind)
	}
}

// emitFee decodes a protocol-fee event into a single FeeEvent; split out to keep Decode under the
// gocognit ceiling.
func emitFee(ev *events.Event, closedAt time.Time, kind string) ([]consumer.Event, error) {
	fe, err := decodeFee(ev, closedAt, kind)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{fe}, nil
}

// emitKill builds a KillEvent for a pool circuit-breaker toggle. These carry no body (SCV_VOID), so
// once requireVoidBody confirms the shape, the classified action and event identity are the signal.
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
