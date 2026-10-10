// Package exchangeratesapi polls exchangeratesapi.io's REST endpoint
// for fiat reference rates.
//
// It emits canonical.OracleUpdate, not Trade: an FX reference rate is a
// computed benchmark, not an executed trade. The triangulation layer uses
// it (`XLM/USD × USD/EUR = XLM/EUR`) when no venue trades a pair directly.
// Its registry class is ClassExchange, a first-party computation.
//
// Professional tier is the minimum: it is the first with a USD base and
// redistribution rights. The free tier (EUR base only) is rejected at
// startup, because a EUR-only base would force every FX consumer to
// triangulate through EUR.
//
// Wire format verified against
// https://exchangeratesapi.io/documentation:
//
//	GET https://api.exchangeratesapi.io/v1/latest?access_key=KEY&base=USD&symbols=EUR,GBP,JPY,...
//
//	{"success": true, "timestamp": 1745000000, "base": "USD",
//	 "date": "YYYY-MM-DD", "rates": {"EUR": 0.92350, "GBP": 0.78450}}
package exchangeratesapi

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SourceName is stamped on every canonical.OracleUpdate this
// package emits. Must match the registry key.
const SourceName = "exchangeratesapi"

// DefaultEndpoint is the exchangeratesapi.io REST base. Includes
// the `/v1` version prefix.
const DefaultEndpoint = "https://api.exchangeratesapi.io/v1"

// LatestPath is the endpoint for current rates. Combined with the
// `?base=...&symbols=...` query we build in poller.go.
const LatestPath = "/latest"

// DefaultPollInterval is the minimum cadence we run at when
// operator doesn't override — matches the Professional tier's
// 1-min refresh. Setting lower would waste quota without gaining
// resolution.
const DefaultPollInterval = 60 * time.Second

// DefaultDecimals is the precision at which we scale the incoming
// float64 rates. Five decimal places is enough for G10 cross-rates
// (typical precision ~4dp); 6 gives headroom for EM currencies. This
// is the scale of the venue's RATE, not the price we emit — see
// InvertedDecimals.
const DefaultDecimals uint8 = 6

// InvertedDecimals is the scale of the EMITTED price, after
// inverting the venue's base-per-symbol rate. Inverting at the same
// scale as the input quantises weak-currency prices by up to ~1.2%:
// a rate in the tens of thousands leaves only 1-2
// significant digits once re-expressed at 6dp. Widening the output
// to 12dp keeps the round-trip accurate regardless of the rate's
// magnitude.
const InvertedDecimals uint8 = 12

// DefaultBase is the base currency we query when operator doesn't
// override. USD is chosen because: (1) it's our primary quote asset,
// (2) triangulating other pairs through USD matches most consumer
// expectations, (3) free tier's EUR-only-base is unusable anyway so
// we may as well bake USD in as the opinionated default.
const DefaultBase = "USD"

// Compile-time assertion: exchange class in registry.
var _ = external.ClassExchange

// Errors surfaced by the poller.
var (
	// ErrAPIKeyRequired — operator enabled the source without
	// providing an API key. Surfaced at NewPoller time so the
	// indexer fails at startup, not at first poll.
	ErrAPIKeyRequired = errors.New("exchangeratesapi: API key required (see config.External.ExchangeRatesApi.APIKey)")

	// ErrAPIRejected — venue returned {"success": false, "error": {...}}.
	// Common causes: invalid key (401), rate limit exhausted (429
	// implicit via the success=false shape), base not available on
	// current tier (free tier rejects base!=EUR).
	ErrAPIRejected = errors.New("exchangeratesapi: API returned success=false")

	// ErrMalformedResponse — JSON didn't decode to the documented
	// shape. Single-poll skip; next tick retries.
	ErrMalformedResponse = errors.New("exchangeratesapi: malformed response")
)
