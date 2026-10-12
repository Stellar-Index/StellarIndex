package divergence_test

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// TestStablecoinDepeg_DivergenceWorkerFires: the aggregator conceals stablecoin-to-fiat drift (ADR-0026), so divergence is the safety net. XLM/USDC at 0.10 is rewritten to XLM/USD with amounts untouched;
// when USDC depegs to $0.95 the references see ~0.095 and RefreshPair(our=0.10) must fire WarningFired.
func TestStablecoinDepeg_DivergenceWorkerFires(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdc, _ := canonical.NewCryptoAsset("USDC")
	xlmUSDCPair, err := canonical.NewPair(xlm, usdc)
	if err != nil {
		t.Fatalf("build XLM/USDC pair: %v", err)
	}

	// Step 1 — simulate one Soroswap trade: 1 XLM ↔ 0.10 USDC.
	trade := canonical.Trade{
		Source:      "soroswap",
		Ledger:      52_000_000,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     1,
		Pair:        xlmUSDCPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000)), // 1 XLM @ 7-decimals
		QuoteAmount: canonical.NewAmount(big.NewInt(1_000_000)),  // 0.10 USDC @ 7-decimals
	}

	// Step 2: apply the aggregator's stablecoin proxy. ProxyTrade rewrites the pair to XLM/fiat:USD and leaves
	// amounts untouched (the policy assumes USDC ~ USD), which is the concealment path ADR-0026 documents.
	proxied, ok := aggregate.ProxyTrade(trade)
	if !ok {
		t.Fatalf("ProxyTrade should rewrite XLM/USDC → XLM/fiat:USD")
	}
	if proxied.Pair.Quote.Type != canonical.AssetFiat || proxied.Pair.Quote.Code != "USD" {
		t.Fatalf("proxied quote = %+v, want fiat:USD", proxied.Pair.Quote)
	}

	// Step 3: what our aggregator would publish. With one trade the VWAP is just the trade price, so the orchestrator
	// is not needed. Amounts are 7-decimal stroops; price is 0.10.
	const ourPrice = 0.10

	// Step 4 — the actual market: USDC depegged to 0.95 USD this
	// hour, so the true XLM/USD is 0.10 × 0.95 = 0.0950. Three
	// independent references see that.
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 0.0950},
		&stubReference{name: "chainlink", price: 0.0950},
		&stubReference{name: "reflector", price: 0.0950},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            2.5, // depeg-grade threshold (matches r1 prod)
		MinSourcesForWarning: 2,
		WarningPersistence:   -1, // isolate the depeg-detection math from the persistence debounce
	})

	// Step 5 — pretend the aggregator just refreshed the pair.
	// The divergence worker compares our (concealed-depeg) price
	// against the references and writes the result to Redis.
	if err := svc.RefreshPair(context.Background(), proxied.Pair, ourPrice, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	// Step 6 — assert the safety net fired. Our 0.10 vs reference
	// 0.0950 is ~5.26% delta, above the 2.5% threshold; with 3
	// agreeing references and MinSources=2 the warning must fire.
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(proxied.Pair).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get %s: %v", cachekeys.Divergence(proxied.Pair), err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal CachedResult: %v", err)
	}
	if !cached.WarningFired {
		t.Errorf("WarningFired = false on simulated USDC depeg "+
			"(our=%g vs ref=%g, delta ~5.3%%); the divergence safety "+
			"net is supposed to compensate for stablecoin late-binding "+
			"concealment per ADR-0026 + F-1230",
			ourPrice, 0.0950)
	}
	if cached.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3 (all stub references should report)", cached.SuccessCount)
	}
}

// TestStablecoinPegHolds_DivergenceWorkerStaysQuiet is the negative case: with the peg held, our concealed-fiat price matches the references and the worker must NOT fire.
func TestStablecoinPegHolds_DivergenceWorkerStaysQuiet(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdc, _ := canonical.NewCryptoAsset("USDC")
	xlmUSDCPair, _ := canonical.NewPair(xlm, usdc)

	trade := canonical.Trade{
		Source:      "soroswap",
		Ledger:      52_000_000,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Pair:        xlmUSDCPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(1_000_000)),
	}
	proxied, ok := aggregate.ProxyTrade(trade)
	if !ok {
		t.Fatalf("ProxyTrade should rewrite")
	}

	const ourPrice = 0.10
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 0.10},
		&stubReference{name: "chainlink", price: 0.10},
		&stubReference{name: "reflector", price: 0.10},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            2.5,
		MinSourcesForWarning: 2,
	})

	// Over-firing on the peg-holds steady state would trip refreshQuiet.
	refreshQuiet(t, svc, rdb, proxied.Pair, ourPrice, time.Now())
}
