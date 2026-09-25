package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingAssetListingReader stands in for the listing directory's
// storage read: it records the exact context it was handed, so a test
// can tell whether that context was already canceled when the read
// observed it.
type blockingAssetListingReader struct {
	rows        map[string]timescale.ListingEntry
	sawCanceled bool
}

func (b *blockingAssetListingReader) ListingDirectoryByAddress(ctx context.Context) (
	map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error,
) {
	b.sawCanceled = ctx.Err() != nil
	return b.rows, timescale.ListingDirectoryCensus{Entries: len(b.rows)}, nil
}

func assetListingCacheTestServer(reader AssetListingDirectoryReader) *Server {
	return &Server{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		listings: reader,
	}
}

// The listing snapshot is a process-wide cache shared by every request
// inside its 60s TTL. A request that happens to trigger a refresh must
// not be able to poison that cache for everyone else by disconnecting
// mid-read: the read has to run on a context detached from the
// caller's, the same way the curated cache's does (GH-523).
func TestAssetListingSnapshot_ReadDoesNotInheritCallerCancellation(t *testing.T) {
	reader := &blockingAssetListingReader{rows: map[string]timescale.ListingEntry{
		"CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4": {
			Address: "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4",
		},
	}}
	s := assetListingCacheTestServer(reader)

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already given up before the refresh runs

	snap := s.assetListingSnapshot(canceled)

	if reader.sawCanceled {
		t.Fatal("the listing read inherited the caller's canceled context: " +
			"one client disconnecting must not poison the shared cache")
	}
	if !snap.available {
		t.Fatalf("snapshot = %+v, want an available snapshot despite the canceled caller", snap)
	}
}

// hangingAssetListingReader never answers on its own: it blocks until
// the context it was handed is done.
type hangingAssetListingReader struct{}

func (hangingAssetListingReader) ListingDirectoryByAddress(ctx context.Context) (
	map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error,
) {
	<-ctx.Done()
	return nil, timescale.ListingDirectoryCensus{}, ctx.Err()
}

// A hung storage read must not hold the cache mutex that every
// /v1/assets, /v1/assets/{id} and /v1/rwa/assets request queues on: the
// detached read runs under its own budget and the refresh returns,
// failed closed, once that budget is spent — and NOT cached as
// listing_unavailable for the full 60s TTL because the caller happened
// to disconnect.
func TestAssetListingSnapshot_HungReadReturnsWithinBudget(t *testing.T) {
	const budget = 50 * time.Millisecond
	s := assetListingCacheTestServer(hangingAssetListingReader{})

	done := make(chan assetListing, 1)
	go func() { done <- s.assetListingSnapshotWithin(context.Background(), budget) }()

	select {
	case snap := <-done:
		if snap.available {
			t.Fatalf("snapshot = %+v, want an unavailable snapshot after the read timed out", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh still blocked 2s after a 50ms budget: the detached read has no deadline")
	}
}

// deadlineRecordingListingReader answers at once, recording the
// deadline of the context the read was handed.
type deadlineRecordingListingReader struct{ deadlines []time.Time }

func (d *deadlineRecordingListingReader) ListingDirectoryByAddress(ctx context.Context) (
	map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error,
) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Time{}
	}
	d.deadlines = append(d.deadlines, dl)
	return nil, timescale.ListingDirectoryCensus{}, nil
}

// The production entry point hands the read a deadline no later than
// [assetListingReadBudget] after the call, even from a caller with none.
func TestAssetListingSnapshot_ReadCarriesTheReadBudget(t *testing.T) {
	reader := &deadlineRecordingListingReader{}
	s := assetListingCacheTestServer(reader)

	s.assetListingSnapshot(context.Background())
	end := time.Now()

	if len(reader.deadlines) != 1 {
		t.Fatalf("reads = %d, want 1", len(reader.deadlines))
	}
	dl := reader.deadlines[0]
	if dl.IsZero() {
		t.Fatal("read ran with no deadline")
	}
	if dl.After(end.Add(assetListingReadBudget)) {
		t.Fatalf("read deadline %v, want no later than %v after the call", dl, assetListingReadBudget)
	}
}
