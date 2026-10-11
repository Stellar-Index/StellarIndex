package blend

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of Blend (ADR-0035 factory-anchored gating). Every business
// event is emitted by a POOL contract and a factory emits only `deploy`, which announces each new
// pool, so we anchor on the factory and fan out to the pools it deploys.
//
// The registry of factory-deployed pools is the load-bearing state: seeded live (each decoded
// `deploy` calls reg.Seed), DB-warmed at boot (`protocol_contracts` via contractid.WithSeed), and
// genesis-seeded by walking factory `deploy` events from the lake (`stellarindex-ops
// seed-protocol-contracts -source blend`, and the ADR-0033 reconcile pre-seed). See
// docs/protocols/blend.md.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a Blend Decoder. Options seed and persist the pool registry
// (contractid.WithSeed / WithHook); with none, only live `deploy` events populate it, which is
// right for a from-genesis stream but not an incremental restart (hence the DB warm in production).
//
// The surface spans both factory versions: MainnetPoolFactories trusts V1 and V2, and decodeByKind
// handles the three V1-only kinds (update_emissions / new_liquidation_auction /
// delete_liquidation_auction) beside the V2 vocabulary. A V3 would need per-WASM-hash dispatch
// (docs/architecture/ingest-pipeline.md#contract-schema-evolution).
func NewDecoder(opts ...contractid.Option) *Decoder {
	// The factory trust roots are intrinsic and hard-coded, so they are installed first; caller
	// opts layer discovered children and persistence on top.
	base := []contractid.Option{contractid.WithFactories(MainnetPoolFactories)}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the decoder's current gate — the pool-factory trust
// roots ∪ every registered pool: exactly the contracts Matches() can accept,
// so it is the contract-id prefilter for the -ch completeness re-derive.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY, not topic symbol
// (ADR-0035): other contracts (SACs, other DeFi) emit `supply`/`claim`/`set_admin` topics too.
//
//   - `deploy` matches ONLY from a canonical Pool Factory (MainnetPoolFactories has more than
//     one). This is the trust root; otherwise a foreign contract could inject a pool into the
//     registry and launder its events as Blend's.
//   - every other event matches ONLY from a REGISTERED pool (a factory descendant).
//
// An un-seeded real pool has its events dropped, so registry completeness is a hard requirement.
// It rests on factory `deploy` events being in the lake (ADR-0033 Claim 1) and on
// MainnetPoolFactories being the complete set; an undocumented factory is the residual risk.
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

// Decode implements [dispatcher.Decoder]. Returns one consumer.Event per decode, with the kind
// preserved in EventKind so the sink can demultiplex: auctions yield New/Fill/DeleteAuctionEvent,
// the 18 money-market / emission / admin events yield PositionEvent / EmissionEvent / AdminEvent.
//
// EventIndex is stamped on position/emission/admin outputs here: without it, same-(asset, user,
// kind) events in one operation collide on the blend_positions / blend_emissions / blend_admin
// primary key and all but one are silently dropped. The auction decoders set it themselves.
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

// firstTopicHex returns a short identifying string for the event's
// topic[0] when no decoder branch matched — used in error messages
// only. Empty topic / non-symbol topic fall through to a
// placeholder rather than failing the error format itself.
func firstTopicHex(e *events.Event) string {
	if len(e.Topic) == 0 {
		return "<empty>"
	}
	return e.Topic[0]
}
