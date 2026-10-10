package v1_test

// The 7d sparkline column on /assets must not be blank for exactly the
// eleven assets that matter (XLM, USDC, PYUSD, EURC, AQUA, yXLM, SHX,
// VELO, BLND, PHO, yUSDC) while the unverified long tail below charted
// fine, and `?include=sparkline7d` must not be a silent no-op on the plain
// listing. Those eleven are precisely the catalogue-projected rows,
// whose wire asset_id is the catalogue SLUG ("xlm", "aqua") — the batch
// series reader was asked for a series under an id that can never match
// a prices_1m row, and answered with its 7-bucket skeleton (all prices
// null), which looks exactly like "this asset never traded".
//
// The mirror-image defect these tests also pin: a row whose price we
// deliberately WITHHOLD (scam-flagged issuer, thin-market substance
// gate) must not publish the same number as a picture. Measured on r1
// the flagged JFKBANK2/RIO rows served price_usd null with
// a full 7-point chart, and their details served 24 hourly + 7 daily
// priced points.

import (
	"context"
	"database/sql"
	"sync"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// The Stellar twins of the two catalogue entries used below.
	nativeAssetID = "native"
	aquaAssetID   = "AQUA-" + otherRealIssuer
)

// sparklineStub is an AssetsReader that records exactly which asset_ids
// the sparkline attach asked for, and answers the way the real batch
// query does: EVERY requested id gets its 7 daily buckets back, but only
// ids with a market get prices in them (the query's want × days CROSS
// JOIN). That fidelity is the point — a stub that returned nothing for
// an unknown id would hide the defect, which on the wire looks like a
// well-formed but priceless series.
type sparklineStub struct {
	*stubAssetsReaderExt
	classic []timescale.AssetRow
	byID    map[string]timescale.AssetRow
	series  map[string][]string

	mu        sync.Mutex
	requested []string
}

func (s *sparklineStub) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if opts.Issuer == "" {
		return s.classic, nil
	}
	// lookupCatalogueTwin's exact-issuer filter.
	var out []timescale.AssetRow
	for _, row := range s.byID {
		if row.IssuerGStrkey == opts.Issuer {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *sparklineStub) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	row, ok := s.byID[assetID]
	if !ok {
		return timescale.AssetRow{}, sql.ErrNoRows
	}
	return row, nil
}

func (s *sparklineStub) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	return s.GetAssetByAssetID(ctx, nativeAssetID)
}

func (s *sparklineStub) GetAssetsPriceHistory7dBatch(_ context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	s.mu.Lock()
	s.requested = append(s.requested, assetIDs...)
	s.mu.Unlock()
	out := make(map[string][]timescale.AssetPricePoint, len(assetIDs))
	for _, id := range assetIDs {
		prices := s.series[id]
		pts := make([]timescale.AssetPricePoint, 0, 7)
		for i, day := range sparklineDays {
			var p *string
			if i < len(prices) {
				v := prices[i]
				p = &v
			}
			pts = append(pts, timescale.AssetPricePoint{T: day, P: p})
		}
		out[id] = pts
	}
	return out, nil
}

func (s *sparklineStub) requestedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requested...)
}

// sparklineDays is the bucket skeleton every series comes back with.
var sparklineDays = []string{
	"2026-08-23T00:00:00Z", "2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z",
	"2026-08-26T00:00:00Z", "2026-08-27T00:00:00Z", "2026-08-28T00:00:00Z",
	"2026-08-29T00:00:00Z",
}

func sparklineSeries(prices ...string) []string { return prices }

// findRowBySlug / findRowByAssetID locate a listing row for assertions.
func findRowBySlug(rows []v1.AssetDetail, slug string) *v1.AssetDetail {
	for i := range rows {
		if rows[i].Slug == slug {
			return &rows[i]
		}
	}
	return nil
}

func findRowByAssetID(rows []v1.AssetDetail, assetID string) *v1.AssetDetail {
	for i := range rows {
		if rows[i].AssetID == assetID {
			return &rows[i]
		}
	}
	return nil
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// pricedPoints returns the non-null prices of a wire series.
func pricedPoints(pts []v1.AssetPricePoint) []string {
	out := make([]string, 0, len(pts))
	for _, pt := range pts {
		if pt.P != nil {
			out = append(out, *pt.P)
		}
	}
	return out
}
