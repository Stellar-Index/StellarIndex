// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestMarketDayFamilies_BindsSACSpellingOntoMember pins the key half: a requested classic asset's SAC
// spelling is bound and folds onto that member, and a requested spelling keeps itself even if an earlier
// request aliases it.
func TestMarketDayFamilies_BindsSACSpellingOntoMember(t *testing.T) {
	const (
		issuerAccount   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sorobanContract = "CAAVZKRT2BJ5A5Y5SAGQNA3IC5EDAODEWBHO5CYR5ASLFDZYOQN6NEO3"
	)
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{sorobanContract: "USTRY:" + issuerAccount})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	ustry, err := canonical.NewClassicAsset("USTRY", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := canonical.NewSorobanAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}

	spellings, members := marketDayFamilies([]canonical.Asset{ustry})
	i := slices.Index(spellings, sac.String())
	if i < 0 {
		t.Fatalf("spellings = %v, want the SAC form %q bound", spellings, sac.String())
	}
	if members[i] != ustry.String() {
		t.Errorf("SAC spelling folds onto %q, want %q", members[i], ustry.String())
	}

	spellings, members = marketDayFamilies([]canonical.Asset{ustry, sac})
	if len(spellings) != 2 {
		t.Fatalf("spellings = %v, want each form bound once", spellings)
	}
	if j := slices.Index(spellings, sac.String()); members[j] != sac.String() {
		t.Errorf("requested SAC spelling folds onto %q, want itself", members[j])
	}
}

// TestDailyMarketDaysQueryGroupsByMember guards the SQL half: grouping by
// the stored spelling would split one member into per-spelling rows and
// double-count an hour traded under both.
func TestDailyMarketDaysQueryGroupsByMember(t *testing.T) {
	q := dailyMarketDaysQuery
	if !strings.Contains(q, "GROUP BY hours.day, hours.member") {
		t.Error("query must group by the alias family's member key")
	}
	if strings.Contains(q, "GROUP BY day, base_asset") {
		t.Error("query groups by the stored base_asset spelling; SAC-form rows would not fold onto their member")
	}
}

// TestDailyMarketDaysQueryReadsBothOrientations: a fill where the maker sold dollars is stored (dollar, asset); reading
// the (asset, dollar) rows alone serves a one-sided VWAP and half the volume.
func TestDailyMarketDaysQueryReadsBothOrientations(t *testing.T) {
	q := dailyMarketDaysQuery
	for _, want := range []string{
		"family.spelling = prices_1h.base_asset",
		"family.spelling = prices_1h.quote_asset",
		"NOT (base_asset = ANY($1) AND quote_asset = ANY($2))",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q", want)
		}
	}
	if n := strings.Count(q, "bucket <= now() - INTERVAL '1 hour'"); n != 2 {
		t.Errorf("closed-bucket guard appears %d time(s), want one per orientation", n)
	}
}

// TestDailyMarketDaysQueryLeavesOutOffMarketHours: the day is built from hours inside the ratio band around its median hour, and the
// centre stays exact (percentile_cont would make it a double).
func TestDailyMarketDaysQueryLeavesOutOffMarketHours(t *testing.T) {
	q := dailyMarketDaysQuery
	for _, want := range []string{
		"percentile_disc(0.5) WITHIN GROUP (ORDER BY usd_leg / asset_leg)",
		"BETWEEN centre.mid / 3 AND centre.mid * 3",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q", want)
		}
	}
	if strings.Contains(q, "percentile_cont") {
		t.Error("percentile_cont returns double precision: the band centre must stay NUMERIC")
	}
}
