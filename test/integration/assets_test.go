//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetsReader exercises DistinctAssets + HasAsset against a
// real Timescale with our migrations applied. Requires the
// `integration` build tag.
func TestAssetsReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty DB → empty list, empty HasAsset.
	got, next, err := store.DistinctAssets(ctx, "", 100)
	if err != nil {
		t.Fatalf("DistinctAssets (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d assets", len(got))
	}
	if next != "" {
		t.Errorf("next cursor should be empty, got %q", next)
	}
	has, _ := store.HasAsset(ctx, c.NativeAsset())
	if has {
		t.Error("HasAsset should be false on empty db")
	}

	// Seed 3 assets via trades: XLM, USDC, PHOENIX.
	xlm := c.NativeAsset()
	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	pho, _ := c.NewSorobanAsset("CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH")

	for i, pair := range []c.Pair{
		mustPair(xlm, usdc),
		mustPair(pho, usdc),
		mustPair(xlm, pho),
	} {
		tr := c.Trade{
			Source:      "test",
			Ledger:      uint32(52_000_000 + i),
			TxHash:      hexTx(i),
			OpIndex:     0,
			Timestamp:   time.Now().UTC().Truncate(time.Second).Add(time.Duration(i) * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}

	// After seeding, the distinct union is {XLM(native), USDC-G..., CBCZ...}.
	got, next, err = store.DistinctAssets(ctx, "", 100)
	if err != nil {
		t.Fatalf("DistinctAssets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 distinct assets, got %d: %v", len(got), ids(got))
	}
	if next != "" {
		t.Errorf("next cursor should be empty when page not full, got %q", next)
	}

	for _, want := range []c.Asset{xlm, usdc, pho} {
		has, err := store.HasAsset(ctx, want)
		if err != nil {
			t.Fatalf("HasAsset %s: %v", want.String(), err)
		}
		if !has {
			t.Errorf("HasAsset(%s) = false, want true", want.String())
		}
	}
	// And a seeded-but-different asset should NOT be found.
	notInDB, _ := c.NewFiatAsset("EUR")
	if has, _ := store.HasAsset(ctx, notInDB); has {
		t.Error("HasAsset(EUR) should be false")
	}

	// F-0157 perf: an unknown classic asset must route through
	// classic_assets PK lookup and return false. The classic_assets
	// table is populated by InsertTrade's registerClassicAssetSeen
	// hook; an asset_id never seen by that hook (e.g. a random
	// 4-char code against a real-but-unrelated G-strkey) must be
	// known-unknown without touching the trades hypertable.
	bogusClassic, err := c.NewClassicAsset(
		"ZZZZ",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	)
	if err != nil {
		t.Fatalf("NewClassicAsset(ZZZZ-G...): %v", err)
	}
	has, hasErr := store.HasAsset(ctx, bogusClassic)
	if hasErr != nil {
		t.Fatalf("HasAsset(ZZZZ-G...): %v", hasErr)
	}
	if has {
		t.Errorf("HasAsset(%s) = true, want false (bogus classic asset)", bogusClassic.String())
	}

	// The non-classic arm (timescale.Store.hasNonClassicAsset) is a
	// window-bounded, alias-complete probe since 2026-09-09. Two halves
	// of its contract only a real Timescale can settle, and the
	// package's scripted-driver tests deliberately cannot:
	//
	//   (a) the `= ANY($1)` alias array has to BIND — pgx encodes the
	//       []string as text[] itself, so a shape Postgres rejects
	//       (42883 and friends) shows up only against a live server;
	//   (b) the `ts >= $2` floor has to actually EXCLUDE, which is the
	//       narrowed semantic the fix chose and the thing most likely to
	//       drift back without a test standing on it.

	// (a) native, crypto:XLM and the XLM SAC are one asset under three
	// canonical ids. Only the `native` leg was seeded above; each of the
	// other two spellings must still find it.
	for _, form := range []c.Asset{mustCryptoTest("XLM"), mustSorobanTest(c.XLMSacContractID)} {
		has, err := store.HasAsset(ctx, form)
		if err != nil {
			t.Fatalf("HasAsset(%s): %v", form.String(), err)
		}
		if !has {
			t.Errorf("HasAsset(%s) = false, want true — the seeded trades carry XLM as `native`, "+
				"and all three forms are the same asset (XLM dual-form rule)", form.String())
		}
	}

	// (b) an asset whose only trade predates the recency window reads as
	// absent. This is the deliberate narrowing: HasAsset answers for the
	// population /v1/assets lists (timescale.MarketsRecencyWindow), not
	// for all of history, because "all of history" needed the unbounded
	// hypertable scan that took /v1/assets/native past its 15s budget.
	stale := sorobanFromSeed(t, 9)
	staleTrade := c.Trade{
		Source:      "test",
		Ledger:      51_000_000,
		TxHash:      hexTx(9),
		OpIndex:     0,
		Timestamp:   time.Now().UTC().Add(-timescale.MarketsRecencyWindow - 48*time.Hour),
		Pair:        mustPair(stale, usdc),
		BaseAmount:  c.NewAmount(big.NewInt(1_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12)),
	}
	if err := store.InsertTrade(ctx, staleTrade); err != nil {
		t.Fatalf("InsertTrade (pre-window): %v", err)
	}
	if has, err := store.HasAsset(ctx, stale); err != nil {
		t.Fatalf("HasAsset(%s): %v", stale.String(), err)
	} else if has {
		t.Errorf("HasAsset(%s) = true, want false — its only trade is older than "+
			"MarketsRecencyWindow (%s), which is outside the window this probe answers for",
			stale.String(), timescale.MarketsRecencyWindow)
	}
	// …and the same contract becomes present the moment it trades inside
	// the window, so (b) is a window boundary and not a dead branch.
	freshTrade := staleTrade
	freshTrade.Ledger = 52_900_000
	freshTrade.TxHash = hexTx(10)
	freshTrade.Timestamp = time.Now().UTC().Truncate(time.Second)
	if err := store.InsertTrade(ctx, freshTrade); err != nil {
		t.Fatalf("InsertTrade (in-window): %v", err)
	}
	if has, err := store.HasAsset(ctx, stale); err != nil {
		t.Fatalf("HasAsset(%s) after in-window trade: %v", stale.String(), err)
	} else if !has {
		t.Errorf("HasAsset(%s) = false after an in-window trade, want true", stale.String())
	}
}

func TestAssetsReaderPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed 5 soroban assets with strkey-valid C-addresses derived
	// from seed bytes. Previous hand-written literals
	// (e.g. "CA001JYLG…XOWMA") were 55 chars — one short of the
	// strkey 56-char requirement, so canonical.NewSorobanAsset
	// rejected them as of 2026-04-23. strkey.Encode produces
	// checksum-valid addresses indexed deterministically by seed so
	// pagination ordering stays reproducible.
	assets := []c.Asset{
		sorobanFromSeed(t, 1),
		sorobanFromSeed(t, 2),
		sorobanFromSeed(t, 3),
		sorobanFromSeed(t, 4),
		sorobanFromSeed(t, 5),
	}
	// Seed each as BASE paired with native XLM.
	for i, a := range assets {
		tr := c.Trade{
			Source: "test", Ledger: uint32(52_000_000 + i),
			TxHash: hexTx(i), OpIndex: 0,
			Timestamp:   time.Now().UTC().Truncate(time.Second).Add(time.Duration(i) * time.Second),
			Pair:        mustPair(a, c.NativeAsset()),
			BaseAmount:  c.NewAmount(big.NewInt(1_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Request a page size of 2 — expect 3 pages (2+2+2 where last
	// page includes 1 extra native + 1 overflow = …). Actually
	// with 5 seeded sorobans + 1 native = 6 distinct assets total.
	// Iterate with cursor until next is empty.
	var allSeen []c.Asset
	cursor := ""
	for iter := 0; iter < 10; iter++ {
		page, next, err := store.DistinctAssets(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		allSeen = append(allSeen, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(allSeen) != 6 {
		t.Errorf("expected 6 distinct assets across pages, got %d: %v",
			len(allSeen), ids(allSeen))
	}

	// Ordering: ascending by asset string. C... sort after "native".
	for i := 1; i < len(allSeen); i++ {
		if allSeen[i-1].String() >= allSeen[i].String() {
			t.Errorf("not sorted at %d: %q >= %q",
				i, allSeen[i-1].String(), allSeen[i].String())
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func mustPair(base, quote c.Asset) c.Pair {
	p, err := c.NewPair(base, quote)
	if err != nil {
		panic(err)
	}
	return p
}

func mustSorobanTest(id string) c.Asset {
	a, err := c.NewSorobanAsset(id)
	if err != nil {
		panic(err)
	}
	return a
}

func mustCryptoTest(code string) c.Asset {
	a, err := c.NewCryptoAsset(code)
	if err != nil {
		panic(err)
	}
	return a
}

// sorobanFromSeed builds a Soroban asset whose C-strkey encodes a
// 32-byte contract ID whose first byte is `seed`. Produces a valid
// checksum-encoded C-strkey (56 chars) so canonical.NewSorobanAsset
// accepts it. Deterministic: the same seed always yields the same
// address, preserving pagination-order reproducibility.
func sorobanFromSeed(t *testing.T, seed byte) c.Asset {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	a, err := c.NewSorobanAsset(s)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	return a
}

func hexTx(i int) string {
	// Deterministic 64-char hex tx-hash for fixture data.
	base := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	// Swap two chars to differentiate; trades hypertable unique
	// key tolerates duplicates via ON CONFLICT DO NOTHING, but
	// we want distinct rows for counting.
	tail := "0123456789abcdef"[i%16]
	return base[:63] + string(tail)
}

func ids(as []c.Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return out
}
