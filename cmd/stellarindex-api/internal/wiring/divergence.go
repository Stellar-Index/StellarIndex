package wiring

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// DivergenceAdapter wraps *divergence.Service to satisfy the v1 DivergenceLooker interface.
type DivergenceAdapter struct {
	svc *divergence.Service
}

func NewDivergenceAdapter(svc *divergence.Service) DivergenceAdapter {
	return DivergenceAdapter{svc: svc}
}

// DivergenceFiringFor reads the cached verdict for the EXACT (asset, quote) pair, never another quote of the same base, so a diverging XLM/GBP cannot flag a clean XLM/USD response.
func (a DivergenceAdapter) DivergenceFiringFor(ctx context.Context, asset, quote canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	pair, perr := canonical.NewPair(asset, quote)
	if perr != nil {
		return false, false, 0, nil //nolint:nilerr // intentional: an unconstructible pair reports unchecked
	}
	return a.svc.LookupCachedPairVerdict(ctx, pair)
}
