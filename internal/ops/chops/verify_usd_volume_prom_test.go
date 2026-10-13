// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestRenderVerifyUSDVolumeProm_emitsEveryKnownTier(t *testing.T) {
	t.Parallel()
	est := newTierRollup()
	est.groups = 7
	est.pricedRows = 1200
	est.unpricedRows = 3
	est.sumUSD.SetFrac(big.NewInt(1234567891), big.NewInt(100)) // 12,345,678.91
	rollups := map[timescale.USDVolumeTier]*usdVolumeTierRollup{timescale.TierEstimated: est}

	day := time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 13, 5, 14, 0, 0, time.UTC)
	out := renderVerifyUSDVolumeProm(day, rollups, 2, now)

	for _, want := range []string{
		"# TYPE stellarindex_verify_usd_volume_tier_usd gauge\n",
		`stellarindex_verify_usd_volume_tier_usd{tier="estimated"} 12345678.91` + "\n",
		`stellarindex_verify_usd_volume_tier_usd{tier="quote_pegged"} 0` + "\n",
		`stellarindex_verify_usd_volume_tier_usd{tier="base_pegged"} 0` + "\n",
		`stellarindex_verify_usd_volume_tier_usd{tier="unvaluable"} 0` + "\n",
		`stellarindex_verify_usd_volume_tier_groups{tier="estimated"} 7` + "\n",
		`stellarindex_verify_usd_volume_tier_rows{tier="estimated",priced="yes"} 1200` + "\n",
		`stellarindex_verify_usd_volume_tier_rows{tier="estimated",priced="no"} 3` + "\n",
		`stellarindex_verify_usd_volume_tier_rows{tier="unvaluable",priced="yes"} 0` + "\n",
		"stellarindex_verify_usd_volume_violations 2\n",
		fmt.Sprintf("stellarindex_verify_usd_volume_day_unix %d\n", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).Unix()),
		fmt.Sprintf("stellarindex_verify_usd_volume_last_run_unix %d\n", now.Unix()),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "e+") {
		t.Errorf("money rendered in float notation:\n%s", out)
	}
}

func TestRenderVerifyUSDVolumeProm_unknownTierAfterKnownOnes(t *testing.T) {
	t.Parallel()
	rollups := map[timescale.USDVolumeTier]*usdVolumeTierRollup{
		timescale.USDVolumeTier("zeta"):  newTierRollup(),
		timescale.USDVolumeTier("alpha"): newTierRollup(),
	}
	out := renderVerifyUSDVolumeProm(time.Unix(0, 0).UTC(), rollups, 0, time.Unix(0, 0))
	unv := strings.Index(out, `tier_usd{tier="unvaluable"}`)
	alpha := strings.Index(out, `tier_usd{tier="alpha"}`)
	zeta := strings.Index(out, `tier_usd{tier="zeta"}`)
	if unv < 0 || alpha < 0 || zeta < 0 || !(unv < alpha && alpha < zeta) {
		t.Fatalf("tier order wrong (unvaluable=%d alpha=%d zeta=%d):\n%s", unv, alpha, zeta, out)
	}
	if n := strings.Count(out, "# TYPE "); n != 6 {
		t.Fatalf("want 6 TYPE headers, got %d", n)
	}
}
