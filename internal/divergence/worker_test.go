package divergence_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
		WarningPersistence:   -1, // isolate the edge-latch behaviour from the persistence debounce
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
		WarningPersistence:   -1, // isolate the threshold gate from the persistence debounce
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
		WarningPersistence:   -1, // isolate the veto from the persistence debounce
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
// above must NOT touch: the symmetric-disagreement case, where
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
				WarningPersistence:   -1, // isolate the per-pair OR aggregation from the persistence debounce
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
	// PERSISTED past the default debounce window. The
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

// stubReference is a Reference implementation that returns canned
// responses. Used to drive Compare's logic deterministically
// without depending on real HTTP.
type stubReference struct {
	name  string
	price float64
	err   error
	delay time.Duration
	asOf  time.Time // zero: observed at the comparison time
}

func (s *stubReference) Name() string { return s.name }

func (s *stubReference) LookupQuote(ctx context.Context, _ canonical.Pair, observedAt time.Time) (divergence.Quote, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return divergence.Quote{}, ctx.Err()
		}
	}
	asOf := s.asOf
	if asOf.IsZero() {
		asOf = freshAt(observedAt)
	}
	return divergence.Quote{Price: s.price, AsOf: asOf}, s.err
}

// freshAt is the as-of of a quote observed at the comparison instant.
func freshAt(observedAt time.Time) time.Time {
	if observedAt.IsZero() {
		return time.Now().UTC()
	}
	return observedAt
}

// priceOf drops a quote's as-of for assertions on the price alone.
func priceOf(q divergence.Quote, err error) (float64, error) { return q.Price, err }

// xlmUSD is a convenient test pair.
func xlmUSD(t *testing.T) canonical.Pair {
	t.Helper()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	return canonical.Pair{Base: canonical.NativeAsset(), Quote: usd}
}

// TestCompare_AllAgree — every reference returns the same price as
// our value. DivergencePct = 0; SuccessCount = N.
func TestCompare_AllAgree(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 0.10},
		&stubReference{name: "b", price: 0.10},
		&stubReference{name: "c", price: 0.10},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3", res.SuccessCount)
	}
	if res.Median != 0.10 {
		t.Errorf("Median = %g, want 0.10", res.Median)
	}
	if res.DivergencePct != 0 {
		t.Errorf("DivergencePct = %g, want 0", res.DivergencePct)
	}
}

// TestCompare_ConsensusAgrees_OurValueOff — references agree but
// our value is 10% off. DivergencePct ≈ 10.
func TestCompare_ConsensusAgrees_OurValueOff(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.10, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d", res.SuccessCount)
	}
	if got := res.DivergencePct; got < 9.9 || got > 10.1 {
		t.Errorf("DivergencePct = %g, want ~10", got)
	}
}

// TestCompare_ReferencesDisagree_MedianHandlesIt — three sources;
// one outlier. Median is robust against the outlier so DivergencePct
// is computed against the consensus, not the average.
func TestCompare_ReferencesDisagree_MedianHandlesIt(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "outlier", price: 100.00}, // ridiculous outlier
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if res.Median != 1.00 {
		t.Errorf("Median = %g, want 1.00 (outlier shouldn't move median)", res.Median)
	}
	if res.DivergencePct > 0.1 {
		t.Errorf("DivergencePct = %g, want ~0", res.DivergencePct)
	}
}

// TestCompare_AssetUnsupported — sentinel error gets a stable
// failure label, distinguishable from generic transport errors.
func TestCompare_AssetUnsupported(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrAssetUnsupported},
		&stubReference{name: "b", price: 1.00},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if res.Failures["a"] != "asset_unsupported" {
		t.Errorf("Failures[a] = %q, want asset_unsupported", res.Failures["a"])
	}
	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", res.SuccessCount)
	}
}

// TestCompare_PriceUnavailable — vendor outage sentinel maps to a
// distinct failure label.
func TestCompare_PriceUnavailable(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrPriceUnavailable},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if res.Failures["a"] != "price_unavailable" {
		t.Errorf("Failures[a] = %q, want price_unavailable", res.Failures["a"])
	}
}

// TestCompare_GenericErrorPassesThrough — non-sentinel error
// surfaces as its verbatim message, NOT as a sentinel label.
// Operator can grep dashboards for the actual cause.
func TestCompare_GenericErrorPassesThrough(t *testing.T) {
	weird := errors.New("connection reset by peer")
	refs := []divergence.Reference{
		&stubReference{name: "a", err: weird},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if res.Failures["a"] != "connection reset by peer" {
		t.Errorf("Failures[a] = %q, want verbatim error", res.Failures["a"])
	}
}

// TestCompare_NoReferencesIsClean — empty refs produces a Result
// with SuccessCount=0 and zero divergence; not an error.
func TestCompare_NoReferencesIsClean(t *testing.T) {
	res := divergence.Compare(context.Background(), nil, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 0 || res.DivergencePct != 0 {
		t.Errorf("empty refs: %+v", res)
	}
}

// TestCompare_MinSuccessForMedian_Honored — when fewer than the
// configured minimum references succeed, DivergencePct stays 0
// even if SuccessCount > 0.
func TestCompare_MinSuccessForMedianHonored(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
	}
	opts := divergence.CompareOptions{MinSuccessForMedian: 2}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.50, time.Now(), opts)
	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", res.SuccessCount)
	}
	if res.DivergencePct != 0 {
		t.Errorf("DivergencePct = %g, want 0 (below MinSuccessForMedian)", res.DivergencePct)
	}
}

// TestCompare_TimeoutBoundsSlowReference — a reference that takes
// longer than the per-reference timeout is recorded as a failure;
// the others still complete.
func TestCompare_TimeoutBoundsSlowReference(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "fast", price: 1.00},
		&stubReference{name: "slow", price: 1.00, delay: 200 * time.Millisecond},
	}
	opts := divergence.CompareOptions{PerReferenceTimeout: 50 * time.Millisecond}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), opts)
	if _, ok := res.Sources["fast"]; !ok {
		t.Errorf("fast reference should have succeeded")
	}
	if _, ok := res.Failures["slow"]; !ok {
		t.Errorf("slow reference should have timed out and landed in Failures")
	}
}

// TestCompare_NonFinitePriceRejected — Inf / NaN / zero / negative
// prices land in Failures, never in Sources.
func TestCompare_NonFinitePriceRejected(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "neg", price: -1.0},
		&stubReference{name: "zero", price: 0.0},
		// NaN check would need a float bit-trick; covered by
		// math.NaN() in real-world parse failures.
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(), divergence.CompareOptions{})
	if len(res.Sources) != 0 {
		t.Errorf("non-positive prices should not be in Sources, got %v", res.Sources)
	}
	if len(res.Failures) != 2 {
		t.Errorf("Failures count = %d, want 2", len(res.Failures))
	}
}

// TestCompare_EvenCountMedian — 4 references; median is mean of
// middle two values.
func TestCompare_EvenCountMedian(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 0.95},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.10},
		&stubReference{name: "d", price: 1.20},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.05, time.Now(), divergence.CompareOptions{})
	// sorted: [0.95, 1.00, 1.10, 1.20] → median = (1.00 + 1.10) / 2 = 1.05
	if got := res.Median; got < 1.04 || got > 1.06 {
		t.Errorf("Median = %g, want ~1.05", got)
	}
}

// ─── CoinGecko reference tests ─────────────────────────────────────

// TestCoinGecko_HappyPath — typical /simple/price response decodes,
// returns the mapped price. The reference batches every configured
// (id, quote) pair into a single request, so we assert the URL
// contains the expected id + quote (alongside others from the
// merged defaults) rather than equality.
func TestCoinGecko_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm we hit the right path with a batched query.
		if r.URL.Path != "/simple/price" {
			t.Errorf("path = %q", r.URL.Path)
		}
		ids := r.URL.Query().Get("ids")
		quotes := r.URL.Query().Get("vs_currencies")
		if !strings.Contains(ids, "stellar") {
			t.Errorf("ids = %q, want to contain 'stellar'", ids)
		}
		if !strings.Contains(quotes, "usd") {
			t.Errorf("vs_currencies = %q, want to contain 'usd'", quotes)
		}
		w.Header().Set("Content-Type", "application/json")
		// last_updated_at mirrors the real /simple/price shape: the
		// reference always requests include_last_updated_at=true and
		// rejects a response that omits it (fail-closed gate).
		_, _ = fmt.Fprintf(w, `{"stellar": {"usd": 0.07142, "last_updated_at": %d}}`, time.Now().Unix())
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL: ts.URL,
		IDMap:   map[string]string{"native": "stellar"},
	})

	price, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if price < 0.07140 || price > 0.07144 {
		t.Errorf("price = %g, want ~0.07142", price)
	}
}

// TestCoinGecko_AssetNotInIDMap — an asset that's neither in the
// operator's IDMap nor in the built-in default falls back to
// ErrAssetUnsupported (not a transport error). The bare native /
// XLM / BTC / ETH paths are now covered by the built-in default,
// so we use a deliberately-unknown synthetic asset to exercise
// the unsupported branch.
func TestCoinGecko_AssetNotInIDMap(t *testing.T) {
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		IDMap: map[string]string{}, // empty — relies on built-in default
	})
	// Build a pair whose base ISN'T in the default IDMap. A classic
	// SEP-1 asset (long-tail issuer) is parseable but not curated;
	// the lookup should return ErrAssetUnsupported.
	base, err := canonical.ParseAsset("AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("ParseAsset(AQUA-...): %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset(fiat:USD): %v", err)
	}
	pair := canonical.Pair{Base: base, Quote: usd}
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now())); !errors.Is(err, divergence.ErrAssetUnsupported) {
		t.Errorf("err = %v, want ErrAssetUnsupported", err)
	}
}

