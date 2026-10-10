//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-migrate/migrate/v4"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalbinance "github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	externalbitstamp "github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	externalcoingecko "github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	externalecb "github.com/Stellar-Index/StellarIndex/internal/sources/external/ecb"
	externalexchangerates "github.com/Stellar-Index/StellarIndex/internal/sources/external/exchangeratesapi"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCCTPRozoEventIndex_SameTypeEventsInOneOpDoNotCollapse is the
// proof. Migrations 0038 / 0039 keyed cctp_events / rozo_events on
// (contract_id, ledger, tx_hash, op_index, event_type, ts) — no intra-op
// discriminator. A single operation that emits TWO events of the SAME type
// (observed on mainnet, e.g. two `attester_enabled` in one MessageTransmitter
// tx) produced two rows that shared every PK column, so the second collapsed
// onto the first under ON CONFLICT — silently dropping it.
//
// Migration 0112 adds event_index to the key. This test inserts two
// same-type events that differ ONLY in event_index and asserts BOTH land.
// If the key omitted event_index, COUNT would be 1 (collapsed); it must be 2.
func TestCCTPRozoEventIndex_SameTypeEventsInOneOpDoNotCollapse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ts := time.Now().UTC().Truncate(time.Second)
	const (
		txHash  = "ababababababababababababababababababababababababababababababab" // 62 chars; + 2-char suffix below = 64 (char(64))
		ledger  = uint32(62_146_641)
		opIndex = uint32(0)
	)

	t.Run("cctp two same-type events in one op", func(t *testing.T) {
		mk := func(idx uint32) timescale.CCTPEvent {
			return timescale.CCTPEvent{
				ContractID: cctp.MainnetMessageTransmitter,
				Ledger:     ledger,
				TxHash:     txHash + "cc", // pad to 64
				OpIndex:    opIndex,
				EventIndex: idx,
				ObservedAt: ts,
				EventType:  timescale.CCTPAttesterEnabled,
				Attributes: map[string]any{"attester": fmt.Sprintf("att-%d", idx)},
			}
		}
		if err := store.InsertCCTPEvent(ctx, mk(0)); err != nil {
			t.Fatalf("InsertCCTPEvent idx=0: %v", err)
		}
		if err := store.InsertCCTPEvent(ctx, mk(1)); err != nil {
			t.Fatalf("InsertCCTPEvent idx=1: %v", err)
		}
		got := countRows(t, store, `SELECT COUNT(*) FROM cctp_events WHERE contract_id = $1 AND ledger = $2 AND tx_hash = $3 AND op_index = $4 AND event_type = 'attester_enabled'`,
			cctp.MainnetMessageTransmitter, int(ledger), txHash+"cc", int(opIndex))
		if got != 2 {
			t.Fatalf("cctp_events rows = %d, want 2 — two same-type events in one op collapsed (C2-13a regression)", got)
		}
		// The two rows must carry distinct event_index (0 and 1).
		idxSum := countRows(t, store, `SELECT COALESCE(SUM(event_index), -1) FROM cctp_events WHERE contract_id = $1 AND ledger = $2 AND tx_hash = $3 AND op_index = $4 AND event_type = 'attester_enabled'`,
			cctp.MainnetMessageTransmitter, int(ledger), txHash+"cc", int(opIndex))
		if idxSum != 1 { // 0 + 1
			t.Errorf("sum(event_index) = %d, want 1 (indexes 0 and 1)", idxSum)
		}
	})

	t.Run("rozo two same-type events in one op", func(t *testing.T) {
		from := "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
		mk := func(idx uint32) timescale.RozoEvent {
			f := from
			return timescale.RozoEvent{
				ContractID:  "CROZOPAYMENTCONTRACTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				Ledger:      ledger,
				TxHash:      txHash + "rz", // pad to 64
				OpIndex:     opIndex,
				EventIndex:  idx,
				ObservedAt:  ts,
				EventType:   timescale.RozoPayment,
				Amount:      "1000000",
				Destination: "GDESTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				From:        &f,
			}
		}
		if err := store.InsertRozoEvent(ctx, mk(0)); err != nil {
			t.Fatalf("InsertRozoEvent idx=0: %v", err)
		}
		if err := store.InsertRozoEvent(ctx, mk(1)); err != nil {
			t.Fatalf("InsertRozoEvent idx=1: %v", err)
		}
		got := countRows(t, store, `SELECT COUNT(*) FROM rozo_events WHERE tx_hash = $1 AND op_index = $2 AND event_type = 'payment'`,
			txHash+"rz", int(opIndex))
		if got != 2 {
			t.Fatalf("rozo_events rows = %d, want 2 — two same-type events in one op collapsed (C2-13a regression)", got)
		}
	})
}

// countRows runs a single-integer-returning query and returns the value.
func countRows(t *testing.T, store *timescale.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("countRows: %v", err)
	}
	return n
}

// TestCCTPReplayDoubleCount_DisarmedByMigration0164 is the proof.
//
// Migration 0112 added event_index to cctp_events' PK and backfilled
// existing rows to event_index=0 on the premise that a prescribed
// re-ingest needs no DELETE. That premise is false: dispatcher_adapter.go
// stamps EventIndex from the event's TRUE intra-op position (cctp's own
// decoder doc: a deposit_for_burn op typically also emits message_sent in
// the SAME op), so a legacy row stored at event_index=0 pre-0112 and a
// projector-replay row computed at its true nonzero index are two DIFFERENT
// PKs for the SAME real-world event — a double-count, not a recovery.
//
// This test reproduces that shape directly against migration 0112 (up to
// 0163, before the disarm): a legacy event_index=0 row plus a "replay" row
// for the same op at its true nonzero index leaves TWO rows for what
// should be one event. It then applies migration 0164 and asserts the
// disarm clears the duplicated legacy state.
func TestCCTPReplayDoubleCount_DisarmedByMigration0164(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 163)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ts := time.Now().UTC().Truncate(time.Second)
	const (
		txHash  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcddc"
		ledger  = uint32(62_146_641)
		opIndex = uint32(0)
	)
	mk := func(idx uint32) timescale.CCTPEvent {
		return timescale.CCTPEvent{
			ContractID: cctp.MainnetMessageTransmitter,
			Ledger:     ledger,
			TxHash:     txHash,
			OpIndex:    opIndex,
			EventIndex: idx,
			ObservedAt: ts,
			EventType:  timescale.CCTPDepositForBurn,
			Attributes: map[string]any{"nonce": "1"},
		}
	}

	// Legacy pre-0112 row: backfilled to event_index=0.
	if err := store.InsertCCTPEvent(ctx, mk(0)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	// Catch-up replay recomputes the event's TRUE intra-op index (nonzero,
	// because message_sent precedes it in this op) and writes a NEW row —
	// a different PK for the SAME real event.
	if err := store.InsertCCTPEvent(ctx, mk(3)); err != nil {
		t.Fatalf("insert replay row: %v", err)
	}

	got := countRows(t, store, `SELECT COUNT(*) FROM cctp_events WHERE tx_hash = $1 AND op_index = $2 AND event_type = 'deposit_for_burn'`,
		txHash, int(opIndex))
	if got != 2 {
		t.Fatalf("pre-disarm cctp_events rows for one real event = %d, want 2 — the double-count this test reproduces did not occur; T434 may already be fixed upstream", got)
	}

	applyMigrationTo164(t, dsn)

	after := countRows(t, store, `SELECT COUNT(*) FROM cctp_events`)
	if after != 0 {
		t.Fatalf("cctp_events rows after migration 0164 = %d, want 0 — disarm did not clear the doubled legacy rows", after)
	}
}

