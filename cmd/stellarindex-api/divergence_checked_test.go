package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// seedCachedDivergence writes `cached` at the real production key for `pair` and registers the
// quote in the per-base index set, as divergence.Service.RefreshPair does, so the test
// exercises the adapter's predicate rather than a mock.
func seedCachedDivergence(t *testing.T, rdb *redis.Client, pair canonical.Pair, cached divergence.CachedResult) {
	t.Helper()
	ctx := context.Background()
	cached.PairID = pair.String()
	body, err := json.Marshal(cached)
	if err != nil {
		t.Fatalf("marshal cached result: %v", err)
	}
	if err := rdb.Set(ctx, cachekeys.Divergence(pair).String(), body, cachekeys.DivergenceTTL).Err(); err != nil {
		t.Fatalf("seed divergence value key: %v", err)
	}
	idx := cachekeys.DivergenceBaseIndex(pair.Base).String()
	if err := rdb.SAdd(ctx, idx, pair.Quote.String()).Err(); err != nil {
		t.Fatalf("seed divergence index set: %v", err)
	}
}

// TestDivergenceAdapter_ChecksMatchWorkerQuorum pins that the API's `divergence_checked` flag
// uses the same source quorum the worker gates WarningFired on. A below-quorum run reached no
// verdict, so reporting checked=true, warning=false would be a clean bill of health the
// cross-check never issued.
func TestDivergenceAdapter_ChecksMatchWorkerQuorum(t *testing.T) {
	const minSources = 2

	xlm := canonical.NativeAsset()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	pair := canonical.Pair{Base: xlm, Quote: usd}

	cases := []struct {
		name        string
		cached      divergence.CachedResult
		wantFiring  bool
		wantChecked bool
		why         string
	}{
		{
			name: "below quorum — one reference responded",
			cached: divergence.CachedResult{
				SuccessCount:   1,
				FailureCount:   2,
				DivergencePct:  42.0,
				AgreementCount: 0,
				// WarningFired is false because the worker refused to
				// evaluate the gate at all, NOT because prices agree.
				WarningFired: false,
			},
			wantFiring:  false,
			wantChecked: false,
			why:         "one responding reference is below min_sources_for_warning, so the worker reached no verdict; reporting checked=true asserts an all-clear that was never computed",
		},
		{
			name: "exactly at quorum, no divergence",
			cached: divergence.CachedResult{
				SuccessCount:   minSources,
				DivergencePct:  0.2,
				AgreementCount: 2,
				WarningFired:   false,
			},
			wantFiring:  false,
			wantChecked: true,
			why:         "quorum met and the warning gate ran clean — this is a genuine all-clear and must stay checked=true",
		},
		{
			name: "above quorum and firing",
			cached: divergence.CachedResult{
				SuccessCount:   3,
				DivergencePct:  18.0,
				AgreementCount: 0,
				WarningFired:   true,
			},
			wantFiring:  true,
			wantChecked: true,
			why:         "a firing result must never be reported as unchecked",
		},
		{
			name: "every reference dark",
			cached: divergence.CachedResult{
				SuccessCount:   0,
				FailureCount:   3,
				AgreementCount: 0,
				WarningFired:   false,
			},
			wantFiring:  false,
			wantChecked: false,
			why:         "the CS-087 baseline: no responding reference is unchecked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })

			svc, err := divergence.NewService(divergence.ServiceOptions{
				Cache:                rdb,
				Threshold:            5.0,
				MinSourcesForWarning: minSources,
				PerReferenceTimeout:  time.Second,
			})
			if err != nil {
				t.Fatalf("divergence.NewService: %v", err)
			}

			cached := tc.cached
			cached.ComputedAt = time.Now().UTC()
			seedCachedDivergence(t, rdb, pair, cached)

			adapter := wiring.NewDivergenceAdapter(svc)
			firing, checked, _, err := adapter.DivergenceFiringFor(context.Background(), xlm, usd)
			if err != nil {
				t.Fatalf("DivergenceFiringFor: %v", err)
			}
			if firing != tc.wantFiring {
				t.Errorf("firing = %v, want %v — %s", firing, tc.wantFiring, tc.why)
			}
			if checked != tc.wantChecked {
				t.Errorf("checked = %v, want %v (success_count=%d, min_sources=%d) — %s",
					checked, tc.wantChecked, tc.cached.SuccessCount, minSources, tc.why)
			}
		})
	}
}

// TestDivergenceAdapter_QuoteSpecific pins that a diverging XLM/GBP verdict never attaches to an
// XLM/USD response; a per-asset LookupCached ORs every quote's WarningFired together.
func TestDivergenceAdapter_QuoteSpecific(t *testing.T) {
	const minSources = 2

	xlm := canonical.NativeAsset()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("build fiat:GBP: %v", err)
	}
	usdPair := canonical.Pair{Base: xlm, Quote: usd}
	gbpPair := canonical.Pair{Base: xlm, Quote: gbp}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc, err := divergence.NewService(divergence.ServiceOptions{
		Cache:                rdb,
		Threshold:            5.0,
		MinSourcesForWarning: minSources,
		PerReferenceTimeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("divergence.NewService: %v", err)
	}

	// GBP diverges; USD is clean.
	seedCachedDivergence(t, rdb, gbpPair, divergence.CachedResult{
		SuccessCount: 3, DivergencePct: 18.0, AgreementCount: 0, WarningFired: true,
	})
	seedCachedDivergence(t, rdb, usdPair, divergence.CachedResult{
		SuccessCount: 3, DivergencePct: 0.1, AgreementCount: 3, WarningFired: false,
		WindowSeconds: 300,
	})

	adapter := wiring.NewDivergenceAdapter(svc)
	firing, checked, window, err := adapter.DivergenceFiringFor(context.Background(), xlm, usd)
	if err != nil {
		t.Fatalf("DivergenceFiringFor: %v", err)
	}
	if window != 5*time.Minute {
		t.Errorf("window = %v, want the verdict's recorded 5m", window)
	}
	if !checked {
		t.Fatalf("checked = false, want true — the USD pair has its own quorum-meeting cached result")
	}
	if firing {
		t.Errorf("firing = true for XLM/USD, want false — GBP's divergence must not leak onto a different quote of the same base")
	}
}

// TestDivergenceAdapter_UnsetQuorumIsNotAlwaysChecked pins that a min_sources_for_warning of 0 or
// below gets divergence.NewService's fallback (2) and `checked` follows it, so a single
// responding reference never reads as cross-checked on default-config deployments.
func TestDivergenceAdapter_UnsetQuorumIsNotAlwaysChecked(t *testing.T) {
	xlm := canonical.NativeAsset()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	for _, raw := range []int{0, -1} {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		svc, err := divergence.NewService(divergence.ServiceOptions{
			Cache:                rdb,
			MinSourcesForWarning: raw,
			PerReferenceTimeout:  time.Second,
		})
		if err != nil {
			t.Fatalf("divergence.NewService: %v", err)
		}
		seedCachedDivergence(t, rdb, canonical.Pair{Base: xlm, Quote: usd}, divergence.CachedResult{
			SuccessCount: 1, AgreementCount: 1, ComputedAt: time.Now().UTC(),
		})
		_, checked, _, err := wiring.NewDivergenceAdapter(svc).DivergenceFiringFor(context.Background(), xlm, usd)
		if err != nil {
			t.Fatalf("DivergenceFiringFor: %v", err)
		}
		if checked {
			t.Errorf("min_sources_for_warning=%d: one reference read as checked; the unset quorum must default to 2", raw)
		}
	}
}
