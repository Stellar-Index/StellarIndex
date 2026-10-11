// Package coinbase streams live trade matches from Coinbase
// Exchange's public WebSocket feed, adding US price discovery for XLM/USD.
//
// We target Coinbase Exchange (no auth for matches/ticker), not Advanced Trade (OAuth), as the
// stable feed matching the Binance/Kraken/Bitstamp capability profile.
//
// Wire details: symbols are "XLM-USD" (dash); one subscribe message covers all product IDs, e.g.
// `{"type":"subscribe","channels":[{"name":"matches","product_ids":[...]}]}`. The `matches`
// channel publishes executed trades, and on subscribe Coinbase also sends one `last_match` per
// product, a real historical trade in the same shape. Numbers arrive as strings; time is RFC 3339
// with nanoseconds.
//
// Wire format reference:
// https://docs.cloud.coinbase.com/exchange/docs/websocket-channels
package coinbase

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SourceName is stamped on every canonical.Trade this package emits.
const SourceName = "coinbase"

// WSEndpoint is Coinbase Exchange's public WebSocket URL. No
// authentication for the matches channel.
const WSEndpoint = "wss://ws-feed.exchange.coinbase.com"

// Message `type` values on the Coinbase wire. We act on `match` and `last_match`; `subscriptions`
// is the subscribe ack and `error` a subscription failure. Other types are never subscribed.
const (
	TypeMatch         = "match"
	TypeLastMatch     = "last_match"
	TypeSubscriptions = "subscriptions"
	TypeError         = "error"
)

// ChannelName is the only channel subscribed; `ticker` and `level2` would add book depth the
// aggregator does not need.
const ChannelName = "matches"

// Compile-time assertion: venue's class matches external.ClassExchange.
var _ = external.ClassExchange

// Errors surfaced by the parser.
var (
	// ErrDustTrade: base x price floor-divided to a 0 quote amount. Tiny lots (e.g. 1e-8 XLM at
	// $0.16) underflow the canonical.NewAmount integer scale (10^8); real trades below our
	// precision floor, dropped silently rather than logged at ERROR.
	ErrDustTrade = errors.New("coinbase: dust trade (quote_amount underflow)")

	// ErrMalformedFrame — envelope or payload JSON didn't match
	// the expected shape. Single-frame skip; stream stays up.
	ErrMalformedFrame = errors.New("coinbase: malformed frame")

	// ErrUnknownProduct — match arrived on a product_id not in our PairMap. Defensive: Start
	// rejects misconfiguration upfront, so this means the venue sent a pair we did not ask for.
	ErrUnknownProduct = errors.New("coinbase: product_id not in configured PairMap")

	// ErrSubscriptionRejected — venue returned a `type:"error"`
	// frame in response to our subscribe. Usually a typo in a
	// product_id; treat as unrecoverable for this connection.
	ErrSubscriptionRejected = errors.New("coinbase: subscription rejected")
)
