package aquarius

import (
	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/contractid"
	"github.com/StellarIndex/stellar-index/internal/events"
)

// Decoder is the dispatcher-facing view of the Aquarius AMM. Every
// Aquarius swap carries its asset identities in the event topics
// (see internal/sources/aquarius/decode.go + the contract-source
// citation there), so trade decoding needs no per-source state. The
// one piece of state the Decoder does carry is the contract-identity
// gate (ADR-0035/0040): the registry of Aquarius pools, anchored on
// the router.
type Decoder struct {
	// reg gates Matches() on contract identity (ADR-0035/0040,
	// CS-026). Trust root = MainnetRouter (the protocol's own
	// registry: its add_pool events announce every pool the
	// protocol's public API serves — verified byte-identical on
	// 2026-07-05, docs/protocols/aquarius.md).
	reg *contractid.Registry
}

// NewDecoder constructs an Aquarius Decoder. Contract-identity gating
// (ADR-0035/0040): the curated mainnet pool set (MainnetPools —
// lake-derived AND byte-identical to the protocol's registry API) is
// ALWAYS seeded, and the router trust root is always installed, so a
// bare NewDecoder() carries the full verified registry (the
// reconciliation catalogue and sub-range re-derives rely on this).
// Caller opts layer the protocol_contracts DB warm + live-upsert
// hook on top; live router `add_pool` events self-register pools
// created after this snapshot (blend-style fan-out).
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetRouter}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes (ADR-0035/0040, CS-026):
//
//   - a pool `trade` matches ONLY when emitted by a REGISTERED
//     Aquarius pool (curated seed + router-announced). The bare
//     Symbol("trade") topic is forgeable — the lake shows both a
//     parallel non-registry router deployment and a look-alike fork
//     emitting the identical shape (docs/protocols/aquarius.md).
//   - the router's `add_pool` matches ONLY when emitted by the
//     canonical router (the trust root) — Decode registers the
//     announced pool so its subsequent trades pass the gate.
//
// COVERAGE NOTE (ADR-0035): an un-registered real pool fail-closes
// into an ADR-0033 recognition gap — visible, never silently
// mis-attributed. Registry completeness is guaranteed by the
// router's add_pool events living in the lake (every API-registry
// pool is announced by one) plus the curated seed for history.
func (d *Decoder) Matches(ev events.Event) bool {
	switch classify(&ev) {
	case EventTrade, EventUpdateReserves, EventDepositLiquidity, EventWithdrawLiquidity,
		EventPoolState, EventClaimReward, EventSetRewardsConfig, EventPositionUpdate,
		EventGaugeDeposit, EventClaimFees, EventRewardsGaugeClaim, EventGaugeClaim,
		EventRewardsGaugeScheduleReward, EventSetRewardsState, EventRewardsGaugeAdd:
		// Pool flow events (trade + liquidity/reserves + the
		// rewards-gauge surface, ROADMAP #89) are gated IDENTICALLY on
		// contract identity: they match ONLY when emitted by a
		// REGISTERED Aquarius pool. The bare topic symbols are
		// forgeable — a look-alike must not be able to inject
		// fabricated reserves/liquidity/rewards any more than it
		// could inject fabricated trades (CS-026).
		return d.reg.Has(ev.ContractID)
	case EventConfigRewards, EventApplyUpgrade, EventCommitUpgrade, EventSetPrivilegedAddrs,
		EventApplyTransferOwnership, EventCommitTransferOwnership,
		EventEnableEmergencyMode, EventDisableEmergencyMode, EventPoolGaugeSwitchToken:
		// Router-scoped governance/upgrade surface (ROADMAP #89):
		// gated on the canonical router trust root ONLY. Real lake
		// bytes show a small family of unidentified non-pool,
		// non-router contracts also emitting several of these kinds
		// (co-occurring with the FLAGGED parallel router deployment —
		// see decode_admin.go's package doc) — those fail-closed here
		// exactly like CA7RQDMM's trade events already do, a visible
		// ADR-0033 recognition gap pending Aquarius-team confirmation,
		// not a silent mis-attribution.
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

// Decode implements [dispatcher.Decoder]. Returns one TradeEvent
// per successful pool-trade decode (Aquarius trades are always
// single-pair). A router `add_pool` announcement registers the new
// pool in the gate registry and emits nothing ((nil, nil) is the
// dispatcher's "match, nothing to emit" shape) — Seed fires the
// persistence hook so the mapping survives restarts.
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
	case EventUpdateReserves:
		rv, err := decodeReserves(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{rv}, nil
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
	default:
		// EventTrade (and, defensively, anything Matches() let
		// through) decodes as a trade.
		trade, err := decodeTrade(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{TradeEvent{Trade: trade}}, nil
	}
}
