//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// checkProtocolEmissions reads /v1/lending/protocols over the rollup's stored days and holds Blend's emissions block to
// the days /emissions/history?interval=1d served (rows, each proven equal to its served day): per window and stream, the
// exact USD summed then rounded once, and the tokens summed exactly. A window reaching before the pool's first day is a
// lower bound naming the days it lacks.
func checkProtocolEmissions(t *testing.T, ctx context.Context, base, token string, floor, through time.Time, rows []timescale.LendingEmissionsUSDDay) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/lending/protocols", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/lending/protocols: %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.LendingProtocolStats `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
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
		s := win.w.Streams
		for stream, got := range map[string]v1.LendingProtocolEmissionAmounts{
			"reserve_schedule": s.ReserveSchedule, "pool_claims": s.PoolClaims,
			"backstop_alloc_backstop": s.BackstopAllocBackstop, "backstop_alloc_pool": s.BackstopAllocPool,
		} {
			usd, amount := servedSums(t, rows, stream, from, through)
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

// servedSums are a stream's served days in [from, through]: USD summed exactly and rounded once, tokens exactly.
func servedSums(t *testing.T, rows []timescale.LendingEmissionsUSDDay, stream string, from, through time.Time) (string, string) {
	t.Helper()
	usd, amount := new(big.Rat), new(big.Rat)
	for _, r := range rows {
		if r.Stream != stream || r.Day.Before(from) || r.Day.After(through) {
			continue
		}
		a, ok := new(big.Rat).SetString(r.Amount)
		if !ok || r.AmountUSD == nil {
			t.Fatalf("served row %+v: tokens or USD not a decimal", r)
		}
		u, ok := new(big.Rat).SetString(*r.AmountUSD)
		if !ok {
			t.Fatalf("served row %+v: USD not a decimal", r)
		}
		amount.Add(amount, a)
		usd.Add(usd, u)
	}
	tokens := amount.FloatString(0)
	if !amount.IsInt() {
		tokens = amount.FloatString(7)
	}
	return usd.FloatString(2), tokens
}
