package v1

import (
	"context"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestReserveHistoryPoint_MatchesLiveReserveView pins that a history point serves the live reserve view's own strings and floats.
func TestReserveHistoryPoint_MatchesLiveReserveView(t *testing.T) {
	big64, _ := new(big.Int).SetString("18446744073709551617", 10)
	cfg := blend.ReserveConfig{CFactor: 9_000_000, LFactor: 9_500_000, SupplyCap: big.NewInt(5), Decimals: 7, Enabled: true}
	st := clickhouse.BlendReserveState{
		Data: blend.ReserveData{
			BRate: big.NewInt(1_000_000_000_001), DRate: big.NewInt(1_100_000_000_000), IRMod: big.NewInt(10_000_000),
			BackstopCredit: big64, Version: blend.PoolV2,
		},
		Metrics: blend.ReserveMetrics{
			SuppliedUnderlying: big64, BorrowedUnderlying: big.NewInt(7), UtilizationPct: 12.3456,
			HasAPR: true, BorrowAPR: 0.061234567, SupplyAPR: 0.041298765,
		},
		Config: &cfg,
	}
	live := reserveStateView(st)
	p := reserveHistoryPoint(clickhouse.LendingReservePoint{CloseTime: time.Unix(100, 0).UTC(), StateLedger: 9, ConfigLedger: 8, State: st})
	if *p.Supplied != live.Supplied || *p.Borrowed != live.Borrowed || *p.UtilizationPct != live.UtilizationPct ||
		!reflect.DeepEqual(p.BorrowAPR, live.BorrowAPR) || !reflect.DeepEqual(p.SupplyAPR, live.SupplyAPR) ||
		!reflect.DeepEqual(p.IRMod, live.IRMod) || !reflect.DeepEqual(p.BackstopCredit, live.BackstopCredit) ||
		*p.CollateralFactor != live.Config.CollateralFactor || *p.LiabilityFactor != live.Config.LiabilityFactor ||
		!reflect.DeepEqual(p.SupplyCap, live.Config.SupplyCap) {
		t.Fatalf("point %+v differs from live view %+v", p, live)
	}
	if *p.BRate != "1000000000001" || *p.DRate != "1100000000000" || *p.ConfigLedger != 8 || *p.Supplied != "18446744073709551617" {
		t.Fatalf("point = %+v", p)
	}
	removed := reserveHistoryPoint(clickhouse.LendingReservePoint{StateLedger: 10, Removed: true})
	if !removed.Removed || removed.Supplied != nil || removed.ConfigLedger != nil || removed.BorrowAPR != nil {
		t.Fatalf("removed point = %+v", removed)
	}
	noCfg := reserveHistoryPoint(clickhouse.LendingReservePoint{StateLedger: 11, State: clickhouse.BlendReserveState{
		Data: st.Data, Metrics: blend.BaseMetrics(blend.ReserveData{BRate: big.NewInt(1), DRate: big.NewInt(1), BSupply: big.NewInt(0), DSupply: big.NewInt(0), Version: blend.PoolV2}),
	}})
	if noCfg.ConfigLedger != nil || noCfg.BorrowAPR != nil || noCfg.SupplyAPR != nil || noCfg.CollateralFactor != nil {
		t.Fatalf("a point with no config in force must carry no APR or config: %+v", noCfg)
	}
}

func TestLendingHistoryFlags_StaleWhenTrailingTheLakeTip(t *testing.T) {
	cases := []struct {
		name     string
		tip, wm  uint32
		dropped  int
		stale    bool
		degraded bool
	}{
		{"within 1000 ledgers", 5000, 4000, 0, false, false},
		{"trails by 1001", 5001, 4000, 0, true, false},
		{"dropped point", 5000, 5000, 1, false, true},
	}
	for _, tc := range cases {
		s := &Server{Options: Options{LakeWatermark: &stubWatermark{ledger: tc.tip, closedAt: time.Now()}}}
		f := s.lendingHistoryFlags(context.Background(), lendingHistoryCoverageView{HistoryThroughLedger: tc.wm, DroppedPoints: tc.dropped})
		if f.Stale != tc.stale || f.Degraded != tc.degraded {
			t.Errorf("%s: flags = %+v", tc.name, f)
		}
	}
}
