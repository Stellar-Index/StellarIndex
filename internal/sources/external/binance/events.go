// Package binance streams live aggregated trades from Binance's public combined-stream WebSocket
// endpoint and translates them into canonical.Trade values.
//
// It uses `@aggTrade`, which merges consecutive same-price fills in the same millisecond: lossless
// for VWAP (volume is preserved) at ~5-10x lower throughput than the per-fill `@trade` stream.
//
// Each frame is {"stream": "xlmusdt@aggTrade", "data": {...}}; data carries s (symbol), p and q
// (exact decimal strings), T (trade time, ms) and m (buyer was maker, so the trade was
// seller-initiated). Symbols have no separator (XLMUSDT) and map through a hardcoded pair map.
//
// An XLMUSDT trade emits as canonical.Trade{Pair: XLM/USDT}: the aggregator maps stablecoins to
// fiat at compute time, never at ingest, so a depeg stays visible in the data.
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

// Parse-path errors. Transient connection errors stay inside Start (logged, metered, retried).
var (
	// ErrDustTrade means base x price floor-divided to a zero quote amount, below the 10^8 scale
	// floor. It is dropped silently rather than logged at ERROR.
	ErrDustTrade = errors.New("binance: dust trade (quote_amount underflow)")

	// ErrMalformedFrame means the frame did not decode to the aggTrade shape; the frame is skipped
	// and counted without aborting the stream.
	ErrMalformedFrame = errors.New("binance: malformed aggTrade frame")

	// ErrUnknownSymbol means the symbol is not in the pair map (a new listing or a renamed
	// stream); the frame is skipped.
	ErrUnknownSymbol = errors.New("binance: symbol not in configured pair map")
)
