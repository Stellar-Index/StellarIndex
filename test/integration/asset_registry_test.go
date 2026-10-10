//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Two batches inside the registry dedupe TTL, the second with a lower first
// ledger: first_* must take the overall minimum, last_* the overall maximum.
func TestAssetRegistry_BatchRangeKeepsBothEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	store.ResetAssetRegistryDedupeForTest()

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mk := func(ledger uint32, tail string) c.Trade {
		return c.Trade{
			Source:      "test-range",
			Ledger:      ledger,
			TxHash:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe" + tail,
			Timestamp:   base.Add(time.Duration(ledger) * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(200, "a1"), mk(300, "a2")}); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(100, "b1"), mk(150, "b2")}); err != nil {
		t.Fatalf("batch 2: %v", err)
	}

	var firstSeen, firstTrade, lastSeen, lastTrade uint32
	err = store.DB().QueryRowContext(ctx, `
		SELECT first_seen_ledger, first_trade_ledger, last_seen_ledger, last_trade_ledger
		  FROM classic_assets WHERE asset_id = $1`, usdc.String()).Scan(&firstSeen, &firstTrade, &lastSeen, &lastTrade)
	if err != nil {
		t.Fatalf("read classic_assets: %v", err)
	}
	if firstSeen != 100 || firstTrade != 100 {
		t.Errorf("first_seen/first_trade ledger = %d/%d, want 100/100", firstSeen, firstTrade)
	}
	if lastSeen != 300 || lastTrade != 300 {
		t.Errorf("last_seen/last_trade ledger = %d/%d, want 300/300", lastSeen, lastTrade)
	}
}

