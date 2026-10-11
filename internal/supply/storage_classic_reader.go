package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ClassicSupplyStore is the storage primitive set the [StorageClassicSupplyReader] consumes. Each sum
// is a non-nil *big.Int, removal-aware, at or before the ledger.
type ClassicSupplyStore interface {
	SumTrustlineBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error)
	SumClaimableBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error)
	SumLPReservesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error)
	SumSACBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error)

	// SACBalanceObservationsExist reports whether any sac_balance_observations row exists at-or-before
	// asOfLedger, regardless of value: Sum* COALESCEs "no rows" to zero, so this feeds
	// [ClassicSupplyComponents.SACObserved].
	SACBalanceObservationsExist(ctx context.Context, assetKey string, asOfLedger uint32) (bool, error)

	TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error)
	SACBalanceForContractAtOrBefore(ctx context.Context, contractHolder, assetKey string, asOfLedger uint32) (*big.Int, error)

	// MinClassicComponentLedger returns the lowest most-recent observation ledger across the four
	// component tables, for the Refresher's stale-component gate. Zero means no signal; implementations
	// that cannot surface it may return (0, nil) and the gate stays permissive.
	MinClassicComponentLedger(ctx context.Context, assetKey string, asOfLedger uint32) (uint32, error)
}

// StorageClassicSupplyReader satisfies [ClassicSupplyReader] from the four classic-supply hypertables
// (ADR-0022). The issuer's own holding is read as a normal trustline observation (zero if absent);
// [LockedSet] accounts/contracts are looked up per entity and summed in Go (short lists).
type StorageClassicSupplyReader struct {
	store  ClassicSupplyStore
	logger *slog.Logger
}

// ClassicSupplyReaderOptions configures a StorageClassicSupplyReader.
// The Logger rides here (the canonical idiom — a logger is never a
// positional constructor param; see docs/architecture/lexicon.md).
type ClassicSupplyReaderOptions struct {
	// Logger surfaces the fail-permissive MinClassicComponentLedger error
	// path; it is the only thing the reader logs. A nil Logger
	// defaults to [slog.Default], keeping the blast radius small for
	// callers that don't wire one.
	Logger *slog.Logger
}

// NewStorageClassicSupplyReader constructs the reader. A zero
// ClassicSupplyReaderOptions (nil Logger) defaults the logger to
// [slog.Default].
func NewStorageClassicSupplyReader(store ClassicSupplyStore, opts ClassicSupplyReaderOptions) *StorageClassicSupplyReader {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &StorageClassicSupplyReader{store: store, logger: logger}
}

// ClassicSupplyAt implements [ClassicSupplyReader]. Any sub-query failure returns an error with no
// partial Components: a partial sum would silently publish wrong totals.
func (r *StorageClassicSupplyReader) ClassicSupplyAt(ctx context.Context, asset canonical.Asset, locked LockedSet, ledger uint32) (ClassicSupplyComponents, error) {
	if asset.Type != canonical.AssetClassic {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: StorageClassicSupplyReader: %w: got type %q", ErrNotClassic, asset.Type)
	}
	assetKey, err := AssetKey(asset)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: derive asset key: %w", err)
	}

	trustline, err := r.store.SumTrustlineBalancesAtOrBefore(ctx, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: trustline sum for %s: %w", assetKey, err)
	}
	claimable, err := r.store.SumClaimableBalancesAtOrBefore(ctx, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: claimable sum for %s: %w", assetKey, err)
	}
	lpReserve, err := r.store.SumLPReservesAtOrBefore(ctx, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: lp-reserve sum for %s: %w", assetKey, err)
	}
	sacWrapped, err := r.store.SumSACBalancesAtOrBefore(ctx, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: sac sum for %s: %w", assetKey, err)
	}
	sacObserved, err := r.store.SACBalanceObservationsExist(ctx, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: sac observation-existence check for %s: %w", assetKey, err)
	}

	issuerBalance, err := r.store.TrustlineBalanceForAccountAtOrBefore(ctx, asset.Issuer, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: issuer trustline lookup for %s: %w", assetKey, err)
	}

	lockedAccounts, err := r.sumPerAccountTrustlines(ctx, locked.Accounts, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: locked-accounts sum for %s: %w", assetKey, err)
	}
	lockedContracts, err := r.sumPerContractSAC(ctx, locked.Contracts, assetKey, ledger)
	if err != nil {
		return ClassicSupplyComponents{}, fmt.Errorf("supply: locked-contracts sum for %s: %w", assetKey, err)
	}

	// Per-component freshness for the Refresher's stale-component
	// gate. A storage-side failure here is non-fatal: the gate stays
	// permissive (MinComponentLedger=0) so a transient query error
	// doesn't reject an otherwise acceptable snapshot. Operator's signal is the WARN log line.
	minLedger, err := r.store.MinClassicComponentLedger(ctx, assetKey, ledger)
	if err != nil {
		// Don't bail — the snapshot is still correct, just
		// without freshness metadata. WARN so the operator sees the
		// stale-component gate silently drop to permissive
		// (MinComponentLedger=0) instead of it happening invisibly.
		r.logger.Warn("classic supply: MinClassicComponentLedger query failed; "+
			"stale-component freshness gate is permissive for this tick",
			"asset_key", assetKey, "ledger", ledger, "err", err)
		minLedger = 0
	}

	return ClassicSupplyComponents{
		Trustline:              trustline,
		Claimable:              claimable,
		LPReserve:              lpReserve,
		SACWrapped:             sacWrapped,
		SACObserved:            sacObserved,
		IssuerBalance:          issuerBalance,
		LockedAccountBalances:  lockedAccounts,
		LockedContractBalances: lockedContracts,
		MinComponentLedger:     minLedger,
	}, nil
}

