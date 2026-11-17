package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type rwaServeEnvelope struct {
	AsOf  time.Time `json:"as_of"`
	Flags struct {
		Stale bool `json:"stale"`
	} `json:"flags"`
}

func getRWAEnvelope(t *testing.T, srv *v1.Server) (rwaServeEnvelope, http.Header) {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env rwaServeEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env, resp.Header
}

// TestRWAAssets_AFailedMembershipReadIsDegraded: a membership whose only build failed is served as the unavailable view
// under flags.degraded and no-store, not as a clean answer.
func TestRWAAssets_AFailedMembershipReadIsDegraded(t *testing.T) {
	srv := v1.New(v1.Options{
		Listings:  rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{err: errors.New("sep1 scan: statement timeout")},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
	})
	_, h := getRWAEnvelope(t, srv)
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on a degraded answer", cc)
	}
}

// rwaDatedListStub serves listing pages as a cache would after its upstream refresh kept failing: old and stale.
type rwaDatedListStub struct {
	*rwaListStub
	at time.Time
}

func (s *rwaDatedListStub) ListAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	rows, err := s.ListAssetsExt(ctx, opts)
	return rows, s.at, true, err
}

// TestRWAAssets_AsOfIsTheOldestListingRead: the money figures are as old as the listing rows behind them, so as_of says so
// and a stale page marks the response stale, rather than stamping two-hour-old prices with the request time.
func TestRWAAssets_AsOfIsTheOldestListingRead(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}}
	srv := v1.New(v1.Options{
		Listings:  rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		AssetsReader: &rwaDatedListStub{
			rwaListStub: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows)},
			at:          old,
		},
	})
	env, h := getRWAEnvelope(t, srv)
	if !env.AsOf.Equal(old) {
		t.Errorf("as_of = %s, want the listing read at %s", env.AsOf, old)
	}
	if !env.Flags.Stale || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("flags = %+v, Cache-Control = %q, want stale and no-store for a page served past its refresh",
			env.Flags, h.Get("Cache-Control"))
	}
}
