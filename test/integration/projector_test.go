//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
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
	if err := sink.Add(ctx, withEventTxs(ext)); err != nil {
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

// TestProjectedRebuild_FailedInsertDoesNotCheckpoint is the proof, and
// it is deliberately end-to-end (real ClickHouse + real Postgres) because the
// bug lived in the seam between them.
//
// The old worker discarded pipeline.HandleEvent's error and checkpointed the
// window unconditionally, justified in-comment by "the idempotent ON CONFLICT
// write is retried by re-running the range". That justification was false in
// the tool's DEFAULT mode: -resume=true SKIPS checkpointed windows, so the
// range was never re-run and the row was gone permanently. A single transient
// Postgres error during a multi-hour historical backfill silently dropped a
// row with no unattended path to recovery.
//
// It has to be a NON-TRADE event to reproduce: trade events deliberately
// return nil from HandleEvent (ADR-0041 block-and-retry owns their outcome),
// so only non-trade inserts can surface an error at this seam. rozo.Event ->
// persistRozoEvent returns its insert error, which is why this fixture uses
// it.
//
// Nor can the completeness verdict be relied on as the backstop the code
// pointed operators at: the reconciliation catalogue registers only `trades`
// for some sources, so a dropped non-trade row is invisible to it.
//
// Failure is injected by renaming rozo_events out from under the insert —
// a real, deterministic Postgres error, not a mock.
//
// Proven red: with the old `_ = pipeline.HandleEvent(...)`, run 1
// checkpoints both windows, run 2 skips them, and the rows are never written.
func TestProjectedRebuild_FailedInsertDoesNotCheckpoint(t *testing.T) {
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
	seedRozoPayment(t, ctx, chAddr, contractID, 3050, "tx-d-4444444444444444444444444444444444444444444444444444444444", 1_000_0000000, "dave-memo")
	seedRozoPayment(t, ctx, chAddr, contractID, 3150, "tx-e-5555555555555555555555555555555555555555555555555555555555", 2_000_0000000, "erin-memo")

	registry, err := projector.BuildRegistry([]string{rozo.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("build projector registry: %v", err)
	}
	src := registry.Sources[0]

	opts := func() chops.ProjectedRebuildOptions {
		return chops.ProjectedRebuildOptions{
			Store: store, ChAddr: chAddr, Source: src,
			From: 3000, To: 3199, Window: 100, Workers: 2,
			Write: true, Resume: true, ProgressInterval: time.Hour,
		}
	}

	// ─── Break the insert target, then run ───────────────────────────────
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE rozo_events RENAME TO rozo_events_hidden`); err != nil {
		t.Fatalf("hide rozo_events: %v", err)
	}

	broken, err := chops.RunProjectedRebuild(ctx, opts())
	if err != nil {
		// The run itself still succeeds — the stream completed; only the
		// inserts failed. That is the shape the bug hid inside.
		t.Fatalf("RunProjectedRebuild (broken): %v", err)
	}
	if broken.InsertErrors == 0 {
		t.Fatalf("InsertErrors = 0, want >0 — the failure injection did not take effect")
	}
	if broken.WindowsHeld == 0 {
		t.Fatalf("WindowsHeld = 0 with InsertErrors = %d — the window was checkpointed despite "+
			"losing rows, so a resumed run will skip it and the rows are gone permanently (COR-09)",
			broken.InsertErrors)
	}

	// No checkpoint may exist for a window that lost rows.
	var cursors int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM ingestion_cursors WHERE source = 'projected-rebuild'`).Scan(&cursors); err != nil {
		t.Fatalf("count cursors: %v", err)
	}
	if cursors != 0 {
		t.Fatalf("ingestion_cursors has %d projected-rebuild row(s) after a run that lost every "+
			"insert, want 0 — a resumed run would skip those windows", cursors)
	}

	// ─── Repair, then resume: the held windows must be REDONE ────────────
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE rozo_events_hidden RENAME TO rozo_events`); err != nil {
		t.Fatalf("restore rozo_events: %v", err)
	}

	repaired, err := chops.RunProjectedRebuild(ctx, opts())
	if err != nil {
		t.Fatalf("RunProjectedRebuild (repaired): %v", err)
	}
	if repaired.WindowsSkipped != 0 {
		t.Errorf("WindowsSkipped = %d, want 0 — the previously-held windows must be re-processed, "+
			"not skipped", repaired.WindowsSkipped)
	}
	if repaired.WindowsProcessed != 2 {
		t.Errorf("WindowsProcessed = %d, want 2", repaired.WindowsProcessed)
	}
	if repaired.InsertErrors != 0 || repaired.WindowsHeld != 0 {
		t.Errorf("after repair: InsertErrors=%d WindowsHeld=%d, want 0/0",
			repaired.InsertErrors, repaired.WindowsHeld)
	}

	// The whole point: the rows the first run lost are now actually present.
	assertRozoEventLedgers(t, ctx, store.DB(), []uint32{3050, 3150})
}

// The projector's cycle is a read-modify-write up to PerSourceTimeout long,
// and its commit is a never-regress UPSERT of a position derived from the
// cycle-start read. A projector-replay RewindCursor landing inside that gap
// writes a LOWER value, so the in-flight cycle's forward write would pass the
// guard and put the cursor back at tip — the replay would print success and
// re-project nothing.
//
// RED on the unfixed behaviour: make AdvanceCursorFrom delegate to
// UpsertCursor (the old commit) and all three tests below fail.

const casSource = "sep41_supply"

func casCursor(t *testing.T, ctx context.Context, store *timescale.Store) timescale.Cursor { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	c, err := store.GetCursor(ctx, "projector", casSource)
	if err != nil {
		t.Fatalf("read projector cursor: %v", err)
	}
	return c
}

// TestAdvanceCursorFrom_RewindHeldOpenWinsTheRace interleaves the two
// writers on two connections, both ways round.
func TestAdvanceCursorFrom_RewindHeldOpenWinsTheRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)
	racer := openVerdictStore(t, ctx, dsn) // second pool → second connection

	const (
		readAt   = uint32(63_700_000) // what the cycle read at its start
		commitTo = uint32(63_700_500) // what it derived from that read
		rewindTo = uint32(62_999_999) // projector-replay -from 63000000
	)

	// Seed plainly and go STRAIGHT to the interleave: the sequential contract
	// lives in TestAdvanceCursorFrom_SequentialContract, so that a failure
	// (or a pass) here is a statement about the race and nothing else.
	if err := store.UpsertCursor(ctx, "projector", casSource, readAt); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	// ── Interleave 1: the rewind is in flight when the cycle commits ─────
	tx, err := racer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("racer begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Byte-for-byte RewindCursor's statement, held uncommitted.
	if _, err := tx.ExecContext(ctx, `
        UPDATE ingestion_cursors
           SET last_ledger = $3, last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger > $3`,
		"projector", casSource, int64(rewindTo)); err != nil {
		t.Fatalf("racer rewind: %v", err)
	}

	type result struct {
		advanced bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		ok, aerr := store.AdvanceCursorFrom(ctx, "projector", casSource,
			timescale.CursorRead{Exists: true, LastLedger: readAt}, commitTo)
		done <- result{ok, aerr}
	}()

	// Non-vacuity: the advance must be parked in its cursor-row WRITE behind
	// the rewind's row lock; anything else means the interleave never armed.
	// Deliberately not pinned to the fix's exact statement text — the
	// property is "the cycle's commit is waiting on the rewind", and pinning
	// the text would make a reverted commit fail HERE instead of on the
	// clobber below, which is the assertion that matters.
	parked := waitForVerdictLockWait(t, ctx, racer.DB(), done2finished(done))
	t.Logf("cycle commit parked behind the rewind in: %s", strings.Join(strings.Fields(parked), " "))
	if !strings.Contains(parked, "ingestion_cursors") {
		t.Fatalf("advance parked in the wrong statement — interleave not armed.\nparked in: %s", parked)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("racer commit: %v", err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("advance did not return within 30s of the rewind's commit")
	}
	if res.err != nil {
		t.Fatalf("advance: %v", res.err)
	}
	if res.advanced {
		t.Errorf("advanced=true although the cursor was rewound under the cycle")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != rewindTo {
		t.Fatalf("cursor = %d, want the rewind point %d — the in-flight cycle's stale commit clobbered the rewind (F159)", c.LastLedger, rewindTo)
	}

	// ── Interleave 2: the cycle's commit is in flight when the rewind lands
	// Re-arm: cursor back at readAt via a legitimate advance.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: rewindTo}, readAt); err != nil || !ok {
		t.Fatalf("re-arm advance: advanced=%v err=%v", ok, err)
	}
	tx2, err := racer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("racer begin 2: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	if _, err := tx2.ExecContext(ctx, `
        UPDATE ingestion_cursors SET last_ledger = $3, last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger = $4`,
		"projector", casSource, int64(commitTo), int64(readAt)); err != nil {
		t.Fatalf("racer advance: %v", err)
	}
	type rewound struct {
		prior uint32
		err   error
	}
	rdone := make(chan rewound, 1)
	go func() {
		prior, err := store.RewindCursor(ctx, "projector", casSource, rewindTo)
		rdone <- rewound{prior, err}
	}()
	parked = waitForVerdictLockWait(t, ctx, racer.DB(), done2finished(rdone))
	if !strings.Contains(parked, "UPDATE ingestion_cursors") || !strings.Contains(parked, "last_ledger > $3") {
		t.Fatalf("rewind parked in the wrong statement — interleave not armed.\nparked in: %s", parked)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("racer commit 2: %v", err)
	}
	select {
	case r := <-rdone:
		if r.err != nil {
			t.Fatalf("rewind behind an in-flight advance: %v", r.err)
		}
		// projector-replay widens its dirty window to this value; the
		// snapshot's readAt would under-record the re-walked range.
		if r.prior != commitTo {
			t.Errorf("rewind reported prior ledger %d, want the advanced %d it actually rewound from", r.prior, commitTo)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("rewind did not return within 30s of the advance's commit")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != rewindTo {
		t.Fatalf("cursor = %d, want the rewind point %d — the rewind must win whichever writer holds the row first", c.LastLedger, rewindTo)
	}
}

// TestAdvanceCursorFrom_SequentialContract pins the statement's contract
// with no concurrency involved: the not-found seed (and its DO NOTHING arm),
// the refusal of a non-advancing write, the refusal of a stale read, and
// the half of UpsertCursor's contract the projector's cursor still relies
// on — first_ledger is set on insert and never moved by an advance.
func TestAdvanceCursorFrom_SequentialContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{}, 100); err != nil || !ok {
		t.Fatalf("seed: advanced=%v err=%v", ok, err)
	}
	// A second not-found-read seed must leave the existing row alone.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{}, 5); err != nil || ok {
		t.Fatalf("seed over an existing row: advanced=%v err=%v, want false/nil", ok, err)
	}
	// A write that is not an advance is a caller bug, not a silent no-op.
	if _, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 100); err == nil {
		t.Fatalf("a non-advancing write returned nil error")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != 100 {
		t.Fatalf("cursor = %d after the refused writes, want 100 untouched", c.LastLedger)
	}
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 250); err != nil || !ok {
		t.Fatalf("advance: advanced=%v err=%v", ok, err)
	}
	var first, last int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT first_ledger, last_ledger FROM ingestion_cursors WHERE source = 'projector' AND sub_source = $1`,
		casSource).Scan(&first, &last); err != nil {
		t.Fatalf("read cursor row: %v", err)
	}
	if first != 100 || last != 250 {
		t.Fatalf("first_ledger=%d last_ledger=%d, want 100/250", first, last)
	}
	// A stale read is refused and changes nothing.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 300); err != nil || ok {
		t.Fatalf("stale read: advanced=%v err=%v, want false/nil", ok, err)
	}
	if c := casCursor(t, ctx, store); c.LastLedger != 250 {
		t.Fatalf("cursor = %d after a refused stale advance, want 250", c.LastLedger)
	}
}