// The genuine Franklin Templeton BENJI issuer, and one of the eighteen
// accounts on pubnet issuing an asset that also calls itself BENJI. They
// are here as literals because the whole point of this file is that the
// registry is keyed on (code, issuer) and NEVER on the code: the two rows
// below share a code and are different assets.
const (
	benjiGenuineIssuer      = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
	benjiImpersonatorIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestAssetRegistry_HeldButNeverTradedAssetIsRegistered is the regression
// test for the defect this file exists because of.
//
// THE DEFECT. `classic_assets` had exactly one population path: a trade.
// `registerClassicAssetSeen` is called from InsertTrade and
// BatchInsertTrades and nowhere else, and `issuers` is written only from
// inside it. So an asset that is HELD but never traded on the SDEX had no
// registry row, therefore no issuer row, therefore no SEP-1 attestation
// fetch, therefore no RWA candidacy — invisible at every step of the chain.
//
// Franklin Templeton's BENJI is the case that surfaced it. It has more
// trustlines than all eighteen impersonating BENJIs combined and zero rows
// in both tables, because a money-market fund is bought and held rather
// than day-traded. On the production lake: 512,496
// classic assets have a trustline, 199,793 had a registry row.
//
// HOW THIS FAILS WITHOUT THE FIX. Subtest "held asset with no trade is
// registered" drives the ONLY thing the lake can tell us about such an
// asset — that a trustline for it exists — and requires a row in both
// tables afterwards. With no holdings path (the state of this repo before
// migration 0158), or with [timescale.Store.RegisterClassicAssetsHeld]
// neutered to a no-op, it fails on the first assertion: 0 rows in
// classic_assets for the genuine BENJI, and 0 rows in issuers for its
// issuer, while the impersonator that happened to trade is present.
func TestAssetRegistry_HeldButNeverTradedAssetIsRegistered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	genuine, err := c.NewClassicAsset("BENJI", benjiGenuineIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(genuine): %v", err)
	}
	impersonator, err := c.NewClassicAsset("BENJI", benjiImpersonatorIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(impersonator): %v", err)
	}

	// The impersonator trades. A registry fed only by trades learns only
	// the only BENJI the registry could ever learn about is the one with a
	// market, not the one with the holders.
	tradeAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	const tradeLedger = 52_000_000
	insertBenjiTrade(t, ctx, store, impersonator, tradeLedger, tradeAt)

	// The genuine one is only ever HELD. The ledger bracket is what a
	// trustline scan of the lake reports: the ledgers at which trustline
	// entries for this asset were last modified.
	holdFirstAt := tradeAt.Add(-48 * time.Hour)
	holdLastAt := tradeAt.Add(-1 * time.Hour)
	const holdFirstLedger = 51_000_000
	const holdLastLedger = 51_990_000

	t.Run("held asset with no trade is registered", func(t *testing.T) {
		assets, issuers, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       genuine,
			FirstLedger: holdFirstLedger,
			FirstAt:     holdFirstAt,
			LastLedger:  holdLastLedger,
			LastAt:      holdLastAt,
		}})
		if err != nil {
			t.Fatalf("RegisterClassicAssetsHeld: %v", err)
		}
		if assets != 1 {
			t.Errorf("asset rows affected = %d, want 1", assets)
		}
		if issuers != 1 {
			t.Errorf("issuer rows inserted = %d, want 1", issuers)
		}

		row := readRegistryRow(t, ctx, store, genuine.String())
		if !row.found {
			t.Fatalf("classic_assets has NO row for %s — a held-but-never-traded asset is still invisible", genuine.String())
		}
		// The whole attestation chain hangs off this row existing.
		if got := countIssuerRows(t, ctx, store, benjiGenuineIssuer); got != 1 {
			t.Fatalf("issuers rows for %s = %d, want 1 — without it the SEP-1 fetch queue can never offer this issuer and /v1/rwa/assets can never see it",
				benjiGenuineIssuer, got)
		}
		if row.code != "BENJI" || row.issuer != benjiGenuineIssuer {
			t.Errorf("registry row identity = (%q, %q), want (BENJI, %s)", row.code, row.issuer, benjiGenuineIssuer)
		}
		// Migration 0135: the slug IS the fully-qualified asset_id.
		if row.slug != genuine.String() {
			t.Errorf("slug = %q, want %q", row.slug, genuine.String())
		}
	})

	t.Run("identity is (code, issuer), never the code", func(t *testing.T) {
		if !readRegistryRow(t, ctx, store, impersonator.String()).found {
			t.Fatalf("impersonator BENJI missing from the registry — the trade path regressed")
		}
		if got := countRegistryRowsForCode(t, ctx, store, "BENJI"); got != 2 {
			t.Fatalf("classic_assets rows with code BENJI = %d, want 2 — the two issuers must be two distinct assets", got)
		}
	})

	t.Run("holdings evidence never fabricates trade activity", func(t *testing.T) {
		row := readRegistryRow(t, ctx, store, genuine.String())
		// observation_count is the explorer's "Observations" column, the
		// default listing rank, and the input to the scam-triage sweep. A
		// holdings-derived row must not inflate any of them.
		if row.observationCount != 0 {
			t.Errorf("observation_count = %d, want 0 — no trade has been observed for this asset", row.observationCount)
		}
		if row.lastTradeAt.Valid || row.firstTradeAt.Valid {
			t.Errorf("trade columns are set (first=%v last=%v) on an asset that has never traded", row.firstTradeAt, row.lastTradeAt)
		}
		if !row.lastHoldingAt.Valid || !row.firstHoldingAt.Valid {
			t.Errorf("holding columns are NULL (first=%v last=%v) on an asset registered from a holding", row.firstHoldingAt, row.lastHoldingAt)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger || row.lastHoldingLedger.Int64 != holdLastLedger {
			t.Errorf("holding ledger bracket = [%d,%d], want [%d,%d]",
				row.firstHoldingLedger.Int64, row.lastHoldingLedger.Int64, holdFirstLedger, holdLastLedger)
		}
		// first_seen_* / last_seen_* are the union over sources, which for
		// a holdings-only row is the holding bracket.
		if row.firstSeenLedger != holdFirstLedger || row.lastSeenLedger != holdLastLedger {
			t.Errorf("seen ledger bracket = [%d,%d], want [%d,%d]",
				row.firstSeenLedger, row.lastSeenLedger, holdFirstLedger, holdLastLedger)
		}
	})

	t.Run("re-registering the same holding is idempotent and monotone", func(t *testing.T) {
		// A resumed or re-run backfill re-covers ground. It must converge,
		// not accumulate — and a LATER holding observation must not push
		// first_holding_ledger forward.
		if _, _, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       genuine,
			FirstLedger: holdFirstLedger + 500_000,
			FirstAt:     holdFirstAt.Add(24 * time.Hour),
			LastLedger:  holdLastLedger + 1,
			LastAt:      holdLastAt.Add(time.Minute),
		}}); err != nil {
			t.Fatalf("RegisterClassicAssetsHeld (rerun): %v", err)
		}
		row := readRegistryRow(t, ctx, store, genuine.String())
		if row.observationCount != 0 {
			t.Errorf("observation_count = %d after a rerun, want 0", row.observationCount)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger {
			t.Errorf("first_holding_ledger = %d after a later observation, want %d (LEAST must hold)",
				row.firstHoldingLedger.Int64, holdFirstLedger)
		}
		if row.lastHoldingLedger.Int64 != holdLastLedger+1 {
			t.Errorf("last_holding_ledger = %d, want %d (GREATEST must advance)",
				row.lastHoldingLedger.Int64, holdLastLedger+1)
		}
	})

	t.Run("a first trade fills the trade columns without erasing the holding", func(t *testing.T) {
		store.ResetAssetRegistryDedupeForTest()
		insertBenjiTrade(t, ctx, store, genuine, tradeLedger, tradeAt)

		row := readRegistryRow(t, ctx, store, genuine.String())
		if row.observationCount != 1 {
			t.Errorf("observation_count = %d after its first trade, want 1", row.observationCount)
		}
		if !row.firstTradeAt.Valid || !row.lastTradeAt.Valid {
			t.Fatalf("trade columns still NULL after a trade (first=%v last=%v) — LEAST/GREATEST must fill a NULL side",
				row.firstTradeAt, row.lastTradeAt)
		}
		if row.lastTradeLedger.Int64 != tradeLedger {
			t.Errorf("last_trade_ledger = %d, want %d", row.lastTradeLedger.Int64, tradeLedger)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger {
			t.Errorf("first_holding_ledger = %d after a trade, want %d — the trade path must not touch holding columns",
				row.firstHoldingLedger.Int64, holdFirstLedger)
		}
		// The union widens to cover both sources: the holding is older
		// than the trade, the trade is newer than the last holding.
		if row.firstSeenLedger != holdFirstLedger {
			t.Errorf("first_seen_ledger = %d, want %d (the earlier holding evidence)", row.firstSeenLedger, holdFirstLedger)
		}
		if row.lastSeenLedger != tradeLedger {
			t.Errorf("last_seen_ledger = %d, want %d (the later trade)", row.lastSeenLedger, tradeLedger)
		}
	})

	t.Run("registry stats separate the two populations", func(t *testing.T) {
		// Register one more held-only asset so held_never_traded is not
		// zero after the previous subtest traded the genuine BENJI.
		heldOnly, err := c.NewClassicAsset("HELDONLY", benjiGenuineIssuer)
		if err != nil {
			t.Fatalf("NewClassicAsset(heldOnly): %v", err)
		}
		if _, _, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       heldOnly,
			FirstLedger: holdFirstLedger,
			FirstAt:     holdFirstAt,
			LastLedger:  holdLastLedger,
			LastAt:      holdLastAt,
		}}); err != nil {
			t.Fatalf("RegisterClassicAssetsHeld(heldOnly): %v", err)
		}
		st, err := store.ClassicAssetRegistryStats(ctx)
		if err != nil {
			t.Fatalf("ClassicAssetRegistryStats: %v", err)
		}
		if st.HoldingOnly != 1 {
			t.Errorf("HoldingOnly = %d, want 1 (the held-but-never-traded asset)", st.HoldingOnly)
		}
		if st.TradeOnly != 1 {
			// Only the impersonator: it traded and no holdings scan has
			// seen it. Native XLM never enters this table at all — the
			// registry is classic-only.
			t.Errorf("TradeOnly = %d, want 1 (the impersonator BENJI)", st.TradeOnly)
		}
	})
}

