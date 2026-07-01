package binance

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/sources/external/scale"
)

// externalAmountDecimals is the fixed integer scale every off-chain
// source normalises to when populating canonical.Trade.BaseAmount /
// QuoteAmount. On-chain sources use per-asset decimals (XLM=7,
// Soroban tokens vary); off-chain venues quote in decimal strings
// with vendor-specific precision. Picking 10^8 uniformly:
//
//   - Matches crypto-native convention (most exchanges use ≤8 dp).
//   - Matches Redstone's on-chain price scale, so aggregator math
//     can mix on- and off-chain Amounts in the same VWAP if the
//     policy elects to.
//   - Headroom for typical FX precision (5–6 dp) without overflow.
//
// Aggregator queries must know which side of the boundary the trade
// came from; canonical.Trade.Source + external.Lookup(source).Class
// answers that cleanly — ClassExchange off-chain sources carry
// 10^8 scale, on-chain sources carry their native token scale.
const externalAmountDecimals = 8

// combinedFrame is the outer envelope every combined-stream message
// arrives in. The inner `data` field is the event payload; we
// dispatch on the stream name's suffix to pick the decoder.
type combinedFrame struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// aggTradePayload matches the Binance aggTrade event shape (event
// type "aggTrade"). Single-letter field names preserved from the
// wire — Binance optimises bandwidth on their real-time streams.
type aggTradePayload struct {
	EventType  string `json:"e"` // "aggTrade"
	EventTime  int64  `json:"E"` // ms since epoch (ignored — we use T)
	Symbol     string `json:"s"` // e.g. "XLMUSDT"
	AggTradeID int64  `json:"a"` // aggregate trade id (unique per symbol)
	Price      string `json:"p"` // decimal string, quote-per-base
	Quantity   string `json:"q"` // decimal string, base asset amount
	FirstID    int64  `json:"f"` // first underlying trade id (unused)
	LastID     int64  `json:"l"` // last underlying trade id (unused)
	TradeTime  int64  `json:"T"` // ms since epoch — authoritative trade timestamp
	IsMaker    bool   `json:"m"` // buyer was maker (seller-initiated trade)
}

// parseAggTradeFrame unmarshals a raw WS frame into canonical.Trade.
// Returns ErrMalformedFrame for JSON shape problems; ErrUnknownSymbol
// if the symbol isn't in pairMap.
//
// pairMap maps Binance symbols (uppercase, no separator) to their
// canonical.Pair. Supplied by the streamer at construction so the
// parser stays pure — no package-global state, trivial to unit-test.
func parseAggTradeFrame(raw []byte, pairMap map[string]canonical.Pair) (canonical.Trade, error) {
	var env combinedFrame
	if err := json.Unmarshal(raw, &env); err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: envelope: %w", ErrMalformedFrame, err)
	}
	if !strings.HasSuffix(env.Stream, "@aggTrade") {
		return canonical.Trade{}, fmt.Errorf("%w: stream %q is not an aggTrade channel",
			ErrMalformedFrame, env.Stream)
	}

	var ev aggTradePayload
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: data: %w", ErrMalformedFrame, err)
	}
	if ev.EventType != "aggTrade" {
		return canonical.Trade{}, fmt.Errorf("%w: unexpected event type %q", ErrMalformedFrame, ev.EventType)
	}
	pair, ok := pairMap[strings.ToUpper(ev.Symbol)]
	if !ok {
		return canonical.Trade{}, fmt.Errorf("%w: %q", ErrUnknownSymbol, ev.Symbol)
	}

	baseAmt, err := scale.DecimalStringToScaledInt(ev.Quantity, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: quantity %q: %w", ErrMalformedFrame, ev.Quantity, err)
	}
	priceScaled, err := scale.DecimalStringToScaledInt(ev.Price, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: price %q: %w", ErrMalformedFrame, ev.Price, err)
	}

	// quote = base × price, both at 10^8 scale, so raw product is
	// at 10^16 — divide by 10^8 to land at 10^8 consistently.
	quoteRaw := new(big.Int).Mul(baseAmt, priceScaled)
	quoteAmt := new(big.Int).Quo(quoteRaw, scale.Pow10(externalAmountDecimals))
	// Dust filter — same rationale as coinbase: tiny lots underflow
	// integer scale; drop silently rather than fail validation.
	if quoteAmt.Sign() == 0 {
		return canonical.Trade{}, ErrDustTrade
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0, // off-chain: no ledger. Aligns with other non-chain venues.
		TxHash:      formatTxHash(ev.Symbol, ev.AggTradeID),
		OpIndex:     0,
		Timestamp:   time.UnixMilli(ev.TradeTime).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmt),
		QuoteAmount: canonical.NewAmount(quoteAmt),
	}, nil
}

// formatTxHash synthesises a 64-hex-char identifier from the venue
// symbol + aggregate trade ID. canonical.Trade.Validate() requires a
// 64-char hex string; CEX trades have no natural Stellar-shaped hash,
// so we construct a stable one per aggregated fill. Symbol-scoped
// aggID is monotonic and globally unique across (symbol, id), so
// no collision risk in practice.
func formatTxHash(symbol string, aggID int64) string {
	s := fmt.Sprintf("%s-%020d", strings.ToUpper(symbol), aggID)
	var hex strings.Builder
	hex.Grow(64)
	for _, b := range []byte(s) {
		fmt.Fprintf(&hex, "%02x", b)
		if hex.Len() >= 64 {
			break
		}
	}
	for hex.Len() < 64 {
		hex.WriteByte('0')
	}
	return hex.String()[:64]
}
