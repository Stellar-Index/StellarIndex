//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFXFixings covers the fx_fixings table (migration 0193): the append
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

// TestFXQuoteAtOrBefore proves the X2.5 forex-snap storage primitive
// returns the most recent FX-source observation at-or-before a cutoff.
// Drives the across-region determinism story: every region serving the
// same closed bucket queries the same hypertable and gets the same row.
func TestFXQuoteAtOrBefore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(usd, eur)

	// Anchor in the past — deterministic windows regardless of clock.
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	// Both legs of an FX quote share one scale, so quote/base is the
	// price ratio with no scale adjustment.
	// 0.92 EUR per USD:    base=1e8,    quote=92_000_000
	// 0.93 EUR per USD:    base=1e8,    quote=93_000_000
	// 0.94 EUR per USD:    base=1e8,    quote=94_000_000
	trades := []c.Trade{
		mkIntegrationTrade("exchangeratesapi", 1, t0, pair, 100_000_000, 92_000_000),
		mkIntegrationTrade("exchangeratesapi", 2, t0.Add(15*time.Minute), pair, 100_000_000, 93_000_000),
		mkIntegrationTrade("exchangeratesapi", 3, t0.Add(30*time.Minute), pair, 100_000_000, 94_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	fx := external.FXSources()
	if len(fx) < 2 {
		t.Fatalf("FXSources() returned %d, want at least 2", len(fx))
	}

	t.Run("hits second observation when cutoff between t1 and t2", func(t *testing.T) {
		cutoff := t0.Add(20 * time.Minute)
		price, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("got source %q, want exchangeratesapi", src)
		}
		if !obs.Equal(t0.Add(15 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v", obs, t0.Add(15*time.Minute))
		}
		want := new(big.Rat).SetFrac(big.NewInt(93_000_000), big.NewInt(100_000_000))
		if price.Cmp(want) != 0 {
			t.Errorf("got price %s, want %s", price.RatString(), want.RatString())
		}
	})

	t.Run("hits third observation at exact bucket-end timestamp", func(t *testing.T) {
		// Cutoff equals the 3rd trade's ts — `<=` semantics include it.
		cutoff := t0.Add(30 * time.Minute)
		_, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("got source %q, want exchangeratesapi", src)
		}
		if !obs.Equal(t0.Add(30 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v", obs, t0.Add(30*time.Minute))
		}
	})

	t.Run("hits first observation at exact first timestamp", func(t *testing.T) {
		_, _, src, err := store.FXQuoteAtOrBefore(ctx, pair, t0, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("got source %q, want exchangeratesapi", src)
		}
	})

	t.Run("returns ErrNoFXQuote when cutoff before first observation", func(t *testing.T) {
		_, _, _, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(-1*time.Minute), fx)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("got err %v, want ErrNoFXQuote", err)
		}
	})

	t.Run("returns ErrNoFXQuote when fxSources is empty", func(t *testing.T) {
		_, _, _, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(1*time.Hour), nil)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("got err %v, want ErrNoFXQuote", err)
		}
	})

	t.Run("source filter excludes non-FX trades", func(t *testing.T) {
		// Insert a same-pair trade from a non-FX source in the same
		// window. Querying with the FX-only filter must NOT return it.
		nonFX := mkIntegrationTrade("binance", 99, t0.Add(45*time.Minute), pair, 100_000_000, 200_000_000)
		if err := store.InsertTrade(ctx, nonFX); err != nil {
			t.Fatalf("InsertTrade non-FX: %v", err)
		}
		_, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(1*time.Hour), fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src == "binance" {
			t.Errorf("filter leaked non-FX source %q (observed_at=%v)", src, obs)
		}
		// Latest FX should still be the 30-min exchangeratesapi row.
		if !obs.Equal(t0.Add(30 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v (latest FX row)", obs, t0.Add(30*time.Minute))
		}
	})

	// NOTE: every subtest above exercises the LEGACY trades fallback —
	// fx_quotes is empty in this test's database, so the fx_quotes-first
	// read (BACKLOG #42) misses and the connector-path trades rows win.
	// That IS the compatibility contract: a re-enabled exchangeratesapi
	// connector keeps working when the massive feed has no rows. The
	// fx_quotes-first behaviour is proven in
	// TestFXQuoteAtOrBeforeFXQuotesFirst below.

	t.Run("FXSources is deterministic and lex-ordered", func(t *testing.T) {
		got := external.FXSources()
		// massive (the forex worker's fx_quotes feed) was bridged into the
		// registry as a SubclassFX source (P0-7). The exact list is the
		// contract: the registry carries one identity per FX upstream; a
		// second entry for the same upstream with IncludeInVWAP:true would
		// double-count its rates. ecb is the forex worker's standby, a
		// separate upstream with IncludeInVWAP:false: its fx_quotes rows
		// must pass the FX-snap class check but never enter VWAP.
		want := []string{"ecb", "exchangeratesapi", "massive"}
		if len(got) != len(want) {
			t.Fatalf("FXSources len=%d, want %d (%v)", len(got), len(want), got)
		}
		for i, s := range want {
			if got[i] != s {
				t.Errorf("FXSources[%d]=%q, want %q", i, got[i], s)
			}
		}
	})
}

// TestFXQuoteAtOrBeforeFXQuotesFirst proves the unified FX read path
// (BACKLOG #42): when the active feed (`massive` → fx_quotes) has a row
// in the lookback, it wins over connector-path trades rows; when its
// newest row is older than the lookback, the snap falls back to the
// legacy trades path; and the fx_quotes rate converts to an EXACT
// *big.Rat (NUMERIC text → Rat, never a float — ADR-0003).
func TestFXQuoteAtOrBeforeFXQuotesFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(usd, eur) // price = EUR per USD

	fx := external.FXSources()
	cutoff := time.Now().UTC().Truncate(time.Hour)
	day := cutoff.Truncate(24 * time.Hour)

	// Legacy connector-path row: 0.94 EUR per USD at cutoff-2h.
	// FX connector trades use the uniform 1e8 scale on each side.
	legacy := mkIntegrationTrade("exchangeratesapi", 11, cutoff.Add(-2*time.Hour), pair, 100_000_000, 94_000_000)
	if err := store.InsertTrade(ctx, legacy); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	t.Run("fx_quotes row wins over the trades row, exact scale", func(t *testing.T) {
		// Active-feed row: rate_usd(EUR) = 0.92, i.e. EUR PER USD — the raw
		// close of Massive's C:USDEUR ticker (units-of-quote per unit-of-base;
		// standard forex convention), which is exactly what forex/client.go
		// stores ("usd→eur ≈ 0.92 means 1 USD buys 0.92 EUR"). NOT 1.085
		// (that is USD-per-EUR = C:EURUSD, the inverse — an earlier fixture used
		// it and, paired with the then-inverted fxSnapFromRows division, the two
		// errors cancelled to a plausible result and masked errors). float64
		// 0.92 round-trips through the pgx driver as the shortest decimal "0.92",
		// stored exactly in NUMERIC.
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
			Bucket: day, Ticker: "EUR", RateUSD: 0.92, InverseUSD: 1.0 / 0.92, Source: "massive",
		}}); err != nil {
			t.Fatalf("InsertFXQuoteBatch: %v", err)
		}

		price, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "massive" {
			t.Errorf("source = %q, want massive (fx_quotes must win over the exchangeratesapi trades row)", src)
		}
		if !obs.Equal(day) {
			t.Errorf("observedAt = %v, want the fx_quotes bucket %v", obs, day)
		}
		// USD/EUR = EUR per USD = quote-per-base = rate_usd(EUR)/rate_usd(USD)
		// = 0.92/1 = 0.92 = 23/25, the SAME quote-per-base orientation the
		// trades fallback returns (subtest below: quote_amount/base_amount).
		// (M3: the old fxSnapFromRows returned base/quote — inverted — so a real
		// 0.92 feed served 1/0.92 = 1.085, the wrong-way-up fiat price.)
		want := new(big.Rat).SetFrac(big.NewInt(23), big.NewInt(25))
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want exactly %s", price.RatString(), want.RatString())
		}
	})

	t.Run("stale fx_quotes row falls back to the trades path", func(t *testing.T) {
		// A cutoff 10 days in the past puts the (today-bucketed)
		// fx_quotes row in the future relative to the query, and the
		// row planted below IS at-or-before the cutoff but 8 days old —
		// outside the 7-day snap lookback. Neither may serve: the
		// legacy trades read must. Anchor a dedicated trades row inside
		// the window.
		staleCutoff := cutoff.Add(-10 * 24 * time.Hour)
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
			Bucket: staleCutoff.Truncate(24 * time.Hour).Add(-8 * 24 * time.Hour),
			Ticker: "EUR", RateUSD: 2.0, InverseUSD: 0.5, Source: "massive",
		}}); err != nil {
			t.Fatalf("InsertFXQuoteBatch (stale row): %v", err)
		}
		old := mkIntegrationTrade("exchangeratesapi", 12, staleCutoff.Add(-30*time.Minute), pair, 100_000_000, 91_000_000)
		if err := store.InsertTrade(ctx, old); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}

		price, _, src, err := store.FXQuoteAtOrBefore(ctx, pair, staleCutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("source = %q, want exchangeratesapi (trades fallback)", src)
		}
		want := new(big.Rat).SetFrac(big.NewInt(91_000_000), big.NewInt(100_000_000))
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
		}
	})

	t.Run("no row anywhere returns ErrNoFXQuote", func(t *testing.T) {
		mxn, err := c.NewFiatAsset("MXN")
		if err != nil {
			t.Fatal(err)
		}
		mxnPair, _ := c.NewPair(usd, mxn)
		_, _, _, err = store.FXQuoteAtOrBefore(ctx, mxnPair, cutoff, fx)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("err = %v, want ErrNoFXQuote", err)
		}
	})
}

