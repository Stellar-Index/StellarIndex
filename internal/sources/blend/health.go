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
	ErrStalePrice         = errors.New("blend: oracle price older than 24 h")
	ErrPriceBaseMismatch  = errors.New("blend: oracle prices do not share one base")
	ErrInvalidInput       = errors.New("blend: invalid health input")
)

// oracleMaxAge is pool.rs::load_price's staleness bound, in seconds.
const oracleMaxAge = 24 * 60 * 60

// Positions is an account's Positions entry keyed by reserve asset (b-tokens and d-tokens).
type Positions struct {
	Collateral  map[string]*big.Int
	Liabilities map[string]*big.Int
	Supply      map[string]*big.Int // non-collateral supply; never counts toward health
}

// HealthReserve is the state of one reserve that health reads: its stored rates (with last_time)
// and its config (c_factor, l_factor and decimals).
type HealthReserve struct {
	Data   *ReserveData
	Config *ReserveConfig
}

// OraclePrice is a raw SEP-40 lastprice value with the oracle's decimals and its timestamp.
type OraclePrice struct {
	Price     *big.Int
	Decimals  uint32
	Timestamp uint64
}

// Health is an account's position data in the oracle's base.
type Health struct {
	CollateralBase *big.Int // effective: c_factor applied, rounded down
	LiabilityBase  *big.Int // effective: l_factor applied, rounded up
	CollateralRaw  *big.Int // collateral before c_factor, rounded down
	// AccrualPendingSeconds is the largest now − last_time over the reserves valued. The contract
	// accrues interest to now before valuing, so stored rates understate debt by that much time:
	// callers must publish accounts near the boundary separately until accrual is reproduced.
	AccrualPendingSeconds uint64
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

// AccountHealth returns the account's position data in the oracle's base at ledger close time
// now, summed per reserve as floor(price·collateral/scalar) and ceil(price·liability/scalar).
// Non-collateral supply never counts. Any missing or stale input for a held reserve is an error
// naming it.
func AccountHealth(pos Positions, reserves map[string]HealthReserve, prices map[string]OraclePrice, version PoolVersion, now uint64) (Health, error) {
	if version != PoolV1 && version != PoolV2 {
		return Health{}, fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	h := Health{CollateralBase: new(big.Int), LiabilityBase: new(big.Int), CollateralRaw: new(big.Int)}
	var base *uint32
	for _, asset := range heldAssets(pos) {
		b, err := balance(pos.Collateral, asset)
		if err != nil {
			return Health{}, fmt.Errorf("collateral %s: %w", asset, err)
		}
		d, err := balance(pos.Liabilities, asset)
		if err != nil {
			return Health{}, fmt.Errorf("liability %s: %w", asset, err)
		}
		// health_factor.rs skips a reserve holding neither, without loading it or its price.
		if b.Sign() == 0 && d.Sign() == 0 {
			continue
		}
		res, price, err := healthInputs(asset, reserves, prices, now)
		if err != nil {
			return Health{}, err
		}
		if base == nil {
			base = &price.Decimals
		} else if *base != price.Decimals {
			return Health{}, fmt.Errorf("%w: %s has %d decimals, another reserve %d", ErrPriceBaseMismatch, asset, price.Decimals, *base)
		}
		if err := h.addReserve(res, price.Price, version, b, d); err != nil {
			return Health{}, fmt.Errorf("%s: %w", asset, err)
		}
		if pending := now - res.Data.LastTime; pending > h.AccrualPendingSeconds {
			h.AccrualPendingSeconds = pending
		}
	}
	return h, nil
}

// addReserve values one reserve's collateral (floor) and liability (ceil) at price/10^decimals.
func (h *Health) addReserve(res HealthReserve, price *big.Int, version PoolVersion, b, d *big.Int) error {
	scalar := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(res.Config.Decimals)), nil)
	if b.Sign() > 0 {
		eff, err := EffectiveCollateral(*res.Data, *res.Config, version, b)
		if err != nil {
			return fmt.Errorf("collateral: %w", err)
		}
		h.CollateralBase.Add(h.CollateralBase, fixedMulFloor(price, eff, scalar))
		h.CollateralRaw.Add(h.CollateralRaw, fixedMulFloor(price, res.Data.BTokensToUnderlying(b), scalar))
	}
	if d.Sign() > 0 {
		eff, err := EffectiveLiability(*res.Data, *res.Config, version, d)
		if err != nil {
			return fmt.Errorf("liability: %w", err)
		}
		h.LiabilityBase.Add(h.LiabilityBase, fixedMulCeil(price, eff, scalar))
	}
	return nil
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

func healthInputs(asset string, reserves map[string]HealthReserve, prices map[string]OraclePrice, now uint64) (HealthReserve, OraclePrice, error) {
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
	if res.Data.LastTime > now {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s last_time %d is after now %d", ErrInvalidInput, asset, res.Data.LastTime, now)
	}
	price, ok := prices[asset]
	if !ok || price.Price == nil {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s", ErrMissingPrice, asset)
	}
	// pool.rs::load_price: timestamp + 86400 < now is refused, so exactly 24 h old is accepted.
	if price.Timestamp+oracleMaxAge < now {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s price at %d, now %d", ErrStalePrice, asset, price.Timestamp, now)
	}
	// V2 load_price refuses price <= 0; V1 does not check, and we refuse rather than value on it.
	if price.Price.Sign() <= 0 {
		return HealthReserve{}, OraclePrice{}, fmt.Errorf("%w: %s price %s is not positive", ErrInvalidInput, asset, price.Price)
	}
	return res, price, nil
}

// Status is what the pool contract would do with a user liquidation of the account.
type Status int

// Account statuses.
const (
	StatusNoDebt       Status = iota // no liabilities: no health factor
	StatusHealthy                    // the liquidation gate refuses the auction
	StatusLiquidatable               // the gate admits a user-liquidation auction
	StatusBadDebt                    // liabilities with zero raw collateral: no lot to auction
)

// Classify applies new_auction / new_liquidation_auction's health gate: V2 requires
// liability_base > collateral_base, V1 only that liability_base is not below collateral_base.
// Zero raw collateral is bad debt, never liquidatable: V2 refuses an empty lot (InvalidLot) and
// V1's sizing divides by collateral_raw. Before publishing StatusLiquidatable the caller must also
// check that no user-liquidation auction is in progress for the account (AuctionInProgress) and
// that the account is not the pool contract or its backstop (InvalidLiquidation); AccountHealth
// has already refused stale prices. See docs/protocols/blend.md "Thresholds".
func Classify(version PoolVersion, h Health) (Status, error) {
	if version != PoolV1 && version != PoolV2 {
		return 0, fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	if h.LiabilityBase.Sign() == 0 {
		return StatusNoDebt, nil
	}
	if h.CollateralRaw.Sign() == 0 {
		return StatusBadDebt, nil
	}
	if belowGate(version, h.CollateralBase, h.LiabilityBase) {
		return StatusLiquidatable, nil
	}
	return StatusHealthy, nil
}

// belowGate is the version's liquidation comparison for an account with debt.
func belowGate(version PoolVersion, c, l *big.Int) bool {
	if version == PoolV1 {
		return l.Cmp(c) >= 0
	}
	return l.Cmp(c) > 0
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

// ShockOutcome classifies a ShockResult.
type ShockOutcome int

// Shock outcomes.
const (
	ShockNeverCrosses        ShockOutcome = iota // no move in this direction reaches the gate
	ShockCrosses                                 // S is the move that brings HF to exactly 1
	ShockAlreadyLiquidatable                     // the gate already admits a liquidation today
)

// ShockResult is a ShockThreshold answer; S is set only for ShockCrosses.
type ShockResult struct {
	Outcome ShockOutcome
	S       *big.Rat
}

// ShockThreshold returns the fraction s at which moving asset X's price by s (down for PriceFall,
// up for PriceRise, all else fixed) makes collateral equal liability, i.e. HF exactly 1. cx, lx
// are the account's effective collateral and liability in X and co, lo the rest, all in the
// oracle base. An account the version's gate already admits is ShockAlreadyLiquidatable. A fall
// is bounded by s = 1 (a price of zero).
func ShockThreshold(version PoolVersion, cx, lx, co, lo *big.Int, move PriceMove) (ShockResult, error) {
	if version != PoolV1 && version != PoolV2 {
		return ShockResult{}, fmt.Errorf("%w %d", ErrUnknownPoolVersion, version)
	}
	c, l := new(big.Int).Add(co, cx), new(big.Int).Add(lo, lx)
	if l.Sign() == 0 {
		return ShockResult{Outcome: ShockNeverCrosses}, nil
	}
	if belowGate(version, c, l) {
		return ShockResult{Outcome: ShockAlreadyLiquidatable}, nil
	}
	netX := new(big.Int).Sub(cx, lx) // exposure to X's price
	netO := new(big.Int).Sub(lo, co) // shortfall of everything else
	harmful := (move == PriceFall && netX.Sign() > 0) || (move == PriceRise && netX.Sign() < 0)
	if !harmful {
		return ShockResult{Outcome: ShockNeverCrosses}, nil
	}
	// HF = 1 at price multiplier m* = netO / netX; a negative m* is no reachable price.
	m := new(big.Rat).SetFrac(netO, netX)
	if m.Sign() < 0 {
		return ShockResult{Outcome: ShockNeverCrosses}, nil
	}
	if move == PriceFall {
		return ShockResult{Outcome: ShockCrosses, S: new(big.Rat).Sub(big.NewRat(1, 1), m)}, nil
	}
	return ShockResult{Outcome: ShockCrosses, S: new(big.Rat).Sub(m, big.NewRat(1, 1))}, nil
}
