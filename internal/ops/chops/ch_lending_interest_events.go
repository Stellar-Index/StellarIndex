package chops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// interestEventDecoder turns the pools' lake events into the fold's cause events. Contract identity is the gate: an
// event of a contract outside the pools is rejected whatever its topic, and a pool's events decode by its
// protocol's ABI. A pool's cause-topic event that does not decode is its transaction's CauseUndecodable marker, so
// the window still derives and that transaction's residuals stay unexplained.
type interestEventDecoder struct {
	protos map[string]lendinginterest.Protocol
	blend  *blend.Decoder
	blnt   *blnt.Decoder
}

func newInterestEventDecoder(protos map[string]lendinginterest.Protocol) interestEventDecoder {
	ids := slices.Sorted(maps.Keys(protos))
	return interestEventDecoder{
		protos: protos,
		blend:  blend.NewDecoder(contractid.WithSeed(ids)),
		blnt:   blnt.NewDecoder(contractid.WithSeed(ids)),
	}
}

// errInterestEventUndecodable wraps why a pool's event did not decode; decode returns the marker beside it.
var errInterestEventUndecodable = errors.New("undecodable, its transaction's residuals stay unexplained")

// decode returns the event's cause events; for one that does not decode, its CauseUndecodable marker together with
// an errInterestEventUndecodable error saying why. Any other error refuses the window.
func (d interestEventDecoder) decode(ev events.Event) ([]lendinginterest.Event, error) {
	proto, ok := d.protos[ev.ContractID]
	if !ok || !ev.InSuccessfulContractCall {
		return nil, nil // another contract's event, or one whose call's changes were rolled back
	}
	closed, err := ev.EventClosedAt()
	if err != nil {
		return nil, fmt.Errorf("lending interest: event %d %s/%d: %w", ev.Ledger, ev.TxHash, ev.EventIndex, err)
	}
	base := lendinginterest.Event{
		Pool: ev.ContractID, Ledger: ev.Ledger, TxHash: ev.TxHash, CloseTime: closed,
		OpIndex: uint32(ev.OperationIndex), EventIndex: uint32(ev.EventIndex), //nolint:gosec // non-negative positions
	}
	outs, err := d.decodeGuarded(ev, proto)
	var out []lendinginterest.Event
	if err == nil && proto == lendinginterest.BLNT {
		out, err = blntEvent(outs, base)
	} else if err == nil {
		out, err = blendEvents(outs, proto, base)
	}
	if err != nil {
		return []lendinginterest.Event{causeEvent(base, lendinginterest.CauseUndecodable, "", nil)},
			fmt.Errorf("lending interest: pool %s event %d %s/%d: %w: %w", ev.ContractID, ev.Ledger, ev.TxHash, ev.EventIndex,
				errInterestEventUndecodable, err)
	}
	return out, nil
}

// decodeGuarded runs the pool protocol's decoder under a recover: a panic is an undecodable event, never a skip.
func (d interestEventDecoder) decodeGuarded(ev events.Event, proto lendinginterest.Protocol) (outs []consumer.Event, err error) {
	var dec interface {
		Matches(events.Event) bool
		Decode(events.Event) ([]consumer.Event, error)
	} = d.blend
	if proto == lendinginterest.BLNT {
		dec = d.blnt
	}
	matched := false
	if perr := completeness.Guard(func() {
		if matched = dec.Matches(ev); matched {
			outs, err = dec.Decode(ev)
		}
	}); perr != nil {
		return nil, fmt.Errorf("decoder panicked: %w", perr)
	}
	if !matched {
		return nil, fmt.Errorf("not a %s event", proto)
	}
	return outs, err
}