// TestFXQuotesGenerationGuard_OperatorCorrectionIsDurable is the
// proven-red test for migration 0141. fx_quotes.rate_usd is
// the denominator of every fiat-quoted usd_volume, so its upsert needs the
// same generation guard as every other derived money-value writer: an
// unguarded `ON CONFLICT (ticker,bucket) DO UPDATE` is pure arrival-order
// last-writer-wins, so an operator correction written by fx-history-backfill
// (source='frankfurter-historical', gen>0) over a key the live worker owns
// (source='massive', gen 0) would be silently reverted by the next daily worker
// refresh.
//
// The fix threads derive_generation through InsertFXQuoteBatch and guards
// the upsert with `fx_quotes.derive_generation <= EXCLUDED.derive_generation`.
// This test exercises the real seam ([Store.SetDeriveGeneration] +
// InsertFXQuoteBatch, exactly what the worker and the tool call) and
// asserts:
//
//   - the operator correction (gen T>0) LANDS over the live gen-0 row (the
//     conflict guard permits 0 <= T), AND
//   - a subsequent live gen-0 worker refresh carrying the stale rate can
//     NEVER revert it (T <= 0 is false) — the correction is durable.
//
// To reproduce the red state: revert only the InsertFXQuoteBatch upsert to
// the unguarded `DO UPDATE SET ... source = EXCLUDED.source` (keep migration
// 0141) and the "correction survives" assertion goes red — the gen-0 replay
// overwrites the corrected rate.
func TestFXQuotesGenerationGuard_OperatorCorrectionIsDurable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const ticker = "EUR"
	bucket := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	const (
		wrongRate     = 0.85 // the transiently-wrong live value
		correctedRate = 0.92 // the operator correction
	)

	read := func() (rate float64, gen int64, source string) {
		const q = `SELECT rate_usd::float8, derive_generation, COALESCE(source, '')
		             FROM fx_quotes WHERE ticker = $1 AND bucket = $2`
		var src sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, ticker, bucket).Scan(&rate, &gen, &src); err != nil {
			t.Fatalf("read fx_quotes (%s, %s): %v", ticker, bucket.Format("2006-01-02"), err)
		}
		return rate, gen, src.String
	}
	closeTo := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	// (1) Live forex worker writes the row at generation 0 (the wrong rate).
	store.SetDeriveGeneration(0)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: wrongRate, InverseUSD: 1.0 / wrongRate, Source: "massive",
	}}); err != nil {
		t.Fatalf("worker gen-0 write: %v", err)
	}

	// (2) Operator correction via fx-history-backfill: a POSITIVE generation
	// (what openBackfillStore stamps) carrying the corrected rate. It must
	// win the conflict (0 <= T).
	gen := time.Now().Unix()
	store.SetDeriveGeneration(gen)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: correctedRate, InverseUSD: 1.0 / correctedRate,
		Source: "frankfurter-historical",
	}}); err != nil {
		t.Fatalf("operator correction write: %v", err)
	}
	if rate, g, src := read(); !closeTo(rate, correctedRate) || g != gen || src != "frankfurter-historical" {
		t.Fatalf("after operator correction: rate=%v gen=%d source=%q, want %v/%d/frankfurter-historical "+
			"(the correction must land over the live gen-0 row)", rate, g, src, correctedRate, gen)
	}

	// (3) The next daily live worker refresh re-writes the row at generation
	// 0 with the stale rate. The guard must PRESERVE the operator correction
	// — this is the durability property regression guard (2) is about.
	store.SetDeriveGeneration(0)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: wrongRate, InverseUSD: 1.0 / wrongRate, Source: "massive",
	}}); err != nil {
		t.Fatalf("worker gen-0 refresh: %v", err)
	}
	if rate, g, src := read(); !closeTo(rate, correctedRate) || g != gen || src != "frankfurter-historical" {
		t.Errorf("after gen-0 worker refresh: rate=%v gen=%d source=%q, want %v/%d/frankfurter-historical "+
			"(the generation guard must make the operator correction durable — a live gen-0 replay "+
			"must NOT revert it)", rate, g, src, correctedRate, gen)
	}
}

// TestFXQuotes_InverseUSDIsTheExactNumericReciprocal pins ADR-0003 on
// the fx_quotes reciprocal: inverse_usd must equal 1 / rate_usd computed
// in NUMERIC, not the worker's float64 quotient. A rate of 3 makes the
// two visibly diverge — the float64 reciprocal is 0.3333333333333333
// (16 digits, then rounded), the NUMERIC one carries the division's own
// precision — so any float-derived value fails the equality.
func TestFXQuotes_InverseUSDIsTheExactNumericReciprocal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Now().UTC().Truncate(24 * time.Hour)
	quotes := []timescale.FXQuote{
		{Bucket: day, Ticker: "XTS", RateUSD: 3, InverseUSD: 1.0 / 3, Source: "massive"},
		{Bucket: day, Ticker: "EUR", RateUSD: 0.92, InverseUSD: 1.0 / 0.92, Source: "massive"},
	}
	if err := store.InsertFXQuoteBatch(ctx, quotes); err != nil {
		t.Fatalf("InsertFXQuoteBatch: %v", err)
	}

	for _, q := range quotes {
		var (
			exact   bool
			inverse string
			rate    string
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT inverse_usd = 1::numeric / rate_usd, inverse_usd::text, rate_usd::text
			   FROM fx_quotes WHERE ticker = $1 AND bucket = $2`, q.Ticker, day,
		).Scan(&exact, &inverse, &rate); err != nil {
			t.Fatalf("read %s: %v", q.Ticker, err)
		}
		if !exact {
			t.Errorf("%s: stored inverse_usd = %s is not the NUMERIC reciprocal of rate_usd = %s "+
				"(a float64 1/rate reached the money column)", q.Ticker, inverse, rate)
		}

		// The read path hands the served-price surfaces that exact text.
		hist, err := store.ListFXHistory(ctx, q.Ticker, day, day)
		if err != nil {
			t.Fatalf("ListFXHistory %s: %v", q.Ticker, err)
		}
		if len(hist) != 1 || hist[0].InverseUSDText != inverse || hist[0].RateUSDText != rate {
			t.Errorf("%s: ListFXHistory = %+v, want one row with InverseUSDText %q, RateUSDText %q", q.Ticker, hist, inverse, rate)
		}
	}
}

// TestLatestFXQuotes_NewestRowPerTickerInWindow pins the read the forex
// worker seeds its held rates from on cold start: one row per ticker, the
// newest bucket at or after since, rate as exact NUMERIC text.
func TestLatestFXQuotes_NewestRowPerTickerInWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
		{Bucket: yesterday.AddDate(0, 0, -1), Ticker: "AED", RateUSD: 3.6, Source: "massive"},
		{Bucket: yesterday, Ticker: "AED", RateUSD: 3.6725, Source: "massive"},
		{Bucket: today, Ticker: "EUR", RateUSD: 0.92, Source: "ecb"},
		{Bucket: today.AddDate(0, 0, -10), Ticker: "CNY", RateUSD: 7.1, Source: "massive"},
	}); err != nil {
		t.Fatalf("InsertFXQuoteBatch: %v", err)
	}

	got, err := store.LatestFXQuotes(ctx, today.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("LatestFXQuotes: %v", err)
	}
	byTicker := map[string]timescale.FXQuote{}
	for _, q := range got {
		if _, dup := byTicker[q.Ticker]; dup {
			t.Fatalf("ticker %s returned twice", q.Ticker)
		}
		byTicker[q.Ticker] = q
	}
	if len(byTicker) != 2 {
		t.Fatalf("got %d tickers %v, want AED and EUR (CNY is outside the window)", len(byTicker), got)
	}
	aed := byTicker["AED"]
	if !aed.Bucket.Equal(yesterday) || aed.RateUSDText != "3.6725" || aed.Source != "massive" {
		t.Errorf("AED = %+v, want bucket %v, rate 3.6725, source massive", aed, yesterday)
	}
	if eur := byTicker["EUR"]; !eur.Bucket.Equal(today) || eur.RateUSDText != "0.92" || eur.Source != "ecb" {
		t.Errorf("EUR = %+v, want bucket %v, rate 0.92, source ecb", eur, today)
	}
}

// TestListingDirectory_FuturePricedAtIsNotFresh executes the listing
// directory's price-freshness predicate on real Postgres. A `priced_at`
// in the future sits above the 24-hour floor for as long as it stays
// ahead of now(), so without the ceiling a frozen price stamped 2099 is
// served as fresh — by both readers and counted by the census.
func TestListingDirectory_FuturePricedAtIsNotFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	contractFresh := "C" + strings.Repeat("F", 55)
	contractFuture := "C" + strings.Repeat("U", 55)
	contractSkewed := "C" + strings.Repeat("S", 55)
	now := time.Now().UTC()
	entries := []timescale.ListingEntry{
		{
			Address: contractFresh, ListingID: "fresh", Symbol: "frs",
			PriceUSD: "1.25", PricedAt: now.Add(-time.Hour),
		},
		{
			Address: contractFuture, ListingID: "future", Symbol: "fut",
			PriceUSD: "9.75", PricedAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			Address: contractSkewed, ListingID: "skewed", Symbol: "skw",
			PriceUSD: "3.5", PricedAt: now.Add(time.Minute),
		},
	}
	if _, _, err := store.ReplaceListingDirectory(ctx, entries, "coingecko"); err != nil {
		t.Fatalf("ReplaceListingDirectory: %v", err)
	}

	byAddr, census, err := store.ListingDirectoryByAddress(ctx)
	if err != nil {
		t.Fatalf("ListingDirectoryByAddress: %v", err)
	}
	fut, ok := byAddr[contractFuture]
	if !ok {
		t.Fatal("the future-stamped row must stay RECOGNISED — only its price is withheld")
	}
	if fut.PriceUSD != "" || !fut.PricedAt.IsZero() {
		t.Errorf("future-stamped row served price %q at %v — a priced_at ahead of now() "+
			"must not count as fresh", fut.PriceUSD, fut.PricedAt)
	}
	if got := byAddr[contractFresh].PriceUSD; got != "1.25" {
		t.Errorf("fresh row price = %q, want %q", got, "1.25")
	}
	if got := byAddr[contractSkewed].PriceUSD; got != "3.5" {
		t.Errorf("a priced_at inside the clock-skew allowance lost its price: got %q", got)
	}
	if census.Priced != 2 {
		t.Errorf("census.Priced = %d, want 2 (fresh, skewed; future excluded)", census.Priced)
	}

	contracts, _, err := store.ListingDirectoryContracts(ctx)
	if err != nil {
		t.Fatalf("ListingDirectoryContracts: %v", err)
	}
	for _, e := range contracts {
		if e.Address == contractFuture && e.PriceUSD != "" {
			t.Errorf("contracts read served the future-stamped price %q", e.PriceUSD)
		}
	}
}

// TestOracleUpdate_ReDeriveThatMovesIdentityDoesNotDuplicate reproduces
// the duplicate-row bug: oracle_updates' primary key carries ts, and the reflector
// op_index formula changed (eventFanoutStride), so a replay over pre-change
// history writes a second row for the same observation instead of
// correcting the first. The assertions state the CORRECT outcome (one row
// per observation); they fail on the current schema and writer.
func TestOracleUpdate_ReDeriveThatMovesIdentityDoesNotDuplicate(t *testing.T) {
	t.Skip("GH-1328 open: the supersede design (identity lookup without a ts predicate " +
		"across an unretained hypertable) is unresolved; remove this skip to reproduce")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Operation 1, event 0, vector slot 3 under the old formula
	// (op*1024 + i) and the current one ((op*64 + ev)*1024 + i).
	old := c.OracleUpdate{
		Source: "reflector-dex", Ledger: 50_000_321, TxHash: strings.Repeat("cd", 32),
		OpIndex: 1*1024 + 3, Timestamp: ts, Asset: xlm, Quote: usd,
		Price: c.NewAmount(big.NewInt(12_345_678_901_234)), Decimals: 14,
	}
	if err := store.InsertOracleUpdate(ctx, old); err != nil {
		t.Fatalf("insert pre-change row: %v", err)
	}

	rederived := old
	rederived.OpIndex = (1*64+0)*1024 + 3
	if err := store.InsertOracleUpdate(ctx, rederived); err != nil {
		t.Fatalf("insert re-derived row: %v", err)
	}
	moved := old
	moved.OpIndex = rederived.OpIndex
	moved.Timestamp = ts.Add(time.Second)
	if err := store.InsertOracleUpdate(ctx, moved); err != nil {
		t.Fatalf("insert ts-corrected row: %v", err)
	}

	var rows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM oracle_updates WHERE source = $1 AND ledger = $2 AND tx_hash = $3`,
		old.Source, old.Ledger, old.TxHash).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("oracle_updates rows for one observation = %d after re-derives that moved op_index and ts, want 1", rows)
	}
}