// applyMigrationTo164 advances a database already at 0163 to 0164 (the
// cctp/rozo replay double-count disarm), mirroring applyMigrationsUpTo but
// on an already-open migrate instance's next step.
func applyMigrationTo164(t *testing.T, dsn string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(164); err != nil {
		t.Fatalf("migrate to 164: %v", err)
	}
}

// TestDecoderOutputFitsStorageSchema builds one canonical.Trade
// per Soroban source with the exact field shape the source's real
// decoder emits (strkey-valid assets, i128-scale amounts, realistic
// timestamps), inserts through the real InsertTrade /
// InsertOracleUpdate paths, then queries the row back. Plus one
// Reflector OracleUpdate.
//
// This proves that canonical.Trade / canonical.OracleUpdate
// produced by the decoders actually satisfy the trades /
// oracle_updates hypertable schema — something pure unit tests
// (without a live DB) can't check. Schema-level concerns the
// unit tests miss:
//
//   - NUMERIC bounds for 128-bit amounts.
//   - NOT NULL columns that weren't populated.
//   - Primary-key uniqueness + ON CONFLICT DO NOTHING semantics.
//   - UTC / timezone coercion on Timestamp round-trip.
//   - TEXT-column length limits for long strkeys.
//
// Decoder correctness is still tested at the unit level in each
// source's real_fixture_test.go against real mainnet captures —
// this file is deliberately about the schema bridge.
//
// One container serves all subtests to keep boot cost amortized.
func TestDecoderOutputFitsStorageSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t.Run("soroswap/trade", func(t *testing.T) {
		// Soroswap emits 7-decimal tokens with i128-scale amounts.
		// Use realistic magnitudes and a matching swap+sync pair —
		// we only persist the resulting canonical.Trade here.
		trade := canonical.Trade{
			Source:    soroswap.SourceName,
			Ledger:    62_240_138,
			TxHash:    hashLit("27ed2ab56d42"),
			OpIndex:   0,
			Timestamp: nowUTC(t),
			Pair: mustPair(
				mustSorobanAsset(t, 0x10),
				mustSorobanAsset(t, 0x11)),
			BaseAmount:  amountLit(t, "842000000"),  // 84.2 at 7 decimals
			QuoteAmount: amountLit(t, "1242000000"), // 124.2 at 7 decimals
		}
		insertAndVerifyTrade(ctx, t, store, trade)
	})

	t.Run("aquarius/trade", func(t *testing.T) {
		// Aquarius trades pass taker (often a router C-strkey) in
		// topic[3]. Include it here so the taker column path is
		// exercised.
		trade := canonical.Trade{
			Source:      aquarius.SourceName,
			Ledger:      62_150_001,
			TxHash:      hashLit("78d86d0651d6"),
			OpIndex:     0,
			Timestamp:   nowUTC(t),
			Pair:        mustPair(mustSorobanAsset(t, 0x20), mustSorobanAsset(t, 0x21)),
			BaseAmount:  amountLit(t, "1000000000"), // 100 at 7 decimals
			QuoteAmount: amountLit(t, "12420000"),   // 12.42 at 6 decimals
			Taker:       mustContractStrkey(t, 0x22),
		}
		insertAndVerifyTrade(ctx, t, store, trade)
	})

	t.Run("phoenix/trade", func(t *testing.T) {
		// Phoenix fills Taker from the sender event body (G- or
		// C-strkey). Use an account G-strkey here to exercise the
		// G-branch of the storage layer's strkey validation.
		trade := canonical.Trade{
			Source:      phoenix.SourceName,
			Ledger:      62_152_147,
			TxHash:      hashLit("e02dd755d908"),
			OpIndex:     0,
			Timestamp:   nowUTC(t),
			Pair:        mustPair(mustSorobanAsset(t, 0x30), mustSorobanAsset(t, 0x31)),
			BaseAmount:  amountLit(t, "999999999999"),
			QuoteAmount: amountLit(t, "1"),
			Taker:       mustAccountStrkey(t, 0x32),
		}
		insertAndVerifyTrade(ctx, t, store, trade)
	})

	t.Run("phoenix/large_i128", func(t *testing.T) {
		// ADR-0003 boundary — value above 2^63 must survive the
		// NUMERIC column round-trip. Catches any silent truncation
		// in the SQL driver or column type.
		big1 := new(big.Int)
		big1.SetString("123456789012345678901234567890", 10) // ~ 2^96
		trade := canonical.Trade{
			Source:      phoenix.SourceName,
			Ledger:      62_152_148,
			TxHash:      hashLit("aaaaaaaaaaaa"),
			OpIndex:     1,
			Timestamp:   nowUTC(t),
			Pair:        mustPair(mustSorobanAsset(t, 0x40), mustSorobanAsset(t, 0x41)),
			BaseAmount:  canonical.NewAmount(big1),
			QuoteAmount: canonical.NewAmount(big1),
			Taker:       mustAccountStrkey(t, 0x42),
		}
		insertAndVerifyTrade(ctx, t, store, trade)
	})

	t.Run("reflector/fiat_oracle", func(t *testing.T) {
		// FX oracle update — fiat asset, USD quote, 14-decimal price.
		eur, err := canonical.NewFiatAsset("EUR")
		if err != nil {
			t.Fatal(err)
		}
		usd, _ := canonical.NewFiatAsset("USD")
		update := canonical.OracleUpdate{
			Source:     reflector.SourceFX,
			ContractID: mustContractStrkey(t, 0xF0),
			Ledger:     62_251_211,
			TxHash:     hashLit("f59b732d06a5"),
			OpIndex:    0,
			Timestamp:  nowUTC(t),
			Asset:      eur,
			Quote:      usd,
			Price:      amountLit(t, "109000000000000"), // 1.09 at 14 decimals
			Decimals:   reflector.DefaultDecimals,
			Observer:   mustAccountStrkey(t, 0xF1),
		}
		insertAndVerifyOracle(ctx, t, store, update)
	})

	t.Run("reflector/crypto_oracle", func(t *testing.T) {
		// CEX oracle update — crypto-ticker asset (ADR-0014),
		// proves the new AssetCrypto type round-trips through the
		// Asset SQL value/scan path.
		btc, err := canonical.NewCryptoAsset("BTC")
		if err != nil {
			t.Fatal(err)
		}
		usd, _ := canonical.NewFiatAsset("USD")
		update := canonical.OracleUpdate{
			Source:     reflector.SourceCEX,
			ContractID: mustContractStrkey(t, 0xC0),
			Ledger:     62_251_266,
			TxHash:     hashLit("eb374149026f"),
			OpIndex:    1,
			Timestamp:  nowUTC(t),
			Asset:      btc,
			Quote:      usd,
			Price:      amountLit(t, "7000000000000000000"), // 70000 at 14 decimals
			Decimals:   reflector.DefaultDecimals,
			Observer:   mustAccountStrkey(t, 0xC1),
		}
		insertAndVerifyOracle(ctx, t, store, update)
	})

	t.Run("reflector/dex_oracle", func(t *testing.T) {
		// DEX oracle update — Soroban asset (on-chain SAC).
		xlm := canonical.NativeAsset()
		update := canonical.OracleUpdate{
			Source:     reflector.SourceDEX,
			ContractID: mustContractStrkey(t, 0xD0),
			Ledger:     62_251_160,
			TxHash:     hashLit("9322ba2f5c95"),
			OpIndex:    0,
			Timestamp:  nowUTC(t),
			Asset:      mustSorobanAsset(t, 0xD2),
			Quote:      xlm,
			Price:      amountLit(t, "12420000000000"),
			Decimals:   reflector.DefaultDecimals,
			Observer:   "",
		}
		insertAndVerifyOracle(ctx, t, store, update)
	})
}

