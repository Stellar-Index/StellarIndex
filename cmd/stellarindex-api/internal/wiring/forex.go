package wiring

import (
	"context"
	"fmt"
	"strconv"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/forex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ForexAdapter bridges the forex.Cache (raw snapshot type) to
// v1.CurrenciesReader (wire-shape projection). The v1 package
// can't import internal/sources/external/forex without inverting
// the dependency direction; the adapter lives here so main.go owns
// the conversion.
type ForexAdapter struct{ cache *forex.Cache }

func NewForexAdapter(c *forex.Cache) *ForexAdapter { return &ForexAdapter{cache: c} }

func (a *ForexAdapter) Latest() *v1.CurrenciesSnapshot {
	snap := a.cache.Latest()
	if snap == nil {
		return nil
	}
	rows := make([]v1.CurrencyEntry, len(snap.Currencies))
	for i, c := range snap.Currencies {
		rows[i] = v1.CurrencyEntry{
			Ticker:    c.Ticker,
			Name:      c.Name,
			RateUSD:   c.RateUSD,
			UpdatedAt: c.UpdateAt,
			Source:    c.Source,
		}
	}
	return &v1.CurrenciesSnapshot{
		Currencies:  rows,
		PublishedAt: snap.PublishedAt,
		FetchedAt:   snap.FetchedAt,
	}
}

// ForexQuoteWriter adapts (*timescale.Store) to forex.FXQuoteWriter and
// forex.FXQuoteReader (the worker can't import timescale without inverting
// the dependency direction). Translates the per-package FXQuote shape.
type ForexQuoteWriter struct{ Store *timescale.Store }

func (w *ForexQuoteWriter) InsertFXQuoteBatch(ctx context.Context, quotes []forex.FXQuote) error {
	if len(quotes) == 0 {
		return nil
	}
	out := make([]timescale.FXQuote, len(quotes))
	for i, q := range quotes {
		out[i] = timescale.FXQuote{
			Bucket:  q.Bucket,
			Ticker:  q.Ticker,
			RateUSD: q.RateUSD,
			Source:  q.Source,
		}
	}
	return w.Store.InsertFXQuoteBatch(ctx, out)
}

// InsertFXFixingBatch adapts the store's fx_fixings append to
// forex.FXFixingWriter; the close stays the vendor's decimal text.
func (w *ForexQuoteWriter) InsertFXFixingBatch(ctx context.Context, bars []forex.FXBar) error {
	out := make([]timescale.FXFixing, len(bars))
	for i, b := range bars {
		out[i] = timescale.FXFixing{
			Ticker: b.Ticker, Grain: b.Grain, BarStart: b.BarStart, BarEnd: b.BarEnd,
			RateUSD: b.CloseText, Source: b.Source,
		}
	}
	_, err := w.Store.InsertFXFixingBatch(ctx, out)
	return err
}

// LatestFXQuotes adapts the store's NUMERIC-text read to forex.FXQuoteReader;
// the float parse is at the forex cache boundary, which is float end to end.
func (w *ForexQuoteWriter) LatestFXQuotes(ctx context.Context, since time.Time) ([]forex.FXQuote, error) {
	rows, err := w.Store.LatestFXQuotes(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]forex.FXQuote, 0, len(rows))
	for _, q := range rows {
		rate, err := strconv.ParseFloat(q.RateUSDText, 64)
		if err != nil {
			return nil, fmt.Errorf("fx_quotes %s rate_usd %q: %w", q.Ticker, q.RateUSDText, err)
		}
		out = append(out, forex.FXQuote{Bucket: q.Bucket, Ticker: q.Ticker, RateUSD: rate, Source: q.Source})
	}
	return out, nil
}