// oracleLatestReferenceSQL is the pre-rewrite statement, kept as the
// semantic oracle the served query must agree with row for row.
const oracleLatestReferenceSQL = `
	SELECT DISTINCT ON (source, quote)
	       source, asset, quote, ledger, tx_hash, op_index, ts
	  FROM oracle_updates
	 WHERE asset = ANY($1) AND ($2 = '' OR source = $2)
	 ORDER BY source, quote, ts DESC, ledger DESC`

// TestLatestOracleUpdatesForAssets_NoFullSort pins the /v1/oracle/latest
// read to a plan that does not sort every matching row. The DISTINCT ON
// form sorted all of an asset's history (318,908 rows, a 63 MB external
// merge, 541 ms warm on r1) to emit 7; the served shape
// aggregates max(ts) per (source, asset, quote) — answered from the
// compressed batches' metadata — and fetches one row per stream.
func TestLatestOracleUpdatesForAssets_NoFullSort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	db.SetMaxOpenConns(1)

	seedOracleLatestFixture(t, ctx, db)

	native := c.NativeAsset()
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	keys := []c.Asset{native, xlm}
	keyStrs := []string{native.String(), xlm.String()}

	got, err := store.LatestOracleUpdatesForAssets(ctx, keys, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets: %v", err)
	}
	stmt := capturePreparedStatement(t, ctx, db, nil, "FROM oracle_updates", "asset = ANY($1)")

	for _, src := range []string{"", "reflector", "dia"} {
		rows, err := store.LatestOracleUpdatesForAssets(ctx, keys, src)
		if err != nil {
			t.Fatalf("LatestOracleUpdatesForAssets(%q): %v", src, err)
		}
		assertOracleLatestMatchesReference(t, ctx, db, rows, keyStrs, src)
	}

	byStream := map[string]c.OracleUpdate{}
	for _, u := range got {
		byStream[u.Source+"|"+u.Quote.String()] = u
	}
	// A stream whose only rows sit in a compressed chunk months back is
	// still "latest": the read is unbounded in age.
	if u, ok := byStream["dia|fiat:USD"]; !ok {
		t.Errorf("stale dia/USD stream (compressed-only) missing from %d rows", len(got))
	} else if u.Ledger != 7777777 {
		t.Errorf("dia/USD ledger = %d at %s, want 7777777 — the late write into the partial compressed chunk", u.Ledger, u.Timestamp)
	}
	// Same ts across two alias keys: the higher ledger wins, as before.
	if u := byStream["reflector|fiat:USD"]; u.Asset.String() != xlm.String() || u.Ledger != 999_999_999 {
		t.Errorf("reflector/USD tie = %s ledger %d, want crypto:XLM ledger 999999999", u.Asset, u.Ledger)
	}

	pc := explainOracleLatest(t, ctx, db, stmt, keyStrs)
	t.Logf("force_custom_plan: %s", pc)
	if pc.maxSortRows > pc.outRowsBound {
		t.Errorf("a Sort node processed %d rows, more than the %d (source, asset, quote) streams "+
			"the answer is drawn from — the plan sorts history again: %v", pc.maxSortRows, pc.outRowsBound, pc.nodes)
	}
	// Rows the compressed tier hands up: one per batch when max(ts) comes
	// from batch metadata, every row when it decompresses. The Sort bound
	// above cannot see the second — a HashAggregate absorbs it.
	if pc.columnarRows*10 > pc.matchingRows {
		t.Errorf("compressed-chunk scans emitted %d of %d matching rows — the aggregate decompresses "+
			"history instead of reading batch metadata: %v", pc.columnarRows, pc.matchingRows, pc.nodes)
	}
	if pc.tempBlocks > 0 {
		t.Errorf("plan spilled %d temp blocks: %v", pc.tempBlocks, pc.nodes)
	}
}