func (r *StorageClassicSupplyReader) sumPerAccountTrustlines(ctx context.Context, accounts []string, assetKey string, ledger uint32) (*big.Int, error) {
	total := big.NewInt(0)
	for _, acc := range accounts {
		if acc == "" {
			return nil, errors.New("supply: empty account in LockedSet.Accounts")
		}
		bal, err := r.store.TrustlineBalanceForAccountAtOrBefore(ctx, acc, assetKey, ledger)
		if err != nil {
			return nil, fmt.Errorf("trustline lookup for %s: %w", acc, err)
		}
		total = new(big.Int).Add(total, bal)
	}
	return total, nil
}

func (r *StorageClassicSupplyReader) sumPerContractSAC(ctx context.Context, contracts []string, assetKey string, ledger uint32) (*big.Int, error) {
	total := big.NewInt(0)
	for _, c := range contracts {
		if c == "" {
			return nil, errors.New("supply: empty contract in LockedSet.Contracts")
		}
		bal, err := r.store.SACBalanceForContractAtOrBefore(ctx, c, assetKey, ledger)
		if err != nil {
			return nil, fmt.Errorf("sac lookup for %s: %w", c, err)
		}
		total = new(big.Int).Add(total, bal)
	}
	return total, nil
}

// Compile-time check.
var _ ClassicSupplyReader = (*StorageClassicSupplyReader)(nil)

// AssetBoundClassicComputer adapts a [ClassicComputer] to [SnapshotComputer] by baking in a fixed
// [canonical.Asset], so the aggregator can run one [Refresher] per watched classic asset.
type AssetBoundClassicComputer struct {
	inner *ClassicComputer
	asset canonical.Asset
}

// NewAssetBoundClassicComputer constructs the per-asset wrapper.
// Errors when the asset isn't a classic credit asset (the
// [ClassicComputer] checks too at Compute time, but failing fast
// at construction is friendlier).
func NewAssetBoundClassicComputer(inner *ClassicComputer, asset canonical.Asset) (*AssetBoundClassicComputer, error) {
	if asset.Type != canonical.AssetClassic {
		return nil, fmt.Errorf("%w: got type %q", ErrNotClassic, asset.Type)
	}
	if inner == nil {
		return nil, errors.New("supply: NewAssetBoundClassicComputer: inner is nil")
	}
	return &AssetBoundClassicComputer{inner: inner, asset: asset}, nil
}

// Compute implements [SnapshotComputer] by delegating to the
// wrapped [ClassicComputer.Compute] with the bound asset.
func (a *AssetBoundClassicComputer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	return a.inner.Compute(ctx, a.asset, ledger, observedAt)
}

// Compile-time check.
var _ SnapshotComputer = (*AssetBoundClassicComputer)(nil)
