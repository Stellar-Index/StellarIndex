package lendinginterest

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// TelescopedGross recomputes one reserve's summed gross over a chain of stored versions without pairing them
// through AccrueBetween: over each run of pairs whose predecessor holds the same d_supply, Σgross telescopes to
// ceil(d·r_last/S) − ceil(d·r_first/S). prev is the value in force before vs (nil when the first version is a
// baseline); vs are the reserve's versions, in any order, as Fold reads them (removals are gaps, same-content
// duplicates in one transaction one change). It proves the fold summed the chain's d_rate movement, nothing about
// the fee, backstop or b_rate split, and nothing when the chain's order is unknown.
func TelescopedGross(prev *blend.ReserveData, vs []Version) (*big.Int, error) {
	sorted := append([]Version(nil), vs...)
	sort.SliceStable(sorted, func(i, j int) bool { return readLess(sorted[i], sorted[j]) })
	var chain []blend.ReserveData
	if prev != nil {
		chain = append(chain, *prev)
	}
	for _, v := range dedupe(sorted) {
		switch {
		case v.Kind != KindReserve:
			return nil, fmt.Errorf("lendinginterest: telescoping a %d-kind version", v.Kind)
		case v.Removed:
			continue
		case !complete(v.Reserve):
			return nil, fmt.Errorf("lendinginterest: telescoping a reserve version with a missing field at ledger %d", v.Ledger)
		}
		chain = append(chain, v.Reserve)
	}
	total := new(big.Int)
	for j, m := 0, 1; m < len(chain); m++ {
		if m < len(chain)-1 && chain[m].DSupply.Cmp(chain[j].DSupply) == 0 {
			continue
		}
		total.Add(total, liabilities(chain[j].DSupply, chain[m]))
		total.Sub(total, liabilities(chain[j].DSupply, chain[j]))
		j = m
	}
	return total, nil
}

// liabilities is ceil(d·d_rate/S) at rd's rate scale (SCALAR_9 on V1, SCALAR_12 on V2).
func liabilities(d *big.Int, rd blend.ReserveData) *big.Int {
	s := big.NewInt(1_000_000_000_000)
	if rd.Version == blend.PoolV1 {
		s = big.NewInt(1_000_000_000)
	}
	n := new(big.Int).Mul(d, rd.DRate)
	n.Add(n, new(big.Int).Sub(s, big.NewInt(1)))
	return n.Quo(n, s)
}