// TestCoinGecko_DefaultIDMapCoversCommonPairs — a stock deployment
// (no operator IDMap) MUST recognise the canonical asset_id forms
// the aggregator computes by default. An empty IDMap would make every
// pair return ErrAssetUnsupported, leaving divergence_observations
// silently empty even though Compare's "ok" counter incremented.
func TestCoinGecko_DefaultIDMapCoversCommonPairs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The batched request includes every default id; just
		// assert stellar made it onto the wire.
		if got := r.URL.Query().Get("ids"); !strings.Contains(got, "stellar") {
			t.Errorf("ids = %q, want to contain stellar (default IDMap missed XLM)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"stellar":{"usd":0.16475,"last_updated_at":%d}}`, time.Now().Unix())
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL: ts.URL,
		IDMap:   map[string]string{}, // empty — the regression scenario
	})
	got, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if got != 0.16475 {
		t.Errorf("price = %v, want 0.16475", got)
	}
}

// TestCoinGecko_RateLimited — 429 maps to ErrPriceUnavailable so the
// Compare layer can distinguish from a permanent unsupported asset.
func TestCoinGecko_RateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL: ts.URL,
		IDMap:   map[string]string{"native": "stellar"},
	})
	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()))
	if !errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("err = %v, want ErrPriceUnavailable", err)
	}
}

// TestCoinGecko_MalformedJSON — parse error surfaces as a generic
// (non-sentinel) transport-style error so the operator sees the
// real cause.
func TestCoinGecko_MalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `not json`)
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL: ts.URL,
		IDMap:   map[string]string{"native": "stellar"},
	})
	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()))
	if err == nil {
		t.Fatal("expected error from malformed JSON")
	}
	if errors.Is(err, divergence.ErrAssetUnsupported) || errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("malformed JSON should NOT match either sentinel; got %v", err)
	}
}

// TestCoinGecko_QuoteNotInMap — fiat:GBP isn't in the test's
// custom QuoteMap → ErrAssetUnsupported.
func TestCoinGecko_QuoteNotInMap(t *testing.T) {
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		IDMap:    map[string]string{"native": "stellar"},
		QuoteMap: map[string]string{"fiat:USD": "usd"}, // GBP missing
	})
	gbp, err := canonical.ParseAsset("fiat:GBP")
	if err != nil {
		t.Fatalf("parse GBP: %v", err)
	}
	pair := canonical.Pair{Base: canonical.NativeAsset(), Quote: gbp}
	_, err = priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
	if !errors.Is(err, divergence.ErrAssetUnsupported) {
		t.Errorf("err = %v, want ErrAssetUnsupported", err)
	}
}

// TestCoinGecko_NameStable — the metric label is locked across
// versions. Renaming is a wire break against alert rules in
// divergence.yml.
func TestCoinGecko_NameStable(t *testing.T) {
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{})
	if ref.Name() != "coingecko" {
		t.Errorf("Name() = %q, want coingecko", ref.Name())
	}
}

// TestCoinGecko_BatchedAcrossPairs: multiple
// per-pair LookupQuote calls within the batch TTL window MUST
// coalesce into a single HTTP request covering every configured
// (id, quote) pair. One call per pair from the orchestrator's per-tick
// loop would be 9 pairs × 2 ticks/min × 1440 min/day = 25,920
// calls/day, well past CoinGecko's demo-tier 10K/day limit; batching
// gives 1 call per tick (~2,880/day).
func TestCoinGecko_BatchedAcrossPairs(t *testing.T) {
	var hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		// Verify the batched request carries every requested id
		// + quote in one shot.
		ids := r.URL.Query().Get("ids")
		quotes := r.URL.Query().Get("vs_currencies")
		for _, want := range []string{"stellar", "bitcoin", "ethereum"} {
			if !strings.Contains(ids, want) {
				t.Errorf("batched ids %q missing %q", ids, want)
			}
		}
		for _, want := range []string{"usd", "eur"} {
			if !strings.Contains(quotes, want) {
				t.Errorf("batched vs_currencies %q missing %q", quotes, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		now := time.Now().Unix()
		_, _ = fmt.Fprintf(w, `{
			"stellar":  {"usd": 0.16, "eur": 0.14, "last_updated_at": %[1]d},
			"bitcoin":  {"usd": 67000, "eur": 62000, "last_updated_at": %[1]d},
			"ethereum": {"usd": 4100,  "eur": 3800, "last_updated_at": %[1]d}
		}`, now)
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL: ts.URL,
		IDMap: map[string]string{
			"native":     "stellar",
			"crypto:BTC": "bitcoin",
			"crypto:ETH": "ethereum",
		},
		QuoteMap: map[string]string{"fiat:USD": "usd", "fiat:EUR": "eur"},
		// Long TTL so all six lookups land in the same batch window.
		BatchTTL: time.Hour,
	})

	usd := mustParseAsset(t, "fiat:USD")
	eur := mustParseAsset(t, "fiat:EUR")
	btc := mustParseAsset(t, "crypto:BTC")
	eth := mustParseAsset(t, "crypto:ETH")
	xlm := canonical.NativeAsset()

	pairs := []canonical.Pair{
		{Base: xlm, Quote: usd},
		{Base: xlm, Quote: eur},
		{Base: btc, Quote: usd},
		{Base: btc, Quote: eur},
		{Base: eth, Quote: usd},
		{Base: eth, Quote: eur},
	}
	for _, p := range pairs {
		price, err := priceOf(ref.LookupQuote(context.Background(), p, time.Now()))
		if err != nil {
			t.Fatalf("LookupQuote(%s): %v", p, err)
		}
		if price <= 0 {
			t.Errorf("LookupQuote(%s) = %g, want > 0", p, price)
		}
	}

	got := atomic.LoadInt64(&hits)
	if got != 1 {
		t.Errorf("HTTP requests = %d, want 1 (6 LookupQuote calls must batch into 1 HTTP call)", got)
	}
}

// TestCoinGecko_BatchTTLExpires — once the batch TTL elapses, the
// next LookupQuote MUST re-fetch (otherwise we'd serve stale prices
// indefinitely on a long-running process).
func TestCoinGecko_BatchTTLExpires(t *testing.T) {
	var hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"stellar": {"usd": 0.16, "last_updated_at": %d}}`, time.Now().Unix())
	}))
	defer ts.Close()

	// Hand-cranked clock: each call to nowFn returns the next slot.
	clock := newFakeClock()
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL:  ts.URL,
		IDMap:    map[string]string{"native": "stellar"},
		QuoteMap: map[string]string{"fiat:USD": "usd"},
		BatchTTL: 30 * time.Second,
		NowFn:    clock.now,
	})

	// Tick 1.
	clock.set(time.Unix(1_000, 0))
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now())); err != nil {
		t.Fatalf("LookupQuote tick 1: %v", err)
	}
	// Within TTL — must reuse cache.
	clock.set(time.Unix(1_010, 0))
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now())); err != nil {
		t.Fatalf("LookupQuote tick 1 (cached): %v", err)
	}
	// Past TTL — must refetch.
	clock.set(time.Unix(1_100, 0))
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now())); err != nil {
		t.Fatalf("LookupQuote tick 2: %v", err)
	}

	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("HTTP requests = %d, want 2 (one fetch per TTL window)", got)
	}
}

// fakeClock is a tiny hand-cranked time source for the TTL test.
// Lives here rather than a shared helper because nothing else in
// the divergence test suite needs deterministic time today.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(0, 0)} }

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func mustParseAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

// panickingReference panics on every LookupQuote. Used to verify
// the comparator's panic-recovery contract — a misbehaving
// reference MUST NOT take the whole Compare run down with it,
// even though the docstring promises "panic recovered ...
// recorded in Failures."
type panickingReference struct {
	name        string
	panicValue  any
	panicOnName bool
}

func (p *panickingReference) Name() string {
	if p.panicOnName {
		panic("Name() panic")
	}
	return p.name
}

func (p *panickingReference) LookupQuote(_ context.Context, _ canonical.Pair, _ time.Time) (divergence.Quote, error) {
	panic(p.panicValue)
}

// TestCompare_PanicInOneReferenceIsolated — one reference panicking
// MUST NOT take the comparator down. The other references' results
// still aggregate; the panic-source surfaces in Failures with a
// "panicked: …" label so operators see which reference is broken
// without reading goroutine traces.
func TestCompare_PanicInOneReferenceIsolated(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "good-a", price: 0.10},
		&panickingReference{name: "bad", panicValue: "kapow"},
		&stubReference{name: "good-b", price: 0.10},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2 (panicking reference must not cancel the others)", res.SuccessCount)
	}
	if res.FailureCount != 1 {
		t.Errorf("FailureCount = %d, want 1", res.FailureCount)
	}
	label, ok := res.Failures["bad"]
	if !ok {
		t.Fatalf("Failures missing 'bad' entry: %+v", res.Failures)
	}
	if label != "panicked: kapow" {
		t.Errorf("Failures[bad] = %q, want %q (panic surfaces with stable label prefix)", label, "panicked: kapow")
	}
	// The good references still drove the median.
	if res.Median != 0.10 {
		t.Errorf("Median = %g, want 0.10", res.Median)
	}
}

// TestCompare_PanicWithErrorValue — a panic with an error type
// (e.g. runtime.Error from a nil-deref) renders via fmt's %v
// verbose form. Pinned because the label is part of the
// operator-facing dashboard surface.
func TestCompare_PanicWithErrorValue(t *testing.T) {
	refs := []divergence.Reference{
		&panickingReference{name: "nil-deref", panicValue: errors.New("runtime: invalid memory address")},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", res.SuccessCount)
	}
	label := res.Failures["nil-deref"]
	if label != "panicked: runtime: invalid memory address" {
		t.Errorf("Failures[nil-deref] = %q, want panicked: runtime: invalid memory address", label)
	}
}

// TestCompare_PanicInName — even Name() panicking shouldn't
// crash the comparator. The failure surfaces under the synthetic
// "_unknown" name so operators see "something panicked here"
// without losing the rest of the run.
func TestCompare_PanicInName(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "good", price: 0.10},
		&panickingReference{panicValue: "swap", panicOnName: true},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1 (good ref still works)", res.SuccessCount)
	}
	if _, ok := res.Failures["_unknown"]; !ok {
		t.Errorf("expected _unknown failure entry, got %+v", res.Failures)
	}
}

// TestCoinGecko_StalenessGate — a FROZEN CoinGecko
// upstream must read as "reference unavailable", never as a fresh
// price that could suppress a real divergence or manufacture a false
// one. The reference requests include_last_updated_at=true and gates
// the returned quote against observedAt using the same discipline as
// the Chainlink and on-chain oracle references.
//
// Guards against ignoring observedAt, which would let a price
// stamped hours ago into the divergence median.
func TestCoinGecko_StalenessGate(t *testing.T) {
	// Fixed bucket-end comparison time the aggregator passes through.
	observedAt := time.Unix(1_770_000_000, 0).UTC()

	// serveAt builds a /simple/price server whose stellar quote is
	// stamped with lastUpdated (unix seconds), and asserts the request
	// opted into include_last_updated_at.
	serveAt := func(lastUpdated time.Time) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("include_last_updated_at") != "true" {
				t.Errorf("include_last_updated_at = %q, want true (no upstream timestamp requested)",
					r.URL.Query().Get("include_last_updated_at"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"stellar": {"usd": 0.16, "last_updated_at": %d}}`, lastUpdated.Unix())
		}))
	}

	// Fresh: upstream stamped 5 minutes before the bucket — well within
	// the 30m default — so the price flows through.
	t.Run("fresh_reference_is_served", func(t *testing.T) {
		ts := serveAt(observedAt.Add(-5 * time.Minute))
		defer ts.Close()
		ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
			BaseURL:  ts.URL,
			IDMap:    map[string]string{"native": "stellar"},
			QuoteMap: map[string]string{"fiat:USD": "usd"},
		})
		price, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), observedAt))
		if err != nil {
			t.Fatalf("LookupQuote(fresh): %v", err)
		}
		if price < 0.159 || price > 0.161 {
			t.Errorf("price = %g, want ~0.16", price)
		}
	})

	// Stale: upstream frozen 90 minutes before the bucket — 3× the 30m
	// default — must be excluded as ErrPriceUnavailable, not served.
	t.Run("stale_reference_is_excluded", func(t *testing.T) {
		ts := serveAt(observedAt.Add(-90 * time.Minute))
		defer ts.Close()
		ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
			BaseURL:  ts.URL,
			IDMap:    map[string]string{"native": "stellar"},
			QuoteMap: map[string]string{"fiat:USD": "usd"},
		})
		_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), observedAt))
		if !errors.Is(err, divergence.ErrPriceUnavailable) {
			t.Fatalf("LookupQuote(stale) err = %v, want ErrPriceUnavailable (frozen feed must not drive divergence)", err)
		}
	})

	// A stale reference feeding Compare must land in Failures (as
	// price_unavailable), NOT in Sources — so it can neither pull the
	// median toward its frozen value nor count as agreement.
	t.Run("stale_reference_does_not_drive_compare", func(t *testing.T) {
		ts := serveAt(observedAt.Add(-90 * time.Minute))
		defer ts.Close()
		ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
			BaseURL:  ts.URL,
			IDMap:    map[string]string{"native": "stellar"},
			QuoteMap: map[string]string{"fiat:USD": "usd"},
		})
		res := divergence.Compare(context.Background(),
			[]divergence.Reference{ref}, xlmUSD(t), 0.16, observedAt, divergence.CompareOptions{})
		if res.SuccessCount != 0 {
			t.Errorf("SuccessCount = %d, want 0 (stale ref must not be a source)", res.SuccessCount)
		}
		if _, ok := res.Sources["coingecko"]; ok {
			t.Errorf("coingecko in Sources; a stale ref must be excluded, got %+v", res.Sources)
		}
		if got := res.Failures["coingecko"]; got != "price_unavailable" {
			t.Errorf("Failures[coingecko] = %q, want price_unavailable", got)
		}
	})
}

