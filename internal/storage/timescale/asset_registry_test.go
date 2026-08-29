package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// newDedupeStore returns a DB-less Store whose dedupe cache is
// fresh, so each `shouldSkipAssetRegistryUpsert` case sees the
// asset as never-upserted. The cache lives on the Store (not the
// package) since the 2026-08-28 cross-DB fix, so a new Store IS
// the reset. F-1243 (codex audit-2026-05-12).
func newDedupeStore() *Store {
	return &Store{}
}

// TestShouldSkipAssetRegistryUpsert_NoCache — first call for an
// asset returns false (don't skip; the upsert needs to run).
func TestShouldSkipAssetRegistryUpsert_NoCache(t *testing.T) {
	s := newDedupeStore()
	if s.shouldSkipAssetRegistryUpsert("USDC-GA5Z", time.Now()) {
		t.Error("first-time check returned skip=true; want false (no cache → must upsert)")
	}
}

// TestShouldSkipAssetRegistryUpsert_WithinTTL — call inside the
// 60s window after a recorded upsert returns true (skip the
// DB round-trip — the F-1243 pre-fix preserved this behaviour
// indefinitely; the wave-46 fix only preserves it for the TTL
// window).
func TestShouldSkipAssetRegistryUpsert_WithinTTL(t *testing.T) {
	s := newDedupeStore()
	now := time.Now()
	s.assetRegistryDedupe.Store("USDC-GA5Z", now.Add(-5*time.Second))
	if !s.shouldSkipAssetRegistryUpsert("USDC-GA5Z", now) {
		t.Error("5s-after-upsert returned skip=false; want true (within 60s TTL)")
	}
}

// TestShouldSkipAssetRegistryUpsert_PastTTL — call outside the
// 60s window returns false (upsert must run so `last_seen_*` +
// `observation_count` advance). This is the F-1243 regression
// — pre-wave-46 the cache had no TTL so every subsequent call
// returned true and the row froze at first observation.
func TestShouldSkipAssetRegistryUpsert_PastTTL(t *testing.T) {
	s := newDedupeStore()
	now := time.Now()
	s.assetRegistryDedupe.Store("USDC-GA5Z", now.Add(-2*time.Minute))
	if s.shouldSkipAssetRegistryUpsert("USDC-GA5Z", now) {
		t.Error("2min-after-upsert returned skip=true; want false (past 60s TTL)")
	}
}

// TestShouldSkipAssetRegistryUpsert_DifferentAssetMisses — the
// cache is per-asset; an entry for asset A doesn't suppress
// asset B.
func TestShouldSkipAssetRegistryUpsert_DifferentAssetMisses(t *testing.T) {
	s := newDedupeStore()
	now := time.Now()
	s.assetRegistryDedupe.Store("USDC-GA5Z", now)
	if s.shouldSkipAssetRegistryUpsert("AQUA-GBNZ", now) {
		t.Error("different-asset returned skip=true; want false (per-asset cache)")
	}
}

// TestShouldSkipAssetRegistryUpsert_CacheCorruption — if some
// future code mistakenly stores a non-time.Time value (legacy
// bug shape from the pre-wave-46 sentinel pattern), the gate
// fails open (returns false) so the upsert still runs.
func TestShouldSkipAssetRegistryUpsert_CacheCorruption(t *testing.T) {
	s := newDedupeStore()
	s.assetRegistryDedupe.Store("USDC-GA5Z", struct{}{}) // pre-wave-46 shape
	if s.shouldSkipAssetRegistryUpsert("USDC-GA5Z", time.Now()) {
		t.Error("corrupt-cache returned skip=true; want false (fail-open to allow upsert)")
	}
}

// TestAssetRegistryDedupeTTL_FrozenValue — pin the TTL so a
// future operator can't quietly tighten it past the audit's
// acceptable window (60s gives roughly minute-level "last
// seen" freshness in the dashboard).
func TestAssetRegistryDedupeTTL_FrozenValue(t *testing.T) {
	if assetRegistryDedupeTTL != 60*time.Second {
		t.Errorf("assetRegistryDedupeTTL = %v, want 60s (operator-visible)", assetRegistryDedupeTTL)
	}
}

// recordingDriver is a minimal database/sql driver that records
// every ExecContext statement and returns success. It stands in
// for one Postgres database so the registry hook's real code path
// can be exercised per-Store without a container.
type recordingDriver struct{ execs []string }

func (d *recordingDriver) Connect(context.Context) (driver.Conn, error) { return d, nil }
func (d *recordingDriver) Driver() driver.Driver                        { return d }
func (d *recordingDriver) Open(string) (driver.Conn, error)             { return d, nil }
func (d *recordingDriver) Prepare(string) (driver.Stmt, error)          { return nil, driver.ErrSkip }
func (d *recordingDriver) Close() error                                 { return nil }
func (d *recordingDriver) Begin() (driver.Tx, error)                    { return nil, driver.ErrSkip }

func (d *recordingDriver) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	d.execs = append(d.execs, q)
	return driver.RowsAffected(1), nil
}

func (d *recordingDriver) classicAssetUpserts() int {
	n := 0
	for _, q := range d.execs {
		if strings.Contains(q, "INSERT INTO classic_assets") {
			n++
		}
	}
	return n
}

// TestAssetRegistryDedupe_IsolatedPerStore — the dedupe cache is
// scoped to ONE Store (one database). asset_id is only unique
// within a database, so a process-wide cache let a second Store
// over a different DB inherit "already upserted within TTL" for
// a classic_assets row that DB never received — the 2026-08-28
// TestAssetsReader `HasAsset(USDC) = false` flake, reproduced
// whenever it ran after any test that inserted a USDC trade into
// a different container. Drives the real registerClassicAssetSeen
// path on two Stores over two (recording) databases: each database
// must receive its own classic_assets upsert.
func TestAssetRegistryDedupe_IsolatedPerStore(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	ctx := context.Background()
	now := time.Now()

	dbA, dbB := &recordingDriver{}, &recordingDriver{}
	a := &Store{db: sql.OpenDB(dbA)}
	b := &Store{db: sql.OpenDB(dbB)}

	if err := a.registerClassicAssetSeen(ctx, usdc, 100, now); err != nil {
		t.Fatalf("store A registerClassicAssetSeen: %v", err)
	}
	if got := dbA.classicAssetUpserts(); got != 1 {
		t.Fatalf("db A classic_assets upserts = %d, want 1", got)
	}

	// Same asset, different Store over a different database, inside
	// the TTL window. The second database has never seen the asset
	// so it MUST get its own upsert.
	if err := b.registerClassicAssetSeen(ctx, usdc, 100, now); err != nil {
		t.Fatalf("store B registerClassicAssetSeen: %v", err)
	}
	if got := dbB.classicAssetUpserts(); got != 1 {
		t.Errorf("db B classic_assets upserts = %d, want 1 (dedupe state leaked from store A — cache must be per-Store, not per-process)", got)
	}
	if got := dbA.classicAssetUpserts(); got != 1 {
		t.Errorf("db A classic_assets upserts = %d after store B's write, want still 1", got)
	}

	// And within ONE store the TTL dedupe still holds: a second
	// trade for the same asset inside the window is skipped.
	if err := a.registerClassicAssetSeen(ctx, usdc, 101, now.Add(time.Second)); err != nil {
		t.Fatalf("store A second registerClassicAssetSeen: %v", err)
	}
	if got := dbA.classicAssetUpserts(); got != 1 {
		t.Errorf("db A classic_assets upserts = %d after in-window repeat, want 1 (TTL dedupe weakened)", got)
	}
}