// casSink records every ledger the projector sinks and parks the FIRST
// call until released — holding a real cycle open mid-flight, after its
// cursor read and before its commit.
type casSink struct {
	mu       sync.Mutex
	sunk     []uint32
	parkOnce sync.Once
	inFlight chan uint32
	release  chan struct{}
}

func (s *casSink) handle(ctx context.Context, ev consumer.Event) error {
	se, ok := ev.(sep41_supply.Event)
	if !ok {
		return nil
	}
	s.mu.Lock()
	s.sunk = append(s.sunk, se.Ledger)
	s.mu.Unlock()
	var park bool
	s.parkOnce.Do(func() { park = true })
	if park {
		s.inFlight <- se.Ledger
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *casSink) count(ledger uint32) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.sunk {
		if l == ledger {
			n++
		}
	}
	return n
}

// TestProjectorReplayRewind_SurvivesAnInFlightCycle is the finding end to
// end: the REAL projector ([projector.New] + Run) over the REAL store, a
// cycle held open mid-flight, and the REAL [timescale.Store.RewindCursor]
// — the call projector-replay makes — landing from a second connection.
// The repair must actually happen: the rewound ledger is sunk again.
func TestProjectorReplayRewind_SurvivesAnInFlightCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)
	replay := openVerdictStore(t, ctx, dsn) // the ops command's own connection

	const (
		rewoundLedger  = uint32(50_000_010) // already projected; the replay wants it re-driven
		inFlightLedger = uint32(50_000_020) // what the live cycle is sinking when the rewind lands
	)
	rowA := mkReconstructableRow(t, rewoundLedger)
	rowB := mkReconstructableRow(t, inFlightLedger)
	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{rowA, rowB}); err != nil {
		t.Fatalf("seed soroban_events: %v", err)
	}
	if err := store.UpsertCursor(ctx, "ledgerstream", "", inFlightLedger); err != nil {
		t.Fatalf("seed ledgerstream cursor: %v", err)
	}
	// The projector has already walked past rewoundLedger.
	if err := store.UpsertCursor(ctx, "projector", casSource, inFlightLedger-1); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	sink := &casSink{inFlight: make(chan uint32, 1), release: make(chan struct{})}
	reg := projector.Registry{Sources: []projector.Source{{Name: casSource, Decoder: &fakeSupplyDecoder{contractID: rowA.ContractID}}}}
	p := projector.New(store, reg, sink.handle, slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = p.Run(runCtx)
	}()
	var releaseOnce sync.Once
	releaseSink := func() { releaseOnce.Do(func() { close(sink.release) }) }
	t.Cleanup(func() {
		releaseSink()
		runCancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("projector Run did not exit within 30s of cancel")
		}
	})

	// The cycle is now provably mid-flight: it read cursor = inFlight-1 and
	// is inside its sink call for inFlightLedger.
	select {
	case got := <-sink.inFlight:
		if got != inFlightLedger {
			t.Fatalf("the parked cycle is sinking ledger %d, want %d — interleave not armed", got, inFlightLedger)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("projector never reached the sink")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != inFlightLedger-1 {
		t.Fatalf("cursor = %d while the cycle is parked, want %d (uncommitted)", c.LastLedger, inFlightLedger-1)
	}

	// projector-replay -from rewoundLedger.
	if _, err := replay.RewindCursor(ctx, "projector", casSource, rewoundLedger-1); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	releaseSink() // the in-flight cycle now runs to its commit

	// The re-projection must happen. In the fixed code the re-walk sinks
	// rewoundLedger BEFORE the cursor can read inFlightLedger again, so a
	// cursor at inFlightLedger with rewoundLedger never sunk is the clobber.
	deadline := time.Now().Add(2*projectorSettle + 30*time.Second)
	for sink.count(rewoundLedger) == 0 {
		cur := casCursor(t, ctx, store).LastLedger
		if cur == inFlightLedger && sink.count(rewoundLedger) == 0 {
			t.Fatalf("the cursor is back at %d and ledger %d was never re-sunk — the in-flight cycle's stale commit reverted projector-replay's rewind, so the repair no-oped (F159)", cur, rewoundLedger)
		}
		if time.Now().After(deadline) {
			t.Fatalf("ledger %d was never re-projected after the rewind (cursor=%d)", rewoundLedger, cur)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// And the projector then catches back up, re-driving the in-flight
	// ledger too (idempotent downstream).
	for casCursor(t, ctx, store).LastLedger != inFlightLedger {
		if time.Now().After(deadline) {
			t.Fatalf("cursor never returned to %d after the re-walk", inFlightLedger)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := sink.count(inFlightLedger); n < 2 {
		t.Errorf("ledger %d sunk %d time(s), want ≥2 (once in flight, once in the re-walk)", inFlightLedger, n)
	}
}

// seekCase is one filter shape the projector's first-event seek must answer
// exactly as the matching stream would.
type seekCase struct {
	name                        string
	from, to                    uint32
	contracts, topics, excludes []string
	want                        uint32
	wantFound                   bool
}

// TestFirstSorobanEventLedger_MatchesStream executes the Postgres seek the
// projector seeds a never-run source from, and pins it to StreamSorobanEvents'
// row set for the same filters (the seed must never skip a row the scan would
// have returned).
func TestFirstSorobanEventLedger_MatchesStream(t *testing.T) {
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
	// mkSyntheticRow: the seed picks the contract; odd seeds carry a NULL
	// topic_0_sym, even seeds "synthetic_event".
	rows := []sorobanevents.Row{
		mkSyntheticRow(t, 1000, t0, 1),
		mkSyntheticRow(t, 1100, t0.Add(time.Second), 2),
		mkSyntheticRow(t, 1200, t0.Add(2*time.Second), 4),
		mkSyntheticRow(t, 1300, t0.Add(3*time.Second), 1),
	}
	if err := store.InsertSorobanEventsBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}
	c1, c2, c4 := rows[0].ContractID, rows[1].ContractID, rows[2].ContractID

	for _, tc := range []seekCase{
		{name: "unfiltered", to: 2000, want: 1000, wantFound: true},
		{name: "contract", to: 2000, contracts: []string{c2}, want: 1100, wantFound: true},
		{name: "contract above from", from: 1001, to: 2000, contracts: []string{c1}, want: 1300, wantFound: true},
		{name: "topic", to: 2000, topics: []string{"synthetic_event"}, want: 1100, wantFound: true},
		{name: "exclude keeps NULL topic", to: 2000, excludes: []string{"synthetic_event"}, want: 1000, wantFound: true},
		{name: "exclude above from", from: 1001, to: 2000, excludes: []string{"synthetic_event"}, want: 1300, wantFound: true},
		{name: "all filters", to: 2000, contracts: []string{c2, c4}, topics: []string{"synthetic_event"}, excludes: []string{"other"}, want: 1100, wantFound: true},
		{name: "below first row", to: 999},
		{name: "to bounds the seek", to: 1199, contracts: []string{c4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := store.FirstSorobanEventLedger(ctx, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes)
			if err != nil {
				t.Fatalf("FirstSorobanEventLedger: %v", err)
			}
			if found != tc.wantFound || got != tc.want {
				t.Fatalf("FirstSorobanEventLedger = (%d, %v), want (%d, %v)", got, found, tc.want, tc.wantFound)
			}
			var streamFirst uint32
			streamFound := false
			if err := store.StreamSorobanEvents(ctx, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes,
				func(r sorobanevents.Row) error {
					if !streamFound || r.Ledger < streamFirst {
						streamFirst, streamFound = r.Ledger, true
					}
					return nil
				}); err != nil {
				t.Fatalf("StreamSorobanEvents: %v", err)
			}
			if streamFound != found || streamFirst != got {
				t.Fatalf("seek (%d, %v) disagrees with stream's first row (%d, %v)", got, found, streamFirst, streamFound)
			}
		})
	}
}

// TestFirstContractEventLedgerFiltered_MatchesStream is the ClickHouse
// (feed-switch) twin of the Postgres seek test.
func TestFirstContractEventLedgerFiltered_MatchesStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	chAddr := clickhouseAddr(t)

	// A ledger range and contracts no other test writes, since the CH
	// container is shared across the package.
	const base = 7_340_000
	contractA, contractB := seekContract(t, 0xA1), seekContract(t, 0xB2)
	seekSeedEvent(t, ctx, chAddr, base+100, contractA, "swap")
	seekSeedEvent(t, ctx, chAddr, base+200, contractB, "transfer")
	seekSeedEvent(t, ctx, chAddr, base+300, contractB, "swap")

	both := []string{contractA, contractB}
	for _, tc := range []seekCase{
		{name: "contract", from: base, to: base + 1000, contracts: []string{contractB}, want: base + 200, wantFound: true},
		{name: "contract and topic", from: base, to: base + 1000, contracts: []string{contractB}, topics: []string{"swap"}, want: base + 300, wantFound: true},
		{name: "exclude", from: base + 101, to: base + 1000, contracts: both, excludes: []string{"transfer"}, want: base + 300, wantFound: true},
		{name: "above from", from: base + 101, to: base + 1000, contracts: both, want: base + 200, wantFound: true},
		{name: "to bounds the seek", from: base, to: base + 199, contracts: []string{contractB}},
		{name: "no match", from: base, to: base + 1000, contracts: []string{contractA}, topics: []string{"transfer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := chstore.FirstContractEventLedgerFiltered(ctx, chAddr, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes)
			if err != nil {
				t.Fatalf("FirstContractEventLedgerFiltered: %v", err)
			}
			if found != tc.wantFound || got != tc.want {
				t.Fatalf("FirstContractEventLedgerFiltered = (%d, %v), want (%d, %v)", got, found, tc.want, tc.wantFound)
			}
			var streamFirst uint32
			streamFound := false
			if err := chstore.StreamContractEventsFiltered(ctx, chAddr, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes,
				false, false, false, func(ev events.Event) error {
					if !streamFound || ev.Ledger < streamFirst {
						streamFirst, streamFound = ev.Ledger, true
					}
					return nil
				}); err != nil {
				t.Fatalf("StreamContractEventsFiltered: %v", err)
			}
			if streamFound != found || streamFirst != got {
				t.Fatalf("seek (%d, %v) disagrees with stream's first row (%d, %v)", got, found, streamFirst, streamFound)
			}
		})
	}
}

func seekContract(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0], raw[1] = seed, 0x5E
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func seekSeedEvent(t *testing.T, ctx context.Context, chAddr string, ledger uint32, contract, topic string) {
	t.Helper()
	closeTime := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC).Add(time.Duration(ledger) * 5 * time.Second)
	sink, err := chstore.Open(ctx, chAddr, 100)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer func() { _ = sink.Close(ctx) }()
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "aa", PrevHash: "bb",
			ProtocolVersion: 22, BucketListHash: "cc", TxCount: 1, OpCount: 1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq:        ledger,
			CloseTime:        closeTime,
			TxHash:           fmt.Sprintf("%064x", ledger),
			ContractID:       contract,
			EventType:        "contract",
			TopicCount:       1,
			Topic0Sym:        topic,
			TopicsXDR:        []string{prB64(t, prSymbol(topic))},
			DataXDR:          prB64(t, prSymbol("x")),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, withEventTxs(ext)); err != nil {
		t.Fatalf("sink add (ledger %d): %v", ledger, err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush (ledger %d): %v", ledger, err)
	}
}

// TestProjectorSinkDurability_TransientFailureDoesNotAdvanceCursor is the
// proven-red test for the projector advancing
// its cursor past a silently-swallowed SINK write failure, which permanently
// drops the row for the sole-writer sep41 domain.
//
// It drives the REAL projector ([projector.New] + [projector.Run]) reading
// one seeded soroban_events row, with a sink that:
//   - FAILS the first cycle's write with a transient Postgres fault (a
//     deadlock, SQLSTATE 40P01 — the transient class an old sink swallowed), then
//   - SUCCEEDS on the retry, delegating to the production
//     [pipeline.HandleEvent] so the row lands for real.
//
// It asserts the two properties the fix must guarantee:
//
//	(a) after the transient failure the projector cursor did NOT advance past
//	    the failing ledger (so the event is not lost); and
//	(b) the next cycle re-reads that ledger and the row LANDS.
//
// RED on the unfixed code: revert cycleOneSource to advance the cursor to
// `toLedger` UNCONDITIONALLY (ignoring the sink error) — keeping the
// SinkFunc/HandleEvent error signatures — and assertion (a) fails (the cursor
// jumps past the failing ledger) and (b) fails (the idle next cycle never
// re-reads it, so the row is permanently lost). This mirrors the silent-loss
// path: `-resume` skips it, reconcile re-sums the equally-short table,
// and obs reports it as `ok`.
func TestProjectorSinkDurability_TransientFailureDoesNotAdvanceCursor(t *testing.T) {
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
		srcName = "sep41_supply"
		ledger  = uint32(50_000_010)
	)

	// Seed one soroban_events row at `ledger`. The fake decoder below ignores
	// its contents (it always emits one sep41 mint), so the row only needs to
	// Reconstruct cleanly — hence OpArgsXDR is nil (random op-args would fail
	// scval decode and soft-fail as a decode error instead of reaching the
	// sink).
	row := mkReconstructableRow(t, ledger)
	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{row}); err != nil {
		t.Fatalf("seed soroban_events: %v", err)
	}

	// Tip: the projector never scans past the live ledgerstream cursor. Set it
	// AT `ledger` so [ledger, ledger] is the exact scan window.
	if err := store.UpsertCursor(ctx, "ledgerstream", "", ledger); err != nil {
		t.Fatalf("seed ledgerstream cursor: %v", err)
	}
	// Projector cursor starts one BELOW `ledger`, so fromLedger = ledger and
	// the single row is in-window on the first cycle.
	if err := store.UpsertCursor(ctx, "projector", srcName, ledger-1); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	// Decoder emits exactly one sep41 mint per matched row, keyed to the row's
	// ledger/tx so the write is a real, valid sep41_supply_events row.
	contractID := row.ContractID
	dec := &fakeSupplyDecoder{contractID: contractID}

	sink := &durabilitySink{
		store:    store,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		failNext: true, // fail the first write transiently
		called:   make(chan struct{}, 8),
	}

	reg := projector.Registry{Sources: []projector.Source{{Name: srcName, Decoder: dec}}}
	p := projector.New(store, reg, sink.handle, slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = p.Run(runCtx)
	}()
	t.Cleanup(func() {
		runCancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("projector Run did not exit within 30s of cancel")
		}
	})

	readCursor := func() uint32 {
		c, err := store.GetCursor(ctx, "projector", srcName)
		if err != nil {
			t.Fatalf("read projector cursor: %v", err)
		}
		return c.LastLedger
	}
	countRows := func() int {
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM sep41_supply_events WHERE contract_id = $1`, contractID).Scan(&n); err != nil {
			t.Fatalf("count sep41_supply_events: %v", err)
		}
		return n
	}

	// ── Cycle 1: the transient sink failure ──────────────────────────────
	select {
	case <-sink.called:
	case <-time.After(30 * time.Second):
		t.Fatal("projector never invoked the sink on the first cycle")
	}
	// Let the cycle finish its post-sink cursor decision. On the UNFIXED code
	// the unconditional UpsertCursor(toLedger) runs synchronously right after
	// the sink returns, so a 500ms settle reliably catches the bad advance.
	time.Sleep(500 * time.Millisecond)

	if got := readCursor(); got != ledger-1 {
		t.Fatalf("after a TRANSIENT sink failure the cursor advanced to %d, want %d "+
			"(C2-1: the projector must NOT advance past a ledger whose write failed transiently — "+
			"the unfixed code jumps to toLedger and permanently drops the sep41 row)", got, ledger-1)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("sep41_supply_events has %d rows after the failed write, want 0", n)
	}

	// ── Cycle 2+: the retry lands the row ────────────────────────────────
	sink.mu.Lock()
	sink.failNext = false // let the next write succeed
	sink.mu.Unlock()

	// The next cycle (Interval later) re-reads `ledger` and commits it.
	deadline := time.Now().Add(2*projectorSettle + 30*time.Second)
	for {
		if readCursor() == ledger {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor never reached %d after the fault cleared — the row was NOT retried "+
				"(C2-1: a held cursor must re-read the failing ledger next cycle)", ledger)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("sep41_supply_events has %d rows after the retry, want 1 (the dropped mint must land)", n)
	}
}

// projectorSettle pads the retry deadline by more than one projector Interval
// so the ticker-driven second cycle has time to run.
const projectorSettle = 5 * time.Second

// durabilitySink is the projector [projector.SinkFunc]: it fails the first
// write transiently (a deadlock), then delegates real writes to the
// production [pipeline.HandleEvent].
type durabilitySink struct {
	mu       sync.Mutex
	failNext bool
	store    *timescale.Store
	logger   *slog.Logger
	called   chan struct{}
}

func (s *durabilitySink) handle(ctx context.Context, ev consumer.Event) error {
	s.mu.Lock()
	fail := s.failNext
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	if fail {
		// A transient Postgres fault mid-cycle (deadlock_detected, SQLSTATE
		// 40P01) — precisely the class a swallowing sink would lose.
		// timescale.IsPermanentDataError classifies it as transient, so the
		// projector must HOLD its cursor and retry rather than skip.
		return &pgconn.PgError{Code: "40P01", Message: "deadlock detected (injected)"}
	}
	// Real production write path — exercises HandleEvent's new error return.
	return pipeline.HandleEvent(ctx, s.logger, s.store, ev)
}

// fakeSupplyDecoder matches every reconstructed row and emits exactly one
// sep41 mint keyed to the row, so the projector's sink writes a valid
// sep41_supply_events row through the real HandleEvent path.
type fakeSupplyDecoder struct{ contractID string }

func (fakeSupplyDecoder) Name() string              { return "sep41_supply" }
func (fakeSupplyDecoder) Matches(events.Event) bool { return true }
func (d *fakeSupplyDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{sep41_supply.Event{
		ContractID:   d.contractID,
		Ledger:       ev.Ledger,
		TxHash:       ev.TxHash,
		OpIndex:      uint32(ev.OperationIndex), //nolint:gosec // test data, small
		EventIndex:   uint32(ev.EventIndex),     //nolint:gosec // test data, small
		ObservedAt:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Kind:         sep41_supply.SymbolMint,
		Amount:       big.NewInt(1000),
		Counterparty: "GCOUNTERPARTY00000000000000000000000000000000000000000000",
	}}, nil
}

// mkReconstructableRow builds one soroban_events row that
// sorobanevents.Reconstruct accepts (valid contract strkey, 32-byte tx hash,
// non-empty topic-0 + body) and that carries NO op-args (so Reconstruct does
// not attempt to scval-decode random bytes).
func mkReconstructableRow(t *testing.T, ledger uint32) sorobanevents.Row {
	t.Helper()
	var cid [32]byte
	cid[0] = 0x11
	cid[1] = 0xAA
	cstrk, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	txh := make([]byte, 32)
	for i := range txh {
		txh[i] = 0x22
	}
	_ = hex.EncodeToString(txh) // Reconstruct hex-encodes TxHash into ev.TxHash
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		TxHash:          txh,
		OpIndex:         0,
		EventIndex:      0,
		ContractID:      cstrk,
		ContractIDHex:   cid[:],
		TopicCount:      1,
		Topic0Sym:       "mint",
		Topic0XDR:       []byte{0x00, 0x01, 0x02, 0x03},
		Topic1XDR:       nil,
		Topic2XDR:       nil,
		Topic3XDR:       nil,
		BodyXDR:         []byte{0x04, 0x05, 0x06, 0x07},
		OpArgsXDR:       nil,
	}
}

const testPassphrase = "Test SDF Network ; September 2015"

// TestEndToEnd_LedgerstreamToTimescale proves the full production
// ingest path works end-to-end against real infrastructure:
//
//	Galexie-shaped .xdr.zst on disk
//	  → internal/ledgerstream
//	  → internal/dispatcher (all decoders registered)
//	  → consumer.Event type-switch
//	  → internal/storage/timescale
//	  → Timescale row lands
//	  → cursor persisted
//
// The test uses the SDK's filesystem datastore (not MinIO) — the
// S3 transport is a separate concern tested in internal/ledgerstream
// (and by the r1 smoke once 165d is deployed). What's proved here
// is the wiring that cmd/stellarindex-indexer runs in production.
//
// Ledger fixtures are constructed in-test using the SDK's
// compressxdr helpers, mirroring what Galexie writes. Two
// sub-tests:
//
//  1. plumbing — bounded range of empty ledgers: the pipeline
//     handles zero-event ledgers without errors, the cursor
//     advances across all of them, and the dispatcher returns no
//     outputs.
//  2. richer fixture — a soroban-flagged envelope carrying a real
//     Reflector FX update event, whose OracleUpdate lands in
//     Timescale with the expected asset/price/timestamp. Exercises
//     the full chain: envelope hash matching → TxMetaV3.SorobanMeta
//     → tx.GetTransactionEvents() → dispatcher routing →
//     reflector decoder → timescale.InsertOracleUpdate →
//     LatestOracleUpdateForAsset round-trip.
//
// A future extension (a real LCM carrying a Soroban trade event
// through the Soroswap/Phoenix/Aquarius paths) would add
// correlation-buffer coverage; the fixture machinery for it is
// the same shape as (2).
func TestEndToEnd_LedgerstreamToTimescale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t.Run("bounded range of empty ledgers", func(t *testing.T) {
		dsDir := t.TempDir()
		// Note: SDK's BoundedRange(from, to) requires to > from,
		// so we seed + request at least 2 ledgers. Production
		// always uses unbounded (to=0); bounded ranges only show
		// up in backfill CLIs or this integration test.
		seqs := []uint32{62_000_100, 62_000_101, 62_000_102}
		seedEmptyLedgers(t, ctx, dsDir, seqs)

		disp := newFullDispatcher(t)
		lsCfg := filesystemLedgerstreamConfig(dsDir)

		events, processed, cursor := runIngest(
			ctx, t, disp, lsCfg, store,
			seqs[0], seqs[len(seqs)-1],
		)

		if processed != uint32(len(seqs)) {
			t.Errorf("processed %d ledgers, want %d", processed, len(seqs))
		}
		if events != 0 {
			t.Errorf("got %d events from empty ledgers, want 0", events)
		}
		if cursor.LastLedger != seqs[len(seqs)-1] {
			t.Errorf("cursor didn't advance to last ledger: got %d want %d",
				cursor.LastLedger, seqs[len(seqs)-1])
		}
	})

	// This is the "richer" fixture test promised in the file-level
	// doc: construct a real-shaped Soroban transaction carrying a
	// Reflector FX update event, drive it all the way through the
	// production pipeline, and verify an OracleUpdate row lands.
	//
	// Building blocks, top-down:
	//
	//  1. xdr.ContractEvent with Reflector's exact wire shape
	//     (topic[0]=Symbol("REFLECTOR"), topic[1]=Symbol("update"),
	//     topic[2]=U64 timestamp_ms, body=Map{"update_data": Vec<
	//     (Symbol("EUR"), i128)>}).
	//  2. xdr.TransactionEnvelope flagged Soroban
	//     (Ext.V=1 + SorobanData set) — IsSorobanTx() must return
	//     true so the SDK reaches TxMetaV3.SorobanMeta.Events.
	//  3. xdr.LedgerCloseMeta V1 with the envelope in a V0Components
	//     phase + matching TxProcessing (result hash = envelope hash,
	//     TxApplyProcessing = TransactionMetaV3 with the event).
	//  4. Seed into the filesystem datastore, run the existing
	//     runIngest, then query Timescale for the landed row.
	t.Run("soroban LCM with reflector FX update lands OracleUpdate", func(t *testing.T) {
		dsDir := t.TempDir()

		const fxContract = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
		fxContractID := mustContractIDFromStrkey(t, fxContract)

		// Reflector publishes timestamps in ms; pick a deterministic
		// instant inside the bounded range so the OracleUpdate row we
		// assert against is easy to diff.
		const tsMs = uint64(1_745_123_456_000) // 2026-04-20T05:50:56Z
		closedAt := time.UnixMilli(int64(tsMs)).UTC()

		// One (asset, price) pair: EUR at 1.0 × 10^14 (the canonical
		// Reflector scale).
		eurPrice := big.NewInt(100_000_000_000_000)
		ev := buildReflectorFXContractEvent(t, fxContractID, tsMs,
			[]string{"EUR"}, []*big.Int{eurPrice})

		// Bounded range ≥ 2 ledgers. Second ledger is empty — the
		// meaningful assertion is that the first ledger's event lands
		// and the cursor advances past it.
		seqs := []uint32{62_100_100, 62_100_101}
		seedSorobanLedger(t, ctx, dsDir, seqs[0], closedAt, ev)
		seedEmptyLedgers(t, ctx, dsDir, seqs[1:])

		// Dispatcher with just the FX decoder registered — scopes the
		// test to exactly what we're proving. WithDecoderObserver
		// stamps a known G-strkey on the row so we can assert on it.
		// AQUA mainnet issuer — chosen because it's a real, stable
		// G-strkey that survives the strkey-CRC validation that the
		// canonical package now enforces (the prior hand-crafted
		// observer strkey "GA7QYN…" had an invalid checksum).
		const observerStrkey = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		fxDecoder := reflector.NewDecoder(reflector.VariantFX, fxContract,
			reflector.WithDecoderObserver(observerStrkey))
		disp := dispatcher.New(fxDecoder)
		lsCfg := filesystemLedgerstreamConfig(dsDir)

		events, processed, cursor := runIngest(
			ctx, t, disp, lsCfg, store,
			seqs[0], seqs[len(seqs)-1],
		)

		if processed != uint32(len(seqs)) {
			t.Errorf("processed %d ledgers, want %d", processed, len(seqs))
		}
		if events != 1 {
			t.Errorf("got %d events, want 1", events)
		}
		if cursor.LastLedger != seqs[len(seqs)-1] {
			t.Errorf("cursor didn't advance: got %d want %d",
				cursor.LastLedger, seqs[len(seqs)-1])
		}

		eur, err := canonical.NewFiatAsset("EUR")
		if err != nil {
			t.Fatalf("NewFiatAsset(EUR): %v", err)
		}
		got, err := store.LatestOracleUpdateForAsset(ctx, reflector.SourceFX, eur)
		if err != nil {
			t.Fatalf("LatestOracleUpdateForAsset: %v", err)
		}
		if got.Source != reflector.SourceFX {
			t.Errorf("Source = %q want %q", got.Source, reflector.SourceFX)
		}
		if got.ContractID != fxContract {
			t.Errorf("ContractID = %q want %q", got.ContractID, fxContract)
		}
		if got.Ledger != seqs[0] {
			t.Errorf("Ledger = %d want %d", got.Ledger, seqs[0])
		}
		if got.Price.BigInt().Cmp(eurPrice) != 0 {
			t.Errorf("Price = %s want %s", got.Price, eurPrice)
		}
		if got.Decimals != reflector.DefaultDecimals {
			t.Errorf("Decimals = %d want %d", got.Decimals, reflector.DefaultDecimals)
		}
		if got.Timestamp.UnixMilli() != int64(tsMs) {
			t.Errorf("Timestamp = %v (ms=%d) want ms=%d",
				got.Timestamp, got.Timestamp.UnixMilli(), tsMs)
		}
		if got.Observer != observerStrkey {
			t.Errorf("Observer = %q want %q", got.Observer, observerStrkey)
		}
	})

	// Redstone subtest — proves the OpArgs pathway added with the Redstone decoder:
	// the dispatcher extracts InvokeContract args from the envelope
	// and the Redstone decoder zips them against event body entries.
	// Without OpArgs the decoder has no feed_ids and can't attribute
	// prices to assets, so a passing test confirms both the wire-up
	// and the decoder logic together.
	t.Run("soroban LCM with redstone write_prices lands OracleUpdates", func(t *testing.T) {
		dsDir := t.TempDir()

		const adapterC = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
		adapterID := mustContractIDFromStrkey(t, adapterC)

		// Two known feeds: BTC + ETH, both in canonical.IsKnownCrypto.
		// Prices at the Redstone 8-decimal scale.
		const pkgTs = uint64(1_745_000_000_000) // ms
		const wrTs = uint64(1_745_000_060_000)
		btcPriceE8 := big.NewInt(50_000_000_000_000) // $500k
		ethPriceE8 := big.NewInt(3_500_000_000_000)  // $35k

		// Relayer strkey from a deterministic 32-byte seed — skips the
		// checksum-drift trap we hit writing redstone's unit tests.
		relayerSeed := [32]byte{
			0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42,
			0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42,
			0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42,
			0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42,
		}
		relayerG, err := strkey.Encode(strkey.VersionByteAccountID, relayerSeed[:])
		if err != nil {
			t.Fatalf("encode relayer strkey: %v", err)
		}

		ev := buildRedstoneWritePricesEvent(t, adapterID, relayerG,
			[]*big.Int{btcPriceE8, ethPriceE8}, pkgTs, wrTs)

		seqs := []uint32{62_200_100, 62_200_101}
		seedRedstoneLedger(t, ctx, dsDir, seqs[0],
			time.UnixMilli(int64(wrTs)).UTC(),
			adapterID, relayerG, []string{"BTC", "ETH"}, ev)
		seedEmptyLedgers(t, ctx, dsDir, seqs[1:])

		disp := dispatcher.New(redstone.NewDecoder(adapterC))
		lsCfg := filesystemLedgerstreamConfig(dsDir)

		events, processed, cursor := runIngest(
			ctx, t, disp, lsCfg, store,
			seqs[0], seqs[len(seqs)-1],
		)

		if processed != uint32(len(seqs)) {
			t.Errorf("processed %d ledgers, want %d", processed, len(seqs))
		}
		if events != 2 {
			t.Errorf("got %d events, want 2 (BTC+ETH)", events)
		}
		if cursor.LastLedger != seqs[len(seqs)-1] {
			t.Errorf("cursor didn't advance: got %d want %d",
				cursor.LastLedger, seqs[len(seqs)-1])
		}

		btc, err := canonical.NewCryptoAsset("BTC")
		if err != nil {
			t.Fatalf("NewCryptoAsset(BTC): %v", err)
		}
		gotBTC, err := store.LatestOracleUpdateForAsset(ctx, redstone.SourceName, btc)
		if err != nil {
			t.Fatalf("LatestOracleUpdateForAsset(BTC): %v", err)
		}
		if gotBTC.Price.BigInt().Cmp(btcPriceE8) != 0 {
			t.Errorf("BTC price = %s want %s", gotBTC.Price, btcPriceE8)
		}
		if gotBTC.Decimals != 8 {
			t.Errorf("BTC decimals = %d want 8", gotBTC.Decimals)
		}
		if gotBTC.Timestamp.UnixMilli() != int64(pkgTs) {
			t.Errorf("BTC ts = %d ms want %d", gotBTC.Timestamp.UnixMilli(), pkgTs)
		}
		if gotBTC.Observer != relayerG {
			t.Errorf("BTC observer = %q want %q", gotBTC.Observer, relayerG)
		}

		eth, err := canonical.NewCryptoAsset("ETH")
		if err != nil {
			t.Fatalf("NewCryptoAsset(ETH): %v", err)
		}
		gotETH, err := store.LatestOracleUpdateForAsset(ctx, redstone.SourceName, eth)
		if err != nil {
			t.Fatalf("LatestOracleUpdateForAsset(ETH): %v", err)
		}
		if gotETH.Price.BigInt().Cmp(ethPriceE8) != 0 {
			t.Errorf("ETH price = %s want %s", gotETH.Price, ethPriceE8)
		}
	})

	// Comet subtest — weighted-AMM SwapEvent landing a canonical.Trade.
	// Simpler than Redstone: no OpArgs dependency (tokens live in the
	// event body by field name), no correlation buffer (single-event
	// decode per swap, unlike Soroswap's swap+sync pairing).
	t.Run("soroban LCM with comet POOL.swap lands Trade", func(t *testing.T) {
		dsDir := t.TempDir()

		// Pool contract ID — MUST be the curated backstop pool: the
		// contract-identity gate (ADR-0035/0040) applies, and comet
		// Matches() rejects any emitter outside comet.MainnetGatedSet, so a
		// synthetic well-formed address would not land a Trade. Using the
		// production curated pool also keeps the gate itself in the exercised path.
		poolRaw, err := strkey.Decode(strkey.VersionByteContract, comet.MainnetBackstopPool)
		if err != nil {
			t.Fatalf("decode curated pool strkey: %v", err)
		}
		var poolID xdr.ContractId
		copy(poolID[:], poolRaw)

		// Two Soroban tokens (base+quote) with deterministic seeds so
		// NewSorobanAsset succeeds and the canonical.Pair round-trips.
		tokenInSeed := [32]byte{0x10}
		for i := 1; i < 32; i++ {
			tokenInSeed[i] = byte(0x10) ^ byte(i)
		}
		tokenOutSeed := [32]byte{0x20}
		for i := 1; i < 32; i++ {
			tokenOutSeed[i] = byte(0x20) ^ byte(i)
		}
		tokenInStrkey, err := strkey.Encode(strkey.VersionByteContract, tokenInSeed[:])
		if err != nil {
			t.Fatalf("encode tokenIn: %v", err)
		}
		tokenOutStrkey, err := strkey.Encode(strkey.VersionByteContract, tokenOutSeed[:])
		if err != nil {
			t.Fatalf("encode tokenOut: %v", err)
		}

		// Caller (trader): G-strkey.
		callerSeed := [32]byte{0x30}
		for i := 1; i < 32; i++ {
			callerSeed[i] = byte(0x30) ^ byte(i)
		}
		callerStrkey, err := strkey.Encode(strkey.VersionByteAccountID, callerSeed[:])
		if err != nil {
			t.Fatalf("encode caller: %v", err)
		}

		amountIn := big.NewInt(1_000_000_000)   // 1.0 at 9 dec
		amountOut := big.NewInt(42_500_000_000) // 42.5 at 9 dec

		ev := buildCometSwapEvent(t, poolID,
			callerStrkey, tokenInStrkey, tokenOutStrkey,
			amountIn, amountOut)

		seqs := []uint32{62_300_100, 62_300_101}
		closedAt := time.Unix(1_745_000_200, 0).UTC()
		seedSorobanLedger(t, ctx, dsDir, seqs[0], closedAt, ev)
		seedEmptyLedgers(t, ctx, dsDir, seqs[1:])

		disp := dispatcher.New(comet.NewDecoder())
		lsCfg := filesystemLedgerstreamConfig(dsDir)

		events, processed, cursor := runIngest(
			ctx, t, disp, lsCfg, store,
			seqs[0], seqs[len(seqs)-1],
		)
		if processed != uint32(len(seqs)) {
			t.Errorf("processed %d ledgers, want %d", processed, len(seqs))
		}
		if events != 1 {
			t.Errorf("got %d events, want 1", events)
		}
		if cursor.LastLedger != seqs[len(seqs)-1] {
			t.Errorf("cursor didn't advance: got %d want %d",
				cursor.LastLedger, seqs[len(seqs)-1])
		}

		base, err := canonical.NewSorobanAsset(tokenInStrkey)
		if err != nil {
			t.Fatalf("NewSorobanAsset(base): %v", err)
		}
		quote, err := canonical.NewSorobanAsset(tokenOutStrkey)
		if err != nil {
			t.Fatalf("NewSorobanAsset(quote): %v", err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		trades, err := store.LatestTradesForPair(ctx, pair, 10)
		if err != nil {
			t.Fatalf("LatestTradesForPair: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("expected 1 trade, got %d", len(trades))
		}
		got := trades[0]
		if got.Source != comet.SourceName {
			t.Errorf("Source = %q want %q", got.Source, comet.SourceName)
		}
		if got.BaseAmount.BigInt().Cmp(amountIn) != 0 {
			t.Errorf("BaseAmount = %s want %s", got.BaseAmount, amountIn)
		}
		if got.QuoteAmount.BigInt().Cmp(amountOut) != 0 {
			t.Errorf("QuoteAmount = %s want %s", got.QuoteAmount, amountOut)
		}
		if got.Taker != callerStrkey {
			t.Errorf("Taker = %q want %q", got.Taker, callerStrkey)
		}
		if got.Ledger != seqs[0] {
			t.Errorf("Ledger = %d want %d", got.Ledger, seqs[0])
		}
	})

	// Band subtest — proves the ContractCallDecoder pathway. This is
	// the "no events" case: Band's StandardReference doesn't emit on
	// relay(), so the dispatcher routes purely on the InvokeContract
	// op itself. If the dispatcher's contract-call loop skips
	// argless/event-only ops, this test catches that.
	t.Run("soroban LCM with band relay (no events) lands OracleUpdates", func(t *testing.T) {
		dsDir := t.TempDir()

		const bandContract = "CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M"
		bandContractID := mustContractIDFromStrkey(t, bandContract)

		relayerSeed := [32]byte{
			0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
			0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
			0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
			0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
		}
		relayerStrkey, err := strkey.Encode(strkey.VersionByteAccountID, relayerSeed[:])
		if err != nil {
			t.Fatalf("encode relayer strkey: %v", err)
		}

		const resolveSec = uint64(1_745_000_300)
		const btcRateE9 = uint64(500_000_000_000_000) // $500k × 10^9
		const xlmRateE9 = uint64(120_000_000)         // $0.12 × 10^9

		seqs := []uint32{62_400_100, 62_400_101}
		closedAt := time.Unix(int64(resolveSec), 0).UTC()
		seedBandRelayLedger(t, ctx, dsDir, seqs[0], closedAt, bandContractID,
			relayerStrkey, []bandRate{{"BTC", btcRateE9}, {"XLM", xlmRateE9}}, resolveSec, 99)
		seedEmptyLedgers(t, ctx, dsDir, seqs[1:])

		disp := dispatcher.New() // no event decoders
		disp.AddContractCallDecoder(band.NewDecoder(bandContract))
		lsCfg := filesystemLedgerstreamConfig(dsDir)

		events, processed, cursor := runIngest(
			ctx, t, disp, lsCfg, store,
			seqs[0], seqs[len(seqs)-1],
		)
		if processed != uint32(len(seqs)) {
			t.Errorf("processed %d ledgers, want %d", processed, len(seqs))
		}
		if events != 2 {
			t.Errorf("got %d events, want 2 (BTC+XLM)", events)
		}
		if cursor.LastLedger != seqs[len(seqs)-1] {
			t.Errorf("cursor didn't advance: got %d want %d",
				cursor.LastLedger, seqs[len(seqs)-1])
		}

		btc, err := canonical.NewCryptoAsset("BTC")
		if err != nil {
			t.Fatalf("NewCryptoAsset(BTC): %v", err)
		}
		gotBTC, err := store.LatestOracleUpdateForAsset(ctx, band.SourceName, btc)
		if err != nil {
			t.Fatalf("LatestOracleUpdateForAsset(BTC): %v", err)
		}
		if gotBTC.Price.BigInt().Uint64() != btcRateE9 {
			t.Errorf("BTC rate = %s want %d", gotBTC.Price, btcRateE9)
		}
		if gotBTC.Decimals != 9 {
			t.Errorf("BTC decimals = %d want 9", gotBTC.Decimals)
		}
		if gotBTC.Timestamp.Unix() != int64(resolveSec) {
			t.Errorf("BTC ts = %v want unix %d", gotBTC.Timestamp, resolveSec)
		}
		if gotBTC.Observer != relayerStrkey {
			t.Errorf("BTC observer = %q want %q (from relay arg[0])", gotBTC.Observer, relayerStrkey)
		}

		xlm, err := canonical.NewCryptoAsset("XLM")
		if err != nil {
			t.Fatalf("NewCryptoAsset(XLM): %v", err)
		}
		gotXLM, err := store.LatestOracleUpdateForAsset(ctx, band.SourceName, xlm)
		if err != nil {
			t.Fatalf("LatestOracleUpdateForAsset(XLM): %v", err)
		}
		if gotXLM.Price.BigInt().Uint64() != xlmRateE9 {
			t.Errorf("XLM rate = %s want %d", gotXLM.Price, xlmRateE9)
		}
	})
}

// ─── helpers ─────────────────────────────────────────────────────

// runIngest mirrors cmd/stellarindex-indexer's processAndPersist
// logic in-test: stream ledgers from the datastore, dispatch them,
// persist each consumer.Event via the appropriate store insert,
// and upsert the pipeline cursor after each ledger. Returns the
// total event count emitted + number of ledgers processed + final
// cursor, so the test can assert on all three.
func runIngest(
	ctx context.Context, t *testing.T,
	disp *dispatcher.Dispatcher, lsCfg ledgerstream.Config,
	store *timescale.Store,
	from, to uint32,
) (events int, processed uint32, cursor timescale.Cursor) {
	t.Helper()

	err := ledgerstream.Stream(ctx, lsCfg, from, to, func(lcm xdr.LedgerCloseMeta) error {
		outputs, err := disp.ProcessLedger(lcm, testPassphrase)
		if err != nil {
			// FAIL, don't log-and-continue: this is the
			// load-bearing Galexie→ledgerstream→dispatcher→sink end-to-end test.
			// Downgrading a ProcessLedger error to t.Logf meant a real decode
			// regression (or a fixture that stops matching after an SDK bump)
			// degraded to "got 0 events, want 1" with the cause only logged.
			t.Errorf("dispatcher rejected ledger %d: %v", lcm.LedgerSequence(), err)
			return nil
		}
		for _, ev := range outputs {
			if err := persistInTest(ctx, store, ev); err != nil {
				t.Errorf("persist %s: %v", ev.EventKind(), err)
				continue
			}
			events++
		}
		processed++
		if err := store.UpsertCursor(ctx, "ledgerstream", "", lcm.LedgerSequence()); err != nil {
			t.Errorf("upsert cursor at ledger %d: %v", lcm.LedgerSequence(), err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ledgerstream.Stream: %v", err)
	}

	cursor, err = store.GetCursor(ctx, "ledgerstream", "")
	if err != nil {
		t.Fatalf("read back cursor: %v", err)
	}
	return events, processed, cursor
}

// persistInTest mirrors handleOneEvent in cmd/stellarindex-indexer
// but without the panic recovery + metrics plumbing — the test
// wants raw errors to surface.
func persistInTest(ctx context.Context, store *timescale.Store, ev consumer.Event) error {
	switch e := ev.(type) {
	case soroswap.TradeEvent:
		return store.InsertTrade(ctx, e.Trade)
	case aquarius.TradeEvent:
		return store.InsertTrade(ctx, e.Trade)
	case phoenix.TradeEvent:
		return store.InsertTrade(ctx, e.Trade)
	case sdex.TradeEvent:
		return store.InsertTrade(ctx, e.Trade)
	case reflector.UpdateEvent:
		return store.InsertOracleUpdate(ctx, e.Update)
	case redstone.UpdateEvent:
		return store.InsertOracleUpdate(ctx, e.Update)
	case band.UpdateEvent:
		return store.InsertOracleUpdate(ctx, e.Update)
	case comet.TradeEvent:
		return store.InsertTrade(ctx, e.Trade)
	}
	return fmt.Errorf("persistInTest: unhandled event %T", ev)
}

// newFullDispatcher registers every production decoder — the
// same set cmd/stellarindex-indexer wires from config when all
// sources are enabled. Reflector contracts use placeholders because
// the empty-ledger tests don't emit events that would be matched
// against them.
func newFullDispatcher(t *testing.T) *dispatcher.Dispatcher {
	t.Helper()
	d := dispatcher.New(
		reflector.NewDecoder(reflector.VariantDEX, "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"),
		reflector.NewDecoder(reflector.VariantCEX, "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"),
		reflector.NewDecoder(reflector.VariantFX, "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"),
		soroswap.NewDecoder(),
		aquarius.NewDecoder(),
		phoenix.NewDecoder(),
	)
	d.AddOpDecoder(sdex.NewDecoder())
	return d
}

// filesystemLedgerstreamConfig builds a ledgerstream.Config
// pointing at a local directory — the same config shape
// cmd/stellarindex-indexer would produce for an S3 datastore, just
// with Type=Filesystem so we don't need MinIO in the unit
// integration suite.
func filesystemLedgerstreamConfig(dir string) ledgerstream.Config {
	return ledgerstream.Config{
		DataStore: datastore.DataStoreConfig{
			Type:              "Filesystem",
			Params:            map[string]string{"destination_path": dir},
			Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
			NetworkPassphrase: testPassphrase,
			Compression:       "zstd",
		},
	}
}

// seedEmptyLedgers writes one valid-but-empty xdr.LedgerCloseMeta
// per sequence in `seqs` to the filesystem datastore rooted at
// `dir`. Publishes the datastore manifest so the ledgerstream's
// LoadSchema call finds it.
//
// "Empty" here means: a LedgerHeader with the right sequence, a
// valid (empty) GeneralizedTransactionSet, no transactions. The
// SDK's ingest reader handles this cleanly — the tx loop hits
// io.EOF immediately and the dispatcher returns zero outputs.
func seedEmptyLedgers(t *testing.T, ctx context.Context, dir string, seqs []uint32) {
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	for _, seq := range seqs {
		lcm := xdr.LedgerCloseMeta{
			V: 1,
			V1: &xdr.LedgerCloseMetaV1{
				LedgerHeader: xdr.LedgerHeaderHistoryEntry{
					Header: xdr.LedgerHeader{
						LedgerSeq: xdr.Uint32(seq),
					},
				},
				TxSet: xdr.GeneralizedTransactionSet{
					V:       1,
					V1TxSet: &xdr.TransactionSetV1{},
				},
			},
		}
		batch := xdr.LedgerCloseMetaBatch{
			StartSequence:    xdr.Uint32(seq),
			EndSequence:      xdr.Uint32(seq),
			LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
		}
		encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
		var buf bytes.Buffer
		if _, err := encoder.WriteTo(&buf); err != nil {
			t.Fatalf("encode batch seq=%d: %v", seq, err)
		}
		key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
		if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
			t.Fatalf("put seq=%d: %v", seq, err)
		}
	}
}

// byteSliceWriterTo adapts a []byte to io.WriterTo — the
// interface datastore.DataStore.PutFile expects.
type byteSliceWriterTo []byte

func (b byteSliceWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

// ─── richer Soroban-event fixture helpers ───────────────────────
// Everything below is scaffolding for the "soroban LCM with
// reflector FX update" subtest. The key design constraint: the
// LCM we construct must satisfy both (a) the SDK reader's
// envelope-hash ↔ TxProcessing matching (storeTransactions at
// ingest/ledger_transaction_reader.go) and (b) the dispatcher's
// topic-byte-equality match against the Reflector decoder.
//
// We lean on the SDK's own encoders for (a) — HashTransactionInEnvelope
// + compressxdr.NewXDREncoder — and on internal/scval for (b).
// Nothing here hand-rolls XDR bytes.

// mustContractIDFromStrkey decodes a C-strkey into a 32-byte
// xdr.ContractId. The reflector decoder compares events by the
// contract's strkey form, so we need the inverse of that here —
// the decoder gets strkey back from contractIDToStrkey() inside
// the dispatcher.
func mustContractIDFromStrkey(t *testing.T, s string) xdr.ContractId {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, s)
	if err != nil {
		t.Fatalf("strkey decode %q: %v", s, err)
	}
	if len(raw) != 32 {
		t.Fatalf("contract strkey %q decoded to %d bytes, want 32", s, len(raw))
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	return cid
}

// buildReflectorFXContractEvent constructs a single xdr.ContractEvent
// matching the Reflector FX oracle's exact on-wire shape. The FX
// variant's update_data entries all use Asset::Other(Symbol) (fiat
// tickers); callers pass a parallel pair of `symbols` and `prices`.
//
// The event body mirrors the SDK-encoded fixture shape in
// internal/sources/reflector/decode_test.go:encodeUpdateBody —
// reproduced here so the integration test doesn't reach into the
// reflector package's test helpers.
func buildReflectorFXContractEvent(
	t *testing.T,
	contractID xdr.ContractId,
	tsMs uint64,
	symbols []string,
	prices []*big.Int,
) xdr.ContractEvent {
	t.Helper()
	if len(symbols) != len(prices) {
		t.Fatalf("symbols/prices length mismatch: %d vs %d", len(symbols), len(prices))
	}

	// topic[0]=Symbol("REFLECTOR"), topic[1]=Symbol("update") — but
	// unlike the events.Event pathway (base64 strings), ContractEvent
	// carries *decoded* ScVals. We pass through the same scval encoder
	// so the round-trip (encode → dispatcher re-encodes to b64) lands
	// at identical topic bytes, keeping the byte-equality match live.
	refSym := xdr.ScSymbol(reflector.EventTopic0)
	updSym := xdr.ScSymbol(reflector.EventTopic1)
	tsVal := xdr.Uint64(tsMs)
	topicRef := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &refSym}
	topicUpd := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &updSym}
	topicTs := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &tsVal}

	// Build the update_data Vec<(Symbol, i128)>.
	tuples := make([]xdr.ScVal, len(symbols))
	for i := range symbols {
		sym := xdr.ScSymbol(symbols[i])
		symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
		hi, lo := splitBigInt128For128Parts(prices[i])
		parts := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
		priceSv := xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
		pair := xdr.ScVec{symSv, priceSv}
		pp := &pair
		tuples[i] = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pp}
	}
	updVec := xdr.ScVec(tuples)
	pUpdVec := &updVec
	innerVec := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pUpdVec}

	keySym := xdr.ScSymbol("update_data")
	keySv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &keySym}
	scMap := xdr.ScMap{xdr.ScMapEntry{Key: keySv, Val: innerVec}}
	pMap := &scMap
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pMap}

	cid := contractID
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: xdr.ScVec{topicRef, topicUpd, topicTs},
				Data:   body,
			},
		},
	}
}

// splitBigInt128For128Parts is the integration-test copy of the
// helper in internal/sources/reflector/decode_test.go. It splits a
// *big.Int into the (hi int64, lo uint64) pair that xdr.Int128Parts
// expects, handling negative values via two's-complement unwind.
func splitBigInt128For128Parts(n *big.Int) (hi int64, lo uint64) {
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	if n.Sign() >= 0 {
		loBig := new(big.Int).And(n, mask64)
		hiBig := new(big.Int).Rsh(n, 64)
		return hiBig.Int64(), loBig.Uint64()
	}
	twoTo128 := new(big.Int).Lsh(big.NewInt(1), 128)
	u := new(big.Int).Add(twoTo128, n)
	loBig := new(big.Int).And(u, mask64)
	hiBig := new(big.Int).Rsh(u, 64)
	return int64(hiBig.Uint64()), loBig.Uint64()
}

// seedSorobanLedger writes a single ledger containing one Soroban
// transaction whose TxMetaV3 carries the given ContractEvent. The
// envelope is the minimal Soroban-flagged shape from the SDK's test
// suite (ingest/ledger_transaction_test.go:48-58): V1 with
// Ext.V=1 and SorobanData set; IsSorobanTx() returns true so
// tx.GetTransactionEvents() reaches SorobanMeta.Events.
//
// The tx hash stored in TxProcessing[i].Result.TransactionHash must
// match HashTransactionInEnvelope(envelope, passphrase) — otherwise
// the reader's hash-lookup fails ("unknown tx hash in LedgerCloseMeta").
func seedSorobanLedger(
	t *testing.T,
	ctx context.Context,
	dir string,
	seq uint32,
	closedAt time.Time,
	ev xdr.ContractEvent,
) {
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	// Idempotent — PublishConfig is a no-op once the manifest is
	// present, so either subtest order works.
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	// ─── envelope ────────────────────────────────────────────
	// Soroban tx envelope shape (same as someSorobanTxEnvelope
	// in the SDK's tests). Empty Operations slice is fine —
	// tx.GetTransactionEvents() pulls from SorobanMeta in V3 meta,
	// not from the op list. The envelope source account is a
	// deterministic G-strkey so accountIDToStrkey() inside the
	// dispatcher doesn't error.
	srcSeed := [32]byte{0x10, 0xDE, 0xAD, 0xBE, 0xEF}
	for i := 5; i < 32; i++ {
		srcSeed[i] = byte(i)
	}
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: srcMuxed,
				Fee:           100,
				SeqNum:        1,
				Cond: xdr.Preconditions{
					Type: xdr.PreconditionTypePrecondNone,
				},
				Memo:       xdr.Memo{Type: xdr.MemoTypeMemoNone},
				Operations: []xdr.Operation{},
				Ext: xdr.TransactionExt{
					V:           1,
					SorobanData: &xdr.SorobanTransactionData{},
				},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}

	// ─── tx result ───────────────────────────────────────────
	// TxSuccess code with an empty Results slice → Successful()
	// is true AND OperationResults() returns ([], true). The
	// dispatcher's classic-op loop iterates over zero ops in that
	// case, skipping cleanly.
	emptyOpResults := []xdr.OperationResult{}
	result := xdr.TransactionResultPair{
		TransactionHash: xdr.Hash(hash),
		Result: xdr.TransactionResult{
			FeeCharged: 100,
			Result: xdr.TransactionResultResult{
				Code:    xdr.TransactionResultCodeTxSuccess,
				Results: &emptyOpResults,
			},
		},
	}

	// ─── tx meta with our event ──────────────────────────────
	// SorobanTransactionMeta.ReturnValue is an ScVal (not a pointer)
	// — its zero value marshals as ScVal{Type:0 = ScvBool} with
	// B=nil, which nil-derefs inside EncodeTo. Force it to ScvVoid
	// so the encoder has a valid arm with no payload.
	meta := xdr.TransactionMeta{
		V: 3,
		V3: &xdr.TransactionMetaV3{
			SorobanMeta: &xdr.SorobanTransactionMeta{
				Events:      []xdr.ContractEvent{ev},
				ReturnValue: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
	}

	// ─── assemble the LCM ────────────────────────────────────
	txProc := xdr.TransactionResultMeta{
		Result:            result,
		FeeProcessing:     xdr.LedgerEntryChanges{},
		TxApplyProcessing: meta,
	}
	phase := xdr.TransactionPhase{
		V: 0,
		V0Components: &[]xdr.TxSetComponent{
			{
				Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
				TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
					Txs: []xdr.TransactionEnvelope{envelope},
				},
			},
		},
	}
	lcm := xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue: xdr.StellarValue{
						CloseTime: xdr.TimePoint(closedAt.Unix()),
					},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{
					Phases: []xdr.TransactionPhase{phase},
				},
			},
			TxProcessing: []xdr.TransactionResultMeta{txProc},
		},
	}

	// ─── encode + publish ────────────────────────────────────
	batch := xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(seq),
		EndSequence:      xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
	}
	encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
	var buf bytes.Buffer
	if _, err := encoder.WriteTo(&buf); err != nil {
		t.Fatalf("encode batch seq=%d: %v", seq, err)
	}
	key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
		t.Fatalf("put seq=%d: %v", seq, err)
	}
}

// ─── band-specific fixture helpers ──────────────────────────────
// Band's Stellar contract emits NO events — it's purely observed
// via the InvokeContract op args (the dispatcher's ContractCallDecoder
// path). This helper seeds a ledger whose Soroban tx invokes
// `StandardReference.relay(from, symbol_rates, resolve_time,
// request_id)` with the given arg payload and no emitted events.

// bandRate is one (symbol, u64_rate_at_E9) pair — mirrors the
// Vec<(Symbol, u64)> shape of Band's symbol_rates arg.
type bandRate struct {
	Symbol string
	Rate   uint64
}

// seedBandRelayLedger writes one ledger containing a single
// Soroban InvokeHostFunction op that targets Band's
// StandardReference.relay(from, symbol_rates, resolve_time,
// request_id). The tx succeeds with zero emitted events — the
// dispatcher reaches the decoder via the ContractCall path only.
func seedBandRelayLedger(
	t *testing.T,
	ctx context.Context,
	dir string,
	seq uint32,
	closedAt time.Time,
	bandContractID xdr.ContractId,
	relayerStrkey string,
	rates []bandRate,
	resolveTime uint64,
	requestID uint64,
) {
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	// ─── InvokeContract op: StandardReference.relay(…) ───
	contractAddr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &bandContractID,
	}
	fromSv := accountStrkeyToAddressScVal(t, relayerStrkey)

	// symbol_rates: Vec<(Symbol, u64)>
	rateItems := make([]xdr.ScVal, len(rates))
	for i, r := range rates {
		sym := xdr.ScSymbol(r.Symbol)
		symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
		u := xdr.Uint64(r.Rate)
		rateSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
		tuple := xdr.ScVec{symSv, rateSv}
		pt := &tuple
		rateItems[i] = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pt}
	}
	outer := xdr.ScVec(rateItems)
	po := &outer
	symbolRatesSv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &po}

	// resolve_time: u64, request_id: u64
	rt := xdr.Uint64(resolveTime)
	resolveSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &rt}
	rid := xdr.Uint64(requestID)
	requestSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &rid}

	invokeOp := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: contractAddr,
						FunctionName:    xdr.ScSymbol(band.FnRelay),
						Args:            []xdr.ScVal{fromSv, symbolRatesSv, resolveSv, requestSv},
					},
				},
			},
		},
	}

	// ─── envelope ────────────────────────────────────────────
	srcSeed := [32]byte{0x30, 0xBA, 0x5D, 0x00}
	for i := 4; i < 32; i++ {
		srcSeed[i] = byte(i) + 0x50
	}
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: srcMuxed,
				Fee:           300,
				SeqNum:        1,
				Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
				Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
				Operations:    []xdr.Operation{invokeOp},
				Ext: xdr.TransactionExt{
					V:           1,
					SorobanData: &xdr.SorobanTransactionData{},
				},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}

	// ─── result: TxSuccess + one InvokeHostFunctionResult ────
	invokeRes := xdr.InvokeHostFunctionResult{
		Code:    xdr.InvokeHostFunctionResultCodeInvokeHostFunctionSuccess,
		Success: new(xdr.Hash),
	}
	opResults := []xdr.OperationResult{{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                     xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionResult: &invokeRes,
		},
	}}
	result := xdr.TransactionResultPair{
		TransactionHash: xdr.Hash(hash),
		Result: xdr.TransactionResult{
			FeeCharged: 300,
			Result: xdr.TransactionResultResult{
				Code:    xdr.TransactionResultCodeTxSuccess,
				Results: &opResults,
			},
		},
	}

	// ─── meta with EMPTY events slice — the whole point of Band ──
	meta := xdr.TransactionMeta{
		V: 3,
		V3: &xdr.TransactionMetaV3{
			SorobanMeta: &xdr.SorobanTransactionMeta{
				Events:      []xdr.ContractEvent{}, // ← explicitly empty
				ReturnValue: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
	}

	txProc := xdr.TransactionResultMeta{
		Result:            result,
		FeeProcessing:     xdr.LedgerEntryChanges{},
		TxApplyProcessing: meta,
	}
	phase := xdr.TransactionPhase{
		V: 0,
		V0Components: &[]xdr.TxSetComponent{
			{
				Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
				TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
					Txs: []xdr.TransactionEnvelope{envelope},
				},
			},
		},
	}
	lcm := xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(closedAt.Unix())},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{
					Phases: []xdr.TransactionPhase{phase},
				},
			},
			TxProcessing: []xdr.TransactionResultMeta{txProc},
		},
	}

	batch := xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(seq),
		EndSequence:      xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
	}
	encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
	var buf bytes.Buffer
	if _, err := encoder.WriteTo(&buf); err != nil {
		t.Fatalf("encode batch seq=%d: %v", seq, err)
	}
	key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
		t.Fatalf("put seq=%d: %v", seq, err)
	}
}