// blockingReference ignores context cancellation entirely — the
// hang hazard. Real-world shapes: a third-party SDK doing a blocking
// socket read, a cgo call, an operator-supplied Reference that forgets
// to select on ctx.Done(). release lets the test unblock it at the end
// so the goroutine doesn't outlive the run.
type blockingReference struct {
	name    string
	release chan struct{}
}

func (b *blockingReference) Name() string { return b.name }

func (b *blockingReference) LookupQuote(_ context.Context, _ canonical.Pair, observedAt time.Time) (divergence.Quote, error) {
	<-b.release // deliberately NOT selecting on ctx.Done()
	return divergence.Quote{Price: 1.00, AsOf: freshAt(observedAt)}, nil
}

// TestCompare_OverallDeadlineReturnsPartial guards the overall deadline.
// If Compare only waits on a WaitGroup, a single reference that
// ignores cancellation blocks the comparison — and the aggregator's
// whole divergence refresh loop behind it — forever. It must instead
// return the partial result within the overall budget, recording the
// stuck reference as a failure.
func TestCompare_OverallDeadlineReturnsPartial(t *testing.T) {
	blocked := &blockingReference{name: "stuck", release: make(chan struct{})}
	defer close(blocked.release)

	refs := []divergence.Reference{
		&stubReference{name: "good", price: 0.10},
		blocked,
	}

	done := make(chan divergence.Result, 1)
	go func() {
		done <- divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10,
			time.Now(), divergence.CompareOptions{
				PerReferenceTimeout: 20 * time.Millisecond,
				OverallTimeout:      50 * time.Millisecond,
			})
	}()

	var res divergence.Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Compare never returned: one uncancellable reference stalled the whole comparison")
	}

	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1 — the responsive reference's price must survive", res.SuccessCount)
	}
	if got, ok := res.Sources["good"]; !ok || got != 0.10 {
		t.Errorf("Sources[good] = %v (present=%v), want 0.10", got, ok)
	}
	if got := res.Failures["stuck"]; got != "overall_deadline_exceeded" {
		t.Errorf("Failures[stuck] = %q, want %q", got, "overall_deadline_exceeded")
	}
	if res.Median != 0.10 {
		t.Errorf("Median = %g, want 0.10 (the one price that arrived)", res.Median)
	}
}

// TestCoinGecko_MissingUpstreamTimestampIsRejected — every
// request sets include_last_updated_at=true and /simple/price returns
// it, so a response WITHOUT the field means the freshness contract the
// staleness gate rests on was not honoured. Waving it through silently
// disabled the staleness gate for that id and served a possibly-frozen
// price as fresh; fail closed instead.
func TestCoinGecko_MissingUpstreamTimestampIsRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No last_updated_at — an intermediary stripped it, or the
		// upstream schema drifted.
		_, _ = fmt.Fprintln(w, `{"stellar": {"usd": 0.16}}`)
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL:  ts.URL,
		IDMap:    map[string]string{"native": "stellar"},
		QuoteMap: map[string]string{"fiat:USD": "usd"},
	})

	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()))
	if !errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Fatalf("LookupQuote err = %v, want ErrPriceUnavailable — an unverifiable-freshness price must not drive divergence", err)
	}

	// And it must be excluded from Compare, not merely flagged.
	res := divergence.Compare(context.Background(),
		[]divergence.Reference{ref}, xlmUSD(t), 0.16, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", res.SuccessCount)
	}
	if got := res.Failures["coingecko"]; got != "price_unavailable" {
		t.Errorf("Failures[coingecko] = %q, want price_unavailable", got)
	}
}

// A reference live by its own 26h budget but observed 20h ago must not
// vote beside fresh quotes against a minutes-wide VWAP: before, the two
// stale prints at 1.00 dragged the median of a 0.91 market to 0.955.
func TestCompare_StaleReferenceCannotVote(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stale := at.Add(-20 * time.Hour)
	refs := []divergence.Reference{
		&stubReference{name: "reflector-cex", price: 0.91},
		&stubReference{name: "coingecko", price: 0.91},
		&stubReference{name: "redstone", price: 1.00, asOf: stale},
		&stubReference{name: "band", price: 1.00, asOf: stale},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.91, at, divergence.CompareOptions{})

	if res.SuccessCount != 2 || res.Median != 0.91 || res.DivergencePct != 0 {
		t.Errorf("SuccessCount=%d Median=%v DivergencePct=%v, want 2 / 0.91 / 0", res.SuccessCount, res.Median, res.DivergencePct)
	}
	for _, name := range []string{"redstone", "band"} {
		if _, ok := res.Sources[name]; ok {
			t.Errorf("%s (observed 20h before the comparison) is in Sources", name)
		}
		if got := res.Failures[name]; got != "too_stale_to_compare" {
			t.Errorf("Failures[%s] = %q, want too_stale_to_compare", name, got)
		}
	}
}

// A quote with no observation time cannot be aged, so it cannot vote.
func TestCompare_QuoteWithoutAsOfCannotVote(t *testing.T) {
	ref := &undatedReference{}
	res := divergence.Compare(context.Background(), []divergence.Reference{ref}, xlmUSD(t), 1, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 0 || res.Failures["undated"] != "too_stale_to_compare" {
		t.Errorf("undated quote: SuccessCount=%d Failures=%v, want 0 and too_stale_to_compare", res.SuccessCount, res.Failures)
	}
}

type undatedReference struct{}

func (*undatedReference) Name() string { return "undated" }

func (*undatedReference) LookupQuote(context.Context, canonical.Pair, time.Time) (divergence.Quote, error) {
	return divergence.Quote{Price: 1}, nil
}

// FX quotes pause over market closes; a fiat/fiat pair is held to the FX
// liveness budget, not the 1h crypto ceiling.
func TestMaxComparableAge_FXPairUsesFXBudget(t *testing.T) {
	eur, err := canonical.ParseAsset("fiat:EUR")
	if err != nil {
		t.Fatal(err)
	}
	fx := canonical.Pair{Base: eur, Quote: xlmUSD(t).Quote}
	if got := divergence.MaxComparableAge(fx); got != divergence.DefaultMaxComparableAgeFX {
		t.Errorf("MaxComparableAge(%s) = %v, want %v", fx, got, divergence.DefaultMaxComparableAgeFX)
	}
	if got := divergence.MaxComparableAge(xlmUSD(t)); got != divergence.DefaultMaxComparableAge {
		t.Errorf("MaxComparableAge(%s) = %v, want %v", xlmUSD(t), got, divergence.DefaultMaxComparableAge)
	}
}

// reflector-dex prices the SDEX book our VWAP is built from, so on an
// SDEX walk it "agrees" with us and disarmed the nobody-agrees leg: two
// independent references at -6% and +6% put the median on our price and
// only reflector-dex corroborated it.
func TestRefreshPair_ReflectorDEXCannotCorroborate(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: divergence.OracleSourceReflectorDEX, price: 1.10},
		&stubReference{name: divergence.OracleSourceReflectorCEX, price: 1.03},
		&stubReference{name: "coingecko", price: 1.17},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1,
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := cached.Sources[divergence.OracleSourceReflectorDEX]; ok {
		t.Error("reflector-dex voted as an independent reference")
	}
	if !cached.WarningFired {
		t.Errorf("WarningFired = false with no independent reference within 5%% (sources %v)", cached.Sources)
	}
}