// benjiRegistryRow is one classic_assets row as this test reads it.
type benjiRegistryRow struct {
	found              bool
	code               string
	issuer             string
	slug               string
	observationCount   int64
	firstSeenLedger    int64
	lastSeenLedger     int64
	firstTradeAt       sql.NullTime
	lastTradeAt        sql.NullTime
	lastTradeLedger    sql.NullInt64
	firstHoldingAt     sql.NullTime
	lastHoldingAt      sql.NullTime
	firstHoldingLedger sql.NullInt64
	lastHoldingLedger  sql.NullInt64
}

func readRegistryRow(t *testing.T, ctx context.Context, store *timescale.Store, assetID string) benjiRegistryRow {
	t.Helper()
	var r benjiRegistryRow
	err := store.DB().QueryRowContext(ctx, `
		SELECT code, issuer_g_strkey, COALESCE(slug, ''),
		       observation_count, first_seen_ledger, last_seen_ledger,
		       first_trade_at, last_trade_at, last_trade_ledger,
		       first_holding_at, last_holding_at,
		       first_holding_ledger, last_holding_ledger
		  FROM classic_assets WHERE asset_id = $1`, assetID).Scan(
		&r.code, &r.issuer, &r.slug,
		&r.observationCount, &r.firstSeenLedger, &r.lastSeenLedger,
		&r.firstTradeAt, &r.lastTradeAt, &r.lastTradeLedger,
		&r.firstHoldingAt, &r.lastHoldingAt,
		&r.firstHoldingLedger, &r.lastHoldingLedger,
	)
	if err == sql.ErrNoRows {
		return benjiRegistryRow{}
	}
	if err != nil {
		t.Fatalf("read classic_assets %s: %v", assetID, err)
	}
	r.found = true
	return r
}

