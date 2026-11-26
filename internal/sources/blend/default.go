package blend

import (
	"errors"
	"fmt"
	"math/big"
)

// DefaultedBRate is the b_rate V2's User::default_liabilities leaves when it burns dTokens of the reserve with
// nothing repaid (one `defaulted_debt` event): b_rate − ceil(ceil(dTokens·d_rate/S)·S/b_supply), floored at 0.
// rd is the reserve as that default reads it: after the transaction's accrual and any earlier default of it, so
// a transaction's defaults compose by applying each in event order to the last result. d_supply also falls by
// dTokens; b_supply and d_rate do not move.
func DefaultedBRate(rd ReserveData, dTokens *big.Int) (*big.Int, error) {
	if rd.Version != PoolV2 {
		return nil, fmt.Errorf("blend: defaulted_debt on pool version %d", rd.Version)
	}
	for _, f := range [...]*big.Int{rd.DRate, rd.BRate, rd.BSupply, dTokens} {
		if f == nil || f.Sign() < 0 {
			return nil, errors.New("blend: default over a missing or negative amount")
		}
	}
	if rd.BSupply.Sign() == 0 {
		return nil, errors.New("blend: default on a reserve with no b_supply")
	}
	loss := fixedDivCeil(rd.DTokensToUnderlying(dTokens), rd.BSupply, scalar12)
	out := new(big.Int).Sub(rd.BRate, loss)
	if out.Sign() < 0 {
		out.SetInt64(0)
	}
	return out, nil
}
