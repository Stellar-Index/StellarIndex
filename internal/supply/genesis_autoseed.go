package supply

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// GenesisSeeder writes one contract's pre-Soroban genesis baseline. It must be
// idempotent (the baseline is SET, not added).
type GenesisSeeder func(ctx context.Context, contractID string) error

// GenesisSeedingComputer wraps a SEP-41 [SnapshotComputer] so a watched
// contract whose genesis baseline is missing seeds itself instead of staying
// withheld until an operator runs `supply seed-sep41-genesis`.
type GenesisSeedingComputer struct {
	inner      SnapshotComputer
	contractID string
	seed       GenesisSeeder
	retryAfter time.Duration
	logger     *slog.Logger

	mu          sync.Mutex
	lastFailure time.Time
}

// GenesisSeedingOptions configures [NewGenesisSeedingComputer].
type GenesisSeedingOptions struct {
	Seed GenesisSeeder
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
	return &GenesisSeedingComputer{inner: inner, contractID: contractID, seed: opts.Seed, retryAfter: opts.RetryAfter, logger: logger}
}

// Compute implements [SnapshotComputer]. On a missing-baseline error it seeds
// once and recomputes; any other outcome, and a failed seed, return the
// inner result unchanged so the refresher still classes it benign.
func (g *GenesisSeedingComputer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
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
