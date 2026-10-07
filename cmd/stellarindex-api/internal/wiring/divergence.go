package wiring

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// DivergenceAdapter wraps *divergence.Service to satisfy the v1
// DivergenceLooker interface. v1 deliberately doesn't import the
// divergence package (kept storage-package-agnostic); this thin
// shim is the wire between them.
type DivergenceAdapter struct {
	svc *divergence.Service
}

func NewDivergenceAdapter(svc *divergence.Service) DivergenceAdapter {
	return DivergenceAdapter{svc: svc}
}

// DivergenceFiringFor reads the cached verdict for the EXACT (asset,
// quote) pair — never another quote of the same base. LookupCached(asset)
// would OR every quote's WarningFired together, so a diverging XLM/GBP
// would flag a clean XLM/USD response. A pair that fails to construct (asset ==
// quote — callers should never reach this, since parsing already
// rejects an identity price) reports unchecked rather than panicking.
// The quorum behind `checked` (LookupCachedPairVerdict) is the
// service's own, so it cannot drift from the one WarningFired was
// gated on, and (firing=true, checked=false) cannot occur: a firing
// pair met the quorum. window is the verdict's recorded aggregation window.
func (a DivergenceAdapter) DivergenceFiringFor(ctx context.Context, asset, quote canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	pair, perr := canonical.NewPair(asset, quote)
	if perr != nil {
		return false, false, 0, nil //nolint:nilerr // intentional: an unconstructible pair reports unchecked
	}
	return a.svc.LookupCachedPairVerdict(ctx, pair)
}
