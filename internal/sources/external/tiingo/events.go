// Package tiingo polls Tiingo's end-of-day endpoint for the published
// daily net asset value of registered mutual funds.
//
// Role in the aggregator:
//
//   - Emits canonical.OracleUpdate, never Trade: a fund NAV is the fund
//     administrator's statement of what one share is worth, not an
//     executed trade.
//   - Rows are keyed `raw:<TICKER>` under source `tiingo`. The fund
//     tickers are on none of the canonical allow-lists, and the raw
//     namespace is what keeps them record-layer only: Pair.Validate
//     refuses a raw leg, so a NAV can never become a pair, a VWAP input
//     or a supply key. The one reader that interprets them is the RWA
//     reference surface, through the curated (code, issuer) → ticker
//     binding in internal/rwa.
//   - Registry class is oracle with IncludeInVWAP false.
//
// Tiingo publishes the SEC-reported NAV, rounded to 2 dp, dated the
// business day it was struck; it lands the evening of that day, so the
// newest bar is usually the previous business day's.
//
// Wire format (captured from the live API):
//
//	GET https://api.tiingo.com/tiingo/daily/WTTSX/prices?startDate=2026-09-22
//	Authorization: Token <key>
//
//	[{"date":"2026-09-29T00:00:00.000Z","close":9.44,"high":9.44,"low":9.44,
//	  "open":9.44,"volume":0,"adjClose":9.44,...}]
//
// A fund with no NAV history returns `[]` with 200 — no NAV, not an error.
//
// Free-tier limits are 50 requests/hour and 1,000/day. The default hourly
// cadence spends one request per ticker per poll: 12 tickers × 24 = 288
// requests/day, 8,928/month.
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
