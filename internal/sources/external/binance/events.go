// Package binance streams live aggregated trades from Binance's
// public combined-stream WebSocket endpoint and translates them into
// canonical.Trade values.
//
// aggTrade, not raw trades: `@aggTrade` merges consecutive fills at the
// same price in the same millisecond, which is lossless for VWAP (volume is
// preserved) at ~5-10× lower throughput than the per-fill `@trade` stream.
//
// Wire format (verified against
// https://developers.binance.com/docs/binance-spot-api-docs/web-socket-streams):
//
//	wss://stream.binance.com:9443/stream?streams=<sym1>@aggTrade/<sym2>@aggTrade
//
// Each frame:
//
//	{
//	  "stream": "xlmusdt@aggTrade",
//	  "data": {
//	    "e": "aggTrade",       // event type
//	    "E": 1745000000000,    // event time (ms)
//	    "s": "XLMUSDT",        // symbol
//	    "a": 123456,           // aggregate trade ID
//	    "p": "0.1758",         // price (string, exact decimal)
//	    "q": "152.34",         // quantity in base (string)
//	    "f": 12345,            // first underlying trade id
//	    "l": 12399,            // last underlying trade id
//	    "T": 1745000000000,    // trade time (ms — the ledger-close-equivalent)
//	    "m": true              // buyer was maker (→ trade was seller-initiated)
//	  }
//	}
//
// Symbols are base+quote with no separator, uppercase (XLMUSDT); the
// normalizer maps them through a hardcoded pair map.
//
// An XLMUSDT trade emits as canonical.Trade{Pair: XLM/USDT}: stablecoins
// are mapped to fiat by the aggregator at compute time, never at ingest,
// so a depeg stays visible in the data.
package binance

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SourceName is stamped on every canonical.Trade this package emits.
// Must match the registry key in external.Registry.
const SourceName = "binance"

// WSEndpoint is the public combined-stream entry point. No auth, no
// API key needed for spot market data streams.
const WSEndpoint = "wss://stream.binance.com:9443/stream"

// Compile-time assertion: the constant matches external.ClassExchange.
var _ = external.ClassExchange

// Errors surfaced by parse path. Transient connection errors live in
// streamer.go and never escape as package-level sentinels — they're
// logged + metered + retried inside Start.
var (
	// ErrDustTrade — base × price floor-divided to a 0 quote amount.
	// Real binance trade below our 10^8 integer-scale precision
	// floor. Drop silently rather than logging at ERROR.
	ErrDustTrade = errors.New("binance: dust trade (quote_amount underflow)")

	// ErrMalformedFrame — frame didn't decode to the aggTrade shape
	// we expect. Single-frame skip; logged and counted, doesn't
	// abort the stream.
	ErrMalformedFrame = errors.New("binance: malformed aggTrade frame")

	// ErrUnknownSymbol — frame's symbol isn't in our pair map.
	// Happens if Binance adds a new listing we haven't configured,
	// or if we subscribe to a stream the exchange later renames.
	// Per-frame skip.
	ErrUnknownSymbol = errors.New("binance: symbol not in configured pair map")
)
