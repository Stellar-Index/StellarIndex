package pipeline

import (
	"context"

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
