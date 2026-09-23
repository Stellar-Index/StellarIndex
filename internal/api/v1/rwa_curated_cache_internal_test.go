package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingRWACuratedReader stands in for the curated arm's storage read:
// it records the exact context it was handed, so a test can tell
// whether that context was already canceled when the read observed it.
type blockingRWACuratedReader struct {
	rows        map[string]timescale.CuratedRWAEntry
	sawCanceled bool
}

func (b *blockingRWACuratedReader) CuratedRWADirectoryByAddress(ctx context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	b.sawCanceled = ctx.Err() != nil
	return b.rows, timescale.CuratedRWACensus{Entries: len(b.rows), Priced: len(b.rows)}, nil
}

func (b *blockingRWACuratedReader) LatestCuratedPublished(context.Context, string) (*timescale.CuratedRWAPublished, error) {
	return nil, nil
}

func rwaCuratedCacheTestServer(reader RWACuratedDirectoryReader) *Server {
	return &Server{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		rwaCurated: reader,
	}
}

// The curated snapshot is a process-wide cache shared by every request
// inside its ten-minute TTL. The request that happens to trigger a
// refresh must not be able to poison that shared cache for everyone
// else by disconnecting mid-read — the read has to run on a context
// detached from the caller's, the same way the membership cache's
// rebuild does.
func TestRWACuratedSnapshot_ReadDoesNotInheritCallerCancellation(t *testing.T) {
	reader := &blockingRWACuratedReader{rows: map[string]timescale.CuratedRWAEntry{
		"CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4": {
			Address: "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4",
		},
	}}
	s := rwaCuratedCacheTestServer(reader)

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already given up before the refresh runs

	snap := s.rwaCuratedSnapshot(canceled)

	if reader.sawCanceled {
		t.Fatal("the curated read inherited the caller's canceled context: " +
			"one client disconnecting must not poison the shared cache")
	}
	if !snap.available {
		t.Fatalf("snapshot = %+v, want an available snapshot despite the canceled caller", snap)
	}
}

// slowRWACuratedReader takes a measurable, synchronous amount of time to
// answer, so a test can tell whether the TTL was stamped before or
// after that time elapsed.
type slowRWACuratedReader struct {
	delay time.Duration
	rows  map[string]timescale.CuratedRWAEntry
}

func (r *slowRWACuratedReader) CuratedRWADirectoryByAddress(_ context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	time.Sleep(r.delay)
	return r.rows, timescale.CuratedRWACensus{Entries: len(r.rows), Priced: len(r.rows)}, nil
}

func (r *slowRWACuratedReader) LatestCuratedPublished(context.Context, string) (*timescale.CuratedRWAPublished, error) {
	return nil, nil
}

// The TTL must be stamped only once the read has actually returned. A
// snapshot cached BEFORE the read completes can claim a freshness
// window for data it never obtained: the delay a slow read takes must
// show up in the stamp, not be stamped over the instant the read began.
func TestRWACuratedSnapshot_StampsTTLAfterTheReadCompletes(t *testing.T) {
	const delay = 100 * time.Millisecond
	reader := &slowRWACuratedReader{
		delay: delay,
		rows: map[string]timescale.CuratedRWAEntry{
			"CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4": {
				Address: "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4",
			},
		},
	}
	s := rwaCuratedCacheTestServer(reader)

	before := time.Now()
	s.rwaCuratedSnapshot(context.Background())

	s.rwaCuratedSnap.mu.Lock()
	stamped := s.rwaCuratedSnap.at
	s.rwaCuratedSnap.mu.Unlock()

	if stamped.Sub(before) < delay/2 {
		t.Fatalf("TTL stamped %v after the call started, want at least ~%v (the read's own delay): "+
			"it was stamped before the read completed", stamped.Sub(before), delay)
	}
}

// hangingRWACuratedReader never answers on its own: it blocks until the
// context it was handed is done.
type hangingRWACuratedReader struct{}

func (hangingRWACuratedReader) CuratedRWADirectoryByAddress(ctx context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	<-ctx.Done()
	return nil, timescale.CuratedRWACensus{}, ctx.Err()
}

func (hangingRWACuratedReader) LatestCuratedPublished(ctx context.Context, _ string) (*timescale.CuratedRWAPublished, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A hung storage read must not hold the cache mutex that every /rwa
// request queues on: the detached read runs under its own budget and
// the refresh returns, failed closed, once that budget is spent.
func TestRWACuratedSnapshot_HungReadReturnsWithinBudget(t *testing.T) {
	const budget = 50 * time.Millisecond
	s := rwaCuratedCacheTestServer(hangingRWACuratedReader{})

	done := make(chan rwaCurated, 1)
	go func() { done <- s.rwaCuratedSnapshotWithin(context.Background(), budget) }()

	select {
	case snap := <-done:
		if snap.available || snap.published != nil {
			t.Fatalf("snapshot = %+v, want an unavailable snapshot after the read timed out", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh still blocked 2s after a 50ms budget: the detached read has no deadline")
	}
}

// deadlineRecordingReader answers at once, recording the deadline of
// the context each read was handed.
type deadlineRecordingReader struct{ deadlines []time.Time }

func (d *deadlineRecordingReader) record(ctx context.Context) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Time{}
	}
	d.deadlines = append(d.deadlines, dl)
}

func (d *deadlineRecordingReader) CuratedRWADirectoryByAddress(ctx context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	d.record(ctx)
	return nil, timescale.CuratedRWACensus{}, nil
}

func (d *deadlineRecordingReader) LatestCuratedPublished(ctx context.Context, _ string) (*timescale.CuratedRWAPublished, error) {
	d.record(ctx)
	return nil, nil
}

// The production entry point hands both reads a deadline no later than
// [rwaCuratedReadBudget] after the call, even from a caller with none.
func TestRWACuratedSnapshot_ReadCarriesTheReadBudget(t *testing.T) {
	reader := &deadlineRecordingReader{}
	s := rwaCuratedCacheTestServer(reader)

	s.rwaCuratedSnapshot(context.Background())
	end := time.Now()

	if len(reader.deadlines) != 2 {
		t.Fatalf("reads = %d, want 2", len(reader.deadlines))
	}
	for i, dl := range reader.deadlines {
		if dl.IsZero() {
			t.Fatalf("read %d ran with no deadline", i)
		}
		if dl.After(end.Add(rwaCuratedReadBudget)) {
			t.Fatalf("read %d deadline %v, want no later than %v after the call", i, dl, rwaCuratedReadBudget)
		}
	}
}
