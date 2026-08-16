//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Two SDF-reserve-style watched accounts. The storage layer treats the
// AccountID as opaque, but these are well-formed G-strkeys for realism.
var watermarkReserveAccounts = []string{
	"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	"GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUV5H",
}

// watermarkStoreAdapter maps *timescale.Store onto
// supply.AccountObservationLookup — the same 20-line adapter the aggregator
// and ops binaries inline (they each own their binary and don't share it).
type watermarkStoreAdapter struct{ s *timescale.Store }

func (a watermarkStoreAdapter) LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (supply.AccountObservationRow, error) {
	row, err := a.s.LatestAccountObservationAtOrBefore(ctx, accountID, asOfLedger)
	if err != nil {
		return supply.AccountObservationRow{}, err
	}
	return supply.AccountObservationRow{
		Balance:   row.Balance,
		IsRemoval: row.IsRemoval,
		Ledger:    row.Ledger,
	}, nil
}

func (a watermarkStoreAdapter) MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error) {
	return a.s.MaxAccountObservationLedger(ctx, asOfLedger)
}

// fixedLedgers is a supply.LedgerLookup that reports one pinned chain tip —
// the snapshot ledger the refresher evaluates against.
type fixedLedgers struct {
	ledger uint32
	at     time.Time
}

func (f fixedLedgers) LatestKnownLedger(context.Context) (uint32, time.Time, error) {
	return f.ledger, f.at, nil
}

// capturingInserter records how many snapshots the refresher accepted (and the
// last one), so a test can distinguish an ACCEPT (gate passed) from a REJECT.
type capturingInserter struct {
	calls int
	last  supply.Supply
}

func (c *capturingInserter) InsertSupply(_ context.Context, s supply.Supply) error {
	c.calls++
	c.last = s
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// insertQuietReserveObservations seeds one observation per reserve account at
// lastChangeLedger and returns the observation timestamp. This models the live
// r1 state: the reserve accounts' NEWEST rows sit at their last balance change,
// deep in the past relative to the current tip.
func insertQuietReserveObservations(t *testing.T, ctx context.Context, store *timescale.Store, lastChangeLedger uint32) {
	t.Helper()
	obsAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, acc := range watermarkReserveAccounts {
		if err := store.InsertAccountObservation(ctx, domain.AccountObservation{
			AccountID:  acc,
			Ledger:     lastChangeLedger,
			ObservedAt: obsAt,
			Balance:    big.NewInt(int64(100 * (i + 1))), // small non-zero reserve
		}); err != nil {
			t.Fatalf("InsertAccountObservation %s@%d: %v", acc, lastChangeLedger, err)
		}
	}
}

func newWatermarkRefresher(store *timescale.Store, snapshotLedger uint32, inserter *capturingInserter) *supply.Refresher {
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	reader := supply.NewLCMReserveBalanceReader(watermarkStoreAdapter{s: store})
	computer, err := supply.NewXLMComputer(watermarkReserveAccounts, reader)
	if err != nil {
		// NewXLMComputer only errors on nil-reader-with-accounts; we always
		// pass a reader, so this is a programming error if hit.
		panic(err)
	}
	// Default thresholds: 1000-ledger stale gate, 17280-ledger (~1 day)
	// dormancy horizon — the exact production defaults the r1 gate ran under.
	return supply.NewRefresher(
		fixedLedgers{ledger: snapshotLedger, at: t0},
		computer,
		inserter,
		discardLogger(),
	)
}

// TestAccountObserverWatermark_QuietObserverStaysFresh is the money-adjacent
// regression proof (F-1320 / R-002 / CS-102 tail). A HEALTHY account observer
// that has PROCESSED up to a fresh ledger but whose watched reserve accounts
// have not CHANGED for far longer than the dormancy horizon must NOT trip the
// XLM supply freshness gate.
//
// RED ON UNFIXED CODE: before the fix, Store.MaxAccountObservationLedger
// returned MAX(ledger) FROM account_observations = the last balance-change
// ledger (50_000_000). The anchor assertion below fails (got 50_000_000, want
// the 50_030_000 watermark), and — end to end — the refresher rejects the
// snapshot as stale_component (gap 30_000 > horizon 17_280) instead of
// accepting it. With the fix the anchor is the fresh watermark and the gate
// accepts.
func TestAccountObserverWatermark_QuietObserverStaysFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lastChange = uint32(50_000_000)
	// The observer has processed 30_000 ledgers past the last reserve change —
	// well past DefaultMaxDormantComponentLedgers (17_280). A quiet asset, a
	// healthy observer.
	const processed = lastChange + 30_000

	insertQuietReserveObservations(t, ctx, store, lastChange)
	if err := store.UpsertAccountObserverWatermark(ctx, processed); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark: %v", err)
	}

	// ── Storage-level: the anchor is the observer WATERMARK, not the last
	// balance change. This is the exact assertion that is red on unfixed code.
	anchor, err := store.MaxAccountObservationLedger(ctx, processed)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor == lastChange {
		t.Fatalf("anchor regressed to the last balance-change ledger %d — a quiet "+
			"reserve account is NOT a stalled observer; this is exactly what froze "+
			"XLM supply and cried wolf on r1", lastChange)
	}
	if anchor != processed {
		t.Fatalf("anchor = %d, want %d (the observer watermark)", anchor, processed)
	}

	// ── End-to-end: the refresher ACCEPTS the snapshot (gate passes).
	inserter := &capturingInserter{}
	out := newWatermarkRefresher(store, processed, inserter).Tick(ctx)
	if out.Kind != supply.OutcomeKindOK {
		t.Fatalf("gate rejected a healthy quiet observer: kind=%s err=%v — the "+
			"freshness anchor must track the observer watermark, not the last "+
			"balance change", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Fatalf("InsertSupply calls = %d, want 1 (snapshot accepted)", inserter.calls)
	}
}

