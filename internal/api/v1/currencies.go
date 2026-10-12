package v1

import (
	"context"
	"math/big"
	"time"
)

// CurrenciesReader exposes the latest in-memory forex snapshot for /v1/price fiat cross-rate triangulation.
type CurrenciesReader interface {
	// Latest returns the most recent forex snapshot, or nil if no
	// fetch has completed yet (warming up).
	Latest() *CurrenciesSnapshot
}

// FXHistoryReader serves persisted fx_quotes history for /v1/chart fiat:fiat pairs and price_type=market_cap.
type FXHistoryReader interface {
	ListFXHistory(ctx context.Context, ticker string, from, to time.Time) ([]FXQuotePoint, error)
}

// FXQuotePoint is a history datum whose rates are exact NUMERIC text parsed to *big.Rat, never a float (ADR-0003).
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

// CurrenciesSnapshot is the v1-side projection of forex.Snapshot, defined here so v1 does not import the source package.
type CurrenciesSnapshot struct {
	Currencies  []CurrencyEntry
	PublishedAt time.Time
	FetchedAt   time.Time
}

// CurrencyEntry is one in-memory currency row of CurrenciesSnapshot, used by /v1/price's fiat cross-rate triangulation.
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
