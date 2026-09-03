package v1

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingAssetsUpstream is fakeAssetsUpstream with a ListAssetsExt that
// parks until released, so a test can prove a call did NOT reach
// upstream synchronously (the whole point of the boot seed: an ~11 s
// cold aggregate must not sit on a user request).
type blockingAssetsUpstream struct {
	fakeAssetsUpstream
	release chan struct{}
	started chan struct{}
	calls   atomic.Int64
}

func newBlockingAssetsUpstream() *blockingAssetsUpstream {
	return &blockingAssetsUpstream{
		release: make(chan struct{}),
		started: make(chan struct{}, 8),
	}
}

func (b *blockingAssetsUpstream) ListAssetsExt(
	ctx context.Context, _ timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	b.calls.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []timescale.AssetRow{{AssetID: "FRESH-GAAA", Slug: "fresh"}}, nil
}

// seedTestOpts is a deliberately awkward options value: every keyed
// dimension is populated and distinct, so a key expression that drops
// one, reorders two, or confuses Issuer with Code cannot accidentally
// still match.
var seedTestOpts = timescale.ListAssetsOptions{
	Limit:  51,
	Issuer: "GISSUER",
	Code:   "GCODE",
	Cursor: "0:12:USDC-GAAA",
	Q:      "qq",
	Order:  timescale.AssetsOrderVolume24hUSDDesc,
}

// TestListAssetsCacheKeyMatchesListAssetsExt is the drift guard the
// boot seed depends on.
//
// listAssetsCacheKey duplicates the key expression inside
// [CachedAssetsReader.ListAssetsExt] because the seed has to address
// the slot the HANDLER will look up. If the two ever diverge — a field
// added to ListAssetsOptions and keyed on one side only, a reordered
// segment, a swapped Issuer/Code — the seed writes a phantom slot: the
// cache reports healthy, the snapshot is read and installed every boot,
// and every caller still pays the ~11 s cold fill. Nothing at runtime
// would say so.
//
// So the real key is not asserted against a literal; it is OBSERVED.
// ListAssetsExt is driven once and the entry it actually created is
// read straight out of the map.
func TestListAssetsCacheKeyMatchesListAssetsExt(t *testing.T) {
	c := NewCachedAssetsReader(&fakeAssetsUpstream{}, time.Minute)
	if _, err := c.ListAssetsExt(context.Background(), seedTestOpts); err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 1 {
		t.Fatalf("ListAssetsExt created %d entries, want exactly 1", len(c.entries))
	}
	want := listAssetsCacheKey(seedTestOpts)
	for got := range c.entries {
		if got != want {
			t.Fatalf("cache-key drift: ListAssetsExt stored under\n  %q\nbut listAssetsCacheKey (the seed's key) builds\n  %q\n"+
				"— the boot seed would write a slot no request looks up", got, want)
		}
	}
}

