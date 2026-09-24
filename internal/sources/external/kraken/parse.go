package kraken

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// externalAmountDecimals mirrors the Binance constant: every
// off-chain source normalises to 10^8 integer scale.
const externalAmountDecimals = 8

// channelEnvelope is the shared outer shape for every v2 message
// (trade, heartbeat, status, subscribe-ack, error). We dispatch on
// Channel first, then on Type (for trade frames — snapshot vs
// update) to pick the decoder.
type channelEnvelope struct {
	Channel string          `json:"channel"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
	// Method / Success present on subscribe acks. Error and the
	// top-level Symbol are present on a per-symbol rejection; an
	// accepted ack names its symbol under Result.
	Method  string          `json:"method,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	Symbol  string          `json:"symbol,omitempty"`
	Result  *ackResult      `json:"result,omitempty"`
}

type ackResult struct {
	Channel string `json:"channel"`
	Symbol  string `json:"symbol"`
}

const methodSubscribe = "subscribe"

// Reasons a trade entry inside a well-formed frame is skipped. Each
// is a member of obs.CEXStreamEntrySkipReasons (lockstep-tested).
const (
	skipUnknownSymbol = "unknown_symbol"
	skipBadQty        = "bad_qty"
	skipBadPrice      = "bad_price"
	skipBadTimestamp  = "bad_timestamp"
	skipBadTradeID    = "bad_trade_id"
	skipOther         = "other"
)

var skipReasons = []string{
	skipUnknownSymbol, skipBadQty, skipBadPrice, skipBadTimestamp, skipBadTradeID, skipOther,
}

// entryError tags a buildTrade failure with its skip reason.
type entryError struct {
	reason string
	err    error
}

func (e *entryError) Error() string { return e.err.Error() }
func (e *entryError) Unwrap() error { return e.err }

// entrySkip is one trade entry parseTradeFrame could not convert.
type entrySkip struct {
	Reason string
	Err    error
}

// subscribeAck is Kraken's per-symbol answer to our subscribe request.
type subscribeAck struct {
	Symbol   string
	Accepted bool
	Message  string // the venue's error text on rejection
}

// frameResult is everything one wire frame decodes to. Skips and Ack
// carry the only signal a renamed or de-listed pair produces.
type frameResult struct {
	Trades []canonical.Trade
	Skips  []entrySkip
	Ack    *subscribeAck
}

// tradePayload is one entry in a v2 trade frame's `data` array.
// Field names verified against
// docs.kraken.com/api/docs/websocket-v2/trade (2026-04-24).
//
// qty / price are json.Number so the decimal-string form reaches
// our scaling helper losslessly — float64 is fine at Kraken's
// precision but the i128 invariant says no floats for price paths.
type tradePayload struct {
	Symbol    string      `json:"symbol"`
	Side      string      `json:"side"`      // "buy" | "sell" (unused for price/volume, retained for future attribution)
	Qty       json.Number `json:"qty"`       // base-asset quantity, decimal
	Price     json.Number `json:"price"`     // quote-per-base, decimal
	OrdType   string      `json:"ord_type"`  // "market" | "limit" (unused)
	TradeID   int64       `json:"trade_id"`  // per-symbol monotonic
	Timestamp string      `json:"timestamp"` // RFC 3339 with subsecond precision
}

// parseFrame dispatches on the channel field; trade frames yield
// zero or more canonical.Trade values (snapshot carries many,
// update carries one or a few) plus the entries that were skipped.
// A subscribe acknowledgement yields Ack. Heartbeat / status frames
// yield an empty result.
//
// Malformed frames return ErrMalformedFrame wrapped; the streamer
// counts and continues.
func parseFrame(raw []byte, pairMap map[string]canonical.Pair) (frameResult, error) {
	var env channelEnvelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve the decimal representation of qty/price
	if err := dec.Decode(&env); err != nil {
		return frameResult{}, fmt.Errorf("%w: envelope: %w", ErrMalformedFrame, err)
	}

	switch env.Channel {
	case ChannelTrade:
		return parseTradeFrame(env, pairMap)
	case ChannelHeartbeat, ChannelStatus:
		return frameResult{}, nil
	}

	if env.Method == methodSubscribe && env.Success != nil {
		return frameResult{Ack: parseSubscribeAck(env)}, nil
	}
	return frameResult{}, nil
}

