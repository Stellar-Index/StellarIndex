package supply

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

// seededComputer reports ErrGenesisBaselineNotSeeded until seeded flips.
type seededComputer struct {
	seeded *bool
	calls  int
}

func (c *seededComputer) Compute(_ context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	c.calls++
	if !*c.seeded {
		return Supply{}, ErrGenesisBaselineNotSeeded
	}
	return Supply{LedgerSequence: ledger, ObservedAt: observedAt, TotalSupply: big.NewInt(7)}, nil
}

func TestGenesisSeedingComputer_UnseededBecomesServedAfterOnePass(t *testing.T) {
	seeded := false
	inner := &seededComputer{seeded: &seeded}
	var seedCalls int
	g := NewGenesisSeedingComputer(inner, "CWRAP", GenesisSeedingOptions{Seed: func(_ context.Context, id string) error {
		seedCalls++
		if id != "CWRAP" {
			t.Fatalf("seed contract = %q", id)
		}
		seeded = true
		return nil
	}, RetryAfter: time.Minute, Logger: discardLogger()})

	got, err := g.Compute(context.Background(), 10, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got.TotalSupply.Cmp(big.NewInt(7)) != 0 || seedCalls != 1 {
		t.Fatalf("total=%v seedCalls=%d, want 7 / 1", got.TotalSupply, seedCalls)
	}
	if _, err := g.Compute(context.Background(), 11, time.Unix(2, 0)); err != nil || seedCalls != 1 {
		t.Fatalf("second pass err=%v seedCalls=%d, want nil / 1 (no re-seed)", err, seedCalls)
	}
}

func TestGenesisSeedingComputer_FailedSeedStaysBenignAndBacksOff(t *testing.T) {
	seeded := false
	inner := &seededComputer{seeded: &seeded}
	var seedCalls int
	g := NewGenesisSeedingComputer(inner, "CWRAP", GenesisSeedingOptions{Seed: func(context.Context, string) error {
		seedCalls++
		return errors.New("lake down")
	}, RetryAfter: time.Hour, Logger: discardLogger()})

	for i := 0; i < 3; i++ {
		if _, err := g.Compute(context.Background(), 10, time.Unix(1, 0)); !errors.Is(err, ErrGenesisBaselineNotSeeded) {
			t.Fatalf("pass %d err = %v, want ErrGenesisBaselineNotSeeded", i, err)
		}
	}
	if seedCalls != 1 {
		t.Fatalf("seedCalls = %d, want 1 (backoff)", seedCalls)
	}
}

func TestGenesisSeedingComputer_OtherErrorsDoNotSeed(t *testing.T) {
	g := NewGenesisSeedingComputer(stubComputer{err: ErrNegativeTotalSupply}, "C", GenesisSeedingOptions{Seed: func(context.Context, string) error {
		t.Fatal("seeded on an unrelated error")
		return nil
	}, RetryAfter: time.Minute, Logger: discardLogger()})
	if _, err := g.Compute(context.Background(), 1, time.Unix(1, 0)); !errors.Is(err, ErrNegativeTotalSupply) {
		t.Fatalf("err = %v", err)
	}
}
