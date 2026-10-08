package divergence_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// newTestService wires a Service against an in-memory miniredis +
// the supplied references. Returns the service, the redis client
// (for direct assertions), and the miniredis handle.
func newTestService(t *testing.T, refs []divergence.Reference, opts divergence.ServiceOptions) (*divergence.Service, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	opts.References = refs
	opts.Cache = rdb
	svc, err := divergence.NewService(opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, rdb, mr
}

// refreshQuiet is the only sound "must not fire" check under the default
// WarningPersistence debounce, which holds WarningFired false on a pair's
// first raw firing whatever the gate under test decides. It refreshes twice,
// a full default window apart, and after each asserts that neither the
// published verdict nor the raw condition (FiringSince, zero while clear)
// fired. Returns the last cached result.
func refreshQuiet(t *testing.T, svc *divergence.Service, rdb *redis.Client,
	pair canonical.Pair, ourPrice float64, t0 time.Time,
) divergence.CachedResult {
	t.Helper()
	var cached divergence.CachedResult
	for i, at := range []time.Time{t0, t0.Add(divergence.DefaultWarningPersistence + time.Minute)} {
		if err := svc.RefreshPair(context.Background(), pair, ourPrice, at); err != nil {
			t.Fatalf("RefreshPair #%d: %v", i+1, err)
		}
		cached = readDivergence(t, rdb, pair)
		if cached.WarningFired || !cached.FiringSince.IsZero() {
			t.Errorf("refresh #%d of %s at our=%g: WarningFired=%v FiringSince=%v, want quiet "+
				"(no published warning and no raw firing)", i+1, pair, ourPrice, cached.WarningFired, cached.FiringSince)
		}
	}
	return cached
}

// TestNewService_RequiresCache — operator misconfig that omits the
// cache should fail loudly at construction, not silently skip writes.
func TestNewService_RequiresCache(t *testing.T) {
	_, err := divergence.NewService(divergence.ServiceOptions{
		References: []divergence.Reference{&stubReference{name: "a", price: 1}},
	})
	if err == nil {
		t.Fatal("expected error when Cache is nil")
	}
}

// TestRefreshPair_NoReferencesIsNoop — empty References list yields
// no Redis writes and no error.
func TestRefreshPair_NoReferencesIsNoop(t *testing.T) {
	svc, _, mr := newTestService(t, nil, divergence.ServiceOptions{})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Errorf("RefreshPair on empty refs: %v", err)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("no-op should not write redis; got keys %v", keys)
	}
}

// TestRefreshPair_HappyPath — references agree with our value;
// CachedResult writes to Redis, WarningFired=false.
func TestRefreshPair_HappyPath(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{})

	cached := refreshQuiet(t, svc, rdb, xlmUSD(t), 1.00, time.Now())
	if cached.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3", cached.SuccessCount)
	}
	if cached.DivergencePct > 0.001 {
		t.Errorf("DivergencePct = %g, want ~0", cached.DivergencePct)
	}
}

// TestRefreshPair_FiresWarning — references agree on a price that
// disagrees with our value by > threshold; WarningFired=true.
// TestRefreshPair_OnWarningFiredEdgeOnly pins that the OnWarningFired hook fires only on the
// `below threshold → above threshold` edge, not on every refresh
// while a divergence stays elevated. Multiple consecutive
// above-threshold refreshes must produce one hook call; a return
// to below-threshold + re-cross re-arms the latch.
func TestRefreshPair_OnWarningFiredEdgeOnly(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	var fired int
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1, // isolate the edge-latch behaviour from the W3-guards-2 debounce
		OnWarningFired: func(_ context.Context, _ canonical.Pair, _ divergence.CachedResult) error {
			fired++
			return nil
		},
	})
	ctx := context.Background()
	// 1) Below threshold (1%) → no fire.
	_ = svc.RefreshPair(ctx, xlmUSD(t), 1.01, time.Now())
	if fired != 0 {
		t.Fatalf("fired=%d after below-threshold refresh, want 0", fired)
	}
	// 2) Above threshold (10%) → first fire.
	_ = svc.RefreshPair(ctx, xlmUSD(t), 1.10, time.Now())
	if fired != 1 {
		t.Fatalf("fired=%d after first above-threshold, want 1", fired)
	}
	// 3) Still above threshold → no second fire (latch held).
	_ = svc.RefreshPair(ctx, xlmUSD(t), 1.12, time.Now())
	if fired != 1 {
		t.Fatalf("fired=%d on still-elevated refresh, want 1 (latch should hold)", fired)
	}
	// 4) Drop below threshold → latch resets (no fire on the way down).
	_ = svc.RefreshPair(ctx, xlmUSD(t), 1.02, time.Now())
	if fired != 1 {
		t.Fatalf("fired=%d on recovery refresh, want 1", fired)
	}
	// 5) Re-cross threshold → second fire.
	_ = svc.RefreshPair(ctx, xlmUSD(t), 1.10, time.Now())
	if fired != 2 {
		t.Fatalf("fired=%d after re-cross, want 2 (latch should re-arm)", fired)
	}
}

