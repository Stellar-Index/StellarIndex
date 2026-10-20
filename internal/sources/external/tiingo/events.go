// Package tiingo polls Tiingo's end-of-day endpoint for the published daily net asset value of
// registered mutual funds.
//
//   - It emits canonical.OracleUpdate, never Trade: a fund NAV is an administrator's statement of
//     share value, not an executed trade.
//   - Rows are keyed `raw:<TICKER>` under source `tiingo`. The tickers are on no canonical
//     allow-list, so Pair.Validate refuses the raw leg: a NAV can never become a pair, a VWAP input
//     or a supply key. Only the RWA reference surface reads them, via the curated
//     (code, issuer) → ticker binding in internal/rwa.
//
// Tiingo publishes the SEC-reported NAV (2 dp) for the business day it was struck, landing that
// evening, so the newest bar is usually the previous business day's.
//
// Wire format, one request per ticker per poll:
// GET https://api.tiingo.com/tiingo/daily/WTTSX/prices?startDate=YYYY-MM-DD with
// `Authorization: Token <key>`. A fund with no NAV history returns `[]` with 200: not an error.
package tiingo

import (
	"errors"
	"time"
)

// SourceName is stamped on every canonical.OracleUpdate and must match the registry key.
const SourceName = "tiingo"

// DefaultEndpoint is the Tiingo REST base.
const DefaultEndpoint = "https://api.tiingo.com"

// DefaultPollInterval is hourly: a NAV strikes once per business day, so faster polling only
// spends quota.
const DefaultPollInterval = time.Hour

// Decimals is the emitted price scale (1e6, as the other FX-style pollers); it holds the 2-dp NAV
// exactly.
const Decimals uint8 = 6

// LookbackDays is how far back each poll asks. A week spans a weekend plus a holiday, so a missed
// poll is back-filled by the next one; re-reading a stored bar is idempotent.
const LookbackDays = 7

// Errors surfaced by the poller.
var (
	// ErrAPIKeyRequired is raised at NewPoller so the indexer fails at startup, not first poll.
	ErrAPIKeyRequired = errors.New("tiingo: API key required (set TIINGO_API_KEY)")

	// ErrNoTickers means the curated fund bindings yielded nothing, which is a wiring bug.
	ErrNoTickers = errors.New("tiingo: no tickers to poll")

	// ErrAPIRejected is a non-200 status from the vendor.
	ErrAPIRejected = errors.New("tiingo: API rejected request")

	// ErrMalformedResponse means the body did not decode to the documented shape.
	ErrMalformedResponse = errors.New("tiingo: malformed response")
)
