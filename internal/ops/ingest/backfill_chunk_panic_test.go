package ingest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestRunChunkGuardedRecoversPanic pins that a chunk panic becomes an error on errCh and wg.Done still fires.
func TestRunChunkGuardedRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chunk := chunkRange{from: 100, to: 200}

	func() {
		// Call runChunkGuarded directly: a real goroutine panic would crash the test binary before the assertion ran.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped runChunkGuarded: %v", r)
			}
		}()
		runChunkGuarded(logger, "backfill-chunk-0", 0, chunk, errCh, &wg, func() error {
			panic("boom: simulated decoder panic")
		})
	}()

	wg.Wait()
	close(errCh)

	var got []error
	for e := range errCh {
		got = append(got, e)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one reported error, got %d: %v", len(got), got)
	}
	wantSubstr := fmt.Sprintf("chunk %d [%d, %d]:", 0, chunk.from, chunk.to)
	if !strings.Contains(got[0].Error(), wantSubstr) {
		t.Fatalf("error %q missing expected prefix %q", got[0], wantSubstr)
	}
	if !strings.Contains(got[0].Error(), "boom: simulated decoder panic") {
		t.Fatalf("error %q does not carry the panic value", got[0])
	}
}

// TestRunChunkGuardedPropagatesRunError confirms an ordinary run() error reaches errCh with no panic fabricated.
func TestRunChunkGuardedPropagatesRunError(t *testing.T) {
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chunk := chunkRange{from: 1, to: 2}
	wantErr := errors.New("ordinary chunk failure")

	runChunkGuarded(logger, "backfill-chunk-1", 1, chunk, errCh, &wg, func() error {
		return wantErr
	})

	wg.Wait()
	close(errCh)

	var got []error
	for e := range errCh {
		got = append(got, e)
	}
	if len(got) != 1 || !errors.Is(got[0], wantErr) {
		t.Fatalf("expected wrapped wantErr, got %v", got)
	}
}
