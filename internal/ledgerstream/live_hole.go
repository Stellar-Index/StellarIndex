package ledgerstream

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// liveHoleCheckAfter is how long an unbounded walk may wait on one ledger before the store is asked whether that
// ledger is a hole. At the tip a ledger arrives every few seconds; a variable only so tests can shorten it.
var liveHoleCheckAfter = 2 * time.Minute

// holeWatch ends an unbounded walk that is waiting on a ledger the store will never deliver. The SDK's fetch worker
// retries a missing object forever on an unbounded range, because at the tip "missing" means "not written yet". Only
// the store's latest ledger tells that from a hole, so a walk waiting on a hole would otherwise stall with no error.
// A nil *holeWatch is a bounded walk: its methods do nothing.
type holeWatch struct {
	store  datastore.DataStore
	schema datastore.DataStoreSchema
	// since is when the walk began waiting on seq (unix nanos); 0 while the callback runs, so a slow callback is
	// never mistaken for a stalled read.
	since atomic.Int64
	seq   atomic.Uint32
}

func (h *holeWatch) begin(seq uint32) {
	if h == nil {
		return
	}
	h.seq.Store(seq)
	h.since.Store(time.Now().UnixNano())
}

func (h *holeWatch) end() {
	if h != nil {
		h.since.Store(0)
	}
}

// run cancels the walk with the hole as its cause; it returns when ctx ends.
func (h *holeWatch) run(ctx context.Context, cancel context.CancelCauseFunc) {
	tick := time.NewTicker(liveHoleCheckAfter / 2)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		since := h.since.Load()
		if since == 0 || time.Since(time.Unix(0, since)) < liveHoleCheckAfter {
			continue
		}
		if err := h.hole(ctx, h.seq.Load()); err != nil {
			cancel(err)
			return
		}
	}
}

// hole returns an error only on positive evidence: seq's object is absent while the store holds a later ledger. A
// store that cannot be read proves nothing and is left to the read deadline and the retry budget.
func (h *holeWatch) hole(ctx context.Context, seq uint32) error {
	key := h.schema.GetObjectKeyFromSequenceNumber(seq)
	if !h.absent(ctx, key) {
		return nil
	}
	tip, ok := h.tip(ctx)
	// The object may have landed between the two reads above, so ask again.
	if !ok || tip <= seq || !h.absent(ctx, key) {
		return nil
	}
	return fmt.Errorf("ledger %d is absent from the datastore, whose latest ledger is %d: a hole below the tip, not a ledger still to be written", seq, tip)
}

// absent reports that the store answered and holds no object at key; an unreadable store is not an absent object.
func (h *holeWatch) absent(ctx context.Context, key string) bool {
	exists, err := h.store.Exists(ctx, key)
	return err == nil && !exists
}

// tip is the store's latest ledger; ok is false when it could not be read.
func (h *holeWatch) tip(ctx context.Context) (uint32, bool) {
	tip, err := datastore.FindLatestLedgerSequence(ctx, h.store)
	return tip, err == nil
}
