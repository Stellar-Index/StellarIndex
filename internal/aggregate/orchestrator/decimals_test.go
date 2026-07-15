package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeDecimalsLookup is a trivial aggregate.DecimalsLookup for tests —
// mirrors internal/api/v1's NonstandardDecimalsCache.Lookup shape without
// pulling in a storage dependency.
type fakeDecimalsLookup map[string]int

func (f fakeDecimalsLookup) Lookup(assetID string) (int, bool) {
	d, ok := f[assetID]
	return d, ok
}

// TestTick_DecimalsLookup_NormalizesNonstandardLeg proves the forward-
// normalization wiring: a pair whose base leg is a confirmed 18-decimal
// Soroban token (the ROADMAP's P2 landmine) gets its published VWAP scaled
// by 10^(18-7), not served at the raw stroop-scale ratio.
func TestTick_DecimalsLookup_NormalizesNonstandardLeg(t *testing.T) {
	// Real on-chain contract id (the founding CS-026/2026-07-08 decimals
	// incident, per docs/operations/runbooks/dex-nonstandard-decimals.md)
	// — reused here purely as a valid, memorable C-strkey fixture.
	token, err := canonical.NewSorobanAsset("CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(token, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// base_amount = 2.5 * 10^18 (18dp), quote_amount = 1.242 * 10^7 (7dp
	// USDC) → true price 0.4968 USDC/token. The raw (unadjusted) ratio
	// would be 0.4968 / 10^11.
	baseAmount := new(big.Int)
	baseAmount.SetString("2500000000000000000", 10)
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().Add(-time.Minute),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmount),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420_000)),
	}

	store := &mockStore{trades: []canonical.Trade{trade}}
	rdb, mr := newTestRedis(t)

	orch := New(store, rdb, Config{
		Pairs:          []canonical.Pair{pair},
		Windows:        []time.Duration{5 * time.Minute},
		DecimalsLookup: fakeDecimalsLookup{token.String(): 18},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := "vwap:" + token.String() + ":" + usdc.String() + ":300"
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get %q: %v", key, err)
	}
	if val[:6] != "0.4968" {
		t.Errorf("published VWAP = %q, want prefix 0.4968 (normalized) not a 10^-11-scale raw ratio", val)
	}
}

// TestTick_DecimalsLookup_NilIsByteIdenticalNoOp proves the default (nil
// DecimalsLookup, matching every deployment/test that predates this field)
// produces the exact same published VWAP as before this change — the
// regression-safety half of constraint #5 (7dp assets untouched).
func TestTick_DecimalsLookup_NilIsByteIdenticalNoOp(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now().Add(-2*time.Minute)),
			buildTrade(t, big.NewInt(20_000_000_000), big.NewInt(3_518_000_000), time.Now().Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)

	// DecimalsLookup deliberately left unset (nil) — same Config shape
	// every pre-existing orchestrator test uses.
	orch := New(store, rdb, Config{
		Pairs:   []canonical.Pair{xlmUsdtPair(t)},
		Windows: []time.Duration{5 * time.Minute},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	key := "vwap:" + xlm.String() + ":" + usdt.String() + ":300"
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get %q: %v", key, err)
	}
	if val[:5] != "0.175" {
		t.Errorf("stored VWAP = %q, want prefix 0.175 (unchanged from pre-normalization behaviour)", val)
	}
}
