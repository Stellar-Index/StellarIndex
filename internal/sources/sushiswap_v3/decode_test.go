package sushiswap_v3

import (
	"errors"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func mustAsset(t *testing.T, contractID string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(contractID)
	if err != nil {
		t.Fatalf("NewSorobanAsset(%s): %v", contractID, err)
	}
	return a
}

// TestGoldenDecodeSwapFields_RealLakeBytes pins every decoded field of a
// real swap body — both post-upgrade directions, the pre-upgrade WASM, and
// the one degenerate event in the protocol's history.
func TestGoldenDecodeSwapFields_RealLakeBytes(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		amount0      string
		amount1      string
		liquidity    string
		sqrtPriceX96 string
		tick         int32
		recipient    string
	}{
		{
			name:         "post-upgrade, trader sells token0",
			body:         goldenSwapSellToken0,
			amount0:      "11199844994",
			amount1:      "-2000791309",
			liquidity:    "23069681065872",
			sqrtPriceX96: "33533771773457200059990987705",
			tick:         -17197,
			recipient:    "GCBYPF2OVPSZ7NJSXIOINCBMOSMY7I6KOWHPZV36OH4OH5R2A63DKUKY",
		},
		{
			name:         "post-upgrade, trader sells token1",
			body:         goldenSwapSellToken1,
			amount0:      "-10008570442",
			amount1:      "1742450000",
			liquidity:    "23088223325684",
			sqrtPriceX96: "33011139624406585858684805867",
			tick:         -17511,
			recipient:    "GASB76SYSIS6ERWG2CNZAFUBYXJPBQEE6VADWJTGOI3BDDCBNINPCTTS",
		},
		{
			name:         "pre-upgrade WASM carries the same seven fields",
			body:         goldenSwapPreUpgrade,
			amount0:      "95945065",
			amount1:      "-16171423",
			liquidity:    "496117981",
			sqrtPriceX96: "31310103761388245070389986772",
			tick:         -18569,
			recipient:    "GDCRZPZYBZ24RHRO3WBPJGFDL7NDFKUQBS3ZDB6YGBJB3TGKMFYBQ3LD",
		},
		{
			name:         "non-directional dust swap",
			body:         goldenSwapNonDirectional,
			amount0:      "1",
			amount1:      "0",
			liquidity:    "92864097278004",
			sqrtPriceX96: "30560552761276594813291032504",
			tick:         -19054,
			recipient:    "CC6QAV7JEG5MYRSPO5Z65E5G2M4ZB64BEG2ZXIZXL55TQT35JDI2LC6K",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sdkDecodeSwapFields(tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Amount0.String() != tc.amount0 {
				t.Errorf("amount0 = %s, want %s", got.Amount0, tc.amount0)
			}
			if got.Amount1.String() != tc.amount1 {
				t.Errorf("amount1 = %s, want %s", got.Amount1, tc.amount1)
			}
			if got.Liquidity.String() != tc.liquidity {
				t.Errorf("liquidity = %s, want %s", got.Liquidity, tc.liquidity)
			}
			if got.SqrtPriceX96.String() != tc.sqrtPriceX96 {
				t.Errorf("sqrt_price_x96 = %s, want %s", got.SqrtPriceX96, tc.sqrtPriceX96)
			}
			if got.Tick != tc.tick {
				t.Errorf("tick = %d, want %d", got.Tick, tc.tick)
			}
			if got.Recipient != tc.recipient {
				t.Errorf("recipient = %s, want %s", got.Recipient, tc.recipient)
			}
		})
	}
}

