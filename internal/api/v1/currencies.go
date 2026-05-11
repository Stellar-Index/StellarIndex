package v1

import (
	"context"
	"time"
)

// CurrenciesReader exposes the latest in-memory forex snapshot.
// Used by /v1/price for fiat-cross-rate triangulation. The forex
// package's *Cache implements it via Latest; the interface keeps
// the dependency direction (forex doesn't import v1).
//
// The legacy /v1/currencies HTTP surface has been removed (no
// production consumers); this reader stays alive as the in-process
// FX-rate seam.
type CurrenciesReader interface {
	// Latest returns the most recent forex snapshot, or nil if no
	// fetch has completed yet (warming up).
	Latest() *CurrenciesSnapshot
}

// FXHistoryReader serves long-form persisted history from the
// fx_quotes hypertable. Used by /v1/chart for fiat:fiat pairs and
// by /v1/chart?price_type=market_cap.
type FXHistoryReader interface {
	ListFXHistory(ctx context.Context, ticker string, from, to time.Time) ([]FXQuotePoint, error)
}

// FXQuotePoint is the storage-layer-projected history datum.
// Mirrors timescale.FXQuote field-for-field with the date axis as
// `Bucket`.
type FXQuotePoint struct {
	Bucket     time.Time
	RateUSD    float64
	InverseUSD float64
}

// CurrenciesSnapshot is the v1-side projection of the forex cache.
// Mirrors forex.Snapshot field-for-field; defined here so the
// binding adapter in cmd/ratesengine-api can convert without this
// package importing the source package.
type CurrenciesSnapshot struct {
	Currencies  []CurrencyEntry
	PublishedAt time.Time
	FetchedAt   time.Time
	History7d   map[string][]CurrencyHistoryRaw
}

// CurrencyHistoryRaw is the per-ticker daily series the adapter
// passes through. Date is UTC; RateUSD is "1 USD = N units of
// ticker".
type CurrencyHistoryRaw struct {
	Date    time.Time
	RateUSD float64
}

// CurrencyEntry is one in-memory currency row carried by
// CurrenciesSnapshot. Consumed by /v1/price's fiat cross-rate
// triangulation path.
type CurrencyEntry struct {
	Ticker            string
	Name              string
	RateUSD           float64
	Change24hPct      *float64
	Change7dPct       *float64
	History7dRates    []float64
	UpdatedAt         time.Time
	CirculatingSupply *float64
	MarketCapUSD      *float64
	CirculationAsOf   string
	CirculationSource string
}
