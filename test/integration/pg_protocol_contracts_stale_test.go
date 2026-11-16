//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A curated row whose contract has left the keep list is selected and deleted; a curated row still listed, a factory
// child and another source's curated row are not. An empty keep list means the whole curated provenance is stale.
func TestStaleProtocolContracts_OnlyCuratedRowsOutsideTheSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		kept    = "CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB"
		removed = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		child   = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		factory = "CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS"
	)
	for _, row := range []struct{ source, id, provenance string }{
		{"src_a", kept, pipeline.CuratedFactoryID},
		{"src_a", removed, pipeline.CuratedFactoryID},
		{"src_a", child, factory},
		{"src_b", removed, pipeline.CuratedFactoryID},
	} {
		if err := store.UpsertProtocolContract(ctx, row.source, row.id, row.provenance, 1); err != nil {
			t.Fatalf("seed %s/%s: %v", row.source, row.id, err)
		}
	}

	stale, err := store.StaleProtocolContracts(ctx, "src_a", pipeline.CuratedFactoryID, []string{kept})
	if err != nil || !slices.Equal(stale, []string{removed}) {
		t.Fatalf("stale = %v, %v; want only %s", stale, err, removed)
	}
	deleted, err := store.DeleteStaleProtocolContracts(ctx, "src_a", pipeline.CuratedFactoryID, []string{kept})
	if err != nil || !slices.Equal(deleted, []string{removed}) {
		t.Fatalf("deleted = %v, %v; want only %s", deleted, err, removed)
	}
	left, err := store.LoadProtocolContracts(ctx, "src_a")
	slices.Sort(left)
	want := []string{kept, child}
	slices.Sort(want)
	if err != nil || !slices.Equal(left, want) {
		t.Fatalf("src_a holds %v, %v; want the kept curated row and the factory child", left, err)
	}
	if other, err := store.LoadProtocolContracts(ctx, "src_b"); err != nil || len(other) != 1 {
		t.Fatalf("src_b holds %v, %v; another source's rows must be untouched", other, err)
	}

	all, err := store.StaleProtocolContracts(ctx, "src_a", pipeline.CuratedFactoryID, nil)
	if err != nil || !slices.Equal(all, []string{kept}) {
		t.Fatalf("with an empty set stale = %v, %v; want every curated row (%s) and never the factory child", all, err, kept)
	}
}