// ─── comet-specific fixture helpers ─────────────────────────────

// buildCometSwapEvent assembles a Comet POOL.swap ContractEvent:
//
//	topic  = (Symbol("POOL"), Symbol("swap"))
//	body   = Map { "caller": Address, "token_in": Address,
//	               "token_out": Address, "token_amount_in": i128,
//	               "token_amount_out": i128 }
//
// Mirrors the contract's `env.events().publish((POOL, swap), event)`
// call at comet-contracts/contracts/src/c_pool/call_logic/pool.rs:191.
func buildCometSwapEvent(
	t *testing.T,
	poolContractID xdr.ContractId,
	caller, tokenIn, tokenOut string,
	amountIn, amountOut *big.Int,
) xdr.ContractEvent {
	t.Helper()

	poolSym := xdr.ScSymbol(comet.EventTopic0)
	swapSym := xdr.ScSymbol(comet.EventSwap)
	topicPool := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &poolSym}
	topicSwap := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &swapSym}

	callerSv := accountStrkeyToAddressScVal(t, caller)
	tokenInSv := contractStrkeyToAddressScVal(t, tokenIn)
	tokenOutSv := contractStrkeyToAddressScVal(t, tokenOut)
	amountInSv := bigIntToI128ScVal(t, amountIn)
	amountOutSv := bigIntToI128ScVal(t, amountOut)

	keys := []string{"caller", "token_amount_in", "token_amount_out", "token_in", "token_out"}
	vals := []xdr.ScVal{callerSv, amountInSv, amountOutSv, tokenInSv, tokenOutSv}
	m := make(xdr.ScMap, len(keys))
	for i, k := range keys {
		sym := xdr.ScSymbol(k)
		m[i] = xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: vals[i],
		}
	}
	pm := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}

	cid := poolContractID
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: xdr.ScVec{topicPool, topicSwap},
				Data:   body,
			},
		},
	}
}

