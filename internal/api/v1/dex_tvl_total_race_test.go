package v1_test

import (
	"context"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// togglingAquariusReader alternates the single pool's reserve between
// two well-known amounts on every read, so consecutive Refresh() cycles
// publish two DIFFERENT (and independently verifiable) tvl_usd figures
// rather than merely two different as_of stamps that could collide at
// RFC3339's second resolution.
type togglingAquariusReader struct {
	n atomic.Int64
}

func (r *togglingAquariusReader) LatestAquariusReserves(context.Context, int) ([]timescale.AquariusPoolReserve, error) {
	n := r.n.Add(1)
	// 400_000_000 raw units @ $0.25 = $10.00; 800_000_000 = $20.00.
	raw := int64(400_000_000)
	if n%2 == 0 {
		raw = 800_000_000
	}
	return []timescale.AquariusPoolReserve{{
		ContractID: "CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV",
		ObservedAt: time.Now(),
		Ledger:     63_000_000,
		Legs: []timescale.AquariusReserveLeg{
			{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(raw))},
		},
	}}, nil
}