// Compare's per-reference fan-out already recovered (a broken
// reference must not take the comparison down; see
// TestCompare_PanicInOneReferenceIsolated), but it recovered PRIVATELY: the
// panic landed in one Result's Failures map and nowhere else, so the alert
// that exists to say "a detached goroutine died" — which reads
// stellarindex_worker_panics_total via stellarindex_worker_panicked — never
// fired. A reference panicking on every tick looked, to the page rule,
// exactly like a reference that was simply unreachable.
//
// This pins the recovered value going through worker.Report as well, and
// deliberately re-asserts the Failures label so routing the panic to the
// metric cannot quietly cost the operator-facing one.
func TestCompare_PanicMovesTheWorkerPanicCounter(t *testing.T) {
	const workerName = "divergence-reference-lookup"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	refs := []divergence.Reference{
		&stubReference{name: "good", price: 0.10},
		&panickingReference{name: "bad", panicValue: "kapow"},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(),
		divergence.CompareOptions{})

	if got := res.Failures["bad"]; got != "panicked: kapow" {
		t.Errorf("Failures[bad] = %q, want %q — the operator-facing label must survive", got, "panicked: kapow")
	}
	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", res.SuccessCount)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a panic "+
			"recovered only into one comparison's Failures map is invisible to the page rule",
			workerName, after, before+1)
	}
}

// A pair with exactly two references loses one to a
// fail-closed freshness gate (price_unavailable). SuccessCount drops below
// the quorum, the verdict is carried forward instead of evaluated, and the
// pass still returns nil — so the pass-level outcome counter reads "ok".
// The per-reference counter and the pair quorum gauge must say otherwise.
func TestRefreshPair_ExportsReferenceOutcomesAndQuorumLoss(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "gh679-healthy", price: 1.40},
		&stubReference{name: "gh679-degraded", err: divergence.ErrPriceUnavailable},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{MinSourcesForWarning: 2})
	pair := xlmUSD(t)

	okBefore := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-healthy", "ok"))
	failBefore := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-degraded", "price_unavailable"))

	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v (a below-quorum pass is not a refresh error)", err)
	}

	if got := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-healthy", "ok")) - okBefore; got != 1 {
		t.Errorf("reference_total{gh679-healthy,ok} moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-degraded", "price_unavailable")) - failBefore; got != 1 {
		t.Errorf("reference_total{gh679-degraded,price_unavailable} moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.DivergencePairQuorumMet.WithLabelValues(pair.String())); got != 0 {
		t.Errorf("pair_quorum_met = %v, want 0: one of two references responded, below quorum 2", got)
	}

	// Recovery re-arms the gauge.
	refs[1].(*stubReference).err = nil
	refs[1].(*stubReference).price = 1.00
	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if got := testutil.ToFloat64(obs.DivergencePairQuorumMet.WithLabelValues(pair.String())); got != 1 {
		t.Errorf("pair_quorum_met = %v after recovery, want 1", got)
	}
}

// The per-reference gap and pair counts reach the registry as bounded
// aggregates: a 12 % miss on one reference trips both thresholds, and a
// pinned refresh (no verdict) clears the pair.
func TestRefreshPair_ExportsBoundedGapGauges(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "gap-far", price: 1.00},
		&stubReference{name: "gap-near", price: 1.12},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{MinSourcesForWarning: 2})
	pair := xlmUSD(t)
	gap := func(ref string) float64 {
		return testutil.ToFloat64(obs.DivergenceMaxAbsFraction.WithLabelValues(ref))
	}
	over := func(th string) float64 {
		return testutil.ToFloat64(obs.DivergencePairsOver.WithLabelValues(th))
	}

	if err := svc.RefreshPair(context.Background(), pair, 1.12, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if got := gap("gap-far"); got < 0.119 || got > 0.121 {
		t.Errorf("max_abs_fraction{gap-far} = %v, want ~0.12", got)
	}
	if got := gap("gap-near"); got != 0 {
		t.Errorf("max_abs_fraction{gap-near} = %v, want 0", got)
	}
	if over("5pct") != 1 || over("10pct") != 1 {
		t.Errorf("pairs_over = 5pct:%v 10pct:%v, want 1 and 1", over("5pct"), over("10pct"))
	}

	if err := svc.RefreshPinnedPair(context.Background(), pair, 1.12, time.Now()); err != nil {
		t.Fatalf("RefreshPinnedPair: %v", err)
	}
	if gap("gap-far") != 0 || over("5pct") != 0 {
		t.Errorf("pinned refresh left gap=%v over5=%v, want both cleared", gap("gap-far"), over("5pct"))
	}
}

// The per-reference outcome is a metric label, so it must be a bounded
// class even where Failures carries verbatim error text.
func TestCompare_OutcomesAreBoundedClasses(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "ok", price: 1.00},
		&stubReference{name: "unsupported", err: divergence.ErrAssetUnsupported},
		&stubReference{name: "stale", err: divergence.ErrTooStaleToCompare},
		&stubReference{name: "slow", price: 1.00, delay: time.Second},
		&stubReference{name: "zero", price: 0},
		&stubReference{name: "opaque", err: errors.New("dial tcp 10.0.0.1:443: connection refused")},
		&panickingReference{name: "broken", panicValue: "kapow"},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(),
		divergence.CompareOptions{PerReferenceTimeout: 50 * time.Millisecond})

	want := map[string]string{
		"ok":          divergence.OutcomeOK,
		"unsupported": divergence.OutcomeAssetUnsupported,
		"stale":       divergence.OutcomeTooStale,
		"slow":        divergence.OutcomeTimeout,
		"zero":        divergence.OutcomeInvalidPrice,
		"opaque":      divergence.OutcomeError,
		"broken":      divergence.OutcomePanicked,
	}
	if len(res.Outcomes) != len(want) {
		t.Errorf("Outcomes = %v, want one entry per reference (%d)", res.Outcomes, len(want))
	}
	for name, outcome := range want {
		if got := res.Outcomes[name]; got != outcome {
			t.Errorf("Outcomes[%s] = %q, want %q", name, got, outcome)
		}
	}
	if got := res.Failures["broken"]; got != "panicked: kapow" {
		t.Errorf("Failures[broken] = %q, want the operator-facing label unchanged", got)
	}
	vocab := map[string]bool{}
	for _, o := range divergence.ReferenceOutcomes {
		vocab[o] = true
	}
	for name, o := range res.Outcomes {
		if !vocab[o] {
			t.Errorf("Outcomes[%s] = %q is outside ReferenceOutcomes", name, o)
		}
	}
}

// The div: key must outlive the gap between two writes of the
// same pair. With the default 300s min interval on a 30s tick the pass runs
// every 330s, so a key written with the bare 300s TTL expired ~30s before
// its rewrite on every cycle — longer when the fan-out is slow — and the
// freeze path read "no cross-oracle data" throughout.
func TestRefreshPair_TTLBridgesRefreshCadence(t *testing.T) {
	const cadence = 330 * time.Second
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{RefreshInterval: cadence})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if ttl := mr.TTL(cachekeys.Divergence(xlmUSD(t)).String()); ttl <= cadence {
		t.Errorf("div: TTL = %v, want > the %v refresh cadence", ttl, cadence)
	}
	if ttl := mr.TTL(cachekeys.DivergenceBaseIndex(xlmUSD(t).Base).String()); ttl <= cadence {
		t.Errorf("div:idx: TTL = %v, want > the %v refresh cadence", ttl, cadence)
	}
}

