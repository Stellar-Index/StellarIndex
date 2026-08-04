package sdex

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Decoder is the dispatcher-facing OpDecoder for classic SDEX
// trades. Unlike the Soroban sources (events.Event-driven) SDEX
// operates on xdr.Operation + xdr.OperationResult pairs via
// dispatcher.OpContext.
//
// Stateless — every claim atom is self-contained. The Decoder
// owns no correlation buffer; a single op may emit N trades
// (one per claim atom) but all of them come from the same
// OpContext in one Decode call.
type Decoder struct{}

// NewDecoder constructs an SDEX Decoder.
func NewDecoder() *Decoder { return &Decoder{} }

// Name implements [dispatcher.OpDecoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.OpDecoder]. True for every op
// type that can produce ClaimAtoms: ManageOffer (sell/buy),
// CreatePassiveSellOffer, PathPayment (strict-receive + strict-
// send). See matchesTradeOp for the authoritative list.
func (*Decoder) Matches(op xdr.Operation) bool {
	return matchesTradeOp(op)
}

// Decode implements [dispatcher.OpDecoder]. Returns zero trades
// for ops that didn't cross the book (failed op, or offer rested
// without claiming anything), one trade per claim atom otherwise.
//
// The Taker on every emitted trade is the tx source account
// (the buyer-side actor who placed the aggressive offer). Maker
// comes from each claim atom — usually a G-address, but for
// liquidity-pool fills it's the pool hash (hex-encoded).
func (*Decoder) Decode(ctx dispatcher.OpContext) ([]consumer.Event, error) {
	atoms := extractClaimAtoms(ctx.Op, ctx.OpResult)
	if len(atoms) == 0 {
		return nil, nil
	}
	taker := ctx.TxSource
	if ctx.OpSource != "" {
		// Per-op source overrides tx-level. Rare (most ops don't
		// set their own source), but the XDR allows it and some
		// advanced callers use it.
		taker = ctx.OpSource
	}

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
		if err != nil {
			// Count the per-claim failure HERE. The previous comment
			// said "counted by the dispatcher", but the dispatcher only
			// counts a non-nil return from Decode — and this function
			// has no such return: every per-claim error is swallowed by
			// this continue and both exits are `nil`. So
			// stellarindex_source_decode_errors_total{source="sdex"}
			// was structurally incapable of ever leaving zero, on the
			// highest-volume source in the system (~925k events/day),
			// while events.go, the README's "decode-error budget", and
			// the sustained-rate alert all name that metric as THE
			// signal for exactly this.
			//
			// It matters because the drop is otherwise invisible in
			// every direction: sdexclaim.IsRealTrade mirrors the same
			// reject, so the census and the lake's
			// classic_trade_effect_count drop the atom too and the
			// ADR-0033 reconcile nets to zero — a new ClaimAtom variant
			// or an unparseable asset code would go on losing real
			// trades indefinitely with a green verdict (cold audit
			// 2026-08-04).
			//
			// Still `continue`, not a returned error: the other claims
			// in this op are independently valid and a returned error
			// would drop them too.
			obs.SourceDecodeErrorsTotal.WithLabelValues(SourceName).Inc()
			continue
		}
		out = append(out, TradeEvent{Trade: trade})
	}
	return out, nil
}

// TradeEvent is the [consumer.Event] this source emits. Matches
// the pattern of soroswap.TradeEvent / aquarius.TradeEvent etc.
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