func TestRefreshPair_FiresWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0, // 5% threshold
		MinSourcesForWarning: 2,
		WarningPersistence:   -1, // isolate the threshold gate from the W3-guards-2 debounce
	})

	// Our price is 10% above the consensus.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	_ = json.Unmarshal(body, &cached)
	if !cached.WarningFired {
		t.Errorf("WarningFired = false on 10%% deviation, want true")
	}
}

// TestRefreshPair_MedianLegVetoedByMajorityAgreement pins that at the
// quorum floor (SuccessCount==2) the median is an arithmetic mean, so
// one reference off by more than 2×threshold must not fire the warning
// when the other reference agreed exactly. threshold=5%,
// ourPrice=50000, Sources={50000 (agrees), 56000 (+12%, disagrees)}:
// Median=53000, DivergencePct≈5.66%>5, but 1 of the 2 (a majority)
// corroborates us, so the median leg must be vetoed.
func TestRefreshPair_MedianLegVetoedByMajorityAgreement(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "agrees", price: 50000},
		&stubReference{name: "stale-fx-cross", price: 56000},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1, // isolate the veto from the W3-guards-2 debounce
	})

	cached := refreshQuiet(t, svc, rdb, xlmUSD(t), 50000, time.Now())
	if cached.AgreementCount != 1 {
		t.Fatalf("AgreementCount = %d, want 1 (sanity: exactly one reference should agree)", cached.AgreementCount)
	}
	if cached.DivergencePct <= 5.0 {
		t.Fatalf("DivergencePct = %g, want > 5 (sanity: the median itself must exceed threshold)", cached.DivergencePct)
	}
}

// TestRefreshPair_ZeroAgreementLegStillFires guards the leg the fix
// above must NOT touch: MNY-22's symmetric-disagreement case, where
// two references straddle ourPrice (median ≈ ourPrice, DivergencePct
// small) but neither individually agrees. AgreementCount==0 must still
// fire regardless of the majority-veto.
func TestRefreshPair_ZeroAgreementLegStillFires(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "high", price: 1.08},
		&stubReference{name: "low", price: 0.92},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1,
	})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	cached := readDivergence(t, rdb, xlmUSD(t))
	if cached.AgreementCount != 0 {
		t.Fatalf("AgreementCount = %d, want 0 (sanity: neither reference should individually agree)", cached.AgreementCount)
	}
	if !cached.WarningFired {
		t.Errorf("WarningFired = false with AgreementCount=0, want true (MNY-22 leg must survive the #1041 median-leg veto)")
	}
}

// TestRefreshPair_BelowMinSourcesNoWarning — even when divergence
// is huge, fewer than MinSourcesForWarning successful references
// suppresses the warning. Single-source disagreement shouldn't fire.
func TestRefreshPair_BelowMinSourcesNoWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "only", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2, // require 2+ agreeing sources
	})
	// A single source 50% away must not fire; it should require ≥ 2.
	cached := refreshQuiet(t, svc, rdb, xlmUSD(t), 1.50, time.Now())
	// But the comparator's data should still be cached so operators
	// can see what one source thinks.
	if cached.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", cached.SuccessCount)
	}
}

// TestRefreshPair_TTLApplied — Redis TTL on the cache entry matches
// cachekeys.DivergenceTTL.
func TestRefreshPair_TTLApplied(t *testing.T) {
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	ttl := mr.TTL(cachekeys.Divergence(xlmUSD(t)).String())
	if ttl == 0 || ttl > cachekeys.DivergenceTTL {
		t.Errorf("TTL = %v, want ≤ %v and > 0", ttl, cachekeys.DivergenceTTL)
	}
}

