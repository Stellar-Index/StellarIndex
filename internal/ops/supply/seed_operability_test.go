package supply

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestScopeSACWrappers — GH #714: -contracts narrows the pass (and so the
// provenance it touches) to the named wrappers; a typo is an error, never an
// empty pass, and no flag means every configured wrapper.
func TestScopeSACWrappers(t *testing.T) {
	configured := map[string]string{"CPHO": "PHO:G1", "CBLND": "BLND:G2", "CKALE": "KALE:G3"}

	got, err := scopeSACWrappers(configured, " CPHO, CKALE ,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["CPHO"] != "PHO:G1" || got["CKALE"] != "KALE:G3" {
		t.Errorf("scoped = %v, want CPHO and CKALE only", got)
	}
	if all, err := scopeSACWrappers(configured, ""); err != nil || len(all) != 3 {
		t.Errorf("no -contracts: got %v, %v; want every configured wrapper", all, err)
	}
	if _, err := scopeSACWrappers(configured, "CPHO,CTYPO"); err == nil || !strings.Contains(err.Error(), "CTYPO") {
		t.Errorf("unknown contract: err = %v, want it named", err)
	}
	if _, err := scopeSACWrappers(nil, "CPHO"); err == nil {
		t.Error("no configured wrappers: err = nil, want nothing-to-seed")
	}
	if err := supplySeedSACBalances([]string{"-contracts", "CPHO", "-heartbeat", "/nonexistent/x.prom"}); err == nil || err.Error() != "-config is required" {
		t.Errorf("err = %v, want -contracts and -heartbeat to parse and stop at the missing -config", err)
	}
}

// fakeClaimableStore fails every batch, and single-row writes for ids in bad.
type fakeClaimableStore struct {
	bad     map[string]bool
	written []string
}

func (f *fakeClaimableStore) InsertClaimableObservationBatch(context.Context, []timescale.ClaimableObservation) error {
	return errors.New("batch rejected")
}

func (f *fakeClaimableStore) InsertClaimableObservation(_ context.Context, o timescale.ClaimableObservation) error {
	if f.bad[o.ClaimableID] {
		return errors.New("row rejected")
	}
	f.written = append(f.written, o.ClaimableID)
	return nil
}

// TestClaimableSeedWriterRowFallback — GH #714: a failed batch no longer
// aborts an hours-long pass. Its rows are retried one by one, the good ones
// land, and the bad one is named in a non-zero exit.
func TestClaimableSeedWriterRowFallback(t *testing.T) {
	store := &fakeClaimableStore{bad: map[string]bool{"b": true}}
	w := &claimableSeedWriter{ctx: context.Background(), store: store, tallies: map[string]*claimableSeedTally{}}
	for _, id := range []string{"a", "b", "c"} {
		if err := w.add(clickhouseSeed(id)); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatalf("flush = %v, want the batch failure absorbed by the per-row retry", err)
	}
	if strings.Join(store.written, ",") != "a,c" {
		t.Errorf("written = %v, want a and c", store.written)
	}
	err := w.failedErr()
	if err == nil || !strings.Contains(err.Error(), "1 claimable observation row(s)") || !strings.Contains(err.Error(), "b at ledger") {
		t.Errorf("failedErr = %v, want the one failed row named", err)
	}
}

// TestClaimableSeedWriterCancelledBatchStops — a batch that failed because the
// run's deadline expired is returned, not retried row by row against a dead
// context.
func TestClaimableSeedWriterCancelledBatchStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeClaimableStore{}
	w := &claimableSeedWriter{ctx: ctx, store: store, tallies: map[string]*claimableSeedTally{}}
	if err := w.add(clickhouseSeed("a")); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(); err == nil || len(store.written) != 0 {
		t.Errorf("flush = %v with %d row(s) retried, want the batch error and no retry", err, len(store.written))
	}
}

// TestSeedProgress — the heartbeat total advances with every reduced window
// and every emitted row, so the long silent walk is not read as a hang.
func TestSeedProgress(t *testing.T) {
	p := &seedProgress{}
	p.window(100, 349)
	p.window(350, 599)
	p.row()
	if p.total != 501 || p.cursor != 599 {
		t.Errorf("total=%d cursor=%d, want 501 (500 ledgers + 1 row) and 599", p.total, p.cursor)
	}
	var none *seedProgress
	none.window(1, 2)
	none.row()
}

func clickhouseSeed(id string) clickhouse.ClaimableBalanceSeed {
	return clickhouse.ClaimableBalanceSeed{ClaimableID: id, AssetKey: "AQUA:" + seedClaimableIssuer, Balance: big.NewInt(1), LedgerSeq: 40_000_000}
}

func TestUpsertClaimableSeedProvenanceNeedsLakeEvidence(t *testing.T) {
	w := &claimableSeedWriter{tallies: map[string]*claimableSeedTally{"AQUA:G1": {count: 1, sum: big.NewInt(1)}}}
	// lakeVerifiedThrough is zero: the store must refuse before touching the DB.
	if err := w.stampProvenance(context.Background(), &timescale.Store{}); err == nil || !strings.Contains(err.Error(), "LakeVerifiedThrough") {
		t.Errorf("stampProvenance = %v, want an unverified pass refused", err)
	}
}
