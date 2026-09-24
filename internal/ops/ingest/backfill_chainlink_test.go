package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// failingOracleStore rejects the rounds whose TxHash is in fail.
type failingOracleStore struct {
	fail    map[string]bool
	written []string
}

func (s *failingOracleStore) InsertOracleUpdate(_ context.Context, u canonical.OracleUpdate) error {
	if s.fail[u.TxHash] {
		return errors.New("statement_timeout")
	}
	s.written = append(s.written, u.TxHash)
	return nil
}

func runChainlinkDrain(t *testing.T, store *failingOracleStore, walk error, dryRun bool, txs ...string) error {
	t.Helper()
	updates := make(chan canonical.OracleUpdate, len(txs))
	for _, tx := range txs {
		updates <- canonical.OracleUpdate{Source: "chainlink", TxHash: tx}
	}
	close(updates)
	walkErr := make(chan error, 1)
	walkErr <- walk
	return drainChainlinkUpdates(context.Background(), updates, walkErr, store, dryRun, 0, time.Now())
}

// TestDrainChainlinkUpdates_DroppedRoundFailsTheRun: a round the writer
// rejects is counted and logged, but the run must not exit 0 on a clean
// walk — the command is idempotent by tx hash, so a zero exit over dropped
// rounds is never re-run (#1185).
func TestDrainChainlinkUpdates_DroppedRoundFailsTheRun(t *testing.T) {
	t.Parallel()
	store := &failingOracleStore{fail: map[string]bool{"b": true}}
	err := runChainlinkDrain(t, store, nil, false, "a", "b", "c")
	if err == nil {
		t.Fatal("drainChainlinkUpdates returned nil with 1 of 3 rounds dropped; the run would exit 0")
	}
	if !strings.Contains(err.Error(), "1 of 3 oracle update(s) failed to insert") {
		t.Fatalf("err = %v, want the dropped-row accounting", err)
	}
	if got := strings.Join(store.written, ","); got != "a,c" {
		t.Fatalf("written = %q, want the good rounds a,c still written", got)
	}
}

// TestDrainChainlinkUpdates_KeepsWalkError: the venue walk's own error must
// survive alongside the row accounting.
func TestDrainChainlinkUpdates_KeepsWalkError(t *testing.T) {
	t.Parallel()
	walk := errors.New("eth_getLogs: 429")
	err := runChainlinkDrain(t, &failingOracleStore{fail: map[string]bool{"a": true}}, walk, false, "a")
	if !errors.Is(err, walk) || !strings.Contains(err.Error(), "failed to insert") {
		t.Fatalf("err = %v, want both the walk error and the dropped-row accounting", err)
	}
}

func TestDrainChainlinkUpdates_CleanAndDryRunAreNil(t *testing.T) {
	t.Parallel()
	if err := runChainlinkDrain(t, &failingOracleStore{}, nil, false, "a", "b"); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	// Dry-run never touches the store; a store that rejects everything proves it.
	if err := runChainlinkDrain(t, &failingOracleStore{fail: map[string]bool{"a": true}}, nil, true, "a"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}
