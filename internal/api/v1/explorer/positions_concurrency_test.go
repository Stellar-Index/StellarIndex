package explorer

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// slowPositionsReader answers every fold after `delay`, counting how
// many folds are in flight at once. Two folds fail so the coverage
// merge is exercised too.
type slowPositionsReader struct {
	delay      time.Duration
	inFlight   atomic.Int32
	maxFlight  atomic.Int32
	blendFails bool
}

func (r *slowPositionsReader) enter() {
	n := r.inFlight.Add(1)
	for {
		m := r.maxFlight.Load()
		if n <= m || r.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(r.delay)
	r.inFlight.Add(-1)
}

func (r *slowPositionsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	r.enter()
	if r.blendFails {
		return nil, errors.New("blend down")
	}
	return nil, nil
}

func (r *slowPositionsReader) BlendBackstopSharesByUser(context.Context, string) ([]timescale.BlendBackstopFold, error) {
	r.enter()
	return nil, errors.New("backstop down")
}

func (r *slowPositionsReader) PhoenixStakeByUser(context.Context, string) ([]timescale.PhoenixStakeFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) CreditPositionsByOwner(context.Context, string) ([]timescale.CreditPositionFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) AquariusGaugeByUser(context.Context, string) ([]timescale.AquariusGaugeFold, error) {
	r.enter()
	return nil, errors.New("aquarius down")
}