// ─── helpers ────────────────────────────────────────────────────

func insertAndVerifyTrade(ctx context.Context, t *testing.T, store *timescale.Store, trade canonical.Trade) {
	t.Helper()
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	rows, err := store.LatestTradesForPair(ctx, trade.Pair, 10)
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	var match *canonical.Trade
	for i := range rows {
		r := rows[i]
		if r.Source == trade.Source &&
			r.Ledger == trade.Ledger &&
			r.TxHash == trade.TxHash &&
			r.OpIndex == trade.OpIndex {
			match = &r
			break
		}
	}
	if match == nil {
		t.Fatalf("inserted trade not found: source=%s ledger=%d tx=%s op=%d",
			trade.Source, trade.Ledger, trade.TxHash, trade.OpIndex)
	}
	if match.BaseAmount.Cmp(trade.BaseAmount) != 0 {
		t.Errorf("base amount round-trip: got %s, want %s",
			match.BaseAmount, trade.BaseAmount)
	}
	if match.QuoteAmount.Cmp(trade.QuoteAmount) != 0 {
		t.Errorf("quote amount round-trip: got %s, want %s",
			match.QuoteAmount, trade.QuoteAmount)
	}
	if match.Taker != trade.Taker {
		t.Errorf("taker round-trip: got %q, want %q", match.Taker, trade.Taker)
	}
	// Timestamp round-trip tolerates microsecond-precision truncation
	// (Postgres TIMESTAMPTZ stores to microseconds; Go time.Time has
	// nanosecond precision).
	drift := match.Timestamp.Sub(trade.Timestamp)
	if drift < -time.Second || drift > time.Second {
		t.Errorf("timestamp drift: got %v, want %v (Δ%v)",
			match.Timestamp, trade.Timestamp, drift)
	}
}

func insertAndVerifyOracle(ctx context.Context, t *testing.T, store *timescale.Store, update canonical.OracleUpdate) {
	t.Helper()
	if err := store.InsertOracleUpdate(ctx, update); err != nil {
		t.Fatalf("InsertOracleUpdate: %v", err)
	}
	got, err := store.LatestOracleUpdateForAsset(ctx, update.Source, update.Asset)
	if err != nil {
		t.Fatalf("LatestOracleUpdateForAsset: %v", err)
	}
	if got == nil {
		t.Fatal("row not found")
	}
	if got.Source != update.Source ||
		got.TxHash != update.TxHash ||
		got.OpIndex != update.OpIndex {
		t.Errorf("identity drift: got %+v, want %+v", got, update)
	}
	if got.Price.Cmp(update.Price) != 0 {
		t.Errorf("price round-trip: got %s, want %s", got.Price, update.Price)
	}
	if !got.Asset.Equal(update.Asset) {
		t.Errorf("asset round-trip: got %+v, want %+v", got.Asset, update.Asset)
	}
	if !got.Quote.Equal(update.Quote) {
		t.Errorf("quote round-trip: got %+v, want %+v", got.Quote, update.Quote)
	}
}

// hashLit pads a short hex prefix to a full 64-char tx_hash so the
// storage layer's regex-or-length check (if any) is satisfied.
func hashLit(prefix string) string {
	const width = 64
	pad := width - len(prefix)
	out := prefix
	for i := 0; i < pad; i++ {
		out += "0"
	}
	return out
}

func amountLit(t *testing.T, decimal string) canonical.Amount {
	t.Helper()
	a, err := canonical.FromString(decimal)
	if err != nil {
		t.Fatalf("FromString(%q): %v", decimal, err)
	}
	return a
}

// mustPair is shared with assets_test.go (also in package
// integration_test) — do not redeclare. See that file for the
// canonical definition.

func mustSorobanAsset(t *testing.T, seed byte) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(mustContractStrkey(t, seed))
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	return a
}

func mustContractStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func mustAccountStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func nowUTC(t *testing.T) time.Time {
	t.Helper()
	// Truncate to seconds so the round-trip delta check is stable
	// across Postgres's microsecond column + Go's nanosecond in-
	// memory representation.
	return time.Now().UTC().Truncate(time.Second)
}