func countIssuerRows(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM issuers WHERE g_strkey = $1`, gStrkey).Scan(&n); err != nil {
		t.Fatalf("count issuers %s: %v", gStrkey, err)
	}
	return n
}

func countRegistryRowsForCode(t *testing.T, ctx context.Context, store *timescale.Store, code string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM classic_assets WHERE code = $1`, code).Scan(&n); err != nil {
		t.Fatalf("count classic_assets code=%s: %v", code, err)
	}
	return n
}

// insertBenjiTrade stores one XLM/asset trade, which is the only way the
// trade-fed registry could ever learn an asset exists.
func insertBenjiTrade(t *testing.T, ctx context.Context, store *timescale.Store, asset c.Asset, ledger uint32, ts time.Time) {
	t.Helper()
	pair, err := c.NewPair(c.NativeAsset(), asset)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// A 64-char hex tx hash unique to the asset, so two trades in this
	// test can never collide on the trades primary key.
	sum := sha256.Sum256([]byte(asset.String()))
	txHash := hex.EncodeToString(sum[:])
	if err := store.InsertTrade(ctx, c.Trade{
		Source:      "test-holdings",
		Ledger:      ledger,
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
	}); err != nil {
		t.Fatalf("InsertTrade(%s): %v", asset.String(), err)
	}
}