// TestGoldenDecodeSwapFields_SqrtPriceKeepsFullWidth guards the one field
// that does not fit a narrower path. sqrt_price_x96 is a U256 Q64.96
// value; decoding it as an i128 low word or a float would truncate it
// silently.
func TestGoldenDecodeSwapFields_SqrtPriceKeepsFullWidth(t *testing.T) {
	got, err := sdkDecodeSwapFields(goldenSwapSellToken0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bits := got.SqrtPriceX96.BigInt().BitLen(); bits <= 64 {
		t.Fatalf("sqrt_price_x96 %s has bit length %d — a real Q64.96 price exceeds 64 bits",
			got.SqrtPriceX96, bits)
	}
}

// TestGoldenDecodePoolCreated_RealLakeBytes pins the factory creation body
// — the only on-chain statement of a pool's token identities.
func TestGoldenDecodePoolCreated_RealLakeBytes(t *testing.T) {
	got, err := sdkDecodePoolCreated(goldenPoolCreated)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Pool != mainPool {
		t.Errorf("pool = %s, want %s", got.Pool, mainPool)
	}
	if got.Token0.String() != tokenXLM {
		t.Errorf("token0 = %s, want %s", got.Token0, tokenXLM)
	}
	if got.Token1.String() != tokenUSDC {
		t.Errorf("token1 = %s, want %s", got.Token1, tokenUSDC)
	}
	if got.Token0.Type != canonical.AssetSoroban || got.Token1.Type != canonical.AssetSoroban {
		t.Errorf("pool tokens must be contract-identified assets, got %v / %v",
			got.Token0.Type, got.Token1.Type)
	}
	if got.FeePips != 3000 {
		t.Errorf("fee = %d, want 3000", got.FeePips)
	}
	if got.TickSpacing != 60 {
		t.Errorf("tick_spacing = %d, want 60", got.TickSpacing)
	}
}

// TestGoldenDecodePoolCreated_AgreesWithCuratedTable proves the in-code
// curated table was derived from the chain rather than typed by hand.
func TestGoldenDecodePoolCreated_AgreesWithCuratedTable(t *testing.T) {
	got, err := sdkDecodePoolCreated(goldenPoolCreated)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	meta, ok := MainnetPools[got.Pool]
	if !ok {
		t.Fatalf("pool %s created on-chain but absent from MainnetPools", got.Pool)
	}
	if meta.Token0 != got.Token0.String() || meta.Token1 != got.Token1.String() {
		t.Errorf("curated tokens (%s, %s) differ from on-chain (%s, %s)",
			meta.Token0, meta.Token1, got.Token0, got.Token1)
	}
	if meta.FeePips != got.FeePips || meta.TickSpacing != got.TickSpacing {
		t.Errorf("curated (fee %d, spacing %d) differs from on-chain (fee %d, spacing %d)",
			meta.FeePips, meta.TickSpacing, got.FeePips, got.TickSpacing)
	}
}

// TestDecodeSwap_DirectionAndExactAmounts is the money test: a decoded
// swap becomes the trade it should, with the base leg the token the trader
// SOLD and the quote leg the magnitude of what they BOUGHT, both exact and
// both identified by contract, never by code.
func TestDecodeSwap_DirectionAndExactAmounts(t *testing.T) {
	tok0 := mustAsset(t, tokenXLM)
	tok1 := mustAsset(t, tokenUSDC)
	closedAt := time.Date(2026, 8, 30, 22, 22, 35, 0, time.UTC)

	tests := []struct {
		name      string
		body      string
		wantBase  string
		wantQuote string
		baseAmt   string
		quoteAmt  string
	}{
		{
			// amount0 = +11199844994 (the pool received XLM),
			// amount1 = -2000791309  (the pool paid out USDC).
			name:      "positive token0 delta means the trader sold token0",
			body:      goldenSwapSellToken0,
			wantBase:  tokenXLM,
			wantQuote: tokenUSDC,
			baseAmt:   "11199844994",
			quoteAmt:  "2000791309",
		},
		{
			// amount0 = -10008570442 (the pool paid out XLM),
			// amount1 = +1742450000  (the pool received USDC).
			name:      "positive token1 delta means the trader sold token1",
			body:      goldenSwapSellToken1,
			wantBase:  tokenUSDC,
			wantQuote: tokenXLM,
			baseAmt:   "1742450000",
			quoteAmt:  "10008570442",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fields, err := sdkDecodeSwapFields(tc.body)
			if err != nil {
				t.Fatalf("decode body: %v", err)
			}
			trade, err := decodeSwap(fields, 64_200_014, swapTxHash, 0, 2, closedAt, tok0, tok1)
			if err != nil {
				t.Fatalf("decodeSwap: %v", err)
			}
			if trade.Pair.Base.String() != tc.wantBase {
				t.Errorf("base = %s, want %s", trade.Pair.Base, tc.wantBase)
			}
			if trade.Pair.Quote.String() != tc.wantQuote {
				t.Errorf("quote = %s, want %s", trade.Pair.Quote, tc.wantQuote)
			}
			if trade.BaseAmount.String() != tc.baseAmt {
				t.Errorf("base_amount = %s, want %s", trade.BaseAmount, tc.baseAmt)
			}
			if trade.QuoteAmount.String() != tc.quoteAmt {
				t.Errorf("quote_amount = %s, want %s", trade.QuoteAmount, tc.quoteAmt)
			}
			if trade.Source != SourceName {
				t.Errorf("source = %s, want %s", trade.Source, SourceName)
			}
			if err := trade.Validate(); err != nil {
				t.Errorf("trade.Validate: %v", err)
			}
		})
	}
}