// TestLookupCached_PresentEntry — RefreshPair → LookupCached round
// trips the entry preserving every field.
func TestLookupCached_PresentEntry(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.05},
		&stubReference{name: "b", price: 1.05},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 1.0, MinSourcesForWarning: 2, WarningPersistence: -1})

	pair := xlmUSD(t)
	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	cached, found, err := svc.LookupCached(context.Background(), pair.Base)
	if err != nil {
		t.Fatalf("LookupCached: %v", err)
	}
	if !found {
		t.Fatal("LookupCached returned found=false on a freshly-cached entry")
	}
	if cached.Detail.PairID != pair.String() {
		t.Errorf("PairID = %q, want %q", cached.Detail.PairID, pair.String())
	}
	if cached.Detail.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2", cached.Detail.SuccessCount)
	}
	if !cached.Checked {
		t.Error("Checked = false with two references at a quorum of two")
	}
	// 1.05 vs 1.00 = ~4.76% deviation. Threshold 1.0% → warning fires.
	if !cached.Firing {
		t.Errorf("Firing = false, expected true (4.76%% > 1%% threshold)")
	}
}

// TestLookupCached_AbsentEntry — querying an asset with no cached
// result returns (zero, false, nil) — not an error.
func TestLookupCached_AbsentEntry(t *testing.T) {
	svc, _, _ := newTestService(t, nil, divergence.ServiceOptions{})
	_, found, err := svc.LookupCached(context.Background(), canonical.NativeAsset())
	if err != nil {
		t.Errorf("LookupCached on absent entry: %v", err)
	}
	if found {
		t.Errorf("found = true on absent entry")
	}
}

// xlmPair builds native/<quote-fiat> for the per-base-asset
// aggregation tests.
func xlmPair(t *testing.T, quoteFiat string) canonical.Pair {
	t.Helper()
	q, err := canonical.ParseAsset("fiat:" + quoteFiat)
	if err != nil {
		t.Fatalf("parse %s: %v", quoteFiat, err)
	}
	return canonical.Pair{Base: canonical.NativeAsset(), Quote: q}
}

// TestLookupCached_PerPairOR_OrderIndependent pins that
// the by-asset reader must report "firing if ANY quote diverges"
// regardless of the order the worker refreshes the base's pairs. A
// per-base key would let the LAST pair refreshed clobber the
// asset's verdict — so XLM/USD diverging but XLM/GBP not would clear
// the warning if GBP refreshed last. Per-pair keys + the OR
// across the base index keep the verdict stable.
func TestLookupCached_PerPairOR_OrderIndependent(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
	}
	xlm := canonical.NativeAsset()
	usdPair := xlmPair(t, "USD")
	gbpPair := xlmPair(t, "GBP")
	ctx := context.Background()

	// Two refresh orderings; both must yield firing=true for the base.
	// Ordering 1: USD (diverging) first, GBP (in-tolerance) last —
	// this is the regression case (GBP would have cleared the base key
	// under a per-base layout).
	// Ordering 2: GBP first, USD last.
	type step struct {
		pair  canonical.Pair
		price float64
	}
	eurPair := xlmPair(t, "EUR")
	for _, tc := range []struct {
		name       string
		order      []step
		firingPair canonical.Pair // expected representative detail row
	}{
		{
			name:       "diverging_first_clean_last",
			order:      []step{{usdPair, 1.50}, {gbpPair, 1.00}},
			firingPair: usdPair,
		},
		{
			name:       "clean_first_diverging_last",
			order:      []step{{gbpPair, 1.00}, {usdPair, 1.50}},
			firingPair: usdPair,
		},
		{
			// Robustness against Redis set-iteration order: the FIRING
			// quote (EUR) sorts lexicographically BEFORE the clean
			// quote (USD), so miniredis's sorted SMembers returns the
			// clean pair LAST. A naive "last value wins" aggregation
			// (a per-base clobber) would read the base
			// verdict as false here — only a true OR keeps it firing.
			name:       "firing_quote_sorts_first",
			order:      []step{{eurPair, 1.50}, {usdPair, 1.00}},
			firingPair: eurPair,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
				Threshold:            5.0,
				MinSourcesForWarning: 2,
				WarningPersistence:   -1, // isolate the per-pair OR aggregation from the W3-guards-2 debounce
			})
			for _, s := range tc.order {
				if err := svc.RefreshPair(ctx, s.pair, s.price, time.Now()); err != nil {
					t.Fatalf("RefreshPair %s: %v", s.pair, err)
				}
			}

			cached, found, err := svc.LookupCached(ctx, xlm)
			if err != nil {
				t.Fatalf("LookupCached: %v", err)
			}
			if !found {
				t.Fatal("found=false after refreshing two pairs for the base")
			}
			if !cached.Firing {
				t.Errorf("WarningFired=false; want true — a quote diverges so the "+
					"base verdict must fire regardless of refresh / iteration order "+
					"(got pair_id=%q)", cached.Detail.PairID)
			}
			// The representative detail row should be the FIRING pair,
			// not the clean one — independent of Redis set order.
			if cached.Detail.PairID != tc.firingPair.String() {
				t.Errorf("representative PairID = %q, want the firing pair %q",
					cached.Detail.PairID, tc.firingPair.String())
			}
		})
	}
}

