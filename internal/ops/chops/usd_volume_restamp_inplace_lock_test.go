package chops

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// An in-place -write restamp refuses to start beside another -write run,
// exactly as a chunk run does, and releases the lock when it finishes.
func TestHoldInPlaceRestampLock(t *testing.T) {
	ctx := context.Background()

	held := newFakeChunkStore(nil)
	held.lockHeld = true
	if _, err := holdInPlaceRestampLock(ctx, held, true, false, io.Discard); !errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) {
		t.Fatalf("in-place -write beside a live run: err = %v, want ErrUSDVolumeRestampLockHeld", err)
	}

	free := newFakeChunkStore(nil)
	finish, err := holdInPlaceRestampLock(ctx, free, true, false, io.Discard)
	if err != nil {
		t.Fatalf("in-place -write: %v", err)
	}
	if free.index("lock") < 0 || free.index("unlock") >= 0 {
		t.Fatalf("lock not held for the walk:\n%s", strings.Join(free.log, "\n"))
	}
	runErr := errors.New("walk failed")
	if got := finish(runErr); !errors.Is(got, runErr) {
		t.Fatalf("finish dropped the run error: %v", got)
	}
	if free.index("unlock") < 0 {
		t.Fatalf("finish did not release the lock:\n%s", strings.Join(free.log, "\n"))
	}

	for _, tc := range []struct {
		name          string
		write, chunks bool
	}{{"dry run", false, false}, {"chunk run (locks in beginChunkWriteRun)", true, true}} {
		s := newFakeChunkStore(nil)
		s.lockHeld = true
		finish, err := holdInPlaceRestampLock(ctx, s, tc.write, tc.chunks, io.Discard)
		if err != nil || finish(nil) != nil || len(s.log) != 0 {
			t.Fatalf("%s: err = %v, store log = %v; want no lock taken", tc.name, err, s.log)
		}
	}
}
