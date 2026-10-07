package ingest

import (
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// rederive-from.sh treats exit 0 as "counts reseeded", so a run with no
// stated mode must fail rather than preview and succeed.
func TestSeedEntryCounts_RequiresStatedMode(t *testing.T) {
	if err := seedEntryCounts([]string{"-config", "/nonexistent.toml"}); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("no mode = %v, want ErrWriteModeUnstated", err)
	}
	if err := seedEntryCounts([]string{"-config", "/nonexistent.toml", "-write"}); err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("-write = %v, want it past the mode check to the config load", err)
	}
}