// seedOracleLatestFixture writes 3 sources x 3 assets x 2 quotes at 30-min
// cadence over 120 days (~100k rows across several chunks), plus a stream
// that stopped 100 days ago and a cross-alias (ts) tie, then compresses
// every chunk older than a week as the r1 policy does.
func seedOracleLatestFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(q)[:60], err)
		}
	}
	const insert = `
		INSERT INTO oracle_updates
		       (source, contract_id, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
		SELECT s.source, NULL,
		       (extract(epoch FROM g.ts)::bigint / 5)::int * 4 + array_position($2::text[], a.asset),
		       md5(s.source || a.asset || q.quote || g.ts::text) || md5(g.ts::text || s.source),
		       0, g.ts, a.asset, q.quote, 1000 + (extract(epoch FROM g.ts)::bigint % 97), 7
		  FROM unnest($1::text[]) AS s(source),
		       unnest($2::text[]) AS a(asset),
		       unnest($3::text[]) AS q(quote),
		       generate_series($4::timestamptz, $5::timestamptz, $6::interval) AS g(ts)`
	exec(insert, "{reflector,band,redstone}", "{native,crypto:XLM,crypto:BTC}", "{fiat:USD,fiat:EUR}",
		now.Add(-120*24*time.Hour), now.Add(-time.Hour), "30 minutes")
	exec(insert, "{dia}", "{crypto:XLM}", "{fiat:USD}",
		now.Add(-110*24*time.Hour), now.Add(-100*24*time.Hour), "1 hour")
	// reflector publishes the newest XLM/USD reading under both alias keys at
	// the same ts; the crypto:XLM row carries the higher ledger.
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('reflector', 999999999, repeat('a', 64), 1, $1, 'crypto:XLM', 'fiat:USD', 5, 7)`, now)
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('reflector', 999999998, repeat('b', 64), 1, $1, 'native', 'fiat:USD', 5, 7)`, now)

	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('oracle_updates', older_than => now() - INTERVAL '7 days') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed < 2 {
		t.Fatalf("compressed %d oracle_updates chunks, want >= 2 — the fixture would not exercise the compressed tier", compressed)
	}
	// A late write into a compressed chunk (the ON CONFLICT upsert path on a
	// replay) leaves it partial; it is also dia's newest reading.
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('dia', 7777777, repeat('c', 64), 0, $1, 'crypto:XLM', 'fiat:USD', 5, 7)`,
		now.Add(-99*24*time.Hour))
	exec(`ANALYZE oracle_updates`)
}

func assertOracleLatestMatchesReference(t *testing.T, ctx context.Context, db *sql.DB, got []c.OracleUpdate, keys []string, src string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, oracleLatestReferenceSQL, keys, src)
	if err != nil {
		t.Fatalf("reference query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var want []string
	for rows.Next() {
		var source, asset, quote, txHash string
		var ledger, opIndex int
		var ts time.Time
		if err := rows.Scan(&source, &asset, &quote, &ledger, &txHash, &opIndex, &ts); err != nil {
			t.Fatalf("reference scan: %v", err)
		}
		want = append(want, fmt.Sprintf("%s|%s|%s|%d|%s|%d|%s", source, asset, quote, ledger, txHash, opIndex, ts.UTC().Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reference rows: %v", err)
	}
	have := make([]string, 0, len(got))
	for _, u := range got {
		have = append(have, fmt.Sprintf("%s|%s|%s|%d|%s|%d|%s", u.Source, u.Asset, u.Quote, u.Ledger, u.TxHash, u.OpIndex, u.Timestamp.UTC().Format(time.RFC3339Nano)))
	}
	slices.Sort(want)
	slices.Sort(have)
	if len(want) == 0 {
		t.Fatalf("reference returned no rows for source %q — the comparison would be vacuous", src)
	}
	if !slices.Equal(have, want) {
		t.Errorf("source %q: served rows differ from the DISTINCT ON reference\n got: %v\nwant: %v", src, have, want)
	}
}

type oracleLatestPlan struct {
	maxSortRows  int64
	outRowsBound int64
	tempBlocks   int64
	columnarRows int64
	matchingRows int64
	executionMS  float64
	nodes        []string
}

func (p oracleLatestPlan) String() string {
	return fmt.Sprintf("max_sort_rows=%d streams=%d columnar_rows=%d matching_rows=%d temp_blocks=%d execution_ms=%.1f",
		p.maxSortRows, p.outRowsBound, p.columnarRows, p.matchingRows, p.tempBlocks, p.executionMS)
}

func explainOracleLatest(t *testing.T, ctx context.Context, db *sql.DB, stmt string, keys []string) oracleLatestPlan {
	t.Helper()
	var streams, matching int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(DISTINCT (source, asset, quote)), count(*) FROM oracle_updates WHERE asset = ANY($1)`,
		keys).Scan(&streams, &matching); err != nil {
		t.Fatalf("count streams: %v", err)
	}
	// The serving pool's plan mode and r1's random_page_cost (postgresql.conf.j2).
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = force_custom_plan`)
	mustExecPlan(t, ctx, db, `SET random_page_cost = 1.1`)
	defer mustExecPlan(t, ctx, db, `RESET random_page_cost`)
	mustExecPlan(t, ctx, db, `PREPARE oracle_latest_probe AS `+stmt)
	defer mustExecPlan(t, ctx, db, `DEALLOCATE oracle_latest_probe`)
	defer mustExecPlan(t, ctx, db, `RESET plan_cache_mode`)
	q := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE oracle_latest_probe('{` + strings.Join(keys, ",") + `}', '')`
	var raw string
	for range 2 {
		if err := db.QueryRowContext(ctx, q).Scan(&raw); err != nil {
			t.Fatalf("EXPLAIN ANALYZE: %v", err)
		}
	}
	type node struct {
		NodeType          string  `json:"Node Type"`
		Provider          string  `json:"Custom Plan Provider"`
		ActualRows        float64 `json:"Actual Rows"`
		ActualLoops       float64 `json:"Actual Loops"`
		TempWrittenBlocks int64   `json:"Temp Written Blocks"`
		Plans             []node  `json:"Plans"`
	}
	var doc []struct {
		Plan          node    `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	out := oracleLatestPlan{outRowsBound: streams, matchingRows: matching, executionMS: doc[0].ExecutionTime, tempBlocks: doc[0].Plan.TempWrittenBlocks}
	var walk func(n node)
	walk = func(n node) {
		out.nodes = append(out.nodes, strings.TrimSpace(n.NodeType+" "+n.Provider))
		if strings.HasPrefix(n.Provider, "Columnar") || n.Provider == "DecompressChunk" {
			out.columnarRows += int64(n.ActualRows * n.ActualLoops)
		}
		if n.NodeType == "Sort" && int64(n.ActualRows*n.ActualLoops) > out.maxSortRows {
			out.maxSortRows = int64(n.ActualRows * n.ActualLoops)
		}
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}

// TestStorage_LatestOracleUpdatesForAssets_KeepsBothLiveQuotes (Q095).
//
// Redstone publishes EUROC as two independent live feeds from the same
// source: EUROC/EUR and EUROC/USD. A
// `DISTINCT ON (source)` in LatestOracleUpdatesForAssets would collapse both
// into one row — whichever quote had the higher (ts, ledger) silently
// winning. This proves both survive, and that
// `?quote=`-shaped filtering (done by the caller) can recover exactly
// one.
func TestStorage_LatestOracleUpdatesForAssets_KeepsBothLiveQuotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	euroc, err := c.NewCryptoAsset("EUROC")
	if err != nil {
		t.Fatalf("NewCryptoAsset(EUROC): %v", err)
	}
	eur, _ := c.NewFiatAsset("EUR")
	usd, _ := c.NewFiatAsset("USD")

	price, _ := new(big.Int).SetString("100030000", 10)    // 1.0003 EUR
	priceUSD, _ := new(big.Int).SetString("113980000", 10) // 1.1398 USD
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		{
			Source: "redstone", Ledger: 60_000_001,
			TxHash:  "3333333333333333333333333333333333333333333333333333333333333333",
			OpIndex: 0, Timestamp: ts.Add(-time.Minute), // older
			Asset: euroc, Quote: eur,
			Price: c.NewAmount(price), Decimals: 8,
		},
		{
			Source: "redstone", Ledger: 60_000_002,
			TxHash:  "4444444444444444444444444444444444444444444444444444444444444444",
			OpIndex: 0, Timestamp: ts, // newer
			Asset: euroc, Quote: usd,
			Price: c.NewAmount(priceUSD), Decimals: 8,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	got, err := store.LatestOracleUpdatesForAssets(ctx, []c.Asset{euroc}, "redstone")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LatestOracleUpdatesForAssets(EUROC, redstone) returned %d row(s), want 2 (one per live quote): %+v", len(got), got)
	}
	byQuote := map[string]c.OracleUpdate{}
	for _, u := range got {
		byQuote[u.Quote.String()] = u
	}
	eurRow, ok := byQuote[eur.String()]
	if !ok {
		t.Fatalf("EUROC/EUR row missing from %+v", got)
	}
	usdRow, ok := byQuote[usd.String()]
	if !ok {
		t.Fatalf("EUROC/USD row missing from %+v", got)
	}
	if eurRow.Ledger != 60_000_001 {
		t.Errorf("EUROC/EUR ledger = %d, want 60000001 (the older, EUR-quoted observation)", eurRow.Ledger)
	}
	if usdRow.Ledger != 60_000_002 {
		t.Errorf("EUROC/USD ledger = %d, want 60000002 (the newer, USD-quoted observation)", usdRow.Ledger)
	}
}

// TestStorage_OracleRawRowsConsumers is the consumer half of the oracle
// capture-totality design against real Timescale: with a mapped row and
// raw rows in the same window, (1) the MEV scan — the only unkeyed
// consumer, feeding the liquidation_cascade correlator — returns the
// mapped row only; (2) the source bespoke page counts the raw feeds
// (totality) and surfaces them as the "Unmapped feeds" KPI; (3) the
// unfiltered streams read still lists the raw row (the API boundary,
// not storage, applies include_unmapped).
func TestStorage_OracleRawRowsConsumers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, _ := c.NewFiatAsset("USD")
	raw, err := c.NewOracleRawAsset("NOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	raw2, err := c.NewOracleRawAsset("ALSONOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	price, _ := new(big.Int).SetString("12420000000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)
	const tx = "3333333333333333333333333333333333333333333333333333333333333333"

	seeds := []c.OracleUpdate{
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 0, Timestamp: ts, Asset: c.NativeAsset(), Quote: usd, Price: c.NewAmount(price), Decimals: 14},
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 1, Timestamp: ts, Asset: raw, Quote: usd, Price: c.NewAmount(price), Decimals: 14},
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 2, Timestamp: ts, Asset: raw2, Quote: usd, Price: c.NewAmount(price), Decimals: 14},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	// (1) MEV scan: raw rows excluded in SQL.
	refs, err := store.OracleUpdatesForMEVScan(ctx, ts.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("OracleUpdatesForMEVScan: %v", err)
	}
	if len(refs) != 1 || refs[0].Asset != "native" {
		t.Fatalf("OracleUpdatesForMEVScan = %+v, want only the native row (raw rows are not cascade evidence)", refs)
	}

	// (2) Bespoke page: totality counted, unmapped surfaced as a KPI.
	blk, err := store.BuildProtocolBespoke(ctx, "reflector-cex", "oracle", 7)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with rows in the window")
	}
	kpi := map[string]string{}
	for _, k := range blk.KPIs {
		kpi[k.Label] = k.Value
	}
	if kpi["Updates (7d)"] != "3" || kpi["Distinct feeds"] != "3" {
		t.Errorf("KPIs = %v, want Updates (7d)=3 and Distinct feeds=3 (totality counts raw rows)", kpi)
	}
	if kpi["Unmapped feeds"] != "2" {
		t.Errorf("Unmapped feeds KPI = %q, want 2", kpi["Unmapped feeds"])
	}

	// (3) Unfiltered streams read lists the raw rows.
	streams, err := store.LatestOracleStreams(ctx)
	if err != nil {
		t.Fatalf("LatestOracleStreams: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range streams {
		seen[u.Asset.String()] = true
	}
	for _, want := range []string{"native", "raw:NOTACOIN", "raw:ALSONOTACOIN"} {
		if !seen[want] {
			t.Errorf("LatestOracleStreams missing %s (got %v)", want, seen)
		}
	}
}

// TestStorage_OracleRawRowsReadBack is the reader half of the oracle
// capture-totality design (PR-1). Every oracle_updates reader
// re-parses the asset column with canonical.ParseAsset; on origin/main
// `raw:NOTACOIN` fell into the classic `<code>:<issuer>` split and the
// keyed readers returned an error (a 500 at the API), while
// LatestOracleStreams silently dropped the row. With the
// canonical.AssetOracleRaw variant the row round-trips through
// InsertOracleUpdate → SQL → ParseAsset on every reader, and stays
// invisible to readers keyed on a MAPPED asset (safe by keying).
func TestStorage_OracleRawRowsReadBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, _ := c.NewFiatAsset("USD")
	raw, err := c.NewOracleRawAsset("NOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	rawFeed, err := c.NewOracleRawAsset("SolvBTC.BBN_FUNDAMENTAL/USD")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	price, _ := new(big.Int).SetString("12420000000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		// A mapped row and a raw row from the SAME source + event — the
		// shape PR-2's decoders will emit for a mixed vector.
		{
			Source: "reflector-cex", Ledger: 52_430_001,
			TxHash:  "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex: 0, Timestamp: ts,
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 14,
		},
		{
			Source: "reflector-cex", Ledger: 52_430_001,
			TxHash:  "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex: 1, Timestamp: ts,
			Asset: raw, Quote: usd,
			Price: c.NewAmount(price), Decimals: 14,
		},
		// A RedStone-shaped feed_id with `.`, `_`, `/` in the symbol.
		{
			Source: "redstone", Ledger: 52_430_002,
			TxHash:  "2222222222222222222222222222222222222222222222222222222222222222",
			OpIndex: 0, Timestamp: ts,
			Asset: rawFeed, Quote: usd,
			Price: c.NewAmount(price), Decimals: 8,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	// LatestOracleUpdatesForAssets keyed on the raw asset returns it,
	// parsed back to the raw variant, code intact.
	got, err := store.LatestOracleUpdatesForAssets(ctx, []c.Asset{raw}, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets(raw): %v", err)
	}
	if len(got) != 1 || !got[0].Asset.Equal(raw) || got[0].OpIndex != 1 {
		t.Fatalf("LatestOracleUpdatesForAssets(raw) = %+v, want the one raw:NOTACOIN row at op_index 1", got)
	}
	if got[0].Asset.IsMapped() {
		t.Error("raw row read back as mapped")
	}

	// Keyed on the mapped asset it is NOT returned — safe by keying.
	got, err = store.LatestOracleUpdatesForAssets(ctx, []c.Asset{c.NativeAsset()}, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets(native): %v", err)
	}
	if len(got) != 1 || !got[0].Asset.Equal(c.NativeAsset()) {
		t.Fatalf("LatestOracleUpdatesForAssets(native) = %+v, want only the native row", got)
	}

	// LatestOracleObservation (the divergence seam) with a raw key.
	obs, err := store.LatestOracleObservation(ctx, "redstone",
		[]string{rawFeed.String()}, []string{usd.String()})
	if err != nil {
		t.Fatalf("LatestOracleObservation(raw feed): %v", err)
	}
	if obs == nil || !obs.Asset.Equal(rawFeed) {
		t.Fatalf("LatestOracleObservation(raw feed) = %+v, want raw:SolvBTC.BBN_FUNDAMENTAL/USD", obs)
	}
	if obs.Asset.Code != "SolvBTC.BBN_FUNDAMENTAL/USD" {
		t.Errorf("raw code not verbatim: %q", obs.Asset.Code)
	}

	// LatestAggregatorPricesForPair with a raw base.
	agg, err := store.LatestAggregatorPricesForPair(ctx, raw, usd, []string{"reflector-cex"})
	if err != nil {
		t.Fatalf("LatestAggregatorPricesForPair(raw): %v", err)
	}
	if len(agg) != 1 || !agg[0].Asset.Equal(raw) {
		t.Fatalf("LatestAggregatorPricesForPair(raw) = %+v, want the raw row", agg)
	}

	// LatestOracleUpdateForAsset (single-key, ErrNotFound shape).
	one, err := store.LatestOracleUpdateForAsset(ctx, "reflector-cex", raw)
	if err != nil {
		t.Fatalf("LatestOracleUpdateForAsset(raw): %v", err)
	}
	if !one.Asset.Equal(raw) {
		t.Errorf("LatestOracleUpdateForAsset(raw) = %+v", one)
	}

	// LatestOracleStreams is unkeyed: on main the raw rows were dropped
	// by its parse-failure `continue`; totality means they are listed.
	streams, err := store.LatestOracleStreams(ctx)
	if err != nil {
		t.Fatalf("LatestOracleStreams: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range streams {
		seen[u.Source+"|"+u.Asset.String()] = true
	}
	for _, want := range []string{
		"reflector-cex|native",
		"reflector-cex|raw:NOTACOIN",
		"redstone|raw:SolvBTC.BBN_FUNDAMENTAL/USD",
	} {
		if !seen[want] {
			t.Errorf("LatestOracleStreams missing %s (got %v)", want, seen)
		}
	}
}

// TestSweepOracleRederive pins the sweep's invariant on compressed chunks:
// it deletes an older-generation row only when a row at the run's own
// generation shares its identity with another ts, so it never deletes the
// only row of an identity nor a row whose identity has no run-generation
// twin. The decrement keeps source_entry_counts equal to the row count.
func TestSweepOracleRederive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mustAsset := func(a c.Asset, err error) c.Asset {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	xlm, btc, eth := mustAsset(c.NewCryptoAsset("XLM")), mustAsset(c.NewCryptoAsset("BTC")), mustAsset(c.NewCryptoAsset("ETH"))
	usd := mustAsset(c.NewFiatAsset("USD"))
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const gen = int64(100)

	row := func(source string, ledger uint32, tx string, op uint32, ts time.Time, asset c.Asset) c.OracleUpdate {
		return c.OracleUpdate{
			Source: source, Ledger: ledger, TxHash: strings.Repeat(tx, 32), OpIndex: op,
			Timestamp: ts, Asset: asset, Quote: usd,
			Price: c.NewAmount(big.NewInt(12_345)), Decimals: 7,
		}
	}
	insert := func(g int64, rows ...c.OracleUpdate) {
		t.Helper()
		store.SetDeriveGeneration(g)
		for _, u := range rows {
			if err := store.InsertOracleUpdate(ctx, u); err != nil {
				t.Fatalf("insert %+v: %v", u, err)
			}
		}
	}

	// Live generation-0 rows, compressed as they would be on r1.
	insert(0,
		row("reflector-dex", 1000, "a1", 5, t0, xlm),         // stale: gen-G twin below
		row("reflector-dex", 1001, "b1", 6, t0, xlm),         // no twin at all
		row("reflector-dex", 1002, "c1", 7, t0, xlm),         // op_index shifted at gen G
		row("reflector-dex", 5000, "d1", 1, t0, xlm),         // twin, but outside the range
		row("reflector-dex", 1003, "e1", 8, t0, xlm),         // twin only at gen 50
		row("band", 2000, "f1", 0, t0.Add(2*time.Hour), btc), // stale, future-dated
		// A nested relay sharing op_index 0. Its ts differs: an equal ts would
		// collide on the primary key.
		row("band", 2000, "f1", 0, t0.Add(3*time.Hour), eth),
	)
	var compressed int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT compress_chunk(ch) FROM show_chunks('oracle_updates') ch) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no oracle_updates chunk compressed; the compressed-DML path would go untested")
	}
	insert(50, row("reflector-dex", 1003, "e1", 8, t0.Add(time.Minute), xlm))
	insert(gen,
		row("reflector-dex", 1000, "a1", 5, t0.Add(time.Minute), xlm),
		row("reflector-dex", 1002, "c1", 70, t0, xlm),
		row("reflector-dex", 5000, "d1", 1, t0.Add(time.Minute), xlm),
		row("band", 2000, "f1", 0, t0, btc),
		row("band", 2001, "a2", 0, t0, btc), // a gen-G multi-update tx
		row("band", 2001, "a2", 0, t0.Add(30*time.Second), eth),
	)

	type key struct {
		ledger, op int
		ts         time.Time
		asset      string
	}
	rows := func(source string) []key {
		t.Helper()
		r, err := store.DB().QueryContext(ctx,
			`SELECT ledger, op_index, ts, asset FROM oracle_updates WHERE source = $1`, source)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var out []key
		for r.Next() {
			var k key
			if err := r.Scan(&k.ledger, &k.op, &k.ts, &k.asset); err != nil {
				t.Fatal(err)
			}
			k.ts = k.ts.UTC()
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.ledger != b.ledger {
				return a.ledger < b.ledger
			}
			if a.op != b.op {
				return a.op < b.op
			}
			if !a.ts.Equal(b.ts) {
				return a.ts.Before(b.ts)
			}
			return a.asset < b.asset
		})
		return out
	}
	tally := func(source string) int64 {
		t.Helper()
		var n int64
		if err := store.DB().QueryRowContext(ctx,
			`SELECT entry_count FROM source_entry_counts WHERE source = $1`, source).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sweep := func(sw timescale.OracleRederiveSweep) timescale.OracleRederiveSweepResult {
		t.Helper()
		res, err := store.SweepOracleRederive(ctx, sw)
		if err != nil {
			t.Fatalf("sweep %+v: %v", sw, err)
		}
		return res
	}

	reflBefore, bandBefore := rows("reflector-dex"), rows("band")
	if len(reflBefore) != 9 || len(bandBefore) != 5 {
		t.Fatalf("seed: %d reflector-dex / %d band rows, want 9 / 5", len(reflBefore), len(bandBefore))
	}

	t.Run("dry run deletes nothing", func(t *testing.T) {
		res := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, DryRun: true})
		if res.Deleted != 2 || res.OpIndexShifted != 1 {
			t.Errorf("dry run: would delete %d, op_index-shifted %d; want 2 (rows at 1000 and 1003, twins at any newer gen), 1", res.Deleted, res.OpIndexShifted)
		}
		if got := rows("reflector-dex"); len(got) != len(reflBefore) {
			t.Errorf("dry run deleted rows: %d left, want %d", len(got), len(reflBefore))
		}
		if _, err := store.SweepOracleRederive(ctx, timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100}); err == nil {
			t.Error("a deleting sweep without the run's generation must refuse")
		}
	})

	t.Run("deletes exactly the stale twin", func(t *testing.T) {
		res := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, Generation: gen})
		if res.Deleted != 1 || res.OpIndexShifted != 1 || res.FutureTS != 0 {
			t.Errorf("sweep = %+v, want 1 deleted, 1 op_index-shifted, 0 future-dated", res)
		}
		want := []key{
			{1000, 5, t0.Add(time.Minute), xlm.String()},
			{1001, 6, t0, xlm.String()},
			{1002, 7, t0, xlm.String()},
			{1002, 70, t0, xlm.String()},
			{1003, 8, t0, xlm.String()},
			{1003, 8, t0.Add(time.Minute), xlm.String()},
			{5000, 1, t0, xlm.String()},
			{5000, 1, t0.Add(time.Minute), xlm.String()},
		}
		assertKeys(t, rows("reflector-dex"), want)
		if got := tally("reflector-dex"); got != 8 {
			t.Errorf("source_entry_counts reflector-dex = %d, want 8 (9 inserted - 1 swept)", got)
		}
		if again := sweep(timescale.OracleRederiveSweep{Source: "reflector-dex", From: 900, To: 1100, Generation: gen}); again.Deleted != 0 {
			t.Errorf("second sweep deleted %d, want 0", again.Deleted)
		}
	})

	t.Run("band identity includes asset", func(t *testing.T) {
		if loose := sweep(timescale.OracleRederiveSweep{Source: "band", From: 1900, To: 2100, DryRun: true}); loose.Deleted != 2 {
			t.Fatalf("precondition: a 4-column band identity would delete %d rows, want 2 (the ETH relay too)", loose.Deleted)
		}
		res := sweep(timescale.OracleRederiveSweep{Source: "band", From: 1900, To: 2100, Generation: gen, AssetScoped: true})
		if res.Deleted != 1 || res.FutureTS != 1 {
			t.Errorf("sweep = %+v, want 1 deleted, 1 future-dated", res)
		}
		want := []key{
			{2000, 0, t0, btc.String()},
			{2000, 0, t0.Add(3 * time.Hour), eth.String()},
			{2001, 0, t0, btc.String()},
			{2001, 0, t0.Add(30 * time.Second), eth.String()},
		}
		assertKeys(t, rows("band"), want)
		if got := tally("band"); got != 4 {
			t.Errorf("source_entry_counts band = %d, want 4", got)
		}
	})
}

func assertKeys[K comparable](t *testing.T, got, want []K) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// 0003's stored comment, verbatim; 0176's down must restore exactly this.
const oracleUpdatesComment0003 = `Every observed oracle publication, one row per (source, ledger, tx_hash, op_index). ` +
	`Hypertable partitioned on ts. See ADR-0006.`

// TestOracleUpdatesPKComment pins the catalog comment on oracle_updates
// after migration 0176 up and down. 0003's comment claimed "one row
// per (source, ledger, tx_hash, op_index)" — the PRIMARY KEY also carries
// `ts`, so a decoder-ts-derivation fix that replays affected ledgers adds a
// second row for the same publication instead of replacing the first. The
// comment lives in pg_description, so only a real database can say what an
// operator reads through `\d+`.
func TestOracleUpdatesPKComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	got := oracleUpdatesTableComment(t, ctx, db)
	for _, want := range []string{
		"NOT because it is part of the logical",
		"canonical.OracleUpdate.ID()",
		"DELETE the stale row",
		"(source, ledger, tx_hash, op_index)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("oracle_updates comment missing %q; got %q", want, got)
		}
	}
	if strings.Contains(got, "one row per (source, ledger, tx_hash, op_index)") {
		t.Errorf("oracle_updates comment still claims the PK is (source, ledger, tx_hash, "+
			"op_index) alone, hiding that `ts` is also part of it; got %q", got)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(175); err != nil {
		t.Fatalf("migrate down to 175: %v", err)
	}
	if got := oracleUpdatesTableComment(t, ctx, db); got != oracleUpdatesComment0003 {
		t.Errorf("0176 down did not restore 0003's comment verbatim:\n got %q\nwant %q", got, oracleUpdatesComment0003)
	}
}

func oracleUpdatesTableComment(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('oracle_updates'::regclass, 'pg_class')`,
	).Scan(&c); err != nil {
		t.Fatalf("read oracle_updates comment: %v", err)
	}
	if !c.Valid {
		t.Fatal("oracle_updates has no catalog comment")
	}
	return c.String
}