// TestExternalFleet_EndToEnd is the Phase-2-ingestion closing-
// ceremony test: proves every external-source class (Streamer,
// Poller, aggregator, authority_sanity) is wired end-to-end
// through external.Run → shared consumer.Event channel →
// Timescale inserts → query round-trip.
//
// Five venues covered, one per representative class:
//
//  1. Binance (Streamer, exchange class) — WS aggTrade frames
//     against a scripted httptest WS server.
//  2. Bitstamp (Streamer, exchange class) — proves multi-streamer
//     fan-out; different wire format (live_trades_* channels).
//  3. ExchangeRatesApi (Poller, exchange class, FX) — inverts
//     base→target rates; emits OracleUpdates not Trades.
//  4. CoinGecko (Poller, aggregator class) — divergence-only;
//     emitted OracleUpdates land in oracle_updates with
//     source=coingecko (VWAP exclusion happens at aggregator
//     query time, not at insert).
//  5. ECB (Poller, authority_sanity class) — daily XML; inverts
//     EUR-base to asset-in-EUR form.
//
// The test does NOT exercise Kraken / Coinbase / CoinMarketCap /
// CryptoCompare since they share shape with one
// of the five and would just add runtime without proving
// additional coverage. Their unit tests at the package level
// already pin correctness.
func TestExternalFleet_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ─── Mock venues ─────────────────────────────────────────────
	binanceSrv := newBinanceMockWS(t)
	defer binanceSrv.Close()

	bitstampSrv := newBitstampMockWS(t)
	defer bitstampSrv.Close()

	exratesSrv := newExRatesMockREST(t)
	defer exratesSrv.Close()

	// The poller must stamp rows with CoinGecko's last_updated_at, not
	// our poll time; a minute in the past keeps the two distinguishable.
	cgUpdatedAt := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	coingeckoSrv := newCoinGeckoMockREST(t, cgUpdatedAt)
	defer coingeckoSrv.Close()

	ecbSrv := newECBMockREST(t)
	defer ecbSrv.Close()

	// ─── Build connectors pointing at the mocks ─────────────────
	binancePM, _ := externalbinance.DefaultPairs()
	binancePairs, _ := externalbinance.DefaultPairList()
	binanceS := externalbinance.NewStreamer(binancePM)
	binanceS.Endpoint = replaceWSScheme(binanceSrv.URL)

	bitstampPM, _ := externalbitstamp.DefaultPairs()
	bitstampPairs, _ := externalbitstamp.DefaultPairList()
	bitstampS := externalbitstamp.NewStreamer(bitstampPM)
	bitstampS.Endpoint = replaceWSScheme(bitstampSrv.URL)

	exrates, err := externalexchangerates.NewPoller("test_key")
	if err != nil {
		t.Fatalf("NewPoller exrates: %v", err)
	}
	exrates.Endpoint = exratesSrv.URL
	exrates.Interval = 100 * time.Millisecond // accelerate for test

	cg := externalcoingecko.NewPoller()
	cg.Endpoint = coingeckoSrv.URL
	cg.Interval = 100 * time.Millisecond

	ecb := externalecb.NewPoller()
	ecb.Endpoint = ecbSrv.URL
	ecb.Interval = 100 * time.Millisecond

	// Pair list the FX pollers target — G10 subset.
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	gbp, _ := canonical.NewFiatAsset("GBP")
	xlmCrypto, _ := canonical.NewCryptoAsset("XLM")
	btcCrypto, _ := canonical.NewCryptoAsset("BTC")
	eurUSD, _ := canonical.NewPair(eur, usd)
	gbpUSD, _ := canonical.NewPair(gbp, usd)
	xlmUSD, _ := canonical.NewPair(xlmCrypto, usd)
	btcUSD, _ := canonical.NewPair(btcCrypto, usd)

	fxPairs := []canonical.Pair{eurUSD, gbpUSD}
	aggregatorPairs := []canonical.Pair{xlmUSD, btcUSD}

	streamers := []external.StreamerSpec{
		{Streamer: binanceS, Pairs: binancePairs},
		{Streamer: bitstampS, Pairs: bitstampPairs},
	}
	pollers := []external.PollerSpec{
		{Poller: exrates, Pairs: fxPairs},
		{Poller: cg, Pairs: aggregatorPairs},
		{Poller: ecb, Pairs: fxPairs},
	}

	// ─── Drain + persist ─────────────────────────────────────────
	events := make(chan consumer.Event, 128)

	wait, err := external.Run(ctx, streamers, pollers, events, nil)
	if err != nil {
		t.Fatalf("external.Run: %v", err)
	}

	// Persist goroutine — the same shape as cmd/stellarindex-indexer's
	// persistEvents but without panic recovery / obs metrics.
	// persistCtx is DECOUPLED from the fleet ctx: `cancel()` below stops the
	// streamers/pollers, but the consumer keeps draining events already
	// buffered on the channel AFTER the cancel — inserting those with the
	// cancelled fleet ctx flaked as "InsertOracleUpdate: context canceled".
	// Same reasoning the post-drain assertions already use a fresh assertCtx.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer persistCancel()
	persistDone := make(chan struct{})
	var insertedTrades, insertedUpdates int
	go func() {
		defer close(persistDone)
		for e := range events {
			switch ev := e.(type) {
			case external.TradeEvent:
				// FAIL on insert error: the CEX/FX fleet
				// is exercised end-to-end only here, so a swallowed insert (e.g.
				// a NUMERIC-scale/schema drift) would otherwise pass silently.
				// t.Errorf (not Fatal) is goroutine-safe.
				if err := store.InsertTrade(persistCtx, ev.Trade); err != nil {
					t.Errorf("InsertTrade: %v", err)
					continue
				}
				insertedTrades++
			case external.UpdateEvent:
				if err := store.InsertOracleUpdate(persistCtx, ev.Update); err != nil {
					t.Errorf("InsertOracleUpdate: %v", err)
					continue
				}
				insertedUpdates++
			default:
				t.Errorf("unhandled event %T (a new event type slipped the fleet test)", e)
			}
		}
	}()

	// Let the fleet run: 2 seconds is enough for mock streamers to
	// drain their fixture frames and for pollers (100ms interval)
	// to fire 10+ times each.
	time.Sleep(2 * time.Second)
	cancel()
	wait()
	close(events)
	<-persistDone

	// Fresh context for post-drain assertions — the streamer ctx
	// was cancelled to shut down the fleet; using it for SELECTs
	// would surface as "context canceled" in the store layer.
	assertCtx, assertCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer assertCancel()

	// ─── Assertions ─────────────────────────────────────────────
	if insertedTrades < 2 {
		t.Errorf("expected at least 2 trades inserted (Binance + Bitstamp), got %d", insertedTrades)
	}
	if insertedUpdates < 5 {
		// 3 pollers × minimum 1 update each. In practice the FX
		// pollers emit 2 updates (EUR+GBP) and CoinGecko 2
		// (XLM/USD + BTC/USD), so 6+ is typical.
		t.Errorf("expected at least 5 updates inserted (pollers × ~2), got %d", insertedUpdates)
	}

	// Spot-check: Binance XLM/USDT trade landed.
	usdt, _ := canonical.NewCryptoAsset("USDT")
	binancePair, _ := canonical.NewPair(xlmCrypto, usdt)
	binanceTrades, err := store.LatestTradesForPair(assertCtx, binancePair, 10)
	if err != nil {
		t.Fatalf("LatestTradesForPair XLM/USDT: %v", err)
	}
	if len(binanceTrades) == 0 {
		t.Error("expected Binance XLM/USDT trade to have landed")
	}

	// Spot-check: ExchangeRatesApi EUR rate landed.
	exratesLatest, err := store.LatestOracleUpdateForAsset(assertCtx, externalexchangerates.SourceName, eur)
	if err != nil {
		t.Errorf("LatestOracleUpdateForAsset exrates EUR: %v", err)
	} else if exratesLatest.Price.BigInt().Sign() <= 0 {
		t.Errorf("exrates EUR price = %s, expected >0", exratesLatest.Price)
	}

	// Spot-check: ECB USD→EUR anchor landed.
	ecbLatest, err := store.LatestOracleUpdateForAsset(assertCtx, externalecb.SourceName, usd)
	if err != nil {
		t.Errorf("LatestOracleUpdateForAsset ecb USD: %v", err)
	} else if !ecbLatest.Quote.Equal(eur) {
		t.Errorf("ecb USD quote = %+v, expected EUR", ecbLatest.Quote)
	}

	// Spot-check: CoinGecko XLM/USD landed — aggregator class.
	cgLatest, err := store.LatestOracleUpdateForAsset(assertCtx, externalcoingecko.SourceName, xlmCrypto)
	if err != nil {
		t.Errorf("LatestOracleUpdateForAsset coingecko XLM: %v", err)
	} else {
		if cgLatest.Price.BigInt().Sign() <= 0 {
			t.Errorf("coingecko XLM price = %s", cgLatest.Price)
		}
		if !cgLatest.Timestamp.Equal(cgUpdatedAt) {
			t.Errorf("coingecko XLM ts = %s, want upstream last_updated_at %s",
				cgLatest.Timestamp, cgUpdatedAt)
		}
	}

	t.Logf("external-fleet end-to-end: %d trades + %d updates inserted", insertedTrades, insertedUpdates)
}

