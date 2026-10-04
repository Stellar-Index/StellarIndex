package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestTick_ExcludedSources_DropsStoredTrades(t *testing.T) {
	now := time.Now()
	store := &mockStore{
		trades: []canonical.Trade{
			buildTradeFrom(t, "binance",
				big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-2*time.Minute)),
			buildTradeFrom(t, "kraken",
				big.NewInt(100_000_000), big.NewInt(1_000_000_000), now.Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)
	orch := New(store, rdb, Config{
		Pairs:           []canonical.Pair{xlmUsdtPair(t)},
		Windows:         []time.Duration{5 * time.Minute},
		ExcludedSources: []string{"kraken"},
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	val, err := mr.Get("vwap:" + xlm.String() + ":" + usdt.String() + ":300")
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if val[:4] != "0.20" {
		t.Errorf("VWAP = %q, want prefix 0.20 (kraken excluded)", val)
	}
}
