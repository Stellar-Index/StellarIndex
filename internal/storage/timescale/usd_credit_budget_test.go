package timescale

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const creditTestIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// backingFXResolver is a stubFXResolver that also reports anchor backing, keyed by asset string.
type backingFXResolver struct {
	stubFXResolver
	backing map[string]string
}

func (b backingFXResolver) AnchorBackingAt(_ context.Context, asset canonical.Asset, _ time.Time) (*big.Rat, bool, error) {
	s, ok := b.backing[asset.String()]
	if !ok {
		return nil, false, nil
	}
	v, _ := new(big.Rat).SetString(s)
	return v, true, nil
}

func rat(s string) *big.Rat {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return v
}

var creditTestTrade int

// mustAdmit admits a fresh trade ID each call.
func mustAdmit(t *testing.T, b *usdCreditBudget, a canonical.Asset, at time.Time, value, backing string, commit bool) bool {
	t.Helper()
	creditTestTrade++
	return mustAdmitID(t, b, fmt.Sprintf("trade-%d", creditTestTrade), a, at, value, backing, commit)
}

func mustAdmitID(t *testing.T, b *usdCreditBudget, id string, a canonical.Asset, at time.Time, value, backing string, commit bool) bool {
	t.Helper()
	ok, err := b.admit(t.Context(), id, a, at, rat(value), rat(backing), commit)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return ok
}

func TestCreditBudget_AdmitsUpToTwiceBackingThenRefuses(t *testing.T) {
	b := newUSDCreditBudget(nil)
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

	if !mustAdmit(t, b, abc, at, "1500", "1000", true) {
		t.Fatal("1500 against a 2000 budget refused")
	}
	if !mustAdmit(t, b, abc, at.Add(time.Minute), "500", "1000", true) {
		t.Fatal("print landing exactly on the budget refused")
	}
	if mustAdmit(t, b, abc, at.Add(2*time.Minute), "0.0000001", "1000", true) {
		t.Fatal("print past the budget admitted")
	}
}

func TestCreditBudget_ZeroBackingRefusesAnyValue(t *testing.T) {
	b := newUSDCreditBudget(nil)
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	if mustAdmit(t, b, abc, time.Now(), "0.01", "0", true) {
		t.Fatal("an asset with no anchor turnover was credited")
	}
}

func TestCreditBudget_DryRunLeavesWindowUnchanged(t *testing.T) {
	b := newUSDCreditBudget(nil)
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if !mustAdmit(t, b, abc, at, "2000", "1000", false) {
			t.Fatalf("dry run %d refused: an earlier dry run was charged", i)
		}
	}
}

func TestCreditBudget_ChargesExpireAfterTheWindow(t *testing.T) {
	b := newUSDCreditBudget(nil)
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)
	mustAdmit(t, b, abc, at, "2000", "1000", true)

	if mustAdmit(t, b, abc, at.Add(23*time.Hour), "1", "1000", false) {
		t.Fatal("charge dropped while its hour is still one of the 24 in the window")
	}
	if !mustAdmit(t, b, abc, at.Add(24*time.Hour), "2000", "1000", false) {
		t.Fatal("charge still counted 24 hour buckets later")
	}
}

func TestCreditBudget_XLMFormsShareOneLedger(t *testing.T) {
	b := newUSDCreditBudget(nil)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mustAdmit(t, b, canonical.NativeAsset(), at, "2000", "1000", true)
	if mustAdmit(t, b, mustAsset(t, "crypto:XLM"), at, "1", "1000", true) {
		t.Fatal("crypto:XLM escaped the budget native already spent")
	}
}

func TestCreditBudget_SeedChargedOnceAndErrorsPropagate(t *testing.T) {
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	calls := 0
	b := newUSDCreditBudget(func(_ context.Context, _ canonical.Asset, from, to time.Time) (map[int64]*big.Rat, error) {
		calls++
		if !to.Equal(at) || !from.Equal(at.Add(-23*time.Hour)) {
			t.Errorf("seed window [%s, %s), want the 24 hour buckets ending at the print", from, to)
		}
		return map[int64]*big.Rat{creditHour(at.Add(-23 * time.Hour)): rat("1500"), creditHour(at): rat("400")}, nil
	})
	if mustAdmit(t, b, abc, at, "101", "1000", true) {
		t.Fatal("a restarted writer ignored the USD already served")
	}
	if !mustAdmit(t, b, abc, at, "100", "1000", true) {
		t.Fatal("print within the seeded budget refused")
	}
	if calls != 1 {
		t.Fatalf("seed ran %d times, want once per asset", calls)
	}
	// The seed keeps its hourly shape: the 1500 bucket leaves the window an hour later, not a whole window later.
	if !mustAdmit(t, b, abc, at.Add(time.Hour), "1400", "1000", true) {
		t.Fatal("a seeded hour outlived the window")
	}

	boom := errors.New("db down")
	failing := newUSDCreditBudget(func(context.Context, canonical.Asset, time.Time, time.Time) (map[int64]*big.Rat, error) {
		return nil, boom
	})
	if _, err := failing.admit(context.Background(), "t", abc, at, rat("1"), rat("1000"), true); !errors.Is(err, boom) {
		t.Fatalf("seed error = %v, want it propagated, never read as an empty window", err)
	}
}

func TestCreditBudget_RetriedTradeChargedOnce(t *testing.T) {
	b := newUSDCreditBudget(nil)
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !mustAdmitID(t, b, "retried", abc, at, "1500", "1000", true) {
			t.Fatalf("attempt %d of the same trade refused: an earlier attempt was charged again", i)
		}
	}
	if !mustAdmit(t, b, abc, at, "500", "1000", true) {
		t.Fatal("retries consumed budget a single charge leaves free")
	}
}

