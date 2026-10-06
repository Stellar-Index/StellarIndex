package pipeline

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeSpectraStore layers an in-memory spectra_markets double over the
// protocol_contracts one.
type fakeSpectraStore struct {
	fakeProtocolContractStore
	markets []timescale.SpectraMarket
}

func (f *fakeSpectraStore) SpectraMarkets(context.Context) ([]timescale.SpectraMarket, error) {
	return f.markets, nil
}

// A market found after the hand-kept list has bare protocol_contracts rows;
// only its spectra_markets row restores the PT and YT roles after a restart.
func TestGatedRegistryOptions_SpectraMarketRowsRestoreRoles(t *testing.T) {
	const pt = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	const yt = "CBQHNAXSI55GX2GN6D67GK7BHVPSLJUGZQEU7WKQU3X7PTUSXHMYZJRR"
	for _, id := range []string{pt, yt} {
		if _, ok := spectra.MainnetContracts[id]; ok {
			t.Fatalf("%s is hand-kept; the test needs a market outside the list", id)
		}
	}
	rows := map[string][]string{spectra.SourceName: {pt, yt}}
	ptMinted := events.Event{ContractID: pt, Topic: []string{scval.MustEncodeSymbol(spectra.EventPTMinted)}}
	ytTransfer := events.Event{ContractID: yt, Topic: []string{scval.MustEncodeSymbol(spectra.EventTransfer)}}

	bare, err := gatedRegistryOptions(context.Background(),
		&fakeProtocolContractStore{rows: rows}, quietLogger(), context.Background(), false)
	if err != nil {
		t.Fatalf("gatedRegistryOptions (bare): %v", err)
	}
	if d := spectra.NewDecoder(bare[spectra.SourceName]...); d.Matches(ptMinted) || d.Matches(ytTransfer) {
		t.Fatal("a role-less protocol_contracts row was admitted; it must fail closed")
	}

	store := &fakeSpectraStore{
		fakeProtocolContractStore: fakeProtocolContractStore{rows: rows},
		markets:                   []timescale.SpectraMarket{{PT: pt, YT: yt}},
	}
	got, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), false)
	if err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}
	d := spectra.NewDecoder(got[spectra.SourceName]...)
	if !d.Matches(ptMinted) {
		t.Error("the recorded PT lost its role across the warm")
	}
	if !d.Matches(ytTransfer) {
		t.Error("the recorded YT lost its role or market across the warm")
	}
}
