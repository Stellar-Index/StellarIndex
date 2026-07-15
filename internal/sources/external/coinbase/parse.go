package coinbase

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

const externalAmountDecimals = 8

// matchPayload covers both `match` and `last_match` message types —
// same field shape, differ only in semantics (last_match is one-per-
// product on subscribe, match is per-trade going forward).
//
// Numbers are strings natively in Coinbase's wire format; no
// json.Number/float complications.
type matchPayload struct {
	Type         string `json:"type"`
	TradeID      int64  `json:"trade_id"`
	MakerOrderID string `json:"maker_order_id"`
	TakerOrderID string `json:"taker_order_id"`
	Side         string `json:"side"`       // "buy" | "sell"
	Size         string `json:"size"`       // base quantity
	Price        string `json:"price"`      // quote-per-base
	ProductID    string `json:"product_id"` // "XLM-USD"
	Sequence     int64  `json:"sequence"`
	Time         string `json:"time"` // RFC 3339 with ns precision
}

// errorPayload is the shape of a `type:"error"` frame — usually a
// subscription rejection ("invalid product_id"). We surface this so
// the streamer can distinguish config bugs from network blips.
type errorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// parseFrame dispatches on the message `type` field. Returns
// (trade, true, nil) for match / last_match frames; (_, false, nil)
// for subscriptions-ack and unknown frames; wraps ErrSubscriptionRejected
// for subscription errors so the streamer fails cleanly rather than
// tight-looping on a bad config.
func parseFrame(raw []byte, pairMap map[string]canonical.Pair) (canonical.Trade, bool, error) {
	// Peek just the `type` field without double-decoding large
	// bodies — we don't have those here, but the pattern is cheap.
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return canonical.Trade{}, false, fmt.Errorf("%w: peek: %w", ErrMalformedFrame, err)
	}

	switch peek.Type {
	case TypeMatch, TypeLastMatch:
		var m matchPayload
		if err := json.Unmarshal(raw, &m); err != nil {
			return canonical.Trade{}, false, fmt.Errorf("%w: match: %w", ErrMalformedFrame, err)
		}
		tr, err := buildTrade(m, pairMap)
		if errors.Is(err, ErrDustTrade) {
			// Real coinbase trade below our integer-scale precision
			// floor (e.g. 1e-8 XLM). The canonical validator would
			// reject it; report as "no trade emitted" so the streamer
			// silently advances to the next frame.
			return canonical.Trade{}, false, nil
		}
		if err != nil {
			return canonical.Trade{}, false, err
		}
		return tr, true, nil
	case TypeSubscriptions:
		// ack — ignore
		return canonical.Trade{}, false, nil
	case TypeError:
		var e errorPayload
		if err := json.Unmarshal(raw, &e); err == nil {
			return canonical.Trade{}, false, fmt.Errorf("%w: %s %s",
				ErrSubscriptionRejected, e.Message, e.Reason)
		}
		return canonical.Trade{}, false, ErrSubscriptionRejected
	}
	// Heartbeats / tickers / unknown — drop.
	return canonical.Trade{}, false, nil
}

// buildTrade converts one decoded match into a canonical.Trade.
func buildTrade(m matchPayload, pairMap map[string]canonical.Pair) (canonical.Trade, error) {
	pair, ok := pairMap[strings.ToUpper(m.ProductID)]
	if !ok {
		return canonical.Trade{}, fmt.Errorf("%w: %q", ErrUnknownProduct, m.ProductID)
	}

	base, err := scale.DecimalStringToScaledInt(m.Size, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: size %q: %w", ErrMalformedFrame, m.Size, err)
	}
	price, err := scale.DecimalStringToScaledInt(m.Price, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: price %q: %w", ErrMalformedFrame, m.Price, err)
	}
	quote := new(big.Int).Quo(new(big.Int).Mul(base, price), scale.Pow10(externalAmountDecimals))
	// Dust filter — when base × price floor-divides to 0 (e.g. a
	// 1e-8 XLM lot at $0.16, or any size where size_float ×
	// price_float < 1e-8 USD), the canonical validator would
	// reject the row with "quote_amount must be positive". These
	// are real coinbase trades, just below our integer-scale
	// precision floor; drop silently.
	if quote.Sign() == 0 {
		return canonical.Trade{}, ErrDustTrade
	}

	ts, err := time.Parse(time.RFC3339Nano, m.Time)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: time %q: %w", ErrMalformedFrame, m.Time, err)
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0,
		TxHash:      formatTxHash(m.ProductID, m.TradeID),
		OpIndex:     0,
		Timestamp:   ts.UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}

// formatTxHash — 64-char hex from (product_id, trade_id). Dash
// stripped for consistency with the Kraken "/"-stripping rule.
func formatTxHash(productID string, tradeID int64) string {
	normalised := strings.ReplaceAll(strings.ToUpper(productID), "-", "")
	s := fmt.Sprintf("%s-%020d", normalised, tradeID)
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