// The last pair of a sequential pass is rewritten a whole pass later than
// the first, and each pair can spend Compare's full overall budget (2x the
// per-reference timeout) on a reference that times out.
func TestRefreshPair_TTLCoversWorstCasePass(t *testing.T) {
	const (
		cadence = 330 * time.Second
		pairs   = 12
		perRef  = 5 * time.Second
	)
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{
		RefreshInterval:     cadence,
		PairCount:           pairs,
		PerReferenceTimeout: perRef,
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	worstGap := cadence + pairs*2*perRef
	if ttl := mr.TTL(cachekeys.Divergence(xlmUSD(t)).String()); ttl <= worstGap {
		t.Errorf("div: TTL = %v, want > %v (cadence + %d pairs x 2 x %v)", ttl, worstGap, pairs, perRef)
	}
}

// TestCountAgreeing pins the cross-oracle agreement math (ADR-0019
// Phase 3): a reference agrees when its per-reference delta is at or
// below the threshold, mirroring the complement of the observation
// sink's strict `>` firing test.
func TestCountAgreeing(t *testing.T) {
	cases := []struct {
		name      string
		ourPrice  float64
		sources   map[string]float64
		threshold float64
		want      int
	}{
		{
			name:      "all agree exactly",
			ourPrice:  1.00,
			sources:   map[string]float64{"a": 1.00, "b": 1.00, "c": 1.00},
			threshold: 5.0,
			want:      3,
		},
		{
			name:     "one outlier excluded",
			ourPrice: 1.00,
			// |1.00-1.30|/1.30*100 ≈ 23.1% > 5%.
			sources:   map[string]float64{"a": 1.001, "b": 0.999, "outlier": 1.30},
			threshold: 5.0,
			want:      2,
		},
		{
			// Delta exactly AT threshold counts as agreement (<=),
			// mirroring flushObservations' strict > firing test.
			// Binary-exact values so the boundary is truly exact:
			// ref=1.0, our=1.5 → |0.5|/1.0*100 = 50.0 == threshold.
			name:      "delta exactly at threshold agrees",
			ourPrice:  1.5,
			sources:   map[string]float64{"a": 1.0},
			threshold: 50.0,
			want:      1,
		},
		{
			// Just past the threshold does not agree.
			name:      "delta just above threshold disagrees",
			ourPrice:  1.5078125, // binary-exact; delta = 50.78125%
			sources:   map[string]float64{"a": 1.0},
			threshold: 50.0,
			want:      0,
		},
		{
			// No responders means UNCHECKED — zero here must
			// pair with SuccessCount=0 at the consumer, never read as
			// "everyone disagrees".
			name:      "empty sources",
			ourPrice:  1.00,
			sources:   map[string]float64{},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "nil sources",
			ourPrice:  1.00,
			sources:   nil,
			threshold: 5.0,
			want:      0,
		},
		{
			// Defensive: zero / negative / non-finite reference prices
			// are skipped (no meaningful delta), matching the
			// observation sink's zero-guard.
			name:      "bad reference prices skipped",
			ourPrice:  1.00,
			sources:   map[string]float64{"zero": 0, "neg": -1, "nan": math.NaN(), "inf": math.Inf(1), "good": 1.00},
			threshold: 5.0,
			want:      1,
		},
		{
			name:      "non-positive our price counts nothing",
			ourPrice:  0,
			sources:   map[string]float64{"a": 1.00},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "NaN our price counts nothing",
			ourPrice:  math.NaN(),
			sources:   map[string]float64{"a": 1.00},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "negative threshold counts nothing",
			ourPrice:  1.00,
			sources:   map[string]float64{"a": 1.00},
			threshold: -1,
			want:      0,
		},
		{
			// Zero threshold: only exact matches agree.
			name:      "zero threshold exact match only",
			ourPrice:  1.00,
			sources:   map[string]float64{"exact": 1.00, "near": 1.0001},
			threshold: 0,
			want:      1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := divergence.CountAgreeing(tc.ourPrice, tc.sources, tc.threshold); got != tc.want {
				t.Errorf("CountAgreeing(%v, %v, %v) = %d, want %d",
					tc.ourPrice, tc.sources, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestRefreshPair_AgreementCountPersisted — RefreshPair computes the
// agreement count against the worker's threshold and persists it in
// the cached JSON, distinct from SuccessCount ("responded" vs
// "corroborates").
func TestRefreshPair_AgreementCountPersisted(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.001},
		&stubReference{name: "dissenter", price: 1.30}, // ~23% off — responds but does not agree
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cached.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3", cached.SuccessCount)
	}
	if cached.AgreementCount != 2 {
		t.Errorf("AgreementCount = %d, want 2 (dissenter responded but must not corroborate)", cached.AgreementCount)
	}
}

// TestRefreshPair_AllReferencesDark_AgreementZeroMeansUnchecked —
// when every reference fails, the cached result reads
// SuccessCount=0 + AgreementCount=0 and RefreshPair returns
// ErrNoReferenceResponded. Consumers must interpret that as
// "unchecked", never as "zero references agree with us".
func TestRefreshPair_AllReferencesDark_AgreementZeroMeansUnchecked(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrPriceUnavailable},
		&stubReference{name: "b", err: divergence.ErrAssetUnsupported},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now())
	if !errors.Is(err, divergence.ErrNoReferenceResponded) {
		t.Fatalf("RefreshPair err = %v, want ErrNoReferenceResponded", err)
	}

	body, gerr := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if gerr != nil {
		t.Fatalf("redis get: %v", gerr)
	}
	var cached divergence.CachedResult
	if uerr := json.Unmarshal(body, &cached); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if cached.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", cached.SuccessCount)
	}
	if cached.AgreementCount != 0 {
		t.Errorf("AgreementCount = %d, want 0 on a dark run", cached.AgreementCount)
	}
}

// TestRefreshPair_AllUnsupportedIsNotAnOutage pins: a pair every
// reference structurally doesn't cover (ErrAssetUnsupported —
// reference.go's own doc: "no reference for this pair on this source,
// not a degradation") must not be reported as ErrNoReferenceResponded.
// Guards against FailureCount folding asset_unsupported in with genuine
// transport failures, which would page a deployment with an uncovered pair as
// "checker running blind" forever with both references healthy.
func TestRefreshPair_AllUnsupportedIsNotAnOutage(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrAssetUnsupported},
		&stubReference{name: "b", err: divergence.ErrAssetUnsupported},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now())
	if err != nil {
		t.Fatalf("RefreshPair err = %v, want nil — every reference is structurally unsupported, not failed", err)
	}

	body, gerr := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if gerr != nil {
		t.Fatalf("redis get: %v", gerr)
	}
	var cached divergence.CachedResult
	if uerr := json.Unmarshal(body, &cached); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if cached.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", cached.SuccessCount)
	}
	if cached.FailureCount != 2 {
		t.Errorf("FailureCount = %d, want 2 (still recorded, just not treated as an outage)", cached.FailureCount)
	}
}

// TestRefreshPair_MixedUnsupportedAndFailedIsAnOutage guards the other
// side: a genuine failure alongside unsupported references
// must still page — the veto is only for the all-unsupported case.
func TestRefreshPair_MixedUnsupportedAndFailedIsAnOutage(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrAssetUnsupported},
		&stubReference{name: "b", err: divergence.ErrPriceUnavailable},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now())
	if !errors.Is(err, divergence.ErrNoReferenceResponded) {
		t.Fatalf("RefreshPair err = %v, want ErrNoReferenceResponded (a genuine failure is mixed in with the unsupported one)", err)
	}
}

// TestRefreshPair_SymmetricStraddleFiresWarning is the
// regression on the SERVED value (flags.divergence_warning, read
// straight off CachedResult.WarningFired).
//
// Two references straddle our price symmetrically — one 20% above,
// one 20% below. Their median lands exactly ON our price, so the
// median-only gate computed DivergencePct ≈ 0 and stayed silent while
// NEITHER reference corroborated us within the 5% threshold. Total
// external disagreement was served as agreement.
func TestRefreshPair_SymmetricStraddleFiresWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "high", price: 1.20},
		&stubReference{name: "low", price: 0.80},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1, // isolate the agreement leg from the persistence debounce
	})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if uerr := json.Unmarshal(body, &cached); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}

	// The straddle is real: median == our price, so the median leg is
	// silent by construction. Pin that, so this test can never pass
	// for the wrong reason.
	if cached.Median != 1.00 {
		t.Fatalf("Median = %g, want exactly 1.00 — the straddle fixture is what makes the median gate blind", cached.Median)
	}
	if cached.DivergencePct != 0 {
		t.Fatalf("DivergencePct = %g, want 0 — the median leg must be silent here", cached.DivergencePct)
	}
	if cached.SuccessCount != 2 {
		t.Fatalf("SuccessCount = %d, want 2", cached.SuccessCount)
	}
	if cached.AgreementCount != 0 {
		t.Fatalf("AgreementCount = %d, want 0 — neither reference is within 5%% of us", cached.AgreementCount)
	}

	if !cached.WarningFired {
		t.Error("WarningFired = false: two references 20% either side of us, NOT ONE corroborating, " +
			"served as 'no divergence' because their median happened to equal our price")
	}
}

// TestRefreshPair_OneDissenterDoesNotFireWarning pins the deliberate
// limit of the agreement leg: it fires on "nobody agrees", NOT
// on "somebody disagrees". With three references and one outlier, two
// still corroborate us and the warning must stay silent — otherwise a
// single flaky reference would pin the customer-visible flag on
// permanently.
func TestRefreshPair_OneDissenterDoesNotFireWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.01},
		&stubReference{name: "flaky", price: 1.40},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})

	// The agreement leg must require that NO reference corroborates us, so
	// one dissenter leaves the pair quiet.
	cached := refreshQuiet(t, svc, rdb, xlmUSD(t), 1.00, time.Now())
	if cached.AgreementCount != 2 {
		t.Fatalf("AgreementCount = %d, want 2", cached.AgreementCount)
	}
}

// fakeOracleReader is an OracleReader that returns a canned row and
// captures the arguments of the last call for mapping assertions.
type fakeOracleReader struct {
	row *canonical.OracleUpdate
	err error

	gotSource    string
	gotBaseKeys  []string
	gotQuoteKeys []string
}

func (f *fakeOracleReader) LatestOracleObservation(_ context.Context, source string, baseKeys, quoteKeys []string) (*canonical.OracleUpdate, error) {
	f.gotSource = source
	f.gotBaseKeys = baseKeys
	f.gotQuoteKeys = quoteKeys
	return f.row, f.err
}

func newOracleRef(t *testing.T, source string, reader divergence.OracleReader, maxAge time.Duration) *divergence.OracleReference {
	t.Helper()
	ref, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Source: source,
		Reader: reader,
		MaxAge: maxAge,
	})
	if err != nil {
		t.Fatalf("NewOracleReference: %v", err)
	}
	return ref
}

func oracleRow(t *testing.T, source, priceDec string, decimals uint8, ts time.Time) *canonical.OracleUpdate {
	t.Helper()
	raw, ok := new(big.Int).SetString(priceDec, 10)
	if !ok {
		t.Fatalf("bad price literal %q", priceDec)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	return &canonical.OracleUpdate{
		Source:    source,
		Ledger:    1,
		TxHash:    "aa",
		Timestamp: ts,
		Asset:     canonical.NativeAsset(),
		Quote:     usd,
		Price:     canonical.NewAmount(raw),
		Decimals:  decimals,
	}
}

// TestNewOracleReference_RequiresSourceAndReader — misconfiguration
// fails loudly at construction.
func TestNewOracleReference_RequiresSourceAndReader(t *testing.T) {
	if _, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Reader: &fakeOracleReader{},
	}); err == nil {
		t.Error("expected error when Source is empty")
	}
	if _, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Source: divergence.OracleSourceBand,
	}); err == nil {
		t.Error("expected error when Reader is nil")
	}
}

