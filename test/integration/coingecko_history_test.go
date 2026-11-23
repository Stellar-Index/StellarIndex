//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A backfilled point stamped at exactly a live row's ts must land beside it, never upsert over it;
// a re-run must add nothing; and the live-coverage probe must see only the live row.
func TestCoinGeckoHistory_IdempotentAndNeverOverwritesLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, _ := canonical.NewPair(xlm, usd)
	from := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	to := from.Add(3 * time.Hour)
	shared := from.Add(time.Hour)

	live := canonical.OracleUpdate{
		Source: coingecko.SourceName, TxHash: scale.SyntheticTxHash("COINGECKO-XLM-USD-live-row"),
		Timestamp: shared, Asset: xlm, Quote: usd, Price: canonical.NewAmount(big.NewInt(17_000_000)), Decimals: 8,
	}
	if err := store.InsertOracleUpdate(ctx, live); err != nil {
		t.Fatalf("insert live: %v", err)
	}

	// Synthetic series: one point on the live row's instant at a different price, one in the hole.
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	body := `{"prices":[[` + ms(shared) + `,0.99],[` + ms(from.Add(2*time.Hour)) + `,0.5]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	p := &coingecko.Poller{Endpoint: srv.URL, TickerToID: map[string]string{"XLM": "stellar"}}

	sqlDB := openDB(t, dsn)
	count := func() (n int, livePrice string) {
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM oracle_updates WHERE source = 'coingecko'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := sqlDB.QueryRowContext(ctx, `SELECT price::text FROM oracle_updates WHERE tx_hash = $1`, live.TxHash).Scan(&livePrice); err != nil {
			t.Fatal(err)
		}
		return n, livePrice
	}
	for run := 1; run <= 2; run++ {
		updates, err := p.History(ctx, []canonical.Pair{pair}, from, to)
		if err != nil || len(updates) != 2 {
			t.Fatalf("run %d: History = %d updates, %v", run, len(updates), err)
		}
		for _, u := range updates {
			if err := store.InsertOracleUpdate(ctx, u); err != nil {
				t.Fatalf("run %d insert: %v", run, err)
			}
		}
		if n, lp := count(); n != 3 || lp != "17000000" {
			t.Fatalf("after run %d: %d rows, live price %s; want 3 rows and the live price untouched", run, n, lp)
		}
	}

	stamps, err := store.OracleStampsInWindow(ctx, coingecko.SourceName, from, to, coingecko.HistoryTxHashPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(stamps) != 1 || !stamps[0].TS.Equal(shared) || stamps[0].Asset != xlm.String() {
		t.Fatalf("live probe = %+v, want only the live row", stamps)
	}
}
