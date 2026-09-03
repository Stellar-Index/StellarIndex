package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// snapshotStubReader is the upstream the seeded cache wraps. It counts
// ListAssetsExt calls so a test can prove the boot seed removed the
// cold read entirely rather than merely making it look fast. Every
// other v1.AssetsReader method is an inert stub — none is on this
// path.
type snapshotStubReader struct {
	// The cached reader single-flights a cold miss, so concurrent
	// readers can reach this stub on different goroutines. A bare
	// int++ here is a data race that `go test -race` fails on — and
	// the race is in the TEST, which makes it the worst kind: it
	// reddens CI for a defect that does not exist in the code under
	// test.
	mu    sync.Mutex
	calls int
}

// callCount reads the counter under the same lock that increments it.
func (s *snapshotStubReader) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *snapshotStubReader) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	n := opts.Limit
	if n > 3 {
		n = 3
	}
	rows := make([]timescale.AssetRow, 0, n)
	for i := range n {
		rows = append(rows, timescale.AssetRow{AssetID: "UPSTREAM-GAAA", ObservationCount: int64(i)})
	}
	return rows, nil
}

func (s *snapshotStubReader) GetAssetBySlug(context.Context, string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *snapshotStubReader) GetAssetByAssetID(context.Context, string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *snapshotStubReader) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *snapshotStubReader) GetAssetTopMarkets(context.Context, string, int) ([]timescale.AssetTopMarket, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetPriceHistory24h(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetPriceHistory7d(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetsPriceHistory24hBatch(
	context.Context, []string,
) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetsPriceHistory7dBatch(
	context.Context, []string,
) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetMarketsCount(context.Context, string) (int64, error) {
	return 0, nil
}

func (s *snapshotStubReader) GetAssetATH(context.Context, string) (*timescale.AssetATH, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetsATHBatch(context.Context, []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func (s *snapshotStubReader) GetAssetTradeCount24h(context.Context, string) (int64, error) {
	return 0, nil
}

func newTestSnapshots(t *testing.T) (*assetsListingSnapshots, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return newAssetsListingSnapshots(rdb, discardLogger()), mr
}

func snapshotRows() []timescale.AssetRow {
	return []timescale.AssetRow{
		{AssetID: "SNAP1-GAAA", Slug: "snap1", Code: "SNAP1", ObservationCount: 7},
		{AssetID: "SNAP2-GAAA", Slug: "snap2", Code: "SNAP2", ObservationCount: 6},
	}
}

// TestAssetsListingSnapshotKeyIsStable is the golden-key test ADR-0007
// asks of every key family: the wire shape is pinned so a refactor
// cannot silently move the namespace and orphan every snapshot the
// running fleet wrote.
func TestAssetsListingSnapshotKeyIsStable(t *testing.T) {
	got := assetsListingSnapshotRedisKey(51, timescale.AssetsOrderVolume24hUSDDesc).String()
	fp := assetRowSchemaFingerprint()
	want := "assets:listing-snapshot:" + fp + ":1:51"
	if got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "assets:listing-snapshot:") {
		t.Fatalf("key %q left the assets:listing-snapshot: namespace", got)
	}
	// The two orders and the limit must all discriminate, or one warmed
	// variant would silently serve another's rows.
	if a, b := assetsListingSnapshotRedisKey(51, timescale.AssetsOrderObservationCountDesc),
		assetsListingSnapshotRedisKey(51, timescale.AssetsOrderVolume24hUSDDesc); a == b {
		t.Fatalf("the two orders collide on %q", a)
	}
	if a, b := assetsListingSnapshotRedisKey(11, timescale.AssetsOrderVolume24hUSDDesc),
		assetsListingSnapshotRedisKey(51, timescale.AssetsOrderVolume24hUSDDesc); a == b {
		t.Fatalf("two limits collide on %q", a)
	}
	if len(fp) != 12 {
		t.Fatalf("schema fingerprint %q is not the expected 12 hex chars", fp)
	}
}

// TestSeedAssetListingsFromSnapshotsRemovesTheColdRead is the #459 fix
// end to end at the binary's seam: what the prewarm persisted on the
// previous boot must, on this one, be serving before anything queries
// Postgres.
//
// The upstream call counter is the assertion that matters. Un-seeded,
// the first ListAssetsExt per variant reaches upstream — on r1 that is
// the ~11 s aggregate the first browser request waited on. Seeded, it
// must be zero.
func TestSeedAssetListingsFromSnapshotsRemovesTheColdRead(t *testing.T) {
	snaps, _ := newTestSnapshots(t)
	ctx := context.Background()
	observedAt := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)

	// Previous process: the prewarm persists every warmed variant.
	for _, o := range assetListingPrewarmOptions() {
		snaps.save(ctx, o, snapshotRows(), observedAt)
	}

	// This process.
	up := &snapshotStubReader{}
	reader := v1.NewCachedAssetsReader(up, 2*time.Minute)
	seeded, requested := seedAssetListingsFromSnapshots(ctx, discardLogger(), reader, snaps)

	if requested != len(assetListingPrewarmOptions()) {
		t.Fatalf("requested = %d, want the full prewarm option set (%d)", requested, len(assetListingPrewarmOptions()))
	}
	if seeded != requested {
		t.Fatalf("seeded %d of %d variants — every one was persisted, so any shortfall is a key or age mismatch",
			seeded, requested)
	}

	for _, o := range assetListingPrewarmOptions() {
		rows, at, stale, err := reader.ListAssetsExtAt(ctx, o)
		if err != nil {
			t.Fatalf("ListAssetsExtAt(limit=%d order=%v): %v", o.Limit, o.Order, err)
		}
		if len(rows) != 2 || rows[0].AssetID != "SNAP1-GAAA" {
			t.Fatalf("limit=%d order=%v served %+v, want the snapshot rows", o.Limit, o.Order, rows)
		}
		if stale {
			t.Errorf("limit=%d: stale = true for a 90s-old snapshot under a 2m TTL", o.Limit)
		}
		if !at.Equal(observedAt) {
			t.Errorf("limit=%d: observedAt = %v, want the snapshot's %v", o.Limit, at, observedAt)
		}
	}
	if got := up.callCount(); got != 0 {
		t.Fatalf("upstream was queried %d times despite a complete boot seed — the seed addressed keys "+
			"the reader does not look up", got)
	}
}

// TestSeedAssetListingsColdRedisDegradesToTodaysBehaviour — requirement
// 3 of the fix: an empty Redis (first deploy, flushed cache) must seed
// nothing, report it, and leave the reader exactly as it was, still
// able to serve from upstream.
func TestSeedAssetListingsColdRedisDegradesToTodaysBehaviour(t *testing.T) {
	snaps, _ := newTestSnapshots(t)
	up := &snapshotStubReader{}
	reader := v1.NewCachedAssetsReader(up, 2*time.Minute)

	seeded, requested := seedAssetListingsFromSnapshots(context.Background(), discardLogger(), reader, snaps)
	if seeded != 0 {
		t.Fatalf("seeded = %d against an empty Redis", seeded)
	}
	if requested == 0 {
		t.Fatal("requested = 0 — the self-accounting line would report a vacuous success")
	}

	rows, _, _, err := reader.ListAssetsExtAt(context.Background(), timescale.ListAssetsOptions{Limit: 3})
	if err != nil {
		t.Fatalf("ListAssetsExtAt: %v", err)
	}
	if len(rows) == 0 || rows[0].AssetID != "UPSTREAM-GAAA" {
		t.Fatalf("served %+v, want the upstream cold fill — a cold Redis must never produce an empty listing", rows)
	}
}

// TestSeedAssetListingsUnreachableRedisDegrades — same contract when
// Redis is configured but down at boot. No panic, no error, no delay
// beyond the caller's budget: seed nothing and serve normally.
func TestSeedAssetListingsUnreachableRedisDegrades(t *testing.T) {
	snaps, mr := newTestSnapshots(t)
	mr.Close() // Redis goes away before the boot seed runs.

	up := &snapshotStubReader{}
	reader := v1.NewCachedAssetsReader(up, 2*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), assetsListingSnapshotBudget)
	defer cancel()

	seeded, requested := seedAssetListingsFromSnapshots(ctx, discardLogger(), reader, snaps)
	if seeded != 0 || requested == 0 {
		t.Fatalf("seeded=%d requested=%d against an unreachable Redis", seeded, requested)
	}
	rows, _, _, err := reader.ListAssetsExtAt(context.Background(), timescale.ListAssetsOptions{Limit: 3})
	if err != nil {
		t.Fatalf("ListAssetsExtAt: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("an unreachable Redis produced an empty listing")
	}
}

// TestSeedAssetListingsNilStoreIsSafe — Redis unconfigured entirely
// (rdb == nil → newAssetsListingSnapshots returns nil).
func TestSeedAssetListingsNilStoreIsSafe(t *testing.T) {
	if newAssetsListingSnapshots(nil, discardLogger()) != nil {
		t.Fatal("newAssetsListingSnapshots(nil) must return nil")
	}
	reader := v1.NewCachedAssetsReader(&snapshotStubReader{}, 2*time.Minute)
	seeded, requested := seedAssetListingsFromSnapshots(context.Background(), discardLogger(), reader, nil)
	if seeded != 0 || requested != len(assetListingPrewarmOptions()) {
		t.Fatalf("seeded=%d requested=%d with no snapshot store", seeded, requested)
	}
}

// TestSeedAssetListingsRefusesOverAgeSnapshot — the freshness bound.
// /v1/assets rows carry price_usd, market_cap and the change pills, so
// past assetsListingSeedMaxAge the right answer is to pay the cold
// query, not to republish an old valuation under a stale flag.
func TestSeedAssetListingsRefusesOverAgeSnapshot(t *testing.T) {
	snaps, _ := newTestSnapshots(t)
	ctx := context.Background()
	tooOld := time.Now().Add(-assetsListingSeedMaxAge - time.Minute)
	for _, o := range assetListingPrewarmOptions() {
		snaps.save(ctx, o, snapshotRows(), tooOld)
	}

	up := &snapshotStubReader{}
	reader := v1.NewCachedAssetsReader(up, 2*time.Minute)
	if seeded, _ := seedAssetListingsFromSnapshots(ctx, discardLogger(), reader, snaps); seeded != 0 {
		t.Fatalf("seeded %d variants from snapshots older than %s", seeded, assetsListingSeedMaxAge)
	}
	rows, _, _, err := reader.ListAssetsExtAt(ctx, timescale.ListAssetsOptions{Limit: 3})
	if err != nil {
		t.Fatalf("ListAssetsExtAt: %v", err)
	}
	if rows[0].AssetID != "UPSTREAM-GAAA" {
		t.Fatalf("served %+v, want the cold fill after refusing an over-age snapshot", rows)
	}
}

// TestSnapshotsAreNeverLaunderedFresh is the writer/reader loop guard.
//
// The prewarm re-persists on every cycle. If it stamped the WRITE time
// instead of the observation time, a page-set nothing had re-observed
// would have its age reset every 60 s — assetsListingSeedMaxAge would
// never fire, and an indefinitely-stale listing would be seeded as
// though it were minutes old. The prewarm therefore takes its timestamp
// from ListAssetsExtAt, and this test drives that exact path: a cache
// serving a stale entry, re-persisted, must carry the ORIGINAL time.
func TestSnapshotsAreNeverLaunderedFresh(t *testing.T) {
	snaps, _ := newTestSnapshots(t)
	ctx := context.Background()

	// A reader already holding a stale entry for every warmed variant.
	observedAt := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	reader := v1.NewCachedAssetsReader(&snapshotStubReader{}, time.Minute)
	for _, o := range assetListingPrewarmOptions() {
		if !reader.SeedListing(o, snapshotRows(), observedAt) {
			t.Fatalf("SeedListing(limit=%d) refused", o.Limit)
		}
	}

	prewarmAssetListings(ctx, discardLogger(), reader, snaps)

	for _, o := range assetListingPrewarmOptions() {
		snap, ok := snaps.load(ctx, o)
		if !ok {
			t.Fatalf("limit=%d order=%v was not persisted", o.Limit, o.Order)
		}
		if !snap.ObservedAt.Equal(observedAt) {
			t.Fatalf("limit=%d: re-persisted observed_at = %v, want the original %v — "+
				"a stale page-set must not be laundered fresh by being copied forward",
				o.Limit, snap.ObservedAt, observedAt)
		}
	}
}

// TestSnapshotStoreRefusesUnservableValues — an empty page-set or an
// undated read must never reach Redis; either would seed something the
// next boot could not serve or could not label.
func TestSnapshotStoreRefusesUnservableValues(t *testing.T) {
	snaps, mr := newTestSnapshots(t)
	ctx := context.Background()
	opts := timescale.ListAssetsOptions{Limit: 51}

	snaps.save(ctx, opts, nil, time.Now())
	if _, ok := snaps.load(ctx, opts); ok {
		t.Error("an empty page-set was persisted")
	}
	snaps.save(ctx, opts, snapshotRows(), time.Time{})
	if _, ok := snaps.load(ctx, opts); ok {
		t.Error("an undated page-set was persisted")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("Redis holds %v after two refused writes", keys)
	}
}

// TestSnapshotUndecodableValueDegrades — a corrupt or foreign value
// under the key must read as a miss, not a panic or a half-populated
// row-set.
func TestSnapshotUndecodableValueDegrades(t *testing.T) {
	snaps, mr := newTestSnapshots(t)
	opts := timescale.ListAssetsOptions{Limit: 51}
	if err := mr.Set(assetsListingSnapshotRedisKey(opts.Limit, opts.Order).String(), "{not json"); err != nil {
		t.Fatalf("seed miniredis: %v", err)
	}
	if _, ok := snaps.load(context.Background(), opts); ok {
		t.Fatal("an undecodable payload was reported as a usable snapshot")
	}
}

// TestSnapshotSchemaFingerprintKeysOutIncompatibleRows — the JSON
// payload is field-named, so a row shape that has since gained or lost
// a field would deserialise silently with zero values and be SERVED as
// data. The fingerprint is in the key so that is unrepresentable: a
// different AssetRow reads a different key and finds nothing.
func TestSnapshotSchemaFingerprintKeysOutIncompatibleRows(t *testing.T) {
	snaps, mr := newTestSnapshots(t)
	ctx := context.Background()
	opts := timescale.ListAssetsOptions{Limit: 51}
	snaps.save(ctx, opts, snapshotRows(), time.Now())

	// Re-file the value under a foreign fingerprint — what a binary
	// built from a different AssetRow would have written.
	realKey := assetsListingSnapshotRedisKey(opts.Limit, opts.Order).String()
	raw, err := mr.Get(realKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	mr.Del(realKey)
	foreign := strings.Replace(realKey, assetRowSchemaFingerprint(), "000000000000", 1)
	if foreign == realKey {
		t.Fatal("the fingerprint is not part of the key — an incompatible row shape would be decoded and served")
	}
	if err := mr.Set(foreign, raw); err != nil {
		t.Fatalf("set: %v", err)
	}

	if _, ok := snaps.load(ctx, opts); ok {
		t.Fatal("a snapshot written under a different AssetRow shape was read back and would have been served")
	}
}

// TestSnapshotRoundTripPreservesNullableColumns — AssetRow is mostly
// pointer fields (a nil price is NOT a zero price), so the JSON hop has
// to preserve the null/zero distinction or the seed would publish
// "$0.00" where the real answer is "no price".
func TestSnapshotRoundTripPreservesNullableColumns(t *testing.T) {
	snaps, _ := newTestSnapshots(t)
	ctx := context.Background()
	opts := timescale.ListAssetsOptions{Limit: 51}

	price, zero := "1.2345", "0"
	rows := []timescale.AssetRow{
		{AssetID: "PRICED-GAAA", PriceUSD: &price, Volume24hUSD: &zero},
		{AssetID: "UNPRICED-GAAA"}, // every pointer nil
	}
	observedAt := time.Now().UTC().Truncate(time.Second)
	snaps.save(ctx, opts, rows, observedAt)

	snap, ok := snaps.load(ctx, opts)
	if !ok {
		t.Fatal("round trip lost the snapshot")
	}
	if !snap.ObservedAt.Equal(observedAt) {
		t.Errorf("observed_at = %v, want %v", snap.ObservedAt, observedAt)
	}
	if len(snap.Rows) != 2 {
		t.Fatalf("rows = %+v", snap.Rows)
	}
	if snap.Rows[0].PriceUSD == nil || *snap.Rows[0].PriceUSD != price {
		t.Errorf("price_usd = %v, want %q", snap.Rows[0].PriceUSD, price)
	}
	if snap.Rows[0].Volume24hUSD == nil || *snap.Rows[0].Volume24hUSD != zero {
		t.Errorf("volume_24h_usd = %v, want the explicit %q", snap.Rows[0].Volume24hUSD, zero)
	}
	if snap.Rows[1].PriceUSD != nil {
		t.Errorf("an unpriced row came back with price_usd = %q — nil must not become a value",
			*snap.Rows[1].PriceUSD)
	}
}

// TestSnapshotTTLOutlivesTheSeedAgeBound — the age check, not the TTL,
// must be what refuses an old snapshot, so an over-age one is logged as
// refused rather than disappearing into an indistinguishable miss.
func TestSnapshotTTLOutlivesTheSeedAgeBound(t *testing.T) {
	if assetsListingSnapshotTTL <= assetsListingSeedMaxAge {
		t.Fatalf("snapshot TTL %s <= seed max age %s: expiry would mask the age refusal",
			assetsListingSnapshotTTL, assetsListingSeedMaxAge)
	}
}

// TestSnapshotPayloadIsSelfDescribing — the persisted JSON must carry
// its observation time under a stable name; the whole staleness
// contract downstream reads it.
func TestSnapshotPayloadIsSelfDescribing(t *testing.T) {
	buf, err := json.Marshal(assetsListingSnapshot{ObservedAt: time.Unix(1, 0).UTC(), Rows: snapshotRows()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(buf, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"observed_at", "rows"} {
		if _, ok := probe[field]; !ok {
			t.Errorf("payload has no %q field: %s", field, buf)
		}
	}
}