// TestRouterRegistry0072DownRestoresNotes executes 0072 up, down and up
// again. The up appends a rename sentence to routers.notes; a down that
// restores only the name lets every re-up append the sentence again.
func TestRouterRegistry0072DownRestoresNotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 71)
	seedName, seedNotes := soroswapRouterRow(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 72)
	upName, upNotes := soroswapRouterRow(t, ctx, db)
	if upName != "soroswap-router" || upNotes == seedNotes {
		t.Fatalf("0072 up = (%q, %q), want the renamed row with the rename note appended", upName, upNotes)
	}

	applyMigrationsUpTo(t, dsn, 71)
	if name, notes := soroswapRouterRow(t, ctx, db); name != seedName || notes != seedNotes {
		t.Errorf("0072 down = (%q, %q), want the pre-0072 row (%q, %q)", name, notes, seedName, seedNotes)
	}

	applyMigrationsUpTo(t, dsn, 72)
	if name, notes := soroswapRouterRow(t, ctx, db); name != upName || notes != upNotes {
		t.Errorf("0072 up after a down = (%q, %q), want the first up's row (%q, %q)", name, notes, upName, upNotes)
	}
}

func soroswapRouterRow(t *testing.T, ctx context.Context, db *sql.DB) (name, notes string) {
	t.Helper()
	var n sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT name, notes FROM routers
         WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'`).Scan(&name, &n); err != nil {
		t.Fatalf("read soroswap router row: %v", err)
	}
	return name, n.String
}

