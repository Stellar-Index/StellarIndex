package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ConfigReserveBalanceReader is a [ReserveBalanceReader] backed by a
// static operator-supplied balance map. The supply-snapshot writer
// uses it as the bootstrap fallback in the chained-reader pattern
// (see docs/architecture/supply-pipeline.md §"The chained-fallback
// reader pattern"): the live [LCMReserveBalanceReader] takes
// precedence when every watched account has an observation, and
// this reader fills the gap when the AccountEntry observer hasn't
// backfilled yet (or, transiently, on storage error).
//
// Operator usage: populate
// `[supply] reserve_balances_stroops = { "G..." = "12345..." }` in
// the operator config. The writer constructs one of these from that
// map and passes it into the chained reader; once the observer has
// covered every account in `sdf_reserve_accounts`, the static map
// is no longer consulted.
//
// Limitations (as a fallback):
//
//   - Static map — no automatic balance refresh. The map carries its
//     own as-of date and the reader refuses to answer once that date
//     is more than maxAge old (or was never set), so a forgotten
//     snapshot fails closed instead of being re-stamped at every new
//     ledger as the current reserve.
//   - No per-account ledger versioning. The reader returns whatever
//     the config says for the requested account regardless of the
//     `ledger` argument. The live [LCMReserveBalanceReader] is the
//     ledger-aware path; this fallback is intentionally
//     ledger-agnostic since its purpose is bring-up only.
//
// Its answers are tagged [ReserveSourceStatic], which the
// [XLMComputer] publishes as [BasisXLMSDFReserveExclusionStatic] with
// no freshness anchor.
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

// NewConfigReserveBalanceReader constructs a reader from a balance
// map. Stroop values are decimal strings (NUMERIC-safe per
// ADR-0003) parsed at construction so a malformed entry fails fast
// at startup rather than mid-snapshot.
//
// Empty input is valid — yields a reader that returns zero for any
// account list (equivalent to "no reserves to exclude"). The
// XLMComputer treats a zero-account input as a configuration where
// the operator hasn't enumerated reserves yet; circulating equals
// total.
//
// asOf is when the balances were taken. A zero asOf is accepted here,
// so an undated map does not stop the process booting, but every
// non-empty read then refuses. maxAge must be positive.
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

// ReserveBalanceTotal sums the configured balances for the supplied
// account list. Missing accounts return an error — silently treating
// an unknown account as zero would yield an over-stated circulating
// supply, which is exactly the failure mode ADR-0011 says we don't
// publish. A non-empty request against an undated or over-age map
// returns [ErrStaticReserveSnapshotExpired].
//
// The `ledger` argument is currently unused; see type-level docstring
// for why.
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
