package blend

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// Account health as the pool contract computes it (health_factor.rs::calculate_from_positions,
// blend-contracts v1.0.0 and blend-contracts-v2 v2.0.0). Every step is an integer floor or ceil at
// the contract's own scale; docs/protocols/blend.md "Health factor" cites each line.

// Missing-input errors. AccountHealth wraps them with the reserve asset; the caller excludes the
// account rather than valuing it on a guessed zero.
var (
	ErrUnknownPoolVersion = errors.New("blend: unknown pool version")
	ErrMissingReserve     = errors.New("blend: reserve missing")
	ErrMissingConfig      = errors.New("blend: reserve config missing")
	ErrMissingRate        = errors.New("blend: reserve rate missing")
	ErrMissingPrice       = errors.New("blend: oracle price missing")
	ErrPriceBaseMismatch  = errors.New("blend: oracle prices do not share one base")
	ErrInvalidInput       = errors.New("blend: invalid health input")
)

// Positions is an account's Positions entry keyed by reserve asset (b-tokens and d-tokens).
type Positions struct {
	Collateral  map[string]*big.Int
	Liabilities map[string]*big.Int
	Supply      map[string]*big.Int // non-collateral supply; never counts toward health
}

// HealthReserve is the state of one reserve that health reads: its stored rates and its config
// (c_factor, l_factor and decimals).
type HealthReserve struct {
	Data   *ReserveData
	Config *ReserveConfig
}

// OraclePrice is a raw SEP-40 lastprice value with the oracle's decimals.
type OraclePrice struct {
	Price    *big.Int
	Decimals uint32
}

func checkVersion(rd ReserveData, version PoolVersion) error {
	if version != PoolV1 && version != PoolV2 {
		return fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	if rd.Version != version {
		return fmt.Errorf("%w: reserve data is pool version %d, health asked for %d", ErrInvalidInput, rd.Version, version)
	}
	return nil
}

// EffectiveCollateral is reserve.rs::to_effective_asset_from_b_token: floor(floor(b·b_rate)·c_factor).
func EffectiveCollateral(rd ReserveData, cfg ReserveConfig, version PoolVersion, bTokens *big.Int) (*big.Int, error) {
	if err := checkVersion(rd, version); err != nil {
		return nil, err
	}
	if rd.BRate == nil {
		return nil, ErrMissingRate
	}
	return fixedMulFloor(rd.BTokensToUnderlying(bTokens), big.NewInt(int64(cfg.CFactor)), scalar7), nil
}

// EffectiveLiability is reserve.rs::to_effective_asset_from_d_token: ceil(ceil(d·d_rate)/l_factor).
func EffectiveLiability(rd ReserveData, cfg ReserveConfig, version PoolVersion, dTokens *big.Int) (*big.Int, error) {
	if err := checkVersion(rd, version); err != nil {
		return nil, err
	}
	if rd.DRate == nil {
		return nil, ErrMissingRate
	}
	if cfg.LFactor == 0 {
		return nil, fmt.Errorf("%w: l_factor is zero", ErrInvalidInput)
	}
	return fixedDivCeil(rd.DTokensToUnderlying(dTokens), big.NewInt(int64(cfg.LFactor)), scalar7), nil
}

// AccountHealth returns the account's effective collateral and liability in the oracle's base,
// summed per reserve as floor(price·collateral/scalar) and ceil(price·liability/scalar).
// Non-collateral supply never counts. Any missing input for a held reserve is an error naming it.
func AccountHealth(pos Positions, reserves map[string]HealthReserve, prices map[string]OraclePrice, version PoolVersion) (collateralBase, liabilityBase *big.Int, err error) {
	if version != PoolV1 && version != PoolV2 {
		return nil, nil, fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	collateralBase, liabilityBase = new(big.Int), new(big.Int)
	var base *uint32
	for _, asset := range heldAssets(pos) {
		b, err := balance(pos.Collateral, asset)
		if err != nil {
			return nil, nil, fmt.Errorf("collateral %s: %w", asset, err)
		}
		d, err := balance(pos.Liabilities, asset)
		if err != nil {
			return nil, nil, fmt.Errorf("liability %s: %w", asset, err)
		}
		// health_factor.rs skips a reserve holding neither, without loading it or its price.
		if b.Sign() == 0 && d.Sign() == 0 {
			continue
		}
		res, price, err := healthInputs(asset, reserves, prices)
		if err != nil {
			return nil, nil, err
		}
		if base == nil {
			base = &price.Decimals
		} else if *base != price.Decimals {
			return nil, nil, fmt.Errorf("%w: %s has %d decimals, another reserve %d", ErrPriceBaseMismatch, asset, price.Decimals, *base)
		}
		cb, lb, err := reserveBase(res, price.Price, version, b, d)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", asset, err)
		}
		collateralBase.Add(collateralBase, cb)
		liabilityBase.Add(liabilityBase, lb)
	}
	return collateralBase, liabilityBase, nil
}

// reserveBase values one reserve's collateral (floor) and liability (ceil) at price/10^decimals.
func reserveBase(res HealthReserve, price *big.Int, version PoolVersion, b, d *big.Int) (cb, lb *big.Int, err error) {
	scalar := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(res.Config.Decimals)), nil)
	cb, lb = new(big.Int), new(big.Int)
	if b.Sign() > 0 {
		eff, err := EffectiveCollateral(*res.Data, *res.Config, version, b)
		if err != nil {
			return nil, nil, fmt.Errorf("collateral: %w", err)
		}
		cb = fixedMulFloor(price, eff, scalar)
	}
	if d.Sign() > 0 {
		eff, err := EffectiveLiability(*res.Data, *res.Config, version, d)
		if err != nil {
			return nil, nil, fmt.Errorf("liability: %w", err)
		}
		lb = fixedMulCeil(price, eff, scalar)
	}
	return cb, lb, nil
}