// contractStrkeyToAddressScVal encodes a C-strkey as ScVal::Address.
func contractStrkeyToAddressScVal(t *testing.T, cStrkey string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cStrkey)
	if err != nil {
		t.Fatalf("strkey decode %q: %v", cStrkey, err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &cid,
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

// bigIntToI128ScVal splits a signed *big.Int into Hi/Lo with
// two's-complement semantics for negatives.
func bigIntToI128ScVal(t *testing.T, n *big.Int) xdr.ScVal {
	t.Helper()
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	var hi int64
	var lo uint64
	if n.Sign() >= 0 {
		loBig := new(big.Int).And(n, mask64)
		hiBig := new(big.Int).Rsh(n, 64)
		hi = hiBig.Int64()
		lo = loBig.Uint64()
	} else {
		twoTo128 := new(big.Int).Lsh(big.NewInt(1), 128)
		u := new(big.Int).Add(twoTo128, n)
		loBig := new(big.Int).And(u, mask64)
		hiBig := new(big.Int).Rsh(u, 64)
		hi = int64(hiBig.Uint64())
		lo = loBig.Uint64()
	}
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

// ─── redstone-specific fixture helpers ──────────────────────────
// Mirrors the Reflector helpers above but targets the RedStone
// Adapter's WritePrices shape (single REDSTONE topic, Map body with
// updater + updated_feeds, plus InvokeContract op args carrying the
// feed_ids list). These prove the OpArgs plumbing end-to-end.

// buildRedstoneWritePricesEvent constructs the RedStone Adapter's
// WritePrices event body:
//
//	topic[0] = Symbol("REDSTONE")
//	body     = Map { "updater": Address, "updated_feeds": Vec<PriceData> }
//	PriceData = Map { "price": U256, "package_timestamp": u64,
//	                  "write_timestamp": u64 }
func buildRedstoneWritePricesEvent(
	t *testing.T,
	contractID xdr.ContractId,
	updaterStrkey string,
	prices []*big.Int,
	packageTs, writeTs uint64,
) xdr.ContractEvent {
	t.Helper()

	redstoneSym := xdr.ScSymbol(redstone.EventTopic0)
	topicRedstone := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &redstoneSym}

	// updater address
	updaterAddrSv := accountStrkeyToAddressScVal(t, updaterStrkey)

	// updated_feeds: Vec<PriceData>
	items := make([]xdr.ScVal, len(prices))
	for i, p := range prices {
		priceSv := bigIntToU256ScVal(t, p)
		pkgU := xdr.Uint64(packageTs)
		wrU := xdr.Uint64(writeTs)
		pkgSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &pkgU}
		wrSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &wrU}
		pdKeys := []string{"price", "package_timestamp", "write_timestamp"}
		pdVals := []xdr.ScVal{priceSv, pkgSv, wrSv}
		pdMap := make(xdr.ScMap, len(pdKeys))
		for j, k := range pdKeys {
			sym := xdr.ScSymbol(k)
			pdMap[j] = xdr.ScMapEntry{
				Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Val: pdVals[j],
			}
		}
		pp := &pdMap
		items[i] = xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pp}
	}
	vec := xdr.ScVec(items)
	pvec := &vec
	feedsSv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}

	outerKeys := []string{"updated_feeds", "updater"}
	outerVals := []xdr.ScVal{feedsSv, updaterAddrSv}
	outer := make(xdr.ScMap, len(outerKeys))
	for i, k := range outerKeys {
		sym := xdr.ScSymbol(k)
		outer[i] = xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: outerVals[i],
		}
	}
	pouter := &outer
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pouter}

	cid := contractID
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: xdr.ScVec{topicRedstone},
				Data:   body,
			},
		},
	}
}