// TestOracleReference_ScaleExactness verifies the raw-integer →
// float64 conversion is exact at every stored oracle scale,
// including values above 2^53 that would corrupt under a float64
// intermediate (ADR-0003). Expected values are computed via
// strconv.ParseFloat of the decimal string — both paths must round
// identically (nearest-even) or the big-int path lost precision.
func TestOracleReference_ScaleExactness(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		price    string // raw integer, base 10
		decimals uint8
		expected string // decimal string of price / 10^decimals
	}{
		// Reflector stores at 14 decimals. Raw value > 2^53.
		{
			"reflector E14 above 2^53", divergence.OracleSourceReflectorCEX,
			"123456789012345678", 14, "1234.56789012345678",
		},
		// Band single-asset relayed rates are E9.
		{
			"band E9", divergence.OracleSourceBand,
			"4567890123", 9, "4.567890123",
		},
		// Redstone per-feed prices are E8.
		{
			"redstone E8", divergence.OracleSourceRedstone,
			"12345678", 8, "0.12345678",
		},
		// E18-class scale (Band's pair-rate scale upstream): 21
		// digits of significand, far above 2^53 — must stay exact
		// through the big.Rat path up to float64's own rounding.
		{
			"E18 above 2^53", divergence.OracleSourceBand,
			"1234567890123456789012", 18, "1234.567890123456789012",
		},
		// XLM-ish small price at 14 decimals.
		{
			"reflector E14 sub-dollar", divergence.OracleSourceReflectorDEX,
			"11072000000000", 14, "0.11072",
		},
	}
	now := time.Now().UTC()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeOracleReader{row: oracleRow(t, tc.source, tc.price, tc.decimals, now)}
			ref := newOracleRef(t, tc.source, reader, time.Hour)

			got, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), now))
			if err != nil {
				t.Fatalf("LookupQuote: %v", err)
			}
			want, err := strconv.ParseFloat(tc.expected, 64)
			if err != nil {
				t.Fatalf("ParseFloat(%q): %v", tc.expected, err)
			}
			if got != want {
				t.Errorf("price = %v, want %v (exact for %s at E%d)",
					got, want, tc.price, tc.decimals)
			}
		})
	}
}

// TestOracleReference_PairMapping_XLMDualIdentity — `native` and
// `crypto:XLM` are the same asset on two wire forms; the reference
// must query BOTH so a Reflector-CEX row published under crypto:XLM
// matches our on-chain native/fiat:USD pair (and vice versa). Every
// other asset maps to exactly its canonical string.
func TestOracleReference_PairMapping_XLMDualIdentity(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now)}
	ref := newOracleRef(t, divergence.OracleSourceReflectorCEX, reader, time.Hour)

	// native base expands to crypto:XLM and the XLM SAC.
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), now)); err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if reader.gotSource != divergence.OracleSourceReflectorCEX {
		t.Errorf("source = %q, want reflector-cex", reader.gotSource)
	}
	if got, want := strings.Join(reader.gotBaseKeys, ","), "native,crypto:XLM,"+canonical.XLMSacContractID; got != want {
		t.Errorf("base keys = %q, want %q", got, want)
	}
	if got := strings.Join(reader.gotQuoteKeys, ","); got != "fiat:USD" {
		t.Errorf("quote keys = %q, want fiat:USD", got)
	}

	// crypto:XLM base expands to native and the XLM SAC.
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	if _, err := priceOf(ref.LookupQuote(context.Background(), canonical.Pair{Base: xlm, Quote: usd}, now)); err != nil {
		t.Fatalf("LookupQuote crypto:XLM: %v", err)
	}
	if got, want := strings.Join(reader.gotBaseKeys, ","), "crypto:XLM,native,"+canonical.XLMSacContractID; got != want {
		t.Errorf("base keys = %q, want %q", got, want)
	}

	// Non-XLM assets don't expand.
	eur, err := canonical.ParseAsset("fiat:EUR")
	if err != nil {
		t.Fatalf("parse EUR: %v", err)
	}
	if _, err := priceOf(ref.LookupQuote(context.Background(), canonical.Pair{Base: eur, Quote: usd}, now)); err != nil {
		t.Fatalf("LookupQuote EUR/USD: %v", err)
	}
	if got := strings.Join(reader.gotBaseKeys, ","); got != "fiat:EUR" {
		t.Errorf("base keys = %q, want fiat:EUR", got)
	}
}

// TestOracleReference_BaseKeysCoverEveryAliasFamily is the guard against
// a hand-rolled key expander drifting from the alias registry: for every
// member of every installed family (the XLM baseline plus a configured
// classic↔SAC pair), the keys bound for that member must include the
// whole family. reflector-dex publishes XLM only under its SAC
// C-address, so a native-keyed read that omits the SAC can never match
// a reflector-dex row.
func TestOracleReference_BaseKeysCoverEveryAliasFamily(t *testing.T) {
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSACContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{usdcSACContract: usdcClassic})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	families := [][]string{
		{"native", "crypto:XLM", canonical.XLMSacContractID},
		{usdcClassic, usdcSACContract},
	}
	for form, canon := range canonical.AllAliasForms() {
		families = append(families, []string{form, canon})
	}

	now := time.Now().UTC()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceReflectorDEX, "20000000000000", 14, now)}
	ref := newOracleRef(t, divergence.OracleSourceReflectorDEX, reader, time.Hour)
	for _, family := range families {
		for _, member := range family {
			base, err := canonical.ParseAsset(member)
			if err != nil {
				t.Fatalf("parse %q: %v", member, err)
			}
			if _, err := priceOf(ref.LookupQuote(context.Background(), canonical.Pair{Base: base, Quote: usd}, now)); err != nil {
				t.Fatalf("LookupQuote %s/USD: %v", member, err)
			}
			bound := make(map[string]bool, len(reader.gotBaseKeys))
			for _, k := range reader.gotBaseKeys {
				bound[k] = true
			}
			for _, want := range family {
				if !bound[want] {
					t.Errorf("base keys for %s = %v, missing alias %s", member, reader.gotBaseKeys, want)
				}
			}
		}
	}
}

// TestOracleReference_QuoteKeysNeverMapUSDCToUSD: reflector-dex rows are
// quoted in the USDC SAC. A fiat:USD pair must not bind any USDC form (that
// would compare a USDC price as USD and hide a depeg), while a classic-USDC
// pair must reach the SAC-quoted rows through the alias registry.
func TestOracleReference_QuoteKeysNeverMapUSDCToUSD(t *testing.T) {
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSACContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{usdcSACContract: usdcClassic})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	now := time.Now().UTC()
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceReflectorDEX, "20000000000000", 14, now)}
	ref := newOracleRef(t, divergence.OracleSourceReflectorDEX, reader, time.Hour)
	lookup := func(quote string) []string {
		q, err := canonical.ParseAsset(quote)
		if err != nil {
			t.Fatalf("parse %q: %v", quote, err)
		}
		if _, err := priceOf(ref.LookupQuote(context.Background(), canonical.Pair{Base: canonical.NativeAsset(), Quote: q}, now)); err != nil {
			t.Fatalf("LookupQuote native/%s: %v", quote, err)
		}
		return reader.gotQuoteKeys
	}

	if got := lookup("fiat:USD"); len(got) != 1 || got[0] != "fiat:USD" {
		t.Errorf("quote keys for fiat:USD = %v, want exactly [fiat:USD]", got)
	}
	got := lookup(usdcClassic)
	if !slices.Contains(got, usdcSACContract) {
		t.Errorf("quote keys for %s = %v, missing the USDC SAC reflector-dex quotes in", usdcClassic, got)
	}
}

// TestOracleReference_NoObservationIsUnsupported — an oracle that
// has never published the pair is an ErrAssetUnsupported (coverage
// information), not a transport failure.
func TestOracleReference_NoObservationIsUnsupported(t *testing.T) {
	reader := &fakeOracleReader{row: nil}
	ref := newOracleRef(t, divergence.OracleSourceBand, reader, time.Hour)
	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now().UTC()))
	if !errors.Is(err, divergence.ErrAssetUnsupported) {
		t.Errorf("err = %v, want ErrAssetUnsupported", err)
	}
}

// TestOracleReference_StaleObservationIsUnavailable — a frozen feed
// must read as "reference unavailable", never as agreement or
// divergence (the staleness gate applied to served rows).
func TestOracleReference_StaleObservationIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-45*time.Minute)),
	}
	ref := newOracleRef(t, divergence.OracleSourceReflectorCEX, reader, 30*time.Minute)
	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), now))
	if !errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("err = %v, want ErrPriceUnavailable", err)
	}

	// Just inside the ceiling passes.
	reader.row = oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-29*time.Minute))
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), now)); err != nil {
		t.Errorf("fresh-enough observation rejected: %v", err)
	}
}

// TestOracleReference_ReaderErrorSurfaces — storage failures are
// generic transport-class failures (Result.Failures verbatim), not
// the unsupported/unavailable sentinels.
func TestOracleReference_ReaderErrorSurfaces(t *testing.T) {
	reader := &fakeOracleReader{err: errors.New("pg down")}
	ref := newOracleRef(t, divergence.OracleSourceRedstone, reader, time.Hour)
	_, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), time.Now().UTC()))
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, divergence.ErrAssetUnsupported) || errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("reader error misclassified as sentinel: %v", err)
	}
}

// TestOracleReference_NonPositivePriceRejected — defensive: a
// zero/negative stored price never becomes a reference price.
func TestOracleReference_NonPositivePriceRejected(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceBand, "0", 9, now)}
	ref := newOracleRef(t, divergence.OracleSourceBand, reader, time.Hour)
	if _, err := priceOf(ref.LookupQuote(context.Background(), xlmUSD(t), now)); err == nil {
		t.Error("expected error for zero price")
	}
}