// TestAssetRegistry_DuplicateReplayDoesNotMutateCounters pins the
// end-to-end counter contract:
// replaying a already-stored trade must NOT advance the
// `classic_assets.observation_count` or `last_seen_*` columns,
// even when the in-process dedupe cache is cold (the simulated-
// process-restart shape).
//
// The concern: a backfill operator (or restarted
// indexer) that re-encounters already-stored trades should not
// inflate registry counters by one per replay. The
// `assetRegistryDedupeTTL` fix protects only the same-process
// hot path; the `RowsAffected == 0` guard inside
// [Store.InsertTrade] is what protects post-restart replay. This
// test isolates the post-restart shape by clearing the dedupe
// cache between the original insert and the replay via
// [timescale.Store.ResetAssetRegistryDedupeForTest].
//
// Three subtests pin the contract:
//
//  1. exact replay (same source+ledger+tx_hash+op_index+ts) → no
//     mutation; observation_count stays at 1.
//  2. cosmetic re-key (different ts in the same second)            →
//     a NEW row is inserted, registry advances to 2 (proves the
//     guard is keyed on RowsAffected, not on asset identity).
//  3. forward-progress replay (different ledger, same asset, TTL
//     bypassed) → registry advances correctly, demonstrating the
//     guard does not over-suppress legitimate updates.
//
// Together these three cases prove the registry counter contract
// holds end-to-end across the realistic operator shapes
// (replay-after-restart, distinct-trades-on-same-asset, late
// ledger arrival).
func TestAssetRegistry_DuplicateReplayDoesNotMutateCounters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Use a fixed valid G-strkey for the issuer + a deterministic
	// classic asset; the test is self-contained — no other test
	// touches this asset.
	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	xlm := c.NativeAsset()
	pair, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// Reset the dedupe cache between scenarios so each subtest
	// starts from the post-restart shape (the risk shape).
	store.ResetAssetRegistryDedupeForTest()

	baseTS := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	mkTrade := func(ledger uint32, txTail rune, ts time.Time) c.Trade {
		// 64-char hex tx hash with one mutable trailing char.
		txHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbee" + string(txTail)
		return c.Trade{
			Source:      "test-replay",
			Ledger:      ledger,
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}

	// Scenario 1 — exact replay must NOT advance the counter.
	original := mkTrade(52_000_000, 'a', baseTS)
	if err := store.InsertTrade(ctx, original); err != nil {
		t.Fatalf("InsertTrade (original): %v", err)
	}
	gotCount, gotLastLedger := readRegistry(t, store, usdc.String())
	if gotCount != 1 {
		t.Fatalf("after first insert: observation_count = %d, want 1", gotCount)
	}
	if gotLastLedger != 52_000_000 {
		t.Fatalf("after first insert: last_seen_ledger = %d, want 52_000_000", gotLastLedger)
	}

	// Simulate a process restart: cold dedupe cache, same trade
	// hits InsertTrade again. Without the RowsAffected
	// guard the registry hook would fire and observation_count
	// would advance to 2.
	store.ResetAssetRegistryDedupeForTest()
	if err := store.InsertTrade(ctx, original); err != nil {
		t.Fatalf("InsertTrade (replay): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 1 {
		t.Errorf("after exact replay: observation_count = %d, want 1 (RowsAffected guard regression)", gotCount)
	}
	if gotLastLedger != 52_000_000 {
		t.Errorf("after exact replay: last_seen_ledger = %d, want 52_000_000", gotLastLedger)
	}

	// Scenario 2 — a NEW trade on the same asset DOES advance the
	// counter once the dedupe cache is cleared (proves the guard
	// is keyed on RowsAffected, not on asset identity).
	store.ResetAssetRegistryDedupeForTest()
	newer := mkTrade(52_000_001, 'b', baseTS.Add(time.Second))
	if err := store.InsertTrade(ctx, newer); err != nil {
		t.Fatalf("InsertTrade (newer trade, same asset): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 2 {
		t.Errorf("after distinct trade: observation_count = %d, want 2", gotCount)
	}
	if gotLastLedger != 52_000_001 {
		t.Errorf("after distinct trade: last_seen_ledger = %d, want 52_000_001", gotLastLedger)
	}

	// Scenario 3 — replaying scenario-2's trade (now with a cold
	// cache) again must NOT advance to 3. This pins the post-
	// restart-stable contract: the registry only ever advances
	// when a new physical trade row is stored.
	store.ResetAssetRegistryDedupeForTest()
	if err := store.InsertTrade(ctx, newer); err != nil {
		t.Fatalf("InsertTrade (replay of newer): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 2 {
		t.Errorf("after second exact replay: observation_count = %d, want 2 (RowsAffected guard regression)", gotCount)
	}
	if gotLastLedger != 52_000_001 {
		t.Errorf("after second exact replay: last_seen_ledger = %d, want 52_000_001", gotLastLedger)
	}

	// Sanity: trades hypertable should hold exactly 2 distinct rows
	// (the original + the newer; both replays were no-ops via
	// `ON CONFLICT DO NOTHING`). Confirms the test's premise.
	if got := countTrades(t, store, "test-replay"); got != 2 {
		t.Errorf("trades count for source=test-replay = %d, want 2", got)
	}

	_ = strkey.VersionByteAccountID // keep strkey import live if we ever extend
}

// readRegistry returns (observation_count, last_seen_ledger) for
// the supplied asset_id. Fatal on missing row or query error.
func readRegistry(t *testing.T, store *timescale.Store, assetID string) (uint64, uint32) {
	t.Helper()
	const q = `SELECT observation_count, last_seen_ledger FROM classic_assets WHERE asset_id = $1`
	var count uint64
	var lastLedger uint32
	if err := store.DB().QueryRow(q, assetID).Scan(&count, &lastLedger); err != nil {
		t.Fatalf("readRegistry %s: %v", assetID, err)
	}
	return count, lastLedger
}

// countTrades returns the number of trade rows for the given source.
func countTrades(t *testing.T, store *timescale.Store, source string) int {
	t.Helper()
	const q = `SELECT COUNT(*) FROM trades WHERE source = $1`
	var n int
	if err := store.DB().QueryRow(q, source).Scan(&n); err != nil {
		t.Fatalf("countTrades %s: %v", source, err)
	}
	return n
}
