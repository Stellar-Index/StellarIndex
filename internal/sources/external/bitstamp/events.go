// Package bitstamp streams live trades from Bitstamp's public
// WebSocket API. Adds EUR/GBP depth to the XLM market coverage
// Kraken already provides.
//
// Differences from Kraken that shape the code:
//
//   - Bitstamp requires ONE subscribe message per channel; we send N
//     subscribe frames sequentially after connect.
//   - Channel naming is "live_trades_xlmusd" (lowercase, concatenated).
//   - Bitstamp emits both float (price, amount) and string (price_str,
//     amount_str) forms. We use the string forms only: no floats on the
//     price path (ADR-0003).
//   - Microtimestamp is stringified microseconds since epoch.
//   - Roughly hourly the server sends `bts:request_reconnect` to rebalance
//     clients. We honour it by closing the connection; the backoff
//     reconnect picks up.
//
// Wire format reference: https://www.bitstamp.net/websocket/v2/
package bitstamp

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SourceName is stamped on every canonical.Trade this package emits.
// Must match the registry key in external.Registry.
const SourceName = "bitstamp"

// WSEndpoint is Bitstamp's public v2 WebSocket URL.
const WSEndpoint = "wss://ws.bitstamp.net"

// Event types on the Bitstamp wire. We only act on `trade`; the
// others either confirm state (subscription_succeeded), indicate
// errors (bts:error), or request we rebalance (bts:request_reconnect).
const (
	EventTrade                   = "trade"
	EventSubscriptionSucceeded   = "bts:subscription_succeeded"
	EventUnsubscriptionSucceeded = "bts:unsubscription_succeeded"
	EventRequestReconnect        = "bts:request_reconnect"
	EventError                   = "bts:error"
)

// ChannelPrefix is the naming convention for live-trade channels —
// "live_trades_<sym>" where sym is lowercase concatenated.
const ChannelPrefix = "live_trades_"

// Compile-time assertion: venue's class matches external.ClassExchange.
var _ = external.ClassExchange

// Errors surfaced by the parser. Transient WS errors live in
// streamer.go.
var (
	// ErrMalformedFrame — JSON shape didn't match the channel
	// envelope or the trade event payload. Single-frame skip.
	ErrMalformedFrame = errors.New("bitstamp: malformed frame")

	// ErrUnknownChannel — trade event arrived on a channel not in
	// the configured pair map. Defensive; Start rejects
	// misconfiguration upfront.
	ErrUnknownChannel = errors.New("bitstamp: channel not in configured PairMap")

	// ErrRequestedReconnect — venue asked us to reconnect. The
	// streamer closes the connection and re-enters the backoff
	// loop; not an error the caller sees but an internal signal.
	ErrRequestedReconnect = errors.New("bitstamp: server requested reconnect")

	// ErrSubscriptionRejected — venue answered a subscribe request
	// with `bts:error` (e.g. a delisted or malformed pair). Not
	// fatal: the socket and its other already-subscribed channels
	// stay live. The streamer logs it and flips
	// obs.CEXStreamSubscriptionRejected so a dead pair doesn't hide
	// behind a sibling's still-fresh CEXStreamLastTradeUnix.
	ErrSubscriptionRejected = errors.New("bitstamp: subscription rejected")

	// ErrDustTrade — base × price floor-divided to a 0 quote
	// amount. Tiny bitstamp lots (e.g. 1e-8 XLM at $0.16) underflow
	// the canonical.NewAmount integer scale (10^8). Real trades, but
	// below our precision floor — drop silently rather than logging
	// at ERROR. Same shape as the Coinbase + Binance dust filter.
	ErrDustTrade = errors.New("bitstamp: dust trade (quote_amount underflow)")
)
