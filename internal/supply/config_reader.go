package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ConfigReserveBalanceReader is a [ReserveBalanceReader] over a static operator-supplied
// balance map, the bootstrap fallback behind [LCMReserveBalanceReader]. It refuses to answer
// once asOf is older than maxAge (or unset), so a stale snapshot fails closed; it ignores `ledger`.
type ConfigReserveBalanceReader struct {
	balances map[string]*big.Int
	asOf     time.Time
	maxAge   time.Duration
	now      func() time.Time
}

// ErrStaticReserveSnapshotExpired is returned by
// [ConfigReserveBalanceReader] when the static balance map is undated
// or older than its configured maximum age.
var ErrStaticReserveSnapshotExpired = errors.New("supply: static reserve-balance snapshot is undated or older than its maximum age")

// NewConfigReserveBalanceReader parses decimal-string stroop balances (ADR-0003) at construction
// so a bad entry fails at startup. Empty input is valid (nothing to exclude). A zero asOf boots
// but makes every non-empty read refuse; maxAge must be positive.
func NewConfigReserveBalanceReader(balancesStroops map[string]string, asOf time.Time, maxAge time.Duration) (*ConfigReserveBalanceReader, error) {
	if maxAge <= 0 {
		return nil, fmt.Errorf("supply: ConfigReserveBalanceReader: max age %v must be positive", maxAge)
	}
	parsed := make(map[string]*big.Int, len(balancesStroops))
	for acc, raw := range balancesStroops {
		if acc == "" {
			return nil, fmt.Errorf("supply: ConfigReserveBalanceReader: empty account key in balance map")
		}
		v, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			return nil, fmt.Errorf("supply: ConfigReserveBalanceReader: parse balance for %s: %q is not a decimal integer", acc, raw)
		}
		if v.Sign() < 0 {
			return nil, fmt.Errorf("supply: ConfigReserveBalanceReader: negative balance for %s: %s", acc, v.String())
		}
		parsed[acc] = v
	}
	return &ConfigReserveBalanceReader{balances: parsed, asOf: asOf, maxAge: maxAge, now: time.Now}, nil
}

// ReserveBalanceTotal sums the configured balances. An unknown account is an error: treating it
// as zero would overstate circulating supply (ADR-0011). An undated or over-age map returns
// [ErrStaticReserveSnapshotExpired]. The `ledger` argument is unused.
func (r *ConfigReserveBalanceReader) ReserveBalanceTotal(_ context.Context, accounts []string, _ uint32) (*big.Int, error) {
	if len(accounts) > 0 {
		if err := r.checkAge(); err != nil {
			return nil, err
		}
	}
	total := big.NewInt(0)
	for _, acc := range accounts {
		v, ok := r.balances[acc]
		if !ok {
			return nil, fmt.Errorf("supply: ConfigReserveBalanceReader: no balance configured for account %s", acc)
		}
		total = new(big.Int).Add(total, v)
	}
	return total, nil
}

// ReserveBalanceTotalSourced implements [ReserveBalanceSourcedReader]:
// every answer from this reader is the static snapshot.
func (r *ConfigReserveBalanceReader) ReserveBalanceTotalSourced(ctx context.Context, accounts []string, ledger uint32) (*big.Int, ReserveSource, error) {
	v, err := r.ReserveBalanceTotal(ctx, accounts, ledger)
	if err != nil {
		return nil, "", err
	}
	return v, ReserveSourceStatic, nil
}

func (r *ConfigReserveBalanceReader) checkAge() error {
	if r.asOf.IsZero() {
		return fmt.Errorf("%w: no as-of date configured", ErrStaticReserveSnapshotExpired)
	}
	if age := r.now().Sub(r.asOf); age > r.maxAge {
		return fmt.Errorf("%w: taken %s, %s old, max age %s",
			ErrStaticReserveSnapshotExpired, r.asOf.UTC().Format(time.DateOnly), age.Round(time.Hour), r.maxAge)
	}
	return nil
}
