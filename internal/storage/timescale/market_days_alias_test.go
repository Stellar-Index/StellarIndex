// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestMarketDayFamilies_BindsSACSpellingOntoMember pins CA2-A07-correct-2's
// key half: the SAC spelling of a requested classic asset is bound and
// folds onto that classic member, and a requested spelling always keeps
// itself even when it is also an alias of an earlier request.
func TestMarketDayFamilies_BindsSACSpellingOntoMember(t *testing.T) {
	const (
		issuerAccount   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sorobanContract = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	)
	reg, err := canonical.NewAliasRegistry(map[string]string{sorobanContract: "USTRY:" + issuerAccount})
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
	if !strings.Contains(q, "GROUP BY day, family.member") {
		t.Error("query must group by the alias family's member key")
	}
	if strings.Contains(q, "GROUP BY day, base_asset") {
		t.Error("query groups by the stored base_asset spelling; SAC-form rows would not fold onto their member")
	}
}
