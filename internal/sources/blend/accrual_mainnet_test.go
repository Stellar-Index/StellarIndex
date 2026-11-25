package blend

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

// Consecutive stored ResData versions of one reserve (asset contract ADEFCE59…E496), read from
// stellar.ledger_entry_changes. Both pools' Config bstop_rate is 0_2000000; per-window inference
// from the data agrees.
type mainnetVersion struct {
	ledger uint32
	tx     string
	rd     rdIn // d_rate, b_rate, b_supply, d_supply, backstop_credit, last_time
}

func mainnetRows(s blendtest.Slice) []mainnetVersion {
	out := make([]mainnetVersion, len(s.Rows))
	for i, r := range s.Rows {
		out[i] = mainnetVersion{r.Ledger, r.Tx, rdIn{r.DRate, r.BRate, r.BSupply, r.DSupply, r.BackstopCredit, r.LastTime}}
	}
	return out
}

// Every pair must reconcile exactly (stored b_rate recomputed, backstop_credit delta equal to the
// backstop's share) except the listed ones, which must be reported with their exact residual. The
// window totals are then rebuilt from chain values alone: gross by telescoping the ceil'd
// liabilities over each run of constant d_supply, backstop from the backstop_credit endpoints.
func TestAccrueBetween_Mainnet(t *testing.T) {
	cases := []struct {
		v PoolVersion
		s blendtest.Slice
	}{
		{PoolV2, blendtest.BlendV2Supplies},
		{PoolV2, blendtest.BlendV2Repays},
		{PoolV1, blendtest.BlendV1},
	}
	if got := blendtest.BlendV1.Reported[53013801]; got != "-3096224423" {
		t.Fatalf("V1 interest-auction residual fixture = %q", got)
	}
	for _, c := range cases {
		t.Run(c.s.Name, func(t *testing.T) {
			rows := mainnetRows(c.s)
			gross, backstop, residual := reconcileMainnet(t, c.v, rows, c.s.Reported)
			if want := telescopedGross(t, c.v, rows); gross.Cmp(want) != 0 {
				t.Errorf("window gross %s, telescoped from chain values %s", gross, want)
			}
			first, last := rows[0].rd.data(t, c.v), rows[len(rows)-1].rd.data(t, c.v)
			credit := new(big.Int).Sub(last.BackstopCredit, first.BackstopCredit)
			if got := new(big.Int).Add(backstop, residual); got.Cmp(credit) != 0 {
				t.Errorf("window backstop %s + residual %s, chain backstop_credit moved %s", backstop, residual, credit)
			}
		})
	}
}

func reconcileMainnet(t *testing.T, v PoolVersion, rows []mainnetVersion, reported map[uint32]string) (gross, backstop, residual *big.Int) {
	t.Helper()
	gross, backstop, residual = new(big.Int), new(big.Int), new(big.Int)
	for i := 1; i < len(rows); i++ {
		p, n := rows[i-1], rows[i]
		a, err := AccrueBetween(p.rd.data(t, v), n.rd.data(t, v), 2_000_000)
		if err != nil {
			t.Fatalf("ledger %d tx %s: %v", n.ledger, n.tx, err)
		}
		wantResidual := "0"
		if r, ok := reported[n.ledger]; ok {
			wantResidual = r
		}
		if !a.BRateMatches || a.BackstopResidual.String() != wantResidual {
			t.Errorf("ledger %d tx %s: b_rate %s recomputed %s, residual %s want %s",
				n.ledger, n.tx, n.rd.bRate, a.ExpectedBRate, a.BackstopResidual, wantResidual)
		}
		if (p.rd.lt == n.rd.lt) != (a.Gross.Sign() == 0) {
			t.Errorf("ledger %d tx %s: gross %s across last_time %d → %d", n.ledger, n.tx, a.Gross, p.rd.lt, n.rd.lt)
		}
		gross.Add(gross, a.Gross)
		backstop.Add(backstop, a.Backstop)
		residual.Add(residual, a.BackstopResidual)
	}
	return gross, backstop, residual
}

// telescopedGross sums ceil(d_supply·d_rate/S) differences over each run of versions whose d_supply
// (the accrual operand) is constant, using fuzz_test.go's independent rounding helper.
func telescopedGross(t *testing.T, v PoolVersion, rows []mainnetVersion) *big.Int {
	t.Helper()
	s := bi(1_000_000_000_000)
	if v == PoolV1 {
		s = bi(1_000_000_000)
	}
	liab := func(dSupply string, dRate string) *big.Int {
		return refCeil(mul(mustBig(t, dSupply), mustBig(t, dRate)), s)
	}
	total, j := new(big.Int), 0
	for m := 1; m < len(rows); m++ {
		if m == len(rows)-1 || rows[m].rd.dSupply != rows[j].rd.dSupply {
			total.Add(total, liab(rows[j].rd.dSupply, rows[m].rd.dRate))
			total.Sub(total, liab(rows[j].rd.dSupply, rows[j].rd.dRate))
			j = m
		}
	}
	return total
}
