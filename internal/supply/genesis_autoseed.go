package supply

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// GenesisSeeder writes one contract's pre-Soroban genesis baseline and must be idempotent (the baseline is SET, not added).
type GenesisSeeder func(ctx context.Context, contractID string) error

// HistoryCoverageCheck returns nil only when the served tier holds every Soroban-era supply event of contractID up to a settled
// ledger at or below ledger. The event table is filled only from the ledger a contract joined the watch-list.
type HistoryCoverageCheck func(ctx context.Context, contractID string, ledger uint32) error

// ErrSEP41HistoryNotCovered is returned while a watched contract's Soroban-era events are not shown to be in the served tier, so a
// sum over them would be partial; benign (`missing_baseline`) until `stellarindex-ops projector-replay -source sep41_supply` lands.
var ErrSEP41HistoryNotCovered = errors.New("supply: SEP-41 contract's Soroban-era events are not shown to be replayed — run `stellarindex-ops projector-replay -source sep41_supply`")

// GenesisSeedingComputer wraps a SEP-41 [SnapshotComputer] so a watched contract with a missing genesis baseline seeds itself instead of staying withheld.
type GenesisSeedingComputer struct {
	inner      SnapshotComputer
	contractID string
	seed       GenesisSeeder
	covers     HistoryCoverageCheck
	retryAfter time.Duration
	logger     *slog.Logger

	mu          sync.Mutex
	lastFailure time.Time
	covered     bool
	coverErr    error
}

// GenesisSeedingOptions configures [NewGenesisSeedingComputer].
type GenesisSeedingOptions struct {
	Seed GenesisSeeder
	// Covers gates every result, seeded or not, until it has passed once in this process; nil disables the gate.
	Covers HistoryCoverageCheck
	// RetryAfter is the wait after a failed seed, so an unreachable lake is
	// not hit on every refresh tick.
	RetryAfter time.Duration
	Logger     *slog.Logger
}

// NewGenesisSeedingComputer binds inner to contractID.
func NewGenesisSeedingComputer(inner SnapshotComputer, contractID string, opts GenesisSeedingOptions) *GenesisSeedingComputer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GenesisSeedingComputer{inner: inner, contractID: contractID, seed: opts.Seed, covers: opts.Covers, retryAfter: opts.RetryAfter, logger: logger}
}

// Compute implements [SnapshotComputer]: on a missing-baseline error it seeds once and recomputes, otherwise returns the inner result unchanged.
func (g *GenesisSeedingComputer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	if err := g.ensureCovered(ctx, ledger); err != nil {
		return Supply{}, err
	}
	s, err := g.inner.Compute(ctx, ledger, observedAt)
	if !errors.Is(err, ErrGenesisBaselineNotSeeded) && !errors.Is(err, ErrNegativeTotalMissingBaseline) {
		return s, err
	}
	if !g.mayAttempt() {
		return s, err
	}
	if seedErr := g.seed(ctx, g.contractID); seedErr != nil {
		g.recordFailure()
		g.logger.Warn("supply: genesis baseline auto-seed failed; supply stays withheld",
			"contract", g.contractID, "err", seedErr)
		return s, err
	}
	g.logger.Info("supply: genesis baseline auto-seeded", "contract", g.contractID)
	return g.inner.Compute(ctx, ledger, observedAt)
}

// ensureCovered runs the coverage check until it passes once; a contract matched live from then on stays covered.
func (g *GenesisSeedingComputer) ensureCovered(ctx context.Context, ledger uint32) error {
	if g.covers == nil {
		return nil
	}
	g.mu.Lock()
	covered := g.covered
	g.mu.Unlock()
	if covered {
		return nil
	}
	if !g.mayAttempt() {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.coverErr
	}
	// The check itself wraps ErrSEP41HistoryNotCovered for a measured difference; a read that failed stays a compute error so it alerts.
	if err := g.covers(ctx, g.contractID, ledger); err != nil {
		g.recordFailure()
		g.logger.Warn("supply: SEP-41 history coverage not shown; supply stays withheld", "contract", g.contractID, "err", err)
		g.mu.Lock()
		g.coverErr = err
		g.mu.Unlock()
		return err
	}
	g.mu.Lock()
	g.covered = true
	g.coverErr = nil
	g.mu.Unlock()
	return nil
}

func (g *GenesisSeedingComputer) mayAttempt() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastFailure.IsZero() || time.Since(g.lastFailure) >= g.retryAfter
}

func (g *GenesisSeedingComputer) recordFailure() {
	g.mu.Lock()
	g.lastFailure = time.Now()
	g.mu.Unlock()
}