// TestDecodeSwap_AmountsAreExactNotRounded proves no lossy path is taken:
// an 11-digit i128 magnitude survives byte for byte.
func TestDecodeSwap_AmountsAreExactNotRounded(t *testing.T) {
	fields, err := sdkDecodeSwapFields(goldenSwapSellToken0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want, ok := new(big.Int).SetString("11199844994", 10)
	if !ok {
		t.Fatal("bad test constant")
	}
	if fields.Amount0.BigInt().Cmp(want) != 0 {
		t.Fatalf("amount0 = %s, want %s", fields.Amount0, want)
	}
}

// TestDecodeSwap_RefusesNonDirectional pins the one real degenerate event
// in the protocol's history. Reporting it as a trade would put a
// zero-quote ratio into every price aggregate reading this source.
func TestDecodeSwap_RefusesNonDirectional(t *testing.T) {
	fields, err := sdkDecodeSwapFields(goldenSwapNonDirectional)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_, err = decodeSwap(fields, 62_712_211, swapTxHash, 0, 23,
		time.Date(2026, 5, 24, 9, 10, 8, 0, time.UTC),
		mustAsset(t, tokenXLM), mustAsset(t, tokenUSDC))
	if !errors.Is(err, ErrNonDirectionalSwap) {
		t.Fatalf("err = %v, want ErrNonDirectionalSwap", err)
	}
}

// TestDecodeSwap_RefusesSameSignDeltas covers the shapes the chain has not
// produced but the wire type permits — both legs positive, both negative,
// or a zero on either side. None has a derivable direction.
func TestDecodeSwap_RefusesSameSignDeltas(t *testing.T) {
	tok0 := mustAsset(t, tokenXLM)
	tok1 := mustAsset(t, tokenUSDC)
	amt := func(s string) canonical.Amount {
		a, err := canonical.FromString(s)
		if err != nil {
			t.Fatalf("FromString(%s): %v", s, err)
		}
		return a
	}
	for _, tc := range []struct{ name, a0, a1 string }{
		{"both positive", "10", "20"},
		{"both negative", "-10", "-20"},
		{"both zero", "0", "0"},
		{"zero base with negative quote", "0", "-20"},
		{"positive base with zero quote", "20", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSwap(
				SwapFields{Amount0: amt(tc.a0), Amount1: amt(tc.a1)},
				64_200_014, swapTxHash, 0, 2, time.Unix(1, 0).UTC(), tok0, tok1)
			if !errors.Is(err, ErrNonDirectionalSwap) {
				t.Fatalf("err = %v, want ErrNonDirectionalSwap", err)
			}
		})
	}
}

