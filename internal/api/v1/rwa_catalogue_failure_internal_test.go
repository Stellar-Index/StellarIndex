package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// gatedCatalogueUpstream answers every listing read once release closes, with an error.
type gatedCatalogueUpstream struct {
	AssetsReader
	release chan struct{}
}

func (u gatedCatalogueUpstream) ListAssetsExt(ctx context.Context, _ timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	select {
	case <-u.release:
		return nil, errors.New("catalogue store unavailable")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// awaitCatalogueRefresh waits for the refresh in flight on opts' catalogue entry, if any.
func awaitCatalogueRefresh(t *testing.T, c *CachedAssetsReader, opts timescale.ListAssetsOptions) {
	t.Helper()
	c.mu.Lock()
	var flight chan struct{}
	if e, ok := c.entries[listAssetsCacheKey(opts)]; ok {
		flight = e.flight
	}
	c.mu.Unlock()
	if flight == nil {
		return
	}
	select {
	case <-flight:
	case <-time.After(10 * time.Second):
		t.Fatal("catalogue refresh did not settle")
	}
}

// TestRWAAssetsView_FailedCatalogueRefreshFlagsTheView: the catalogue's stale verdict mixes its 2-minute age with its refresh
// failure, and the view judges age by its own 10-minute bound, so the failure must be read on its own. Rows 3 minutes old
// behind a failed refresh flag the view at build and at serve; the same rows behind a refresh still running are clean.
func TestRWAAssetsView_FailedCatalogueRefreshFlagsTheView(t *testing.T) {
	const issuer = "GISSUER"
	m := rwaMembership{members: []rwaMember{{issuer: issuer, code: "CETES"}}}
	opts := timescale.ListAssetsOptions{Limit: rwaAssetsPerIssuer, Issuer: issuer, Type: "classic"}
	for _, failFirst := range []bool{false, true} {
		up := gatedCatalogueUpstream{release: make(chan struct{})}
		c := NewCachedAssetsReader(up, AssetCatalogueTTL)
		if !c.SeedListing(opts, []timescale.AssetRow{{Code: "OTHER"}}, time.Now().Add(-3*time.Minute)) {
			t.Fatal("seed refused")
		}
		if failFirst {
			if _, _, _, err := c.ListAssetsExtAt(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			close(up.release)
			awaitCatalogueRefresh(t, c, opts)
		}
		s := &Server{AssetsReader: c}
		_, join, err := s.rwaListingRows(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		b := s.judgeRWAAssetsView(rwaAssetsBuild{catalogueAt: join.observedAt, catalogueReads: join.reads}, false, false)
		if join.unanswered != failFirst || b.flags.Stale != failFirst || b.flags.Degraded != failFirst {
			t.Errorf("rows 3m old, catalogue refresh failed=%v: build unanswered=%v, served flags=%+v; want flagged iff failed",
				failFirst, join.unanswered, b.flags)
		}
		if failFirst {
			continue
		}
		// The refresh running at build time fails afterwards: the view built clean is flagged when served.
		close(up.release)
		awaitCatalogueRefresh(t, c, opts)
		if b = s.judgeRWAAssetsView(b, false, false); !b.flags.Stale || !b.flags.Degraded {
			t.Errorf("catalogue refresh failed after the build: served flags=%+v, want stale and degraded", b.flags)
		}
	}
}
