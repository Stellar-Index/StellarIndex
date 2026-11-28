//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// checkProtocolEmissions reads /v1/lending/protocols over the rollup's stored days and holds Blend's emissions block to
// what /emissions/history?interval=1d serves over each window's days: per stream, the window's USD (rounded once) and
// its tokens. A window reaching before the pool's first day is a lower bound naming the days it lacks.
func checkProtocolEmissions(t *testing.T, ctx context.Context, base, pool, token string, floor, through time.Time) {
	t.Helper()
	var env struct {
		Data []v1.LendingProtocolStats `json:"data"`
	}
	getEmissionsJSON(t, ctx, base+"/v1/lending/protocols", &env)
	var em *v1.LendingProtocolEmissions
	for i := range env.Data {
		if env.Data[i].Protocol == "blend" {
			em = &env.Data[i].Emissions
		}
	}
	if em == nil || em.Degraded || em.EmissionToken == nil || *em.EmissionToken != token {
		t.Fatalf("blend emissions %+v; want the BLND SAC's block, not degraded", em)
	}
	windows := []struct {
		days int
		w    v1.LendingProtocolEmissionsWindow
	}{{1, em.Day}, {7, em.Week}, {30, em.Month}, {0, em.AllTime}}
	for _, win := range windows {
		from := floor
		if win.days > 0 {
			from = through.AddDate(0, 0, 1-win.days)
		}
		if win.w.ThroughDay == nil || *win.w.ThroughDay != through.Format(time.DateOnly) {
			t.Fatalf("%dd through %v; want %s", win.days, win.w.ThroughDay, through.Format(time.DateOnly))
		}
		s, served := win.w.Streams, servedWindow(t, ctx, base, pool, from, through)
		for stream, got := range map[string]v1.LendingProtocolEmissionAmounts{
			"reserve_schedule": s.ReserveSchedule, "pool_claims": s.PoolClaims,
			"backstop_alloc_backstop": s.BackstopAllocBackstop, "backstop_alloc_pool": s.BackstopAllocPool,
		} {
			usd, amount := served[stream][0], served[stream][1]
			if got.AmountUSD == nil || *got.AmountUSD != usd || got.Amount == nil || *got.Amount != amount {
				t.Errorf("%dd %s = %+v; /emissions/history serves %s USD, %s tokens", win.days, stream, got, usd, amount)
			}
			if short := from.Before(floor); got.LowerBound != short || got.AmountLowerBound != short {
				t.Errorf("%dd %s lower bound %v/%v; want %v (days before the first: %v)", win.days, stream, got.LowerBound,
					got.AmountLowerBound, short, win.w.Excluded)
			}
		}
		if bc := s.BackstopClaims; bc.AmountUSD != nil || bc.USDReason == nil || *bc.USDReason != "stream_unavailable" || !bc.LowerBound {
			t.Errorf("%dd backstop_claims = %+v; want null: Blend's backstop claims name no pool", win.days, bc)
		}
	}
}

// servedWindow is what /emissions/history?interval=1d serves over the days [from, through] as window totals (USD rounded
// once, tokens exact) per stored stream, as [usd, tokens]; a stream with no tokens and no USD is served as nothing.
func servedWindow(t *testing.T, ctx context.Context, base, pool string, from, through time.Time) map[string][2]string {
	t.Helper()
	url := fmt.Sprintf("%s/v1/lending/pools/%s/emissions/history?interval=1d&from=%s&to=%s", base, pool,
		from.Format(time.RFC3339), through.Add(24*time.Hour).Format(time.RFC3339))
	var env struct {
		Data v1.LendingEmissionsHistoryView `json:"data"`
	}
	getEmissionsJSON(t, ctx, url, &env)
	v := env.Data
	zero := func(usd *string, tokens string) [2]string {
		if usd == nil && tokens == "0" {
			return [2]string{"0.00", "0"}
		}
		if usd == nil {
			t.Fatalf("%s: %s tokens served with no USD", url, tokens)
		}
		return [2]string{*usd, tokens}
	}
	out := map[string][2]string{
		"pool_claims":             zero(v.PoolClaims.USD.AmountUSD, v.PoolClaims.Totals.Amount),
		"backstop_alloc_backstop": {"0.00", "0"}, "backstop_alloc_pool": {"0.00", "0"}, "reserve_schedule": {"0.00", "0"},
	}
	if u := v.BackstopAlloc.USD; u != nil {
		out["backstop_alloc_backstop"] = zero(u.BackstopEmissionsUSD, v.BackstopAlloc.Totals.BackstopEmissions)
		out["backstop_alloc_pool"] = zero(u.PoolEmissionsUSD, v.BackstopAlloc.Totals.PoolEmissions)
	}
	switch lines := v.Reserves.Streams; len(lines) {
	case 0:
	case 1:
		out["reserve_schedule"] = zero(lines[0].USD.AmountUSD, lines[0].Tokens)
	default:
		t.Fatalf("%s: %d schedule lines; this check reads one window total per stream", url, len(lines))
	}
	return out
}

func getEmissionsJSON(t *testing.T, ctx context.Context, url string, into any) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}
