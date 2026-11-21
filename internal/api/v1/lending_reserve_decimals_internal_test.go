package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type countingAssetsReader struct {
	*truthAssetsReader
	reads int
}

func (c *countingAssetsReader) GetAssetByAssetID(ctx context.Context, id string) (timescale.AssetRow, error) {
	c.reads++
	return c.truthAssetsReader.GetAssetByAssetID(ctx, id)
}

// A reserve with no established decimals is never valued, so its price chain is not read at all.
func TestReserveValue_UnknownDecimalsReadsNoPrice(t *testing.T) {
	r := &countingAssetsReader{truthAssetsReader: catalogueRowReader("2.5")}
	st := catalogueOnlyReserve()
	st.DecimalsFound = false
	rv, supplied, _ := New(Options{AssetsReader: r}).buildReserveView(context.Background(), st)
	if rv.SuppliedUSD != nil || supplied != nil || rv.Decimals != nil {
		t.Fatalf("unknown-decimals reserve valued: supplied_usd=%s", strPtrOr(rv.SuppliedUSD))
	}
	if r.reads != 0 {
		t.Errorf("price chain read %d catalogue row(s) for a reserve that cannot be valued, want 0", r.reads)
	}
}