// TestRefreshPair_OnChainOracleReferences is the worker-cycle test:
// a Service wired with three on-chain oracle references (two fresh,
// one stale) refreshes a pair and the cached result + observation
// sink carry the per-reference outcomes under the oracle source
// labels — same observation schema as the HTTP references.
func TestRefreshPair_OnChainOracleReferences(t *testing.T) {
	now := time.Now().UTC()
	// reflector-cex: XLM at 0.11 (14 decimals), fresh.
	cexReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-time.Minute)),
	}
	// redstone: XLM at 0.11 (8 decimals), fresh.
	redstoneReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceRedstone, "11000000", 8, now.Add(-time.Minute)),
	}
	// band: stale beyond its ceiling.
	bandReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceBand, "110000000", 9, now.Add(-48*time.Hour)),
	}
	refs := []divergence.Reference{
		newOracleRef(t, divergence.OracleSourceReflectorCEX, cexReader, 30*time.Minute),
		newOracleRef(t, divergence.OracleSourceRedstone, redstoneReader, 26*time.Hour),
		newOracleRef(t, divergence.OracleSourceBand, bandReader, 26*time.Hour),
	}
	sink := &recordingObservationSink{}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{ObservationSink: sink})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 0.11, now); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cached.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2 (cex + redstone)", cached.SuccessCount)
	}
	if got := cached.Sources[divergence.OracleSourceReflectorCEX]; got != 0.11 {
		t.Errorf("reflector-cex price = %v, want 0.11", got)
	}
	if got := cached.Sources[divergence.OracleSourceRedstone]; got != 0.11 {
		t.Errorf("redstone price = %v, want 0.11", got)
	}
	if got := cached.Failures[divergence.OracleSourceBand]; got != "price_unavailable" {
		t.Errorf("band failure = %q, want price_unavailable", got)
	}
	// FiringSince is the raw condition; WarningFired alone is held false
	// on a first refresh by the default debounce whatever the gate says.
	if cached.WarningFired || !cached.FiringSince.IsZero() {
		t.Errorf("agreeing references must not fire: WarningFired=%v FiringSince=%v",
			cached.WarningFired, cached.FiringSince)
	}

	// Observation sink got one row per SUCCESSFUL reference, labeled
	// by oracle source.
	seen := map[string]bool{}
	for _, o := range sink.records {
		seen[o.Reference] = true
		if o.OurPrice != "0.11" {
			t.Errorf("observation %s OurPrice = %q, want decimal string \"0.11\"", o.Reference, o.OurPrice)
		}
		if o.Firing {
			t.Errorf("observation %s unexpectedly firing", o.Reference)
		}
	}
	if !seen[divergence.OracleSourceReflectorCEX] || !seen[divergence.OracleSourceRedstone] {
		t.Errorf("sink references = %v, want reflector-cex + redstone", seen)
	}
	if seen[divergence.OracleSourceBand] {
		t.Error("stale band reference must not produce an observation row")
	}
}

// readDivergence fetches + decodes the CachedResult the worker wrote
// for a pair. Fails the test on any miss/decode error so the callers
// stay assertion-only.
func readDivergence(t *testing.T, rdb *redis.Client, pair canonical.Pair) divergence.CachedResult {
	t.Helper()
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get %s: %v", cachekeys.Divergence(pair), err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal CachedResult: %v", err)
	}
	return cached
}

// TestRefreshPair_FastMoveDoesNotFalseWarn is the red
// proof for debouncing. OurPrice is a shortest-window (5m) VWAP; the references are
// instantaneous spot quotes. On a fast upward move the spot has jumped
// but our VWAP still averages in the pre-move trades, so our value
// legitimately lags the references by more than the threshold. Before
// the fix that raised flags.divergence_warning immediately even though
// nothing was wrong.
//
// A SINGLE refresh of a real 10% gap (well over the 5% threshold, with
// no reference corroborating us) must NOT fire the warning: the gap has
// not yet persisted past the debounce window, so it is indistinguishable
// from the mechanical VWAP-vs-spot lag of a fast move.
//
// This is the non-vacuous assertion: against a worker that
// set WarningFired = checked && (DivergencePct > threshold || nobody
// agrees) this single 10% refresh fires true and the test fails red.
func TestRefreshPair_FastMoveDoesNotFalseWarn(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	// Default debounce (no WarningPersistence set) — the production
	// posture the fix ships with.
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})

	// Our 5m VWAP is 10% above the spot references — a fast move the
	// VWAP hasn't caught up to yet.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	cached := readDivergence(t, rdb, xlmUSD(t))

	// Pin that the RAW divergence is genuinely present — so the test can
	// never pass for the wrong reason (e.g. the references agreeing).
	if cached.DivergencePct < 9.0 {
		t.Fatalf("DivergencePct = %g, want ~10 — the fixture must present a real over-threshold gap", cached.DivergencePct)
	}
	if cached.AgreementCount != 0 {
		t.Fatalf("AgreementCount = %d, want 0 — no reference corroborates us at this instant", cached.AgreementCount)
	}
	// ...yet the WARNING must be held: a single-tick over-threshold gap
	// is exactly the fast-move artefact the debounce suppresses.
	if cached.WarningFired || cached.FiringSince.IsZero() {
		t.Errorf("single-refresh 10%% gap: WarningFired=%v FiringSince=%v, want the raw streak started "+
			"but the warning held — a fast-move VWAP-vs-spot lag must not raise a false divergence warning "+
			"before it has persisted past the debounce window", cached.WarningFired, cached.FiringSince)
	}
}

// TestRefreshPair_SustainedDivergenceStillWarns proves the debounce
// does not blind a genuine divergence: the same over-threshold gap,
// still present one debounce window later, DOES fire. This is the
// "genuine-divergence detection preserved" half of the fix.
func TestRefreshPair_SustainedDivergenceStillWarns(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()

	// First observation of the gap — held (debounced).
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0); err != nil {
		t.Fatalf("RefreshPair #1: %v", err)
	}
	if got := readDivergence(t, rdb, pair); got.WarningFired || got.FiringSince.IsZero() {
		t.Fatalf("first observation: WarningFired=%v FiringSince=%v; the raw streak must start and "+
			"the debounce must hold the warning for one window", got.WarningFired, got.FiringSince)
	}

	// Same gap, one debounce window later — a genuine sustained
	// divergence. The warning must now fire.
	if err := svc.RefreshPair(context.Background(), pair, 1.10,
		t0.Add(divergence.DefaultWarningPersistence+time.Minute)); err != nil {
		t.Fatalf("RefreshPair #2: %v", err)
	}
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false on a divergence that persisted a full window: the debounce " +
			"must not blind a sustained divergence")
	}
}

// TestRefreshPair_TransientDivergenceSelfClears proves the gate keys on
// an UNINTERRUPTED streak, not on wall-clock-since-first-ever. A gap
// that appears, clears, then reappears must restart its persistence
// clock — so a pair of brief spikes straddling more than a window never
// fires. This is what stops the debounce from degrading into a plain
// "warn if we've ever been over threshold for N minutes" bound.
func TestRefreshPair_TransientDivergenceSelfClears(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	win := divergence.DefaultWarningPersistence

	// Spike #1: 10% gap → held.
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0); err != nil {
		t.Fatalf("RefreshPair spike#1: %v", err)
	}
	if got := readDivergence(t, rdb, pair); got.WarningFired || got.FiringSince.IsZero() {
		t.Fatalf("spike #1: WarningFired=%v FiringSince=%v; the raw streak must start and the warning be held",
			got.WarningFired, got.FiringSince)
	}

	// Recovery: prices agree again → streak resets.
	if err := svc.RefreshPair(context.Background(), pair, 1.00, t0.Add(win/2)); err != nil {
		t.Fatalf("RefreshPair recovery: %v", err)
	}
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true on recovery; prices agree here")
	}

	// Spike #2, well past a window since spike #1 — but the streak
	// restarted at recovery, so this is a fresh onset and must NOT fire
	// even though (t0.Add(2*win) - t0) > window.
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0.Add(2*win)); err != nil {
		t.Fatalf("RefreshPair spike#2: %v", err)
	}
	if readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = true on a fresh spike: the persistence clock must restart after the " +
			"divergence self-cleared, not accumulate across disconnected spikes")
	}
}

// TestRefreshPair_OnWarningHookFiresOnlyAfterPersistence pins that the
// edge-triggered customer-webhook hook rides the DEBOUNCED WarningFired,
// so subscribers aren't paged on a fast-move blip. The hook must fire on
// the persisted false→true edge, not on the first raw over-threshold
// refresh.
func TestRefreshPair_OnWarningHookFiresOnlyAfterPersistence(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
	}
	var fired int
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(_ context.Context, _ canonical.Pair, _ divergence.CachedResult) error {
			fired++
			return nil
		},
	})
	pair := xlmUSD(t)
	t0 := time.Now()

	// First over-threshold refresh: debounced, so no hook.
	_ = svc.RefreshPair(context.Background(), pair, 1.10, t0)
	if fired != 0 {
		t.Fatalf("hook fired=%d after the first (debounced) refresh, want 0", fired)
	}

	// Persisted a full window later: WarningFired flips true → one hook.
	_ = svc.RefreshPair(context.Background(), pair, 1.10, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	if fired != 1 {
		t.Fatalf("hook fired=%d after the divergence persisted, want 1", fired)
	}
}

