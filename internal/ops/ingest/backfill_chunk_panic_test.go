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

// TestRunChunkGuardedRecoversPanic pins NS27: a panic inside one parallel
// backfill chunk must not propagate past runChunkGuarded (which would crash
// the whole process, per Go's cross-goroutine panic semantics). It must
// instead be turned into an error on errCh and wg.Done must still fire.
func TestRunChunkGuardedRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chunk := chunkRange{from: 100, to: 200}

	func() {
		// A real goroutine's panic would crash the test binary before this
		// deferred assertion ever ran; calling runChunkGuarded directly
		// (recover() works in any deferring frame, not only a `go` one)
		// still proves the guard, without taking the whole process down if
		// the fix regresses.
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

// TestRunChunkGuardedPropagatesRunError confirms the guard's non-panic path
// is unchanged: an ordinary error from run() still reaches errCh, and no
// panic is fabricated.
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