// TestVWAPUSDFXResolver_SubstanceGate executes the valuation substance
// gate against real TimescaleDB, with the exact tiers installed so peg
// markets carry the volume_usd production gives them.
//
//   - USDX: the wash-ring shape. 0.1 USDX <-> 0.1 USDC round trips every
//     20 min ($4.80/day) set a $1 rate, and a two-trade USDX/XLM book
//     sets the same $1 through the bridge. Without the gate the resolver
//     prices USDX at $1; with it, neither market clears the floor.
//   - DEEP: 30 half-hourly $108.50 trades against USDC — clears the floor.
//   - OLD: the same market 30 days ago and nothing since — it values a
//     trade at its own time, held to the hour-grain floor.
func TestVWAPUSDFXResolver_SubstanceGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		tokenIssuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	)
	usdcID := "USDC-" + usdcIssuer
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	asset := func(code, issuer string) c.Asset {
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	usdc := asset("USDC", usdcIssuer)
	usdx, deep, old := asset("USDX", tokenIssuer), asset("DEEP", tokenIssuer), asset("OLD", tokenIssuer)

	at := time.Now().UTC().Truncate(time.Minute)
	hour := at.Truncate(time.Hour)
	then := hour.Add(-30 * 24 * time.Hour)

	nonce := 0
	insert := func(ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for i := range 30 {
		step := time.Duration(i) * 30 * time.Minute
		insert(hour.Add(-15*time.Hour+step), pair(c.NativeAsset(), usdc), 10_000_000_000, 2_500_000_000) // XLM at $0.25
		insert(hour.Add(-15*time.Hour+step), pair(deep, usdc), 1_000_000_000, 1_085_000_000)
		insert(then.Add(-15*time.Hour+step), pair(old, usdc), 1_000_000_000, 1_085_000_000)
	}
	for i := range 48 {
		insert(hour.Add(-17*time.Hour+time.Duration(i)*20*time.Minute), pair(usdx, usdc), 1_000_000, 1_000_000)
	}
	insert(hour.Add(-3*time.Hour), pair(usdx, c.NativeAsset()), 100_000_000, 400_000_000)
	insert(hour.Add(-2*time.Hour), pair(usdx, c.NativeAsset()), 100_000_000, 400_000_000)
	// The XLM-quote anchor read prices_1m before it was refreshed; stamp the
	// ring's XLM leg at its true $10 so the bridge has a bucket to use.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 10 WHERE base_asset = $1 AND quote_asset = 'native'`, usdx.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	for _, view := range []string{"prices_1m", "prices_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+view+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}

	resolver := func(disableGate bool) *timescale.VWAPUSDFXResolver {
		t.Helper()
		r, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
			USDPegs:              []string{usdcID},
			Freshness:            -1, // staleness is not under test
			DisableSubstanceGate: disableGate,
		})
		if err != nil {
			t.Fatalf("NewVWAPUSDFXResolver: %v", err)
		}
		return r
	}
	price := func(r *timescale.VWAPUSDFXResolver, a c.Asset, ts time.Time) (string, bool) {
		t.Helper()
		got, ok, err := r.USDPriceAt(ctx, a, ts)
		if err != nil {
			t.Fatalf("USDPriceAt(%s): %v", a, err)
		}
		return trimTrailingZeros(got), ok
	}

	if got, ok := price(resolver(true), usdx, at); !ok || got != "1" {
		t.Fatalf("fixture: ungated USDPriceAt(USDX) = (%q, %t), want the ring's $1", got, ok)
	}
	gated := resolver(false)
	if got, ok := price(gated, usdx, at); ok {
		t.Errorf("USDPriceAt(USDX) = %q: a wash-ring market valued a trade", got)
	}
	if got, ok := price(gated, deep, at); !ok || got != "1.085" {
		t.Errorf("USDPriceAt(DEEP) = (%q, %t), want 1.085 from a market with substance", got, ok)
	}
	if got, ok := price(gated, old, then); !ok || got != "1.085" {
		t.Errorf("USDPriceAt(OLD, 30d ago) = (%q, %t), want 1.085: deep then, dormant now", got, ok)
	}
	if got, ok := price(gated, c.NativeAsset(), at); !ok || got != "0.25" {
		t.Errorf("USDPriceAt(XLM) = (%q, %t), want the ungated 0.25 anchor", got, ok)
	}
}

