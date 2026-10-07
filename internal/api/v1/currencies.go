package v1

import (
	"context"
	"math/big"
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

// FXQuotePoint is the storage-layer-projected history datum. Both
// rates are the fx_quotes NUMERIC columns' exact text; they are parsed
// to *big.Rat, never through a float (ADR-0003).
type FXQuotePoint struct {
	Bucket time.Time
	// RateUSDText is rate_usd: units of the ticker per 1 USD.
	RateUSDText string
	// InverseUSDText is inverse_usd: USD per 1 unit of the ticker.
	InverseUSDText string
}

// rateUSDRat is the point's exact USD→fiat rate; ok is false when there
// is no text or it is not > 0.
func (p FXQuotePoint) rateUSDRat() (*big.Rat, bool) { return positiveRat(p.RateUSDText) }

// inverseUSDRat is the point's exact fiat→USD rate; ok is false when
// there is no text or it is not > 0.
func (p FXQuotePoint) inverseUSDRat() (*big.Rat, bool) { return positiveRat(p.InverseUSDText) }

func positiveRat(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, false
	}
	return r, true
}

// CurrenciesSnapshot is the v1-side projection of the forex cache.
// Mirrors forex.Snapshot field-for-field; defined here so the
// binding adapter in cmd/stellarindex-api can convert without this
// package importing the source package.
type CurrenciesSnapshot struct {
	Currencies  []CurrencyEntry
	PublishedAt time.Time
	FetchedAt   time.Time
}

// CurrencyEntry is one in-memory currency row carried by
// CurrenciesSnapshot. Consumed by /v1/price's fiat cross-rate
// triangulation path.
type CurrencyEntry struct {
	Ticker string
	Name   string
	//floatmoney:ok known debt: forex-pipeline float chain (forex cache.go/worker.go RateUSD)
	RateUSD      float64
	Change24hPct *float64
	Change7dPct  *float64
	UpdatedAt    time.Time
	// Source is the feed that published RateUSD; empty reads as the
	// primary feed (see fxSourceOf).
	Source string
}
