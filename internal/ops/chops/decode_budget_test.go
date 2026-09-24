package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A re-derive that soft-skipped undecodable records must not exit 0 unless
// the operator accepted that many skips: an unattended run would otherwise
// report success while rows are missing from the target.

func TestChParticipantBackfillDecodeErrorsFailTheRun(t *testing.T) {
	orig := backfillOperationParticipants
	t.Cleanup(func() { backfillOperationParticipants = orig })
	backfillOperationParticipants = func(context.Context, string, uint32, uint32, uint32, bool, func(string, ...any)) (clickhouse.ParticipantBackfillStats, error) {
		return clickhouse.ParticipantBackfillStats{OpsScanned: 10, Participants: 4, DecodeErrors: 3}, nil
	}
	stubLakeContiguousThrough(t, 10)
	base := []string{"-from", "2", "-to", "10"}

	err := chParticipantBackfill(base)
	if err == nil || !strings.Contains(err.Error(), "3 record(s) skipped") {
		t.Fatalf("3 decode errors under the default budget: err = %v, want a non-nil error naming 3 skipped records", err)
	}
	if err := chParticipantBackfill(append(base, "-max-decode-errors", "3")); err != nil {
		t.Fatalf("3 decode errors within -max-decode-errors 3: err = %v, want nil", err)
	}
	if err := chParticipantBackfill(append(base, "-max-decode-errors", "2")); err == nil {
		t.Fatal("3 decode errors over -max-decode-errors 2: err = nil, want non-nil")
	}
}

func TestChCap67MovementsDecodeErrorsFailTheOneShotRun(t *testing.T) {
	orig := cap67CatchUpOnce
	t.Cleanup(func() { cap67CatchUpOnce = orig })
	cap67CatchUpOnce = func(context.Context, string, uint32, uint32, uint32, bool, uint32) (cap67CatchUp, error) {
		return cap67CatchUp{start: 100, last: 200, rows: 5, skipped: 2}, nil
	}
	base := []string{"-from", "100", "-to", "200"}

	err := chCap67Movements(base)
	if err == nil || !strings.Contains(err.Error(), "2 record(s) skipped") {
		t.Fatalf("2 skipped transfer events under the default budget: err = %v, want a non-nil error naming 2 skipped records", err)
	}
	if err := chCap67Movements(append(base, "-max-decode-errors", "2")); err != nil {
		t.Fatalf("2 skips within -max-decode-errors 2: err = %v, want nil", err)
	}
}

func TestDecodeBudgetCleanRunPasses(t *testing.T) {
	if err := enforceDecodeBudget("x", 0, 0); err != nil {
		t.Fatalf("zero skips: err = %v, want nil", err)
	}
}
