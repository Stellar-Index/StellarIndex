package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeHomeDomainLookup fails a chosen call index (0-based, by call order)
// and otherwise resolves every account to the same fixed domain.
type fakeHomeDomainLookup struct {
	failOnCall int
	calls      int
}

func (f *fakeHomeDomainLookup) AccountHomeDomains(_ context.Context, accounts []string) (map[string]string, error) {
	idx := f.calls
	f.calls++
	if idx == f.failOnCall {
		return nil, errors.New("lake unavailable")
	}
	out := make(map[string]string, len(accounts))
	for _, a := range accounts {
		out[a] = "example.com"
	}
	return out, nil
}

type fakeHomeDomainWriter struct {
	written map[string]string
}

func (f *fakeHomeDomainWriter) SyncIssuerHomeDomain(_ context.Context, gStrkey, homeDomain string) (bool, error) {
	if f.written == nil {
		f.written = map[string]string{}
	}
	f.written[gStrkey] = homeDomain
	return true, nil
}

// TestIssuerEnrichLoop_ContinuesPastBatchFailure proves that a single
// batch's lookup failure no longer aborts every batch behind it: with 3
// batches of 10 ids and the middle batch's lookup failing, the loop must
// still attempt (and count) all 3 batches, only skipping the one that
// failed.
func TestIssuerEnrichLoop_ContinuesPastBatchFailure(t *testing.T) {
	ids := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		ids = append(ids, fmt.Sprintf("GISSUER%02d", i))
	}
	lookup := &fakeHomeDomainLookup{failOnCall: 1} // the second batch fails
	writer := &fakeHomeDomainWriter{}

	found, updated, failedBatches := issuerEnrichLoop(context.Background(), lookup, writer, ids, 10, false)

	if lookup.calls != 3 {
		t.Fatalf("lookup was called %d time(s), want 3 — the batch after the failing one must still be attempted", lookup.calls)
	}
	if failedBatches != 1 {
		t.Fatalf("failedBatches = %d, want 1", failedBatches)
	}
	// Batches 0 and 2 (20 ids) resolve and write cleanly; batch 1's 10 ids
	// are skipped entirely because the lookup itself failed.
	if found != 20 {
		t.Fatalf("found = %d, want 20", found)
	}
	if updated != 20 {
		t.Fatalf("updated = %d, want 20", updated)
	}
	if len(writer.written) != 20 {
		t.Fatalf("writer wrote %d row(s), want 20", len(writer.written))
	}
}