// TestLookupCached_PerPairOR_AllClean — when every quote is within
// tolerance the base verdict must be false (no false positives from
// the OR aggregation).
func TestLookupCached_PerPairOR_AllClean(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	ctx := context.Background()
	for _, p := range []canonical.Pair{xlmPair(t, "USD"), xlmPair(t, "GBP")} {
		refreshQuiet(t, svc, rdb, p, 1.00, time.Now())
	}
	cached, found, err := svc.LookupCached(ctx, canonical.NativeAsset())
	if err != nil {
		t.Fatalf("LookupCached: %v", err)
	}
	if !found {
		t.Fatal("found=false after refreshing two clean pairs")
	}
	if cached.Firing {
		t.Errorf("WarningFired=true with every quote in-tolerance; want false")
	}
}

// TestRefreshPair_DefaultsApplied — zero-value options use sensible
// defaults: 5% threshold, 2 min-sources, 5s timeout.
func TestRefreshPair_DefaultsApplied(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	// Zero-value options: defaults should kick in (5% threshold,
	// 2 min sources). 4% deviation → no warning.
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{})
	base := time.Now()
	refreshQuiet(t, svc, rdb, xlmUSD(t), 1.04, base)

	// 6% deviation → warning fires, but only once the divergence has
	// PERSISTED past the default debounce window (W3-guards-2). The
	// first refresh must NOT fire — that is the fast-move false-warning
	// class the debounce removes; a sustained 6% gap must clear it.
	t0 := base.Add(time.Hour)
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.06, t0); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	cached := readDivergence(t, rdb, xlmUSD(t))
	if cached.WarningFired || cached.FiringSince.IsZero() {
		t.Errorf("6%% deviation on the first refresh: WarningFired=%v FiringSince=%v, want the raw "+
			"condition recorded but the warning held by the default debounce", cached.WarningFired, cached.FiringSince)
	}

	// Second refresh, same 6% gap, one debounce window later → fires.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.06,
		t0.Add(divergence.DefaultWarningPersistence+time.Minute)); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	cached = readDivergence(t, rdb, xlmUSD(t))
	if !cached.WarningFired {
		t.Errorf("6%% deviation should fire under default 5%% threshold once it persists past the debounce")
	}
}

// recordingObservationSink captures every RecordObservation call so
// tests can pin the worker fires per-reference rows.
type recordingObservationSink struct {
	records []divergence.ObservationRecord
}

func (r *recordingObservationSink) RecordObservation(_ context.Context, obs divergence.ObservationRecord) error {
	r.records = append(r.records, obs)
	return nil
}

// TestRefreshPair_FiresObservationSink — when a sink is wired, the
// worker must call it once per (pair, reference) tuple per refresh
// with the right deltas + firing flag.
func TestRefreshPair_FiresObservationSink(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "coingecko", price: 1.00},
	}
	sink := &recordingObservationSink{}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 1,
		ObservationSink:      sink,
	})

	// Our price is 10% above both refs — both observations should
	// be recorded with status=firing.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	if len(sink.records) != 2 {
		t.Fatalf("sink got %d records, want 2 (one per reference)", len(sink.records))
	}
	for _, r := range sink.records {
		if !r.Firing {
			t.Errorf("ref %s: Firing=false, want true (10%% delta exceeds 5%% threshold)", r.Reference)
		}
		// ADR-0003: OurPrice/RefPrice/DeltaPct are decimal
		// strings, never float64 — asserting exact strings (not just
		// "parses to a float") pins the shortest-round-trip format
		// the worker must emit, matching the read side's ::text cast.
		if r.OurPrice != "1.1" {
			t.Errorf("ref %s: OurPrice = %q, want decimal string \"1.1\"", r.Reference, r.OurPrice)
		}
		if r.RefPrice != "1" {
			t.Errorf("ref %s: RefPrice = %q, want decimal string \"1\"", r.Reference, r.RefPrice)
		}
		// (1.10 - 1.00) / 1.00 * 100 = 10
		delta, err := strconv.ParseFloat(r.DeltaPct, 64)
		if err != nil {
			t.Fatalf("ref %s: DeltaPct = %q is not a decimal string: %v", r.Reference, r.DeltaPct, err)
		}
		if delta < 9.99 || delta > 10.01 {
			t.Errorf("ref %s: DeltaPct = %q, want ~10", r.Reference, r.DeltaPct)
		}
	}
}

