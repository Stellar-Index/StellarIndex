// Package coinbase streams live trade matches from Coinbase
// Exchange's public WebSocket feed, adding US price discovery for XLM/USD.
//
// We target Coinbase Exchange (the ex-Pro API: no auth for matches/ticker),
// not Advanced Trade (OAuth, rate-limited when unauthenticated): Exchange is
// the stable institutional feed and matches the Binance/Kraken/Bitstamp
// capability profile.
//
// Wire details:
//
//   - Symbol format is "XLM-USD" (dash).
//   - One subscribe message covers all product IDs:
//     `{"type":"subscribe","channels":[{"name":"matches","product_ids":[...]}]}`.
//   - The `matches` channel publishes executed trades. On subscribe
//     Coinbase also sends one `last_match` per product, which is a real
//     historical trade in the same shape as live matches.
//   - Numbers arrive as strings; time is RFC 3339 with nanoseconds.
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

// Message `type` values on the Coinbase wire. We act on `match`
// and `last_match`; `subscriptions` is the subscribe ack; `error`
// is a subscription failure; others (heartbeat, ticker, l2update)
// we never subscribe to in v1.
const (
	TypeMatch         = "match"
	TypeLastMatch     = "last_match"
	TypeSubscriptions = "subscriptions"
	TypeError         = "error"
)

// ChannelName — only `matches` is subscribed in v1. `ticker` and
// `level2` channels exist and would add book-depth detail; defer
// until the aggregator needs it.
const ChannelName = "matches"

// Compile-time assertion: venue's class matches external.ClassExchange.
var _ = external.ClassExchange

// Errors surfaced by the parser.
var (
	// ErrDustTrade — base × price floor-divided to a 0 quote
	// amount. Tiny coinbase lots (e.g. 1e-8 XLM at $0.16) underflow
	// the canonical.NewAmount integer scale (10^8). Real trades, but
	// below our precision floor — drop silently rather than logging
	// at ERROR.
	ErrDustTrade = errors.New("coinbase: dust trade (quote_amount underflow)")

	// ErrMalformedFrame — envelope or payload JSON didn't match
	// the expected shape. Single-frame skip; stream stays up.
	ErrMalformedFrame = errors.New("coinbase: malformed frame")

	// ErrUnknownProduct — match arrived on a product_id not in
	// our PairMap. Defensive: Start rejects subscription-level
	// misconfiguration upfront, so hitting this at runtime means
	// Coinbase listed a new pair we're implicitly subscribed to.
	ErrUnknownProduct = errors.New("coinbase: product_id not in configured PairMap")

	// ErrSubscriptionRejected — venue returned a `type:"error"`
	// frame in response to our subscribe. Usually a typo in a
	// product_id; treat as unrecoverable for this connection.
	ErrSubscriptionRejected = errors.New("coinbase: subscription rejected")
)