// ─── Mock servers ───────────────────────────────────────────────

func replaceWSScheme(u string) string {
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}

// newBinanceMockWS returns an httptest server that accepts a
// combined-stream WS connection and writes a single aggTrade frame
// for XLMUSDT before holding the connection open.
func newBinanceMockWS(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{
          "stream":"xlmusdt@aggTrade",
          "data":{"e":"aggTrade","E":1745000000000,"s":"XLMUSDT","a":1,"p":"0.17582","q":"152.34","f":1,"l":1,"T":1745000000000,"m":true}
        }`))
		<-r.Context().Done()
	}))
}

// newBitstampMockWS returns an httptest server that accepts the
// Bitstamp subscribe message and replies with one live_trades_xlmusd
// event.
func newBitstampMockWS(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()

		// Read all subscribe messages Bitstamp's streamer sends (one per pair).
		// We don't need to ack them; just drain.
		var muSub sync.Mutex
		go func() {
			for {
				_, _, err := conn.Read(r.Context())
				if err != nil {
					muSub.Lock()
					muSub.Unlock()
					return
				}
			}
		}()

		// Wait briefly for subscribe messages to arrive.
		time.Sleep(100 * time.Millisecond)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{
          "event":"trade",
          "channel":"live_trades_xlmusd",
          "data":{
            "id":999,"timestamp":"1745000000","microtimestamp":"1745000000000000",
            "amount":100.0,"amount_str":"100.0","price":0.17580,"price_str":"0.17580",
            "type":0,"buy_order_id":1,"sell_order_id":2
          }
        }`))
		<-r.Context().Done()
	}))
}

