package ledgerstream

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestTiered_GetFile_BothMissing_ErrorNamesBothTiers is the RLT-282
// regression guard for the tiered-datastore site of the "a short walk
// reads as full coverage" class.
//
// both_missing is the data-integrity PAGE condition: neither tier holds
// the object, so the reader is stalled on a genuine hole. Every other
// error path in this file carries "tiered:" context; this one returned
// the COLD store's error verbatim, which is byte-identical to the
// routine hot miss the fallback exists to absorb and names only the
// tier that was consulted second. That matters because of what happens
// to the error next: the SDK wraps it as "ledger object containing
// sequence N is missing", and on any bounded ops walk
// TolerateTrailingMissing converts it into a clean walk-complete. The
// walk then reports a short range as a whole one, and the only surviving
// trace that BOTH tiers were asked is a counter nobody reads after the
// fact.
//
// The wrap must not change any control flow — IsNotFound and the SDK's
// own ledger_buffer both branch on errors.Is(err, os.ErrNotExist) to
// decide between retrying an unbounded range and aborting a bounded one
// — so this asserts the identity survives as well as the wording.
func TestTiered_GetFile_BothMissing_ErrorNamesBothTiers(t *testing.T) {
	t.Parallel()
	hot := newFakeStore("hot")
	cold := newFakeStore("cold")

	const path = "FFFFFFFF--0-63/0000000032.xdr.zstd"
	ts := NewTieredDataStore(hot, cold)
	_, _, err := ts.GetFile(context.Background(), path)
	if err == nil {
		t.Fatal("GetFile with the object absent from both tiers returned nil")
	}

	// Identity first: the wrap is only safe if it is transparent.
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false — the fallback chain and the SDK's retry/abort "+
			"branch both key off this", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(%v, os.ErrNotExist) = false — the underlying not-found must "+
			"remain unwrappable", err)
	}

	msg := err.Error()
	if !strings.Contains(msg, "tiered:") {
		t.Errorf("both-missing error %q carries no tiered: context, unlike every other "+
			"error path in this file", msg)
	}
	if !strings.Contains(msg, "BOTH") {
		t.Errorf("both-missing error %q does not say both tiers were consulted and both "+
			"missed — it reads as the ordinary hot miss the cold tier exists to absorb", msg)
	}
	if !strings.Contains(msg, path) {
		t.Errorf("both-missing error %q does not name the object %q", msg, path)
	}

	// And it must stay distinguishable from its two neighbours: a cold
	// HIT is not an error at all, and a cold TRANSIENT failure is an
	// error that is NOT a not-found.
	coldHit := newFakeStore("cold-hit")
	coldHit.files[path] = "COLD-BODY"
	if _, _, herr := NewTieredDataStore(newFakeStore("hot"), coldHit).GetFile(context.Background(), path); herr != nil {
		t.Errorf("hot-miss/cold-hit returned %v, want nil", herr)
	}

	flaky := newFakeStore("cold-flaky")
	flaky.getErr[path] = errors.New("dial tcp: i/o timeout")
	_, _, terr := NewTieredDataStore(newFakeStore("hot"), flaky).GetFile(context.Background(), path)
	if terr == nil {
		t.Fatal("a transient cold failure returned nil")
	}
	if IsNotFound(terr) {
		t.Errorf("a transient cold failure (%v) must not read as not-found — that would "+
			"turn a reachability problem into a tolerated hole", terr)
	}
}
