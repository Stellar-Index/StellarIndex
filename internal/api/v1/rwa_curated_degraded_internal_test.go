package v1

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type failingRWACuratedReader struct {
	directoryErr, publishedErr error
}

func (f failingRWACuratedReader) CuratedRWADirectoryByAddress(context.Context, string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	return nil, timescale.CuratedRWACensus{}, f.directoryErr
}

func (f failingRWACuratedReader) LatestCuratedPublished(context.Context, string) (*timescale.CuratedRWAPublished, error) {
	return nil, f.publishedErr
}

// A curated read that errored is an outage: the view must be degraded (no-store, short TTL), not a quiet curator served clean.
func TestAttachRWACurated_AFailedReadDegradesTheView(t *testing.T) {
	down := errors.New("postgres: connection reset")
	for _, tt := range []struct {
		name   string
		reader failingRWACuratedReader
	}{
		{"published totals read failed", failingRWACuratedReader{publishedErr: down}},
		{"directory read failed", failingRWACuratedReader{directoryErr: down}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := rwaCuratedCacheTestServer(tt.reader)
			view := &RWAAssetsView{}
			if degraded := s.attachRWACurated(context.Background(), view, time.Now()); !degraded {
				t.Error("degraded = false after a curated read errored; the envelope would read as a healthy, empty curator")
			}
		})
	}
	if degraded := rwaCuratedCacheTestServer(failingRWACuratedReader{}).attachRWACurated(
		context.Background(), &RWAAssetsView{}, time.Now()); degraded {
		t.Error("degraded = true when both reads answered with nothing")
	}
}

type curatedEntriesReader struct {
	rows map[string]timescale.CuratedRWAEntry
}

func (c curatedEntriesReader) CuratedRWADirectoryByAddress(context.Context, string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	return c.rows, timescale.CuratedRWACensus{Entries: len(c.rows), Contracts: len(c.rows)}, nil
}

func (curatedEntriesReader) LatestCuratedPublished(context.Context, string) (*timescale.CuratedRWAPublished, error) {
	return nil, nil
}

// A budget spent before any curated row was valued serves every row unvalued: the block must still say why and how many,
// not serve assets_valued=0 with no reason.
func TestAttachRWACurated_ACutBudgetNamesTheUnvaluedRows(t *testing.T) {
	const a = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const b = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	s := rwaCuratedCacheTestServer(curatedEntriesReader{rows: map[string]timescale.CuratedRWAEntry{
		a: {Address: a, AssetCode: "TBILL"}, b: {Address: b, AssetCode: "BOND"},
	}})
	net := big.NewInt(10_000_0000000)
	s.TokenSupply = &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
		a: {Total: net, Mint: net, FlowCount: 4}, b: {Total: net, Mint: net, FlowCount: 4},
	}}
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	view := &RWAAssetsView{}

	if degraded := s.attachRWACurated(spent, view, time.Now()); !degraded {
		t.Error("degraded = false after the valuation budget was spent")
	}
	out := view.Curated
	if out == nil || out.Status != "served" || out.AssetsValued != 0 || len(view.CuratedAssets) != 2 {
		t.Fatalf("curated = %+v rows = %d, want served with 2 unvalued rows", out, len(view.CuratedAssets))
	}
	if !strings.Contains(out.Basis, "cut short, so 2 row(s)") {
		t.Errorf("basis does not name the 2 unread rows: %q", out.Basis)
	}
}

// A valuation cut short leaves rows served beside the block. The block must count them and bound its total, never call the
// arm unavailable while curated_assets carries its rows.
func TestRWACuratedSummarise_ACutValuationServesTheRowsAsALowerBound(t *testing.T) {
	const a = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const b = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	snap := rwaCurated{available: true, wired: true, byAddress: map[string]timescale.CuratedRWAEntry{a: {}, b: {}}}
	value, verified := "100.00", "50.00"
	rows := []RWAAsset{
		{ContractID: a, Curator: &RWACurator{}, ReferenceValuation: RWAReferenceValuation{ValueUSD: &value}},
		{ContractID: b, Curator: &RWACurator{}},
	}

	out := rwaCuratedSummarise(snap, rows, 0, true, &verified, false, nil, time.Now())

	if out.Status != "served" || out.Assets != len(rows) || out.AssetsValued != 1 {
		t.Fatalf("status = %q assets = %d valued = %d, want served/%d/1 beside the rows it serves",
			out.Status, out.Assets, out.AssetsValued, len(rows))
	}
	if out.AdditionalValueUSD == nil || *out.AdditionalValueUSD != "100.00" || !out.AdditionalValueLowerBound {
		t.Errorf("additional = %v lower_bound = %v, want 100.00 as a lower bound", out.AdditionalValueUSD, out.AdditionalValueLowerBound)
	}
	if out.CombinedValueUSD == nil || !out.CombinedValueLowerBound {
		t.Errorf("combined = %v lower_bound = %v, want a lower bound", out.CombinedValueUSD, out.CombinedValueLowerBound)
	}
	if !strings.Contains(out.Basis, "cut short, so 1 row(s)") {
		t.Errorf("basis does not name the unread row: %q", out.Basis)
	}
}
