package supply

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// liveArmDown is a live reserve reader whose balance read falls through
// (one reserve account unobserved) while its observer watermark is
// healthy — the shape in which the static map used to be published
// under the live basis with a fresh-looking anchor.
type liveArmDown struct{ watermark uint32 }

func (l liveArmDown) ReserveBalanceTotal(_ context.Context, _ []string, _ uint32) (*big.Int, error) {
	return nil, fmt.Errorf("%w: account GA2: not found", ErrNoObservation)
}

func (l liveArmDown) MinReserveAccountLedger(_ context.Context, _ []string, _ uint32) (uint32, error) {
	return l.watermark, nil
}

func staticArmComputer(t *testing.T) *XLMComputer {
	t.Helper()
	static, err := NewConfigReserveBalanceReader(map[string]string{"GA1": "100", "GA2": "200"}, time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("static reader: %v", err)
	}
	c, err := NewXLMComputer([]string{"GA1", "GA2"}, NewChainedReserveBalanceReader(liveArmDown{watermark: 49_999_990}, static))
	if err != nil {
		t.Fatalf("computer: %v", err)
	}
	return c
}

// A snapshot built from the static map must say so on the wire and must
// not borrow the live observer's freshness anchor.
func TestXLMCompute_StaticArmHasDistinctBasisAndNoAnchor(t *testing.T) {
	got, err := staticArmComputer(t).Compute(context.Background(), 50_000_000, time.Now())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got.Basis != BasisXLMSDFReserveExclusionStatic {
		t.Errorf("Basis = %q, want %q", got.Basis, BasisXLMSDFReserveExclusionStatic)
	}
	if got.MinComponentLedger != 0 {
		t.Errorf("MinComponentLedger = %d, want 0: a hand-entered map has no freshness anchor", got.MinComponentLedger)
	}
	want := new(big.Int).Sub(XLMTotalSupplyStroops(), big.NewInt(300))
	if got.CirculatingSupply.Cmp(want) != 0 {
		t.Errorf("circulating = %s, want %s", got.CirculatingSupply, want)
	}
}

func TestRefresher_StaticReserveArm(t *testing.T) {
	ledgers := stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()}

	t.Run("strict freshness refuses it", func(t *testing.T) {
		ins := &stubInserter{}
		r := NewRefresher(ledgers, staticArmComputer(t), ins, discardLogger(), WithStrictFreshnessRequired(true))
		out := r.Tick(context.Background())
		if out.Kind != OutcomeKindMissingFreshness || ins.calls != 0 {
			t.Fatalf("outcome %q, inserts %d; want missing_freshness and no insert", out.Kind, ins.calls)
		}
	})

	t.Run("permissive publishes it under a non-ok outcome", func(t *testing.T) {
		ins := &stubInserter{}
		r := NewRefresher(ledgers, staticArmComputer(t), ins, discardLogger())
		out := r.Tick(context.Background())
		if out.Kind != OutcomeKindStaticReserve || ins.calls != 1 {
			t.Fatalf("outcome %q, inserts %d; want static_reserve and one insert", out.Kind, ins.calls)
		}
	})

	t.Run("strict refuses a static basis even with an anchor", func(t *testing.T) {
		ins := &stubInserter{}
		comp := stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000),
			CirculatingSupply:  big.NewInt(900),
			Basis:              BasisXLMSDFReserveExclusionStatic,
			MinComponentLedger: 49_999_999,
		}}
		r := NewRefresher(ledgers, comp, ins, discardLogger(), WithStrictFreshnessRequired(true))
		if out := r.Tick(context.Background()); out.Kind != OutcomeKindMissingFreshness || ins.calls != 0 {
			t.Fatalf("outcome %q, inserts %d; want missing_freshness and no insert", out.Kind, ins.calls)
		}
	})
}

// An expired static map must fail the tick rather than fall back.
func TestXLMCompute_ExpiredStaticArmFails(t *testing.T) {
	static, err := NewConfigReserveBalanceReader(map[string]string{"GA1": "100", "GA2": "200"},
		time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("static reader: %v", err)
	}
	static.now = func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) }
	c, err := NewXLMComputer([]string{"GA1", "GA2"}, NewChainedReserveBalanceReader(liveArmDown{}, static))
	if err != nil {
		t.Fatalf("computer: %v", err)
	}
	ins := &stubInserter{}
	r := NewRefresher(stubLedgers{ledger: 50_000_000, observedAt: time.Now()}, c, ins, discardLogger())
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindComputeError || ins.calls != 0 {
		t.Fatalf("outcome %q, inserts %d; want compute_error and no insert (err %v)", out.Kind, ins.calls, out.Err)
	}
}
