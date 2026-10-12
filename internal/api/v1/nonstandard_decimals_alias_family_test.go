package v1_test

import (
	"context"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A partly-flagged alias family makes the non-alias-folding Lookup serve a WRONG number: the price leg scales per source pair,
// the supply leg by the requested base's decimals, so they diverge by a power of ten. The row arrives at RUNTIME, so the guard
// in Refresh is pinned here; XLM's three disjoint spellings make flagging only one a realistic slip.
func refreshWith(t *testing.T, rows ...timescale.NonstandardDecimalsAsset) {
	t.Helper()
	c := v1.NewNonstandardDecimalsCache(&stubNonstandardDecimalsReader{rows: rows}, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestNonstandardDecimals_PartialAliasFamily_IsCounted(t *testing.T) {
	before := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal)
	// `native` is one of XLM's three spellings; the other two are absent.
	refreshWith(t, timescale.NonstandardDecimalsAsset{Asset: "native", Decimals: 6, Source: "aquarius"})
	if got := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal) - before; got == 0 {
		t.Fatal("a partly-flagged alias family was not counted: price and supply can diverge by a power of ten and nothing would say so")
	}
}

func TestNonstandardDecimals_SingletonFamily_IsNotCounted(t *testing.T) {
	before := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal)
	// Every flagged row on the real table today is a bare C-strkey whose alias
	// family is a singleton — the guard must stay silent on those, or it is
	// noise on every refresh in production.
	refreshWith(t, timescale.NonstandardDecimalsAsset{Asset: flaggedAsset, Decimals: 6, Source: "aquarius"})
	if got := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal) - before; got != 0 {
		t.Fatalf("a singleton alias family was counted %v times — the guard would fire on every production refresh", got)
	}
}

func TestNonstandardDecimals_CompleteAliasFamily_IsNotCounted(t *testing.T) {
	before := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal)
	// The correct way to flag an aliasing asset: every spelling present.
	refreshWith(t,
		timescale.NonstandardDecimalsAsset{Asset: "native", Decimals: 6, Source: "aquarius"},
		timescale.NonstandardDecimalsAsset{Asset: "crypto:XLM", Decimals: 6, Source: "aquarius"},
		timescale.NonstandardDecimalsAsset{Asset: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA", Decimals: 6, Source: "aquarius"},
	)
	if got := obstest.CounterValue(obs.NonstandardDecimalsPartialAliasFamilyTotal) - before; got != 0 {
		t.Fatalf("a fully-flagged alias family was counted %v times", got)
	}
}