// newExRatesMockREST serves ExchangeRatesApi shape.
func newExRatesMockREST(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"success":   true,
			"timestamp": 1745000000,
			"base":      "USD",
			"date":      "2026-04-24",
			"rates":     map[string]any{"EUR": 0.9235, "GBP": 0.7845},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

// newCoinGeckoMockREST serves /api/v3/simple/price with
// include_last_updated_at's per-id field set to updatedAt.
func newCoinGeckoMockREST(t *testing.T, updatedAt time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/simple/price") {
			http.NotFound(w, r)
			return
		}
		lastUpdated := float64(updatedAt.Unix())
		body := map[string]map[string]float64{
			"stellar": {"usd": 0.17582, "last_updated_at": lastUpdated},
			"bitcoin": {"usd": 50000.0, "last_updated_at": lastUpdated},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

// newECBMockREST serves the gesmes XML shape.
func newECBMockREST(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <Cube>
    <Cube time="2026-04-23">
      <Cube currency="USD" rate="1.0825"/>
      <Cube currency="GBP" rate="0.8450"/>
    </Cube>
  </Cube>
</gesmes:Envelope>`)
	}))
}

// TestRozoMemoBytes_HostileMemoLandsInPostgres is the proof against a
// REAL Postgres, through the production path end to end:
//
//	events.Event -> rozo.Decoder.Decode -> pipeline.HandleEvent
//	             -> persistRozoEvent -> Store.InsertRozoEvent -> rozo_events
//
// A Rozo v1 memo is an ScString — the payer picks its BYTES, for one
// stroop. rozo_events.memo is `text`, and Postgres refuses a NUL or an
// invalid UTF-8 sequence there with SQLSTATE 22021. The projector classes
// that as a permanent data error and skips the event for good, so before
// the fix every case below marked hostile failed HandleEvent and left no
// row: an attacker-chosen, un-closable hole in the bridge history.
//
// Asserted per memo: the insert succeeds; exactly one row lands; the stored
// text is the exact deterministic scval.ToText value; the on-chain bytes
// are recoverable both in Go (scval.FromText) and in SQL (the documented
// decode(substr(memo, 3), 'hex') recipe); and a replay of the same event
// rewrites the same single row. Ordinary memos must be stored verbatim.
func TestRozoMemoBytes_HostileMemoLandsInPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cases := []struct {
		name     string
		memo     string
		wantText string
	}{
		{"ordinary tag stored verbatim", "binance-deposit-tag-987654", "binance-deposit-tag-987654"},
		{"empty memo stays empty not NULL", "", ""},
		{"multibyte utf8 stored verbatim", "commande-№42-日本", "commande-№42-日本"},
		{"highest rune and a noncharacter", "a\U0010FFFF￿b", "a\U0010FFFF￿b"},
		{"hostile: NUL byte", "tag\x00tail", `\x746167007461696c`},
		{"hostile: invalid utf8", "tag\xff\xfe", `\x746167fffe`},
		{"hostile: truncated multibyte", "ab\xe2\x82", `\x6162e282`},
		{"hostile: overlong NUL", "\xc0\x80", `\xc080`},
		{"hostile: encoded surrogate", "\xed\xa0\x80", `\xeda080`},
		{"hostile: above U+10FFFF", "\xf4\x90\x80\x80", `\xf4908080`},
		{"literal that mimics an encoding", `\x00`, `\x5c783030`},
	}

	logger := discardLogger()
	decoder := rozo.NewDecoder()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := uint32(62_800_000 + i)
			txHash := fmt.Sprintf("%064x", 0xf052_0000+i)
			ev := rozoPaymentEventWithMemo(t, ledger, txHash, tc.memo)

			// Twice: the second pass is the re-derive. It must produce the
			// same row, not a second one and not a different memo.
			for pass := 1; pass <= 2; pass++ {
				out, err := decoder.Decode(ev)
				if err != nil || len(out) != 1 {
					t.Fatalf("pass %d: Decode = %d events, err %v — a hostile memo must not cost the event", pass, len(out), err)
				}
				if err := pipeline.HandleEvent(ctx, logger, store, out[0]); err != nil {
					t.Fatalf("pass %d: HandleEvent: %v — Postgres refused the row; in production this is a "+
						"permanent skip and the payment is lost (F052)", pass, err)
				}
			}

			if n := countRows(t, store, `SELECT COUNT(*) FROM rozo_events WHERE ledger = $1 AND tx_hash = $2`, int(ledger), txHash); n != 1 {
				t.Fatalf("rozo_events rows = %d, want exactly 1", n)
			}

			var (
				memo    sql.NullString
				sqlBack []byte
			)
			if err := store.DB().QueryRowContext(ctx, `
				SELECT memo,
				       CASE WHEN left(memo, 2) = '\x'
				            THEN decode(substr(memo, 3), 'hex')
				            ELSE convert_to(memo, 'UTF8') END
				  FROM rozo_events WHERE ledger = $1 AND tx_hash = $2`, int(ledger), txHash).Scan(&memo, &sqlBack); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !memo.Valid {
				t.Fatal("memo stored as NULL; a payment memo is always a value")
			}
			if memo.String != tc.wantText {
				t.Fatalf("stored memo = %q, want %q", memo.String, tc.wantText)
			}
			goBack, err := scval.FromText(memo.String)
			if err != nil {
				t.Fatalf("FromText(%q): %v", memo.String, err)
			}
			if string(goBack) != tc.memo {
				t.Fatalf("bytes lost (Go): on-chain %q, recovered %q", tc.memo, goBack)
			}
			if !bytes.Equal(sqlBack, []byte(tc.memo)) {
				t.Fatalf("bytes lost (SQL recipe): on-chain %q, recovered %q", tc.memo, sqlBack)
			}
		})
	}
}

// rozoPaymentEventWithMemo builds the live long-topic v1 PaymentEvent
// ({amount, destination, from, memo} ScMap, topic ("payment_event",)) for a
// gated mainnet Rozo contract, with a caller-chosen memo.
func rozoPaymentEventWithMemo(t *testing.T, ledger uint32, txHash, memo string) events.Event {
	t.Helper()
	from := prMakeAccountStrkey(t, 0x21)
	dest := prMakeAccountStrkey(t, 0x31)
	body := prScMap(
		xdr.ScMapEntry{Key: prSymbol("amount"), Val: prI128(big.NewInt(1))},
		xdr.ScMapEntry{Key: prSymbol("destination"), Val: prAccountAddr(t, dest)},
		xdr.ScMapEntry{Key: prSymbol("from"), Val: prAccountAddr(t, from)},
		xdr.ScMapEntry{Key: prSymbol("memo"), Val: prScString(memo)},
	)
	return events.Event{
		Type:                     "contract",
		Ledger:                   ledger,
		LedgerClosedAt:           "2026-09-01T00:00:00Z",
		ContractID:               rozo.MainnetPaymentContract,
		OperationIndex:           0,
		EventIndex:               0,
		TxHash:                   txHash,
		InSuccessfulContractCall: true,
		Topic:                    []string{prB64(t, prSymbol("payment_event"))},
		Value:                    prB64(t, body),
	}
}

// TestCopyMergeSEP41_GenerationGuard is the proven-red DB-backed test that
// the BULK COPY+merge writers used by ch_rebuild (CopyMergeSEP41SupplyEvents /
// CopyMergeSEP41Transfers) carry the SAME generation-guarded corrective-upsert
// semantics as their per-row siblings, not generation-0 `ON CONFLICT DO NOTHING`.
//
// A naive bulk path (a) omits derive_generation from the COPY column list, so
// every bulk row takes the column DEFAULT 0, and (b) merges DO NOTHING, so a
// corrected re-derive of a wrong money value silently no-ops against an
// existing PK. With that shape:
//   - "corrective re-derive lands" goes RED (DO NOTHING keeps the wrong V1).
//   - "gen-0 replay cannot revert" would trivially pass on DO NOTHING but is
//     kept as the companion assertion that pins the guard direction once the
//     merge is DO UPDATE.
//
// TV-3 is the same defect observed from ch_rebuild: with the primary bulk
// path at DO NOTHING and the per-row fallback at gen-guarded DO UPDATE, a
// COPY batch error silently flipped additive-recovery into an overwrite.
// Unifying the bulk path on the guarded DO UPDATE makes the two paths
// semantically identical, which this test's use of the real bulk writer pins.
//
// To reproduce red: revert copyMergeUpsertSQL (sep41_copy.go) to
// `... ON CONFLICT ... DO NOTHING` (keep migration 0110 + SetDeriveGeneration)
// and the "corrective re-derive lands" assertion goes red.
func TestCopyMergeSEP41_GenerationGuard(t *testing.T) {
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
		v1 = 12_000_000 // wrong original
		v2 = 99_000_000 // corrected re-derive
		v3 = 55_000_000 // stale gen-0 replay (must be ignored)
	)

	// ── sep41_supply_events via CopyMergeSEP41SupplyEvents ─────────────────
	t.Run("SupplyEvents", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			ledger     = uint32(70_200_001)
		)
		obs := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
		txHash := pad64("3", 9)
		mk := func(amt int64) timescale.SEP41SupplyEvent {
			return timescale.SEP41SupplyEvent{
				ContractID: contractID,
				Ledger:     ledger,
				TxHash:     txHash,
				OpIndex:    0,
				EventIndex: 0,
				ObservedAt: obs,
				Kind:       timescale.SEP41EventMint,
				Amount:     big.NewInt(amt),
			}
		}
		read := func() (amount string, gen int64) {
			const q = `SELECT amount::text, derive_generation FROM sep41_supply_events WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&amount, &gen); err != nil {
				t.Fatalf("read sep41_supply_events: %v", err)
			}
			return amount, gen
		}

		// gen 1 — the wrong original value lands via the bulk path.
		store.SetDeriveGeneration(1)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v1)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V1: %v", err)
		}
		if amt, gen := read(); amt != "12000000" || gen != 1 {
			// The generation must NOT be the DEFAULT 0 (TV-1): if derive_generation
			// is omitted from the COPY list, gen reads 0 here and this fails.
			t.Fatalf("after gen-1 bulk load: amount=%s gen=%d, want 12000000 / gen 1 "+
				"(TV-1: omitting derive_generation from COPY makes gen default to 0)", amt, gen)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in place.
		// The unfixed DO-NOTHING bulk merge keeps V1 → this goes red.
		store.SetDeriveGeneration(2)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v2)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V2: %v", err)
		}
		if amt, gen := read(); amt != "99000000" || gen != 2 {
			t.Errorf("after gen-2 corrective bulk re-derive: amount=%s gen=%d, want 99000000 / gen 2 "+
				"(TV-1/TV-3: the old DO NOTHING keeps 12000000)", amt, gen)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not revert.
		store.SetDeriveGeneration(0)
		if err := store.CopyMergeSEP41SupplyEvents(ctx, []timescale.SEP41SupplyEvent{mk(v3)}); err != nil {
			t.Fatalf("CopyMergeSEP41SupplyEvents V3 (gen 0 replay): %v", err)
		}
		if amt, _ := read(); amt != "99000000" {
			t.Errorf("after gen-0 bulk replay: amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", amt)
		}
	})

	// ── sep41_transfers via CopyMergeSEP41Transfers ────────────────────────
	t.Run("Transfers", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			from       = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
			to         = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
			ledger     = uint32(70_300_001)
		)
		obs := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
		txHash := pad64("4", 11)
		mk := func(amt int64) timescale.SEP41TransferRow {
			return timescale.SEP41TransferRow{
				ContractID: contractID,
				Ledger:     ledger,
				TxHash:     txHash,
				OpIndex:    0,
				EventIndex: 0,
				ObservedAt: obs,
				Kind:       timescale.SEP41Transfer,
				FromAddr:   from,
				ToAddr:     to,
				Amount:     big.NewInt(amt),
			}
		}
		read := func() (amount string, gen int64) {
			const q = `SELECT amount::text, derive_generation FROM sep41_transfers WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&amount, &gen); err != nil {
				t.Fatalf("read sep41_transfers: %v", err)
			}
			return amount, gen
		}

		store.SetDeriveGeneration(1)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v1)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V1: %v", err)
		}
		if amt, gen := read(); amt != "12000000" || gen != 1 {
			t.Fatalf("after gen-1 bulk load: amount=%s gen=%d, want 12000000 / gen 1", amt, gen)
		}

		store.SetDeriveGeneration(2)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v2)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V2: %v", err)
		}
		if amt, gen := read(); amt != "99000000" || gen != 2 {
			t.Errorf("after gen-2 corrective bulk re-derive: amount=%s gen=%d, want 99000000 / gen 2 "+
				"(TV-1/TV-3: the old DO NOTHING keeps 12000000)", amt, gen)
		}

		store.SetDeriveGeneration(0)
		if err := store.CopyMergeSEP41Transfers(ctx, []timescale.SEP41TransferRow{mk(v3)}); err != nil {
			t.Fatalf("CopyMergeSEP41Transfers V3 (gen 0 replay): %v", err)
		}
		if amt, _ := read(); amt != "99000000" {
			t.Errorf("after gen-0 bulk replay: amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", amt)
		}
	})
}

// TestMigration0174_NoOpLeavesCompressedChunksCompressed pins the v0.92.1
// neutralisation of 0174: over two populated compressed chunks it
// decompresses nothing and adds no constraint, leaves the version clean at
// 174, and its down succeeds both on that no-op state and on the v0.92.0
// state, where the old body had added sep41_transfers_amount_check.
func TestMigration0174_NoOpLeavesCompressedChunksCompressed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 173)
	if err := insertSEP41TransferAmount(ctx, db, 1, "transfer", "7"); err != nil {
		t.Fatalf("seed 2026-09 row: %v", err)
	}
	seedCompressedSEP41TransferChunk(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 174)
	requireCompressedSEP41TransferChunks(t, ctx, db, 2, "after the 0174 no-op")
	requireSEP41AmountCheck(t, ctx, db, false, "after the 0174 no-op")
	requireSchemaVersion(t, ctx, db, 174)
	if err := insertSEP41TransferAmount(ctx, db, 2, "set_admin", "-5"); err != nil {
		t.Fatalf("after the 0174 no-op a row the old CHECK refused was refused anyway (%v): something still adds the constraint", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM sep41_transfers WHERE op_index = 2`); err != nil {
		t.Fatalf("clear the probe row: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 173)
	requireSEP41AmountCheck(t, ctx, db, false, "after 0174 down from the no-op")

	// The v0.92.0 state: the old body ran, so the CHECK exists at 174.
	if _, err := db.ExecContext(ctx, `SELECT decompress_chunk(c, true) FROM show_chunks('sep41_transfers') c`); err != nil {
		t.Fatalf("decompress for the v0.92.0 0174 state: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE sep41_transfers ADD CONSTRAINT sep41_transfers_amount_check
		    CHECK ((amount IS NULL OR amount >= 0)
		           AND (event_kind NOT IN ('transfer', 'approve') OR amount IS NOT NULL))`); err != nil {
		t.Fatalf("recreate the v0.92.0 0174 state: %v", err)
	}
	applyMigrationsUpTo(t, dsn, 174)
	requireSEP41AmountCheck(t, ctx, db, true, "v0.92.0 state after the 0174 no-op")

	// Recompress before the down: the ADD CONSTRAINT above ran against
	// decompressed chunks, and left uncompressed. The v0.92.0-state down
	// (DROP CONSTRAINT on a CHECK that actually exists) is only exercised
	// against production's shape if it runs against compressed chunks too.
	seedCompressedSEP41TransferChunk(t, ctx, db)
	requireCompressedSEP41TransferChunks(t, ctx, db, 2, "recompressed before the v0.92.0-state down")

	applyMigrationsUpTo(t, dsn, 173)
	requireSEP41AmountCheck(t, ctx, db, false, "after 0174 down from the v0.92.0 state")
}

func requireSEP41AmountCheck(t *testing.T, ctx context.Context, db *sql.DB, want bool, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE conname = 'sep41_transfers_amount_check'
		   AND conrelid = 'sep41_transfers'::regclass`).Scan(&n); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	if (n == 1) != want {
		t.Fatalf("%s: sep41_transfers_amount_check present = %v, want %v", when, n == 1, want)
	}
}

func requireSchemaVersion(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != want || dirty {
		t.Fatalf("schema_migrations = %d dirty=%v, want %d clean", version, dirty, want)
	}
}

// insertSEP41TransferAmount writes one row at 2026/09/01 with op_index op;
// amount "" is SQL NULL.
func insertSEP41TransferAmount(ctx context.Context, db *sql.DB, op int, kind, amount string) error {
	var amt any
	if amount != "" {
		amt = amount
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO sep41_transfers (ledger_close_time, ledger, tx_hash, op_index,
		                             contract_id, event_kind, amount)
		VALUES (TIMESTAMPTZ '2026-09-01 00:00:00Z', 60000000, '\x01'::bytea, $1,
		        'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC', $2, $3::numeric)`,
		op, kind, amt)
	return err
}

// seedCompressedSEP41TransferChunk upserts one valid row in an old chunk and
// compresses both chunks, the shape production is in when 0174 applies.
func seedCompressedSEP41TransferChunk(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sep41_transfers (ledger_close_time, ledger, tx_hash, op_index,
		                             contract_id, event_kind, amount)
		VALUES (TIMESTAMPTZ '2025-01-15 00:00:00Z', 55000000, '\x02'::bytea, 0,
		        'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC', 'transfer', 42)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed historical sep41_transfers row: %v", err)
	}
	// Counted from compress_chunk itself, not timescaledb_information.chunks.
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('sep41_transfers') c
		) s`).Scan(&n); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if n != 2 {
		t.Fatalf("compressed %d sep41_transfers chunks, want 2", n)
	}
}

func requireCompressedSEP41TransferChunks(t *testing.T, ctx context.Context, db *sql.DB, want int, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'sep41_transfers' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read chunk compression state: %v", err)
	}
	if n != want {
		t.Fatalf("%s: %d compressed sep41_transfers chunks, want %d", when, n, want)
	}
}

// TestSorobanEventsBatchInsert exercises the
// InsertSorobanEventsBatch → CountSorobanEventsInRange paths against
// real TimescaleDB. ADR-0029. Inserts 100 synthetic rows across the
// full topic+body+op_args shape coverage and asserts they all land.
// Also verifies idempotency (re-inserting the same batch is a no-op
// via ON CONFLICT DO NOTHING).
func TestSorobanEventsBatchInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	rows := make([]sorobanevents.Row, 0, 100)
	for i := 0; i < 100; i++ {
		rows = append(rows, mkSyntheticRow(t, uint32(50_000_000+i), t0.Add(time.Duration(i)*time.Second), byte(i)))
	}

	if err := store.InsertSorobanEventsBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}

	got, err := store.CountSorobanEventsInRange(ctx, 50_000_000, 50_000_099)
	if err != nil {
		t.Fatalf("CountSorobanEventsInRange: %v", err)
	}
	if got != 100 {
		t.Errorf("CountSorobanEventsInRange = %d, want 100", got)
	}

	// Idempotency: re-insert the same batch — count unchanged.
	if err := store.InsertSorobanEventsBatch(ctx, rows); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	got2, err := store.CountSorobanEventsInRange(ctx, 50_000_000, 50_000_099)
	if err != nil {
		t.Fatalf("CountSorobanEventsInRange (after re-insert): %v", err)
	}
	if got2 != 100 {
		t.Errorf("after re-insert count = %d, want 100 (idempotent)", got2)
	}

	// Empty batch is a no-op.
	if err := store.InsertSorobanEventsBatch(ctx, nil); err != nil {
		t.Errorf("InsertSorobanEventsBatch(nil) = %v, want nil", err)
	}
	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{}); err != nil {
		t.Errorf("InsertSorobanEventsBatch(empty slice) = %v, want nil", err)
	}
}