func TestCreditBudget_SweepDropsStaleLedgers(t *testing.T) {
	b := newUSDCreditBudget(nil)
	stale := mustAsset(t, "OLD-"+creditTestIssuer)
	live := mustAsset(t, "ABC-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mustAdmit(t, b, stale, at, "1", "1000", true)
	later := at.Add(3 * creditBudgetWindow)
	for i := 0; i < 1024; i++ {
		mustAdmit(t, b, live, later, "0", "1000", true)
	}
	if _, ok := b.served[creditKey(stale)]; ok {
		t.Fatal("a ledger two windows stale survived the sweep")
	}
	if _, ok := b.served[creditKey(live)]; !ok {
		t.Fatal("sweep dropped a live ledger")
	}
	if len(b.charged) != 1024 {
		t.Fatalf("charged holds %d IDs, want the 1024 live ones (stale dropped)", len(b.charged))
	}
}

func TestCreditBudget_SweepKeepsAFreshEmptyLedger(t *testing.T) {
	b := newUSDCreditBudget(nil)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	filler := mustAsset(t, "ABC-"+creditTestIssuer)
	b.latest = at
	b.admits = 1022
	mustAdmit(t, b, filler, at, "0", "1000", true)
	fresh := mustAsset(t, "NEW-"+creditTestIssuer)
	mustAdmit(t, b, fresh, at, "1500", "1000", true) // the 1024th admit sweeps
	if mustAdmit(t, b, fresh, at, "600", "1000", true) {
		t.Fatal("the sweep dropped a ledger created in the same admit, losing its charge")
	}
}

func TestCreditBoundUSDVolume(t *testing.T) {
	ctx := context.Background()
	abc := mustAsset(t, "ABC-"+creditTestIssuer)
	def := mustAsset(t, "DEF-"+creditTestIssuer)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// 10 DEF at $50 = $500 against the rated leg's $800.
	legs := creditLegs{rated: abc, ratedVal: rat("800"), other: def, otherAmount: canonical.NewAmount(big.NewInt(100_000_000)), decimals: 7, at: at}
	r := backingFXResolver{
		stubFXResolver: stubFXResolver{prices: map[string]string{def.String(): "50"}},
		backing:        map[string]string{abc.String(): "100", def.String(): "300"},
	}

	t.Run("stores the smaller leg, charged to the deeper anchor market", func(t *testing.T) {
		b := newUSDCreditBudget(nil)
		v, refused, err := creditBoundUSDVolume(ctx, r, b, legs, true)
		if err != nil || refused || v == nil || v.Cmp(rat("500")) != 0 {
			t.Fatalf("got (%v, %v, %v), want 500 admitted", v, refused, err)
		}
		// DEF's budget is 600 with 500 spent; ABC's (200) is untouched.
		if mustAdmit(t, b, def, at, "101", "300", false) {
			t.Fatal("the charge did not land on DEF")
		}
		if !mustAdmit(t, b, abc, at, "200", "100", false) {
			t.Fatal("the charge landed on the thinner leg")
		}
	})

	t.Run("refused over budget is NULL, not capped", func(t *testing.T) {
		thin := r
		thin.backing = map[string]string{abc.String(): "100", def.String(): "200"}
		v, refused, err := creditBoundUSDVolume(ctx, thin, newUSDCreditBudget(nil), legs, true)
		if err != nil || !refused || v != nil {
			t.Fatalf("got (%v, %v, %v), want refused with no value", v, refused, err)
		}
	})

	t.Run("unrated other leg charges the rated leg at its own value", func(t *testing.T) {
		unrated := r
		unrated.stubFXResolver = stubFXResolver{prices: map[string]string{}}
		v, refused, err := creditBoundUSDVolume(ctx, unrated, newUSDCreditBudget(nil), legs, true)
		if err != nil || !refused || v != nil {
			t.Fatalf("got (%v, %v, %v), want 800 refused against ABC's 200 budget", v, refused, err)
		}
	})

	t.Run("an unrated other leg keeps the single-leg ceiling", func(t *testing.T) {
		deep := backingFXResolver{stubFXResolver: stubFXResolver{prices: map[string]string{}}, backing: map[string]string{abc.String(): "1000000000"}}
		huge := legs
		huge.ratedVal = rat("150000000")
		v, refused, err := creditBoundUSDVolume(ctx, deep, newUSDCreditBudget(nil), huge, true)
		if err != nil || refused || v != nil {
			t.Fatalf("got (%v, %v, %v), want a $150M one-leg print unpriced however deep the budget", v, refused, err)
		}
	})

	// The legacy ceiling keeps the rated leg while the legs agree within 10x.
	t.Run("no budget or no backing reporter keeps the legacy ceiling", func(t *testing.T) {
		v, refused, err := creditBoundUSDVolume(ctx, r, nil, legs, true)
		if err != nil || refused || v == nil || v.Cmp(rat("800")) != 0 {
			t.Fatalf("nil budget: got (%v, %v, %v), want the legacy 800", v, refused, err)
		}
		v, refused, err = creditBoundUSDVolume(ctx, r.stubFXResolver, newUSDCreditBudget(nil), legs, true)
		if err != nil || refused || v == nil || v.Cmp(rat("800")) != 0 {
			t.Fatalf("plain resolver: got (%v, %v, %v), want the legacy 800", v, refused, err)
		}
	})
}