// TestRefreshPair_NoSinkIsLegacyBehaviour — the pre-Phase-2 default
// (no sink) keeps the legacy Redis-only path working unchanged.
func TestRefreshPair_NoSinkIsLegacyBehaviour(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		// ObservationSink: nil (default)
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	// No sink, no records — but no panic, no error. Legacy
	// behaviour preserved.
}

// TestRefreshPair_ObservationStampedWithComparisonTime pins observed_at. The durable divergence_observations row answers "when
// did this divergence occur", and observed_at is part of its conflict
// key — so it must carry the comparison instant the caller supplied
// (the same instant handed to every Reference), not the wall clock at
// flush time, which trails it by the whole reference fan-out.
func TestRefreshPair_ObservationStampedWithComparisonTime(t *testing.T) {
	// A comparison time deliberately far from wall-clock now, so a
	// time.Now() stamp cannot coincidentally pass.
	comparedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	sink := &recordingObservationSink{}
	svc, _, _ := newTestService(t, []divergence.Reference{
		&stubReference{name: "chainlink", price: 1.00},
	}, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 1,
		ObservationSink:      sink,
	})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, comparedAt); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if len(sink.records) != 1 {
		t.Fatalf("sink got %d records, want 1", len(sink.records))
	}
	if got := sink.records[0].ObservedAt; !got.Equal(comparedAt) {
		t.Errorf("ObservedAt = %v, want the comparison time %v (wall-clock write time misattributes when the divergence occurred)",
			got, comparedAt)
	}
}

// TestRefreshPair_ObservationCarriesReferenceAsOf — each row records when
// ITS reference observed the price, not only the comparison time:
// two references answering the same comparison from different instants must
// persist different RefObservedAt values, so a reader can tell a fresh quote
// from a 50-minute-old one that still passed the comparability ceiling.
func TestRefreshPair_ObservationCarriesReferenceAsOf(t *testing.T) {
	comparedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	freshAsOf := comparedAt.Add(-30 * time.Second)
	agedAsOf := comparedAt.Add(-50 * time.Minute)

	sink := &recordingObservationSink{}
	svc, _, _ := newTestService(t, []divergence.Reference{
		&stubReference{name: "chainlink", price: 1.00, asOf: freshAsOf},
		&stubReference{name: "coingecko", price: 1.01, asOf: agedAsOf},
	}, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 1,
		ObservationSink:      sink,
	})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, comparedAt); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	want := map[string]time.Time{"chainlink": freshAsOf, "coingecko": agedAsOf}
	if len(sink.records) != len(want) {
		t.Fatalf("sink got %d records, want %d", len(sink.records), len(want))
	}
	for _, r := range sink.records {
		if !r.RefObservedAt.Equal(want[r.Reference]) {
			t.Errorf("ref %s: RefObservedAt = %v, want the quote's AsOf %v",
				r.Reference, r.RefObservedAt, want[r.Reference])
		}
		if !r.ObservedAt.Equal(comparedAt) {
			t.Errorf("ref %s: ObservedAt = %v, want the comparison time %v", r.Reference, r.ObservedAt, comparedAt)
		}
	}
}