// TestSorobanEventsBatchInsert_PreservesFiveOrMoreTopics is the
// DB-round-trip guard: a row carrying MORE than four topics must
// persist through InsertSorobanEventsBatch → topics_xdr (migration
// 0114) → StreamSorobanEvents with EVERY topic byte-for-byte intact,
// in order. A schema of four fixed topic_0..3 columns only would leave topics 5+
// nowhere to land; the topics_xdr bytea[] column keeps them
// all.
func TestSorobanEventsBatchInsert_PreservesFiveOrMoreTopics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const ledger = 50_500_000
	ts := time.Date(2026, 5, 25, 8, 0, 0, 0, time.UTC)

	var cid [32]byte
	cid[0] = 0x5A
	cid[1] = 0xBB
	cstrk, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	txh := make([]byte, 32)
	for i := range txh {
		txh[i] = 0x5A
	}

	// Six distinct topics — beyond the pre-0114 four-column cap.
	topics := [][]byte{
		mkBytes(0x01, 8),
		mkBytes(0x02, 9),
		mkBytes(0x03, 10),
		mkBytes(0x04, 11),
		mkBytes(0x05, 12), // topic[4]
		mkBytes(0x06, 13), // topic[5]
	}

	row := domain.SorobanEventRow{
		Ledger:          ledger,
		LedgerCloseTime: ts,
		TxHash:          txh,
		OpIndex:         0,
		EventIndex:      0,
		ContractID:      cstrk,
		ContractIDHex:   cid[:],
		TopicCount:      int16(len(topics)),
		Topic0Sym:       "multi_topic_event",
		Topic0XDR:       topics[0],
		Topic1XDR:       topics[1],
		Topic2XDR:       topics[2],
		Topic3XDR:       topics[3],
		TopicsXDR:       topics,
		BodyXDR:         mkBytes(0x77, 32),
	}

	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{row}); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}

	var got []domain.SorobanEventRow
	if err := store.StreamSorobanEvents(ctx, ledger, ledger, nil, nil, nil,
		func(r domain.SorobanEventRow) error {
			got = append(got, r)
			return nil
		}); err != nil {
		t.Fatalf("StreamSorobanEvents: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("streamed %d rows, want 1", len(got))
	}
	streamed := got[0]
	if len(streamed.TopicsXDR) != len(topics) {
		t.Fatalf("streamed TopicsXDR len = %d, want %d (topics 5+ lost through the DB round-trip)",
			len(streamed.TopicsXDR), len(topics))
	}
	for i, want := range topics {
		if !bytes.Equal(streamed.TopicsXDR[i], want) {
			t.Errorf("TopicsXDR[%d] = %x, want %x", i, streamed.TopicsXDR[i], want)
		}
	}
}