// TestAccountObserverWatermark_StalledObserverStillTrips proves dead-observer
// detection is PRESERVED. When the observer's watermark is FROZEN (it stopped
// advancing) while the chain tip keeps climbing, the anchor freezes at the
// watermark and the gate correctly fails closed past the dormancy horizon —
// it does not republish a frozen supply stamped at the current tip.
func TestAccountObserverWatermark_StalledObserverStillTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lastChange = uint32(50_000_000)
	// The observer advanced a little, then STALLED (died).
	const frozenWatermark = lastChange + 100
	// The chain tip, meanwhile, has run far ahead of the frozen watermark.
	const snapshotLedger = frozenWatermark + 30_000

	insertQuietReserveObservations(t, ctx, store, lastChange)
	if err := store.UpsertAccountObserverWatermark(ctx, frozenWatermark); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark: %v", err)
	}

	// The anchor is the FROZEN watermark (bounded by, but far below, the tip).
	anchor, err := store.MaxAccountObservationLedger(ctx, snapshotLedger)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor != frozenWatermark {
		t.Fatalf("anchor = %d, want %d (the frozen observer watermark)", anchor, frozenWatermark)
	}

	// The gate must fail closed: a dead observer is not a dormant asset.
	inserter := &capturingInserter{}
	out := newWatermarkRefresher(store, snapshotLedger, inserter).Tick(ctx)
	if out.Kind != supply.OutcomeKindStaleComponent {
		t.Fatalf("gate did not fail closed on a stalled observer: kind=%s err=%v — "+
			"a frozen watermark past the dormancy horizon must be rejected as "+
			"stale_component", out.Kind, out.Err)
	}
	if inserter.calls != 0 {
		t.Fatalf("InsertSupply calls = %d, want 0 (snapshot rejected)", inserter.calls)
	}
}

// TestAccountObserverWatermark_Monotonic pins the never-regress guard: a lower
// or equal processed_ledger is a silent no-op, so an out-of-order or replayed
// writer can never drag the freshness anchor backwards.
func TestAccountObserverWatermark_Monotonic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const high = uint32(50_050_000)
	if err := store.UpsertAccountObserverWatermark(ctx, high); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark(high): %v", err)
	}
	// A regressing write must not move the anchor.
	if err := store.UpsertAccountObserverWatermark(ctx, high-10_000); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark(regress): %v", err)
	}
	// asOf is set above `high` so the LEAST() bound doesn't clamp the result.
	anchor, err := store.MaxAccountObservationLedger(ctx, high+1)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor != high {
		t.Fatalf("anchor = %d, want %d — a regressing upsert must be a no-op", anchor, high)
	}

	// Empty-table posture is exercised implicitly elsewhere, but assert the
	// bound too: asOf below the watermark clamps the anchor to asOf.
	bounded, err := store.MaxAccountObservationLedger(ctx, high-5_000)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger(bounded): %v", err)
	}
	if bounded != high-5_000 {
		t.Fatalf("bounded anchor = %d, want %d (LEAST clamps to asOf)", bounded, high-5_000)
	}
}
