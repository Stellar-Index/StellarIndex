//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestProjectedRebuild_TwoWindowRunThenResume is the ADR-0048 D3 end-to-end
// proof: seed a few Rozo v1 Payment events into the ClickHouse lake across
// three ledger windows, run chops.RunProjectedRebuild against a real
// Postgres, and assert
//
//  1. rows land through the SAME decoder + sink path the live projector
//     uses (pipeline.HandleEvent — exercised indirectly via
//     RunProjectedRebuild), and
//  2. a second invocation with -resume (Resume: true) SKIPS the
//     already-checkpointed windows entirely — not just idempotently
//     re-writing them, but never re-streaming ClickHouse for that range —
//     while still picking up a newly-extended window.
//
// Rozo is the fixture source of choice: its decoder gates on a fixed,
// in-code contract-id set with no factory/child registry to warm and no
// oracle config to supply, so the test needs no gated-registry seeding —
// projector.BuildRegistry needs only an empty config.OracleConfig.
func TestProjectedRebuild_TwoWindowRunThenResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = rozo.MainnetPaymentContract

	// Three PaymentEvents in three DIFFERENT 100-ledger windows:
	//   window [1000,1099] -> ledger 1050
	//   window [1100,1199] -> ledger 1150
	//   window [1200,1299] -> ledger 1250 (seeded only for the second run)
	seedRozoPayment(t, ctx, chAddr, contractID, 1050, "tx-a-1111111111111111111111111111111111111111111111111111111111", 1_000_0000000, "alice-memo")
	seedRozoPayment(t, ctx, chAddr, contractID, 1150, "tx-b-2222222222222222222222222222222222222222222222222222222222", 2_000_0000000, "bob-memo")

	registry, err := projector.BuildRegistry([]string{rozo.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("build projector registry: %v", err)
	}
	if len(registry.Sources) != 1 {
		t.Fatalf("expected exactly one registered source for %q, got %d", rozo.SourceName, len(registry.Sources))
	}
	src := registry.Sources[0]

	// ─── Run 1: covers [1000,1199], both seeded events ───────────────────
	result1, err := chops.RunProjectedRebuild(ctx, chops.ProjectedRebuildOptions{
		Store:            store,
		ChAddr:           chAddr,
		Source:           src,
		From:             1000,
		To:               1199,
		Window:           100,
		Workers:          2,
		Write:            true,
		Resume:           true,
		ProgressInterval: time.Hour, // never fires; keep test output quiet
	})
	if err != nil {
		t.Fatalf("RunProjectedRebuild (run 1): %v", err)
	}
	if result1.WindowsPlanned != 2 || result1.WindowsProcessed != 2 || result1.WindowsSkipped != 0 {
		t.Fatalf("run 1 windows: planned=%d processed=%d skipped=%d, want 2/2/0",
			result1.WindowsPlanned, result1.WindowsProcessed, result1.WindowsSkipped)
	}
	if result1.EventsEmitted != 2 {
		t.Fatalf("run 1 EventsEmitted = %d, want 2", result1.EventsEmitted)
	}
	if got := result1.KindCounts["rozo.event"]; got != 2 {
		t.Fatalf("run 1 KindCounts[rozo.event] = %d, want 2", got)
	}

	assertRozoEventLedgers(t, ctx, store.DB(), []uint32{1050, 1150})

	// ─── Seed a THIRD event in a not-yet-covered window ──────────────────
	seedRozoPayment(t, ctx, chAddr, contractID, 1250, "tx-c-3333333333333333333333333333333333333333333333333333333333", 3_000_0000000, "carol-memo")

	// ─── Run 2: extend To to 1299 with -resume — must SKIP windows 1 & 2 ──
	result2, err := chops.RunProjectedRebuild(ctx, chops.ProjectedRebuildOptions{
		Store:            store,
		ChAddr:           chAddr,
		Source:           src,
		From:             1000,
		To:               1299,
		Window:           100,
		Workers:          2,
		Write:            true,
		Resume:           true,
		ProgressInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("RunProjectedRebuild (run 2): %v", err)
	}
	if result2.WindowsPlanned != 3 {
		t.Fatalf("run 2 WindowsPlanned = %d, want 3", result2.WindowsPlanned)
	}
	if result2.WindowsSkipped != 2 {
		t.Fatalf("run 2 WindowsSkipped = %d, want 2 (the two already-checkpointed windows from run 1)", result2.WindowsSkipped)
	}
	if result2.WindowsProcessed != 1 {
		t.Fatalf("run 2 WindowsProcessed = %d, want 1 (only the newly-extended window)", result2.WindowsProcessed)
	}
	// The strongest resume assertion: run 2 must not have RE-STREAMED the
	// already-done windows at all, so EventsRead/EventsEmitted reflect only
	// the one new window's event — not idempotent re-processing of all 3.
	if result2.EventsEmitted != 1 {
		t.Fatalf("run 2 EventsEmitted = %d, want 1 (resume must skip re-streaming done windows entirely, not just no-op their writes)", result2.EventsEmitted)
	}

	// Final state: all three rows present, no duplicates from either run.
	assertRozoEventLedgers(t, ctx, store.DB(), []uint32{1050, 1150, 1250})
}

// assertRozoEventLedgers queries rozo_events directly (no repo reader
// exists for this narrow assertion) and checks the exact set of distinct
// ledgers present — proving both "the rows landed" and "no duplicates /
// no phantom rows from a resumed run re-touching already-done windows".
func assertRozoEventLedgers(t *testing.T, ctx context.Context, db *sql.DB, want []uint32) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT ledger FROM rozo_events ORDER BY ledger`)
	if err != nil {
		t.Fatalf("query rozo_events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []uint32
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("scan rozo_events.ledger: %v", err)
		}
		got = append(got, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("rozo_events ledgers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rozo_events ledgers = %v, want %v", got, want)
		}
	}
}

// seedRozoPayment writes one ClickHouse contract_events row shaped exactly
// like a real Rozo v1 PaymentEvent — the same on-wire ScMap shape
// rozo.DecodePayment expects ({from, destination, amount, memo}) — plus its
// minimal ledger header, through the production chstore.Sink so the
// fixture goes through the same write path any other Tier-1 lake test
// uses (TestClickHouseLakeRoundTrip).
func seedRozoPayment(t *testing.T, ctx context.Context, chAddr, contractID string, ledger uint32, txHash string, amountStroops int64, memo string) {
	t.Helper()
	closeTime := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC).Add(time.Duration(ledger) * 5 * time.Second)

	from := prMakeAccountStrkey(t, byte(ledger%251+1))
	dest := prMakeAccountStrkey(t, byte((ledger+7)%251+1))
	amount := big.NewInt(amountStroops)

	body := prScMap(
		xdr.ScMapEntry{Key: prSymbol("amount"), Val: prI128(amount)},
		xdr.ScMapEntry{Key: prSymbol("destination"), Val: prAccountAddr(t, dest)},
		xdr.ScMapEntry{Key: prSymbol("from"), Val: prAccountAddr(t, from)},
		xdr.ScMapEntry{Key: prSymbol("memo"), Val: prScString(memo)},
	)

	sink, err := chstore.Open(ctx, chAddr, 100)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer func() { _ = sink.Close(ctx) }()

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq:       ledger,
			CloseTime:       closeTime,
			LedgerHash:      "aa",
			PrevHash:        "bb",
			ProtocolVersion: 22,
			BucketListHash:  "cc",
			TxCount:         1,
			OpCount:         1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq:        ledger,
			CloseTime:        closeTime,
			TxHash:           txHash,
			OpIndex:          0,
			EventIndex:       0,
			ContractID:       contractID,
			EventType:        "contract",
			TopicCount:       1,
			Topic0Sym:        "payment_event",
			TopicsXDR:        []string{prB64(t, prSymbol("payment_event"))},
			DataXDR:          prB64(t, body),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add (ledger %d): %v", ledger, err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush (ledger %d): %v", ledger, err)
	}
}

// ─── small XDR-encode helpers (mirrors internal/sources/rozo/decode_test.go's
// pattern — the canonical SDK-encode shape used across the source fleet's
// own fixture-building tests) ────────────────────────────────────────────

func prSymbol(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func prScString(s string) xdr.ScVal {
	v := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
}

func prI128(n *big.Int) xdr.ScVal {
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	loBig := new(big.Int).And(n, mask64)
	hiBig := new(big.Int).Rsh(n, 64)
	p := xdr.Int128Parts{Hi: xdr.Int64(hiBig.Int64()), Lo: xdr.Uint64(loBig.Uint64())}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func prScMap(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

func prMakeAccountStrkey(t *testing.T, seedByte byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seedByte
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func prAccountAddr(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, strk)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", strk, err)
	}
	var ed xdr.Uint256
	copy(ed[:], raw)
	scAccount := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &ed}
	scAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &scAccount}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &scAddr}
}

func prB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}
