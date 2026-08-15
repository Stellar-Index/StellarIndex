//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestResumeStalled_ArmsUSDVolumeResolution is the CWR-1 (audit-2026-08-14)
// proven-red test. The resume-stalled recovery tool marches stalled cursors
// through the SAME runBackfillChunk trade-write path as the main `backfill`
// subcommand, so the store it opens must be armed for a trade-writing
// re-derive — a POSITIVE derive generation (so a corrected re-derive wins the
// writers' ON CONFLICT guard) AND the USD-volume resolvers (so on-chain DEX
// trades resolve a real usd_volume instead of NULL). Before the fix
// resume-stalled opened the store raw and skipped this wiring, so a re-derived
// on-chain DEX trade landed with usd_volume=NULL at gen 0 and the A-CRIT-1
// reDeriveNullVolumeGuard was inert (it only fires once the generation is
// positive).
//
// This exercises the exact seam resume-stalled now runs after timescale.Open
// — [ingest.ArmTradeWriteStore] — then inserts an on-chain DEX (sdex) trade
// quoted in a USD-pegged classic asset and asserts the tier-1 usd_volume lands
// non-NULL with the correct value.
//
// To reproduce the red state: revert the body of [ingest.ArmTradeWriteStore]
// to `return nil` (the pre-fix behaviour: no SetDeriveGeneration, no
// InstallUSDVolumeResolution). InsertTrade then computes usd_volume=NULL and
// the non-NULL assertion goes red.
func TestResumeStalled_ArmsUSDVolumeResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)

	// Config exactly as resume-stalled receives it from
	// parseResumeStalledFlags: USDC declared as a USD-pegged classic asset
	// so tier-1 prices a DEX trade quoted in it with no FX/CAGG lookup.
	var cfg config.Config
	cfg.Trades.USDPeggedClassicAssets = []string{"USDC-" + usdcIssuer}

	// The wiring under test — the single call resume-stalled now makes
	// right after timescale.Open.
	if err := ingest.ArmTradeWriteStore(store, cfg); err != nil {
		t.Fatalf("ArmTradeWriteStore: %v", err)
	}

	// An on-chain DEX trade: 100 XLM for 108.5 USDC (classic 7-dec).
	// tier-1 usd_volume = 1_085_000_000 / 1e7 = 108.5.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	trade := mkIntegrationTrade("sdex", 1, ts, xlmUSDC, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	var uv sql.NullString
	const q = `SELECT usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
	if err := store.DB().QueryRowContext(ctx, q, "sdex", trade.Ledger).Scan(&uv); err != nil {
		t.Fatalf("read usd_volume: %v", err)
	}
	if !uv.Valid {
		t.Fatal("usd_volume = NULL — resume-stalled wrote a DEX trade without " +
			"USD-volume resolution installed (CWR-1 regression)")
	}
	if uv.String != "108.50000000" && uv.String != "108.5" {
		t.Errorf("usd_volume = %q, want 108.5 (tier-1 quote = 1_085_000_000 / 1e7)", uv.String)
	}
}