// parseSubscribeAck reads the venue's verdict on one subscribed
// symbol. A rejection names the symbol at the top level; an accepted
// ack names it under result.
func parseSubscribeAck(env channelEnvelope) *subscribeAck {
	ack := &subscribeAck{Symbol: env.Symbol, Accepted: *env.Success}
	if env.Result != nil && env.Result.Symbol != "" {
		ack.Symbol = env.Result.Symbol
	}
	if !ack.Accepted {
		var msg string
		if err := json.Unmarshal(env.Error, &msg); err != nil {
			msg = string(env.Error)
		}
		ack.Message = msg
	}
	return ack
}

// parseTradeFrame decodes the `data` array of a trade channel
// frame. Each entry is one trade; all share the same channel+type
// metadata. A bad entry is skipped and reported, not fatal to the
// frame.
func parseTradeFrame(env channelEnvelope, pairMap map[string]canonical.Pair) (frameResult, error) {
	if len(env.Data) == 0 {
		return frameResult{}, nil
	}
	var items []tradePayload
	dec := json.NewDecoder(bytes.NewReader(env.Data))
	dec.UseNumber()
	if err := dec.Decode(&items); err != nil {
		return frameResult{}, fmt.Errorf("%w: trade data: %w", ErrMalformedFrame, err)
	}
	res := frameResult{Trades: make([]canonical.Trade, 0, len(items))}
	for _, t := range items {
		trade, err := buildTrade(t, pairMap)
		if errors.Is(err, ErrDustTrade) {
			continue // a real trade below the 10^8 precision floor
		}
		if err != nil {
			reason := skipOther
			var ee *entryError
			if errors.As(err, &ee) {
				reason = ee.reason
			}
			res.Skips = append(res.Skips, entrySkip{Reason: reason, Err: err})
			continue
		}
		res.Trades = append(res.Trades, trade)
	}
	return res, nil
}

// buildTrade turns one decoded tradePayload into a canonical.Trade.
func buildTrade(t tradePayload, pairMap map[string]canonical.Pair) (canonical.Trade, error) {
	pair, ok := pairMap[strings.ToUpper(t.Symbol)]
	if !ok {
		return canonical.Trade{}, &entryError{skipUnknownSymbol, fmt.Errorf("%w: %q", ErrUnknownSymbol, t.Symbol)}
	}

	base, err := scale.DecimalStringToScaledInt(t.Qty.String(), externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, &entryError{skipBadQty, fmt.Errorf("%w: qty %q: %w", ErrMalformedFrame, t.Qty.String(), err)}
	}
	price, err := scale.DecimalStringToScaledInt(t.Price.String(), externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, &entryError{skipBadPrice, fmt.Errorf("%w: price %q: %w", ErrMalformedFrame, t.Price.String(), err)}
	}
	// quote = base × price / 10^8
	quoteRaw := new(big.Int).Mul(base, price)
	quote := new(big.Int).Quo(quoteRaw, scale.Pow10(externalAmountDecimals))

	// Dust filter — when base × price floor-divides to 0 (e.g.
	// a 1e-8 XLM lot at $0.16), the canonical validator rejects
	// the row with "quote_amount must be positive, got 0". These
	// are real Kraken trades, just below our integer-scale
	// precision floor; drop silently. Same shape as the
	// Coinbase + Binance + Bitstamp dust filter.
	if quote.Sign() == 0 {
		return canonical.Trade{}, ErrDustTrade
	}

	ts, err := time.Parse(time.RFC3339Nano, t.Timestamp)
	if err != nil {
		return canonical.Trade{}, &entryError{skipBadTimestamp, fmt.Errorf("%w: timestamp %q: %w", ErrMalformedFrame, t.Timestamp, err)}
	}

	txHash, err := formatTxHash(t.Symbol, t.TradeID)
	if err != nil {
		return canonical.Trade{}, &entryError{skipBadTradeID, err}
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0, // no ledger off-chain
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   ts.UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}

// formatTxHash — see binance.formatTxHash for rationale. 64-char
// hex synthesised from (symbol, trade_id) for canonical.Trade
// validation; a slash-stripped symbol over 11 bytes would truncate
// trade_id, so it is refused.
func formatTxHash(symbol string, tradeID int64) (string, error) {
	// Normalise symbol — strip slash so the hash matches regardless
	// of how Kraken formats it. "XLM/USD" and "XLMUSD" yield the
	// same underlying bytes prefix; safe for dedup across potential
	// future alias changes.
	normalised := strings.ReplaceAll(strings.ToUpper(symbol), "/", "")
	return scale.StrictSyntheticTxHash(fmt.Sprintf("%s-%020d", normalised, tradeID))
}