// TestSeedListingServesStaleWithoutBlocking is the #459 fix itself.
//
// Un-seeded, a cold key takes fetchRows branch (C) and BLOCKS on the
// upstream aggregate — measured at 11,658 ms on r1's 2026-09-03 boot.
// Seeded, the same call must take branch (A'): return the seeded rows
// immediately and refresh behind the response.
//
// The upstream here never returns until released, so a fix that still
// blocks cannot pass: the test would deadlock on the read rather than
// merely report a wrong value.
func TestSeedListingServesStaleWithoutBlocking(t *testing.T) {
	up := newBlockingAssetsUpstream()
	c := NewCachedAssetsReader(up, time.Minute)

	observedAt := time.Now().Add(-5 * time.Minute) // older than the TTL
	seeded := []timescale.AssetRow{{AssetID: "SEEDED-GAAA", Slug: "seeded"}}
	if !c.SeedListing(seedTestOpts, seeded, observedAt) {
		t.Fatal("SeedListing refused a well-formed seed")
	}

	done := make(chan struct{})
	var (
		rows  []timescale.AssetRow
		at    time.Time
		stale bool
		err   error
	)
	go func() {
		defer close(done)
		rows, at, stale, err = c.ListAssetsExtAt(context.Background(), seedTestOpts)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ListAssetsExtAt blocked on the upstream fill — the seed did not take the stale-serve branch")
	}

	if err != nil {
		t.Fatalf("ListAssetsExtAt: %v", err)
	}
	if len(rows) != 1 || rows[0].AssetID != "SEEDED-GAAA" {
		t.Fatalf("served %+v, want the seeded row-set", rows)
	}
	if !stale {
		t.Error("stale = false; a page-set observed 5 minutes ago under a 1-minute TTL is stale by construction")
	}
	if !at.Equal(observedAt) {
		t.Errorf("observedAt = %v, want the seed's real observation time %v — as_of must not be re-dated by the seed",
			at, observedAt)
	}

	// Exactly one detached refresh, and it is the seeded entry that
	// gets replaced once it lands.
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no background refresh was started behind the stale serve")
	}
	close(up.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		fresh, freshAt, freshStale, ferr := c.ListAssetsExtAt(context.Background(), seedTestOpts)
		if ferr != nil {
			t.Fatalf("ListAssetsExtAt after refresh: %v", ferr)
		}
		if len(fresh) == 1 && fresh[0].AssetID == "FRESH-GAAA" {
			if freshStale {
				t.Error("stale = true after a completed refresh")
			}
			if !freshAt.After(observedAt) {
				t.Errorf("observedAt = %v did not advance past the seed's %v after a real refresh", freshAt, observedAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never replaced the seeded rows")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := up.calls.Load(); got != 1 {
		t.Errorf("upstream called %d times, want exactly 1 (single-flighted refresh)", got)
	}
}

// TestSeedListingRefusals pins every case where seeding must decline
// and leave the pre-#459 cold-fill behaviour untouched. Each of these
// would otherwise publish something the cache could not label
// honestly — or, worse, an empty listing where the real one would have
// been served.
func TestSeedListingRefusals(t *testing.T) {
	rows := []timescale.AssetRow{{AssetID: "SEEDED-GAAA"}}
	past := time.Now().Add(-time.Minute)

	t.Run("empty row-set", func(t *testing.T) {
		c := NewCachedAssetsReader(&fakeAssetsUpstream{}, time.Minute)
		if c.SeedListing(seedTestOpts, nil, past) {
			t.Fatal("seeded an empty page-set — a boot seed must never displace a real listing with nothing")
		}
	})
	t.Run("undated", func(t *testing.T) {
		c := NewCachedAssetsReader(&fakeAssetsUpstream{}, time.Minute)
		if c.SeedListing(seedTestOpts, rows, time.Time{}) {
			t.Fatal("seeded an undated row-set — it could not be labelled stale or given an honest as_of")
		}
	})
	t.Run("future-dated", func(t *testing.T) {
		c := NewCachedAssetsReader(&fakeAssetsUpstream{}, time.Minute)
		if c.SeedListing(seedTestOpts, rows, time.Now().Add(time.Hour)) {
			t.Fatal("seeded a future-dated row-set — it would read as fresh indefinitely")
		}
	})
	t.Run("cache disabled", func(t *testing.T) {
		c := NewCachedAssetsReader(&fakeAssetsUpstream{}, 0)
		if c.SeedListing(seedTestOpts, rows, past) {
			t.Fatal("seeded a disabled cache")
		}
	})
	t.Run("never clobbers an existing entry", func(t *testing.T) {
		c := NewCachedAssetsReader(&fakeAssetsUpstream{}, time.Minute)
		if _, err := c.ListAssetsExt(context.Background(), seedTestOpts); err != nil {
			t.Fatalf("ListAssetsExt: %v", err)
		}
		if c.SeedListing(seedTestOpts, rows, past) {
			t.Fatal("seed overwrote a live cache entry")
		}
		got, _, _, err := c.ListAssetsExtAt(context.Background(), seedTestOpts)
		if err != nil {
			t.Fatalf("ListAssetsExtAt: %v", err)
		}
		if len(got) != 1 || got[0].AssetID != "native" {
			t.Fatalf("served %+v, want the live entry's rows", got)
		}
	})
}

// TestListAssetsExtAtUncachedReportsFresh — with the cache disabled the
// read is live, which is exactly the "as_of = now, not stale" case the
// …At contract defines (mirrors CachedMarketsReader.DistinctPairsExtAt).
func TestListAssetsExtAtUncachedReportsFresh(t *testing.T) {
	c := NewCachedAssetsReader(&fakeAssetsUpstream{}, 0)
	rows, at, stale, err := c.ListAssetsExtAt(context.Background(), seedTestOpts)
	if err != nil {
		t.Fatalf("ListAssetsExtAt: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if !at.IsZero() || stale {
		t.Fatalf("at=%v stale=%v, want (zero, false) for an uncached live read", at, stale)
	}
}