// TestDecodeSwap_FansOutOpIndexByEventIndex proves two swaps in one
// operation get distinct trade identities. Without the fan-out they share
// (source, ledger, tx_hash, op_index, ts) and the second is silently
// dropped by the writer's ON CONFLICT. The lake shows the shape is real
// here: tx f6fb00ef…c308 carries swaps at event indices 2 and 14 of one
// operation.
func TestDecodeSwap_FansOutOpIndexByEventIndex(t *testing.T) {
	fields, err := sdkDecodeSwapFields(goldenSwapSellToken0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	tok0, tok1 := mustAsset(t, tokenXLM), mustAsset(t, tokenUSDC)
	closedAt := time.Unix(1, 0).UTC()
	first, err := decodeSwap(fields, 64_200_014, swapTxHash, 0, 2, closedAt, tok0, tok1)
	if err != nil {
		t.Fatalf("decodeSwap first: %v", err)
	}
	second, err := decodeSwap(fields, 64_200_014, swapTxHash, 0, 14, closedAt, tok0, tok1)
	if err != nil {
		t.Fatalf("decodeSwap second: %v", err)
	}
	if first.OpIndex == second.OpIndex {
		t.Fatalf("both swaps got op_index %d — they would collide on the trades key", first.OpIndex)
	}
	if first.ID() == second.ID() {
		t.Fatalf("both swaps got trade id %s", first.ID())
	}
}

// TestDecodeSwap_TakerIsTheOutputRecipient records the counterparty
// attribution contract: an AMM has no resting maker, so maker stays empty
// and the source is declared in timescale.AMMSignerSources.
func TestDecodeSwap_TakerIsTheOutputRecipient(t *testing.T) {
	fields, err := sdkDecodeSwapFields(goldenSwapSellToken0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	trade, err := decodeSwap(fields, 64_200_014, swapTxHash, 0, 2, time.Unix(1, 0).UTC(),
		mustAsset(t, tokenXLM), mustAsset(t, tokenUSDC))
	if err != nil {
		t.Fatalf("decodeSwap: %v", err)
	}
	if trade.Taker != "GCBYPF2OVPSZ7NJSXIOINCBMOSMY7I6KOWHPZV36OH4OH5R2A63DKUKY" {
		t.Errorf("taker = %q", trade.Taker)
	}
	if trade.Maker != "" {
		t.Errorf("maker = %q, want empty for an AMM", trade.Maker)
	}
}

// TestDecodeSwapFields_RejectsForeignBodyShapes proves the decoder fails
// rather than guessing when a gated contract emits something else.
func TestDecodeSwapFields_RejectsForeignBodyShapes(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not a map", scval.MustEncodeSymbol("swap")},
		{"map without the swap fields", goldenPoolCreated},
		{"not base64 at all", "not-a-scval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sdkDecodeSwapFields(tc.body); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// TestDecodePoolCreated_RejectsForeignBodyShapes mirrors the swap case for
// the creation body — the one that seeds the identity gate.
func TestDecodePoolCreated_RejectsForeignBodyShapes(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not a map", scval.MustEncodeSymbol("pool_created")},
		{"map without the creation fields", goldenSwapSellToken0},
		{"not base64 at all", "not-a-scval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sdkDecodePoolCreated(tc.body); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// TestClassify_OneElementSymbolTopicsOnly pins the topic shape. Every
// event in this protocol carries exactly one Symbol topic, which is why
// the topic alone can never establish identity.
func TestClassify_OneElementSymbolTopicsOnly(t *testing.T) {
	for name, topic := range map[string]string{
		EventSwap:        TopicSymbolSwap,
		EventMint:        TopicSymbolMint,
		EventBurn:        TopicSymbolBurn,
		EventCollect:     TopicSymbolCollect,
		EventInit:        TopicSymbolInit,
		EventUpgraded:    TopicSymbolUpgraded,
		EventMigrated:    TopicSymbolMigrated,
		EventPoolCreated: TopicSymbolPoolCreated,
	} {
		if got := classify(&events.Event{Topic: []string{topic}}); got != name {
			t.Errorf("classify(%s) = %q, want %q", name, got, name)
		}
	}
	if got := classify(&events.Event{Topic: []string{TopicSymbolSwap, TopicSymbolSwap}}); got != "" {
		t.Errorf("two-element topic classified as %q, want unrecognized", got)
	}
	if got := classify(&events.Event{Topic: nil}); got != "" {
		t.Errorf("empty topic classified as %q, want unrecognized", got)
	}
	if got := classify(&events.Event{Topic: []string{scval.MustEncodeSymbol("transfer")}}); got != "" {
		t.Errorf("foreign symbol classified as %q, want unrecognized", got)
	}
}

// TestMainnetPools_EveryTokenIsAValidContractAsset guards the curated
// table: an unconvertible entry would leave a gated pool with no token
// mapping, silently dropping its trades.
func TestMainnetPools_EveryTokenIsAValidContractAsset(t *testing.T) {
	for pool, meta := range MainnetPools {
		for _, tok := range []string{meta.Token0, meta.Token1} {
			asset, err := canonical.NewSorobanAsset(tok)
			if err != nil {
				t.Errorf("pool %s token %s: %v", pool, tok, err)
				continue
			}
			if asset.Type != canonical.AssetSoroban || asset.ContractID != tok {
				t.Errorf("pool %s token %s resolved to %v", pool, tok, asset)
			}
		}
		if meta.Token0 == meta.Token1 {
			t.Errorf("pool %s has the same asset on both legs", pool)
		}
		if _, err := canonical.NewSorobanAsset(pool); err != nil {
			t.Errorf("pool key %s is not a contract strkey: %v", pool, err)
		}
		if meta.CreatedAt < FactoryGenesisLedger {
			t.Errorf("pool %s created at %d, before the factory genesis %d",
				pool, meta.CreatedAt, FactoryGenesisLedger)
		}
	}
}

// TestMainnetPools_FeeTierMatchesTickSpacing pins the three canonical V3
// tiers the factory has deployed. A mismatch means the table was edited by
// hand rather than derived from the chain.
func TestMainnetPools_FeeTierMatchesTickSpacing(t *testing.T) {
	want := map[uint32]int32{500: 10, 3000: 60, 10000: 200}
	for pool, meta := range MainnetPools {
		spacing, ok := want[meta.FeePips]
		if !ok {
			t.Errorf("pool %s has unknown fee tier %d", pool, meta.FeePips)
			continue
		}
		if meta.TickSpacing != spacing {
			t.Errorf("pool %s fee %d paired with spacing %d, want %d",
				pool, meta.FeePips, meta.TickSpacing, spacing)
		}
	}
}

// TestMainnetGatedSet_IsTheWholePoolTableSorted keeps the gate seed and
// the money mapping from drifting apart, and keeps the projector's
// contract-id prefilter deterministic.
func TestMainnetGatedSet_IsTheWholePoolTableSorted(t *testing.T) {
	got := MainnetGatedSet()
	if len(got) != len(MainnetPools) {
		t.Fatalf("gated set has %d entries, pool table has %d", len(got), len(MainnetPools))
	}
	if !sort.StringsAreSorted(got) {
		t.Error("gated set is not sorted — the projector prefilter must be deterministic")
	}
	for _, id := range got {
		if _, ok := MainnetPools[id]; !ok {
			t.Errorf("gated set entry %s is not in the pool table", id)
		}
	}
	if _, isPool := MainnetPools[MainnetFactory]; isPool {
		t.Error("the factory must not also be listed as a pool")
	}
	if len(MainnetFactories) == 0 || MainnetFactories[0] != MainnetFactory {
		t.Error("MainnetFactories must contain the verified factory trust root")
	}
}
