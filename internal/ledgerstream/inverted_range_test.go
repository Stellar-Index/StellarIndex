package ledgerstream_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// An inverted bounded range is rejected before any datastore is opened —
// on the tiered path the cold tier used to be opened (a live bucket
// round-trip in production) before walkDataStore got to validateRange.
func TestStream_InvertedRangeRejectedBeforeAnyDatastoreOpens(t *testing.T) {
	hotCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": t.TempDir()},
		Schema: datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
	}
	coldCfg := hotCfg
	coldOpens := 0
	cfg := ledgerstream.Config{
		DataStore:     hotCfg,
		ColdDataStore: coldCfg,
		ColdDataStoreFactory: func(ctx context.Context) (datastore.DataStore, error) {
			coldOpens++
			return datastore.NewDataStore(ctx, coldCfg)
		},
	}

	err := ledgerstream.Stream(context.Background(), cfg, 10, 5, func(xdr.LedgerCloseMeta) error { return nil })

	if err == nil || !strings.Contains(err.Error(), "must not be less than start") {
		t.Fatalf("Stream(10, 5) = %v, want the validateRange rejection", err)
	}
	if coldOpens != 0 {
		t.Errorf("cold tier opened %d time(s) for an inverted range, want 0", coldOpens)
	}
}
