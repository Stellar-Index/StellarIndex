// Package kraken streams live trades from Kraken's public WebSocket v2 trade channel. XLM/USD,
// XLM/EUR and XLM/GBP are all natively quoted (no stablecoin proxy).
//
// Differences from Binance that shape the code:
//   - Symbol format is "XLM/USD" (slash-separated, uppercase); pairs.go normalises both sides.
//   - Subscription is an explicit JSON method call after connect.
//   - Numbers arrive as JSON floats; decoding via [encoding/json.Number] keeps the original decimal
//     string and bypasses float entirely (ADR-0003).
//
// Wire format reference: https://docs.kraken.com/api/docs/websocket-v2/trade
//
// The snapshot (last ~50 trades) carries real timestamps and is emitted like any trade; a
// re-delivered snapshot dedupes on the synthesised tx_hash (symbol + trade_id). BackfillTrades
// derives the same tx_hash; candles key on close time and never match a live row, so
// backfill-external refuses a window that already holds rows.
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

// Errors surfaced by the parser. Transient WS errors live in
// streamer.go and never escape as sentinels — they're logged and
// reconnected against.
var (
	// ErrMalformedFrame — JSON shape didn't match either the
	// channel envelope or one of the known payload types. Single-
	// frame skip; stream stays up.
	ErrMalformedFrame = errors.New("kraken: malformed frame")

	// ErrUnknownSymbol — trade's symbol isn't in the configured
	// PairMap. Happens if Kraken lists a new pair we haven't
	// added, or if we subscribed to a symbol that wasn't in the
	// map at construct time (defensive — Start rejects this path).
	ErrUnknownSymbol = errors.New("kraken: symbol not in configured PairMap")

	// ErrDustTrade means base x price floor-divided to a 0 quote amount: tiny lots underflow the
	// canonical.NewAmount integer scale (10^8). Real trades below our precision floor, dropped
	// silently rather than logged at ERROR, as in the other venues' dust filters.
	ErrDustTrade = errors.New("kraken: dust trade (quote_amount underflow)")
)
