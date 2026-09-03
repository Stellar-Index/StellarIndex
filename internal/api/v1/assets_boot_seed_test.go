package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetList_BootSeededPageIsLabelledStale is the honesty half of
// #459.
//
// A boot-seeded page-set is real data, but it was observed by a
// PREVIOUS process and is stale by construction. Serving it is the
// fix; serving it under `flags.stale: false` with `as_of` stamped to
// the moment of the request would be a new lie — precisely the one
// /v1/markets was corrected for (markets.go: "never now() over rows a
// failing refresh has let age past the TTL").
//
// The assertions are on the CORRECTED VALUES: stale must be true, and
// as_of must equal the seed's real observation time to the second, not
// merely be non-zero.
func TestAssetList_BootSeededPageIsLabelledStale(t *testing.T) {
	const ttl = time.Minute
	reader := v1.NewCachedAssetsReader(&paginatingAssetsReader{total: 1000}, ttl)

	// The key the handler will look up for `?limit=2`: it overfetches
	// by one, so Limit is 3. Built through the same SeedListing seam
	// the boot path uses.
	observedAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	opts := timescale.ListAssetsOptions{Limit: 3}
	seeded := []timescale.AssetRow{
		{AssetID: "SEEDED1-GAAA", Slug: "seeded1", Code: "SEEDED1", ObservationCount: 9},
		{AssetID: "SEEDED2-GAAA", Slug: "seeded2", Code: "SEEDED2", ObservationCount: 8},
	}
	if !reader.SeedListing(opts, seeded, observedAt) {
		t.Fatal("SeedListing refused the boot seed")
	}

	srv := v1.New(v1.Options{AssetsReader: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  []v1.AssetDetail `json:"data"`
		AsOf  time.Time        `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 || env.Data[0].AssetID != "SEEDED1-GAAA" {
		t.Fatalf("data = %+v, want the seeded rows served straight from the boot seed", env.Data)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false over a page-set observed 30 minutes ago — a boot seed must be labelled, not passed off as fresh")
	}
	if !env.AsOf.Equal(observedAt) {
		t.Errorf("as_of = %s, want the served rows' real observation time %s",
			env.AsOf.Format(time.RFC3339Nano), observedAt.Format(time.RFC3339Nano))
	}
}

// TestAssetList_FreshCachedPageIsNotLabelledStale is the other side of
// the same contract: the honest label must discriminate. A page-set
// inside the TTL is not stale, and stamping `stale: true` on every
// cached response would make the flag worthless.
func TestAssetList_FreshCachedPageIsNotLabelledStale(t *testing.T) {
	reader := v1.NewCachedAssetsReader(&paginatingAssetsReader{total: 1000}, time.Minute)
	observedAt := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Second)
	if !reader.SeedListing(timescale.ListAssetsOptions{Limit: 3}, []timescale.AssetRow{
		{AssetID: "SEEDED1-GAAA", Slug: "seeded1", Code: "SEEDED1"},
	}, observedAt) {
		t.Fatal("SeedListing refused the seed")
	}

	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: reader}))
	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	var env struct {
		AsOf  time.Time `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if env.Flags.Stale {
		t.Error("flags.stale = true over a page-set observed 2 seconds ago under a 1-minute TTL")
	}
	if !env.AsOf.Equal(observedAt) {
		t.Errorf("as_of = %s, want the served rows' observation time %s",
			env.AsOf.Format(time.RFC3339Nano), observedAt.Format(time.RFC3339Nano))
	}
}

// TestAssetList_UncachedReaderStillFresh — the degraded path. A wired
// reader with no cache (every test double in this package, and any
// deployment with the listing cache off) answers live, so it must not
// acquire a stale flag or a back-dated as_of from this change.
func TestAssetList_UncachedReaderStillFresh(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 10}}))
	before := time.Now().UTC().Add(-time.Second)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	var env struct {
		Data  []v1.AssetDetail `json:"data"`
		AsOf  time.Time        `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 {
		t.Fatalf("data = %+v", env.Data)
	}
	if env.Flags.Stale {
		t.Error("flags.stale = true on a live uncached read")
	}
	if env.AsOf.Before(before) {
		t.Errorf("as_of = %s is back-dated; a live read stamps now", env.AsOf.Format(time.RFC3339Nano))
	}
}