// accountStrkeyToAddressScVal encodes a G-strkey as an ScVal::Address
// via the strkey→AccountId→ScAddress path. Used for both the event's
// updater field and the corresponding InvokeContract arg.
func accountStrkeyToAddressScVal(t *testing.T, gStrkey string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, gStrkey)
	if err != nil {
		t.Fatalf("strkey decode %q: %v", gStrkey, err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &pub,
	}
	addr := xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &aid,
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

// bigIntToU256ScVal splits a non-negative *big.Int into four uint64
// words and wraps into ScVal::U256. Mirrors the redstone package-
// level helper so the integration test doesn't import from the
// _test.go file.
func bigIntToU256ScVal(t *testing.T, n *big.Int) xdr.ScVal {
	t.Helper()
	if n.Sign() < 0 {
		t.Fatalf("u256 does not accept negative: %s", n)
	}
	buf := n.Bytes()
	if len(buf) > 32 {
		t.Fatalf("value exceeds 256 bits: %s", n)
	}
	padded := make([]byte, 32)
	copy(padded[32-len(buf):], buf)
	w := func(b []byte) uint64 {
		var v uint64
		for _, x := range b {
			v = v<<8 | uint64(x)
		}
		return v
	}
	parts := xdr.UInt256Parts{
		HiHi: xdr.Uint64(w(padded[0:8])),
		HiLo: xdr.Uint64(w(padded[8:16])),
		LoHi: xdr.Uint64(w(padded[16:24])),
		LoLo: xdr.Uint64(w(padded[24:32])),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &parts}
}

// seedRedstoneLedger writes a single ledger containing one Soroban
// transaction that:
//
//  1. Invokes the RedStone Adapter's write_prices(updater, feed_ids,
//     payload) function — the op args the dispatcher plumbs through
//     to the decoder as events.Event.OpArgs.
//  2. Emits one REDSTONE contract event whose body matches the
//     write_prices call (same updater, same feed count).
//
// This is the paired envelope+meta shape a real adapter tx produces
// on mainnet. The dispatcher's hash-matching + OpArgs extraction are
// exactly what's being exercised here.
func seedRedstoneLedger(
	t *testing.T,
	ctx context.Context,
	dir string,
	seq uint32,
	closedAt time.Time,
	adapterContractID xdr.ContractId,
	updaterStrkey string,
	feedIDs []string,
	ev xdr.ContractEvent,
) {
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	// ─── InvokeContract op (write_prices(updater, feed_ids, payload)) ──
	// ContractAddress inside the invoke-op targets the adapter.
	adapterSv := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &adapterContractID,
	}
	updaterArg := accountStrkeyToAddressScVal(t, updaterStrkey)

	// feed_ids Vec<String>
	feedItems := make([]xdr.ScVal, len(feedIDs))
	for i, id := range feedIDs {
		s := xdr.ScString(id)
		feedItems[i] = xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &s}
	}
	feedVec := xdr.ScVec(feedItems)
	pFeedVec := &feedVec
	feedIDsArg := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pFeedVec}

	// payload Bytes — decoder ignores content, just needs a valid ScVal
	payloadBytes := xdr.ScBytes{0x01, 0x02, 0x03}
	payloadArg := xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &payloadBytes}

	invokeOp := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: adapterSv,
						FunctionName:    xdr.ScSymbol(redstone.WriteFnName),
						Args:            []xdr.ScVal{updaterArg, feedIDsArg, payloadArg},
					},
				},
			},
		},
	}

	// ─── Soroban envelope carrying the op ─────────────────────
	srcSeed := [32]byte{0x20, 0xDE, 0xAD, 0xBE, 0xEF}
	for i := 5; i < 32; i++ {
		srcSeed[i] = byte(i) + 1
	}
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: srcMuxed,
				Fee:           200,
				SeqNum:        1,
				Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
				Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
				Operations:    []xdr.Operation{invokeOp},
				Ext: xdr.TransactionExt{
					V:           1,
					SorobanData: &xdr.SorobanTransactionData{},
				},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}

	// ─── tx result: TxSuccess + one (stub) InvokeHostFunctionResult ──
	// The dispatcher's classic-op walk iterates operations paired
	// with op results. We include exactly one result matching the
	// envelope's one op — success with empty sub-value so no
	// op-decoder consumes it (we registered none).
	invokeRes := xdr.InvokeHostFunctionResult{
		Code: xdr.InvokeHostFunctionResultCodeInvokeHostFunctionSuccess,
		// Success arm: Hash of the return value. Zero hash is fine —
		// the dispatcher doesn't inspect it.
		Success: new(xdr.Hash),
	}
	opResults := []xdr.OperationResult{{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                     xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionResult: &invokeRes,
		},
	}}
	result := xdr.TransactionResultPair{
		TransactionHash: xdr.Hash(hash),
		Result: xdr.TransactionResult{
			FeeCharged: 200,
			Result: xdr.TransactionResultResult{
				Code:    xdr.TransactionResultCodeTxSuccess,
				Results: &opResults,
			},
		},
	}

	// ─── tx meta with our event ───────────────────────────────
	meta := xdr.TransactionMeta{
		V: 3,
		V3: &xdr.TransactionMetaV3{
			SorobanMeta: &xdr.SorobanTransactionMeta{
				Events:      []xdr.ContractEvent{ev},
				ReturnValue: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
	}

	// ─── assemble + publish the LCM ───────────────────────────
	txProc := xdr.TransactionResultMeta{
		Result:            result,
		FeeProcessing:     xdr.LedgerEntryChanges{},
		TxApplyProcessing: meta,
	}
	phase := xdr.TransactionPhase{
		V: 0,
		V0Components: &[]xdr.TxSetComponent{
			{
				Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
				TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
					Txs: []xdr.TransactionEnvelope{envelope},
				},
			},
		},
	}
	lcm := xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(closedAt.Unix())},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{
					Phases: []xdr.TransactionPhase{phase},
				},
			},
			TxProcessing: []xdr.TransactionResultMeta{txProc},
		},
	}

	batch := xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(seq),
		EndSequence:      xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
	}
	encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
	var buf bytes.Buffer
	if _, err := encoder.WriteTo(&buf); err != nil {
		t.Fatalf("encode batch seq=%d: %v", seq, err)
	}
	key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
		t.Fatalf("put seq=%d: %v", seq, err)
	}
}
