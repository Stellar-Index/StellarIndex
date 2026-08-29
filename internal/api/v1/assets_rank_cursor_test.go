package v1_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// #356: the listing's ORDER BY grew a LEADING rank-tier key (flagged /
// unpriced rows sort below rankable ones). The keyset cursor MUST carry
// that key — a cursor that encodes fewer keys than the ORDER BY ranks on
// resumes at the wrong place and drops whole tiers of rows. These pin the
// handler's half of that contract: it emits the store's encoding verbatim
// (tier first) on BOTH /v1/assets paths, and the emitted cursor is
// accepted back by the same handler's validator.

// The reported #356 row, as the store would hand it to the handler.
// Named constants rather than inline literals — a G-strkey spelled out
// next to a field whose name ends in "Key" trips gitleaks' generic-api-key
// rule (it is a public issuer address, not a secret), and the sibling
// directory tests already use this shape.
const (
	jfkBankIssuer  = "GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK"
	jfkBankAssetID = "JFKBANK2-" + jfkBankIssuer
)

// rankTierAssetsReader returns one demoted row carrying the sort keys the
// store's listing query would have produced.
type rankTierAssetsReader struct {
	stubAssetsReaderExt
	lastCursor string
}

func (r *rankTierAssetsReader) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	r.lastCursor = opts.Cursor
	tier := 2
	sortVol := "62341.98422258"
	// Two rows so the handler's overfetch-by-one sees a next page at limit=1.
	rows := make([]timescale.AssetRow, 0, 2)
	for i := 0; i < 2; i++ {
		rows = append(rows, timescale.AssetRow{
			AssetID:          jfkBankAssetID,
			Slug:             jfkBankAssetID,
			Code:             "JFKBANK2",
			IssuerGStrkey:    jfkBankIssuer,
			ObservationCount: int64(1779 - i),
			SortVolume24hUSD: &sortVol,
			RankTier:         &tier,
		})
	}
	return rows, nil
}

func nextCursorOf(t *testing.T, ts, path string) string {
	t.Helper()
	resp := mustGet(t, ts+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, resp.StatusCode)
	}
	var env struct {
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)
	if env.Pagination == nil || env.Pagination.Next == "" {
		t.Fatalf("GET %s emitted no next cursor", path)
	}
	return env.Pagination.Next
}

func TestAssetList_NextCursorCarriesTheRankTier(t *testing.T) {
	reader := &rankTierAssetsReader{}
	srv := v1.New(v1.Options{AssetsReader: reader})
	ts := httpTestServer(t, srv)

	// Observation-count listing: <rank_tier>:<observation_count>:<asset_id>.
	obs := nextCursorOf(t, ts.URL, "/v1/assets?limit=1")
	wantObs := "2:1779:" + jfkBankAssetID
	if obs != wantObs {
		t.Fatalf("observation-count next cursor = %q, want %q", obs, wantObs)
	}

	// Unified listing's classic phase:
	// classic:<rank_tier>:<sort_volume>:<asset_id>. The sort volume stays
	// the §4-B adjusted key, not the raw payload volume.
	cls := nextCursorOf(t, ts.URL, "/v1/assets?asset_class=all&limit=1")
	wantCls := "classic:2:62341.98422258:" + jfkBankAssetID
	if cls != wantCls {
		t.Fatalf("classic-phase next cursor = %q, want %q", cls, wantCls)
	}

	// Both must be accepted back by the handler that minted them (AGT-06
	// validates cursors at the boundary), and reach the store intact.
	for _, tc := range []struct{ path, wantInner string }{
		{"/v1/assets?limit=1&cursor=" + url.QueryEscape(obs), obs},
		{"/v1/assets?asset_class=all&limit=1&cursor=" + url.QueryEscape(cls), strings.TrimPrefix(cls, "classic:")},
	} {
		resp := mustGet(t, ts.URL+tc.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("replaying own cursor on %s: status = %d, want 200", tc.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
		if reader.lastCursor != tc.wantInner {
			t.Errorf("store received cursor %q, want %q", reader.lastCursor, tc.wantInner)
		}
	}
}
