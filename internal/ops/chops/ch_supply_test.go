package chops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// -seed-flows is ch-supply's only write. The mode is checked before -config
// is even required, so each refusal here is the gate, not a missing flag.
func TestCHSupply_SeedFlowsWriteGate(t *testing.T) {
	t.Parallel()
	if err := chSupply([]string{"-seed-flows"}); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Errorf("bare -seed-flows: err = %v, want ErrWriteModeUnstated (a scripted seed must not silently preview)", err)
	}
	if err := chSupply([]string{"-write"}); err == nil || !strings.Contains(err.Error(), "-write applies -seed-flows") {
		t.Errorf("-write without -seed-flows: err = %v, want a refusal", err)
	}
	for _, args := range [][]string{{"-seed-flows", "-dry-run"}, {"-seed-flows", "-write"}, nil} {
		err := chSupply(args)
		if err == nil || !strings.Contains(err.Error(), "-config, -from, -to are required") {
			t.Errorf("%v: err = %v, want it past the mode check to the required-flag check", args, err)
		}
	}
}

// A dry-run seed counts every row, across a batch boundary, without dialing
// ClickHouse: the address is a closed port, so any write would fail.
func TestSupplyFlowSeeder_DryRunCountsWithoutWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := newSupplyFlowSeeder(ctx, "127.0.0.1:1", true, false)
	if err != nil {
		t.Fatalf("dry-run seeder dialed ClickHouse: %v", err)
	}
	for range supplyFlowBatchN + 1 {
		if err := s.add(ctx, clickhouse.SupplyFlowRow{Kind: "mint"}); err != nil {
			t.Fatalf("dry-run add wrote: %v", err)
		}
	}
	if err := s.finish(ctx); err != nil {
		t.Fatalf("dry-run finish wrote: %v", err)
	}
	if s.seeded != supplyFlowBatchN+1 {
		t.Errorf("seeded = %d, want %d", s.seeded, supplyFlowBatchN+1)
	}
	if _, err := newSupplyFlowSeeder(ctx, "127.0.0.1:1", true, true); err == nil {
		t.Error("-write seeder against a closed port: err = nil, want the ensure-table dial error")
	}
}
