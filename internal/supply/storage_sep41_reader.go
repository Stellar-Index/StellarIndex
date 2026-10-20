package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SEP41SupplyStore is the storage primitive set of [StorageSEP41SupplyReader]; SEP-41 contracts share the SAC `DataKey::Balance(Address) -> i128` shape, and a SAC's rows sit under its classic CODE:ISSUER key.
type SEP41SupplyStore interface {
	SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (SEP41KindTotals, error)
	SACBalanceForContractAtOrBefore(ctx context.Context, contractHolder, assetKey string, asOfLedger uint32) (*big.Int, error)
	TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error)

	// MinSEP41ComponentLedger returns MAX(ledger) across sep41_supply_events for a contract with
	// events at or before asOfLedger: the producer-wide watermark for the stale-component gate (a
	// quiet contract is not stale, a stalled producer is). 0 means no events yet; (0, nil) keeps
	// the gate permissive.
	MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error)

	// SEP41GenesisBaselineSeeded reports whether a pre-Soroban genesis baseline is seeded (migration
	// 0088), so a not-yet-seeded SAC wrapper's negative total is the benign `missing_baseline`
	// outcome rather than a paging `compute_error`. (false, nil) means not seeded.
	SEP41GenesisBaselineSeeded(ctx context.Context, contractID string) (bool, error)
}

// SEP41KindTotals mirrors the timescale.SEP41KindTotals shape without importing timescale (which imports supply); the aggregator adapter projects between them.
type SEP41KindTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// StorageSEP41SupplyReader satisfies [SEP41SupplyReader] from sep41_supply_events totals plus per-holder SAC-balance lookups (ADR-0023); AdminBalance is always 0 (set_admin is untracked), so do NOT "fix" it with a placeholder.
type StorageSEP41SupplyReader struct {
	store SEP41SupplyStore
	// sacClassicKeys is [SACClassicKeys] of the observer's sac_wrappers.
	sacClassicKeys map[string]string
}

// NewStorageSEP41SupplyReader constructs the reader; sacClassicKeys is [SACClassicKeys] of the observer's sac_wrappers, nil meaning no watched contract is a SAC.
func NewStorageSEP41SupplyReader(store SEP41SupplyStore, sacClassicKeys map[string]string) *StorageSEP41SupplyReader {
	return &StorageSEP41SupplyReader{store: store, sacClassicKeys: sacClassicKeys}
}

// SEP41SupplyAt implements [SEP41SupplyReader]: kind totals plus per-holder balances for the locked set, with AdminBalance 0 (see [StorageSEP41SupplyReader]).
func (r *StorageSEP41SupplyReader) SEP41SupplyAt(ctx context.Context, asset canonical.Asset, locked LockedSet, ledger uint32) (SEP41SupplyComponents, error) {
	if asset.Type != canonical.AssetSoroban {
		return SEP41SupplyComponents{}, fmt.Errorf("supply: StorageSEP41SupplyReader: %w: got type %q", ErrNotSoroban, asset.Type)
	}
	contractID := asset.ContractID
	if contractID == "" {
		return SEP41SupplyComponents{}, errors.New("supply: StorageSEP41SupplyReader: empty ContractID")
	}

	totals, err := r.store.SEP41KindTotalsAtOrBefore(ctx, contractID, ledger)
	if err != nil {
		return SEP41SupplyComponents{}, fmt.Errorf("supply: SEP41 kind totals for %s: %w", contractID, err)
	}

	lockedAccounts, err := r.sumPerHolder(ctx, locked.Accounts, contractID, ledger)
	if err != nil {
		return SEP41SupplyComponents{}, fmt.Errorf("supply: locked-accounts sum for %s: %w", contractID, err)
	}
	lockedContracts, err := r.sumPerHolder(ctx, locked.Contracts, contractID, ledger)
	if err != nil {
		return SEP41SupplyComponents{}, fmt.Errorf("supply: locked-contracts sum for %s: %w", contractID, err)
	}

	// Per-component freshness. Non-fatal on query error: stamp zero,
	// which keeps the gate permissive.
	minLedger, err := r.store.MinSEP41ComponentLedger(ctx, contractID, ledger)
	if err != nil {
		minLedger = 0
	}

	// Migration 0088 genesis-baseline flag. Non-fatal on error: treated as not-seeded, so a negative total routes to the
	// benign `missing_baseline` outcome.
	genesisSeeded, err := r.store.SEP41GenesisBaselineSeeded(ctx, contractID)
	if err != nil {
		genesisSeeded = false
	}

	return SEP41SupplyComponents{
		MintTotal:              totals.Mint,
		BurnTotal:              totals.Burn,
		ClawbackTotal:          totals.Clawback,
		AdminBalance:           big.NewInt(0),
		LockedAccountBalances:  lockedAccounts,
		LockedContractBalances: lockedContracts,
		MinComponentLedger:     minLedger,
		GenesisBaselineSeeded:  genesisSeeded,
	}, nil
}

