package sdex

import (
	"errors"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Decoder is the dispatcher-facing OpDecoder for classic SDEX trades, working on xdr.Operation + OperationResult pairs via dispatcher.OpContext; stateless, since every claim atom is self-contained.
type Decoder struct{}

// NewDecoder constructs an SDEX Decoder.
func NewDecoder() *Decoder { return &Decoder{} }

// Name implements [dispatcher.OpDecoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.OpDecoder]: true for every op type that can produce ClaimAtoms (see matchesTradeOp for the authoritative list).
func (*Decoder) Matches(op xdr.Operation) bool {
	return matchesTradeOp(op)
}

// Decode implements [dispatcher.OpDecoder]: zero trades for ops that didn't cross the book, one per claim atom otherwise. Taker is the tx source; Maker comes from each claim atom (a G-address, or the hex pool hash for liquidity-pool fills).
func (d *Decoder) Decode(ctx dispatcher.OpContext) ([]consumer.Event, error) {
	out, _ := d.decodeCounted(ctx)
	return out, nil
}

// DecodeCounted is Decode plus the count of claim atoms that failed to decode (errors other than [ErrNoOpClaim]), so the completeness census marks a ledger BLIND rather than "zero trades, clean"; a separate method because Decode cannot report it without breaking the dispatcher.OpDecoder signature.
func (d *Decoder) DecodeCounted(ctx dispatcher.OpContext) ([]consumer.Event, int) {
	return d.decodeCounted(ctx)
}

func (*Decoder) decodeCounted(ctx dispatcher.OpContext) ([]consumer.Event, int) {
	atoms := extractClaimAtoms(ctx.Op, ctx.OpResult)
	if len(atoms) == 0 {
		return nil, 0
	}
	taker := ctx.TxSource
	if ctx.OpSource != "" {
		// Per-op source overrides tx-level. Rare (most ops don't
		// set their own source), but the XDR allows it and some
		// advanced callers use it.
		taker = ctx.OpSource
	}

	var failed int
	out := make([]consumer.Event, 0, len(atoms))
	for i, atom := range atoms {
		trade, err := decodeClaimAtom(
			atom,
			ctx.Ledger,
			ctx.ClosedAt,
			ctx.TxHash,
			ctx.OpIndex,
			i,
			taker,
		)
		if errors.Is(err, ErrNoOpClaim) {
			continue
		}
		if err != nil {
			// Count the failure HERE: the dispatcher only counts a non-nil return from Decode,
			// and this function swallows per-claim errors, so the decode-errors metric would
			// never move. The drop is otherwise invisible: sdexclaim.IsRealTrade mirrors the
			// reject, so the census and the lake's classic_trade_effect_count drop the atom too
			// and the reconcile nets to zero. Still `continue`, not a returned error: the
			// other claims in this op are independently valid.
			obs.SourceDecodeErrorsTotal.WithLabelValues(SourceName).Inc()
			failed++
			continue
		}
		out = append(out, TradeEvent{Trade: trade})
	}
	return out, failed
}

// TradeEvent is the [consumer.Event] this source emits, like soroswap.TradeEvent / aquarius.TradeEvent.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "sdex.trade" }

// Source implements [consumer.Event].
func (TradeEvent) Source() string { return SourceName }

// Compile-time checks — catches interface drift at build time.
var (
	_ dispatcher.OpDecoder = (*Decoder)(nil)
	_ consumer.Event       = TradeEvent{}
)
