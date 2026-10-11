// Package kraken streams live trades from Kraken's public WebSocket v2 trade channel. XLM/USD,
// XLM/EUR and XLM/GBP are all natively quoted (no stablecoin proxy).
//
// Symbols are "XLM/USD" (slash-separated, uppercase) and subscription is an explicit JSON call
// after connect. Numbers arrive as JSON floats; [encoding/json.Number] keeps the decimal string
// so no float is involved (ADR-0003).
//
// Wire format reference: https://docs.kraken.com/api/docs/websocket-v2/trade
//
// The snapshot (last ~50 trades) is emitted like any trade; a re-delivered snapshot dedupes on
// the synthesised tx_hash (symbol + trade_id), which BackfillTrades derives identically. Candles
// key on close time and never match a live row, so backfill-external refuses a window that
// already holds rows.
package kraken

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SourceName is stamped on every canonical.Trade this package emits.
// Must match the registry key in external.Registry.
const SourceName = "kraken"

// WSEndpoint is Kraken's public v2 WebSocket URL. No auth for the
// trade channel.
const WSEndpoint = "wss://ws.kraken.com/v2"

// Channel names on the Kraken v2 stream. We only act on `trade`; the
// others we log-and-ignore so the stream stays open through
// subscription-ack, heartbeat, and status frames.
const (
	ChannelTrade     = "trade"
	ChannelHeartbeat = "heartbeat"
	ChannelStatus    = "status"
)

// Compile-time assertion: the constant matches external.ClassExchange.
var _ = external.ClassExchange

// Errors surfaced by the parser. Transient WS errors stay in streamer.go: they are logged and
// reconnected, never returned as sentinels.
var (
	// ErrMalformedFrame means the JSON matched neither the channel envelope nor a known payload;
	// the single frame is skipped and the stream stays up.
	ErrMalformedFrame = errors.New("kraken: malformed frame")

	// ErrUnknownSymbol means the trade's symbol is not in the configured PairMap, e.g. Kraken
	// listed a new pair we have not added.
	ErrUnknownSymbol = errors.New("kraken: symbol not in configured PairMap")

	// ErrDustTrade means base x price floor-divided to a 0 quote amount: tiny lots underflow the
	// canonical.NewAmount integer scale (10^8). Real trades below our precision floor, dropped
	// silently rather than logged at ERROR, as in the other venues' dust filters.
	ErrDustTrade = errors.New("kraken: dust trade (quote_amount underflow)")
)