func (r *StorageSEP41SupplyReader) sumPerHolder(ctx context.Context, holders []string, contractID string, ledger uint32) (*big.Int, error) {
	total := big.NewInt(0)
	for _, h := range holders {
		if h == "" {
			return nil, errors.New("supply: empty holder in LockedSet")
		}
		bal, err := r.holderBalance(ctx, h, contractID, ledger)
		if err != nil {
			return nil, err
		}
		total = new(big.Int).Add(total, bal)
	}
	return total, nil
}

// holderBalance reads holder's balance where the observers wrote it: a pure SEP-41 contract keeps
// all balances under its contract id; a SAC keeps only contract holders there, under its classic
// key, and a G holder's balance is a trustline. Config validation guarantees each holder is observed.
func (r *StorageSEP41SupplyReader) holderBalance(ctx context.Context, holder, contractID string, ledger uint32) (*big.Int, error) {
	classicKey, isSAC := r.sacClassicKeys[contractID]
	if !isSAC {
		bal, err := r.store.SACBalanceForContractAtOrBefore(ctx, holder, contractID, ledger)
		if err != nil {
			return nil, fmt.Errorf("sac lookup for %s: %w", holder, err)
		}
		return bal, nil
	}
	if canonical.IsAccountID(holder) {
		bal, err := r.store.TrustlineBalanceForAccountAtOrBefore(ctx, holder, classicKey, ledger)
		if err != nil {
			return nil, fmt.Errorf("trustline lookup for %s: %w", holder, err)
		}
		return bal, nil
	}
	bal, err := r.store.SACBalanceForContractAtOrBefore(ctx, holder, classicKey, ledger)
	if err != nil {
		return nil, fmt.Errorf("sac lookup for %s: %w", holder, err)
	}
	return bal, nil
}

// Compile-time check.
var _ SEP41SupplyReader = (*StorageSEP41SupplyReader)(nil)

// AssetBoundSEP41Computer adapts a [SEP41Computer] to [SnapshotComputer] with a fixed [canonical.Asset], one per watched SEP-41 contract.
type AssetBoundSEP41Computer struct {
	inner *SEP41Computer
	asset canonical.Asset
}

// NewAssetBoundSEP41Computer constructs the per-contract wrapper, failing fast if the asset isn't a Soroban contract.
func NewAssetBoundSEP41Computer(inner *SEP41Computer, asset canonical.Asset) (*AssetBoundSEP41Computer, error) {
	if asset.Type != canonical.AssetSoroban {
		return nil, fmt.Errorf("%w: got type %q", ErrNotSoroban, asset.Type)
	}
	if inner == nil {
		return nil, errors.New("supply: NewAssetBoundSEP41Computer: inner is nil")
	}
	return &AssetBoundSEP41Computer{inner: inner, asset: asset}, nil
}

// Compute implements [SnapshotComputer] by delegating to [SEP41Computer.Compute] with the bound asset.
func (a *AssetBoundSEP41Computer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	return a.inner.Compute(ctx, a.asset, ledger, observedAt)
}

var _ SnapshotComputer = (*AssetBoundSEP41Computer)(nil)
