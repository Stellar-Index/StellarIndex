package soroswap

import (
	"fmt"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
	"github.com/RatesEngine/rates-engine/internal/stellarrpc"
)

// RawPair is what a stellar-rpc event tells us about a swap/sync
// pair BEFORE we decode the SCVal payload. We collect these by
// (ledger, tx_hash, op_index) key, then finalise once we have both
// the swap + its sync.
type RawPair struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	Pair     string    // pair contract address (C…)
	ClosedAt time.Time // from event.ledgerClosedAt

	Swap *stellarrpc.Event // populated when we see a swap
	Sync *stellarrpc.Event // populated when we see a sync
}

// Complete reports whether this pair has both events and is ready
// to emit as a trade.
func (r RawPair) Complete() bool { return r.Swap != nil && r.Sync != nil }

// groupKey is the (ledger, tx_hash, op_index) tuple we use to
// correlate swap + sync. It's ordered so Go-map-ready.
type groupKey struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
}

func keyOf(e *stellarrpc.Event) groupKey {
	return groupKey{Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex)}
}

// classify decides what kind of Soroswap event this is by looking
// at topic[0]. Uses the pre-encoded Symbol blobs from events.go for
// byte-match speed (no XDR decode per event).
func classify(e *stellarrpc.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolSwap:
		return EventSwap
	case TopicSymbolSync:
		return EventSync
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolNewPair:
		return EventNewPair
	default:
		return ""
	}
}

// decodeSwap turns a swap+sync RawPair into a canonical.Trade. Needs
// the real XDR SCVal decoder to pull amounts out of the events'
// value fields — see the TODO(#0) below.
//
// Token0 / token1 asset identities come from the factory's
// new_pair event for this pair; the consumer holds that mapping
// in an in-memory cache.
func decodeSwap(r RawPair, tok0, tok1 canonical.Asset) (canonical.Trade, error) {
	if !r.Complete() {
		return canonical.Trade{}, ErrSwapWithoutSync
	}
	if r.Swap == nil {
		return canonical.Trade{}, fmt.Errorf("%w: swap nil", ErrMalformedPayload)
	}

	// TODO(#0): XDR-decode Swap.Value into
	//   { amount0_in, amount1_in, amount0_out, amount1_out, to }
	// and Sync.Value into
	//   { reserve0, reserve1 }
	// per the pair contract schema in soroswap-core/contracts/pair.
	//
	// Until we take the SDK dep, the stubs below return zero —
	// callers treat zero amounts as a decode failure and skip.
	amount0In, amount1In, err := decodeSwapAmounts(r.Swap.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("decode swap value: %w", err)
	}
	amount0Out, amount1Out, err := decodeSwapOutAmounts(r.Swap.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("decode swap outs: %w", err)
	}

	// Trade direction: whichever side had non-zero `in` is the base
	// asset the trader sold; the other side is what they bought.
	var base, quote canonical.Asset
	var baseAmt, quoteAmt canonical.Amount
	switch {
	case amount0In.Sign() > 0 && amount1Out.Sign() > 0:
		base, baseAmt = tok0, amount0In
		quote, quoteAmt = tok1, amount1Out
	case amount1In.Sign() > 0 && amount0Out.Sign() > 0:
		base, baseAmt = tok1, amount1In
		quote, quoteAmt = tok0, amount0Out
	default:
		return canonical.Trade{}, fmt.Errorf("%w: neither side had a positive in+out", ErrMalformedPayload)
	}

	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      r.Ledger,
		TxHash:      r.TxHash,
		OpIndex:     r.OpIndex,
		Timestamp:   r.ClosedAt,
		Pair:        pair,
		BaseAmount:  baseAmt,
		QuoteAmount: quoteAmt,
	}, nil
}

// ─── Stubs awaiting the SDK-backed decoder ─────────────────────────
// These compile and return zero-valued amounts; the unit tests
// substitute them via the decoderHooks indirection below so we can
// exercise the correlation logic without real XDR.

// decoderHooks lets tests inject fake decoders. In production these
// point at the real SDK-backed implementations.
var (
	decodeSwapAmounts    = stubDecodeSwapAmounts
	decodeSwapOutAmounts = stubDecodeSwapOutAmounts
)

func stubDecodeSwapAmounts(valueB64 string) (canonical.Amount, canonical.Amount, error) {
	// TODO(#0): replace with real SCVal -> (amount0_in, amount1_in)
	return canonical.Amount{}, canonical.Amount{}, nil
}

func stubDecodeSwapOutAmounts(valueB64 string) (canonical.Amount, canonical.Amount, error) {
	// TODO(#0): replace with real SCVal -> (amount0_out, amount1_out)
	return canonical.Amount{}, canonical.Amount{}, nil
}
