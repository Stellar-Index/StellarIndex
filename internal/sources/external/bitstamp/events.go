// Package bitstamp streams live trades from Bitstamp's public WebSocket API, adding EUR/GBP depth
// to the XLM coverage Kraken provides.
//
// Differences from Kraken that shape the code:
//
//   - One subscribe message per channel, sent sequentially after connect.
//   - Channel naming is "live_trades_xlmusd" (lowercase, concatenated).
//   - Only the string forms (price_str, amount_str) are used: no floats on the price path (ADR-0003).
//   - Microtimestamp is stringified microseconds since epoch.
//   - Roughly hourly the server sends `bts:request_reconnect`; we close the connection and the
//     backoff reconnect picks up.
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

	// ErrSubscriptionRejected: the venue answered a subscribe with `bts:error` (e.g. a delisted
	// pair). Not fatal, other channels stay live. The streamer flips
	// obs.CEXStreamSubscriptionRejected so a dead pair does not hide behind a sibling's fresh
	// CEXStreamLastTradeUnix.
	ErrSubscriptionRejected = errors.New("bitstamp: subscription rejected")

	// ErrDustTrade: base × price floor-divides to a 0 quote amount (e.g. 1e-8 XLM at $0.16
	// underflows the canonical.NewAmount 10^8 scale). Real trades below our precision floor are
	// dropped silently, as in the Coinbase and Binance dust filters.
	ErrDustTrade = errors.New("bitstamp: dust trade (quote_amount underflow)")
)
