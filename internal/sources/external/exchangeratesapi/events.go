// Package exchangeratesapi polls exchangeratesapi.io's REST endpoint
// for fiat reference rates.
//
// It emits canonical.OracleUpdate, not Trade: an FX reference rate is a computed benchmark, not an
// executed trade. The triangulation layer uses it (`XLM/USD × USD/EUR = XLM/EUR`) when no venue
// trades a pair directly.
//
// Professional tier is the minimum (first with a USD base and redistribution rights). The free
// tier (EUR base only) is rejected at startup, because it would force every FX consumer to
// triangulate through EUR.
//
// Wire format (https://exchangeratesapi.io/documentation):
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

// SourceName is stamped on every canonical.OracleUpdate and must match the registry key.
const SourceName = "exchangeratesapi"

// DefaultEndpoint is the REST base, including the /v1 prefix.
const DefaultEndpoint = "https://api.exchangeratesapi.io/v1"

// LatestPath is the current-rates endpoint, combined with the query built in poller.go.
const LatestPath = "/latest"

// DefaultPollInterval matches the Professional tier's 1-minute refresh; polling faster only
// burns quota.
const DefaultPollInterval = 60 * time.Second

// DefaultDecimals is the scale applied to the incoming float64 rates: 6dp covers G10 crosses
// (~4dp typical) with headroom for EM currencies. It scales the venue's RATE, not the emitted
// price; see InvertedDecimals.
const DefaultDecimals uint8 = 6

// InvertedDecimals is the scale of the EMITTED price after inverting the venue's base-per-symbol
// rate. Inverting at the input scale quantises weak-currency prices by up to ~1.2% (a rate in the
// tens of thousands keeps only 1-2 significant digits at 6dp); 12dp keeps the round-trip accurate
// at any magnitude.
const InvertedDecimals uint8 = 12

// DefaultBase is the base currency queried unless the operator overrides it. USD is our primary
// quote asset and the usual triangulation pivot.
const DefaultBase = "USD"

// Compile-time assertion: exchange class in registry.
var _ = external.ClassExchange

// Errors surfaced by the poller.
var (
	// ErrAPIKeyRequired is raised at NewPoller time so a missing key fails at startup, not at
	// first poll.
	ErrAPIKeyRequired = errors.New("exchangeratesapi: API key required (see config.External.ExchangeRatesApi.APIKey)")

	// ErrAPIRejected means the venue returned success=false: invalid key, exhausted rate limit,
	// or a base the tier does not allow (free tier rejects base!=EUR).
	ErrAPIRejected = errors.New("exchangeratesapi: API returned success=false")

	// ErrMalformedResponse — JSON didn't decode to the documented
	// shape. Single-poll skip; next tick retries.
	ErrMalformedResponse = errors.New("exchangeratesapi: malformed response")
)