// TestVWAPUSDFXResolver_QueriesPrices1m exercises the
// production path against a real postgres: seed an EURC/USDC
// trade, refresh prices_1m, then call USDPriceAt(EURC, now+1m) and
// verify the resolver picks up the VWAP through the USDC peg.
func TestVWAPUSDFXResolver_QueriesPrices1m(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	// Anchor 2h ago so the trade lands inside the prices_1m window
	// the CAGG materialises by default. Single trade is enough —
	// the resolver only needs one VWAP row.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trade := mkIntegrationTrade("sdex", 1, t0,
		pair,
		1_000_000_000, // 100 EURC at 7-decimals
		1_085_000_000) // 108.5 USDC at 7-decimals → 1.085 EUR/USD
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Resolver with USDC's classic asset key on the peg list.
	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-" + usdcIssuer},
		// -1 = freshness check disabled. A `0` is silently overridden to the 1h
		// default by the constructor, which would pass for this test (1m gap < 1h)
		// but fail any historical-replay test where the trade was older than 1h.
		Freshness: -1,
		// One trade is no market; this pins the lookup, not the gate.
		DisableSubstanceGate: true,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	// Query at the bucket-end timestamp. The resolver should hit
	// the seeded row.
	got, ok, err := resolver.USDPriceAt(ctx, eurc, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("expected resolver to find EURC/USDC VWAP, got ok=false")
	}
	// VWAP = quote/base = 1.085. NUMERIC is exact; CAGG-rendered
	// text strips trailing zeros but preserves precision.
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q", got, "1.085")
	}
}

// TestVWAPUSDFXResolver_DustBucketDoesNotOutrankRealBucket is the
// Postgres-semantics proof for the tier-3a dust floor (MNY-22, re-fixed
// after the circular first attempt was reverted at 7b69cd33).
//
// queryDB takes the FRESHEST qualifying bucket, so a single sub-cent
// fill landing in a newer minute than the real market sets the USD
// valuation rate for every trade quoted in that asset for the whole
// freshness window. Seeded here at production shape: a real 108.50-USDC
// bucket, then five minutes later a 2-stroop/40000-stroop crumb whose
// "price" is 20000 USDC per EURC — an artifact of dividing two tiny
// integers, worth $0.004.
//
// Without the floor this returns "20000". With it the crumb is excluded and the
// real bucket's 1.085 is served.
//
// The dust bucket is deliberately made of trades that DO carry
// usd_volume semantics identical to the real one (both are NULL here —
// see [TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume]), so this also
// demonstrates the floor discriminates without a USD-valued column.
func TestVWAPUSDFXResolver_DustBucketDoesNotOutrankRealBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	// The real market: 100 EURC for 108.50 USDC → 1.085.
	realBucket := mkIntegrationTrade("sdex", 1, t0, pair, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, realBucket); err != nil {
		t.Fatalf("InsertTrade(real): %v", err)
	}
	// The dust: 2 stroops of EURC for 40000 stroops of USDC — $0.004 of
	// the peg, five minutes FRESHER than the real bucket.
	dustAt := t0.Add(5 * time.Minute)
	dust := mkIntegrationTrade("sdex", 2, dustAt, pair, 2, 40_000)
	if err := store.InsertTrade(ctx, dust); err != nil {
		t.Fatalf("InsertTrade(dust): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Sanity-check the fixture against the CAGG itself: the dust bucket
	// must really be the freshest row and must really carry the absurd
	// VWAP, or the test would pass for the wrong reason.
	var (
		freshestBucket time.Time
		freshestVWAP   string
	)
	if err := store.DB().QueryRowContext(ctx,
		`SELECT bucket, vwap::text FROM prices_1m
		  WHERE base_asset = $1 AND quote_asset = $2
		  ORDER BY bucket DESC LIMIT 1`,
		eurc.String(), usdc.String(),
	).Scan(&freshestBucket, &freshestVWAP); err != nil {
		t.Fatalf("fixture check: %v", err)
	}
	if !freshestBucket.UTC().Equal(dustAt) {
		t.Fatalf("fixture: freshest bucket = %s, want the dust bucket %s", freshestBucket.UTC(), dustAt)
	}
	if got := trimTrailingZeros(freshestVWAP); got != "20000" {
		t.Fatalf("fixture: dust bucket vwap = %q, want 20000", got)
	}

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-" + usdcIssuer},
		// -1 disables the freshness bound so BOTH buckets are eligible;
		// the choice between them is the floor's job, not staleness'.
		Freshness: -1,
		// Two buckets are no market; this pins the dust floor, not the gate.
		DisableSubstanceGate: true,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := resolver.USDPriceAt(ctx, eurc, dustAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("expected the real EURC/USDC bucket to still resolve, got ok=false")
	}
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q (the real bucket; %q is the $0.004 dust crumb)",
			got, "1.085", "20000")
	}
}

// TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume pins the
// non-circularity of the tier-3a dust floor, and is the regression
// guard against re-introducing the `volume_usd` form reverted at
// 7b69cd33.
//
// `trades.usd_volume` is written by the USD-resolution step, and tier
// 3a IS that step for a pair quoted in a peg — so a floor keyed on
// `prices_1m.volume_usd` can never be cleared by a pair the resolver
// has not already priced. The assertion below is deliberately
// two-sided: the seeded bucket carries volume_usd = 0 (no resolver
// installed on this store, so every trade inserted NULL and the CAGG
// coalesced it), AND the resolver still returns the rate.
func TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trade := mkIntegrationTrade("sdex", 1, t0, pair, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// The premise: the bucket has NOT been USD-valued. If this ever
	// stops holding, the circularity argument needs re-deriving before
	// anyone reaches for volume_usd again.
	var nullUSDVolumes int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM trades WHERE usd_volume IS NULL`,
	).Scan(&nullUSDVolumes); err != nil {
		t.Fatalf("usd_volume check: %v", err)
	}
	if nullUSDVolumes != 1 {
		t.Fatalf("fixture: %d trades with NULL usd_volume, want 1", nullUSDVolumes)
	}
	var volumeUSD string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT volume_usd::text FROM prices_1m
		  WHERE base_asset = $1 AND quote_asset = $2`,
		eurc.String(), usdc.String(),
	).Scan(&volumeUSD); err != nil {
		t.Fatalf("volume_usd check: %v", err)
	}
	if v := trimTrailingZeros(volumeUSD); v != "0" {
		t.Fatalf("fixture: prices_1m.volume_usd = %q, want 0 (nothing has valued this pair yet)", v)
	}

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs:   []string{"USDC-" + usdcIssuer},
		Freshness: -1,
		// The gate reads volume_usd, which the exact tiers stamp on a peg
		// market in production; this pins the dust floor's own bootstrap.
		DisableSubstanceGate: true,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := resolver.USDPriceAt(ctx, eurc, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("resolver could not bootstrap a never-priced pair (ok=false) — the tier-3a floor is circular again")
	}
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q", got, "1.085")
	}
}

// trimTrailingZeros renders a Postgres NUMERIC::text in the same
// canonical form the resolver returns, so fixture assertions can be
// written against the arithmetic value rather than the column scale.
func trimTrailingZeros(s string) string {
	if !strings.ContainsRune(s, '.') {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// TestVWAPUSDFXResolver_NoMatchReturnsOk False — asset with no
// against-peg row produces (`""`, ok=false, nil err). Pre-Phase-2
// behaviour preserved for assets we don't cover yet.
func TestVWAPUSDFXResolver_NoMatchReturnsOk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	resolver, _ := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	})

	// No trades inserted; asking for an obscure asset against the
	// peg → no match. The boundary is (empty rate, ok=false, nil err).
	obscure, _ := c.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	_ = usdc
	got, ok, err := resolver.USDPriceAt(ctx, obscure, time.Now().UTC())
	if err != nil {
		t.Errorf("err = %v, want nil for no-match case", err)
	}
	if ok {
		t.Errorf("expected ok=false for no-data asset, got rate=%q ok=true", got)
	}
	if got != "" {
		t.Errorf("expected empty rate, got %q", got)
	}
}

// anchorFixture seeds trades and lets a test stamp each one's usd_volume, the
// anchor's weight, independently of its legs.
type anchorFixture struct {
	t     *testing.T
	ctx   context.Context
	store *timescale.Store
	now   time.Time
	nonce int
	usd   map[int]string
}

func newAnchorFixture(t *testing.T) *anchorFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &anchorFixture{t: t, ctx: ctx, store: store, now: time.Now().UTC().Truncate(time.Minute), usd: map[int]string{}}
}

