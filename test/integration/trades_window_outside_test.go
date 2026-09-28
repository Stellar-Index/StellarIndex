//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func candleRows(t *testing.T, source string, pair c.Pair, hourStart time.Time, g time.Duration) []c.Trade {
	t.Helper()
	// Pair-specific: a shared symbol gives two pairs the same PK at one
	// (source, closeTs), so the second seed upserts over the first.
	symbol := pair.Base.Code + pair.Quote.Code
	var out []c.Trade
	for open := hourStart; open.Before(hourStart.Add(time.Hour)); open = open.Add(g) {
		closeTs := open.Add(g - time.Second)
		h, err := scale.CandleTxHash(symbol, closeTs.Unix(), g)
		if err != nil {
			t.Fatalf("CandleTxHash: %v", err)
		}
		out = append(out, c.Trade{
			Source:      source,
			TxHash:      h,
			Timestamp:   closeTs,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(17_000_000)),
		})
	}
	return out
}

// Store.TradesInWindowOutside is what backfill-external's -allow-overlap
// guard refuses on. Over a window holding an hour of stored 1m candles, a
// 1h re-run must see all 60 minute rows as ones it would not overwrite
// (writing it would double the hour's volume), while a 1m re-run sees
// none. Rows for another venue, pair or outside the window never count.
func TestTradesInWindowOutside_CandleGranularityReRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlmUSD := earliestPair(t, "crypto:XLM", "fiat:USD")
	xlmEUR := earliestPair(t, "crypto:XLM", "fiat:EUR")
	hour := time.Date(2025, 4, 18, 18, 0, 0, 0, time.UTC)
	from, to := hour, hour.Add(time.Hour)

	minutes := candleRows(t, "kraken", xlmUSD, hour, time.Minute)
	seed := append([]c.Trade{}, minutes...)
	seed = append(seed, candleRows(t, "bitstamp", xlmUSD, hour, time.Minute)...)
	seed = append(seed, candleRows(t, "kraken", xlmEUR, hour, time.Minute)...)
	seed = append(seed, candleRows(t, "kraken", xlmUSD, to, time.Minute)[0]) // `to` is exclusive
	for i, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d InsertTrade: %v", i, err)
		}
	}

	hourly := candleRows(t, "kraken", xlmUSD, hour, time.Hour)
	n, first, err := store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, hourly)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (1h batch): %v", err)
	}
	if n != 60 || !first.Equal(minutes[0].Timestamp) {
		t.Fatalf("1h batch over stored 1m rows = (%d, %v), want (60, %v)", n, first, minutes[0].Timestamp)
	}

	n, _, err = store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, minutes)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (1m batch): %v", err)
	}
	if n != 0 {
		t.Fatalf("same-granularity re-run sees %d foreign row(s), want 0", n)
	}

	// An empty batch sees every stored row in scope and nothing else.
	n, _, err = store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, nil)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (empty batch): %v", err)
	}
	if n != 60 {
		t.Fatalf("empty batch sees %d row(s), want the 60 in scope", n)
	}
}
