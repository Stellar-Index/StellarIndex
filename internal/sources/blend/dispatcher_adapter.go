package blend

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing Blend view (ADR-0035 factory-anchored gating): pools emit every business event and the factory only `deploy`, so the pool registry (live deploys, DB warm, genesis seed) is the load-bearing state (docs/protocols/blend.md).
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a Blend Decoder; options seed and persist the pool registry (contractid.WithSeed / WithHook). It spans factory V1 and V2 (decodeByKind handles the V1-only kinds); a V3 would need per-WASM-hash dispatch.
func NewDecoder(opts ...contractid.Option) *Decoder {
	// The factory trust roots are intrinsic and hard-coded, so they are installed first; caller
	// opts layer discovered children and persistence on top.
	base := []contractid.Option{contractid.WithFactories(MainnetPoolFactories)}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the current gate (factory trust roots plus every registered pool), exactly what Matches() can accept, as the contract-id prefilter for the -ch completeness re-derive.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on CONTRACT IDENTITY, not topic (ADR-0035): `deploy` matches only from a canonical MainnetPoolFactories entry (the trust root), every other event only from a REGISTERED pool; an un-seeded real pool has its events dropped.
func (d *Decoder) Matches(ev events.Event) bool {
	kind := classifyAny(&ev)
	if kind == "" {
		return false
	}
	if kind == EventDeploy {
		return d.reg.IsFactory(ev.ContractID)
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder], returning one consumer.Event per decode with the kind kept in EventKind; EventIndex is stamped here because without it same-(asset, user, kind) events in one operation collide on the primary key and all but one are dropped.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	outs, err := d.decodeByKind(ev)
	if err != nil {
		return nil, err
	}
	ei := uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	for i, o := range outs {
		switch e := o.(type) {
		case PositionEvent:
			e.EventIndex = ei
			outs[i] = e
		case EmissionEvent:
			e.EventIndex = ei
			outs[i] = e
		case AdminEvent:
			e.EventIndex = ei
			outs[i] = e
			// Fan-out (ADR-0035): register the deployed pool so its later events pass the Matches
			// gate. Seed fires the persistence hook, so the mapping survives a restart even after
			// the projector cursor passes this ledger. Matches() already guaranteed the deploy came
			// from a MainnetPoolFactories member, so Target is a genuine pool.
			if e.Kind == EventDeploy && e.Target != "" {
				d.reg.Seed(e.Target, e.ContractID, ev.Ledger)
			}
		}
	}
	return outs, nil
}

func (d *Decoder) decodeByKind(ev events.Event) ([]consumer.Event, error) { //nolint:gocyclo,gocognit,funlen,cyclop // one case per Blend event kind; flattening makes the dispatch table easier to audit against pool/src/events.rs.
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	kind := classifyAny(&ev)
	switch kind {
	// ─── Auction events (blend_auctions table) ───────────────
	case EventNewAuction:
		decode := decodeNewAuction
		if len(ev.Topic) == v1NewAuctionTopicArity {
			decode = decodeNewAuctionV1
		}
		out, err := decode(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventFillAuction:
		decode := decodeFillAuction
		if isV1FillAuction(&ev) {
			decode = decodeFillAuctionV1
		}
		out, err := decode(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDeleteAuction:
		out, err := decodeDeleteAuction(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil

	// ─── Money-market position events (blend_positions) ───────
	case EventSupply, EventWithdraw,
		EventSupplyCollateral, EventWithdrawCollateral,
		EventBorrow, EventRepay, EventFlashLoan:
		out, err := decodePositionEvent(&ev, kind, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil

	// ─── Emission / credit-risk events (blend_emissions) ──────
	case EventGulp:
		out, err := decodeGulp(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventClaim:
		out, err := decodeClaim(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventReserveEmissions:
		out, err := decodeReserveEmissionUpdate(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventGulpEmissions:
		out, err := decodeGulpEmissions(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventUpdateEmissions:
		out, err := decodeUpdateEmissions(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventBadDebt:
		decode := decodeBadDebt
		if len(ev.Topic) == v1BadDebtTopicArity {
			decode = decodeBadDebtV1
		}
		out, err := decode(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDefaultedDebt:
		out, err := decodeDefaultedDebt(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil

	// ─── Admin / status / factory events (blend_admin) ────────
	case EventSetAdmin:
		out, err := decodeSetAdmin(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventUpdatePool:
		out, err := decodeUpdatePool(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventQueueSetReserve:
		out, err := decodeQueueSetReserve(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventCancelSetReserve:
		out, err := decodeCancelSetReserve(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventSetReserve:
		out, err := decodeSetReserve(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventSetStatus:
		out, err := decodeSetStatus(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDeploy:
		out, err := decodeDeploy(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil

	// ─── V1 pool-factory events (blend_admin) ──────────────────
	case EventNewLiquidationAuction:
		out, err := decodeNewLiquidationAuctionV1(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDeleteLiquidationAuction:
		out, err := decodeDeleteLiquidationAuctionV1(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil

	default:
		return nil, fmt.Errorf("%w: topic[0]=%q", ErrNotBlendEvent, firstTopicHex(&ev))
	}
}

// firstTopicHex returns a short topic[0] description for error messages only; non-symbol topics get a placeholder.
func firstTopicHex(e *events.Event) string {
	if len(e.Topic) == 0 {
		return "<empty>"
	}
	return e.Topic[0]
}