// contract returns a deterministic contract asset, registered as discovered
// so the detail query can resolve it.
func (f *anchorFixture) contract(label string) c.Asset {
	f.t.Helper()
	h := sha256.Sum256([]byte("xlm-usd-anchor/" + label))
	id, err := strkey.Encode(strkey.VersionByteContract, h[:])
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := c.NewSorobanAsset(id)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.RecordDiscovered(f.ctx, discovery.Hit{
		ContractID: id, Kind: discovery.KindSEP41, EventType: discovery.EventTransfer,
		Ledger: 50_000_000, ObservedAtRFC3339: f.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		f.t.Fatalf("RecordDiscovered %s: %v", label, err)
	}
	return a
}

// trade records base -> quote at ago before now; usd, when set, is the
// trade's usd_volume.
func (f *anchorFixture) trade(source string, ago time.Duration, base, quote c.Asset, baseAmt, quoteAmt int64, usd string) {
	f.t.Helper()
	p, err := c.NewPair(base, quote)
	if err != nil {
		f.t.Fatal(err)
	}
	f.nonce++
	if err := f.store.InsertTrade(f.ctx, mkIntegrationTrade(source, f.nonce, f.now.Add(-ago), p, baseAmt, quoteAmt)); err != nil {
		f.t.Fatalf("InsertTrade %s #%d: %v", source, f.nonce, err)
	}
	if usd != "" {
		f.usd[50_000_000+f.nonce] = usd
	}
}

// refresh stamps usd_volume, materialises prices_1m and runs the rollup.
func (f *anchorFixture) refresh() {
	f.t.Helper()
	db := f.store.DB()
	for ledger, usd := range f.usd {
		if _, err := db.ExecContext(f.ctx, `UPDATE trades SET usd_volume = $1::numeric WHERE ledger = $2`, usd, ledger); err != nil {
			f.t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	for _, q := range []string{
		`UPDATE trades SET usd_volume = 100 WHERE usd_volume IS NULL`,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(f.ctx, q); err != nil {
			f.t.Fatalf("%s: %v", q, err)
		}
	}
	if err := f.store.RefreshAssetListingRollups(f.ctx); err != nil {
		f.t.Fatalf("RefreshAssetListingRollups: %v", err)
	}
}

// snapshot returns the rollup's stored price and change_7d_pct for id; ok is
// false when the rollup did not price it.
func (f *anchorFixture) snapshot(id string) (price, change7d sql.NullString, ok bool) {
	f.t.Helper()
	err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT price_usd::text, change_7d_pct::text FROM asset_price_snapshot WHERE asset_id = $1`, id).
		Scan(&price, &change7d)
	if err == sql.ErrNoRows {
		return price, change7d, false
	}
	if err != nil {
		f.t.Fatalf("snapshot %s: %v", id, err)
	}
	return price, change7d, true
}

func ratEq(t *testing.T, got, want string) bool {
	t.Helper()
	g, ok1 := new(big.Rat).SetString(got)
	w, ok2 := new(big.Rat).SetString(want)
	return ok1 && ok2 && g.Cmp(w) == 0
}

// TestXLMUSDAnchor_AliasDirectionPrecedenceAndTimeOfTrade seeds XLM/USD as a
// timeline of single-form windows and prices one XLM-quoted asset in each,
// on the rollup (grid anchor) and the detail query (lateral anchor):
//
//	N-12m   native form, both directions: 0.40 ($300) + 1/2 ($100) -> 0.425
//	N-2m    SAC dust 0.90 ($0.50), newer than the native minute -> ignored
//	N-3h10m crypto:XLM/fiat:USD only (CEX)                   0.30
//	N-6h5m  (USDC-SAC, XLM-SAC) only, the inverted direction 0.20
//	N-10h1m native 0.99 — 61 min before S's trade            -> S unpriced
//	N-12h59 native 0.25 — 59 min before T's trade            -> T priced
//	N-7d2h30m native 0.40, seeding the 7d grid edge for E's change.
//
// Every asset trades at 2 XLM (R at 4, E at 1), so price = 2 × anchor at
// its own minute. On main, Q, R and T are unpriced (no native/USDC row),
// native reads 0.40, and P reads 0.80.
func TestXLMUSDAnchor_AliasDirectionPrecedenceAndTimeOfTrade(t *testing.T) {
	f := newAnchorFixture(t)
	const unit, cexUnit = 10_000_000, 100_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC, err := c.NewSorobanAsset(c.XLMSacContractID)
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	cryptoXLM := c.Asset{Type: c.AssetCrypto, Code: "XLM"}
	fiatUSD := c.Asset{Type: c.AssetFiat, Code: "USD"}
	m, h := time.Minute, time.Hour

	f.trade("sdex", 12*m, native, usdc, 10*unit, 4*unit, "300")
	f.trade("sdex", 12*m, usdc, native, 1*unit, 2*unit, "100")
	f.trade("soroswap", 2*m, xlmSAC, usdcSAC, 10*unit, 9*unit, "0.5")
	f.trade("coinbase", 3*h+10*m, cryptoXLM, fiatUSD, 10*cexUnit, 3*cexUnit, "3")
	f.trade("aquarius", 6*h+5*m, usdcSAC, xlmSAC, 1*unit, 5*unit, "1")
	f.trade("sdex", 10*h+1*m, native, usdc, 10*unit, 99*unit/10, "9.9")
	f.trade("sdex", 12*h+59*m, native, usdc, 4*unit, 1*unit, "1")
	f.trade("sdex", 7*24*h+2*h+30*m, native, usdc, 10*unit, 4*unit, "4")

	P, Q, R, S := f.contract("P"), f.contract("Q"), f.contract("R"), f.contract("S")
	T, U, E := f.contract("T"), f.contract("U"), f.contract("E")
	H, K := f.contract("H"), f.contract("K")
	f.trade("soroswap", 1*m, P, xlmSAC, 1*unit, 2*unit, "")
	f.trade("sdex", 3*h, Q, native, 1*unit, 2*unit, "")
	f.trade("aquarius", 6*h, xlmSAC, R, 4*unit, 1*unit, "")
	f.trade("sdex", 9*h, S, native, 1*unit, 2*unit, "")
	f.trade("sdex", 12*h, T, native, 1*unit, 2*unit, "")
	f.trade("soroswap", 2*24*h, U, usdc, 2*unit, 3*unit, "")
	f.trade("sdex", 9*h, U, native, 1*unit, 2*unit, "")
	f.trade("sdex", 1*m, E, native, 1*unit, 1*unit, "")
	f.trade("sdex", 7*24*h+2*h-5*m, E, native, 1*unit, 1*unit, "")
	f.trade("soroswap", 3*h, K, xlmSAC, 1*unit, 2*unit, "")
	f.trade("soroswap", 30*m, H, K, 1*unit, 3*unit, "")
	f.refresh()

	want := map[string]string{
		P.String(): "0.85", // native-form anchor; the newer SAC dust is ignored
		Q.String(): "0.60", // crypto:XLM-only window, at Q's own minute
		R.String(): "0.80", // SAC-only, inverted-only window
		T.String(): "0.50", // anchor 59 min old
		U.String(): "1.5",  // stale anchor -> the older direct row still prices
		E.String(): "0.425",
	}
	for id, w := range want {
		price, _, ok := f.snapshot(id)
		if !ok || !ratEq(t, price.String, w) {
			t.Errorf("rollup %s price_usd = %v (ok=%v), want %s", id, price.String, ok, w)
		}
		row, err := f.store.GetAssetBySlug(f.ctx, id)
		if err != nil {
			t.Errorf("GetAssetBySlug %s: %v", id, err)
			continue
		}
		if row.PriceUSD == nil || !ratEq(t, *row.PriceUSD, w) {
			t.Errorf("detail %s price_usd = %s, want %s", id, derefOr(row.PriceUSD), w)
		}
	}

	if _, _, ok := f.snapshot(S.String()); ok {
		t.Errorf("rollup priced S through an anchor 61 min older than its trade")
	}
	if row, err := f.store.GetAssetBySlug(f.ctx, S.String()); err != nil || row.PriceUSD != nil {
		t.Errorf("detail S price_usd = %s (err %v), want unpriced", derefOr(row.PriceUSD), err)
	}

	if _, ch, _ := f.snapshot(E.String()); !ch.Valid || !ratEq(t, ch.String, "6.25") {
		t.Errorf("rollup E change_7d_pct = %v, want 6.25 (anchor at the 7d grid edge)", ch.String)
	}

	if price, _, ok := f.snapshot("native"); !ok || !ratEq(t, price.String, "0.425") {
		t.Errorf("rollup native price_usd = %v, want 0.425 (volume_usd-weighted, both directions)", price.String)
	}
	nrow, err := f.store.GetNativeAssetRow(f.ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if nrow.PriceUSD == nil || !ratEq(t, *nrow.PriceUSD, "0.425") {
		t.Errorf("native row price_usd = %s, want 0.425", derefOr(nrow.PriceUSD))
	}

	cands, err := f.store.TransitiveUSDPriceCandidates(f.ctx, H.String())
	if err != nil {
		t.Fatalf("TransitiveUSDPriceCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Hop != K.String() || !ratEq(t, cands[0].PriceUSD, "1.8") {
		t.Errorf("transitive H via K = %+v, want one candidate at 1.8 (3 K × 2 XLM × 0.30 at K's minute)", cands)
	}
}

// TestXLMUSDAnchor_NoFreshAnchorUnpricesOnlyTheXLMArm: with XLM/USD last
// printed 2 h ago, native and an XLM-quoted asset are unpriced while a
// direct-USD asset keeps its price (so the zero-row prune guard is not what
// saves it). On main all three priced, the XLM ones at the 2-hour-old rate.
func TestXLMUSDAnchor_NoFreshAnchorUnpricesOnlyTheXLMArm(t *testing.T) {
	f := newAnchorFixture(t)
	const unit = 10_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	f.trade("sdex", 2*time.Hour, native, usdc, 10*unit, 4*unit, "4")
	D, X := f.contract("D"), f.contract("X")
	f.trade("soroswap", 10*time.Minute, D, usdc, 1*unit, 2*unit, "")
	f.trade("sdex", 5*time.Minute, X, native, 1*unit, 2*unit, "")
	f.refresh()

	if price, _, ok := f.snapshot(D.String()); !ok || !ratEq(t, price.String, "2") {
		t.Errorf("direct asset price_usd = %v (ok=%v), want 2", price.String, ok)
	}
	for _, id := range []string{"native", X.String()} {
		if price, _, ok := f.snapshot(id); ok {
			t.Errorf("rollup priced %s at %s with no XLM/USD within the hour", id, price.String)
		}
	}
	nrow, err := f.store.GetNativeAssetRow(f.ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if nrow.PriceUSD != nil {
		t.Errorf("native row price_usd = %s, want unpriced", *nrow.PriceUSD)
	}
}