func blendEvents(outs []consumer.Event, proto lendinginterest.Protocol, base lendinginterest.Event) ([]lendinginterest.Event, error) {
	var out []lendinginterest.Event
	for _, o := range outs {
		var es []lendinginterest.Event
		var err error
		switch e := o.(type) {
		case blend.FillAuctionEvent:
			es, err = blendFill(e, proto, base)
		case blend.EmissionEvent:
			if e.Kind != blend.EventGulp {
				continue
			}
			if proto != lendinginterest.BlendV2 || e.Amount == nil {
				return nil, fmt.Errorf("gulp on a %s pool, or without an amount", proto)
			}
			es = []lendinginterest.Event{causeEvent(base, lendinginterest.CauseGulp, e.Asset, new(big.Int).Set(e.Amount))}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	return out, nil
}

// blendFill is an interest fill's lot, negated per asset; V1's body has no lot, so its fill is attribution only.
func blendFill(e blend.FillAuctionEvent, proto lendinginterest.Protocol, base lendinginterest.Event) ([]lendinginterest.Event, error) {
	if e.AuctionType != blend.AuctionTypeInterest {
		return nil, nil
	}
	if (e.Data == nil) != (proto == lendinginterest.BlendV1) {
		return nil, fmt.Errorf("a %s pool's fill_auction with the other pool version's body", proto)
	}
	if e.Data == nil {
		base.Cause = lendinginterest.CauseInterestFillV1
		return []lendinginterest.Event{base}, nil
	}
	out := make([]lendinginterest.Event, 0, len(e.Data.Lot))
	for _, l := range e.Data.Lot {
		if l.Asset.Type != canonical.AssetSoroban || l.Amount == nil {
			return nil, fmt.Errorf("interest lot entry %v without a contract asset amount", l.Asset)
		}
		out = append(out, causeEvent(base, lendinginterest.CauseInterestFill, l.Asset.ContractID, new(big.Int).Neg(l.Amount)))
	}
	return out, nil
}

func blntEvent(outs []consumer.Event, base lendinginterest.Event) ([]lendinginterest.Event, error) {
	if len(outs) != 1 {
		return nil, fmt.Errorf("blnt decoded %d events", len(outs))
	}
	e, ok := outs[0].(blnt.Event)
	if !ok {
		return nil, fmt.Errorf("blnt decoded a %T", outs[0])
	}
	switch e.EventType {
	case "fill_auction":
		t, ok := e.Attributes["auction_type"].(uint32)
		if !ok {
			return nil, fmt.Errorf("fill_auction auction_type is %T, not a u32", e.Attributes["auction_type"])
		}
		if t != blend.AuctionTypeInterest {
			return nil, nil
		}
		return blntInterestLot(base, e.Attributes["filled_auction_data"])
	case "gulp":
		n, err := decimal(e.Amount)
		if err != nil {
			return nil, fmt.Errorf("gulp token_delta: %w", err)
		}
		return []lendinginterest.Event{causeEvent(base, lendinginterest.CauseGulp, e.Asset, n)}, nil
	case "reconcile_loss":
		s, _ := e.Attributes["backstop_credit_loss"].(string)
		n, err := decimal(s)
		if err != nil {
			return nil, fmt.Errorf("reconcile_loss backstop_credit_loss: %w", err)
		}
		return []lendinginterest.Event{causeEvent(base, lendinginterest.CauseReconcileLoss, e.Asset, n.Neg(n))}, nil
	}
	return nil, nil
}

// blntInterestLot reads filled_auction_data.lot as scval.ToJSON renders it: asset strkey → decimal amount.
func blntInterestLot(base lendinginterest.Event, data any) ([]lendinginterest.Event, error) {
	m, _ := data.(map[string]any)
	lot, ok := m["lot"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("interest fill without a filled_auction_data lot (%T)", data)
	}
	out := make([]lendinginterest.Event, 0, len(lot))
	for _, asset := range slices.Sorted(maps.Keys(lot)) {
		s, _ := lot[asset].(string)
		n, err := decimal(s)
		if err != nil {
			return nil, fmt.Errorf("interest lot %s: %w", asset, err)
		}
		out = append(out, causeEvent(base, lendinginterest.CauseInterestFill, asset, n.Neg(n)))
	}
	return out, nil
}

func causeEvent(base lendinginterest.Event, c lendinginterest.Cause, asset string, amount *big.Int) lendinginterest.Event {
	base.Cause, base.Asset, base.Amount = c, asset, amount
	return base
}

func decimal(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("amount %q is not a decimal integer", s)
	}
	return n, nil
}

// readInterestEvents reads and decodes [lo, hi]'s cause events of the pools, counting the rows read per ledger for
// the census; the read counts towards the window's version bound.
func (p *interestPass) readInterestEvents(ctx context.Context, lo, hi uint32, budget int) ([]lendinginterest.Event, map[uint32]uint64, error) {
	ids := make([]string, len(p.pools))
	for i, c := range p.pools {
		ids[i] = c.ID
	}
	var out []lendinginterest.Event
	reads := map[uint32]uint64{}
	err := lendingInterestEvents(ctx, p.addr, lo, hi, ids, func(ev events.Event) error {
		if len(out) >= budget {
			return errInterestWindowTooLarge
		}
		reads[ev.Ledger]++
		es, err := p.events.decode(ev)
		if errors.Is(err, errInterestEventUndecodable) {
			_, _ = fmt.Fprintf(p.log, "%s: %v\n", lendingInterestJob, err)
			err = nil
		}
		out = append(out, es...)
		return err
	})
	return out, reads, err
}

// lendingInterestUndecodableMetric counts, on the run's heartbeat, the pools' cause events it could not decode.
const lendingInterestUndecodableMetric = "stellarindex_lending_interest_undecodable_events"

// countUndecodable adds the folded window's undecodable markers to the run's gauge, once per folded read.
func (p *interestPass) countUndecodable(es []lendinginterest.Event) {
	for _, e := range es {
		if e.Cause == lendinginterest.CauseUndecodable {
			p.undecodable++
		}
	}
	p.hb.Gauge(lendingInterestUndecodableMetric, "Pool cause-topic events the lending interest run read and could not decode; "+
		"each leaves its transaction's residuals unexplained.", p.undecodable)
}

// eventsCovered proves the cause events of every ledger holding a version classified by them were read in full: the
// lake holds every event stellar.ledgers declares there, and exactly as many cause events of the pools as the
// pass read (reads, over this window and the carried open hour's).
func (p *interestPass) eventsCovered(ctx context.Context, rows []lendinginterest.HourRow, reads ...map[uint32]uint64) error {
	var ledgers []uint32
	for _, r := range rows {
		ledgers = append(ledgers, r.ResidualLedgers...)
	}
	slices.Sort(ledgers)
	ledgers = slices.Compact(ledgers)
	want := make(map[uint32]uint64, len(ledgers))
	for _, l := range ledgers {
		for _, r := range reads {
			want[l] += r[l]
		}
	}
	ids := make([]string, len(p.pools))
	for i, c := range p.pools {
		ids[i] = c.ID
	}
	return lendingInterestEventsCovered(ctx, p.addr, ids, want)
}