// TestRefreshPair_StampsVerdictWindow — the cached verdict names the
// aggregation window its OurPrice was computed over, so an API
// surface serving a different window can tell the verdict does not speak to
// its value; a service built without a window leaves the verdict unscoped.
func TestRefreshPair_StampsVerdictWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{"shortest window recorded", 5 * time.Minute, 5 * time.Minute},
		{"no window configured", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rdb, _ := newTestService(t, []divergence.Reference{
				&stubReference{name: "chainlink", price: 1.00},
				&stubReference{name: "coingecko", price: 1.00},
			}, divergence.ServiceOptions{Threshold: 5.0, MinSourcesForWarning: 2, Window: tc.window})
			if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
				t.Fatalf("RefreshPair: %v", err)
			}
			if got := readDivergence(t, rdb, xlmUSD(t)).WindowSeconds; got != int(tc.want/time.Second) {
				t.Errorf("cached WindowSeconds = %d, want %d", got, int(tc.want/time.Second))
			}
			_, checked, window, err := svc.LookupCachedPairVerdict(context.Background(), xlmUSD(t))
			if err != nil || !checked {
				t.Fatalf("LookupCachedPairVerdict: checked=%v err=%v, want a checked verdict", checked, err)
			}
			if window != tc.want {
				t.Errorf("verdict window = %v, want %v", window, tc.want)
			}
		})
	}
}

// TestRefreshPair_ObservationFallsBackWhenNoComparisonTime — a caller
// that supplies no comparison time keeps the previous behaviour (the
// worker's own computed-at) rather than persisting a zero timestamp
// into observed_at, which is part of the row's conflict key.
func TestRefreshPair_ObservationFallsBackWhenNoComparisonTime(t *testing.T) {
	sink := &recordingObservationSink{}
	svc, _, _ := newTestService(t, []divergence.Reference{
		&stubReference{name: "chainlink", price: 1.00},
	}, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 1,
		ObservationSink:      sink,
	})

	before := time.Now().UTC()
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Time{}); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if len(sink.records) != 1 {
		t.Fatalf("sink got %d records, want 1", len(sink.records))
	}
	got := sink.records[0].ObservedAt
	if got.IsZero() {
		t.Fatal("ObservedAt is zero; a zero comparison time must fall back to the computed-at stamp")
	}
	if got.Before(before) {
		t.Errorf("ObservedAt = %v, want at/after the call start %v", got, before)
	}
}

// TestNoUnfalsifiableWarningNegatives keeps the single-refresh "must not
// fire" out of this package: under the default WarningPersistence debounce,
// `if x.WarningFired {` after fewer than two refreshes passes whatever the
// gate under test decides. Use refreshQuiet, or also assert FiringSince.
func TestNoUnfalsifiableWarningNegatives(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob test files: %v (found %d)", err, len(files))
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			scanned++
			for _, pos := range unfalsifiableWarningNegatives(fn) {
				t.Errorf("%s: %s asserts WarningFired is false after fewer than two refreshes without "+
					"observing FiringSince — unfalsifiable under the default debounce; use refreshQuiet",
					fset.Position(pos), fn.Name.Name)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no Test functions scanned")
	}
}

// unfalsifiableWarningNegatives returns each `if <x>.WarningFired {` in fn
// preceded by fewer than two refreshes, unless fn disables the debounce.
func unfalsifiableWarningNegatives(fn *ast.FuncDecl) []token.Pos {
	var refreshAt, negatives []token.Pos
	debounceOff := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch fun := x.Fun.(type) {
			case *ast.SelectorExpr:
				if fun.Sel.Name == "RefreshPair" {
					refreshAt = append(refreshAt, x.Pos())
				}
			case *ast.Ident:
				if fun.Name == "refreshQuiet" {
					refreshAt = append(refreshAt, x.Pos(), x.Pos())
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "WarningPersistence" && types.ExprString(x.Value) == "-1" {
				debounceOff = true
			}
		case *ast.IfStmt:
			if sel, ok := x.Cond.(*ast.SelectorExpr); ok && sel.Sel.Name == "WarningFired" {
				negatives = append(negatives, x.Pos())
			}
		}
		return true
	})
	if debounceOff {
		return nil
	}
	var out []token.Pos
	for _, neg := range negatives {
		before := 0
		for _, r := range refreshAt {
			if r < neg {
				before++
			}
		}
		if before < 2 {
			out = append(out, neg)
		}
	}
	return out
}

// A hook error leaves the episode undelivered, so the next still-firing
// refresh retries instead of the notification being dropped.
func TestRefreshPair_HookErrorReleasesLatch(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	var calls int
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) error {
			calls++
			if calls == 1 {
				return errors.New("transient")
			}
			return nil
		},
	})
	ctx := context.Background()
	for i, want := range []int{1, 2, 2} {
		_ = svc.RefreshPair(ctx, xlmUSD(t), 1.10, time.Now())
		if calls != want {
			t.Fatalf("refresh %d: hook calls=%d, want %d", i+1, calls, want)
		}
	}
}
