//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSushiswapV3PositionEvents_RoundTrip executes the 0203 schema through
// real TimescaleDB: one row per action, a collect with NULL liquidity, the
// CHECK that refuses a mint without liquidity, u128 amounts above 2^64
// surviving NUMERIC, and the idempotent re-insert.
func TestSushiswapV3PositionEvents_RoundTrip(t *testing.T) {
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
		pool   = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
		owner  = "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4"
		sender = "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF"
		token0 = "native"
		token1 = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		// 2^128 - 1: the widest u128 a pool event can carry.
		maxU128 = "340282366920938463463374607431768211455"
	)
	txHash, _ := hex.DecodeString(strings.Repeat("ab", 32))
	t0 := time.Date(2026, 3, 3, 20, 52, 22, 0, time.UTC)

	base := timescale.SushiswapV3PositionEvent{
		Pool: pool, Ledger: 61_487_383, LedgerCloseTime: t0, TxHash: txHash,
		OpIndex: 0, EventIndex: 3, Owner: owner,
		Token0: token0, Token1: token1,
		TickLower: -18780, TickUpper: -15180,
		Amount0: "100000000", Amount1: "18231333",
	}

	mint := base
	mint.Action, mint.Sender, mint.Liquidity = "mint", sender, "496117982"
	burn := base
	burn.EventIndex, burn.Action, burn.Liquidity = 4, "burn", maxU128
	burn.Amount0 = maxU128
	collect := base
	collect.EventIndex, collect.Action = 5, "collect"

	for _, e := range []timescale.SushiswapV3PositionEvent{mint, burn, collect, mint} {
		if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.Action, err)
		}
	}

	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sushiswap_v3_position_events WHERE pool = $1`, pool).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("rows = %d, want 3 (mint re-insert must be idempotent)", n)
	}

	var amount0, liq string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT amount_0::text, liquidity::text FROM sushiswap_v3_position_events
		WHERE pool = $1 AND action = 'burn'`, pool).Scan(&amount0, &liq); err != nil {
		t.Fatalf("read burn: %v", err)
	}
	if amount0 != maxU128 || liq != maxU128 {
		t.Errorf("burn amount0/liquidity = %s/%s, want %s — u128 lost precision", amount0, liq, maxU128)
	}

	var nullLiq, senderNull bool
	if err := store.DB().QueryRowContext(ctx, `
		SELECT liquidity IS NULL, sender IS NULL FROM sushiswap_v3_position_events
		WHERE pool = $1 AND action = 'collect'`, pool).Scan(&nullLiq, &senderNull); err != nil {
		t.Fatalf("read collect: %v", err)
	}
	if !nullLiq || !senderNull {
		t.Errorf("collect liquidity NULL = %v, sender NULL = %v, want both true", nullLiq, senderNull)
	}

	// The CHECK is the backstop behind the Go validator: raw SQL must be
	// refused too.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sushiswap_v3_position_events (
			ledger_close_time, pool, ledger, tx_hash, op_index, event_index,
			action, owner, token_0, token_1, tick_lower, tick_upper,
			liquidity, amount_0, amount_1)
		VALUES ($1, $2, 1, $3, 0, 9, 'mint', $4, $5, $6, 0, 1, NULL, 1, 1)`,
		t0, pool, txHash, owner, token0, token1)
	if err == nil {
		t.Error("mint with NULL liquidity was accepted; CHECK constraint missing")
	}
}

// TestSushiswapV3PositionEvents_GenerationGuard: a corrected gen-1 re-derive
// replaces the live gen-0 row, and a later gen-0 replay of the same event
// cannot revert it.
func TestSushiswapV3PositionEvents_GenerationGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	txHash, _ := hex.DecodeString(strings.Repeat("cd", 32))
	e := timescale.SushiswapV3PositionEvent{
		Pool:   "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3",
		Ledger: 61_487_400, LedgerCloseTime: time.Date(2026, 3, 3, 21, 0, 0, 0, time.UTC),
		TxHash: txHash, EventIndex: 1, Action: "mint", Liquidity: "10",
		Owner:  "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4",
		Sender: "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF",
		Token0: "native", Token1: "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		TickLower: -10, TickUpper: 10, Amount0: "111", Amount1: "222",
	}
	read := func() (string, int64) {
		var a string
		var g int64
		if err := store.DB().QueryRowContext(ctx, `
			SELECT amount_0::text, derive_generation FROM sushiswap_v3_position_events
			WHERE pool = $1 AND ledger = $2`, e.Pool, e.Ledger).Scan(&a, &g); err != nil {
			t.Fatalf("read: %v", err)
		}
		return a, g
	}

	if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
		t.Fatalf("gen0 insert: %v", err)
	}

	store.SetDeriveGeneration(1)
	corrected := e
	corrected.Amount0 = "999"
	if err := store.InsertSushiswapV3PositionEvent(ctx, corrected); err != nil {
		t.Fatalf("gen1 insert: %v", err)
	}
	if a, g := read(); a != "999" || g != 1 {
		t.Fatalf("after gen1 write amount_0/gen = %s/%d, want 999/1", a, g)
	}

	store.SetDeriveGeneration(0)
	if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
		t.Fatalf("gen0 replay: %v", err)
	}
	if a, g := read(); a != "999" || g != 1 {
		t.Errorf("after gen0 replay amount_0/gen = %s/%d, want 999/1 — gen0 reverted the corrected row", a, g)
	}
}