// TestRefreshPair_BelowQuorumTickFreezesWarning pins that a refresh which
// cannot reach the reference quorum is a no-op on warning state. A firing
// pair drops to one responding reference for a single refresh: the cached
// WarningFired must stay true (not be rewritten to an all-clear), the
// persistence streak must survive so the warning is still up the moment
// references return, and no second webhook may be emitted for the same
// ongoing episode.
func TestRefreshPair_BelowQuorumTickFreezesWarning(t *testing.T) {
	flaky := &stubReference{name: "chainlink", price: 1.00}
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		flaky,
	}
	var fired int
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(_ context.Context, _ canonical.Pair, _ divergence.CachedResult) error {
			fired++
			return nil
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	step := divergence.DefaultWarningPersistence + time.Minute

	_ = svc.RefreshPair(ctx, pair, 1.10, t0)
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step))
	if got := readDivergence(t, rdb, pair); !got.WarningFired || fired != 1 {
		t.Fatalf("setup: WarningFired=%v hooks=%d, want a firing pair with one hook", got.WarningFired, fired)
	}

	flaky.err = divergence.ErrPriceUnavailable
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+time.Minute))
	below := readDivergence(t, rdb, pair)
	if below.SuccessCount != 1 {
		t.Fatalf("below-quorum tick SuccessCount = %d, want 1", below.SuccessCount)
	}
	if !below.WarningFired {
		t.Errorf("below-quorum tick rewrote WarningFired to false; want the last verdict (true) carried forward")
	}

	flaky.err = nil
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+2*time.Minute))
	if got := readDivergence(t, rdb, pair); !got.WarningFired {
		t.Errorf("first refresh after references returned: WarningFired=false; the persistence streak was reset by the below-quorum tick")
	}
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(2*step+2*time.Minute))
	if fired != 1 {
		t.Errorf("hook fired %d times for one uninterrupted divergence, want 1", fired)
	}
}

// restartedService builds a second Service over the SAME Redis, standing
// in for the aggregator process that comes back after a deploy: fresh
// in-memory state, the previous process's cached results still in place.
func restartedService(t *testing.T, refs []divergence.Reference, rdb *redis.Client, hook divergence.WarningHook) *divergence.Service {
	t.Helper()
	svc, err := divergence.NewService(divergence.ServiceOptions{
		References:           refs,
		Cache:                rdb,
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired:       hook,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func depeggedRefs() []divergence.Reference {
	return []divergence.Reference{
		&stubReference{name: "coingecko", price: 0.93},
		&stubReference{name: "chainlink", price: 0.93},
		&stubReference{name: "reflector", price: 0.93},
	}
}

// TestRefreshPair_RestartKeepsPublishedWarning: a divergence that has been
// published for a while must stay published across an aggregator restart,
// and the restarted process must not re-send divergence.firing for it.
// Maps that start empty would make the first post-restart refresh
// write WarningFired=false — /v1/price said "cross-checked and agrees" for
// a 7% depeg — and the next matured refresh re-fired the webhook.
func TestRefreshPair_RestartKeepsPublishedWarning(t *testing.T) {
	refs := depeggedRefs()
	var firstHooks, secondHooks int
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) error {
			firstHooks++
			return nil
		},
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired || firstHooks != 1 {
		t.Fatalf("precondition: want a published warning and one hook, got fired=%v hooks=%d",
			readDivergence(t, rdb, pair).WarningFired, firstHooks)
	}

	restarted := restartedService(t, refs, rdb, func(context.Context, canonical.Pair, divergence.CachedResult) error {
		secondHooks++
		return nil
	})
	t1 := t0.Add(41 * time.Minute)
	_ = restarted.RefreshPair(ctx, pair, 1.00, t1)
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false on the first refresh after a restart for a pair that never " +
			"stopped diverging: the restart published a false all-clear")
	}
	_ = restarted.RefreshPair(ctx, pair, 1.00, t1.Add(divergence.DefaultWarningPersistence+time.Minute))
	if secondHooks != 0 {
		t.Errorf("restarted process fired divergence.firing %d time(s) for a divergence already "+
			"announced before the restart, want 0", secondHooks)
	}
}

// TestRefreshPair_StreakSurvivesRestartInsideDebounce: restarts closer
// together than the persistence window must not reset the debounce clock,
// or a crash-looping aggregator never publishes the warning at all.
func TestRefreshPair_StreakSurvivesRestartInsideDebounce(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 1.00, t0.Add(3*time.Minute))
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true 3m into the streak; the debounce must still hold it")
	}
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 1.00, t0.Add(6*time.Minute))
	got := readDivergence(t, rdb, pair)
	if !got.WarningFired {
		t.Error("WarningFired = false 6m into a divergence spanning two restarts: the streak " +
			"restarted with each process and the warning can never publish")
	}
	if !got.FiringSince.Equal(t0) {
		t.Errorf("FiringSince = %v, want the streak start %v", got.FiringSince, t0)
	}
}

// TestRefreshPair_RestartFromLegacyCachedWarning covers the deploy that
// ships FiringSince: the cached result was written without it, and a
// published warning must still carry over rather than reset.
func TestRefreshPair_RestartFromLegacyCachedWarning(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	legacy, err := json.Marshal(map[string]any{
		"pair_id": pair.String(), "warning_fired": true, "success_count": 3,
		"computed_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.Set(context.Background(), cachekeys.Divergence(pair).String(), legacy, cachekeys.DivergenceTTL).Err(); err != nil {
		t.Fatalf("seed legacy result: %v", err)
	}
	_ = svc.RefreshPair(context.Background(), pair, 1.00, time.Now())
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false after restarting over a pre-FiringSince cached warning")
	}
}

// TestRefreshPair_RestartDoesNotResurrectClearedWarning: the restore only
// carries a streak forward; a pair that is no longer diverging publishes
// false on its first post-restart refresh, whatever the prior entry said.
func TestRefreshPair_RestartDoesNotResurrectClearedWarning(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(10*time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("precondition: want a published warning")
	}
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 0.93, t0.Add(11*time.Minute))
	got := readDivergence(t, rdb, pair)
	if got.WarningFired || !got.FiringSince.IsZero() {
		t.Errorf("after recovery: WarningFired=%v FiringSince=%v, want false and zero",
			got.WarningFired, got.FiringSince)
	}
}

// TestRefreshPair_GapDoesNotMatureStreak: a pair that fired once,
// then went unevaluated for hours (no VWAP, parse error, below quorum), must
// not publish on the single firing observation after the gap. Elapsed time
// across an unobserved interval is not evidence the divergence persisted.
func TestRefreshPair_GapDoesNotMatureStreak(t *testing.T) {
	var fired int
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) error {
			fired++
			return nil
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	afterGap := t0.Add(3 * time.Hour)

	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, afterGap)
	got := readDivergence(t, rdb, pair)
	if got.WarningFired || fired != 0 {
		t.Fatalf("first firing refresh after a 3h unevaluated gap: WarningFired=%v hooks=%d, "+
			"want the warning held — one post-gap sample must not satisfy the persistence debounce",
			got.WarningFired, fired)
	}
	if !got.FiringSince.Equal(afterGap) {
		t.Errorf("FiringSince = %v, want the streak restarted at %v", got.FiringSince, afterGap)
	}

	_ = svc.RefreshPair(ctx, pair, 1.00, afterGap.Add(divergence.DefaultWarningPersistence+time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired || fired != 1 {
		t.Errorf("divergence observed across two refreshes after the gap did not publish (hooks=%d)", fired)
	}
}

// TestRefreshPair_SlowCadenceStillWarns: an operator cadence longer than the
// debounce (divergence_min_interval_seconds = 1h) is not a gap; consecutive
// refreshes one interval apart must still mature the streak.
func TestRefreshPair_SlowCadenceStillWarns(t *testing.T) {
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		RefreshInterval:      time.Hour,
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(time.Hour))
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("two consecutive firing refreshes at a 1h cadence did not publish: the gap reset blinded the warning")
	}
}

// TestRefreshPair_PublishedWarningSurvivesGap: once published, an
// evaluation gap must not flap the warning off and re-send the webhook.
func TestRefreshPair_PublishedWarningSurvivesGap(t *testing.T) {
	var fired int
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) error {
			fired++
			return nil
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(3*time.Hour))
	if !readDivergence(t, rdb, pair).WarningFired || fired != 1 {
		t.Errorf("published warning after a gap: WarningFired=%v hooks=%d, want true and 1",
			readDivergence(t, rdb, pair).WarningFired, fired)
	}
}

// failFirstHook errors on its first call and succeeds afterwards.
func failFirstHook(calls *int) divergence.WarningHook {
	return func(context.Context, canonical.Pair, divergence.CachedResult) error {
		*calls++
		if *calls == 1 {
			return errors.New("transient")
		}
		return nil
	}
}

// A failed hook delivery is retry state only: a following below-quorum tick
// must still carry the published verdict (true) forward, and the retry
// happens on the next evaluated firing refresh.
func TestRefreshPair_HookErrorKeepsVerdictThroughBelowQuorum(t *testing.T) {
	flaky := &stubReference{name: "chainlink", price: 1.00}
	refs := []divergence.Reference{&stubReference{name: "coingecko", price: 1.00}, flaky}
	var calls int
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired:       failFirstHook(&calls),
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	step := divergence.DefaultWarningPersistence + time.Minute

	_ = svc.RefreshPair(ctx, pair, 1.10, t0)
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step))
	if calls != 1 || !readDivergence(t, rdb, pair).WarningFired {
		t.Fatalf("setup: hooks=%d WarningFired=%v, want 1 failed hook on a firing pair",
			calls, readDivergence(t, rdb, pair).WarningFired)
	}

	flaky.err = divergence.ErrPriceUnavailable
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("below-quorum tick after a failed hook rewrote WarningFired to false for a diverging pair")
	}

	flaky.err = nil
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+2*time.Minute))
	if calls != 2 || !readDivergence(t, rdb, pair).WarningFired {
		t.Errorf("after recovery: hooks=%d WarningFired=%v, want the hook retried once and the verdict kept",
			calls, readDivergence(t, rdb, pair).WarningFired)
	}
}

// A failed hook delivery must not make the next refresh after an evaluation
// gap look like a fresh, unpublished streak: the verdict stays true and the
// hook is retried.
func TestRefreshPair_HookErrorKeepsVerdictThroughGap(t *testing.T) {
	var calls int
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired:       failFirstHook(&calls),
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	if calls != 1 {
		t.Fatalf("setup: hooks=%d, want 1 failed hook", calls)
	}

	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(3*time.Hour))
	if got := readDivergence(t, rdb, pair); !got.WarningFired || calls != 2 {
		t.Errorf("refresh after a gap: WarningFired=%v hooks=%d, want true and the hook retried (2)", got.WarningFired, calls)
	}
}
