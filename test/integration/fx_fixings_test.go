//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFXFixings covers the fx_fixings table (migration 0192): the append
// path and the vendor-time binding a closed fiat cross converts at. One
// container; each subtest owns its tickers.
func TestFXFixings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Monday 00:00 UTC.
	t0 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	const maxAge = 76 * time.Hour
	hour := func(ticker string, start time.Time, rate string) timescale.FXFixing {
		return timescale.FXFixing{Ticker: ticker, Grain: timescale.FXGrainHour, BarStart: start, BarEnd: start.Add(time.Hour), RateUSD: rate, Source: "massive"}
	}
	day := func(ticker string, start time.Time, rate string) timescale.FXFixing {
		return timescale.FXFixing{Ticker: ticker, Grain: timescale.FXGrainDay, BarStart: start, BarEnd: start.Add(24 * time.Hour), RateUSD: rate, Source: "massive"}
	}
	insert := func(t *testing.T, rows ...timescale.FXFixing) int64 {
		t.Helper()
		n, err := store.InsertFXFixingBatch(ctx, rows)
		if err != nil {
			t.Fatalf("InsertFXFixingBatch: %v", err)
		}
		return n
	}
	bind := func(t *testing.T, ticker string, e time.Time, age time.Duration) (timescale.FXFixingBinding, bool) {
		t.Helper()
		got, err := store.FXFixingAtOrBefore(ctx, []string{ticker}, e, age)
		if err != nil {
			t.Fatalf("FXFixingAtOrBefore: %v", err)
		}
		b, ok := got[ticker]
		return b, ok
	}
	count := func(t *testing.T, ticker string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM fx_fixings WHERE ticker = $1`, ticker).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("RateExactText", func(t *testing.T) {
		insert(t, hour("EUR", t0, "0.7412345678901234567"))
		b, ok := bind(t, "EUR", t0.Add(4*time.Hour), maxAge)
		if !ok || b.RateUSD != "0.7412345678901234567" {
			t.Fatalf("binding = %+v ok=%v, want the exact stored text", b, ok)
		}
		if b.Resolution != timescale.FXResolutionHourly || !b.BarEnd.Equal(t0.Add(time.Hour)) {
			t.Errorf("resolution %q bar_end %s", b.Resolution, b.BarEnd)
		}
	})

	t.Run("InsertIdempotent", func(t *testing.T) {
		rows := []timescale.FXFixing{hour("GBP", t0, "0.79"), hour("GBP", t0.Add(time.Hour), "0.791")}
		if n := insert(t, rows...); n != 2 {
			t.Fatalf("first insert = %d, want 2", n)
		}
		if n := insert(t, rows...); n != 0 {
			t.Fatalf("re-insert = %d, want 0", n)
		}
		if n := count(t, "GBP"); n != 2 {
			t.Fatalf("rows = %d, want 2", n)
		}
	})

	t.Run("GrainKeyed", func(t *testing.T) {
		if n := insert(t, hour("JPY", t0, "150.1"), day("JPY", t0, "150.2")); n != 2 {
			t.Fatalf("hourly + daily at one bar_start = %d rows, want 2", n)
		}
	})

	t.Run("CheckRejectsZero", func(t *testing.T) {
		if _, err := store.InsertFXFixingBatch(ctx, []timescale.FXFixing{hour("CHF", t0, "0")}); err == nil {
			t.Fatal("a zero rate was stored")
		}
	})

	t.Run("GenerationWins", func(t *testing.T) {
		insert(t, hour("CAD", t0, "1.35"))
		store.SetDeriveGeneration(7)
		insert(t, hour("CAD", t0, "1.36"))
		store.SetDeriveGeneration(0)
		// A later generation-0 re-write of the same bar changes nothing.
		insert(t, hour("CAD", t0, "1.37"))
		b, ok := bind(t, "CAD", t0.Add(4*time.Hour), maxAge)
		if !ok || b.RateUSD != "1.36" || b.Generation != 7 {
			t.Fatalf("binding = %+v, want generation 7 at 1.36", b)
		}
	})

	t.Run("SourceEntryCountsUntouched", func(t *testing.T) {
		var before, after int64
		q := `SELECT COALESCE(SUM(entry_count), 0) FROM source_entry_counts`
		if err := db.QueryRowContext(ctx, q).Scan(&before); err != nil {
			t.Fatal(err)
		}
		insert(t, hour("AUD", t0, "1.5"), hour("AUD", t0.Add(time.Hour), "1.51"))
		if err := db.QueryRowContext(ctx, q).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatalf("source_entry_counts moved %d → %d", before, after)
		}
	})

	t.Run("LagBoundary", func(t *testing.T) {
		for i := range 6 {
			insert(t, hour("SEK", t0.Add(time.Duration(i)*time.Hour), "10."+string(rune('0'+i))))
		}
		// e − 3h = t0+2h: the bar ending exactly then binds.
		if b, _ := bind(t, "SEK", t0.Add(5*time.Hour), maxAge); !b.BarStart.Equal(t0.Add(time.Hour)) {
			t.Errorf("e=t0+5h bound bar %s, want t0+1h", b.BarStart)
		}
		if b, _ := bind(t, "SEK", t0.Add(5*time.Hour-time.Nanosecond), maxAge); !b.BarStart.Equal(t0) {
			t.Errorf("e=t0+5h−1ns bound bar %s, want t0", b.BarStart)
		}
	})

	t.Run("WeekendCarry", func(t *testing.T) {
		fri := t0.Add(4*24*time.Hour + 21*time.Hour) // Friday 21:00
		insert(t, hour("NOK", fri, "10.5"))
		mon := fri.Add(3*24*time.Hour + 4*time.Hour) // Monday 01:00
		if b, ok := bind(t, "NOK", mon, maxAge); !ok || !b.BarStart.Equal(fri) {
			t.Fatalf("Monday 01:00 bound %+v ok=%v, want Friday's last bar", b, ok)
		}
		if _, ok := bind(t, "NOK", mon, 24*time.Hour); ok {
			t.Fatal("a 24h max age still bound the Friday bar")
		}
	})

	t.Run("GrainTiebreak", func(t *testing.T) {
		insert(t, day("DKK", t0, "6.9"), hour("DKK", t0.Add(23*time.Hour), "6.8"))
		b, ok := bind(t, "DKK", t0.Add(27*time.Hour), maxAge)
		if !ok || b.Grain != timescale.FXGrainHour || b.RateUSD != "6.8" {
			t.Fatalf("binding = %+v, want the hourly bar on a shared bar_end", b)
		}
	})

	t.Run("DailyEra", func(t *testing.T) {
		insert(t, day("PLN", t0, "4.0"), day("PLN", t0.Add(24*time.Hour), "4.1"))
		b, ok := bind(t, "PLN", t0.Add(2*24*time.Hour+5*time.Hour), maxAge)
		if !ok || b.RateUSD != "4.1" || b.Resolution != timescale.FXResolutionDaily {
			t.Fatalf("binding = %+v, want the daily bar with daily resolution", b)
		}
	})

	t.Run("PreFixingsDaily", func(t *testing.T) {
		for _, tk := range []string{"CZK", "HUF"} {
			if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
				{Bucket: t0, Ticker: tk, RateUSD: 23, InverseUSD: 1.0 / 23, Source: "massive"},
				{Bucket: t0.Add(24 * time.Hour), Ticker: tk, RateUSD: 24, InverseUSD: 1.0 / 24, Source: "massive"},
			}); err != nil {
				t.Fatalf("InsertFXQuoteBatch: %v", err)
			}
		}
		// CZK's fixings start later; HUF has none at all.
		insert(t, hour("CZK", t0.Add(10*24*time.Hour), "25"))
		e := t0.Add(2*24*time.Hour + 12*time.Hour)
		for _, tk := range []string{"CZK", "HUF"} {
			b, ok := bind(t, tk, e, maxAge)
			if !ok || b.Resolution != timescale.FXResolutionDaily || b.RateUSD != "24" ||
				!b.BarStart.Equal(t0.Add(24*time.Hour)) || !b.BarEnd.Equal(t0.Add(48*time.Hour)) {
				t.Errorf("%s: binding = %+v ok=%v, want the fx_quotes day closed by e − lag", tk, b, ok)
			}
		}
		// e − lag − 24h is before t0+1d: the earlier day binds.
		if b, _ := bind(t, "HUF", t0.Add(2*24*time.Hour+2*time.Hour), maxAge); b.RateUSD != "23" {
			t.Errorf("HUF near the day boundary bound %q, want the previous day's 23", b.RateUSD)
		}
	})

	t.Run("EraGapWithholds", func(t *testing.T) {
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
			{Bucket: t0.Add(9 * 24 * time.Hour), Ticker: "TRY", RateUSD: 32, InverseUSD: 1.0 / 32, Source: "massive"},
		}); err != nil {
			t.Fatalf("InsertFXQuoteBatch: %v", err)
		}
		insert(t, hour("TRY", t0, "31"))
		if b, ok := bind(t, "TRY", t0.Add(10*24*time.Hour), maxAge); ok {
			t.Fatalf("a gap inside the fixings era bound %+v; it must withhold", b)
		}
	})

	t.Run("LookbackMiss", func(t *testing.T) {
		if b, ok := bind(t, "ZAR", t0, maxAge); ok {
			t.Fatalf("a ticker with no data bound %+v", b)
		}
	})

	t.Run("FXQuoteAtOrBeforeFixingsArm", func(t *testing.T) {
		usd, _ := c.NewFiatAsset("USD")
		mxn, _ := c.NewFiatAsset("MXN")
		pair, _ := c.NewPair(usd, mxn)
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
			{Bucket: t0, Ticker: "MXN", RateUSD: 17, InverseUSD: 1.0 / 17, Source: "massive"},
		}); err != nil {
			t.Fatalf("InsertFXQuoteBatch: %v", err)
		}
		// Before the first fixing the fx_quotes day still answers.
		price, observed, _, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(2*time.Hour), external.FXSources())
		if err != nil || price.RatString() != "17" || !observed.Equal(t0) {
			t.Fatalf("pre-fixings = %v at %s (err %v), want the fx_quotes 17 at t0", price, observed, err)
		}
		insert(t, hour("MXN", t0.Add(time.Hour), "17.25"))
		price, observed, src, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(5*time.Hour), external.FXSources())
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if price.FloatString(2) != "17.25" || !observed.Equal(t0.Add(2*time.Hour)) || src != "massive" {
			t.Fatalf("fixings arm = %s at %s from %q, want 17.25 at the bar end", price.FloatString(4), observed, src)
		}
	})

	t.Run("LoadFXFixingWindow", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Hour)
		insert(t, hour("ILS", now.Add(-2*time.Hour), "3.7"), hour("ILS", now.Add(-72*time.Hour), "3.6"))
		rows, loadedAt, err := store.LoadFXFixingWindow(ctx, 24*time.Hour, time.Time{})
		if err != nil {
			t.Fatalf("LoadFXFixingWindow: %v", err)
		}
		if d := time.Since(loadedAt); d < 0 || d > time.Minute {
			t.Errorf("loadedAt %s is not the load's statement time", loadedAt)
		}
		var ils []string
		for _, r := range rows {
			if r.Ticker == "ILS" {
				ils = append(ils, r.RateUSD)
			}
		}
		if len(ils) != 1 || ils[0] != "3.7" {
			t.Fatalf("ILS rows in a 24h window = %v, want only the recent bar", ils)
		}
		delta, _, err := store.LoadFXFixingWindow(ctx, 24*time.Hour, loadedAt)
		if err != nil {
			t.Fatalf("delta load: %v", err)
		}
		if len(delta) != 0 {
			t.Fatalf("delta after no writes = %d rows", len(delta))
		}
		insert(t, hour("ILS", now.Add(-time.Hour), "3.71"))
		delta, _, err = store.LoadFXFixingWindow(ctx, 24*time.Hour, loadedAt)
		if err != nil || len(delta) != 1 || delta[0].RateUSD != "3.71" {
			t.Fatalf("delta = %+v (err %v), want the one new bar", delta, err)
		}
	})
}