// mkSyntheticRow constructs one well-formed sorobanevents.Row with
// distinct (ledger, tx_hash) so the batch insert exercises the PK
// + index paths.
func mkSyntheticRow(t *testing.T, ledger uint32, ts time.Time, seed byte) sorobanevents.Row {
	t.Helper()
	var cid [32]byte
	cid[0] = seed
	cid[1] = 0xAA
	cstrk, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	txh := make([]byte, 32)
	for i := range txh {
		txh[i] = seed
	}
	// Topic 0 — encode a plausible Symbol so topic_0_sym exercises
	// the not-NULL index path on some rows. For variety: alternate
	// rows have a NULL topic_0_sym (we use empty string for those).
	topic0 := mkBytes(seed, 16)
	var sym string
	if seed%2 == 0 {
		sym = "synthetic_event"
	}
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: ts,
		TxHash:          txh,
		OpIndex:         int16(seed % 4),
		EventIndex:      0,
		ContractID:      cstrk,
		ContractIDHex:   cid[:],
		TopicCount:      2,
		Topic0Sym:       sym,
		Topic0XDR:       topic0,
		Topic1XDR:       mkBytes(seed, 12),
		Topic2XDR:       nil,
		Topic3XDR:       nil,
		BodyXDR:         mkBytes(seed, 64),
		OpArgsXDR:       mkBytes(seed, 24),
	}
}

func mkBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed ^ byte(i)
	}
	return b
}

// _ keeps hex imported in case follow-up tests want to inspect hex.
var _ = hex.EncodeToString
