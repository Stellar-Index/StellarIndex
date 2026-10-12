package binance

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// externalAmountDecimals is the fixed integer scale (10^8) off-chain sources normalise Trade.BaseAmount / QuoteAmount to; on-chain
// sources use per-asset decimals. 10^8 matches crypto convention and Redstone's on-chain price scale, with headroom for FX (5-6 dp).
// Aggregators must check Trade.Source + external.Lookup(source).Class for the scale.
const externalAmountDecimals = 8

// combinedFrame is the outer envelope of every combined-stream message; the inner `data` is the payload, decoded by the stream name's
// suffix.
type combinedFrame struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// aggTradePayload matches the Binance aggTrade event; single-letter field names are the wire's (bandwidth optimisation).
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

// parseAggTradeFrame unmarshals a raw WS frame into canonical.Trade; ErrMalformedFrame for bad JSON shape, ErrUnknownSymbol if absent
// from pairMap (uppercase Binance symbol -> pair, supplied by the streamer so the parser stays pure).
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
	if ev.TradeTime == 0 {
		// canonical.Trade.Validate only rejects the zero time.Time, and
		// time.UnixMilli(0) (1970-01-01) is not that — so a missing or
		// literal-zero "T" must be caught here, not relied on downstream.
		return canonical.Trade{}, fmt.Errorf("%w: missing or zero trade time", ErrMalformedFrame)
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

	txHash, err := formatTxHash(ev.Symbol, ev.AggTradeID)
	if err != nil {
		return canonical.Trade{}, err
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0, // off-chain: no ledger. Aligns with other non-chain venues.
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   time.UnixMilli(ev.TradeTime).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmt),
		QuoteAmount: canonical.NewAmount(quoteAmt),
	}, nil
}

// formatTxHash synthesises a 64-hex tx identifier from venue symbol + aggregate trade ID (Trade.Validate requires one; CEX trades have
// none). A symbol over 11 bytes is refused: it would truncate aggID's low digits and break uniqueness.
func formatTxHash(symbol string, aggID int64) (string, error) {
	return scale.StrictSyntheticTxHash(fmt.Sprintf("%s-%020d", strings.ToUpper(symbol), aggID))
}
