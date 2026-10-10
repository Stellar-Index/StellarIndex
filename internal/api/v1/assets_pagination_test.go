package v1_test

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// paginatingAssetsReader embeds the full stub and overrides only
// ListAssetsExt, honouring opts.Limit by returning min(Limit, total)
// rows so the handler's overfetch-by-one logic is exercised exactly as
// the real store would drive it.
type paginatingAssetsReader struct {
	stubAssetsReaderExt
	total int
}

func (p *paginatingAssetsReader) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	n := opts.Limit
	if n > p.total {
		n = p.total
	}
	rows := make([]timescale.AssetRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, timescale.AssetRow{
			AssetID:          "USDC-GAAA",
			Slug:             "usdc",
			Code:             "USDC",
			ObservationCount: int64(i + 1),
		})
	}
	return rows, nil
}

// TestAssetList_AssetsPaginationEmitsCursor pins the case when the assetsReader
// catalogue holds more than `limit` rows, /v1/assets MUST emit a next
// cursor. The previous handler passed `limit` (not limit+1) to the
// store, so the overfetch sentinel never appeared and the listing was
// stuck on its first page over a ~199K-asset directory.
func TestAssetList_AssetsPaginationEmitsCursor(t *testing.T) {
	srv := v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 1000}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.AssetDetail `json:"data"`
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 50 {
		t.Fatalf("returned %d rows, want exactly the page size 50 (overfetch row must be trimmed)", len(env.Data))
	}
	if env.Pagination == nil || env.Pagination.Next == "" {
		t.Fatalf("no next cursor emitted despite 1000 > 50 rows available (F-1326)")
	}
}

// TestAssetList_RejectsMalformedCursor guards cursor validation on both the
// default listing and the unified (asset_class=all) classic phase: without it
// a malformed cursor falls through to the keyset predicate's degenerate
// (0, "") case and reads as a quiet end-of-pagination (empty page, 200 OK).
func TestAssetList_RejectsMalformedCursor(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 1000}}))
	for _, q := range []string{
		"limit=50&cursor=not-a-valid-cursor",
		"asset_class=all&limit=50&cursor=classic:not-a-valid-cursor",
	} {
		t.Run(q, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets?"+q)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a malformed cursor", resp.StatusCode)
			}
		})
	}
}

// TestAssetList_AssetsPaginationLastPageNoCursor confirms the tail page
// (rows ≤ limit) correctly omits the cursor.
func TestAssetList_AssetsPaginationLastPageNoCursor(t *testing.T) {
	srv := v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 30}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.AssetDetail `json:"data"`
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 30 {
		t.Fatalf("returned %d rows, want 30", len(env.Data))
	}
	if env.Pagination != nil && env.Pagination.Next != "" {
		t.Fatalf("unexpected next cursor on the final page: %q", env.Pagination.Next)
	}
}
