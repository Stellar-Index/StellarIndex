//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSushiswapV3ManagerEvents_SinkRoundTrip drives decoded manager events through the production sink and reads each
// column back: u128 values above 2^63 stay exact, a field the kind does not carry is NULL, re-delivery is idempotent, and
// the table's shape CHECKs refuse a row the Go validator would.
func TestSushiswapV3ManagerEvents_SinkRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const (
		owner   = "GCK43OVWWHKJMSWHMOOBOV2YLJ3PY4ME44Q7QHTH6T24MV6XRWEYBAHS"
		maxU128 = "340282366920938463463374607431768211455"
	)
	big128, _ := new(big.Int).SetString(maxU128, 10)
	txHash := strings.Repeat("e7", 32)
	base := sushiswap_v3.ManagerEvent{
		ContractID: sushiswap_v3.MainnetPositionManager, Ledger: 65_682_016, TxHash: txHash,
		ObservedAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
	}
	mint := base
	mint.Kind, mint.EventIndex, mint.TokenID, mint.Owner = sushiswap_v3.EventMint, 1, 1877, owner
	decrease := base
	decrease.Kind, decrease.EventIndex, decrease.TokenID = sushiswap_v3.EventDecreaseLiquidity, 2, 1877
	decrease.Liquidity, decrease.HasLiquidity = canonical.NewAmount(big128), true
	decrease.Amount0, decrease.Amount1, decrease.HasAmounts = canonical.NewAmount(big128), canonical.NewAmount(big.NewInt(1073002473137)), true
	collect := base
	collect.Kind, collect.EventIndex, collect.TokenID, collect.Recipient = sushiswap_v3.EventCollect, 3, ^uint32(0), owner
	collect.Amount0, collect.Amount1, collect.HasAmounts = canonical.NewAmount(big.NewInt(0)), canonical.NewAmount(big.NewInt(5)), true

	for _, ev := range []sushiswap_v3.ManagerEvent{mint, decrease, collect, mint} {
		if err := pipeline.HandleEvent(ctx, logger, store, ev); err != nil {
			t.Fatalf("HandleEvent %s: %v", ev.Kind, err)
		}
	}

	type row struct {
		kind, tokenID, owner, recipient, liq, a0, a1 string
	}
	rows, err := store.DB().QueryContext(ctx, `
		SELECT kind, token_id::text, coalesce(owner, '-'), coalesce(recipient, '-'),
		       coalesce(liquidity::text, '-'), coalesce(amount_0::text, '-'), coalesce(amount_1::text, '-')
		FROM sushiswap_v3_manager_events WHERE tx_hash = $1 ORDER BY event_index`, mustHex(t, txHash))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.tokenID, &r.owner, &r.recipient, &r.liq, &r.a0, &r.a1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []row{
		{"mint", "1877", owner, "-", "-", "-", "-"},
		{"decrease_liquidity", "1877", "-", "-", maxU128, maxU128, "1073002473137"},
		{"collect", "4294967295", "-", owner, "-", "0", "5"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v (re-delivered mint must be idempotent)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The CHECKs back the Go validator: a mint carrying amounts is refused by raw SQL too.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sushiswap_v3_manager_events (
			ledger_close_time, manager, ledger, tx_hash, op_index, event_index, kind, token_id, owner, amount_0, amount_1)
		VALUES ($1, $2, 1, $3, 0, 9, 'mint', 1, $4, 1, 1)`,
		base.ObservedAt, base.ContractID, mustHex(t, txHash), owner)
	if err == nil {
		t.Error("mint with amounts was accepted; shape CHECK missing")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
