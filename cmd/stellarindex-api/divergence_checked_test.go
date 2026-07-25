package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// seedCachedDivergence writes `cached` at the real production key for
// `pair` and registers the quote in the per-base index set, exactly as
// divergence.Service.RefreshPair does. Using the real key builders +
// the real CachedResult JSON keeps this a test of the adapter's
// predicate rather than a test of a mock.
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

// TestDivergenceAdapter_ChecksMatchWorkerQuorum pins COR-14
// (audit-2026-07-23): the API's `divergence_checked` flag must use the
// same source quorum the divergence worker gates WarningFired on.
//
// The worker computes `checked := res.SuccessCount >= s.minSources` and
// hard-forces WarningFired=false below that floor — a below-quorum run
// reached NO verdict. The pre-fix adapter answered `checked =
// SuccessCount > 0`, so a single responding reference produced
// `divergence_checked=true, divergence_warning=false`: a clean bill of
// health the cross-check never issued, which is precisely the
// misreading CS-087 introduced the flag to prevent.
//
// Against the un-fixed `cached.SuccessCount > 0` predicate the
// below-quorum subtest fails with `checked = true, want false`.
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

			adapter := newDivergenceAdapter(svc, minSources)
			firing, checked, err := adapter.DivergenceFiringFor(context.Background(), xlm)
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

// TestNewDivergenceAdapter_ClampsUnsetQuorum — an operator who leaves
// divergence.min_sources_for_warning at 0 gets divergence.NewService's
// own fallback (2). Without the clamp the adapter's predicate would
// degrade to `SuccessCount >= 0`, i.e. "always checked", re-opening
// COR-14 for exactly the default-config deployments it matters most on.
func TestNewDivergenceAdapter_ClampsUnsetQuorum(t *testing.T) {
	for _, raw := range []int{0, -1} {
		got := newDivergenceAdapter(nil, raw).minSources
		if got != defaultDivergenceMinSources {
			t.Errorf("newDivergenceAdapter(_, %d).minSources = %d, want %d (must mirror divergence.NewService's clamp)",
				raw, got, defaultDivergenceMinSources)
		}
	}
}