// heldAssets lists the assets with a collateral or liability entry, sorted so errors are stable.
func heldAssets(pos Positions) []string {
	seen := map[string]bool{}
	for a := range pos.Collateral {
		seen[a] = true
	}
	for a := range pos.Liabilities {
		seen[a] = true
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// balance reads an absent entry as zero; a present nil or a negative balance is a decode fault.
func balance(m map[string]*big.Int, asset string) (*big.Int, error) {
	v, ok := m[asset]
	if !ok {
		return new(big.Int), nil
	}
	if v == nil {
		return nil, fmt.Errorf("%w: nil balance", ErrInvalidInput)
	}
	if v.Sign() < 0 {
		return nil, fmt.Errorf("%w: negative balance %s", ErrInvalidInput, v)
	}
	return v, nil
}

func healthInputs(asset string, reserves map[string]HealthReserve, prices map[string]OraclePrice) (HealthReserve, OraclePrice, error) {
	res, ok := reserves[asset]
	if !ok {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s", ErrMissingReserve, asset)
	}
	if res.Config == nil {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s", ErrMissingConfig, asset)
	}
	if res.Data == nil || res.Data.BRate == nil || res.Data.DRate == nil {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s", ErrMissingRate, asset)
	}
	price, ok := prices[asset]
	if !ok || price.Price == nil {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s", ErrMissingPrice, asset)
	}
	// V2 load_price refuses price <= 0; V1 does not check, and we refuse rather than value on it.
	if price.Price.Sign() <= 0 {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s price %s is not positive", ErrInvalidInput, asset, price.Price)
	}
	return res, price, nil
}

// Liquidatable reports whether new_auction / new_liquidation_auction's health gate admits a user
// liquidation: V2 requires liability_base > collateral_base, V1 only that liability_base is not
// below collateral_base. An account with no debt is never liquidatable.
func Liquidatable(version PoolVersion, collateralBase, liabilityBase *big.Int) (bool, error) {
	if version != PoolV1 && version != PoolV2 {
		return false, fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	if liabilityBase.Sign() == 0 {
		return false, nil
	}
	if version == PoolV1 {
		return liabilityBase.Cmp(collateralBase) >= 0, nil
	}
	return liabilityBase.Cmp(collateralBase) > 0, nil
}

var hf4Scale = big.NewInt(10_000)

// HFFloor4 renders collateral/liability to 4 decimals, rounded down. ok is false when the account
// has no debt, which has no health factor; never decide liquidation on this string.
func HFFloor4(collateralBase, liabilityBase *big.Int) (hf string, ok bool) {
	if liabilityBase.Sign() == 0 {
		return "", false
	}
	q := new(big.Int).Mul(collateralBase, hf4Scale)
	q.Div(q, liabilityBase)
	whole, frac := new(big.Int).QuoRem(q, hf4Scale, new(big.Int))
	return fmt.Sprintf("%d.%04d", whole, frac), true
}

// InBand reports whether collateral/liability < num/den, compared exactly. An account with no
// debt is below no bound. den must be positive.
func InBand(collateralBase, liabilityBase, num, den *big.Int) bool {
	if den.Sign() <= 0 {
		panic("blend: InBand bound with non-positive denominator")
	}
	if liabilityBase.Sign() == 0 {
		return false
	}
	lhs := new(big.Int).Mul(collateralBase, den)
	rhs := new(big.Int).Mul(liabilityBase, num)
	return lhs.Cmp(rhs) < 0
}

// PriceMove is the direction of a single-asset price shock.
type PriceMove int

// Price shock directions.
const (
	PriceFall PriceMove = iota
	PriceRise
)

// ShockThreshold returns the fraction s at which moving asset X's price by s (down for PriceFall,
// up for PriceRise, all else fixed) makes collateral equal liability, i.e. HF exactly 1. cx, lx
// are the account's effective collateral and liability in X and co, lo the rest, all in the
// oracle base. nil means no move in that direction crosses HF = 1. A result <= 0 means the
// account is already at or below 1; a fall is bounded by 1 (a price of zero).
func ShockThreshold(cx, lx, co, lo *big.Int, move PriceMove) *big.Rat {
	netX := new(big.Int).Sub(cx, lx) // exposure to X's price
	netO := new(big.Int).Sub(lo, co) // shortfall of everything else
	if netX.Sign() == 0 {
		return nil
	}
	// HF(m) = 1 at price multiplier m* = netO / netX; it must be a reachable, non-negative price.
	m := new(big.Rat).SetFrac(netO, netX)
	if m.Sign() < 0 {
		return nil
	}
	one := big.NewRat(1, 1)
	switch move {
	case PriceFall:
		if netX.Sign() < 0 {
			return nil
		}
		return new(big.Rat).Sub(one, m)
	case PriceRise:
		if netX.Sign() > 0 {
			return nil
		}
		return new(big.Rat).Sub(m, one)
	}
	return nil
}
