//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A same-generation replay of an unchanged trade must not rewrite the row: on a
// compressed chunk that UPDATE decompresses it and bloats the chunk's heap.
func TestTradeUpsert_EqualGenerationNoOpReplayDoesNotWrite(t *testing.T) {
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
	xlm, _ := c.NewCryptoAsset("XLM")
	pair, _ := c.NewPair(xlm, usd)
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	read := func(ledger uint32) (xmin, quote, usdVol string) {
		const q = `SELECT xmin::text, quote_amount::text, coalesce(usd_volume::text, '')
		             FROM trades WHERE source = 'binance' AND ledger = $1`
		if err := store.DB().QueryRowContext(ctx, q, ledger).Scan(&xmin, &quote, &usdVol); err != nil {
			t.Fatalf("read trade %d: %v", ledger, err)
		}
		return xmin, quote, usdVol
	}

	writers := map[string]func(c.Trade) error{
		"InsertTrade":       func(tr c.Trade) error { return store.InsertTrade(ctx, tr) },
		"BatchInsertTrades": func(tr c.Trade) error { return store.BatchInsertTrades(ctx, []c.Trade{tr}) },
	}
	nonce := 20
	for name, write := range writers {
		nonce++
		t.Run(name, func(t *testing.T) {
			store.SetDeriveGeneration(3)
			tr := mkIntegrationTrade("binance", nonce, ts, pair, 100_000_000, 12_000_000)
			if err := write(tr); err != nil {
				t.Fatalf("first write: %v", err)
			}
			x0, _, uv0 := read(tr.Ledger)
			if uv0 == "" {
				t.Fatal("fixture must populate usd_volume")
			}

			if err := write(tr); err != nil {
				t.Fatalf("identical replay: %v", err)
			}
			if x1, _, _ := read(tr.Ledger); x1 != x0 {
				t.Errorf("identical same-generation replay rewrote the row (xmin %s -> %s)", x0, x1)
			}

			if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('trades') c`); err != nil {
				t.Fatalf("compress_chunk: %v", err)
			}
			// xmin is unreadable through transparent decompression.
			decompress := func() {
				_, _ = store.DB().ExecContext(ctx, `SELECT decompress_chunk(c, if_compressed => true) FROM show_chunks('trades') c`)
			}
			t.Cleanup(decompress)
			if err := write(tr); err != nil {
				t.Fatalf("identical replay into compressed chunk: %v", err)
			}
			var partial int
			if err := store.DB().QueryRowContext(ctx, `
				SELECT count(*) FROM _timescaledb_catalog.chunk ch
				  JOIN _timescaledb_catalog.hypertable h ON h.id = ch.hypertable_id
				 WHERE h.table_name = 'trades' AND ch.status & 8 = 8`).Scan(&partial); err != nil {
				t.Fatalf("chunk status: %v", err)
			}
			if partial != 0 {
				t.Errorf("identical replay left %d trades chunk(s) partially decompressed", partial)
			}
			decompress()

			changed := mkIntegrationTrade("binance", nonce, ts, pair, 100_000_000, 99_000_000)
			if err := write(changed); err != nil {
				t.Fatalf("changed replay: %v", err)
			}
			if _, q, _ := read(tr.Ledger); q != "99000000" {
				t.Errorf("same-generation correction: quote_amount = %s, want 99000000", q)
			}

			// Same amounts, a stored usd_volume the resolver now prices differently: still a write.
			if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 0.5 WHERE source = 'binance' AND ledger = $1`, tr.Ledger); err != nil {
				t.Fatalf("seed stale usd_volume: %v", err)
			}
			if err := write(changed); err != nil {
				t.Fatalf("usd_volume replay: %v", err)
			}
			if _, _, uv := read(tr.Ledger); uv != "0.99000000" && uv != "0.99" {
				t.Errorf("same-generation usd_volume correction: usd_volume = %s, want 0.99", uv)
			}
		})
	}

	t.Run("NullUSDVolumeKeepsStored", func(t *testing.T) {
		base, _ := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
		quote, _ := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
		unpriced, _ := c.NewPair(base, quote)
		if err := timescale.InstallUSDVolumeResolution(store, nil, nil, nil); err != nil {
			t.Fatalf("install usd resolution: %v", err)
		}
		store.SetDeriveGeneration(3)
		tr := mkIntegrationTrade("binance", 40, ts, unpriced, 100_000_000, 12_000_000)
		if err := store.BatchInsertTrades(ctx, []c.Trade{tr}); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 7 WHERE source = 'binance' AND ledger = $1`, tr.Ledger); err != nil {
			t.Fatalf("seed usd_volume: %v", err)
		}
		x0, _, _ := read(tr.Ledger)
		if err := store.BatchInsertTrades(ctx, []c.Trade{tr}); err != nil {
			t.Fatalf("replay: %v", err)
		}
		x1, _, uv := read(tr.Ledger)
		if uv != "7" {
			t.Errorf("an unpriced same-generation replay changed usd_volume to %q, want the stored 7", uv)
		}
		if x1 != x0 {
			t.Errorf("an unpriced same-generation replay rewrote the row (xmin %s -> %s)", x0, x1)
		}
	})
}
