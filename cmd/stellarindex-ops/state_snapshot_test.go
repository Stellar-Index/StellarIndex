package main

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestParseSnapScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantAll   bool
		wantStore bool
		wantErr   bool
	}{
		{"contracts", false, false, false},
		{"all", true, false, false},
		{"storage", false, true, false},
		{"", false, false, true},
		{"nonsense", false, false, true},
	}
	for _, c := range cases {
		sc, err := parseSnapScope(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSnapScope(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSnapScope(%q): unexpected error %v", c.in, err)
			continue
		}
		if sc.all != c.wantAll || sc.storage != c.wantStore {
			t.Errorf("parseSnapScope(%q) = %+v, want {all:%v storage:%v}", c.in, sc, c.wantAll, c.wantStore)
		}
	}
}

// TestShouldCollect_ContractDataStorage is the regression guard for the
// 2026-07-06 dormant-current-state fill: contract_data STORAGE entries (SAC
// Balance / Blend reserve) must be collected under scope=storage but NOT under
// scope=contracts or scope=all — the exact gap that hid ~99% of PHO supply.
func TestShouldCollect_ContractDataStorage(t *testing.T) {
	t.Parallel()
	// (typ, isInstance) → collected? per scope.
	type key struct {
		typ        xdr.LedgerEntryType
		isInstance bool
	}
	cases := []struct {
		name                    string
		k                       key
		contracts, all, storage bool
	}{
		{"contract_data storage", key{xdr.LedgerEntryTypeContractData, false}, false, false, true},
		{"contract_data instance", key{xdr.LedgerEntryTypeContractData, true}, true, true, true},
		{"contract_code", key{xdr.LedgerEntryTypeContractCode, false}, true, true, true},
		{"liquidity_pool", key{xdr.LedgerEntryTypeLiquidityPool, false}, false, true, true},
		{"account", key{xdr.LedgerEntryTypeAccount, false}, false, true, false},
		{"trustline", key{xdr.LedgerEntryTypeTrustline, false}, false, true, false},
		{"ttl (never)", key{xdr.LedgerEntryTypeTtl, false}, false, false, false},
	}
	scopes := map[string]snapScope{
		"contracts": {},
		"all":       {all: true},
		"storage":   {storage: true},
	}
	for _, c := range cases {
		want := map[string]bool{"contracts": c.contracts, "all": c.all, "storage": c.storage}
		for sname, sc := range scopes {
			tally := &snapTally{scope: sc}
			got := tally.shouldCollect(c.k.typ, c.k.isInstance)
			if got != want[sname] {
				t.Errorf("%s under scope=%s: shouldCollect=%v, want %v", c.name, sname, got, want[sname])
			}
		}
	}
}

func TestWithinModWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		maxMod    uint32
		ledgerSeq uint32
		want      bool
	}{
		{0, 55_000_000, true},           // no bound → always in
		{0, 63_000_000, true},           // no bound → always in
		{62_000_000, 55_000_000, true},  // dormant tail → in
		{62_000_000, 62_000_000, false}, // at the floor → out (strictly below)
		{62_000_000, 63_000_000, false}, // above floor → out (already captured live)
	}
	for _, c := range cases {
		tally := &snapTally{maxModLedger: c.maxMod}
		if got := tally.withinModWindow(c.ledgerSeq); got != c.want {
			t.Errorf("withinModWindow(maxMod=%d, ls=%d) = %v, want %v", c.maxMod, c.ledgerSeq, got, c.want)
		}
	}
}
