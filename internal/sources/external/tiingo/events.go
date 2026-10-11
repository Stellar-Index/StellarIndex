// Package tiingo polls Tiingo's end-of-day endpoint for the published
// daily net asset value of registered mutual funds.
//
// Role in the aggregator:
//   - Emits canonical.OracleUpdate, never Trade: a fund NAV is an
//     administrator's statement of share value, not an executed trade.
//   - Rows are keyed `raw:<TICKER>` under source `tiingo`. The fund tickers are on no canonical
//     allow-list, so Pair.Validate refuses the raw leg: a NAV can never become a pair, a VWAP
//     input or a supply key. Only the RWA reference surface reads them, via the curated (code,
//     issuer) → ticker binding in internal/rwa.
//     ticker binding in internal/rwa.
//
// Tiingo publishes the SEC-reported NAV (2 dp) for the business day it was struck, landing that
// evening, so the newest bar is usually the previous business day's.
//
// Wire format: GET https://api.tiingo.com/tiingo/daily/WTTSX/prices?startDate=YYYY-MM-DD
// with `Authorization: Token <key>`, returning
// [{"date":"YYYY-MM-DDT00:00:00.000Z","close":9.44,...}]. A fund with no NAV history returns `[]` with 200: no NAV, not an error.
// request per ticker per poll.
package tiingo

import (
	"errors"
	"time"
)

// SourceName is stamped on every canonical.OracleUpdate this package
// emits. Must match the registry key.
const SourceName = "tiingo"

// DefaultEndpoint is the Tiingo REST base.
const DefaultEndpoint = "https://api.tiingo.com"

// DefaultPollInterval is hourly: a NAV strikes once per business day, so
// polling faster spends quota without adding a row.
const DefaultPollInterval = time.Hour

// Decimals is the scale of the emitted price, matching the other
// off-chain FX-style pollers' 1e6. It holds Tiingo's 2-dp NAV exactly.
const Decimals uint8 = 6

// LookbackDays is how far back each poll asks. A week spans a weekend
// plus a holiday, so one missed day of polling is back-filled on the next
// successful poll; re-reading a bar already stored is idempotent.
const LookbackDays = 7

// Errors surfaced by the poller.
var (
	// ErrAPIKeyRequired — the source is enabled without a key. Surfaced
	// at NewPoller so the indexer fails at startup, not at first poll.
	ErrAPIKeyRequired = errors.New("tiingo: API key required (set TIINGO_API_KEY)")

	// ErrNoTickers — nothing to poll. The ticker list comes from the
	// curated fund bindings, so an empty list is a wiring bug.
	ErrNoTickers = errors.New("tiingo: no tickers to poll")

	// ErrAPIRejected — a non-200 status from the vendor.
	ErrAPIRejected = errors.New("tiingo: API rejected request")

	// ErrMalformedResponse — the body did not decode to the documented
	// shape.
	ErrMalformedResponse = errors.New("tiingo: malformed response")
)
